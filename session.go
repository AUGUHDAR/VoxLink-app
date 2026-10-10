package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	defaultHeartbeatInterval = 5 * time.Second
	maxHeartbeatFailures     = 5
	maxPollFailures          = 10
	wsRetryWait              = 30 * time.Second
	httpPollInterval         = time.Second
	wsResponseTimeout        = 15 * time.Second
	// WS 健康时信令由服务端推送实时到达，帧轮询只是安全网（对齐 mod 端 5s 策略）
	wsPollInterval = 5 * time.Second
)

// session 单个房间会话：心跳循环 + 信令循环，直到 stop 或致命错误。
type session struct {
	app    *App
	code   string
	token  string
	isHost bool

	done     chan struct{} // 会话结束（stop 或 fatal）
	doneOnce sync.Once
	stopCh   chan struct{} // 主动停止（LeaveRoom）
	stopOnce sync.Once
	fatalMu  sync.Mutex
	fatalled bool

	since atomic.Int64 // 信令游标：收到的最大 timestamp（ms）

	// WS 复用为通用帧通道：wsLoop 持有连接，心跳/轮询经 wsSend 帧化发送，
	// 读 goroutine 按 ID 回填 pending（推送帧 id=0 走 handleSignals）。
	wsMu      sync.Mutex
	wsConn    *websocket.Conn
	wsPending map[int64]chan *wsFrame
}

func newSession(app *App, code, token string, isHost bool) *session {
	return &session{
		app:    app,
		code:   code,
		token:  token,
		isHost: isHost,
		done:   make(chan struct{}),
		stopCh: make(chan struct{}),
	}
}

func (s *session) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// stop 主动停止会话（LeaveRoom），不触发 closed 事件。
// done 必须一并关闭：心跳循环只认 done，若不关它 LeaveRoom 后旧循环会带着旧 token
// 每 5s 继续心跳，服务端就一直算本端在房内，且 run() 的 clearSession/unbindSession 永不执行。
func (s *session) stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.closeDone()
}

func (s *session) closeDone() {
	s.doneOnce.Do(func() { close(s.done) })
}

// fatal 标记会话致命结束，广播 closed 事件（只执行一次）。
func (s *session) fatal(msg string) {
	s.fatalMu.Lock()
	defer s.fatalMu.Unlock()
	if s.fatalled {
		return
	}
	s.fatalled = true
	s.app.netLog("error", "会话结束: "+msg)
	s.app.emit("session:state", map[string]any{
		"state":   "closed",
		"message": msg,
		"room":    s.app.snapshotRoom(),
	})
	s.closeDone()
}

// run 并发运行心跳与信令循环，返回时清理 App 状态与连接引擎。
func (s *session) run() {
	defer s.app.clearSession(s)
	defer s.app.engine.unbindSession(s)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.heartbeatLoop()
	}()
	go func() {
		defer wg.Done()
		s.signalLoop()
	}()
	wg.Wait()
}

// isFatalAuthError 判断是否为会话必须终止的致命错误码。
func isFatalAuthError(code string) bool {
	switch code {
	case "ROOM_EXPIRED", "ROOM_CLOSED", "ROOM_EVICTED", "INVALID_TOKEN":
		return true
	}
	return false
}

// ---- 心跳循环 ----

// 心跳的容错口径（重要，勿改回"失败即致命"）：
// 只有服务端明确判定房间失效（410 / ROOM_CLOSED / ROOM_EXPIRED / ROOM_EVICTED /
// INVALID_TOKEN）才结束会话。网络抖动、超时、5xx 一律退避重试——
// 房主侧一次"会话自杀"就等于整间房：心跳停 → 服务端 room_ttl(300s) 判房主失联 →
// 房间从列表消失且拒绝新加入，对房客表现为"房主关闭了房间"。
// 打洞/中继期间的瞬时网络失败尤其常见（自建 UDP 洪水会挤占 HTTP），
// 因此这里必须扛住，而不是 25s 就放弃整间房。
const heartbeatMaxBackoff = 30 * time.Second

func heartbeatBackoff(interval time.Duration, fails int) time.Duration {
	wait := interval
	for i := 0; i < fails && wait < heartbeatMaxBackoff; i++ {
		wait *= 2
	}
	if wait > heartbeatMaxBackoff {
		wait = heartbeatMaxBackoff
	}
	return wait
}

