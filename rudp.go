package main

// rudp.go — 可靠 UDP 隧道（对齐模组 ReliableUdpTransport.java，帧格式逐字节互通）：
//
//	帧头 11B：56 4C + type(u8) + seq(u32 BE) + ack(u32 BE)
//	DATA/FEC_XOR 附加：payloadLen(u16 BE)；DATA 头 13B；FEC 头 13B + [13]=count
//	type: 1 PUNCH(收到回 56 4C 02 00 00) 2 PUNCH_ACK 3 DATA 4 ACK(纯11B)
//	      7 DISCONNECT 8 KEEPALIVE(11B，收到必回) 9 FEC_XOR 10 RESTART(忽略) 11 VOICE(忽略)
//
// 语义：累计 ACK（ack=nextExpectedSeq）、RTO RFC6298 夹 [100,800]ms、
// 重传退避 rto+min(retries,3)*250、乱序窗口 128、<512B 小包首发 50ms 后复制重发一次、
// KEEPALIVE 1s、静默>8s 降级、pending 且静默>24s 判死、>60s 硬超时。
// FEC：发送按组 4 包 + XOR（ReliableUdpTransport.java:820-842 字节布局），
// 接收端收齐 3+XOR 恢复缺失包。连接建立第一件事：互发 KEEPALIVE（无 SYN 握手）。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rudpTypePunch      byte = 1
	rudpTypePunchAck   byte = 2
	rudpTypeData       byte = 3
	rudpTypeAck        byte = 4
	rudpTypeDisconnect byte = 7
	rudpTypeKeepalive  byte = 8
	rudpTypeFecXor     byte = 9
	rudpTypeRestart    byte = 10
	rudpTypeVoice      byte = 11

	rudpHeaderSize  = 11
	rudpMaxPayload  = 1400
	rudpRecvBufSize = 1500
	// TURN 包封下 FEC 帧(22头+块+4MAC)必须 ≤ 节点 1400 上限，超了整包静默丢（模组 TURN_MAX_SEND_CHUNK）
	rudpTurnMaxChunk = 1374

	rudpFecGroupSize  = 4
	rudpRecvWindow    = 128 // 乱序窗口
	rudpBufChunksMax  = 512 // 交付队列上限（对齐模组 bufferedChunks）
	rudpSmallThresh   = 512
	rudpSmallDupDelay = 50 * time.Millisecond

	rudpTick            = 50 * time.Millisecond
	rudpKeepaliveEvery  = 1 * time.Second
	rudpRtoMinMs        = 100
	rudpRtoMaxMs        = 800
	rudpInitRtoMs       = 200
	rudpBackoffMs       = 250
	rudpSilenceDegrade  = 8 * time.Second
	rudpSilenceDead     = 24 * time.Second
	rudpSilenceHard     = 60 * time.Second
	rudpStuckWriteLimit = 30 * time.Second
)

var (
	errRudpClosed = errors.New("rudp: 连接已关闭")
	errRudpStuck  = errors.New("rudp: 传输停滞（对端无响应）")

	// rudpPunchAckPb 收到 PUNCH 的回包：56 4C 02 00 00（模组 sendPunchAck 同款）
	rudpPunchAckPb = []byte{0x56, 0x4C, rudpTypePunchAck, 0, 0}
)

// ---- seq 运算（对齐模组 seqAfter/seqDiff，模 2^32）----

func seqAfter(a, b uint32) bool {
	return a > b && a-b < 1073741823 || a < b && b-a > 1073741823
}

func seqDiff(newer, older uint32) uint32 {
	return newer - older
}

// ---- 帧编解码 ----

type rudpFrame struct {
	Type       byte
	Seq        uint32
	Ack        uint32
	Payload    []byte // DATA 载荷 / FEC 的 XOR 载荷
	FecCount   int    // FEC_XOR：原始包个数
	FecLengths []int  // FEC_XOR：各原始包长度
}

