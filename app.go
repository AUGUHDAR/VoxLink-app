package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	appVersion       = "1.1.9-beta"
	defaultServerURL = "https://p2p.wuhui.icu"
)

// appCapabilities 本端协议能力（与模组 ProtocolNegotiator.CURRENT_CAPABILITIES 对齐，
// 只声明本端真正实现的项：模组同步/overlay 认证未接入，故不声明）。
//   - relay / continuous_retry：玩家中继与持续重试
//   - ice_restart：链路死亡后的重新协商（requestIceRestart）
//   - punchAuthV1：打洞控制包与数据面帧附截断 HMAC-SHA256
//   - stdTurnV1：TURN 中继走标准 RFC 5766（否则自动回落自定义协议 v1）
var appCapabilities = []string{"relay", "ice_restart", "continuous_retry", "punchAuthV1", "stdTurnV1"}

// App 是绑定给前端的 Wails 结构体，持有全部会话状态。
type App struct {
	ctx          context.Context
	api          *apiClient
	settingsPath string
	settings     appSettings
	engine       *connEngine

	mu      sync.Mutex
	state   string // "idle" | "hosting" | "in_room"
	code    string
	token   string
	isHost  bool
	room    map[string]any
	session *session
}

// NewApp 创建 App 实例（测试可覆盖 settingsPath）。
func NewApp() *App {
	a := &App{
		api:      newAPIClient(),
		state:    "idle",
		settings: defaultSettings(),
	}
	a.settingsPath = defaultSettingsPath()
	a.engine = newConnEngine(a)
	return a
}

// startup 由 Wails 在启动时调用。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.loadSettings()
}

// shutdown 停掉会话（不保证 leave 成功）。
func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	s := a.session
	a.mu.Unlock()
	if s != nil {
		s.stop()
		a.engine.unbindSession(s)
	}
}

// ---- 事件辅助 ----

// emit 安全地发 Wails 事件（ctx 为 nil 时静默跳过，便于测试）。
func (a *App) emit(event string, data any) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, event, data)
}

// netLogSanitize 兜底消敏：去掉 IPv4、loopback 字面量、:port、IP:port 等模式。
// 本轮改造清单要求所有 emitNetLog 消息不得包含任何 IP/端口/地址，正则自检也在测试中。
var (
	netLogIPv4Re   = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	netLogHostPort = regexp.MustCompile(`(?:\[?[0-9a-fA-F:]+\]?:\d{1,5})`)
	netLogAnyPort  = regexp.MustCompile(`:\d{2,5}\b`)
)

// sanitizeNetLogMessage 防御性兜底：去掉消息中可能的 IP/端口模式（出现即替换为 *）。
func sanitizeNetLogMessage(msg string) string {
	msg = netLogIPv4Re.ReplaceAllString(msg, "*")
	msg = netLogHostPort.ReplaceAllString(msg, "*:*")
	msg = netLogAnyPort.ReplaceAllString(msg, ":*")
	return msg
}

// netLog 记录网络层日志并推送 "net:log" 事件。
// 出口消敏：消息经 sanitizeNetLogMessage 处理，确保不再含 IP/端口/地址字面量。
func (a *App) netLog(level, msg string) {
	clean := sanitizeNetLogMessage(msg)
	log.Printf("[%s] %s", level, clean)
	a.emit("net:log", map[string]any{
		"level": level,
		"msg":   clean,
		"ts":    time.Now().UnixMilli(),
	})
}

// ---- 内部状态辅助 ----

func (a *App) baseURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.settings.ServerURL == "" {
		return defaultServerURL
	}
	return a.settings.ServerURL
}

func (a *App) snapshotRoom() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.room == nil {
		return nil
	}
	return copyMap(a.room)
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// setState 更新会话状态字段。
func (a *App) setState(state, code, token string, isHost bool, room map[string]any, s *session) {
	a.mu.Lock()
	a.state = state
	a.code = code
	a.token = token
	a.isHost = isHost
	a.room = room
	a.session = s
	a.mu.Unlock()
}

