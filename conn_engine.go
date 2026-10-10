package main

// conn_engine.go — 连接引擎编排：信令分发、UDP 打洞 → rudp 隧道 → 本地端口映射桥、
// 直连探测（TryDirect）与 MC 端口探测（DetectMcPorts）。玩家中继见 relay.go。
//
// 角色分工（对齐模组 RoomManager/ConnectionManager）：
//   - guest：JoinRoom 成功自动开始 p2p（等 host 的 holepunch_offer → 发 punch_info →
//     等 holepunch_mapped/齐射时刻 → 打洞 → rudp → 本地桥）；TryDirect / UsePlayerRelay
//     由前端按需触发；结果全部经 conn:state 事件推送。
//   - host：收到 join_request 起 host 侧引擎（打洞 socket 绑 hostPort → STUN →
//     签发 holepunch_offer → 收 punch_info 回 holepunch_mapped 并开始打洞 → lazy 桥）。
//     host 端不发 conn:state 事件（host 会话只做应答与桥接）。
//
// 幂等：guest 用 cycle CAS 防重复 holepunch_offer 重入（对齐模组 connectionCycleActive）。

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// conn:state 事件字段取值
const (
	phaseP2P      = "p2p"
	phaseDirect   = "direct"
	phasePrelay   = "prelay"
	phaseTurn     = "turn"
	statusTrying  = "trying"
	statusFailed  = "failed"
	statusSuccess = "success"
)

const (
	offerWaitMappedMax  = 5 * time.Second  // 等 holepunch_mapped 最长（同时受 punchSyncTimeMs 约束）
	punchSyncMaxWait    = 5 * time.Second  // punchSyncTimeMs 时钟差容忍上限
	punchCycleInterval  = 1 * time.Second  // 两次打洞周期之间的退避
	hostPunchLifetime   = 10 * time.Minute // host 打洞等待窗口（实际受会话寿命约束）
	directAttempts      = 3                // 直连探测尝试次数
	directDialTimeout   = 3 * time.Second
	directRetryInterval = 2 * time.Second
	punchInfoTimeout    = 15 * time.Second // host 等 punch_info 的清理窗口
	relayRequestTimeout = 20 * time.Second // relay_request 无响应判失败
	relayPunchTimeout   = 15 * time.Second // 对中继者打洞超时
	relaySetupTimeout   = 15 * time.Second // 中继节点对 target 打洞超时

	// 打洞终局常量（对齐 mod ConnectionManager：正交，前者管整会话零收包，后者管预测关闭频次）
	zeroRecvFinalRoundLimit = 20 // 持续重试轮次上限：整会话零收包才允许判定终局
	predictionOffCap        = 50 // PREDICTION_OFF 累计频次封顶
)

// emitConnState 推送 conn:state 事件（host 端不调用）。
func (a *App) emitConnState(phase, status, address, detail string) {
	a.emit("conn:state", map[string]any{
		"phase":   phase,
		"status":  status,
		"address": address,
		"detail":  detail,
	})
}

// notifyState 引擎侧状态推送：会话已解绑时静默（避免离开房间后还推失败态给前端）。
func (e *connEngine) notifyState(phase, status, address, detail string) {
	e.mu.Lock()
	alive := e.sess != nil
	e.mu.Unlock()
	if !alive {
		return
	}
	e.app.emitConnState(phase, status, address, detail)
}

// ---- 引擎 ----

// hostPeer host 侧单个房客的连接上下文。
type hostPeer struct {
	id              string
	caps            []string // join_request 携带的加入者能力表
	natClass        natClass // punch_info 上报的加入者 NAT 分级
	joinerPortRange int      // punch_info 上报的加入者预测范围（反向打洞用）
	puncher         *puncher
	rudp            *rudpConn
	mapped          *stunMappedAddr // joiner 映射地址（punch_info 上报）
	hostMapped      *stunMappedAddr // 我方打洞 socket 映射（offer 时缓存）
	hostDelta       int
}

type connEngine struct {
	app *App

	mu       sync.Mutex
	sess     *session
	code     string
	token    string
	isHost   bool
	clientID string
	hostPort int
	hostIp   string

	ctx    context.Context
	cancel context.CancelFunc

	// guest 侧状态
	gCycle         int  // 连接周期代数（幂等）
	gActive        bool // 打洞周期进行中（cycle CAS）
	gOffer         map[string]any
	gLastOffer     map[string]any  // ICE 重启用：最近一次 holepunch_offer
	gMapped        *stunMappedAddr // 最新 hostMapped（holepunch_mapped / offer）
	gMappedCh      chan *stunMappedAddr
	gMappedDelta   int
	gHostPortRange int      // holepunch 系列信令携带的 host 预测范围
	gRemoteNat     natClass // 信令携带的 host NAT 分级（中途可纠偏）
	gPunchSock     *net.UDPConn
	gPuncher       *puncher
	gRudp          *rudpConn
	gBridge        *tcpBridge
	gP2PDone       bool
	gTcpLn         net.Listener // TCP 打洞（tcp_punch.go）成功后的本地代理监听
	gTcpConn       net.Conn     // TCP 打洞得到的远端连接，被所有 MC 接入共享

	// 打洞会话记账（终局判定）
	punchRecvEver      bool // 整个会话是否收到过对端任何 UDP 包
	predictionOffCount int  // PREDICTION_OFF 累计频次（封顶 50）
	retryRound         int  // 持续重试轮次（零收包终局判定）

	// TCP SimOpen（tcp_punch.go）
	tcpPunchInFlight bool
	gMyMappedIP      string // guest 本端 STUN 映射 IP（tcp_punch_info 用）

	// host 侧 NAT 分级（TCP punch 门控等）
	hostNatClass natClass

	// guest 玩家中继（relay.go）
	rInFlight      bool
	rDone          bool
	rAttempt       int64 // 中继尝试轮次号：重试/退房时递增，用于作废上一轮在途收尾
	rResult        chan string
	rSock          *net.UDPConn
	rPuncher       *puncher
	rRudp          *rudpConn
	rBridge        *tcpBridge
	rConnectedHint bool
	rAsNodeStop    func()

	// TURN 中继（turn_flow.go）
	turnInFlight      bool
	turnSession       *turnSession
	stdTurnSession    *stdTurnSession
	turnTransport     *rudpConn
	turnBridge        *tcpBridge
	turnKeepaliveStop func()
	turnReadyApplied  bool
	turnPeerID        string

	// host 侧状态
	hPeers map[string]*hostPeer
	// 本会话已向某候选派发过几次 relay_setup（挑最闲的候选用）
	hRelayDispatched map[string]int
}

func newConnEngine(a *App) *connEngine {
	return &connEngine{app: a}
}

// bindSession 会话启动时绑定引擎（guest 自动开始 p2p）。
func (e *connEngine) bindSession(s *session, clientID string) {
	ctx, cancel := context.WithCancel(context.Background())
	room := s.app.snapshotRoom()

	e.mu.Lock()
	e.sess = s
	e.code = s.code
	e.token = s.token
	e.isHost = s.isHost
	e.clientID = clientID
	e.ctx = ctx
	e.cancel = cancel
	if room != nil {
		e.hostPort = roomInt(room, "hostPort")
		e.hostIp = roomStr(room, "hostIp")
	}
	if s.isHost {
		e.hPeers = map[string]*hostPeer{}
	} else {
		e.gMappedCh = make(chan *stunMappedAddr, 4)
	}
	isHost := s.isHost
	e.mu.Unlock()

	e.app.netLog("info", fmt.Sprintf("连接引擎就绪（role=%s）", roleStr(isHost)))
	if !isHost {
		// JoinRoom 成功自动开始 p2p
		e.notifyState(phaseP2P, statusTrying, "", "")
	}
}

