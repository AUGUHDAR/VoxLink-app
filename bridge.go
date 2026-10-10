package main

// bridge.go — 本地端口映射桥 + 玩家中继 v2 转发器：
//   - guest：rudp 就绪后 net.Listen("tcp","127.0.0.1:0")，MC 一连上本地桥就立刻把
//     握手字节送进隧道（host 桥 lazy 依赖这个）；单客户端语义：新 MC 连入关闭旧的。
//   - host：lazy 桥——等隧道第一个 DATA（MC 握手字节）到达才 Dial 127.0.0.1:<hostPort>
//     （失败每 1s 重试，10s 窗口）；30s 零流量 watchdog 拆桥，之后下一段 DATA 再重拨。
//     多个 guest = 多条独立 rudp 会话各自桥一条 TCP。
//   - 中继转发（模组 RelayBridge.java 2026-10-05 v2 重写）：只做"流↔流"搬运，
//     不关心流由哪条通道承载；任一方向 EOF/出错即整个会话收尾、双腿互关。
//     v1 复用了候选者连 host 的传输裸流，两条 MC 会话字节交错进同一条有序流 +
//     InputStream 双读者互抢（1.0.x 起从未真正可用），已被本地复现实验实证并废除，
//     所以这里没有"对端没 mux 就退回裸流"的降级路径。

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	bridgeBufSize     = 32 * 1024
	bridgeIdleTimeout = 30 * time.Second
	bridgeDialWindow  = 10 * time.Second
	bridgeDialRetry   = 1 * time.Second
	bridgeDialTimeout = 2 * time.Second

	// relayBufferSize 模组 RelayBridge.RELAY_BUFFER_SIZE
	relayBufferSize = 65536
)

// bridgeStream 桥一端的字节流：读可被 abort 打断、写按隧道分片。
// rudp 整传输与 mux 子通道都满足它，因此同一段桥逻辑既能承载自有会话也能承载被中继会话。
type bridgeStream struct {
	in     func(p []byte, abort <-chan struct{}) (int, error)
	out    func(p []byte) (int, error)
	closer func()          // 会话级收尾：整传输腿关整条隧道，子通道腿只 closeSubstream
	done   <-chan struct{} // 承载它的传输终止信号
}

// rudpBridgeStream 整传输腿（模组 RelayBridge.legOf）：被关闭的就是这条会话本身。
func rudpBridgeStream(rc *rudpConn) *bridgeStream {
	return &bridgeStream{
		in:     rc.readAbort,
		out:    rc.Write,
		closer: func() { _ = rc.Close() },
		done:   rc.doneCh,
	}
}

// rudpMuxRelayStream mux 子通道腿（模组 CM:13360-13378 的匿名 RelayLeg）：
// 收尾只关子通道，绝不关父传输——候选者自己的会话（chan 0）不能被被中继的人连坐。
func rudpMuxRelayStream(rc *rudpConn, ch int) *bridgeStream {
	sc := rc.substream(ch)
	return &bridgeStream{
		in:     sc.read,
		out:    sc.Write,
		closer: func() { rc.closeSubstream(ch) },
		done:   rc.doneCh,
	}
}

// tcpBridge 一条 MC TCP 连接 ↔ 隧道流（rudp 会话或 mux 子通道）的搬运桥。
type tcpBridge struct {
	stream   *bridgeStream
	ln       net.Listener // guest 模式才有
	tcp      atomic.Pointer[net.Conn]
	lastAct  atomic.Int64
	stopOnce sync.Once
	stopCh   chan struct{}
	downCb   func() // MC 侧连接断开（pump 退出）时回调
}

func newTcpBridge(stream *bridgeStream, downCb func()) *tcpBridge {
	return &tcpBridge{stream: stream, stopCh: make(chan struct{}), downCb: downCb}
}

func (b *tcpBridge) touch() { b.lastAct.Store(time.Now().UnixMilli()) }

// idleMs 距最近一次流量的毫秒数。
func (b *tcpBridge) idleMs() int64 { return time.Now().UnixMilli() - b.lastAct.Load() }