// startSession 启动后台会话 goroutine 并广播状态。
// 房间元数据必须含 clientType / gameVersion / loader（缺则填空字符串）：
// - clientType：join 由 /room/join 返回、create 由本端写 "app"。
// - gameVersion/loader：同上，join 响应可能不带，前端会按 "未指定" 兜底显示。
func (a *App) startSession(code, token string, isHost bool, room map[string]any, clientID string) {
	state := "in_room"
	if isHost {
		state = "hosting"
	}
	if room == nil {
		room = map[string]any{}
	}
	if _, ok := room["clientType"]; !ok {
		room["clientType"] = ""
	}
	if _, ok := room["gameVersion"]; !ok {
		room["gameVersion"] = ""
	}
	if _, ok := room["loader"]; !ok {
		room["loader"] = ""
	}
	s := newSession(a, code, token, isHost)
	a.setState(state, code, token, isHost, room, s)
	a.engine.bindSession(s, clientID)
	a.emit("session:state", map[string]any{
		"state": state,
		"room":  copyMap(room),
	})
	go s.run()
}

// leaveInternal 停止当前会话并尽力通知服务器离开。幂等。
func (a *App) leaveInternal() {
	a.mu.Lock()
	s := a.session
	code, token, isHost := a.code, a.token, a.isHost
	a.mu.Unlock()

	if s != nil {
		a.engine.relayRelease()
		a.engine.unbindSession(s)
		s.stop()
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
			a.netLog("warn", "等待会话循环退出超时，强制继续")
		}
		// 尽力通知服务器（失败忽略：房间会因心跳超时自动回收）
		_ = a.api.post(a.baseURL(), "/room/leave", map[string]any{
			"code":   code,
			"token":  token,
			"isHost": isHost,
		}, nil)
	}
	a.setState("idle", "", "", false, nil, nil)
}

// clearSession 由会话 goroutine 在结束时调用，重置 App 状态（仅当仍是本会话）。
func (a *App) clearSession(s *session) {
	a.mu.Lock()
	if a.session == s {
		a.session = nil
		a.state = "idle"
		a.code = ""
		a.token = ""
		a.isHost = false
		a.room = nil
	}
	a.mu.Unlock()
}

// updateRoomInfo 心跳响应携带的房间信息回写（有变化才广播）。
func (a *App) updateRoomInfo(s *session, currentPlayers int, name string) {
	a.mu.Lock()
	if a.session != s {
		a.mu.Unlock()
		return
	}
	changed := false
	if currentPlayers > 0 && a.room != nil && a.room["currentPlayers"] != currentPlayers {
		a.room["currentPlayers"] = currentPlayers
		changed = true
	}
	if name != "" && a.room != nil && a.room["name"] != name {
		a.room["name"] = name
		changed = true
	}
	state := a.state
	var room map[string]any
	if a.room != nil {
		room = copyMap(a.room)
	}
	a.mu.Unlock()

	if changed {
		a.emit("session:state", map[string]any{"state": state, "room": room})
	}
}

// ---- JSON 结果包装 ----

func wrapOK(data any) string {
	b, err := json.Marshal(map[string]any{"ok": true, "data": data})
	if err != nil {
		return `{"ok":false,"error":"INTERNAL","message":"结果序列化失败"}`
	}
	return string(b)
}

func wrapErr(code, msg string) string {
	b, err := json.Marshal(map[string]any{"ok": false, "error": code, "message": msg})
	if err != nil {
		return `{"ok":false,"error":"INTERNAL","message":"错误序列化失败"}`
	}
	return string(b)
}

// wrapCallErr 把底层错误转为统一格式：服务器错误透传，其余归为 NETWORK。
func wrapCallErr(err error) string {
	if ae, ok := err.(*APIError); ok {
		return wrapErr(ae.Code, ae.Message)
	}
	return wrapErr("NETWORK", err.Error())
}

// parseParams 解析前端传来的 JSON 字符串参数。
func parseParams(jsonStr string, out any) error {
	if jsonStr == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(jsonStr), out); err != nil {
		return fmt.Errorf("参数解析失败: %w", err)
	}
	return nil
}