func roleStr(isHost bool) string {
	if isHost {
		return "host"
	}
	return "guest"
}

func (e *connEngine) currentHostPort() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hostPort
}

// unbindSession 会话结束（LeaveRoom / fatal / 应用退出）时清理。幂等，只对本会话生效。
func (e *connEngine) unbindSession(s *session) {
	e.mu.Lock()
	if e.sess == nil || e.sess != s {
		e.mu.Unlock()
		return
	}
	e.app.netLog("info", "连接引擎停止")
	e.mu.Unlock()
	e.teardown()
}

// teardown 全量清理：停打洞、关 socket、拆桥、尽力通知对端 disconnect。
func (e *connEngine) teardown() {
	e.teardownTurn(false)
	e.mu.Lock()
	if e.cancel != nil {
		e.cancel()
	}
	// 快照并置空全部资源引用
	gPuncher, gSock, gRudp, gBridge := e.gPuncher, e.gPunchSock, e.gRudp, e.gBridge
	gTcpLn, gTcpConn := e.gTcpLn, e.gTcpConn
	rPuncher, rSock, rRudp, rBridge := e.rPuncher, e.rSock, e.rRudp, e.rBridge
	asNodeStop := e.rAsNodeStop
	peers := e.hPeers
	// 断开通知所需快照（复位前抓取；复位后 postSignal 读不到会话字段）
	sendBye := e.sess != nil && e.code != "" && e.token != ""
	byeCode, byeToken, byeBase := e.code, e.token, e.app.baseURL()
	byeTo := "host"
	byeIsHost := e.isHost
	if byeIsHost {
		byeTo = ""
	}
	// 引擎复位
	e.sess = nil
	e.code = ""
	e.token = ""
	e.clientID = ""
	e.isHost = false
	e.hostPort = 0
	e.hostIp = ""
	e.gOffer = nil
	e.gLastOffer = nil
	e.gMapped = nil
	e.gPunchSock = nil
	e.gPuncher = nil
	e.gRudp = nil
	e.gBridge = nil
	e.gTcpLn = nil
	e.gTcpConn = nil
	e.gP2PDone = false
	e.gActive = false
	e.gMappedCh = nil
	e.punchRecvEver = false
	e.predictionOffCount = 0
	e.retryRound = 0
	e.tcpPunchInFlight = false
	e.gMyMappedIP = ""
	e.hostNatClass = natUnknown
	e.turnInFlight = false
	e.turnSession = nil
	e.stdTurnSession = nil
	e.turnTransport = nil
	e.turnBridge = nil
	e.turnKeepaliveStop = nil
	e.turnReadyApplied = false
	e.turnPeerID = ""
	e.rInFlight = false
	e.rDone = false
	e.rAttempt++ // 作废上一轮中继的在途打洞/看门狗，防止退房后晚到的隧道写回新会话
	e.rResult = nil
	e.rSock = nil
	e.rPuncher = nil
	e.rRudp = nil
	e.rBridge = nil
	e.rConnectedHint = false
	e.rAsNodeStop = nil
	e.hPeers = nil
	e.hRelayDispatched = nil
	e.mu.Unlock()

	// 尽力通知对端（模组离开时也发 disconnect）
	if sendBye {
		_ = e.app.api.post(byeBase, "/signal/send", map[string]any{
			"code":   byeCode,
			"token":  byeToken,
			"isHost": byeIsHost,
			"type":   "disconnect",
			"data":   map[string]any{},
			"to":     byeTo,
		}, nil)
	}

	// 关闭资源（rudp Close 会连带停桥的接收侧；bridge.stop 关 TCP 侧）
	if gPuncher != nil {
		gPuncher.stop()
	}
	if gSock != nil {
		_ = gSock.Close()
	}
	if gBridge != nil {
		gBridge.stop()
	}
	if gRudp != nil {
		_ = gRudp.Close()
	}
	if gTcpLn != nil {
		_ = gTcpLn.Close()
	}
	if gTcpConn != nil {
		_ = gTcpConn.Close()
	}
	if rPuncher != nil {
		rPuncher.stop()
	}
	if rSock != nil {
		_ = rSock.Close()
	}
	if rBridge != nil {
		rBridge.stop()
	}
	if rRudp != nil {
		_ = rRudp.Close()
	}
	if asNodeStop != nil {
		asNodeStop()
	}
	for _, p := range peers {
		if p.puncher != nil {
			p.puncher.stop()
		}
		if p.rudp != nil {
			_ = p.rudp.Close()
		}
	}
}

// ---- 信令发送 ----

// sendSignal 发送一条信令（POST /signal/send，读取当前会话快照后外发）。
func (e *connEngine) sendSignal(typ string, data map[string]any, to string) error {
	e.mu.Lock()
	code, token, isHost := e.code, e.token, e.isHost
	base := e.app.baseURL()
	e.mu.Unlock()
	if code == "" || token == "" {
		return fmt.Errorf("会话未绑定")
	}
	body := map[string]any{
		"code":   code,
		"token":  token,
		"isHost": isHost,
		"type":   typ,
		"data":   data,
		"to":     to,
	}
	return e.app.api.post(base, "/signal/send", body, nil)
}

// ---- 信令分发（session.handleSignals 调用）----

// engineSignals 引擎关心的信令类型。
var engineSignals = map[string]bool{
	"join_request":     true,
	"holepunch_offer":  true,
	"holepunch_mapped": true,
	"punch_info":       true,
	"peer_port":        true,
	"disconnect":       true,
	"relay_request":    true,
	"relay_notify":     true,
	"relay_accept":     true,
	"relay_declined":   true,
	"relay_setup":      true,
	"relay_ready":      true,
	"turn_alloc":       true,
	"turn_ready":       true,
	"turn_nack":        true,
	"ice_restart":      true,
	"tcp_punch_info":   true,
	"tcp_punch_go":     true,
}