// rudpEncode 编码一帧。PUNCH/PUNCH_ACK 为 5 字节明文控制包（56 4C type 00 00），
// 与模组 ReliableUdpTransport.sendPunchAck 逐字节一致。
func rudpEncode(f rudpFrame) []byte {
	switch f.Type {
	case rudpTypePunch, rudpTypePunchAck:
		return []byte{0x56, 0x4C, f.Type, byte(f.Seq >> 8), byte(f.Seq)}
	case rudpTypeData:
		out := make([]byte, 13+len(f.Payload))
		out[0], out[1] = 0x56, 0x4C
		out[2] = f.Type
		binary.BigEndian.PutUint32(out[3:7], f.Seq)
		binary.BigEndian.PutUint32(out[7:11], f.Ack)
		binary.BigEndian.PutUint16(out[11:13], uint16(len(f.Payload)))
		copy(out[13:], f.Payload)
		return out
	case rudpTypeFecXor:
		count := len(f.FecLengths)
		if f.FecCount > 0 {
			count = f.FecCount
		}
		bodyOff := 14 + count*2
		out := make([]byte, bodyOff+len(f.Payload))
		out[0], out[1] = 0x56, 0x4C
		out[2] = f.Type
		binary.BigEndian.PutUint32(out[3:7], f.Seq) // seq 字段 = groupId
		binary.BigEndian.PutUint32(out[7:11], 0)
		binary.BigEndian.PutUint16(out[11:13], uint16(len(f.Payload)))
		out[13] = byte(count)
		for i := 0; i < count; i++ {
			binary.BigEndian.PutUint16(out[14+i*2:16+i*2], uint16(f.FecLengths[i]))
		}
		copy(out[bodyOff:], f.Payload)
		return out
	default: // ACK/KEEPALIVE/DISCONNECT/RESTART/VOICE
		extra := 0
		if f.Type == rudpTypeVoice {
			extra = len(f.Payload)
		}
		out := make([]byte, rudpHeaderSize+extra)
		out[0], out[1] = 0x56, 0x4C
		out[2] = f.Type
		binary.BigEndian.PutUint32(out[3:7], f.Seq)
		binary.BigEndian.PutUint32(out[7:11], f.Ack)
		copy(out[rudpHeaderSize:], f.Payload)
		return out
	}
}

// rudpDecode 解码一帧；无法解析返回 ok=false。
func rudpDecode(b []byte) (f rudpFrame, ok bool) {
	if len(b) < 3 {
		return f, false
	}
	if b[0] != 0x56 || b[1] != 0x4C {
		return f, false
	}
	switch b[2] {
	case rudpTypePunch, rudpTypePunchAck:
		// 5 字节控制包：nonce 落在 Seq 位置便于回显
		if len(b) < 5 {
			return f, false
		}
		return rudpFrame{Type: b[2], Seq: uint32(binary.BigEndian.Uint16(b[3:5]))}, true
	case rudpTypeData:
		if len(b) < 13 {
			return f, false
		}
		pl := int(binary.BigEndian.Uint16(b[11:13]))
		if 13+pl > len(b) {
			return f, false
		}
		payload := make([]byte, pl)
		copy(payload, b[13:13+pl])
		return rudpFrame{
			Type:    rudpTypeData,
			Seq:     binary.BigEndian.Uint32(b[3:7]),
			Ack:     binary.BigEndian.Uint32(b[7:11]),
			Payload: payload,
		}, true
	case rudpTypeFecXor:
		if len(b) < 14 {
			return f, false
		}
		xorLen := int(binary.BigEndian.Uint16(b[11:13]))
		count := int(b[13])
		xorOff := 14 + count*2
		if count < 0 || xorOff+xorLen > len(b) {
			return f, false
		}
		lengths := make([]int, count)
		for i := 0; i < count; i++ {
			lengths[i] = int(binary.BigEndian.Uint16(b[14+i*2 : 16+i*2]))
		}
		payload := make([]byte, xorLen)
		copy(payload, b[xorOff:xorOff+xorLen])
		return rudpFrame{
			Type:       rudpTypeFecXor,
			Seq:        binary.BigEndian.Uint32(b[3:7]),
			Payload:    payload,
			FecCount:   count,
			FecLengths: lengths,
		}, true
	default:
		if len(b) < rudpHeaderSize {
			return f, false
		}
		return rudpFrame{
			Type:    b[2],
			Seq:     binary.BigEndian.Uint32(b[3:7]),
			Ack:     binary.BigEndian.Uint32(b[7:11]),
			Payload: append([]byte(nil), b[rudpHeaderSize:]...),
		}, true
	}
}

