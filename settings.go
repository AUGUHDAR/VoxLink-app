package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// appSettings 持久化设置，存于 %APPDATA%/VoxLinkApp/settings.json。
type appSettings struct {
	ServerURL string `json:"serverUrl"`
	Theme     string `json:"theme"`
	// ClientTag 启动器/用户来源标记（如 kamucl），随 create 上报服务端供后台展示
	// （server-go/roommanager.go:409-413 同款校验）；默认空 = 不上报。
	ClientTag string `json:"clientTag"`
	// RelayEnabled 与模组 VoxLinkConfig.relayEnabled 同一个开关、同一个默认值 true（:41/:64/:92）。
	// 一个开关管两件事："为别人中继"与"用别人的中继"（模组 tryRelay 在关闭时以
	// connection.relay_unavailable 直接退出，CM:10520-10526），它不管 TURN（fireAutoTurn 无此判，CM:12830-12837）。
	// 用 *bool 是为了区分"老配置文件没这一项"（nil→按默认 true）与用户显式关掉。
	RelayEnabled *bool `json:"relayEnabled"`
}

func defaultSettings() appSettings {
	on := true
	return appSettings{
		ServerURL:    defaultServerURL,
		Theme:        "light",
		ClientTag:    "",
		RelayEnabled: &on,
	}
}

// relayEnabled 当前中继开关（nil 表示历史配置没这一项，按模组默认 true）。
func (a *App) relayEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.settings.RelayEnabled == nil {
		return true
	}
	return *a.settings.RelayEnabled
}

// defaultSettingsPath 返回设置文件路径（基于 os.UserConfigDir，Windows 即 %APPDATA%）。
func defaultSettingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		// 兜底：用户目录下
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "VoxLinkApp-settings.json"
		}
		dir = home
	}
	return filepath.Join(dir, "VoxLinkApp", "settings.json")
}

// loadSettings 从磁盘加载设置，缺失/损坏时回落默认值。
// 契约：serverUrl 恒为默认（磁盘上的历史自定义值一律忽略，只应用 theme）。
func (a *App) loadSettings() {
	s := defaultSettings()
	data, err := os.ReadFile(a.settingsPath)
	if err == nil {
		_ = json.Unmarshal(data, &s)
	}
	s.ServerURL = defaultServerURL
	if s.Theme == "" {
		s.Theme = "light"
	}
	s.ClientTag = normalizeClientTag(s.ClientTag)
	a.mu.Lock()
	a.settings = s
	a.mu.Unlock()
}

// saveSettings 把当前设置写盘。
func (a *App) saveSettings() error {
	a.mu.Lock()
	s := a.settings
	a.mu.Unlock()

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("设置序列化失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(a.settingsPath), 0o755); err != nil {
		return fmt.Errorf("创建设置目录失败: %w", err)
	}
	if err := os.WriteFile(a.settingsPath, data, 0o644); err != nil {
		return fmt.Errorf("写入设置文件失败: %w", err)
	}
	return nil
}

// validateServerURL 校验服务器地址（允许 http/https）。
func validateServerURL(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" {
		return false
	}
	lower := strings.ToLower(u)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// clientTagPattern 与服务端 roommanager.go:411 的正则逐字节一致（^[a-z0-9_\-]{1,32}$，
// Go RE2 里 `_`/`-` 在字符类中无需转义，写法 ^[a-z0-9_-]{1,32}$ 语义相同）。
var clientTagPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// normalizeClientTag 复刻服务端处理（server-go/roommanager.go:410-413）：
// 先 TrimSpace 再 ToLower，然后匹配正则；不合规"丢弃"为空而非报错——
// 与服务端对非法 clientTag 静默置空的口径一致（兼容老客户端语义）。
func normalizeClientTag(tag string) string {
	t := strings.ToLower(strings.TrimSpace(tag))
	if t == "" {
		return ""
	}
	if !clientTagPattern.MatchString(t) {
		return ""
	}
	return t
}

// validateRoomCode 校验 6 位房间码字符集。
func validateRoomCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for _, c := range code {
		if !strings.ContainsRune(charset, c) {
			return false
		}
	}
	return true
}