// ==================== 绑定方法（前端契约，勿改签名） ====================

// GetSettings 返回 {serverUrl, theme, version}。serverUrl 恒为默认（应用内不可改）。
func (a *App) GetSettings() string {
	a.mu.Lock()
	theme := a.settings.Theme
	a.mu.Unlock()
	return wrapOK(map[string]any{
		"serverUrl": defaultServerURL,
		"theme":     theme,
		"version":   appVersion,
	})
}

// SaveSettings 保存 {theme?}（参数里的 serverUrl 一律忽略）并返回同 GetSettings。
func (a *App) SaveSettings(settingsJSON string) string {
	var req struct {
		ServerURL string `json:"serverUrl"` // 忽略（收到也不应用）
		Theme     string `json:"theme"`
	}
	if err := parseParams(settingsJSON, &req); err != nil {
		return wrapErr("INVALID_PARAMS", err.Error())
	}

	a.mu.Lock()
	if req.Theme != "" {
		a.settings.Theme = req.Theme
	}
	a.mu.Unlock()

	if err := a.saveSettings(); err != nil {
		return wrapErr("IO", err.Error())
	}
	return a.GetSettings()
}

// GetCategories 返回分类 map（键值如 survival->生存）。
func (a *App) GetCategories() string {
	var data map[string]any
	if err := a.api.get(a.baseURL(), "/categories", nil, &data); err != nil {
		return wrapCallErr(err)
	}
	return wrapOK(data)
}

// ListRooms 参数 {page,size,category,loader,search}；search 服务端不支持时本地过滤。
func (a *App) ListRooms(queryJSON string) string {
	var req struct {
		Page     int    `json:"page"`
		Size     int    `json:"size"`
		Category string `json:"category"`
		Loader   string `json:"loader"`
		Search   string `json:"search"`
	}
	if err := parseParams(queryJSON, &req); err != nil {
		return wrapErr("INVALID_PARAMS", err.Error())
	}
	if req.Page <= 0 {
		req.Page = 1
	}
	if req.Size <= 0 {
		req.Size = 20
	}

	query := url.Values{
		"page":       {strconv.Itoa(req.Page)},
		"size":       {strconv.Itoa(req.Size)},
		"clientType": {"mod"},
	}
	if req.Category != "" {
		query.Set("category", req.Category)
	}
	if req.Loader != "" {
		query.Set("loader", req.Loader)
	}

	var data struct {
		Rooms []map[string]any `json:"rooms"`
		Total int              `json:"total"`
		Page  int              `json:"page"`
		Size  int              `json:"size"`
	}
	if err := a.api.get(a.baseURL(), "/room/list", query, &data); err != nil {
		return wrapCallErr(err)
	}

	if req.Search != "" {
		data.Rooms = filterRooms(data.Rooms, req.Search)
		data.Total = len(data.Rooms)
	}
	return wrapOK(map[string]any{
		"rooms": data.Rooms,
		"total": data.Total,
		"page":  data.Page,
		"size":  data.Size,
	})
}

// filterRooms 按房间名本地模糊过滤（不区分大小写）。
func filterRooms(rooms []map[string]any, search string) []map[string]any {
	needle := strings.ToLower(search)
	out := make([]map[string]any, 0, len(rooms))
	for _, r := range rooms {
		name, _ := r["name"].(string)
		if strings.Contains(strings.ToLower(name), needle) {
			out = append(out, r)
		}
	}
	return out
}

// RoomInfo 查询房间公开信息。
func (a *App) RoomInfo(code string) string {
	if code == "" {
		return wrapErr("INVALID_PARAMS", "房间码不能为空")
	}
	var data map[string]any
	if err := a.api.get(a.baseURL(), "/room/info", url.Values{"code": {code}}, &data); err != nil {
		return wrapCallErr(err)
	}
	return wrapOK(data)
}