// ---- 连接 ----

type rudpPending struct {
	data     []byte
	sendTime time.Time
	retries  int
}

// rudpPathCodec 传输层"路径"包封编解码器（UdpPath.Codec 同款语义）：
// 直连路径 codec=nil（rudp 帧裸跑）；TURN 路径 codec=TurnPathCodec/StdTurnPathCodec。
type rudpPathCodec interface {
	encode(frame []byte) []byte
	decode(packet []byte) []byte
}

// rudpConn 一条可靠 UDP 会话，实现 io.Reader/io.Writer（字节流，供 TCP 桥搬运）。
type rudpConn struct {
	conn   *net.UDPConn
	remote atomic.Pointer[net.UDPAddr] // 发送目标（可随对端源地址漂移更新）

	// 路径包封（TURN 等中继路径；nil=直连裸跑；Start 前设置）
	codec rudpPathCodec

	// punchAuthV1：认证模式下所有帧附 4 字节截断 HMAC-SHA256
	authMu               sync.Mutex
	authKey              []byte
	allowAuthDowngrade   bool
	consecutiveAuthDrops int

	// 发送侧（sendMu 保护）
	sendMu         sync.Mutex
	nextSeq        uint32
	oldestUnacked  uint32
	pending        map[uint32]*rudpPending
	srtt           float64
	rttvar         float64
	rtoMs          int
	window         int
	failures       int
	degraded       bool
	lastAckSeq     uint32
	dupAckCount    int
	fecSendGroup   [][]byte
	fecSendGroupID int64
	ackNotify      chan struct{} // ACK 到达通知（容量 1）

	// 接收侧（recvMu 保护）
	recvMu       sync.Mutex
	nextExpected atomic.Uint32
	recvBuf      map[uint32][]byte
	buffered     int // 交付队列当前 chunk 数
	fecGroups    map[uint32]map[uint32][]byte
	fecXor       map[uint32][]byte
	fecLens      map[uint32][]int
	fecMinGroup  uint32
	fecHasMin    bool
	chunks       [][]byte // 已按序待读载荷
	dataNotify   chan struct{}

	recvHits  atomic.Int32 // 新端口连续命中计数（漂移重绑）
	driftPort atomic.Int32

	lastRecv  atomic.Int64
	closed    atomic.Bool
	started   atomic.Bool
	closeOnce sync.Once
	doneCh    chan struct{}
	closeMsg  atomic.Pointer[string]
	onClosed  func(reason string)
	onRestart func() // 收到对端 RESTART 帧时触发（ICE 重启）
}

// newRudp 在给定 UDP socket 与对端地址上建立会话（需调用 Start 启动）。
func newRudp(conn *net.UDPConn, remote *net.UDPAddr) *rudpConn {
	c := &rudpConn{
		conn:           conn,
		pending:        map[uint32]*rudpPending{},
		rtoMs:          rudpInitRtoMs,
		window:         64,
		fecSendGroupID: -1,
		recvBuf:        map[uint32][]byte{},
		fecGroups:      map[uint32]map[uint32][]byte{},
		fecXor:         map[uint32][]byte{},
		fecLens:        map[uint32][]int{},
		ackNotify:      make(chan struct{}, 1),
		dataNotify:     make(chan struct{}, 1),
		doneCh:         make(chan struct{}),
	}
	if remote != nil {
		c.remote.Store(remote)
	}
	c.lastRecv.Store(time.Now().UnixMilli())
	return c
}

// setOnClosed 注册关闭回调（对端断开/判死/重传超限时触发一次）。
func (c *rudpConn) setOnClosed(fn func(reason string)) { c.onClosed = fn }

// setOnRestart 注册对端 RESTART 帧回调（ICE 重启）。
func (c *rudpConn) setOnRestart(fn func()) { c.onRestart = fn }

// setCodec 设置路径包封编解码器（须在 Start 前调用；nil=直连裸跑）。
func (c *rudpConn) setCodec(pc rudpPathCodec) { c.codec = pc }

