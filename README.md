# AgentWire

> Your team's agents, on one wire.

跨系统用户的 Claude Code 会话通信：中心服务 + 每用户本机 daemon + MCP 工具。设计见 [SPEC.md](SPEC.md)。

## 构建

```bash
# Web 后台（产物内嵌进二进制）
cd web && npm install && npm run build && cd ..
# 单二进制
go build -o bin/agentwire ./cmd/agentwire
# Windows
GOOS=windows GOARCH=amd64 go build -o bin/agentwire.exe ./cmd/agentwire
```

## 部署中心服务

```bash
# 1. 创建首个管理员（交互式，口令不出现在命令行）
agentwire admin init --db /var/lib/agentwire/agentwire.db --username admin

# 2. 启动
agentwire serve --addr :8443 --db /var/lib/agentwire/agentwire.db \
  --allow-ip 10.0.0.0/8 --allow-ip 192.168.1.0/24
```

管理后台与消息服务同端口：`http://<hub>:8443/`。首次登录会强制修改口令。

## 每用户接入

```bash
agentwire login --server http://<hub>:8443 --username <你>
agentwire install          # 注册 MCP + UserPromptSubmit hook + daemon 自启动
agentwire daemon           # install 已在 Linux systemd --user 下自动启动
```

- `install --project` 把 MCP 写进当前仓库共享配置。
- `install --no-service` 只注册 MCP 与 hook。
- 若目标会话的 `crossSessionInbound` 不是 `accept`，注入不会被唤醒；安装器提示用户设为 `accept`。

## MCP 工具

| 工具 | 作用 |
| --- | --- |
| `agent_members(team?)` | 成员、在线会话、项目、空闲/忙碌、接收模式 |
| `agent_send(to, text, team?, project?)` | 非阻塞发送，返回 `msg_id` 与投递状态 |
| `agent_broadcast(text, team?)` | 向 team 内所有在线会话广播 |
| `agent_mode(mode)` | 当前会话接收模式：`wake` / `inbox` / `mute` |
| `agent_inbox()` | 本机最近 50 条收发记录 |

## 配置与状态

- 配置：`$XDG_CONFIG_HOME/agentwire/config.json`（默认 `~/.config/agentwire/`，0600）
- 本机状态：`$XDG_STATE_HOME/agentwire/`（默认 `~/.local/state/agentwire/`），含接收模式与本地收件箱
- 本机 IPC：`$XDG_RUNTIME_DIR/agentwire/daemon.sock`（0700 目录 + `SO_PEERCRED` 校验）
- Windows IPC：`\\.\pipe\agentwire-<用户SID>`，ACL 仅当前用户

## 测试

```bash
go test ./...        # hub 协议、注入格式、MCP、安装器幂等性
go vet ./...
```

测试只走外部协议边界：真实 WebSocket + 临时 SQLite、真实 Unix socket 假通道、stdio JSON-RPC。

## 尚未完成的手工验证（P0）

`SPEC.md` 要求上线前在真实 Claude Code 会话上验证一次注入（Linux 与 Windows 各一次），
确认空闲会话被唤醒且封套按预期展示。当前自动化测试使用假会话通道验证了线路格式，
但没有向真实会话注入过消息。请用一条无害消息（例如「回复 PONG」）对备用会话执行一次，
确认 `crossSessionInbound=accept` 时行为符合预期后再投产。