// stop 关闭监听与当前 TCP 连接（不动承载它的隧道本身）。幂等。
func (b *tcpBridge) stop() {
	b.stopOnce.Do(func() {
		close(b.stopCh)
		if ln := b.ln; ln != nil {
			_ = ln.Close()
		}
		if c := b.tcp.Load(); c != nil {
			_ = (*c).Close()
		}
	})
}

func (b *tcpBridge) setConn(conn net.Conn, old *net.Conn) {
	if old != nil {
		_ = (*old).Close()
	}
	b.tcp.Store(&conn)
	b.touch()
}

// startGuestBridge guest 侧：监听 127.0.0.1 随机端口，返回本地代理地址。
// downCb：MC 侧断开（或空闲拆桥）时回调（引擎据此复位 p2p 状态并关 rudp）。
func startGuestBridge(rc *rudpConn, downCb func()) (string, *tcpBridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("本地端口监听失败: %w", err)
	}
	b := newTcpBridge(rudpBridgeStream(rc), downCb)
	b.ln = ln
	b.touch()

	rc.setOnClosed(func(string) { b.stop() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // 监听关闭（桥停止）
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true) // MC 握手字节必须立刻进隧道
			}
			b.setConn(conn, b.tcp.Load())
			go b.pump(conn)
		}
	}()
	return ln.Addr().String(), b, nil
}

// startHostLazyBridge host 侧：等隧道第一个 DATA（MC 握手字节）到达才拨号本地 MC 端口，
// 失败每 1s 重试共 10s 窗口；30s 零流量 watchdog 拆桥，之后可由下一段 DATA 触发重拨。
func startHostLazyBridge(rc *rudpConn, hostPort int, logf func(level, msg string)) {
	startHostLazyBridgeOnStream(rudpBridgeStream(rc), hostPort, logf, nil)
}

// startHostMuxRelayBridge host 侧被中继房客桥（模组 P2PBridge.startUdpHostBridgeForRelayedClient）：
// 候选者在其 joiner 传输上分配的 mux 子通道 ↔ 本地 MC 端口。桥死只收尾子通道，
// 承载它的候选者传输（连同候选者自己的会话）必须活着。
func startHostMuxRelayBridge(rc *rudpConn, muxChan, hostPort int, logf func(level, msg string), onClose func()) {
	startHostLazyBridgeOnStream(rudpMuxRelayStream(rc, muxChan), hostPort, logf, onClose)
}

// startHostLazyBridgeOnStream lazy 桥主体，流可以是整传输也可以是子通道。
// onClose：会话级收尾（子通道桥用它通知上层这条被中继会话结束了；整传输桥传 nil）。
func startHostLazyBridgeOnStream(stream *bridgeStream, hostPort int, logf func(level, msg string), onClose func()) {
	go func() {
		buf := make([]byte, bridgeBufSize)
		for {
			n, err := stream.in(buf, nil) // 阻塞等 MC 握手字节（KEEPALIVE 维持隧道活性）
			if err != nil || n == 0 {
				if onClose != nil {
					onClose()
				}
				return // 隧道关闭
			}
			conn := bDialLocalMC(hostPort, stream, logf)
			if conn == nil {
				if logf != nil {
					logf("error", "host 桥：无法连接本地 MC 端口，关闭该条隧道")
				}
				stream.closer()
				if onClose != nil {
					onClose()
				}
				return
			}
			b := newTcpBridge(stream, nil)
			b.setConn(conn, nil)
			// 首包（MC 握手字节）先写入，再进入双向搬运
			if _, err := conn.Write(buf[:n]); err != nil {
				_ = conn.Close()
				stream.closer()
				if onClose != nil {
					onClose()
				}
				return
			}
			go b.pump(conn)
			// watchdog 判定 30s 零流量：拆桥回到等待下一段 DATA 的状态
			idle := b.watchdogOnce()
			b.stop()
			if !idle {
				if onClose != nil {
					onClose()
				}
				return // 隧道已关闭
			}
			if logf != nil {
				logf("info", "host 桥：长时间无流量，拆桥待重拨")
			}
		}
	}()
}

