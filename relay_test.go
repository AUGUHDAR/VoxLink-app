package main

// relay_test.go — 玩家中继的房主侧契约（对齐模组 1.1.9 的候选池裁决 + 1.2.0 的 v2 数据面）：
//   - 候选池 = 除本房外所有房间里 relayEnabled 且非对称 NAT 的人（excludeRoom 自报）
//   - 派发按"本会话已用过几次"挑最闲的候选，不退化成永远取第一个
//   - relay_setup 带 replyHostId，relay_accept 必须转告自家房客
//   - 1.2.0：relay_accept 必须带 chan，且候选者传输已 mux 化，否则一律不报 connected

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func newRelayTestApp(t *testing.T, handler http.HandlerFunc) *App {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	a := NewApp()
	a.settingsPath = filepath.Join(t.TempDir(), "settings.json")
	a.mu.Lock()
	a.settings.ServerURL = srv.URL
	a.mu.Unlock()
	return a
}

func sendJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TestFetchRelayCandidatesUsesGlobalPoolWithExcludeRoom 不带 roomCode（带了就只回本房成员），
// 整房排除靠 excludeRoom=<本房>。
func TestFetchRelayCandidatesUsesGlobalPoolWithExcludeRoom(t *testing.T) {
	var gotRoute, gotRoomCode, gotExclude string
	a := newRelayTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotRoute, gotRoomCode, gotExclude = q.Get("route"), q.Get("roomCode"), q.Get("excludeRoom")
		sendJSON(w, map[string]any{"success": true, "data": map[string]any{"candidates": []any{
			map[string]any{"clientId": "c1", "natType": "full-cone", "mappedIp": "203.0.113.9", "mappedPort": 41000},
		}}})
	})
	e := a.engine
	e.mu.Lock()
	e.code = "HOST01"
	e.mu.Unlock()

	cands, err := e.fetchRelayCandidates(e.code)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ClientID != "c1" {
		t.Fatalf("candidates = %+v", cands)
	}
	if gotRoute != "/relay/candidates" {
		t.Fatalf("route = %q", gotRoute)
	}
	if gotRoomCode != "" {
		t.Fatalf("must not scope the pool to our own room, got roomCode=%q", gotRoomCode)
	}
	if gotExclude != "HOST01" {
		t.Fatalf("excludeRoom = %q, want HOST01", gotExclude)
	}
}

func relayCand(id, nat, ip string, port int) relayCandidate {
	return relayCandidate{ClientID: id, NatType: nat, MappedIP: ip, MappedPort: port}
}

// TestPickRelayCandidateSkipsOwnRoomAndBalancesLoad 自家房客/发起人/自己的房主身份/
// 对称与 unknown NAT 一律不投；剩下的按已派发次数升序取最闲的。
func TestPickRelayCandidateSkipsOwnRoomAndBalancesLoad(t *testing.T) {
	a := NewApp()
	e := a.engine
	e.mu.Lock()
	e.code = "HOST01"
	e.clientID = "self-client"
	e.hPeers = map[string]*hostPeer{"mate-1": {}, "mate-2": {}}
	e.mu.Unlock()

	cands := []relayCandidate{
		relayCand("mate-1", "full-cone", "203.0.113.1", 40001), // 同房成员：候选池要整房排除
		relayCand("requester", "full-cone", "203.0.113.2", 40002),
		relayCand("self-client", "full-cone", "203.0.113.3", 40003),
		relayCand("host-HOST01", "full-cone", "203.0.113.4", 40004), // 本房房主身份
		relayCand("sym-guy", "symmetric", "203.0.113.5", 40005),
		relayCand("no-addr", "full-cone", "", 0),
		relayCand("busy", "full-cone", "203.0.113.6", 40006),
		relayCand("idle", "cone", "203.0.113.7", 40007),
	}

	if got := e.pickRelayCandidate(cands, "requester", "self-client"); got == nil || got.ClientID != "busy" {
		t.Fatalf("first pick = %+v, want busy（服务端洗牌后的首位，载荷计数相同）", got)
	}
	// 第二次派发：busy 已经背了一次，必须让给更闲的 idle，而不是继续取第一个
	if got := e.pickRelayCandidate(cands, "requester", "self-client"); got == nil || got.ClientID != "idle" {
		t.Fatalf("second pick = %+v, want idle（按派发次数挑最闲）", got)
	}

	// 全被排除时返回 nil，让调用方 declined
	onlyMine := []relayCandidate{relayCand("mate-2", "full-cone", "203.0.113.8", 40008)}
	if got := e.pickRelayCandidate(onlyMine, "requester", "self-client"); got != nil {
		t.Fatalf("own-room-only pool must yield nil, got %+v", got)
	}
}