// onSignal 把一条信令交给引擎（只处理引擎关心的类型）。
func (e *connEngine) onSignal(typ, from string, data map[string]any) {
	if !engineSignals[typ] {
		return
	}
	e.mu.Lock()
	sess := e.sess
	e.mu.Unlock()
	if sess == nil || sess.isDone() {
		return
	}
	// 信令来源门控（与 server-go/signal_auth.go 的服务端矩阵配套的第二道防线）。
	// 服务端把 from 绑成 token 推导出的真实身份：房主侧发出的任何信令 from 恒为 "host"，
	// 房客侧恒为其 clientId，因此本端可按角色判定归属。修复前任一房客发
	// host_closing / turn_alloc / relay_setup / peer_port / ice_restart，
	// 就能让本端拆线、把本地 MC 端口桥到它指定地址、或改写打洞目标。
	if !e.signalFromAllowed(typ, from) {
		e.app.netLog("warn", "忽略来源不符的信令: "+typ)
		return
	}

	switch typ {
	case "join_request":
		if e.roleIsHost() {
			e.hostOnJoinRequest(from, data)
		}
	case "holepunch_offer":
		if !e.roleIsHost() {
			e.guestOnOffer(from, data)
		}
	case "holepunch_mapped":
		if !e.roleIsHost() {
			e.guestOnMapped(data)
		}
	case "punch_info":
		if e.roleIsHost() {
			e.hostOnPunchInfo(from, data)
		}
	case "peer_port":
		e.onPeerPort(data)
	case "disconnect":
		e.onPeerDisconnect(from)
	case "relay_request":
		if e.roleIsHost() {
			e.hostOnRelayRequest(from, data)
		}
	case "relay_notify":
		if !e.roleIsHost() {
			e.guestOnRelayNotify(from, data)
		}
	case "relay_declined":
		if !e.roleIsHost() {
			e.guestRelayFail(e.relayAttemptCur(), "房主或中继者拒绝了中继请求")
		}
	case "relay_setup":
		if !e.roleIsHost() {
			e.guestOnRelaySetup(from, data)
		}
	case "relay_accept":
		if e.roleIsHost() {
			e.hostOnRelayAccept(data)
		} else {
			e.app.netLog("info", "中继节点已接受")
		}
	case "relay_ready":
		e.app.netLog("info", "房客中继就绪")
	case "turn_alloc":
		if e.roleIsHost() {
			e.hostOnTurnAlloc(from, data)
		}
	case "turn_ready":
		if !e.roleIsHost() {
			e.guestOnTurnReady(data)
		}
	case "turn_nack":
		if !e.roleIsHost() {
			e.guestOnTurnNack(data)
		}
	case "ice_restart":
		e.onRequestIceRestart(from)
	case "tcp_punch_info":
		if e.roleIsHost() {
			e.hostOnTcpPunchInfo(from, data)
		}
	case "tcp_punch_go":
		if !e.roleIsHost() {
			e.guestOnTcpPunchGo(data)
		}
	}
}

// signalFromAllowed 判定一条信令的来源是否与本端角色相称。
//   - 房客：只接受房主/服务端权威信令（from=="host"）
//   - 房主：只接受自家房客；join_request 例外（服务端在新房客加入时代为注入，
//     此时该房客尚未登记进 hPeers）；"host" 来源覆盖服务端注入的 room_evicted 等
func (e *connEngine) signalFromAllowed(typ, from string) bool {
	if e.roleIsHost() {
		if typ == "join_request" || from == "host" {
			return true
		}
		e.mu.Lock()
		_, known := e.hPeers[from]
		e.mu.Unlock()
		return known
	}
	return from == "host"
}

func (e *connEngine) roleIsHost() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.isHost
}

// ---- 通用工具 ----

func roomStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func roomInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func sigStr(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func sigInt(data map[string]any, key string) int { return roomInt(data, key) }

func sigInt64(data map[string]any, key string) int64 {
	switch v := data[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func sigInts(data map[string]any, key string) []int {
	arr, ok := data[key].([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(arr))
	for _, item := range arr {
		out = append(out, roomInt(map[string]any{"v": item}, "v"))
	}
	return out
}

func sigBool(data map[string]any, key string) bool {
	if v, ok := data[key].(bool); ok {
		return v
	}
	return false
}

// udpAddrOf 由 STUN 映射地址构造 UDP 目标。
func udpAddrOf(m *stunMappedAddr) *net.UDPAddr {
	if m == nil {
		return nil
	}
	ip := net.ParseIP(m.IP)
	if ip == nil || m.Port <= 0 {
		return nil
	}
	return &net.UDPAddr{IP: ip, Port: m.Port}
}

// getLocalIP 取本机局域网 IP（UDP connect 探测默认路由出口，不实际发包）。
func getLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

func localPort(uc *net.UDPConn) int {
	if a, ok := uc.LocalAddr().(*net.UDPAddr); ok {
		return a.Port
	}
	return 0
}

// beginGuestCycle cycle CAS：防重复 holepunch_offer / 迟到信号重入。
func (e *connEngine) beginGuestCycle() (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gActive || e.gP2PDone {
		return e.gCycle, false
	}
	e.gActive = true
	e.gCycle++
	return e.gCycle, true
}

func (e *connEngine) endGuestCycle() {
	e.mu.Lock()
	e.gActive = false
	e.mu.Unlock()
}

func (e *connEngine) genValid(gen int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sess != nil && gen == e.gCycle
}

// closeGuestPunchSock 关闭 p2p 打洞 socket，并在它正是登记中的那一个时同步清掉引用。
// 只关不清会让玩家中继路径（relay.go 的 reused 判据）把一个已关闭的 socket 当作
// 可复用句柄拿去打洞——而"可尝试中继"正是终局判定时提示给玩家的动作。
func (e *connEngine) closeGuestPunchSock(sock *net.UDPConn) {
	if sock == nil {
		return
	}
	_ = sock.Close()
	e.mu.Lock()
	if e.gPunchSock == sock {
		e.gPunchSock = nil
	}
	e.mu.Unlock()
}

func (e *connEngine) ctxDone() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == nil {
		return nil
	}
	return e.ctx.Done()
}

func (e *connEngine) mappedChan() chan *stunMappedAddr {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gMappedCh == nil {
		e.gMappedCh = make(chan *stunMappedAddr, 4)
	}
	return e.gMappedCh
}

// ---- guest：p2p 打洞流程 ----

// guestOnOffer 收到 host 的 holepunch_offer（模组 handleHolePunchOffer 对应）。
func (e *connEngine) guestOnOffer(from string, data map[string]any) {
	gen, ok := e.beginGuestCycle()
	if !ok {
		e.app.netLog("warn", "忽略重复/迟到的 holepunch_offer（周期进行中或已连通）")
		return
	}
	e.mu.Lock()
	e.gOffer = copyMap(data)
	e.gLastOffer = copyMap(data)
	if ip := sigStr(data, "hostIp"); ip != "" {
		e.hostIp = ip
	}
	if port := sigInt(data, "hostPort"); port > 0 {
		e.hostPort = port
	}
	if ip := sigStr(data, "hostMappedIp"); ip != "" && sigInt(data, "hostMappedPort") > 0 {
		e.gMapped = &stunMappedAddr{IP: ip, Port: sigInt(data, "hostMappedPort")}
	}
	if d := sigInt(data, "hostMappedPortDelta"); d != 0 {
		e.gMappedDelta = d
	}
	if r := sigInt(data, "hostMappedPortRange"); r > 0 {
		e.gHostPortRange = r
	}
	if sigBool(data, "hostSymmetric") {
		e.gRemoteNat = natHardSym
		if sigBool(data, "hostEasySym") {
			e.gRemoteNat = natEasySym
		}
	}
	offer := copyMap(data)
	e.mu.Unlock()

	go e.guestPunchFlow(gen, offer, from)
}

// guestPunchFlow guest 打洞主流程（对齐模组 ConnectionManager 打洞编排）：
// socket → STUN 分级 → punch_info → 等 mapped/齐射 → 档位推荐 + PunchTuner 自适应
// 无限轮次循环（仅"连续 20 轮零收包"或"PREDICTION_OFF 封顶 50 且零收包"允许终局）。
//
// 关键约束：
//   - 轮次无上限。PREDICTION_OFF_CAP=50 / ZERO_RECV_FINAL_ROUND_LIMIT=20 均只在
//     整会话从未收到对端任何 UDP 包时才判定终局（到达后仅停止直连打洞，
//     TURN/玩家中继按钮仍可用，是否使用由玩家决定）。
//   - 每轮用 PunchTuner.nextParams 调参；轮次耗尽升级档位（DEFAULT→AGGRESSIVE→HARDSYM）。
//   - 心跳闭环：链路死亡（rudp 判死/MC 断开）→ requestIceRestart + 重新打洞。
func (e *connEngine) guestPunchFlow(gen int, offer map[string]any, from string) {
	defer e.endGuestCycle()

	// 全程共用同一个打洞 socket：失败保留复用，跨周期不重绑。
	sock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		e.app.netLog("error", "创建打洞 socket 失败: "+err.Error())
		e.notifyState(phaseP2P, statusFailed, "", "创建打洞 socket 失败")
		return
	}

	// STUN 采样（本端 NAT 分级 + punch_info 映射地址）
	samples := stunSampleSeries(sock, stunServers, 3, 2, 1500*time.Millisecond)
	var mine *stunMappedAddr
	if len(samples) > 0 {
		mine = &samples[0]
	} else {
		e.app.netLog("warn", "STUN 探测全部失败，punch_info 不带映射地址")
	}
	samplePorts := make([]int, len(samples))
	for i, s := range samples {
		samplePorts[i] = s.Port
	}
	localNat := natClassFromDelta(samplePorts)
	if mine != nil {
		e.mu.Lock()
		e.gMyMappedIP = mine.IP
		e.mu.Unlock()
	}

	// 对端 NAT 分级：offer 标志（host 自报）> 房间 natType
	remoteNat := natClassFromRoomType(roomStr(e.app.snapshotRoom(), "natType"))
	if sigBool(offer, "hostSymmetric") {
		remoteNat = natHardSym
		if sigBool(offer, "hostEasySym") {
			remoteNat = natEasySym
		}
	}

	// punch_info（带本端对称标志与 delta/range，供 host 反向打洞预测）
	punchData := map[string]any{}
	if mine != nil {
		punchData["joinerMappedIp"] = mine.IP
		punchData["joinerMappedPort"] = mine.Port
	}
	if lip := getLocalIP(); lip != "" {
		punchData["joinerLocalIp"] = lip
	}
	if localNat.symmetric() {
		punchData["joinerSymmetric"] = true
		if localNat == natEasySym {
			punchData["joinerEasySym"] = true
		}
		if d := stunDeltaFromSamples(samples); d != 0 {
			punchData["joinerMappedPortDelta"] = d
		}
	}
	if err := e.sendSignal("punch_info", punchData, "host"); err != nil {
		e.app.netLog("warn", "发送 punch_info 失败: "+err.Error())
	}

	// 齐射时刻：host 墙钟+3000（容忍时钟差，最多等 5s）
	startAt := time.Now()
	if syncMs := sigInt64(offer, "punchSyncTimeMs"); syncMs > 0 {
		st := time.UnixMilli(syncMs)
		if d := time.Until(st); d > 0 && d <= punchSyncMaxWait {
			startAt = st
		} else if d > punchSyncMaxWait {
			e.app.netLog("warn", fmt.Sprintf("punchSyncTimeMs 时钟偏差过大（%v），忽略齐射同步", d))
		}
	}

	// 等 holepunch_mapped（齐射时刻前；offer 已带 mapped 则到点即走）
	waitDur := time.Until(startAt)
	if waitDur < 0 {
		waitDur = 0
	}
	if waitDur > offerWaitMappedMax {
		waitDur = offerWaitMappedMax
	}
	var hostMapped *stunMappedAddr
	timer := time.NewTimer(waitDur)
	defer timer.Stop()
	select {
	case m := <-e.mappedChan():
		hostMapped = m
	case <-timer.C:
	case <-e.ctxDone():
		_ = sock.Close()
		return
	}
	if hostMapped == nil {
		e.mu.Lock()
		hostMapped = e.gMapped
		e.mu.Unlock()
	}

	// 目标选择：holepunch_mapped > offer.hostMapped > hostIp:hostPort
	var target *net.UDPAddr
	e.mu.Lock()
	hostIp, hostPort := e.hostIp, e.hostPort
	e.mu.Unlock()
	switch {
	case hostMapped != nil && udpAddrOf(hostMapped) != nil:
		target = udpAddrOf(hostMapped)
	case hostIp != "" && hostPort > 0:
		target = &net.UDPAddr{IP: net.ParseIP(hostIp), Port: hostPort}
	default:
		_ = sock.Close()
		e.app.netLog("error", "holepunch_offer 缺少可用目标地址")
		e.notifyState(phaseP2P, statusFailed, "", "缺少打洞目标地址")
		return
	}

	// 档位推荐 + punchAuthV1 密钥
	profile := recommendProfile(localNat, remoteNat)
	punchProfileSwitch(profile, "nat_matrix_"+localNat.String()+"x"+remoteNat.String())
	e.app.netLog("info", "NAT 分级 local="+localNat.String()+" remote="+remoteNat.String()+" → 档位 "+profile.name)

	roomCode := ""
	authKey := func() []byte {
		room := e.app.snapshotRoom()
		roomCode = roomStr(room, "code")
		if roomCode == "" {
			e.mu.Lock()
			roomCode = e.code
			e.mu.Unlock()
		}
		if hostSupportsCap(room, "punchAuthV1") {
			e.mu.Lock()
			cid := e.clientID
			e.mu.Unlock()
			if key := punchAuthDeriveDirectKey(roomCode, cid); key != nil {
				e.app.netLog("info", "打洞认证模式已启用（punchAuthV1）")
				return key
			}
		}
		return nil
	}()

	hostPortRange := sigInt(offer, "hostMappedPortRange")

	// 一次性 sendSignal hook：每周期切换目标/预测时复用。
	punchOnPeer := func(addr *net.UDPAddr) {
		_ = e.sendSignal("peer_port", map[string]any{"peer_ip": addr.IP.String(), "peer_port": addr.Port}, "host")
	}

	e.mu.Lock()
	e.gPunchSock = sock
	e.mu.Unlock()

	params := punchParamsFromProfile(profile)
	var lastFailure *punchFailureReason
	var lastResult *punchResult
	cycle := 0
	for {
		// 会话已结束 → 静默退出
		select {
		case <-e.ctxDone():
			e.closeGuestPunchSock(sock)
			return
		default:
		}
		if !e.genValid(gen) {
			e.closeGuestPunchSock(sock)
			return
		}
		// 已走 direct/prelay/TURN 通道成功 → p2p 不再尝试（先到者优先）
		e.mu.Lock()
		alreadyDone := e.gP2PDone
		e.mu.Unlock()
		if alreadyDone {
			e.closeGuestPunchSock(sock)
			return
		}

		// 终局判定（两个常量均要求整会话零收包）
		e.mu.Lock()
		recvEver := e.punchRecvEver
		offCount := e.predictionOffCount
		round := e.retryRound
		e.mu.Unlock()
		if offCount >= predictionOffCap && !recvEver {
			e.app.netLog("warn", "PREDICTION_OFF 封顶（50）且零收包，终止直连打洞")
			e.notifyState(phaseP2P, statusFailed, "", "端口预测已封顶，可尝试中继")
			e.closeGuestPunchSock(sock)
			return
		}
		if round >= zeroRecvFinalRoundLimit && !recvEver {
			e.app.netLog("warn", "持续重试 20 轮零收包，判定终局")
			e.notifyState(phaseP2P, statusFailed, "", "对端无响应，可尝试中继")
			e.closeGuestPunchSock(sock)
			return
		}

		// 当前最新目标（host 侧可能发来新的 mapped / peer_port）
		e.mu.Lock()
		curTarget := target
		curMapped := e.gMapped
		e.mu.Unlock()
		if curMapped != nil && udpAddrOf(curMapped) != nil {
			curTarget = udpAddrOf(curMapped)
		}

		// 对端 NAT 分级/范围随信令纠偏（模组：新证据即重推荐档位）
		e.mu.Lock()
		peerNat := e.gRemoteNat
		peerRange := e.gHostPortRange
		e.mu.Unlock()
		if peerNat != natUnknown && peerNat != remoteNat {
			remoteNat = peerNat
			if next := recommendProfile(localNat, remoteNat); next != profile {
				profile = next
				punchProfileSwitch(profile, "peer_nat_update_"+remoteNat.String())
				e.app.netLog("info", "对端 NAT 更新 → 档位 "+profile.name)
			}
		}
		if peerRange > 0 {
			hostPortRange = peerRange
		}

		// PunchTuner 自适应调参
		params = tunerNextParams(profile, localNat, remoteNat, cycle, profile.maxCycles, lastFailure, lastResult)
		strategy := selectStrategy(localNat, remoteNat, cycle, false)

		p := newPuncher(sock, profile, params, authKey)
		p.setLogf(e.app.netLog)
		p.setTarget(curTarget)
		p.setOnPeer(punchOnPeer)
		e.mu.Lock()
		e.gPuncher = p
		e.mu.Unlock()

		// 首轮等到齐射时刻再开打；后续轮直接发
		if cycle == 0 {
			if d := time.Until(startAt); d > 0 {
				select {
				case <-e.ctxDone():
					p.cancel()
					e.closeGuestPunchSock(sock)
					return
				case <-time.After(d):
				}
			}
		}
		select {
		case <-e.ctxDone():
			p.cancel()
			e.closeGuestPunchSock(sock)
			return
		default:
		}

		basePort := curTarget.Port
		predRange := hostPortRange
		if predRange <= 0 {
			predRange = params.portRange
		}

		var res *punchResult
		switch {
		case profile == punchEasySymDual:
			// 双易对称：多 socket 对轰（RECIPE bomb 窗口）
			res = p.punchEasySymDual(curTarget)
		case profile == punchHardSym:
			// 硬对称：socket 组全端口洗牌喷洒（PpsLimiter 3000）
			group := makePunchSocketGroup(sock, profile.hardSymSocketCount)
			res = p.punchGroup(group, 0)
		case strategy == strategyReverseOnly && cycle == 0:
			// 锥×硬对称首轮：本端反向优先，只听不发（对端扫我）
			res = p.punchPrediction(basePort, 0, false)
		case predRange <= 0:
			res = p.punchPrediction(basePort, 0, false)
		default:
			res = p.punchPrediction(basePort, predRange, false)
		}

		if res != nil && res.success {
			winConn, actual := p.win()
			if winConn == nil {
				winConn = sock
			}
			if !e.genValid(gen) {
				p.cancel()
				_ = winConn.Close()
				e.closeGuestPunchSock(sock)
				return
			}
			e.mu.Lock()
			alreadyDone = e.gP2PDone
			if !alreadyDone {
				e.gPuncher = nil
				e.gPunchSock = nil
			}
			hostIp, hostPort = e.hostIp, e.hostPort
			e.mu.Unlock()
			if alreadyDone {
				p.cancel()
				_ = winConn.Close()
				e.closeGuestPunchSock(sock)
				return
			}

			rc := newRudp(winConn, actual)
			if authKey != nil {
				rc.setAuthKey(authKey, true)
			}
			rc.setOnRestart(func() { e.guestIceRestart() })
			rc.Start()
			_, bridge, berr := startGuestBridge(rc, func() { e.guestBridgeDown() })
			if berr != nil {
				p.cancel()
				_ = rc.Close()
				e.closeGuestPunchSock(sock)
				e.notifyState(phaseP2P, statusFailed, "", "本地桥建立失败："+berr.Error())
				return
			}
			e.mu.Lock()
			// 写回屏障与写回同锁：建桥期间可能已退房（sess=nil）、已改道（gP2PDone）
			// 或已重开一轮（gCycle 变了）。旧写法只判 gP2PDone，而复位会把它清回 false，
			// 于是往已拆掉的引擎上挂 rudp/bridge → 三个 goroutine + 一个 TCP 监听口留到进程退出。
			if e.gP2PDone || e.sess == nil || gen != e.gCycle {
				e.mu.Unlock()
				p.cancel()
				_ = rc.Close()
				if bridge != nil {
					bridge.stop()
				}
				e.closeGuestPunchSock(sock)
				e.app.netLog("warn", "P2P 隧道建立过晚（本端已改道/已退房/已重开一轮），丢弃")
				return
			}
			e.gRudp = rc
			e.gBridge = bridge
			e.gP2PDone = true
			e.mu.Unlock()
			p.cancel() // 关掉组内未赢 socket（winConn 除外）
			if winConn != sock {
				_ = sock.Close()
			}
			e.app.netLog("info", "P2P 打洞成功，rudp 隧道已建立")
			directAddr := net.JoinHostPort(hostIp, strconv.Itoa(hostPort))
			e.notifyState(phaseP2P, statusSuccess, directAddr, "")
			return
		}

		// 本轮失败：记账 + 调参输入
		if res != nil {
			e.mu.Lock()
			if res.recvPunch > 0 || res.recvAck > 0 {
				e.punchRecvEver = true
			}
			e.mu.Unlock()
			reason := classifyPunchFailure(res)
			if reason == failurePredictionOff {
				e.mu.Lock()
				e.predictionOffCount++
				n := e.predictionOffCount
				e.mu.Unlock()
				if n == predictionOffCap {
					e.app.netLog("warn", "PREDICTION_OFF 累计达 50，本轮会话直连预测打洞将停用")
				}
			}
			lastFailure = &reason
			lastResult = res
		}
		p.cancel() // 关闭组模式附加 socket（单 socket 不受影响）

		// TCP SimOpen 邀约（本端锥形、cycle≥1）
		e.tryTcpHolePunch(cycle, localNat, e.guestMyMappedIP())

		// 轮次推进：cycle 耗尽 → 持续重试轮 +1 + 档位升级 + cycle 归零
		cycle++
		if cycle >= profile.maxCycles {
			e.mu.Lock()
			e.retryRound++
			rr := e.retryRound
			e.mu.Unlock()
			profile = escalateProfileForRound(profile, rr)
			punchProfileSwitch(profile, "continuous_retry_round")
			e.app.netLog("info", "打洞周期耗尽，进入持续重试第 "+strconv.Itoa(rr)+" 轮（档位 "+profile.name+"）")
			cycle = 0
		}

		select {
		case <-e.ctxDone():
			e.closeGuestPunchSock(sock)
			return
		case <-time.After(punchCycleInterval):
		}
	}
}

// makePunchSocketGroup 构造打洞 socket 组：首 socket 复用共享 socket，其余新建。
func makePunchSocketGroup(first *net.UDPConn, count int) []*net.UDPConn {
	if count < 1 {
		count = 1
	}
	group := make([]*net.UDPConn, 0, count)
	if first != nil {
		group = append(group, first)
	}
	for len(group) < count {
		uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			break
		}
		group = append(group, uc)
	}
	return group
}

// escalateProfileForRound 持续重试轮档位升级（DEFAULT→AGGRESSIVE→HARDSYM）。
func escalateProfileForRound(current *punchProfile, round int) *punchProfile {
	if current == punchHardSym {
		return current
	}
	if current == punchAggressive {
		return punchHardSym
	}
	return punchAggressive
}

func (e *connEngine) guestMyMappedIP() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gMyMappedIP
}

