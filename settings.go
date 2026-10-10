package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// appSettings 持久化设置，存于 %APPDATA%/VoxLinkApp/settings.json。
type appSettings struct {
	ServerURL string `json:"serverUrl"`
	Theme     string `json:"theme"`
}

func defaultSettings() appSettings {
	return appSettings{
		ServerURL: defaultServerURL,
		Theme:     "light",
	}
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
