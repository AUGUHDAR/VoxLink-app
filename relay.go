package main

// relay.go — 玩家中继（尽力互通模组 RelayBridge / ConnectionManager 的 relay_* 信令）：
//   - guest 发 relay_request {clientId} to="host"（20s 无响应 failed）。
//   - 收 relay_notify 分派：{relayIp,relayPort} → 用打洞 socket punch 中继者（15s）→
//     建 rudp → 本地桥 → emit success → 发 relay_ready {clientId} to="host"；
//     {connected:true} → 中继链路已就绪（模组间互桥），等我们到中继者的 rudp 起来同样处理。
//   - 失败/被拒：relay_declined 或超时 → failed。
//   - host 收 relay_request：查 GET /relay/candidates?excludeRoom=<本房>（候选池=除本房外所有
//     房间，服务端已滤 sym/unknown 并洗牌）→ 本地再排 requester/self/自家房客，按已派发次数
//     取最闲的一个 → 向其发 relay_setup {targetClientId, targetIp, targetPort, replyHostId}
//     （服务端跨房间转发）；同时给 requester 回 relay_notify {relayIp, relayPort}。
//     候选为空 → relay_declined。收候选的 relay_accept {forClientId} → 转告该房客
//     relay_notify {connected:true}。
//   - 我们作为中继节点（guest 收 relay_setup）：与 target punch（15s）→ 若已有到 host 的
//     rudp 则做流搬运桥（host rudp ↔ target rudp 互拷字节）→ 回 relay_accept {forClientId}
//     to="host"；没有到 host 的隧道则 declined。

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

// punchRelayOnce 对给定目标做单 socket 直打（玩家中继路径，无端口预测）。
// 成功返回赢下的 socket 与对端实际地址；调用方负责失败时关闭 sock。
func punchRelayOnce(sock *net.UDPConn, target *net.UDPAddr, timeout time.Duration) (*net.UDPConn, *net.UDPAddr, error) {
	profile := punchProfileNow()
	params := punchParamsFromProfile(profile)
	params.timeoutMs = int(timeout / time.Millisecond)
	p := newPuncher(sock, profile, params, nil)
	p.setTarget(target)
	res := p.punchPrediction(target.Port, 0, false)
	if res == nil || !res.success {
		return nil, nil, fmt.Errorf("打洞未成功")
	}
	win, actual := p.win()
	if win == nil {
		win = sock
	}
	if actual == nil {
		return win, target, nil
	}
	return win, actual, nil
}

// usePlayerRelay guest 请求玩家中继（App.UsePlayerRelay 调用）。
func (e *connEngine) usePlayerRelay() error {
	e.mu.Lock()
	sess := e.sess
	isHost := e.isHost
	inFlight := e.rInFlight
	live := e.rRudp != nil || e.rBridge != nil
	clientID := e.clientID
	e.mu.Unlock()

	if sess == nil {
		return fmt.Errorf("当前没有房间会话")
	}
	if isHost {
		return fmt.Errorf("房主无需玩家中继")
	}
	if inFlight {
		return fmt.Errorf("玩家中继已在进行中")
	}
	if live {
		return fmt.Errorf("玩家中继已建立")
	}

	// p2p 打洞仍在进行时先停掉（用户显式选择了中继兜底）：
	// 保留其 socket 供中继打洞复用；若 rudp 已建立则 socket 归 rudp 所有，不复用
	e.mu.Lock()
	if e.gPuncher != nil && !e.gP2PDone {
		e.gPuncher.stop()
		e.gPuncher = nil
		if e.gRudp != nil {
			e.gPunchSock = nil
		}
	}
	e.rAttempt++
	attempt := e.rAttempt
	e.rInFlight = true
	e.rResult = make(chan string, 4)
	e.rDone = false
	e.mu.Unlock()

	e.notifyState(phasePrelay, statusTrying, "", "")
	if err := e.sendSignal("relay_request", map[string]any{"clientId": clientID}, "host"); err != nil {
		e.guestRelayFail(attempt, "发送中继请求失败: "+err.Error())
		return nil
	}
	e.app.netLog("info", "已发送 relay_request，等待房主派发中继…")

	go func() {
		select {
		case addr := <-e.relayResultChan():
			if addr == "" {
				return // 已被清理
			}
		case <-e.ctxDone():
			return
		case <-time.After(relayRequestTimeout):
			e.guestRelayFail(attempt, "中继请求超时（20s 无响应）")
		}
	}()
	return nil
}

func (e *connEngine) relayResultChan() chan string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rResult == nil {
		e.rResult = make(chan string, 4)
	}
	return e.rResult
}

// relayResultChanSend 通知中继结果（addr 非空=成功地址；空串=忽略）。
func (e *connEngine) relayResultChanSend(addr string) {
	select {
	case e.relayResultChan() <- addr:
	default:
	}
}