// TestHostOnRelayAcceptForwardsConnected 候选者回执 relay_accept 后，房主要把
// "桥已就绪"转成 relay_notify{connected} 投给当初发起请求的房客。
func TestHostOnRelayAcceptForwardsConnected(t *testing.T) {
	type sig struct {
		Code   string         `json:"code"`
		Token  string         `json:"token"`
		IsHost bool           `json:"isHost"`
		Type   string         `json:"type"`
		To     string         `json:"to"`
		Data   map[string]any `json:"data"`
	}
	var got []sig
	a := newRelayTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		var s sig
		_ = json.NewDecoder(r.Body).Decode(&s)
		got = append(got, s)
		sendJSON(w, map[string]any{"success": true, "data": map[string]any{}})
	})
	e := a.engine
	e.mu.Lock()
	if e.hPeers == nil {
		e.hPeers = map[string]*hostPeer{}
	}
	e.code, e.token, e.isHost = "HOST01", "host-token", true
	e.mu.Unlock()

	// 玩家中继 v2：host 侧接一条被中继会话，必须有该候选者的、已 mux 化的 joiner 传输。
	uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("建测试 UDP socket 失败: %v", err)
	}
	t.Cleanup(func() { _ = uc.Close() })
	candRudp := newRudp(uc, nil)
	candRudp.enableMux()
	e.mu.Lock()
	e.hPeers["cand-1"] = &hostPeer{id: "cand-1", rudp: candRudp}
	e.mu.Unlock()

	// 旧版候选者不带 chan：v1 裸流已被实证为跨流污染，一律不接
	e.hostOnRelayAccept("cand-1", map[string]any{"forClientId": "guest-9"})
	if len(got) != 0 {
		t.Fatalf("chan-less relay_accept must be ignored (v1 retired), signals=%+v", got)
	}

	e.hostOnRelayAccept("cand-1", map[string]any{"forClientId": "guest-9", "chan": 1})
	if len(got) != 1 {
		t.Fatalf("signals = %+v, want exactly one relay_notify", got)
	}
	s := got[0]
	if s.Type != "relay_notify" || s.To != "guest-9" || s.Code != "HOST01" || !s.IsHost {
		t.Fatalf("forwarded signal = %+v", s)
	}
	if s.Data["connected"] != true {
		t.Fatalf("relay_notify must carry connected=true, got %+v", s.Data)
	}

	// 缺 forClientId 就无从转告，不能广播
	e.hostOnRelayAccept("cand-1", map[string]any{"chan": 2})
	if len(got) != 1 {
		t.Fatalf("blank forClientId must not send anything, signals=%+v", got)
	}

	// 候选者传输未 mux 化（例如它其实是旧版）：不报 connected，让被中继侧超时换下一个候选
	e.mu.Lock()
	e.hPeers["cand-old"] = &hostPeer{id: "cand-old", rudp: newRudp(uc, nil)}
	e.mu.Unlock()
	e.hostOnRelayAccept("cand-old", map[string]any{"forClientId": "guest-9", "chan": 3})
	if len(got) != 1 {
		t.Fatalf("non-mux candidate must not report connected, signals=%+v", got)
	}
}

// 注册进候选池时上报的 natType：判 cone 必须连公网 IP 一起比。
// 服务端候选门槛是"含 sym 的不进池"（relaymanager.go:343），
// 所以把对称 NAT 报错成 cone 不是显示问题，而是让别人把流量投递到一台收不到包的候选上。
func TestNatTypeFromSamplesRequiresPublicIPEquality(t *testing.T) {
	sa := func(ip string, port int) stunMappedAddr { return stunMappedAddr{IP: ip, Port: port} }
	cases := []struct {
		name   string
		sample []stunMappedAddr
		want   string
	}{
		{"同 IP 同端口", []stunMappedAddr{sa("1.1.1.1", 20000), sa("1.1.1.1", 20000)}, "full_cone"},
		{"同 IP 漂移", []stunMappedAddr{sa("1.1.1.1", 20000), sa("1.1.1.1", 20400)}, "symmetric"},
		{"异 IP 同端口", []stunMappedAddr{sa("1.1.1.1", 20000), sa("2.2.2.2", 20000)}, "symmetric"},
		{"单样本", []stunMappedAddr{sa("1.1.1.1", 20000)}, "unknown"},
		{"无样本", nil, "unknown"},
	}
	for _, c := range cases {
		if got := natTypeFromSamples(c.sample, ""); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// 无样本才用旧值兜底（有样本时旧值不能盖掉实测结果）
	if got := natTypeFromSamples(nil, "full_cone"); got != "full_cone" {
		t.Errorf("空样本应沿用旧值, got %q", got)
	}
	if got := natTypeFromSamples([]stunMappedAddr{sa("1.1.1.1", 20000), sa("2.2.2.2", 20000)}, "full_cone"); got != "symmetric" {
		t.Errorf("有样本时旧值不该盖掉实测, got %q", got)
	}
}