// setAuthKey 启用 punchAuthV1 帧认证；allowDowngrade=true 时对端连续 3 帧未认证
// 自动回明文（旧引擎互操作，实证：host armed 而对端明文会互丢致死）。
func (c *rudpConn) setAuthKey(key []byte, allowDowngrade bool) {
	c.authMu.Lock()
	if key == nil {
		c.authKey = nil
	} else {
		c.authKey = append([]byte(nil), key...)
	}
	c.allowAuthDowngrade = allowDowngrade
	c.consecutiveAuthDrops = 0
	c.authMu.Unlock()
}

// armedAuthKey 取当前认证密钥（nil=明文）。
func (c *rudpConn) armedAuthKey() []byte {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	return c.authKey
}

// authDrop 认证失败丢弃计数；连续 3 次且允许降级 → 回明文。
func (c *rudpConn) authDrop() {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.authKey == nil {
		return
	}
	c.consecutiveAuthDrops++
	if c.allowAuthDowngrade && c.consecutiveAuthDrops >= 3 {
		c.authKey = nil
	}
}

// authOk 认证通过，复位计数。
func (c *rudpConn) authOk() {
	c.authMu.Lock()
	c.consecutiveAuthDrops = 0
	c.authMu.Unlock()
}

// Start 启动收发循环；连接建立第一件事：发一个 KEEPALIVE（模组 start() 即发）。
func (c *rudpConn) Start() {
	c.started.Store(true)
	c.sendFrame(rudpFrame{Type: rudpTypeKeepalive})
	go c.recvLoop()
	go c.tickLoop()
	go c.keepaliveLoop()
}

// IsConnected 连接是否存活。
func (c *rudpConn) IsConnected() bool { return !c.closed.Load() }

// RemoteAddr 当前发送目标（可能已随对端漂移更新）。
func (c *rudpConn) RemoteAddr() *net.UDPAddr { return c.remote.Load() }

func (c *rudpConn) closeWith(reason string) {
	c.closeMsg.CompareAndSwap(nil, &reason)
	_ = c.Close()
}

// Close 发送 DISCONNECT 并关闭会话与底层 socket。幂等。
func (c *rudpConn) Close() error {
	c.closeOnce.Do(func() {
		// 先尽力发送 DISCONNECT（closed 置位后 sendFrame 会跳过发送）
		if addr := c.remote.Load(); addr != nil {
			udpSendTo(c.conn, rudpEncode(rudpFrame{Type: rudpTypeDisconnect}), addr)
		}
		c.closed.Store(true)
		close(c.doneCh)
		c.conn.SetReadDeadline(time.Now())
		_ = c.conn.Close()
		c.notify(c.dataNotify)
		c.notify(c.ackNotify)
		if fn := c.onClosed; fn != nil {
			msg := "closed"
			if p := c.closeMsg.Load(); p != nil {
				msg = *p
			}
			go fn(msg)
		}
	})
	return nil
}

func (c *rudpConn) notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// sendFrame 尽力发送一帧到当前目标（写错误全部忽略：Windows WSAECONNRESET 等）。
// 帧序：rudp 帧 →（认证模式附 4 字节截断 MAC）→（路径包封 codec）。
func (c *rudpConn) sendFrame(f rudpFrame) {
	if c.closed.Load() {
		return
	}
	addr := c.remote.Load()
	if addr == nil {
		return
	}
	pkt := rudpEncode(f)
	if key := c.armedAuthKey(); key != nil {
		pkt = punchAuthAppendTrailer4(key, pkt)
	}
	if c.codec != nil {
		pkt = c.codec.encode(pkt)
	}
	udpSendTo(c.conn, pkt, addr)
}

// ---- 接收循环 ----

func (c *rudpConn) recvLoop() {
	buf := make([]byte, rudpRecvBufSize)
	for {
		select {
		case <-c.doneCh:
			return
		default:
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(rudpTick))
		n, from, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-c.doneCh:
				return
			default:
				continue // 读超时/WSAECONNRESET 一律继续
			}
		}
		pkt := buf[:n]
		if c.codec != nil {
			frame := c.codec.decode(pkt)
			if frame == nil {
				continue
			}
			pkt = frame
		}
		// 认证模式：所有帧必须携带有效截断 MAC，失败一律丢弃（连续 3 帧未认证
		// 且允许降级时回明文——旧引擎互操作）
		if key := c.armedAuthKey(); key != nil {
			if len(pkt) < 9 || !punchAuthVerifyTrailer4(key, pkt) {
				c.authDrop()
				continue
			}
			c.authOk()
		}
		frame, ok := rudpDecode(pkt)
		if !ok {
			continue
		}
		c.maybeRebindRemote(from)
		c.processFrame(frame)
	}
}