// relayAttemptCur 当前中继尝试轮次号（信令侧无本地轮次上下文时取它）。
func (e *connEngine) relayAttemptCur() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rAttempt
}

// guestRelayFail 中继失败收尾（幂等；轮次不符说明本端已开始新一轮，旧轮直接作废）。
func (e *connEngine) guestRelayFail(attempt int64, reason string) {
	e.mu.Lock()
	if attempt != e.rAttempt || e.rDone {
		e.mu.Unlock()
		return
	}
	e.rDone = true
	e.rInFlight = false
	sock, puncher, rudp, bridge := e.rSock, e.rPuncher, e.rRudp, e.rBridge
	e.rSock = nil
	e.rPuncher = nil
	e.rRudp = nil
	e.rBridge = nil
	e.mu.Unlock()

	if puncher != nil {
		puncher.stop()
	}
	if bridge != nil {
		bridge.stop()
	}
	if rudp != nil {
		_ = rudp.Close()
	}
	if sock != nil {
		_ = sock.Close()
	}
	e.app.netLog("warn", "玩家中继失败: "+reason)
	e.notifyState(phasePrelay, statusFailed, "", reason)
}

// guestOnRelayNotify 收到房主的 relay_notify（两种形态分派）。
func (e *connEngine) guestOnRelayNotify(from string, data map[string]any) {
	if from != "host" && from != "" {
		return
	}
	attempt := e.relayAttemptCur()
	relayIP := sigStr(data, "relayIp")
	relayPort := sigInt(data, "relayPort")
	if relayIP != "" && relayPort > 0 {
		e.app.netLog("info", "收到 relay_notify，开始打洞")
		go e.relayPunchFlow(attempt, relayIP, relayPort)
		return
	}
	if sigBool(data, "connected") {
		// 模组间已互桥（中继链路就绪）：等我们到中继者的 rudp 起来同样处理
		e.mu.Lock()
		e.rConnectedHint = true
		already := e.rRudp != nil
		e.mu.Unlock()
		e.app.netLog("info", "收到 relay_notify(connected)：中继链路就绪")
		if already {
			// rudp 已建立但尚未回报（罕见）：直接以现有桥地址回报成功
			if b := e.relayBridgeAddr(); b != "" {
				e.relaySuccess(attempt, b)
			}
		}
	}
}

// relaySuccess 中继链路就绪收尾：回报地址、通知 host（幂等；旧轮次到达直接丢弃）。
// 中继链路 success 的 address 永远是本地桥 127.0.0.1:<port>，绝不暴露公网 IP。
func (e *connEngine) relaySuccess(attempt int64, addr string) {
	e.mu.Lock()
	if attempt != e.rAttempt || e.rDone {
		e.mu.Unlock()
		return
	}
	e.rDone = true
	e.rInFlight = false
	clientID := e.clientID
	e.mu.Unlock()

	e.app.netLog("info", "中继链路就绪")
	e.notifyState(phasePrelay, statusSuccess, addr, "")
	if err := e.sendSignal("relay_ready", map[string]any{"clientId": clientID}, "host"); err != nil {
		e.app.netLog("warn", "发送 relay_ready 失败: "+err.Error())
	}
	e.relayResultChanSend(addr)
}

func (e *connEngine) relayBridgeAddr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rBridge != nil && e.rBridge.ln != nil {
		return e.rBridge.ln.Addr().String()
	}
	return ""
}

// relayPunchFlow 对中继者打洞（15s）→ rudp → 本地桥 → success → relay_ready。
// attempt 是本轮中继尝试的轮次号：用户在失败后重试会开出新一轮，旧轮晚到的隧道必须丢弃，
// 否则同一 socket 上会跑两条 rudp 互抢帧。
func (e *connEngine) relayPunchFlow(attempt int64, relayIP string, relayPort int) {
	// 复用 p2p 打洞 socket（仅当它不归任何活跃 puncher/rudp 所有时），否则新建
	e.mu.Lock()
	if e.rDone || attempt != e.rAttempt || e.sess == nil {
		e.mu.Unlock()
		return
	}
	sock := e.gPunchSock
	reused := sock != nil && e.gRudp == nil && e.gPuncher == nil
	e.mu.Unlock()
	if !reused {
		s, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			e.guestRelayFail(attempt, "创建中继 socket 失败: "+err.Error())
			return
		}
		sock = s
	}

	target := &net.UDPAddr{IP: net.ParseIP(relayIP), Port: relayPort}
	win, actual, perr := punchRelayOnce(sock, target, relayPunchTimeout)
	if perr != nil {
		if !reused {
			_ = sock.Close()
		}
		e.guestRelayFail(attempt, "中继打洞失败（"+perr.Error()+"）")
		return
	}
	if win != sock {
		e.closeGuestPunchSock(sock)
		sock = win
	}

	rc := newRudp(sock, actual)
	rc.Start()
	local, bridge, berr := startGuestBridge(rc, func() { e.relayBridgeDown() })
	if berr != nil {
		_ = rc.Close()
		e.guestRelayFail(attempt, "中继本地桥建立失败: "+berr.Error())
		return
	}
	e.mu.Lock()
	if e.rDone || attempt != e.rAttempt || e.sess == nil {
		// 写回屏障：打洞/建桥期间 20s 预算已判失败、本端已重开一轮或已退房，晚到的隧道直接丢弃，
		// 否则既漏 socket 又会在"已失败"之后翻一个成功出来
		e.mu.Unlock()
		_ = rc.Close()
		bridge.stop()
		if !reused {
			_ = sock.Close()
		}
		e.app.netLog("warn", "中继隧道建立过晚（本端已判失败或已重开一轮），丢弃")
		return
	}
	e.rSock = sock
	e.rPuncher = nil
	e.rRudp = rc
	e.rBridge = bridge
	e.mu.Unlock()

	e.app.netLog("info", "中继隧道建立")
	e.relaySuccess(attempt, local)
}