// guestOnMapped 收到 host 的 holepunch_mapped。
func (e *connEngine) guestOnMapped(data map[string]any) {
	ip := sigStr(data, "hostMappedIp")
	port := sigInt(data, "hostMappedPort")
	if ip == "" || port <= 0 {
		return
	}
	m := &stunMappedAddr{IP: ip, Port: port}
	e.mu.Lock()
	e.gMapped = m
	if d := sigInt(data, "hostMappedPortDelta"); d != 0 {
		e.gMappedDelta = d
	}
	if r := sigInt(data, "hostMappedPortRange"); r > 0 {
		e.gHostPortRange = r
	}
	if sigBool(data, "hostSymmetric") {
		e.gRemoteNat = natHardSym
		if sigBool(data, "hostEasySym") {
			e.gRemoteNat = natEasySym
		}
	}
	puncher := e.gPuncher
	e.mu.Unlock()

	select {
	case e.mappedChan() <- m:
	default:
	}
	if puncher != nil {
		// 打洞进行中收到：刷新目标（对端纠偏/漂移）；预测范围由 PunchTuner 周期参数接管
		puncher.setTarget(udpAddrOf(m))
	}
}

// onPeerPort 收到 peer_port：对端以其实际源地址纠偏我方打洞目标。
func (e *connEngine) onPeerPort(data map[string]any) {
	ip := sigStr(data, "peer_ip")
	port := sigInt(data, "peer_port")
	if ip == "" || port <= 0 {
		return
	}
	addr := &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
	e.mu.Lock()
	puncher := e.gPuncher
	e.mu.Unlock()
	if puncher != nil {
		puncher.setTarget(addr)
	}
}