func (s *session) heartbeatLoop() {
	interval := defaultHeartbeatInterval
	seq := int64(0)
	fails := 0

	for {
		if s.isDone() {
			return
		}

		seq++
		body := map[string]any{
			"code":            s.code,
			"token":           s.token,
			"isHost":          s.isHost,
			"seq":             seq,
			"currentInterval": 5,
			"mcPlayerCount":   0,
		}
		var hb struct {
			HeartbeatInterval int    `json:"heartbeatInterval"`
			CurrentPlayers    int    `json:"currentPlayers"`
			Name              string `json:"name"`
		}
		// 心跳优先走 WS 帧（省去每 5s 一次的 HTTP 请求）；WS 未连接/失败回退 HTTP
		var err error
		if f, ok := s.wsSend("/room/heartbeat", body); ok {
			if !f.Success {
				err = &APIError{Code: f.Error, Message: f.Message, Status: 400}
			} else if len(f.Data) > 0 {
				_ = json.Unmarshal(f.Data, &hb)
			}
		} else {
			err = s.app.api.post(s.app.baseURL(), "/room/heartbeat", body, &hb)
		}
		wait := interval
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) {
				if ae.Status == http.StatusGone || isFatalAuthError(ae.Code) {
					msg := ae.Message
					if msg == "" {
						msg = "房间已失效"
					}
					s.fatal(msg)
					return
				}
			}
			fails++
			if fails == 1 || fails%maxHeartbeatFailures == 0 {
				s.app.netLog("warn", fmt.Sprintf("心跳失败(%d 次)，继续重试: %v", fails, err))
			}
			wait = heartbeatBackoff(interval, fails)
		} else {
			if fails >= maxHeartbeatFailures {
				s.app.netLog("info", fmt.Sprintf("心跳已恢复（中断 %d 次）", fails))
			}
			fails = 0
			s.app.updateRoomInfo(s, hb.CurrentPlayers, hb.Name)
			if hb.HeartbeatInterval > 0 {
				interval = time.Duration(hb.HeartbeatInterval) * time.Second
			}
		}

		select {
		case <-s.done:
			return
		case <-time.After(wait):
		}
	}
}

// ---- 信令循环 ----

// signalLoop WS 优先；失败回退 HTTP 轮询，最多 30s 后重试 WS。
func (s *session) signalLoop() {
	s.since.Store(time.Now().UnixMilli() - 30_000)

	for {
		if s.isDone() {
			return
		}
		if err := s.wsLoop(); err != nil && !s.isDone() {
			s.app.netLog("warn", "WebSocket 断开，回退 HTTP 轮询: "+err.Error())
		}
		if s.isDone() {
			return
		}
		if s.httpPollLoop(wsRetryWait) {
			return // 会话已结束
		}
		s.app.netLog("info", "尝试重连 WebSocket...")
	}
}

// httpPollLoop 每 1s 轮询 /signal/poll，最多运行 maxWait；返回 true 表示会话结束。
// 与心跳同口径：网络类失败只退避重试（signalLoop 会在 maxWait 后改试 WS），
// 唯有服务端判定房间失效才致命——否则房主的信令循环一死，整间房就跟着没了。
func (s *session) httpPollLoop(maxWait time.Duration) bool {
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	interval := httpPollInterval
	fails := 0

	for {
		select {
		case <-s.done:
			return true
		case <-s.stopCh:
			return true
		case <-timer.C:
			return false
		case <-time.After(interval):
		}

		body := map[string]any{
			"code":   s.code,
			"token":  s.token,
			"isHost": s.isHost,
			"since":  s.since.Load(),
		}
		var sd signalsData
		err := s.app.api.post(s.app.baseURL(), "/signal/poll", body, &sd)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) {
				if ae.Status == http.StatusGone || isFatalAuthError(ae.Code) {
					msg := ae.Message
					if msg == "" {
						msg = "房间已失效"
					}
					s.fatal(msg)
					return true
				}
			}
			fails++
			if fails == 1 || fails%maxPollFailures == 0 {
				s.app.netLog("warn", fmt.Sprintf("信令轮询失败(%d 次)，继续重试: %v", fails, err))
			}
			// 连续失败逐步退避（1s→2s→4s…上限 10s），避免网络全断时空转打服务端
			interval = httpPollInterval << min(fails-1, 3)
			if interval > 10*time.Second {
				interval = 10 * time.Second
			}
			continue
		}
		if fails >= maxPollFailures {
			s.app.netLog("info", fmt.Sprintf("信令轮询已恢复（中断 %d 次）", fails))
		}
		fails = 0
		interval = httpPollInterval
		s.handleSignals(sd)
	}
}

// handleSignals 分发信令并推进 since 游标（同时交给连接引擎处理）。
func (s *session) handleSignals(sd signalsData) {
	maxTs := s.since.Load()
	for _, sig := range sd.S {
		if sig.Timestamp > maxTs {
			maxTs = sig.Timestamp
		}
		data := sig.Data
		if data == nil {
			data = map[string]any{}
		}
		s.app.emit("signal", map[string]any{
			"type":      sig.Type,
			"from":      sig.From,
			"data":      data,
			"timestamp": sig.Timestamp,
		})
		// 连接引擎按 type 分发（只处理引擎关心的类型）
		s.app.engine.onSignal(sig.Type, sig.From, data)
	}
	if sd.TS > maxTs {
		maxTs = sd.TS
	}
	s.since.Store(maxTs)
}

// ---- WebSocket 通道 ----

type wsRequest struct {
	ID     int64  `json:"id"`
	Route  string `json:"route"`
	Method string `json:"method"`
	Body   any    `json:"body"`
}

