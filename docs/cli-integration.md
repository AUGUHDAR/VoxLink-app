# VoxLink CLI 调用说明

> 面向启动器与脚本作者。CLI 版与 UI 版是同一套联机引擎，功能一致，只是没有窗口。
> 目标是**尽量向后兼容**：命令与字段以本文与 `describe --json` 为准，我们会避免破坏性改动；
> 若确有需要调整，会在同一版本线上保留兼容路径，并在发版说明里标注。

## 基本形态

```
voxlink-cli <域> <动作> [参数]        # 一次性命令，stdout 输出一行 JSON 后退出
voxlink-cli <域> <动作> --events ndjson  # 长任务：先一行结果，随后逐行推送事件流
voxlink-cli describe --json          # 机器可读：命令表 + 协议号 + 能力清单 + 常量
voxlink-cli --version                # 版本与协议号
```

## 统一信封

除事件流外，所有输出都是一行 JSON：

```json
{"ok":true,"data":{…}}
{"ok":false,"error":"ROOM_NOT_FOUND","code":"ROOM_NOT_FOUND","message":"房号不存在或密码不对"}
```

`code` 是机器可读错误码（与 VoxLink 服务端同源），启动器按码分支即可，`message` 是按 `--lang` 渲染的人话，不要拿去匹配。

## 事件流

`--events ndjson` 时，每行一个事件对象：

```json
{"event":"state","phase":"p2p","status":"trying","ts":…}
{"event":"state","phase":"relay","status":"success","address":"127.0.0.1:51234","ts":…}
{"event":"log","level":"info","msg":"…","ts":…}
```

`phase` 取 `p2p` / `direct` / `relay`，`status` 取 `trying` / `success` / `failed`。拿到 `address` 就写进 MC 服务器地址。

## 典型集成流程

```
voxlink-cli room create --name "我的服" --port 25565 --events ndjson
voxlink-cli room join --code AB12CD --events ndjson
voxlink-cli room leave
```

日志、工单、中继开关等其余命令请用 `describe --json` 枚举——那一份是实时从程序里出来的，永远比文档准。