// guestBridgeDown MC 断开/桥拆除/链路判死时复位 p2p 状态并重新打洞（心跳闭环）。
func (e *connEngine) guestBridgeDown() {
	e.guestTeardownP2P()
	e.app.netLog("warn", "MC 隧道已断开，发起 ICE 重启重新打洞")
	e.guestIceRestart()
}

// guestTeardownP2P 拆除 guest p2p 隧道与桥（不动 TURN/中继）。
// UDP rudp 桥与 TCP 打洞桥互斥（TCP 只在 p2p 未成时建立），因此这里一并拆是安全的。
func (e *connEngine) guestTeardownP2P() {
	e.mu.Lock()
	rc := e.gRudp
	bridge := e.gBridge
	tcpLn, tcpConn := e.gTcpLn, e.gTcpConn
	e.gRudp = nil
	e.gBridge = nil
	e.gTcpLn = nil
	e.gTcpConn = nil
	e.gP2PDone = false
	e.gActive = false
	e.mu.Unlock()
	if bridge != nil {
		bridge.stop()
	}
	if rc != nil {
		_ = rc.Close()
	}
	if tcpLn != nil {
		_ = tcpLn.Close()
	}
	if tcpConn != nil {
		_ = tcpConn.Close()
	}
}

// guestIceRestart 心跳闭环：链路死亡 → 通知对端 + 以最近一次 offer 重新驱动打洞。
func (e *connEngine) guestIceRestart() {
	// 上线类型是 ice_restart（requestIceRestart 为模组方法名，发错会被服务端拒收）
	_ = e.sendSignal("ice_restart", map[string]any{}, "host")
	e.mu.Lock()
	e.punchRecvEver = false
	e.predictionOffCount = 0
	e.retryRound = 0
	offer := e.gLastOffer
	e.mu.Unlock()
	if offer == nil {
		return
	}
	if gen, ok := e.beginGuestCycle(); ok {
		go e.guestPunchFlow(gen, copyMap(offer), "host")
	}
}

// onRequestIceRestart 收到对端 ice_restart：拆当前链路重新协商。
func (e *connEngine) onRequestIceRestart(from string) {
	e.app.netLog("warn", "收到对端 ICE 重启请求，拆除当前链路重新协商")
	if e.roleIsHost() {
		e.mu.Lock()
		p := e.hPeers[from]
		delete(e.hPeers, from)
		e.mu.Unlock()
		if p != nil {
			if p.puncher != nil {
				p.puncher.cancel()
			}
			if p.rudp != nil {
				_ = p.rudp.Close()
			}
		}
		return
	}
	e.guestTeardownP2P()
	// 重新驱动（本端不再回发 ice_restart，避免回声风暴）
	e.mu.Lock()
	e.punchRecvEver = false
	e.predictionOffCount = 0
	e.retryRound = 0
	offer := e.gLastOffer
	e.mu.Unlock()
	if offer != nil {
		if gen, ok := e.beginGuestCycle(); ok {
			go e.guestPunchFlow(gen, copyMap(offer), "host")
		}
	}
}

// onPeerDisconnect 收到对端 disconnect。
func (e *connEngine) onPeerDisconnect(from string) {
	if e.roleIsHost() {
		e.mu.Lock()
		p := e.hPeers[from]
		delete(e.hPeers, from)
		e.mu.Unlock()
		if p != nil {
			if p.puncher != nil {
				p.puncher.stop()
			}
			if p.rudp != nil {
				_ = p.rudp.Close()
			}
			e.app.netLog("info", "对端已断开")
		}
		return
	}
	// guest：房主断开 → 清理连接状态（保留房间会话），由无限打洞周期循环重新尝试
	e.mu.Lock()
	rc := e.gRudp
	bridge := e.gBridge
	puncher := e.gPuncher
	e.gRudp = nil
	e.gBridge = nil
	e.gP2PDone = false
	e.gActive = false
	e.mu.Unlock()
	if puncher != nil {
		puncher.stop()
	}
	if bridge != nil {
		bridge.stop()
	}
	if rc != nil {
		_ = rc.Close()
	}
	e.app.netLog("warn", "房主已断开，将重新尝试打洞")
}

// ---- host：打洞应答与桥接 ----

// hostOnJoinRequest 收到（服务端代发的）join_request → 起 host 侧引擎。
func (e *connEngine) hostOnJoinRequest(from string, data map[string]any) {
	e.mu.Lock()
	if e.hPeers == nil {
		e.hPeers = map[string]*hostPeer{}
	}
	if old := e.hPeers[from]; old != nil && (old.rudp != nil || old.puncher != nil) {
		e.mu.Unlock()
		e.app.netLog("warn", "忽略重复 join_request")
		return
	}
	// 先同步登记占位（防并发 join_request 双开 socket），重活在 goroutine 里做
	var caps []string
	if arr, ok := data["clientCapabilities"].([]any); ok {
		for _, c := range arr {
			if s, ok := c.(string); ok {
				caps = append(caps, s)
			}
		}
	}
	peer := &hostPeer{id: from, caps: caps}
	e.hPeers[from] = peer
	hostPort := e.hostPort
	e.mu.Unlock()
	if hostPort <= 0 {
		e.app.netLog("error", "收到 join_request 但缺少房间 hostPort")
		e.dropHostPeer(peer)
		return
	}
	go e.hostServeJoin(peer, hostPort)
}