// maybeRebindRemote 对端端口漂移重绑（简化模组 pendingRebindPort 双命中确认：
// 同 IP 新端口连续两次有效帧 → 更新发送目标）。
func (c *rudpConn) maybeRebindRemote(from *net.UDPAddr) {
	if from == nil {
		return
	}
	cur := c.remote.Load()
	if cur == nil {
		c.remote.Store(from)
		return
	}
	if cur.Port == from.Port && cur.IP.Equal(from.IP) {
		c.recvHits.Store(0)
		return
	}
	if !cur.IP.Equal(from.IP) {
		return
	}
	if int(c.driftPort.Load()) == from.Port {
		if c.recvHits.Add(1) >= 2 {
			c.remote.Store(from)
			c.recvHits.Store(0)
		}
	} else {
		c.driftPort.Store(int32(from.Port))
		c.recvHits.Store(1)
	}
}

func (c *rudpConn) markRecv() { c.lastRecv.Store(time.Now().UnixMilli()) }

func (c *rudpConn) processFrame(f rudpFrame) {
	switch f.Type {
	case rudpTypePunch:
		c.markRecv()
		// 回 56 4C 02 00 00（模组 sendPunchAck 同款 5 字节）
		c.sendFrame(rudpFrame{Type: rudpTypePunchAck})
	case rudpTypePunchAck:
		c.markRecv()
	case rudpTypeData:
		c.handleData(f)
	case rudpTypeAck:
		c.markRecv()
		c.processAck(f.Ack)
	case rudpTypeDisconnect:
		c.closeWith("对端断开")
	case rudpTypeKeepalive:
		c.markRecv()
		c.resetFailures()
		// 收到必回一个 KEEPALIVE（ack=nextExpected，模组 sendKeepalive 同款）
		c.sendFrame(rudpFrame{Type: rudpTypeKeepalive, Ack: c.nextExpected.Load()})
	case rudpTypeFecXor:
		c.markRecv()
		c.handleFecXor(f)
	case rudpTypeRestart:
		c.markRecv()
		// 对端 RESTART：触发本端 ICE 重启（拆除重建）
		if fn := c.onRestart; fn != nil {
			go fn()
		}
	case rudpTypeVoice:
		c.markRecv() // 忽略
	}
}

func (c *rudpConn) handleData(f rudpFrame) {
	c.markRecv()
	c.processAck(f.Ack)
	payload := f.Payload
	if payload == nil {
		c.sendAck()
		return
	}
	seq := f.Seq
	groupID := seq / rudpFecGroupSize

	c.recvMu.Lock()
	// FEC 分组登记（收端永远登记，超窗清理）
	c.fecStoreLocked(groupID, seq, payload)

	expected := c.nextExpected.Load()
	switch {
	case seq == expected:
		if c.buffered >= rudpBufChunksMax {
			c.recvMu.Unlock()
			c.sendAck()
			return
		}
		c.deliverLocked(payload)
		for {
			nxt := c.nextExpected.Load()
			cached, okc := c.recvBuf[nxt]
			if !okc || c.buffered >= rudpBufChunksMax {
				break
			}
			delete(c.recvBuf, nxt)
			c.deliverLocked(cached)
		}
	case seqAfter(seq, expected) && seqDiff(seq, expected) < rudpRecvWindow:
		if _, dup := c.recvBuf[seq]; !dup {
			c.recvBuf[seq] = payload
		}
	default:
		// 旧包/超出乱序窗口：只 ACK 不交付
		c.recvMu.Unlock()
		c.sendAck()
		return
	}
	c.recvMu.Unlock()
	c.notify(c.dataNotify)

	c.sendAck()
	c.fecTryRecover(groupID)
}