// CreateRoom 创建房间并自动开启会话。
// 契约：参数 {name,password,category,visible,hostPort,loader,gameVersion}（无 maxPlayers/authType）。
// hostPort 必填（1024-65535）；create body 的 maxPlayers 固定 20、不发 authType；
// loader/gameVersion 用传入值（空则 "unknown"）；natType 用探测结果（拿不到 "unknown"），
// clientCapabilities 见 appCapabilities（relay/ice_restart/continuous_retry/punchAuthV1/stdTurnV1）。
//
// 流程：参数校验 → 会话互斥检查 → 端口探测 → STUN → 调用 /room/create → 启会话。
// 端口探测失败不再调用 create API，直接返回 PORT_NOT_LISTENING。
func (a *App) CreateRoom(reqJSON string) string {
	var req struct {
		Name        string `json:"name"`
		Password    string `json:"password"`
		Category    string `json:"category"`
		Visible     *bool  `json:"visible"`
		HostPort    int    `json:"hostPort"`
		Loader      string `json:"loader"`
		GameVersion string `json:"gameVersion"`
	}
	if err := parseParams(reqJSON, &req); err != nil {
		return wrapErr("INVALID_PARAMS", err.Error())
	}
	if strings.TrimSpace(req.Name) == "" {
		return wrapErr("INVALID_PARAMS", "房间名不能为空")
	}
	if req.HostPort < 1024 || req.HostPort > 65535 {
		return wrapErr("INVALID_PARAMS", "hostPort 必须在 1024-65535 之间")
	}
	loader := strings.TrimSpace(req.Loader)
	if loader == "" {
		loader = "unknown"
	}
	gameVersion := strings.TrimSpace(req.GameVersion)
	if gameVersion == "" {
		gameVersion = "unknown"
	}
	visible := true
	if req.Visible != nil {
		visible = *req.Visible
	}

	// 会话互斥：hosting/in_room 时拒绝再建房（不再自动 LeaveRoom）。
	if msg := requireIdleSession(a); msg != "" {
		return msg
	}

	// 端口探测：本地必须有人在监听 hostPort（MC / 服务端）。
	if err := probeHostPort(req.HostPort); err != nil {
		return wrapErr("PORT_NOT_LISTENING",
			fmt.Sprintf("端口 %d 没有服务在监听，请先在 MC 开好局域网或启动服务端", req.HostPort))
	}

	// natType 用 STUN 探测结果（拿不到 "unknown"）
	natType := quickNatType()

	body := map[string]any{
		"name":                  req.Name,
		"maxPlayers":            20,
		"hostPort":              req.HostPort,
		"natType":               natType,
		"visible":               visible,
		"category":              req.Category,
		"gameVersion":           gameVersion,
		"loader":                loader,
		"clientType":            "app",
		"clientProtocolVersion": 7,
		"clientCapabilities":    appCapabilities,
	}
	if req.Password != "" {
		body["password"] = req.Password
	}

	var data struct {
		Code      string `json:"code"`
		HostToken string `json:"hostToken"`
		Name      string `json:"name"`
		HostIP    string `json:"hostIp"`
		HostPort  int    `json:"hostPort"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if err := a.api.post(a.baseURL(), "/room/create", body, &data); err != nil {
		return wrapCallErr(err)
	}

	room := map[string]any{
		"code":           data.Code,
		"name":           data.Name,
		"hostIp":         data.HostIP,
		"hostPort":       data.HostPort,
		"maxPlayers":     20,
		"currentPlayers": 1,
		"hasPassword":    req.Password != "",
		"category":       req.Category,
		"gameVersion":    gameVersion,
		"loader":         loader,
		"clientType":     "app",
		"expiresIn":      data.ExpiresIn,
		"isHost":         true,
	}
	a.startSession(data.Code, data.HostToken, true, room, "")

	return wrapOK(map[string]any{
		"code":      data.Code,
		"hostToken": data.HostToken,
		"name":      data.Name,
		"hostIp":    data.HostIP,
		"hostPort":  data.HostPort,
		"expiresIn": data.ExpiresIn,
	})
}

// requireIdleSession 会话互斥检查：hosting/in_room 时返回 SESSION_ACTIVE 错误串。
// CreateRoom 与 JoinRoom 共用（建房中再建房/再加入是事故源，删除原来的自动 LeaveRoom 幂等）。
func requireIdleSession(a *App) string {
	a.mu.Lock()
	state := a.state
	a.mu.Unlock()
	if state == "hosting" || state == "in_room" {
		return wrapErr("SESSION_ACTIVE", "请先退出当前房间再操作")
	}
	return ""
}

// JoinRoom 加入房间并自动开启会话（body 带与 create 相同的 caps 组）。
// 流程：参数校验 → 会话互斥 → 调用 /room/join → 启会话。
// 不再做自动 LeaveRoom 幂等：建房中再加入是事故源，统一走 SESSION_ACTIVE。
func (a *App) JoinRoom(reqJSON string) string {
	var req struct {
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if err := parseParams(reqJSON, &req); err != nil {
		return wrapErr("INVALID_PARAMS", err.Error())
	}
	req.Code = strings.ToUpper(strings.TrimSpace(req.Code))
	if !validateRoomCode(req.Code) {
		return wrapErr("INVALID_PARAMS", "房间码必须为 6 位（字符集 A-Z 去掉 I/O、2-9 去掉 0/1）")
	}

	// 会话互斥：hosting/in_room 时拒绝加入。
	if msg := requireIdleSession(a); msg != "" {
		return msg
	}

	body := map[string]any{
		"code":                  req.Code,
		"clientType":            "app",
		"clientProtocolVersion": 7,
		"clientCapabilities":    appCapabilities,
	}
	if req.Password != "" {
		body["password"] = req.Password
	}

	var data struct {
		ClientToken string         `json:"clientToken"`
		ClientID    string         `json:"clientId"`
		Room        map[string]any `json:"room"`
	}
	if err := a.api.post(a.baseURL(), "/room/join", body, &data); err != nil {
		return wrapCallErr(err)
	}

	room := data.Room
	if room == nil {
		room = map[string]any{}
	}
	room["code"] = req.Code
	room["isHost"] = false
	// join 响应可能不带 clientType/gameVersion/loader，标准化透传字段（"app" 一律由本端填）。
	if _, ok := room["clientType"]; !ok || room["clientType"] == "" {
		room["clientType"] = "app"
	}
	a.startSession(req.Code, data.ClientToken, false, room, data.ClientID)

	return wrapOK(map[string]any{
		"clientToken": data.ClientToken,
		"clientId":    data.ClientID,
		"room":        room,
	})
}

// LeaveRoom 停止会话并尽力通知服务器离开。
func (a *App) LeaveRoom() string {
	a.mu.Lock()
	had := a.session != nil
	a.mu.Unlock()
	if !had {
		return wrapOK(map[string]any{"left": false})
	}
	a.leaveInternal()
	a.emit("session:state", map[string]any{"state": "idle"})
	return wrapOK(map[string]any{"left": true})
}

// GetSessionState 返回当前会话状态。
func (a *App) GetSessionState() string {
	a.mu.Lock()
	state := a.state
	var room any
	if state != "idle" && a.room != nil {
		room = copyMap(a.room)
	}
	a.mu.Unlock()
	return wrapOK(map[string]any{"state": state, "room": room})
}

// SendRoomUpdate host 修改房间信息（name/category/visible/password）。
// 契约：收到 maxPlayers/authType 字段直接忽略（不再透传到服务端）。
func (a *App) SendRoomUpdate(reqJSON string) string {
	var req map[string]any
	if err := parseParams(reqJSON, &req); err != nil {
		return wrapErr("INVALID_PARAMS", err.Error())
	}

	a.mu.Lock()
	code, token := a.code, a.token
	hosting := a.state == "hosting" && a.isHost
	a.mu.Unlock()

	if req["code"] != nil {
		code, _ = req["code"].(string)
	}
	if req["token"] != nil {
		token, _ = req["token"].(string)
	}
	if code == "" || token == "" {
		return wrapErr("NO_SESSION", "当前没有房间会话")
	}

	body := map[string]any{}
	// 仅接受 name/category/visible/password；maxPlayers/authType 一律忽略
	for _, k := range []string{"name", "category", "visible", "password"} {
		if v, ok := req[k]; ok {
			body[k] = v
		}
	}
	body["code"] = code
	body["token"] = token
	body["isHost"] = hosting

	if err := a.api.post(a.baseURL(), "/room/update", body, nil); err != nil {
		return wrapCallErr(err)
	}
	return wrapOK(map[string]any{})
}

// GetRoomMods 查询房间模组支持情况。
func (a *App) GetRoomMods(code string) string {
	if code == "" {
		return wrapErr("INVALID_PARAMS", "房间码不能为空")
	}
	var data map[string]any
	if err := a.api.post(a.baseURL(), "/room/mods", map[string]any{"code": code}, &data); err != nil {
		return wrapCallErr(err)
	}
	return wrapOK(data)
}

// RelayStatus 查询中继是否启用。
func (a *App) RelayStatus() string {
	var data map[string]any
	if err := a.api.get(a.baseURL(), "/relay/status", nil, &data); err != nil {
		return wrapCallErr(err)
	}
	return wrapOK(data)
}

// RelayList 查询中继节点列表。
func (a *App) RelayList() string {
	var data map[string]any
	if err := a.api.get(a.baseURL(), "/relay/list", nil, &data); err != nil {
		return wrapCallErr(err)
	}
	return wrapOK(data)
}

// GetStun 获取本机公网地址。
func (a *App) GetStun() string {
	var data map[string]any
	if err := a.api.get(a.baseURL(), "/stun", nil, &data); err != nil {
		return wrapCallErr(err)
	}
	return wrapOK(data)
}

// OpenInBrowser 用系统默认浏览器打开 http/https 链接。
func (a *App) OpenInBrowser(rawURL string) {
	u := strings.TrimSpace(rawURL)
	if !validateServerURL(u) {
		a.netLog("warn", "拒绝打开非 http/https 链接: "+rawURL)
		return
	}
	// rundll32 参数中不能直接带未转义的 URL 特殊字符问题不大，但用 cmd /c start 会吃掉 & 等字符，
	// 因此选择 rundll32 url.dll,FileProtocolHandler。
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	if err := cmd.Start(); err != nil {
		a.netLog("error", "打开浏览器失败: "+err.Error())
		return
	}
	// Start 之后必须 Wait 回收，否则每开一次浏览器就留一个未回收的子进程句柄。
	// rundll32 会立刻把链接交给 shell 后退出，这里异步回收不阻塞界面。
	go func() { _ = cmd.Wait() }()
}

// ==================== 连接引擎绑定方法 ====================

// DetectMcPorts 探测本机 TCP LISTENING 端口及进程名，java* 进程优先。
// 返回 {ok,data:{ports:[{port,pid,name}]}}。
func (a *App) DetectMcPorts() string {
	ports, err := detectMcPorts()
	if err != nil {
		return wrapErr("DETECT_FAILED", err.Error())
	}
	return wrapOK(map[string]any{"ports": ports})
}

// TryDirect 直连探测（TCP hostIp:hostPort，3 次尝试）。结果经 conn:state 事件推送。
func (a *App) TryDirect() string {
	if err := a.engine.tryDirect(); err != nil {
		return wrapErr("NO_SESSION", err.Error())
	}
	return wrapOK(map[string]any{})
}

// UsePlayerRelay 请求玩家中继。结果经 conn:state 事件推送。
func (a *App) UsePlayerRelay() string {
	if err := a.engine.usePlayerRelay(); err != nil {
		return wrapErr("INVALID_STATE", err.Error())
	}
	return wrapOK(map[string]any{})
}

// UseTurnRelay 请求 TURN 中继（节点中继，自定义协议 v1 / 标准 RFC 5766 自动协商）。
// 结果经 conn:state 事件推送（phase=turn）。仅房客可用。
func (a *App) UseTurnRelay() string {
	if err := a.engine.useTurnRelay(); err != nil {
		return wrapErr("INVALID_STATE", err.Error())
	}
	return wrapOK(map[string]any{})
}
