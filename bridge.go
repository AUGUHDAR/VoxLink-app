package main

// bridge.go — 本地端口映射桥：把本地 TCP 连接字节流与 rudp DATA 双向搬运。
//   - guest：rudp 就绪后 net.Listen("tcp","127.0.0.1:0")，MC 一连上本地桥就立刻把
//     握手字节送进隧道（host 桥 lazy 依赖这个）；单客户端语义：新 MC 连入关闭旧的。
//   - host：lazy 桥——等隧道第一个 DATA（MC 握手字节）到达才 Dial 127.0.0.1:<hostPort>
//     （失败每 1s 重试，10s 窗口）；30s 零流量 watchdog 拆桥，之后下一段 DATA 再重拨。
//     多个 guest = 多条独立 rudp 会话各自桥一条 TCP。

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
)

// tcpBridge 一条 MC TCP 连接 ↔ rudp 会话的搬运桥。
type tcpBridge struct {
	rc       *rudpConn
	ln       net.Listener // guest 模式才有
	tcp      atomic.Pointer[net.Conn]
	lastAct  atomic.Int64
	stopOnce sync.Once
	stopCh   chan struct{}
	downCb   func() // MC 侧连接断开（pump 退出）时回调
}

func newTcpBridge(rc *rudpConn, downCb func()) *tcpBridge {
	return &tcpBridge{rc: rc, stopCh: make(chan struct{}), downCb: downCb}
}

func (b *tcpBridge) touch() { b.lastAct.Store(time.Now().UnixMilli()) }

// idleMs 距最近一次流量的毫秒数。
func (b *tcpBridge) idleMs() int64 { return time.Now().UnixMilli() - b.lastAct.Load() }

// stop 关闭监听与当前 TCP 连接（不动 rudp 会话本身）。幂等。
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
	b := newTcpBridge(rc, downCb)
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
	go func() {
		buf := make([]byte, bridgeBufSize)
		for {
			n, err := rc.Read(buf) // 阻塞等 MC 握手字节（KEEPALIVE 维持隧道活性）
			if err != nil || n == 0 {
				return // 隧道关闭
			}
			conn := bDialLocalMC(hostPort, rc, logf)
			if conn == nil {
				if logf != nil {
					logf("error", "host 桥：无法连接本地 MC 端口，关闭该条隧道")
				}
				_ = rc.Close()
				return
			}
			b := newTcpBridge(rc, nil)
			b.setConn(conn, nil)
			rc.setOnClosed(func(string) { b.stop() })
			// 首包（MC 握手字节）先写入，再进入双向搬运
			if _, err := conn.Write(buf[:n]); err != nil {
				_ = conn.Close()
				_ = rc.Close()
				return
			}
			go b.pump(conn)
			// watchdog 判定 30s 零流量：拆桥回到等待下一段 DATA 的状态
			idle := b.watchdogOnce()
			b.stop()
			if !idle {
				return // 隧道已关闭
			}
			if logf != nil {
				logf("info", "host 桥：长时间无流量，拆桥待重拨")
			}
		}
	}()
}

// bDialLocalMC 拨号本地 MC 端口，失败每 1s 重试，总窗口 10s。
func bDialLocalMC(hostPort int, rc *rudpConn, logf func(level, msg string)) net.Conn {
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
		case <-rc.doneCh:
			return nil
		case <-time.After(bridgeDialRetry):
		}
	}
}

// pump 双向搬运：TCP → rudp、rudp → TCP。任一方向出错即双向退出，
// 并回调 downCb（guest 引擎据此复位；host 侧借 rc 关闭传导）。
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

	go func() { // TCP → rudp
		defer stopBoth()
		buf := make([]byte, bridgeBufSize)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				b.touch()
				if _, werr := b.rc.Write(buf[:n]); werr != nil {
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
	go func() { // rudp → TCP
		defer stopBoth()
		buf := make([]byte, bridgeBufSize)
		for {
			n, err := b.rc.Read(buf)
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

// pumpRelay 中继者流搬运桥：host rudp ↔ target rudp 两条会话互拷字节
// （模组 RelayBridge 就是 rudp 流搬运，不是 NAT 转发）。返回停止函数。
func pumpRelay(hostRc, targetRc *rudpConn) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	stopFn := func() {
		once.Do(func() {
			close(done)
			_ = hostRc.Close()
			_ = targetRc.Close()
		})
	}
	go func() { // host → target
		buf := make([]byte, bridgeBufSize)
		for {
			n, err := hostRc.Read(buf)
			if n > 0 {
				if _, werr := targetRc.Write(buf[:n]); werr != nil {
					stopFn()
					return
				}
			}
			if err != nil {
				stopFn()
				return
			}
		}
	}()
	go func() { // target → host
		buf := make([]byte, bridgeBufSize)
		for {
			n, err := targetRc.Read(buf)
			if n > 0 {
				if _, werr := hostRc.Write(buf[:n]); werr != nil {
					stopFn()
					return
				}
			}
			if err != nil {
				stopFn()
				return
			}
		}
	}()
	go func() { // 任一端断开即停
		select {
		case <-hostRc.doneCh:
		case <-targetRc.doneCh:
		case <-done:
			return
		}
		stopFn()
	}()
	return stopFn
}