// fecStoreLocked 登记 DATA 到 FEC 组并做窗口清理（须持 recvMu）。
func (c *rudpConn) fecStoreLocked(groupID, seq uint32, payload []byte) {
	g := c.fecGroups[groupID]
	if g == nil {
		g = map[uint32][]byte{}
		c.fecGroups[groupID] = g
	}
	g[seq] = payload
	if !c.fecHasMin || groupID < c.fecMinGroup {
		c.fecMinGroup = groupID
		c.fecHasMin = true
	}
	if len(c.fecGroups) > 20 {
		cutoff := c.fecMinGroup + 10
		for gid := range c.fecGroups {
			if gid < cutoff {
				delete(c.fecGroups, gid)
				delete(c.fecXor, gid)
				delete(c.fecLens, gid)
			}
		}
		if len(c.fecGroups) > 0 {
			minG := uint32(0xFFFFFFFF)
			for gid := range c.fecGroups {
				if gid < minG {
					minG = gid
				}
			}
			c.fecMinGroup = minG
		}
	}
}

// deliverLocked 载荷入队（须持 recvMu）。
func (c *rudpConn) deliverLocked(payload []byte) {
	c.chunks = append(c.chunks, payload)
	c.buffered++
	c.nextExpected.Store(c.nextExpected.Load() + 1)
}

// fecTryRecover 收齐 3 个 DATA + XOR → 恢复缺失包（模组 tryFecRecovery 同款判定）。
func (c *rudpConn) fecTryRecover(groupID uint32) {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()

	groupData := c.fecGroups[groupID]
	xorPayload := c.fecXor[groupID]
	origLens := c.fecLens[groupID]
	if groupData == nil || xorPayload == nil || origLens == nil {
		return
	}
	startSeq := groupID * rudpFecGroupSize
	missingSeq := uint32(0)
	missingIdx := -1
	received := 0
	for i := 0; i < rudpFecGroupSize; i++ {
		s := startSeq + uint32(i)
		if _, okd := groupData[s]; okd {
			received++
		} else if seqAfter(c.nextExpected.Load(), s) { // 已交付过的也算收到
			received++
		} else {
			missingSeq = s
			missingIdx = i
		}
	}
	switch {
	case received == rudpFecGroupSize-1 && missingIdx >= 0:
		recovered := append([]byte(nil), xorPayload...)
		for _, p := range groupData {
			n := len(p)
			if n > len(recovered) {
				n = len(recovered)
			}
			for i := 0; i < n; i++ {
				recovered[i] ^= p[i]
			}
		}
		origLen := len(recovered)
		if missingIdx < len(origLens) {
			origLen = origLens[missingIdx]
		}
		if origLen < len(recovered) {
			recovered = recovered[:origLen]
		}
		if c.buffered < rudpBufChunksMax {
			c.recvBuf[missingSeq] = recovered
			for {
				nxt := c.nextExpected.Load()
				cached, okc := c.recvBuf[nxt]
				if !okc || c.buffered >= rudpBufChunksMax {
					break
				}
				delete(c.recvBuf, nxt)
				c.deliverLocked(cached)
			}
		}
		delete(c.fecGroups, groupID)
		delete(c.fecXor, groupID)
		delete(c.fecLens, groupID)
	case received == rudpFecGroupSize:
		delete(c.fecGroups, groupID)
		delete(c.fecXor, groupID)
		delete(c.fecLens, groupID)
	}
}

func (c *rudpConn) handleFecXor(f rudpFrame) {
	c.recvMu.Lock()
	c.fecXor[f.Seq] = f.Payload
	c.fecLens[f.Seq] = f.FecLengths
	c.recvMu.Unlock()
	c.fecTryRecover(f.Seq)
}

func (c *rudpConn) sendAck() {
	c.sendFrame(rudpFrame{Type: rudpTypeAck, Ack: c.nextExpected.Load()})
}

// ---- 发送侧 ----

// processAck 累计 ACK：推进 oldestUnacked、更新 RTO、唤醒写侧。
func (c *rudpConn) processAck(ack uint32) {
	c.sendMu.Lock()
	if ack == c.lastAckSeq {
		c.dupAckCount++
		if c.dupAckCount >= 3 && len(c.pending) > 0 {
			if pp, okp := c.pending[c.oldestUnacked]; okp {
				pp.sendTime = time.Now()
				pp.retries++
				c.sendDataPacket(c.oldestUnacked, pp.data, false)
			}
			c.dupAckCount = 0
		}
	} else {
		c.lastAckSeq = ack
		c.dupAckCount = 0
	}

	for len(c.pending) > 0 && seqAfter(ack, c.oldestUnacked) {
		if pp, okp := c.pending[c.oldestUnacked]; okp {
			if pp.retries == 0 {
				c.updateRtoLocked(time.Since(pp.sendTime).Milliseconds())
			}
			delete(c.pending, c.oldestUnacked)
		}
		c.oldestUnacked++
		c.failures = 0
		c.degraded = false
	}
	c.sendMu.Unlock()
	c.notify(c.ackNotify)
}

