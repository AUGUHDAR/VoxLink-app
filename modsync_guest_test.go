package main

// modsync_guest_test.go — 离线 diff 的六类分流与文件名归一化，
// 对照 ModSyncGuestService.computeDiff（:427-559）与 normalizeFileName（:566-589）。

import (
	"crypto/sha1"
	"encoding/hex"
	"testing"
)

func sha1Of(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func modSyncTestEnv(t *testing.T) modSyncEnv {
	t.Helper()
	return modSyncEnv{
		Loader:      "fabric",
		GameVersion: "1.20.1",
		ModsDir:     t.TempDir(),
	}
}

func modSyncTestManifest(mods []any, unknown []any) map[string]any {
	m := map[string]any{
		"loader":        "fabric",
		"mcVersion":     "1.20.1",
		"mods":          mods,
		"unknownMods":   unknown,
		"protocolVersion": modSyncProtocolVersion,
	}
	return m
}

func modSyncTestWireEntry(projectID, fileName, sha1Hex string, size int64, url string, loaders, gvs []any) map[string]any {
	return map[string]any{
		"projectId":     projectID,
		"slug":          projectID,
		"title":         projectID,
		"versionNumber": "1.0",
		"fileName":      fileName,
		"url":           url,
		"sha1":          sha1Hex,
		"sha512":        "",
		"size":          float64(size),
		"loaders":       loaders,
		"gameVersions":  gvs,
	}
}

func TestModSyncDiffInstalledBySha1IsSilent(t *testing.T) {
	env := modSyncTestEnv(t)
	writeJar(t, env.ModsDir, "sodium.jar", "same-bytes")
	manifest := modSyncTestManifest([]any{
		modSyncTestWireEntry("pSodium", "sodium.jar", sha1Of("same-bytes"), 10,
			"https://cdn.modrinth.com/x/sodium.jar", []any{"fabric"}, []any{"1.20.1"}),
	}, []any{})

	d := modSyncComputeDiff(manifest, env)
	if len(d.Downloadable)+len(d.ManualEntries)+len(d.Unresolvable)+len(d.VersionDiff) != 0 {
		t.Fatalf("已装（sha1 命中）不该产生任何提示：%+v", d)
	}
}

func TestModSyncDiffInstalledButDisabled(t *testing.T) {
	env := modSyncTestEnv(t)
	// 玩家把 sodium.jar 重命名为 sodium.jar.disabled
	writeJar(t, env.ModsDir, "sodium.jar.disabled", "disabled-bytes")
	manifest := modSyncTestManifest([]any{
		modSyncTestWireEntry("pDis", "sodium.jar", sha1Of("disabled-bytes"), 9,
			"https://cdn.modrinth.com/x/sodium.jar", []any{"fabric"}, []any{"1.20.1"}),
	}, []any{})

	d := modSyncComputeDiff(manifest, env)
	if len(d.Downloadable) != 0 {
		t.Fatalf("被禁用=已装，不应再下载：%v", d.Downloadable)
	}
	if len(d.Unresolvable) != 1 || d.Unresolvable[0].Key != langModSyncDisabledHint {
		t.Fatalf("应给 disabled_hint：%+v", d.Unresolvable)
	}
	if len(d.Unresolvable[0].Args) != 2 || d.Unresolvable[0].Args[1] != "sodium.jar" {
		t.Fatalf("hint 参数应是 (title, fileName)：%+v", d.Unresolvable[0])
	}
}

func TestModSyncDiffVersionMismatchNeverTouchesFiles(t *testing.T) {
	modSyncResetGateStateForTest() // skipped 是进程级一次性集合，-count=N 第二轮会撞在旧键上
	env := modSyncTestEnv(t)
	// 归一化名相同（fabric/mc 噪音词会被剔掉）、sha1 不同 → 版本不一致组
	writeJar(t, env.ModsDir, "anothermod-fabric-1.19.9.jar", "old-version-bytes")
	manifest := modSyncTestManifest([]any{
		modSyncTestWireEntry("pVer", "anothermod-fabric-1.20.1.jar", sha1Of("new-version-bytes"), 8,
			"https://cdn.modrinth.com/x/anothermod.jar", []any{"fabric"}, []any{"1.20.1"}),
	}, []any{})

	d := modSyncComputeDiff(manifest, env)
	if len(d.VersionDiff) != 1 || d.VersionDiff[0].Key != langModSyncDiffLine {
		t.Fatalf("应进 versionDiff 组：%+v", d.VersionDiff)
	}
	if len(d.SkipKeys) != 1 || d.SkipKeys[0] != "pVer@1.0" {
		t.Fatalf("skipKey 必须是 projectId@versionNumber：%v", d.SkipKeys)
	}
	if len(d.Downloadable) != 0 {
		t.Fatal("版本不一致组不得自动下载")
	}
}

func TestModSyncDiffDownloadableVersusManual(t *testing.T) {
	env := modSyncTestEnv(t)
	compatible := modSyncTestWireEntry("pOK", "okmod-1.0.jar", sha1Of("nope1"), 7,
		"https://cdn.modrinth.com/x/okmod.jar", []any{"fabric"}, []any{"1.20.1"})
	wrongLoader := modSyncTestWireEntry("pWR", "wrmod-1.0.jar", sha1Of("nope2"), 7,
		"https://cdn.modrinth.com/x/wrmod.jar", []any{"forge"}, []any{"1.20.1"})
	noURL := modSyncTestWireEntry("pNoURL", "nourl-1.0.jar", sha1Of("nope3"), 7,
		"", []any{"fabric"}, []any{"1.20.1"})
	manifest := modSyncTestManifest([]any{compatible, wrongLoader, noURL}, []any{"自写模组.jar"})

	d := modSyncComputeDiff(manifest, env)
	if len(d.Downloadable) != 1 || d.Downloadable[0].ProjectID != "pOK" {
		t.Fatalf("只有 loaders+gameVersions+url 全合格才可下载：%+v", d.Downloadable)
	}
	// 2 条不合格 + 1 条 unknownMods
	if len(d.ManualEntries) != 3 {
		t.Fatalf("不合格与 unknownMods 都应进手动组：%+v", d.ManualEntries)
	}
}

func TestModSyncDiffLegacyManifestNoSha1(t *testing.T) {
	env := modSyncTestEnv(t)
	p := writeJar(t, env.ModsDir, "legacy.jar", "abcdefgh")
	st, err := modSyncFileSize(p)
	if err != nil {
		t.Fatal(err)
	}
	// 无 sha1 → 精确文件名匹配即视为已装（:505-506）
	byName := modSyncTestWireEntry("pL1", "legacy.jar", "", st, "https://cdn.modrinth.com/x/legacy.jar",
		[]any{"fabric"}, []any{"1.20.1"})
	d := modSyncComputeDiff(modSyncTestManifest([]any{byName}, []any{}), env)
	if len(d.Downloadable)+len(d.ManualEntries) != 0 {
		t.Fatalf("精确文件名命中=已装：%+v", d)
	}

	// 无 sha1 且文件名不同、但归一名+字节数一致 → 仍视为已装（:511-518）
	byNorm := modSyncTestWireEntry("pL2", "legacy-1.0.jar", "", st, "https://cdn.modrinth.com/x/legacy.jar",
		[]any{"fabric"}, []any{"1.20.1"})
	d2 := modSyncComputeDiff(modSyncTestManifest([]any{byNorm}, []any{}), env)
	if len(d2.Downloadable)+len(d2.VersionDiff)+len(d2.ManualEntries) != 0 {
		t.Fatalf("归一名+大小一致应视为已装：%+v", d2)
	}
}

func TestModSyncNormalizeFileName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Sodium-fabric-mc1.20.1-0.5.3.jar", "sodium"},
		{"Iron-Chests-Mod-1.20.1.jar", "ironchestsmod"},
		{"Xaero_Minimap_Fabric_1.20.1.jar", "xaerominimap"},
		{"", ""},
		{"no-dot-jar", "nodotjar"},
	}
	for _, c := range cases {
		if got := modSyncNormalizeFileName(c.in); got != c.want {
			t.Errorf("normalize(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestModSyncCleanTextStripsSectionCodes(t *testing.T) {
	// ModSyncSelectScreen.cleanText（:175-177）：Java 源码里的 "\u00a7." 到正则阶段已是「§ + 任意一击」，
	// 所以 § 和它后面那个字符一起被吃掉——"§aName" → "Name"，不是 "aName"。
	if got := modSyncCleanText("§aName\tX"); got != "NameX" {
		t.Fatalf("cleanText=%q want %q", got, "NameX")
	}
	if got := modSyncCleanText("a§bC"); got != "aC" {
		t.Fatalf("cleanText=%q want %q", got, "aC")
	}
	if got := modSyncCleanText(""); got != "" {
		t.Fatalf("空串应原样返回：%q", got)
	}
}

func TestModSyncSkipKeySessionDedupe(t *testing.T) {
	modSyncResetGateStateForTest() // 本用例正是验"同 skipKey 只提示一次"，必须先清进程级集合
	env := modSyncTestEnv(t)
	// 归一化按 [^a-z]+ 切词：版本号那截是分隔符，所以 dupmod-1.0 与 dupmod-2.0 同基名=同一模组不同版本。
	// 大小刻意与清单 entry.size(5) 不一致，避免走"归一名+大小一致视为已装"的兜底。
	writeJar(t, env.ModsDir, "dupmod-1.0.jar", "v1-bytes")
	entry := modSyncTestWireEntry("pDup", "dupmod-2.0.jar", sha1Of("v2-bytes"), 5,
		"https://cdn.modrinth.com/x/dupmod.jar", []any{"fabric"}, []any{"1.20.1"})
	manifest := modSyncTestManifest([]any{entry}, []any{})

	first := modSyncComputeDiff(manifest, env)
	if len(first.VersionDiff) != 1 {
		t.Fatalf("首次应提示：%+v", first.VersionDiff)
	}
	second := modSyncComputeDiff(manifest, env)
	if len(second.VersionDiff) != 0 {
		t.Fatalf("同 skipKey 本会话只提示一次（:537-543）：%+v", second.VersionDiff)
	}
}