// relayBridgeDown 中继桥拆除断开。
func (e *connEngine) relayBridgeDown() {
	e.mu.Lock()
	rc, bridge := e.rRudp, e.rBridge
	e.rRudp = nil
	e.rBridge = nil
	e.mu.Unlock()
	if bridge != nil {
		bridge.stop()
	}
	if rc != nil {
		_ = rc.Close()
	}
	e.notifyState(phasePrelay, statusFailed, "", "中继隧道已断开")
}

// ---- host：relay_request 派发 ----

// relayCandidate /relay/candidates 返回的候选。
type relayCandidate struct {
	ClientID   string `json:"clientId"`
	RoomCode   string `json:"roomCode"`
	NatType    string `json:"natType"`
	MappedIP   string `json:"mappedIp"`
	MappedPort int    `json:"mappedPort"`
}

// hostOnRelayRequest 房主收到 relay_request：挑选候选中继者并派发。
func (e *connEngine) hostOnRelayRequest(from string, data map[string]any) {
	e.app.netLog("info", "收到 relay_request")

	e.mu.Lock()
	requester := e.hPeers[from]
	selfID := e.clientID
	roomCode := e.code
	e.mu.Unlock()

	if requester == nil || requester.mapped == nil {
		e.app.netLog("warn", "relay_request 但缺少 requester 映射地址，拒绝")
		_ = e.sendSignal("relay_declined", map[string]any{}, from)
		return
	}

	cands, err := e.fetchRelayCandidates(roomCode)
	if err != nil {
		e.app.netLog("warn", "查询中继候选失败: "+err.Error())
	}
	cand := e.pickRelayCandidate(cands, from, selfID)
	if cand == nil {
		e.app.netLog("warn", "无可用中继候选，拒绝 relay_request")
		_ = e.sendSignal("relay_declined", map[string]any{}, from)
		return
	}

	// replyHostId：候选若也是房主（host-<CODE>）没有"自家房主"可回执，只能按这个 id 直投回来
	setup := map[string]any{
		"targetClientId": from,
		"targetIp":       requester.mapped.IP,
		"targetPort":     requester.mapped.Port,
		"replyHostId":    "host-" + strings.ToUpper(roomCode),
	}
	if err := e.sendSignal("relay_setup", setup, cand.ClientID); err != nil {
		e.app.netLog("warn", "发送 relay_setup 失败: "+err.Error())
	}
	// 给 requester 回 relay_notify（候选的映射地址）
	notify := map[string]any{
		"relayIp":   cand.MappedIP,
		"relayPort": cand.MappedPort,
	}
	if err := e.sendSignal("relay_notify", notify, from); err != nil {
		e.app.netLog("warn", "发送 relay_notify 失败: "+err.Error())
	}
	e.app.netLog("info", "中继派发完成")
}

// fetchRelayCandidates GET /relay/candidates?excludeRoom=<本房>：候选池=除本房外所有房间
// 里 relayEnabled 且非对称 NAT 的人；不能带 roomCode（带了就只返回本房成员）。
func (e *connEngine) fetchRelayCandidates(selfRoom string) ([]relayCandidate, error) {
	var data struct {
		Candidates []relayCandidate `json:"candidates"`
	}
	q := url.Values{}
	if selfRoom != "" {
		q.Set("excludeRoom", strings.ToUpper(selfRoom))
	}
	if err := e.app.api.get(e.app.baseURL(), "/relay/candidates", q, &data); err != nil {
		return nil, err
	}
	return data.Candidates, nil
}