// bDialLocalMC 拨号本地 MC 端口，失败每 1s 重试，总窗口 10s。
func bDialLocalMC(hostPort int, stream *bridgeStream, logf func(level, msg string)) net.Conn {
	deadline := time.Now().Add(bridgeDialWindow)
	for {
		d := bridgeDialTimeout
		if remain := time.Until(deadline); remain < d {
			d = remain
		}
		if d <= 0 {
			return nil
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), d)
		if err == nil {
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}
			return conn
		}
		if logf != nil {
			logf("warn", "host 桥：连接本地 MC 端口失败，稍后重试")
		}
		if time.Now().Add(bridgeDialRetry).After(deadline) {
			return nil
		}
		select {
		case <-stream.done:
			return nil
		case <-time.After(bridgeDialRetry):
		}
	}
}

// pump 双向搬运：TCP → 隧道、隧道 → TCP。任一方向出错即双向退出，
// 并回调 downCb（guest 引擎据此复位；host 侧借隧道关闭传导）。
func (b *tcpBridge) pump(conn net.Conn) {
	done := make(chan struct{})
	var once sync.Once
	stopBoth := func() {
		once.Do(func() {
			close(done)
			_ = conn.Close()
			b.stop()
		})
	}

	go func() { // TCP → 隧道
		defer stopBoth()
		buf := make([]byte, bridgeBufSize)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				b.touch()
				if _, werr := b.stream.out(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
			select {
			case <-b.stopCh:
				return
			default:
			}
		}
	}()
	go func() { // 隧道 → TCP
		defer stopBoth()
		buf := make([]byte, bridgeBufSize)
		for {
			n, err := b.stream.in(buf, b.stopCh)
			if n > 0 {
				b.touch()
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	<-done
	if cb := b.downCb; cb != nil {
		cb()
	}
}

// watchdogOnce 每 5s 检查一次，30s 零流量返回 true（调用方拆桥）。
// 隧道关闭（stopCh 关闭）返回 false。
func (b *tcpBridge) watchdogOnce() bool {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-b.stopCh:
			return false
		case <-t.C:
			if b.idleMs() > bridgeIdleTimeout.Milliseconds() {
				return true
			}
		}
	}
}

// ---- 玩家中继 v2 转发器（模组 network/RelayBridge.java）----

// relayLeg 一条被转发的腿：给出来往流与关闭动作。
// 子通道腿的 close 只 closeSubstream，不动父传输；整传输腿（对被中继房客的专打传输）关整条。
type relayLeg struct {
	stream *bridgeStream
}

// relayBridge 中继转发会话表（模组 RelayBridge 单例）。
// v2 只做流↔流转发：被中继会话走候选者↔host 传输的 mux 子通道（chan 1..7），
// 与候选者自有会话（chan 0）字节级隔离。
type relayBridge struct {
	logf   func(level, msg string)
	mu     sync.Mutex
	active map[string]*relaySession
}

var (
	relayBridgeMu    sync.Mutex
	relayBridgeValue *relayBridge
)

// getRelayBridge 进程内单例（模组 RelayBridge.getInstance(scheduler)：首次调用者定 logger）。
func getRelayBridge(logf func(level, msg string)) *relayBridge {
	relayBridgeMu.Lock()
	defer relayBridgeMu.Unlock()
	if relayBridgeValue == nil {
		relayBridgeValue = &relayBridge{logf: logf, active: map[string]*relaySession{}}
	}
	return relayBridgeValue
}

// relayBridgeIfStarted 实例尚未建立时返回 nil（模组 stop() 里 getInstance(null) != null 那道守卫：
// 本端从没当中继节点时不该去动任何会话表）。
func relayBridgeIfStarted() *relayBridge {
	relayBridgeMu.Lock()
	defer relayBridgeMu.Unlock()
	return relayBridgeValue
}

func (b *relayBridge) log(level, msg string) {
	if b != nil && b.logf != nil {
		b.logf(level, msg)
	}
}

// startRelay 建立一条中继转发会话：A 腿与 B 腿双向泵流。
// 任一方向读到 EOF/出错即停止整个会话并关闭双腿；重复 key 静默忽略（幂等屏障：
// 同一 key 起两次会有两套泵抢同一条流，字节直接被切碎）。
func (b *relayBridge) startRelay(relayKey, peerAID, peerBID string, legA, legB *relayLeg) {
	if relayKey == "" || legA == nil || legB == nil || legA == legB {
		b.log("warn", "[Relay] Reject invalid relay session: "+relayKey)
		return
	}
	session := &relaySession{owner: b, key: relayKey, peerAID: peerAID, peerBID: peerBID, legA: legA, legB: legB}
	b.mu.Lock()
	if _, ok := b.active[relayKey]; ok {
		b.mu.Unlock()
		b.log("info", "[Relay] Relay session already exists: "+relayKey)
		return
	}
	b.active[relayKey] = session
	b.mu.Unlock()
	session.startForwarding()
	b.log("info", fmt.Sprintf("[Relay] Relay started: %s (A=%s, B=%s)", relayKey, peerAID, peerBID))
}

// relayCountForPeer 该对端当前在转发几条会话（模组派发候选时按它排序取最闲的）。
func (b *relayBridge) relayCountForPeer(peerID string) int {
	count := 0
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.active {
		if s.peerAID == peerID || s.peerBID == peerID {
			count++
		}
	}
	return count
}

// stopAllRelays 拆掉全部中继会话（本端退房/停止中继时）。
func (b *relayBridge) stopAllRelays() {
	b.mu.Lock()
	sessions := make([]*relaySession, 0, len(b.active))
	for _, s := range b.active {
		sessions = append(sessions, s)
	}
	b.mu.Unlock()
	if len(sessions) == 0 {
		return
	}
	b.log("info", fmt.Sprintf("[Relay] Stopping %d relay sessions", len(sessions)))
	for _, s := range sessions {
		s.stop()
	}
	b.mu.Lock()
	clear(b.active)
	b.mu.Unlock()
}

// relaySession A↔B 一条转发会话：两条泵，任一退出即整体收尾。
type relaySession struct {
	owner      *relayBridge
	key        string
	peerAID    string
	peerBID    string
	legA       *relayLeg
	legB       *relayLeg
	forwarding atomic.Bool
	done       chan struct{} // 泵线程的打断闸（Java thread.interrupt 的 Go 形态）
	interrupt  sync.Once
}

// relayLegOf 包一层，便于调用处表达"这条腿是整传输/子通道"。
func relayLegOf(stream *bridgeStream) *relayLeg { return &relayLeg{stream: stream} }

func (s *relaySession) startForwarding() {
	s.forwarding.Store(true)
	s.done = make(chan struct{})
	go s.pump("A->B", s.legA.stream, s.legB.stream)
	go s.pump("B->A", s.legB.stream, s.legA.stream)
}

// pump 单向搬运：读到的字节原样写进对腿。EOF/出错/会话已停即退出并收尾整个会话。
func (s *relaySession) pump(dir string, from, to *bridgeStream) {
	buf := make([]byte, relayBufferSize)
	for s.forwarding.Load() {
		n, err := from.in(buf, s.done)
		if err == nil && n > 0 {
			_, err = to.out(buf[:n])
		}
		if err != nil {
			if s.forwarding.Load() {
				s.owner.log("warn", "[Relay] "+dir+" end: "+err.Error())
			}
			break
		}
	}
	s.stop()
}

// stop 会话收尾：CAS 幂等（两条腿都会调，只生效一次），关双腿、摘表、打断另一条泵。
func (s *relaySession) stop() {
	if s.forwarding.CompareAndSwap(true, false) {
		s.owner.log("info", "[Relay] Relay session stopped: "+s.key)
		s.legA.stream.closer()
		s.legB.stream.closer()
		// 模组此处用 getInstance(null) != null 做"实例已存在"守卫，摘的是 owner 的表：
		// 本端从没当中继节点（单例没建过）时不该动任何会话表
		if relayBridgeIfStarted() != nil {
			s.owner.removeSession(s.key)
		}
	}
	// CAS 之外无条件打断：晚到的第二条泵也必须从阻塞读里出来
	s.interrupt.Do(func() {
		if s.done != nil {
			close(s.done)
		}
	})
}

// removeSession 从会话表摘除（key 相同才摘，避免误摘同 key 的新会话）。
func (b *relayBridge) removeSession(key string) {
	b.mu.Lock()
	delete(b.active, key)
	b.mu.Unlock()
}