// updateRtoLocked RFC6298（夹 [100,800]ms，对齐模组 updateRto）。
func (c *rudpConn) updateRtoLocked(sampleMs int64) {
	if sampleMs <= 0 {
		return
	}
	s := float64(sampleMs)
	if c.srtt <= 0 {
		c.srtt = s
		c.rttvar = s / 2
	} else {
		c.rttvar = (3*c.rttvar + absF(c.srtt-s)) / 4
		c.srtt = (7*c.srtt + s) / 8
	}
	rto := int(c.srtt + maxF(10, 4*c.rttvar))
	if rto < rudpRtoMinMs {
		rto = rudpRtoMinMs
	}
	if rto > rudpRtoMaxMs {
		rto = rudpRtoMaxMs
	}
	c.rtoMs = rto
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func (c *rudpConn) resetFailures() {
	c.sendMu.Lock()
	c.failures = 0
	c.degraded = false
	c.sendMu.Unlock()
}

// sendDataPacket 编码并发送 DATA；小包首发 50ms 后复制重发一次（非重传路径）。
func (c *rudpConn) sendDataPacket(seq uint32, payload []byte, first bool) {
	c.sendFrame(rudpFrame{
		Type:    rudpTypeData,
		Seq:     seq,
		Ack:     c.nextExpected.Load(),
		Payload: payload,
	})
	if first && len(payload) < rudpSmallThresh && !c.degraded {
		dup := append([]byte(nil), payload...)
		time.AfterFunc(rudpSmallDupDelay, func() {
			if c.closed.Load() {
				return
			}
			c.sendFrame(rudpFrame{
				Type: rudpTypeData, Seq: seq, Ack: c.nextExpected.Load(), Payload: dup,
			})
		})
	}
}

// computeXorPayload 多包异或（对齐模组 computeXorPayload：按最长长度逐字节异或）。
func computeXorPayload(payloads [][]byte) []byte {
	maxLen := 0
	for _, p := range payloads {
		if len(p) > maxLen {
			maxLen = len(p)
		}
	}
	xor := make([]byte, maxLen)
	for _, p := range payloads {
		for i := 0; i < len(p); i++ {
			xor[i] ^= p[i]
		}
	}
	return xor
}

// sendFecFrame FEC_XOR 帧字节布局照模组 sendFecPacket（820-842 行）。
func (c *rudpConn) sendFecFrame(groupID uint32, xorPayload []byte, lengths []int) {
	c.sendFrame(rudpFrame{
		Type:       rudpTypeFecXor,
		Seq:        groupID,
		Payload:    xorPayload,
		FecCount:   len(lengths),
		FecLengths: lengths,
	})
}

// maxSendChunk 当前路径的发送分块上限：TURN 包封路径要给信封让出字节，否则节点按 1400 静默丢。
func (c *rudpConn) maxSendChunk() int {
	if c.codec != nil {
		return rudpTurnMaxChunk
	}
	return rudpMaxPayload
}

// Write 实现 io.Writer：按当前路径上限分片写入隧道。
func (c *rudpConn) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		end := written + c.maxSendChunk()
		if end > len(p) {
			end = len(p)
		}
		if err := c.writeChunk(p[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

// writeChunk 发送一个分片：窗口控制 → 登记 pending → 发 DATA → FEC 组管理。
func (c *rudpConn) writeChunk(chunk []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	stuckStart := time.Time{}
	var stuckBase uint32
	for seqDiff(c.nextSeq, c.oldestUnacked) >= uint32(c.window) {
		if c.closed.Load() {
			return errRudpClosed
		}
		c.sendMu.Unlock()
		select {
		case <-c.doneCh:
			c.sendMu.Lock()
			return errRudpClosed
		case <-c.ackNotify:
		case <-time.After(time.Second):
		}
		c.sendMu.Lock()
		if c.closed.Load() {
			return errRudpClosed
		}
		if stuckStart.IsZero() || c.oldestUnacked != stuckBase {
			stuckStart = time.Now()
			stuckBase = c.oldestUnacked
		} else if time.Since(stuckStart) >= rudpStuckWriteLimit {
			go c.closeWith("传输停滞")
			return errRudpStuck
		}
	}

	seq := c.nextSeq
	c.nextSeq++
	data := append([]byte(nil), chunk...)
	c.pending[seq] = &rudpPending{data: data, sendTime: time.Now()}
	c.sendDataPacket(seq, data, true)

	// FEC 发送组：组满 4 包补发一个 XOR（模组默认开 FEC，组内同步发送保证帧序）
	groupID := int64(seq / rudpFecGroupSize)
	if groupID != c.fecSendGroupID {
		c.fecSendGroup = c.fecSendGroup[:0]
		c.fecSendGroupID = groupID
	}
	c.fecSendGroup = append(c.fecSendGroup, data)
	if len(c.fecSendGroup) == rudpFecGroupSize {
		group := make([][]byte, rudpFecGroupSize)
		copy(group, c.fecSendGroup)
		lengths := make([]int, rudpFecGroupSize)
		for i, g := range group {
			lengths[i] = len(g)
		}
		c.fecSendGroup = c.fecSendGroup[:0]
		c.sendFecFrame(uint32(groupID), computeXorPayload(group), lengths)
	}
	return nil
}

// tickLoop 重传/降级/判死循环（50ms）。
func (c *rudpConn) tickLoop() {
	t := time.NewTicker(rudpTick)
	defer t.Stop()
	for {
		select {
		case <-c.doneCh:
			return
		case <-t.C:
		}
		c.retransmitCheck()
	}
}

func (c *rudpConn) retransmitCheck() {
	now := time.Now()
	silence := time.Since(time.UnixMilli(c.lastRecv.Load()))

	c.sendMu.Lock()
	if len(c.pending) > 0 {
		if c.failures >= 5 && silence > rudpSilenceDegrade {
			c.degraded = true
		}
		if silence > rudpSilenceDead {
			c.sendMu.Unlock()
			c.closeWith("静默超24s且有待确认包，判对端已死")
			return
		}
	}
	if silence > rudpSilenceHard {
		c.sendMu.Unlock()
		c.closeWith("静默超60s")
		return
	}

	maxRetrans := 24000 / max(c.rtoMs, rudpRtoMinMs)
	maxRetrans = min(maxRetrans, 120)
	maxRetrans = max(maxRetrans, 10)

	for seq, pp := range c.pending {
		backoff := time.Duration(c.rtoMs+min(pp.retries, 3)*rudpBackoffMs) * time.Millisecond
		if now.Sub(pp.sendTime) <= backoff {
			continue
		}
		if pp.retries >= maxRetrans {
			c.sendMu.Unlock()
			c.closeWith(fmt.Sprintf("seq %d 重传超限", seq))
			return
		}
		pp.sendTime = now
		pp.retries++
		c.failures++
		c.sendDataPacket(seq, pp.data, false)
	}
	c.sendMu.Unlock()
}

func (c *rudpConn) keepaliveLoop() {
	t := time.NewTicker(rudpKeepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-c.doneCh:
			return
		case <-t.C:
			if c.closed.Load() {
				return
			}
			c.sendFrame(rudpFrame{Type: rudpTypeKeepalive, Ack: c.nextExpected.Load()})
		}
	}
}

// Read 实现 io.Reader：阻塞读取按序到达的载荷块。连接关闭后队列排空返回 io.EOF。
func (c *rudpConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		c.recvMu.Lock()
		if len(c.chunks) > 0 {
			chunk := c.chunks[0]
			n := copy(p, chunk)
			if n == len(chunk) {
				c.chunks = c.chunks[1:]
				c.buffered--
			} else {
				c.chunks[0] = chunk[n:]
			}
			c.recvMu.Unlock()
			return n, nil
		}
		closed := c.closed.Load()
		c.recvMu.Unlock()
		if closed {
			return 0, io.EOF
		}
		select {
		case <-c.doneCh:
		case <-c.dataNotify:
		case <-time.After(500 * time.Millisecond):
		}
	}
}