// dropHostPeer 移除尚未建链的 peer（仍是指向同一实例时）。
func (e *connEngine) dropHostPeer(peer *hostPeer) {
	e.mu.Lock()
	cur := e.hPeers[peer.id]
	if cur == peer && cur.rudp == nil {
		delete(e.hPeers, peer.id)
	}
	e.mu.Unlock()
}

// hostServeJoin host 侧连接主流程：绑 hostPort → STUN 分级+预测 → 签发 holepunch_offer →
// 等 punch_info → 轮次循环打洞（PunchTuner 自适应）→ rudp → lazy 桥。
func (e *connEngine) hostServeJoin(peer *hostPeer, hostPort int) {
	// 打洞 socket 优先绑 hostPort（绑定失败退随机；MC 走 TCP，UDP 端口不冲突）
	sock, err := punchListen(hostPort)
	if err != nil {
		e.app.netLog("error", "创建 host 打洞 socket 失败: "+err.Error())
		e.dropHostPeer(peer)
		return
	}

	// STUN 双采样（NAT 分级 + 映射地址）
	samples := stunSampleSeries(sock, stunServers, 2, 2, 1500*time.Millisecond)
	var hostMapped *stunMappedAddr
	if len(samples) > 0 {
		hostMapped = &samples[0]
	}
	samplePorts := make([]int, len(samples))
	for i, s := range samples {
		samplePorts[i] = s.Port
	}
	hostNat := natClassFromDelta(samplePorts)
	e.mu.Lock()
	e.hostNatClass = hostNat
	e.mu.Unlock()

	offer := map[string]any{"hostPort": hostPort}
	e.mu.Lock()
	if e.hostIp != "" {
		offer["hostIp"] = e.hostIp
	}
	e.mu.Unlock()
	if lip := getLocalIP(); lip != "" {
		offer["hostLocalIp"] = lip
	}
	hostDelta := 0
	hostRange := 0
	if hostMapped != nil {
		offer["hostMappedIp"] = hostMapped.IP
		offer["hostMappedPort"] = hostMapped.Port
		hostDelta = stunDeltaFromSamples(samples)
		if hostNat.symmetric() {
			offer["hostSymmetric"] = true
			if hostNat == natEasySym {
				offer["hostEasySym"] = true
			}
			// 对称 NAT：顺序采样 10 次做端口预测，把 delta/range 告知 guest（模组 P-PRE 同款）
			if pd, pr := stunPredictForOffer(sock); pd != 0 || pr > 0 {
				hostDelta = pd
				hostRange = pr
			}
			if hostDelta != 0 {
				offer["hostMappedPortDelta"] = hostDelta
			}
			if hostRange > 0 {
				offer["hostMappedPortRange"] = hostRange
			}
		}
	}
	// peer 的这两个字段会被信令分发协程（hostOnPunchInfo）读，必须与它同一把锁。
	e.mu.Lock()
	peer.hostMapped = hostMapped
	peer.hostDelta = hostDelta
	e.mu.Unlock()

	// RTT 齐射同步：host 墙钟 + 3000（模组 RoomManager 同款）
	offer["punchSyncTimeMs"] = time.Now().Add(3 * time.Second).UnixMilli()

	if err := e.sendSignal("holepunch_offer", offer, peer.id); err != nil {
		e.app.netLog("warn", "发送 holepunch_offer 失败: "+err.Error())
		_ = sock.Close()
		e.dropHostPeer(peer)
		return
	}
	e.app.netLog("info", "已签发 holepunch_offer，等待房客穿透")

	// 等 punch_info（guest 映射地址 + 对称标志），窗口 punchInfoTimeout
	deadline := time.After(punchInfoTimeout)
	for {
		e.mu.Lock()
		cur := e.hPeers[peer.id]
		mapped := peer.mapped
		e.mu.Unlock()
		if mapped != nil {
			break
		}
		if cur != peer {
			_ = sock.Close()
			return
		}
		select {
		case <-e.ctxDone():
			_ = sock.Close()
			e.dropHostPeer(peer)
			return
		case <-deadline:
			e.app.netLog("warn", "等待 punch_info 超时，清理 host 打洞")
			_ = sock.Close()
			e.dropHostPeer(peer)
			return
		case <-time.After(200 * time.Millisecond):
		}
	}

	// punchAuthV1：joiner 声明能力 → 双方派生同一密钥
	var authKey []byte
	if len(peer.caps) > 0 {
		for _, c := range peer.caps {
			if c == "punchAuthV1" {
				e.mu.Lock()
				code := e.code
				e.mu.Unlock()
				authKey = punchAuthDeriveDirectKey(code, peer.id)
				break
			}
		}
	}
	if authKey != nil {
		e.app.netLog("info", "打洞认证模式已启用（punchAuthV1，host 侧）")
	}

	// 轮次循环打洞（对齐 guest 侧：PunchTuner 调参 + 档位升级，会话寿命内无上限）
	e.mu.Lock()
	joinerNat := peer.natClass
	joinerRange0 := peer.joinerPortRange
	e.mu.Unlock()
	if joinerNat == natUnknown {
		joinerNat = natCone
	}
	_ = joinerRange0
	profile := recommendProfile(hostNat, joinerNat)
	punchProfileSwitch(profile, "host_nat_matrix_"+hostNat.String()+"x"+joinerNat.String())
	e.app.netLog("info", "host NAT 分级 local="+hostNat.String()+" joiner="+joinerNat.String()+" → 档位 "+profile.name)

	params := punchParamsFromProfile(profile)
	var lastFailure *punchFailureReason
	var lastResult *punchResult
	cycle := 0
	deadlineMs := time.Now().Add(hostPunchLifetime)
	for time.Now().Before(deadlineMs) {
		select {
		case <-e.ctxDone():
			_ = sock.Close()
			return
		default:
		}
		e.mu.Lock()
		cur := e.hPeers[peer.id]
		won := peer.rudp != nil
		mapped := peer.mapped
		e.mu.Unlock()
		if cur != peer || won {
			_ = sock.Close()
			return
		}
		if mapped == nil {
			_ = sock.Close()
			return
		}

		params = tunerNextParams(profile, hostNat, joinerNat, cycle, profile.maxCycles, lastFailure, lastResult)
		strategy := selectStrategy(hostNat, joinerNat, cycle, false)

		p := newPuncher(sock, profile, params, authKey)
		p.setLogf(e.app.netLog)
		p.setTarget(udpAddrOf(mapped))
		p.setOnPeer(func(addr *net.UDPAddr) {
			_ = e.sendSignal("peer_port", map[string]any{"peer_ip": addr.IP.String(), "peer_port": addr.Port}, peer.id)
		})
		e.mu.Lock()
		peer.puncher = p
		e.mu.Unlock()

		e.mu.Lock()
		joinerRange := peer.joinerPortRange
		e.mu.Unlock()
		if joinerRange <= 0 {
			joinerRange = params.portRange
		}
		var res *punchResult
		switch {
		case profile == punchHardSym:
			group := makePunchSocketGroup(sock, profile.hardSymSocketCount)
			res = p.punchGroup(group, 0)
		case strategy == strategyReverseOnly && cycle == 0:
			res = p.punchPrediction(mapped.Port, 0, false)
		case joinerRange <= 0:
			res = p.punchPrediction(mapped.Port, 0, false)
		default:
			res = p.punchPrediction(mapped.Port, joinerRange, false)
		}

		if res != nil && res.success {
			winConn, actual := p.win()
			if winConn == nil {
				winConn = sock
			}
			rc := newRudp(winConn, actual)
			if authKey != nil {
				rc.setAuthKey(authKey, true)
			}
			rc.setOnRestart(func() {
				e.app.netLog("warn", "host 侧收到 ICE 重启请求，重新协商该房客")
				e.hostRestartPeer(peer.id)
			})
			rc.Start()
			e.mu.Lock()
			cur := e.hPeers[peer.id]
			if cur != peer || peer.rudp != nil {
				e.mu.Unlock()
				_ = rc.Close()
				p.cancel()
				_ = sock.Close()
				return
			}
			peer.rudp = rc
			peer.puncher = nil
			mcPort := e.hostPort
			e.mu.Unlock()
			p.cancel() // 关组内未赢 socket
			if winConn != sock {
				_ = sock.Close()
			}
			e.app.netLog("info", "host 端 rudp 隧道已建立，等待 MC 握手")
			startHostLazyBridge(rc, mcPort, e.app.netLog)
			return
		}

		if res != nil {
			reason := classifyPunchFailure(res)
			lastFailure = &reason
			lastResult = res
		}
		p.cancel()

		cycle++
		if cycle >= profile.maxCycles {
			profile = escalateProfileForRound(profile, 0)
			punchProfileSwitch(profile, "host_continuous_retry")
			cycle = 0
		}

		select {
		case <-e.ctxDone():
			_ = sock.Close()
			return
		case <-time.After(punchCycleInterval):
		}
	}
	_ = sock.Close()
	e.mu.Lock()
	cur := e.hPeers[peer.id]
	if cur == peer && cur.rudp == nil {
		delete(e.hPeers, peer.id)
	}
	e.mu.Unlock()
	e.app.netLog("warn", "host 打洞等待窗口耗尽")
}