// pickRelayCandidate 过滤候选并按本会话已派发次数升序取最闲的一个（服务端已洗牌，
// 同载荷时保持服务端顺序即可，不要退化成"永远选第一个"）。
func (e *connEngine) pickRelayCandidate(cands []relayCandidate, requesterID, selfID string) *relayCandidate {
	e.mu.Lock()
	dispatched := e.hRelayDispatched
	ownHostID := "host-" + strings.ToUpper(e.code)
	peers := make(map[string]bool, len(e.hPeers))
	for id := range e.hPeers {
		peers[id] = true
	}
	e.mu.Unlock()

	ntOK := make([]relayCandidate, 0, len(cands))
	for _, c := range cands {
		// 服务端不回传 roomCode，同房成员只能在本地按在册房客表剔掉
		if c.ClientID == "" || c.ClientID == requesterID || c.ClientID == selfID || c.ClientID == ownHostID || peers[c.ClientID] {
			continue
		}
		if c.MappedIP == "" || c.MappedPort <= 0 {
			continue
		}
		nt := strings.ToLower(c.NatType)
		if nt == "" || strings.Contains(nt, "sym") || strings.Contains(nt, "strict") || strings.Contains(nt, "unknown") {
			continue
		}
		ntOK = append(ntOK, c)
	}
	if len(ntOK) == 0 {
		return nil
	}
	sort.SliceStable(ntOK, func(i, j int) bool {
		return dispatched[ntOK[i].ClientID] < dispatched[ntOK[j].ClientID]
	})
	best := ntOK[0]
	e.mu.Lock()
	if e.hRelayDispatched == nil {
		e.hRelayDispatched = map[string]int{}
	}
	e.hRelayDispatched[best.ClientID]++
	e.mu.Unlock()
	return &best
}

// hostOnRelayAccept 候选者回执：把"桥已就绪"转告自家房客（relay_notify{connected}）。
// 房主候选（host-<CODE>）只会回给请求方房主，这一步不做房客就只能干等超时。
func (e *connEngine) hostOnRelayAccept(data map[string]any) {
	forClientID := sigStr(data, "forClientId")
	if forClientID == "" {
		return
	}
	e.app.netLog("info", "中继节点已接受")
	if err := e.sendSignal("relay_notify", map[string]any{"connected": true}, forClientID); err != nil {
		e.app.netLog("warn", "转发 relay_notify 失败: "+err.Error())
	}
}

// ---- 我们作为中继节点（guest 收 relay_setup）----

// guestOnRelaySetup 收到 relay_setup：与 target 打洞并做 host↔target 流搬运桥。
func (e *connEngine) guestOnRelaySetup(from string, data map[string]any) {
	targetClientID := sigStr(data, "targetClientId")
	targetIP := sigStr(data, "targetIp")
	targetPort := sigInt(data, "targetPort")

	e.mu.Lock()
	hostRudp := e.gRudp
	e.mu.Unlock()

	if hostRudp == nil || targetIP == "" || targetPort <= 0 {
		// 自己没有到 host 的隧道 → declined
		e.app.netLog("warn", "收到 relay_setup 但无到 host 的隧道，declined")
		_ = e.sendSignal("relay_declined", map[string]any{}, "host")
		return
	}
	e.app.netLog("info", "受托为中继节点")

	go func() {
		sock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			_ = e.sendSignal("relay_declined", map[string]any{}, "host")
			return
		}
		target := &net.UDPAddr{IP: net.ParseIP(targetIP), Port: targetPort}
		win, actual, perr := punchRelayOnce(sock, target, relaySetupTimeout)
		if perr != nil {
			_ = sock.Close()
			e.app.netLog("warn", "中继节点对 target 打洞失败: "+perr.Error())
			_ = e.sendSignal("relay_declined", map[string]any{}, "host")
			return
		}
		if win != sock {
			_ = sock.Close()
			sock = win
		}
		targetRc := newRudp(sock, actual)
		targetRc.Start()
		stop := pumpRelay(hostRudp, targetRc)
		e.mu.Lock()
		e.rAsNodeStop = stop
		e.mu.Unlock()
		if err := e.sendSignal("relay_accept", map[string]any{"forClientId": targetClientID}, "host"); err != nil {
			e.app.netLog("warn", "发送 relay_accept 失败: "+err.Error())
		}
		e.app.netLog("info", "中继桥已建立")
	}()
}

// relayRelease 中继资源释放钩子。
// 契约口径："POST /relay/release（若有 TURN/relay 资源）"——该端点要求 /relay/allocate
// 返回的 32 位 TURN sessionId，我们全程未分配 TURN 资源（玩家中继走 P2P rudp 流搬运），
// 因此退出时无需调用；保留钩子以便未来接入 TURN 时补充。
func (e *connEngine) relayRelease() {
	_ = e
}