// wsFrame 服务器帧：响应帧带 id/success/data；主动推送帧 id=0 且 push="signals"。
type wsFrame struct {
	ID      int64           `json:"id"`
	Push    string          `json:"push"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type signalsData struct {
	S  []signalMsg `json:"s"`
	TS int64       `json:"ts"`
}

type signalMsg struct {
	ID        json.RawMessage `json:"id"`
	From      string          `json:"from"`
	Type      string          `json:"type"`
	Data      map[string]any  `json:"data"`
	Timestamp int64           `json:"timestamp"`
	To        string          `json:"to"`
}

// wsURLFrom 把 http(s) 基址转为 ws(s) 的 /ws 地址。
func wsURLFrom(baseURL string) string {
	u := strings.TrimRight(baseURL, "/")
	if strings.HasPrefix(u, "https://") {
		u = "wss://" + strings.TrimPrefix(u, "https://")
	} else if strings.HasPrefix(u, "http://") {
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u + "/ws"
}

// wsSend 在 WS 已连接时发送一个帧请求并等待响应；WS 未连接返回 ok=false（调用方回退 HTTP）。
func (s *session) wsSend(route string, body any) (*wsFrame, bool) {
	s.wsMu.Lock()
	conn := s.wsConn
	if conn == nil || s.wsPending == nil {
		s.wsMu.Unlock()
		return nil, false
	}
	id := time.Now().UnixNano()
	ch := make(chan *wsFrame, 1)
	s.wsPending[id] = ch
	s.wsMu.Unlock()

	payload, err := json.Marshal(wsRequest{ID: id, Route: route, Method: "POST", Body: body})
	if err != nil {
		s.wsMu.Lock()
		delete(s.wsPending, id)
		s.wsMu.Unlock()
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), wsResponseTimeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		s.wsMu.Lock()
		delete(s.wsPending, id)
		s.wsMu.Unlock()
		return nil, false
	}
	select {
	case f := <-ch:
		return f, true
	case <-time.After(wsResponseTimeout):
		s.wsMu.Lock()
		delete(s.wsPending, id)
		s.wsMu.Unlock()
		return nil, false
	case <-s.done:
		return nil, false
	case <-s.stopCh:
		return nil, false
	}
}

// wsLoop 维持一次 WebSocket 连接；推送帧实时分发，帧轮询降为 5s 安全网，
// 同一连接同时承载心跳等带 ID 的帧请求（wsSend）。
// 返回错误表示连接失效，由上层回退 HTTP 轮询。
func (s *session) wsLoop() error {
	wsURL := wsURLFrom(s.app.baseURL())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // 会话结束时中断读写
		select {
		case <-s.done:
			cancel()
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	dialCtx, dialCancel := context.WithTimeout(ctx, httpTimeout)
	hdr := http.Header{}
	hdr.Set("User-Agent", httpUserAgent)
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	dialCancel()
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	defer conn.Close(websocket.StatusNormalClosure, "")

	s.wsMu.Lock()
	s.wsConn = conn
	s.wsPending = make(map[int64]chan *wsFrame)
	s.wsMu.Unlock()
	s.app.netLog("info", "WebSocket 已连接")

	defer func() { // 断开后摘除连接，挂起的 wsSend 由超时兜底回退 HTTP
		s.wsMu.Lock()
		s.wsConn = nil
		s.wsPending = nil
		s.wsMu.Unlock()
	}()

	// 读 goroutine：推送帧直接分发，响应帧按 ID 回填 pending。
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f wsFrame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			if f.Push != "" || f.ID == 0 {
				var sd signalsData
				if json.Unmarshal(f.Data, &sd) == nil {
					s.handleSignals(sd)
				}
				continue
			}
			s.wsMu.Lock()
			ch, ok := s.wsPending[f.ID]
			if ok {
				delete(s.wsPending, f.ID)
			}
			s.wsMu.Unlock()
			if ok {
				select {
				case ch <- &f:
				default:
				}
			}
		}
	}()

	ticker := time.NewTicker(wsPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return errors.New("会话已结束")
		case <-s.stopCh:
			return errors.New("会话已停止")
		case <-ticker.C:
		}

		reqBody := map[string]any{
			"code":   s.code,
			"token":  s.token,
			"isHost": s.isHost,
			"since":  s.since.Load(),
		}
		f, ok := s.wsSend("/signal/poll", reqBody)
		if !ok {
			return errors.New("发送失败")
		}
		if !f.Success {
			code := f.Error
			if isFatalAuthError(code) {
				msg := f.Message
				if msg == "" {
					msg = "房间已失效"
				}
				s.fatal(msg)
				return errors.New("会话已结束")
			}
			return fmt.Errorf("服务端错误: %s %s", code, f.Message)
		}
		// 轮询响应与推送同构：复用 handleSignals 推进 since 游标（幂等）
		var sd signalsData
		if json.Unmarshal(f.Data, &sd) == nil {
			s.handleSignals(sd)
		}
	}
}
