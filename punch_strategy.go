package main

// punch_strategy.go — NAT 分级与打洞策略矩阵（对齐模组 NatClass.java /
// PunchStrategySelector.java）。NAT 分级取自 StunProbe 两服务器 delta 法：
// 端口一致 → cone；|delta| < 100 → easySym；否则 hardSym。

type natClass int

const (
	natUnknown natClass = iota
	natCone
	natEasySym
	natHardSym
)

func (c natClass) String() string {
	switch c {
	case natCone:
		return "CONE"
	case natEasySym:
		return "EASY_SYM"
	case natHardSym:
		return "HARD_SYM"
	}
	return "UNKNOWN"
}

func (c natClass) symmetric() bool { return c == natEasySym || c == natHardSym }

// natClassFromDelta 两服务器映射端口差分级（StunProbe delta 法语义）。
// 样本不足 → unknown；端口一致 → cone；|d|<100 → easySym；否则 hardSym。
func natClassFromDelta(ports []int) natClass {
	if len(ports) < 2 {
		return natUnknown
	}
	delta := ports[len(ports)-1] - ports[0]
	if delta == 0 {
		return natCone
	}
	if delta < 0 {
		delta = -delta
	}
	if delta < 100 {
		return natEasySym
	}
	return natHardSym
}

// natClassFromRoomType 房间上报 natType 字符串 → 分级。
func natClassFromRoomType(t string) natClass {
	switch t {
	case "full_cone", "restricted_cone", "port_restricted_cone", "cone":
		return natCone
	case "symmetric_easy_inc", "symmetric_easy_dec", "symmetric_easy":
		return natEasySym
	case "symmetric", "symmetric_hard":
		return natHardSym
	}
	return natUnknown
}

// recommendProfile (localNat,remoteNat) 排列矩阵（NatClass.recommendProfile 同款）。
func recommendProfile(local, remote natClass) *punchProfile {
	if local == natUnknown || remote == natUnknown {
		return punchAggressive
	}
	if local == natHardSym && remote == natHardSym {
		return punchHardSym
	}
	if (local == natEasySym && remote == natHardSym) || (local == natHardSym && remote == natEasySym) {
		return punchHardSym
	}
	if local == natEasySym && remote == natEasySym {
		return punchEasySymDual
	}
	if local == natCone && remote == natCone {
		return punchFastLane
	}
	if (local == natCone && remote == natEasySym) || (local == natEasySym && remote == natCone) {
		return punchV100
	}
	if (local == natCone && remote == natHardSym) || (local == natHardSym && remote == natCone) {
		return punchAggressive
	}
	return punchDefault
}

type punchStrategy int

const (
	strategyDirectOnly punchStrategy = iota
	strategyDirectWithReverseParallel
	strategyReverseFirst
	strategyRelayFallbackFast
	strategyReverseOnly
	strategyParallelFromStart
	strategyReverseThenForward
)

// selectStrategy PunchStrategySelector.select 同款矩阵。
// isLegacyPeer：老版本对端强制 DIRECT_ONLY。
func selectStrategy(localNat, remoteNat natClass, cycle int, isLegacyPeer bool) punchStrategy {
	if isLegacyPeer {
		return strategyDirectOnly
	}
	if localNat == natUnknown || remoteNat == natUnknown {
		if localNat == natUnknown || remoteNat == natUnknown {
			known := remoteNat
			if localNat != natUnknown {
				known = localNat
			}
			if known.symmetric() {
				return strategyReverseFirst
			}
		}
		return strategyDirectWithReverseParallel
	}
	if (localNat == natCone && remoteNat == natEasySym) || (localNat == natEasySym && remoteNat == natCone) {
		return strategyReverseFirst
	}
	if (localNat == natCone && remoteNat == natHardSym) || (localNat == natHardSym && remoteNat == natCone) {
		if cycle == 0 {
			return strategyReverseOnly
		}
		return strategyReverseThenForward
	}
	if (localNat == natEasySym && remoteNat == natHardSym) || (localNat == natHardSym && remoteNat == natEasySym) {
		return strategyReverseFirst
	}
	if localNat == natHardSym && remoteNat == natHardSym {
		return strategyDirectWithReverseParallel
	}
	if localNat == natEasySym && remoteNat == natEasySym {
		return strategyDirectWithReverseParallel
	}
	if localNat == natCone && remoteNat == natCone {
		return strategyDirectOnly
	}
	return strategyDirectWithReverseParallel
}
