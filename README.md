# VoxLink Desktop（应用端）

**产品定位**：VoxLink 应用端 = 模组的独立分身，专为模组不支持的版本（乃至其他游戏）联机准备，完全独立于 mod 运行；但联机互通双向兼容（mod 房 ↔ 应用端房，打洞/rudp 与模组逐字节互通）。玩家加入靠在 MC 手动填地址：隧道成功给 `127.0.0.1:<端口>`（本地映射），直连成功给 `hostIp:<端口>`。

当前版本 **1.1.9-beta**。安全纪律：UI 与日志**永不展示**中继节点地址/任何公网 IP（连接日志仅简要中文状态，后端出口统一消敏正则过滤）；中继节点列表卡片已整体移除。

连接引擎与 mod 端 1.1.5→1.1.8 对齐：打洞引擎为模组同款（PunchProfile 8 档 / PunchTuner 自适应调参 / PortPredictor 线性回归+delta 融合预测 / NAT 分级策略矩阵 / 双易对称多 socket 对轰 / 硬对称全端口洗牌喷洒 / TCP 同时打开打洞 / punchAuthV1 报文认证），TURN 中继双栈（自定义协议 v1 + 标准 RFC 5766，含 BIND 3 轮×5 发重试、ROLE_CONFLICT 容忍、UDP 黑洞自动降级 TCP 承载、15s/240s 保活），链路死亡走 requestIceRestart 重新协商。终局口径与 mod 一致：轮次无上限，仅“整会话零收包且满 20 轮”或“PREDICTION_OFF 累计 50 次且零收包”判定直连失败，是否使用中继由玩家决定。

加入连接流程：join 成功即自动尝试 P2P 打洞；20s 后亮"尝试直连"与"使用 TURN 中继"，再 20s 后亮"使用玩家中继"；任一阶段失败后两个中继入口仍保留；任一路径打通后给出地址——隧道类是 `127.0.0.1:<随机端口>`（本地映射，玩家在 MC 里添加该服务器直进）、直连类是 `hostIp:hostPort`。

## 运行与打包

产物：`成品/App/VoxLinkApp.exe`（单文件，双击即用，无需安装）。

```bash
cd app-desktop
go build -trimpath -tags "desktop,production" -ldflags "-s -w -H windowsgui" -o "../成品/App/VoxLinkApp.exe" .
```

注意：必须带 `-tags desktop,production`，否则启动弹 "Wails applications will not be built without the correct build tags"。

## 结构

- `main.go` — Wails 入口，embed `frontend/dist`
- `app.go` — 绑定方法（全部 JSON 字符串进出，`{ok,data}` / `{ok:false,error,message}` 信封）
- `api.go` — HTTP 客户端（`{base}/?route=<route>`，生产基址 `https://p2p.wuhui.icu`，服务器地址不开放修改）
- `session.go` — 房间会话：心跳循环 + 信令循环（WS `wss://…/ws` 优先，断线回退 HTTP `/signal/poll` 1s）
- `conn_engine.go` / `punch.go` / `rudp.go` / `stun.go` / `bridge.go` / `relay.go` — 连接引擎：UDP 打洞（5 字节控制包、单向判赢、WSAECONNRESET 容忍、delta 端口预测）、可靠 UDP（11B 帧头/累计 ACK/FEC 组4+XOR/KEEPALIVE 回显，与模组 ReliableUdpTransport 逐字节互通）、STUN、guest 本地桥 + host lazy 桥、直连探测、玩家中继（relay_request/notify/setup/accept/ready）
- `settings.go` — `%APPDATA%/VoxLinkApp/settings.json`（serverUrl 固定 / theme）
- `frontend/dist/` — 免构建原生 HTML/CSS/JS；主题 token 全部 CSS 变量（`:root` 亮色，`html[data-theme="dark"]` 暗色），组件样式禁止裸色值；logo 用真图标 `assets/img/icon.png`
- `smoke_test.go` / `engine_test.go` — 单元测试 + 本机双端 rudp/桥集成测试 + live 连通性测试

## 事件

- `session:state` — `{state, room, message?}`（closed 时前端回大厅）
- `signal` — `{type, from, data, timestamp}`（join_request/connected/host_closing/…）
- `conn:state` — `{phase:"p2p"|"direct"|"prelay", status:"trying"|"failed"|"success"|"available", address?, detail?}`
- `net:log` — `{level, msg, ts}`（房间页连接日志面板）

## 未实现（与模组对齐的取舍）

- 打洞的生日攻击多 socket 组 / TCP SimOpen / reverse punch / punchAuthV1 认证帧（caps 不声明 punchAuthV1，全程走旧版明文字节；对称 NAT 极端场景由直连/玩家中继兜底）
- TURN 数据面（界面仍展示中继节点列表，但连接流程未接 TURN 隧道）
- **跨网互通未实测**：帧格式/信令字段与模组源码逐字节对照 + 单测 + 本机双端互跑已过，首次线上联调建议盯 `net:log` 的 p2p/rudp 日志