// hostRestartPeer 拆除某房客链路并重新走 join 协商（ICE 重启 host 侧）。
func (e *connEngine) hostRestartPeer(peerID string) {
	e.mu.Lock()
	p := e.hPeers[peerID]
	delete(e.hPeers, peerID)
	var caps []string
	if p != nil {
		caps = p.caps
	}
	e.mu.Unlock()
	if p != nil {
		if p.puncher != nil {
			p.puncher.cancel()
		}
		if p.rudp != nil {
			_ = p.rudp.Close()
		}
	}
	if caps != nil || p != nil {
		e.hostOnJoinRequest(peerID, map[string]any{"clientId": peerID, "clientCapabilities": toAnySlice(caps)})
	}
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// hostOnPunchInfo 收到 guest punch_info → 回 holepunch_mapped 并开始向 guest 打洞。
func (e *connEngine) hostOnPunchInfo(from string, data map[string]any) {
	ip := sigStr(data, "joinerMappedIp")
	port := sigInt(data, "joinerMappedPort")
	e.mu.Lock()
	peer := e.hPeers[from]
	var hm *stunMappedAddr
	var hd int
	if peer != nil {
		if ip != "" && port > 0 {
			peer.mapped = &stunMappedAddr{IP: ip, Port: port}
		}
		// joiner 对称标志 + delta/range（反向打洞预测用；easySym 为对端权威判定）
		if sigBool(data, "joinerSymmetric") {
			peer.natClass = natHardSym
			if sigBool(data, "joinerEasySym") || absInt(sigInt(data, "joinerMappedPortDelta")) < 100 {
				peer.natClass = natEasySym
			}
		} else {
			peer.natClass = natCone
		}
		if r := sigInt(data, "joinerMappedPortRange"); r > 0 {
			peer.joinerPortRange = r
		}
		// 我方映射是本 goroutine 之外写入的，必须在这把锁里取快照
		hm, hd = peer.hostMapped, peer.hostDelta
	}
	e.mu.Unlock()
	if peer == nil {
		return
	}

	// 回 holepunch_mapped（对齐模组字段：hostMappedIp/hostMappedPort/hostMappedPorts）
	mappedData := map[string]any{}
	if hm != nil {
		mappedData["hostMappedIp"] = hm.IP
		mappedData["hostMappedPort"] = hm.Port
		mappedData["hostMappedPorts"] = []int{hm.Port}
		if hd != 0 {
			mappedData["hostMappedPortDelta"] = hd
		}
	}
	if lip := getLocalIP(); lip != "" {
		mappedData["hostLocalIp"] = lip
	}
	if err := e.sendSignal("holepunch_mapped", mappedData, from); err != nil {
		e.app.netLog("warn", "发送 holepunch_mapped 失败: "+err.Error())
	}

	// 打洞进行中：刷新目标（对端纠偏/漂移）
	e.mu.Lock()
	pn := peer.puncher
	mp := peer.mapped
	e.mu.Unlock()
	if pn != nil && mp != nil {
		pn.setTarget(udpAddrOf(mp))
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---- TryDirect：直连探测 ----

// tryDirectContext 直连探测防并发。
var directMu sync.Mutex

// tryDirect TCP 直连探测 hostIp:hostPort，成功即回报（探测连接随即关闭）。
func (e *connEngine) tryDirect() error {
	e.mu.Lock()
	sess := e.sess
	hostIp, hostPort := e.hostIp, e.hostPort
	e.mu.Unlock()
	if sess == nil {
		return fmt.Errorf("当前没有房间会话")
	}
	if !directMu.TryLock() {
		return fmt.Errorf("直连探测已在进行中")
	}
	defer directMu.Unlock()

	go func() {
		e.notifyState(phaseDirect, statusTrying, "", "")
		if hostIp == "" || hostPort <= 0 {
			e.app.netLog("warn", "直连探测缺少目标")
			e.notifyState(phaseDirect, statusFailed, "", "缺少房主地址（尚未收到 holepunch_offer）")
			return
		}
		target := net.JoinHostPort(hostIp, strconv.Itoa(hostPort))
		for i := 0; i < directAttempts; i++ {
			conn, err := net.DialTimeout("tcp", target, directDialTimeout)
			if err == nil {
				_ = conn.Close() // 探测成功即关闭，玩家自己用 MC 连
				e.app.netLog("info", "直连探测成功")
				e.notifyState(phaseDirect, statusSuccess, target, "")
				return
			}
			e.app.netLog("warn", "直连探测未通")
			if i < directAttempts-1 {
				select {
				case <-e.ctxDone():
					return
				case <-time.After(directRetryInterval):
				}
			}
		}
		e.notifyState(phaseDirect, statusFailed, "", "直连探测未通")
	}()
	return nil
}

// ---- DetectMcPorts：netstat + tasklist ----

// DetectMcPorts 实现见 mc_ports.go（独立文件）。
