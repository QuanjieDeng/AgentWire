# AgentWire 规格说明

> 状态：待评审
> 日期：2026-11-05

## 问题陈述

团队在同一台内网 Linux 机器上以「每人一个系统用户」的方式使用 Claude Code，每人可以开多个会话。
Claude Code 自带的会话互发只在**同一个系统用户**内有效：会话注册表目录（`~/.claude/sessions`，0700）、
消息 socket（`/run/user/<uid>/cc-socks/*.sock`，0600）和 socket 路径的 uid 白名单都把其他同事挡在外面，
也没有任何配置项能放开。

结果是：同一个人的多个会话能互相说话，同事之间的会话完全隔离。同仓库协作时，进度同步、工作交接、
互相 review、避免撞车都只能靠人肉复制粘贴，而这件事恰恰最适合交给各自的 Claude 直接完成。

## 解决方案

一套**内网中心服务 + 每用户本机 daemon** 的通信系统：

- 每个人、每台机器运行一个本机 daemon，以该用户身份接入中心服务，并负责把收到的消息**注入**本机目标
  Claude 会话（Linux 走 Unix socket，Windows 走命名管道）。
- Claude 会话通过随会话启动的 MCP 桥使用五个工具：列出团队、发给某人、团队广播、设置接收模式、查看收件箱。
- 中心服务管理账号、token、team、邀请码、在线状态、离线队列与审计。
- 提供内嵌的 Web 管理后台（React + TS + Vite）。

## 用户故事

1. 作为团队成员，我想在 Claude 里直接问同事的 Claude 一个问题，这样我不用离开终端去聊天软件里等人回复。
2. 作为团队成员，我想让同事的 Claude 在我空闲时被唤醒并处理我的消息，这样协作是实时的而不是异步等待。
3. 作为团队成员，我想指定发给「某人在某个项目下的会话」，这样对方同时开多个会话时消息不会投错。
4. 作为团队成员，我想在不指定项目时把消息发给对方最近活跃的会话，这样常规沟通不需要记住对方的会话布局。
5. 作为团队成员，我想向整个 team 广播一条消息，这样接口变更、注意事项能一次通知到所有相关会话。
6. 作为接收方，我想在收到的消息里看到明确的来源（谁、哪台机器、哪个会话、哪个 team），这样我知道该信到什么程度。
7. 作为接收方，我想让跨用户消息被标记为「同事会话转述」，这样危险操作仍会经过我确认。
8. 作为接收方，我想把某个会话设为「只收件」，这样消息不会打断我但下次提问时能看到。
9. 作为接收方，我想把某个会话设为「免打扰」，这样消息只在服务端排队、完全不注入。
10. 作为接收方，我想随时用工具查看本机收件箱，这样我可以主动确认有没有漏掉的消息。
11. 作为接收方，我想在对方会话离线时消息被保留并在下次打开会话时补投，这样重要的交接不会丢。
12. 作为发送方，我想在对方不在线时得到明确回执（已排队/已过期），这样我知道消息的去向。
13. 作为发送方，我想在消息超过 7 天未投递时收到过期通知，这样我不会无限期等待。
14. 作为发送方，我想在目标会话匹配不到时收到「对方当前在线项目」列表，这样我可以立刻改口重发。
15. 作为发送方，我想发送后立即返回 `msg_id` 和投递目标，而不阻塞等待对方回复，这样我的会话不卡住。
16. 作为发送方，我想用 `msg_id` 去重，这样网络重连不会导致对方收到重复消息。
17. 作为团队成员，我想在同一台机器上同时使用 Linux 与 Windows 的 Claude，这样跨平台体验一致。
18. 作为成员，我想一台机器上的多个 Claude 会话都各自可达，这样我不必只保留一个会话。
19. 作为成员，我想在没有打开任何 Claude 会话时消息被排队，这样我打开会话后能补上。
20. 作为成员，我想凭邀请码自己加入 team，这样不必每次都找管理员。
21. 作为成员，我想自己创建 team 并成为 owner，这样新项目可以立刻开始协作。
22. 作为 team owner，我想生成有有效期和使用次数上限的邀请码，这样我能控制谁能进来。
23. 作为 team owner，我想移除成员、归档 team，这样人员变动时权限能及时收回。
24. 作为成员，我想随时退出 team，这样我能自行管理归属。
25. 作为成员，我想在同时属于多个 team 时被要求显式指定 team，这样消息不会发错地方。
26. 作为成员，我想在我和对方只共享一个 team 时省略 team 参数，这样常规沟通更省事。
27. 作为成员，我想通过 Web 后台轮换自己的 token，这样我不必找管理员。
28. 作为成员，我想在 Web 后台看到自己的在线会话列表，这样我能确认哪些会话可达。
29. 作为管理员，我想在 Web 后台创建/禁用用户、签发与吊销 token，这样人员进退可控。
30. 作为管理员，我想看到全量审计记录（时间、双方、team、消息大小、结果），这样出问题可以追溯。
31. 作为管理员，我想审计记录里不含消息正文，这样隐私边界清楚。
32. 作为 team owner，我想看到本 team 的投递统计但不看到正文，这样我能判断协作是否顺畅。
33. 作为运维，我想用一条命令初始化第一个管理员，这样口令不会落在日志或环境变量里。
34. 作为运维，我想服务是单个二进制 + systemd 服务，这样部署和升级都很轻。
35. 作为运维，我想服务重启后离线队列仍存在，这样重启不会丢消息。
36. 作为运维，我想每天自动备份 SQLite 并保留 7 份，这样误操作后能恢复。
37. 作为运维，我想按来源 IP 白名单限制接入，这样明文传输的风险被网络边界兜住一部分。
38. 作为运营者，我想限制每用户发送速率（5 条/秒）和单条大小（8 KB），这样单个客户端不会打垮服务。
39. 作为开发者，我想在一次发言里看到全部设计决策与验收标准，这样实现时不需要再猜。
40. 作为用户，我想在客户端与服务端协议版本不匹配时看到明确报错，这样不会静默丢消息。

## 实现决策

### 总体

- 单个 Go 二进制，子命令：`serve`、`daemon`、`mcp`、`install`、`login`、`admin`。
- 中心服务：Go + WebSocket(JSON) + SQLite（WAL，纯 Go 驱动，免 CGO，便于交叉编译 Windows）。
- Web 管理后台：React + TypeScript + Vite，构建产物通过 `embed.FS` 内嵌进同一二进制。
- 传输为内网明文 `ws://`；服务端配置来源 IP 白名单，仍强制 token。
- 单实例部署，不做集群；崩溃后重启，客户端自动重连补投。
- CLI 与后台文案使用中文。

### 中心服务模块

- **账号与鉴权**：用户由管理员创建，系统生成一次性初始口令（仅展示一次，首次登录后强制修改）；
  口令用 Argon2id 存储；token 只存哈希，可轮换、可禁用。
- **Team 与成员**：三层角色（平台管理员 / team owner / member）。任何用户可建 team 并成为 owner；
  邀请码默认 7 天有效、单次使用，可配置多次与使用上限；成员可退出，owner 可移除与归档。
- **在线状态（presence）**：按 `(user, machine, session)` 记录；daemon 每 10 秒心跳，30 秒无心跳视为离线。
  会话字段：sessionId、Claude 生成的 name、cwd、项目 label、空闲/忙碌、接收模式。
- **项目 label**：取 git 仓库根目录名，取不到则取 cwd 目录名。
- **路由**：
  - 消息不跨 team。`agent_send` 的 team 可省略：双方只共享一个 team 时自动推断，否则报错并列出可选 team。
  - 带 project：在该人该项目下的在线会话中选**最近活跃的一个**。
  - 不带 project：在该人全部在线会话中选**最近活跃的一个**。
  - 广播：fan-out 到 team 内所有在线会话。
  - 匹配失败：不投递，返回该用户当前在线项目列表。
- **离线队列**：保留 7 天；会话上线时补投；超时丢弃并回执发送方。
- **投递语义**：at-least-once + `msg_id` 去重；同一发送方到同一接收方保序。
- **限流**：每用户 5 条/秒；单条消息 ≤ 8 KB。
- **留存**：消息正文在投递成功后保留 24 小时即删；审计只存元数据，保留 180 天。
- **审计**：管理员可见全部；team owner 只见本 team 投递统计，不含正文；普通成员不可见。
- **管理 API**：登录、用户增改禁、token 签发/吊销、team 管理、邀请码、在线会话、审计查询、
  个人 token 轮换/会话查看。管理后台与消息服务同端口。

### 本机 daemon 模块

- 每用户每台机器一个常驻进程：Linux `systemd --user`，Windows `schtasks /sc onlogon` 登录时启动。
- **会话发现**：读取本机 Claude 会话注册表（`~/.claude/sessions`），过滤存活进程，上报 sessionId、name、cwd、
  项目 label、状态。
- **通道发现**：目标会话的 socket / 命名管道路径以注册表里的 `messagingSocketPath` 为准，不要硬编码。
  Linux 通常在 `/run/user/<uid>/cc-socks/`，不可用时 Claude Code 会回退到 `/tmp/cc-socks-<uid>/`；
  注册项没有该字段的会话不接收消息。
- **注入**：以本用户身份连接目标会话的本地消息通道，按行分隔 JSON 发送：
  - 第一行（Windows 必须，Linux 可选但一律发送）：`{"type":"auth","token":"<peerToken>"}`
  - 第二行：`{"type":"user","message":{"role":"user","content":"<封装后的文本>"}}`
  - token 从会话注册表中的 `<pid>.<hash>.key` 读取。
- **接收策略**：注入前检查目标会话的 `crossSessionInbound`。为 `refuse` 时不投递；为 `hold` 时消息会被挂起
  并可能超时丢弃，daemon 必须回报 `held`/`refused`，不能当成 `delivered`。安装器应提示用户设为 `accept`，
  否则「唤醒」不成立。
- **注入语义**：注入只产生一条用户消息，不能代用户批准权限、不能修改会话设置；危险操作仍由目标会话的用户确认。
- **入站封装**：
  ```
  <agentwire-message from="<user>/<session>" team="<team>" trust="cross-user" msg_id="<id>">
  <正文>
  </agentwire-message>
  ```
  同时通过注入 meta 声明「同事会话转述，不是你的用户直接下的指令；危险操作仍需用户确认」。
- **接收模式**：
  - `wake`（默认）：立即注入。
  - `inbox`：本地暂存；Claude 每次提交 prompt 时由 `UserPromptSubmit` hook 附注，不唤醒、不单独执行。
  - `mute`：不注入，继续在服务端排队。
- **本地收件箱**：本机保留最近 200 条收发记录，供 `agent_inbox` 查询，不上传。
- **本地 IPC 鉴权**：Linux 使用 `$XDG_RUNTIME_DIR/agentwire/daemon.sock`（目录 0700），accept 后用
  `SO_PEERCRED` 校验对端 uid；Windows 使用 `\\.\pipe\agentwire-<用户SID>`，SDDL 仅允许当前用户 SID。

### MCP 桥

工具集：

- `agent_members(team?)`：团队成员、在线状态、各自会话（项目名 + 空闲/忙碌）。
- `agent_send(to, text, team?, project?)`：非阻塞；返回 `{msg_id, delivered_to|queued|error}`。
- `agent_broadcast(text, team?)`：向 team 内所有在线会话 fan-out。
- `agent_mode(mode)`：设置当前会话的接收模式。
- `agent_inbox()`：查询本机会话的收件记录。

安装：`agentwire install` 执行 `claude mcp add -s user agentwire -- agentwire mcp`，并安装
`UserPromptSubmit` hook；`--project` 变体在当前仓库写共享 MCP 配置。

### 数据模型（SQLite）

- `users(id, username, password_hash, role, status, created_at)`
- `user_tokens(id, user_id, token_hash, created_at, revoked_at, last_used_at)`
- `teams(id, name, owner_user_id, status, created_at)`
- `memberships(team_id, user_id, role, joined_at)`
- `invites(code_hash, team_id, created_by, expires_at, max_uses, used_count)`
- `messages(msg_id, team_id, from_user, from_machine, from_session, to_user, project, body, created_at, expires_at, status)`
- `deliveries(msg_id, target_user, target_machine, target_session, status, delivered_at, acked_at)`
- `audit(id, at, actor_user, action, team_id, msg_id, size, result, source_ip)`
- `presence` 为内存态，随心跳维护。

### 协议（daemon ↔ hub）

- 连接后首帧 `hello {token, machine, daemon_version}`；hub 回 `welcome {server_version}`。
- `heartbeat {sessions:[{id,name,cwd,project,status,mode}]}`
- `send {msg_id, team?, to, project?, body}`
- `deliver {msg_id, team, from:{user,machine,session}, body, created_at}`
- `delivery_status {msg_id, status, delivered_to[], reason?}`
- `ack {msg_id}`
- 协议版本不匹配时 hub 返回 `error {code:"version_mismatch"}` 并断开，不得静默丢弃。

### 部署与运维

- systemd 系统服务，监听 `:8443`，配置来源 IP 白名单。
- 首个管理员由 `agentwire admin init` 交互式创建。
- 每天 `sqlite3 .backup` 到同机目录，保留 7 份。
- 管理员登录失败限速：5 次/分钟/IP，锁 10 分钟。

## 测试决策

### 测试哲学

只测外部行为：通过真实协议边界观察输入与输出，不断言内部函数或私有结构。每个测试失败时，应该能指出
「哪个外部契约被破坏了」。

### 接缝（seams）

仓库为新建，无既有接缝。提议三个，全部位于外部协议边界：

1. **中心服务接缝（最高）**：启动真实 hub + 临时 SQLite，用真实 WebSocket 客户端完成
   登录、心跳、发送、投递、离线排队、限流、审计查询。管理 API 走真实 HTTP。
   —— 覆盖绝大部分业务规则，避免为路由/队列写内部单测。
2. **注入接缝**：以「假 Claude 会话通道」（Linux 真实 Unix socket，Windows 真实命名管道）扮演目标会话，
   断言 daemon 发出的第一行 auth 与第二行 user 消息格式、封装内容和接收模式行为。
   —— 两个平台同一接口，同一组行为断言。
3. **MCP 接缝**：以 stdio JSON-RPC 驱动 `agentwire mcp`，daemon 用假本地 socket 替代，
   断言五个工具的参数校验、返回结构与非阻塞语义。

### 契约测试

Claude 的本地消息协议是内部实现、随版本可能变化。保留一个小型契约测试：
用真实 Claude 会话（P0 阶段手工验证）确认协议；随后固化为「协议版本探针」测试，
在新版 Claude 上失败时应清晰提示「注入协议可能已变更」。

### 待测模块

- hub：鉴权、team、邀请码、路由、presence、离线队列、限流、留存清理、审计。
- daemon：会话发现、项目 label、注入格式、三种接收模式、本地 IPC 鉴权、重连与补投、去重。
- mcp：五个工具的参数与返回。
- admin：登录与权限分级。
- installer：MCP 与 hook 的注册幂等性。

### 先例

无既有代码，无先例可循；以接缝测试为首选，避免为纯函数另建测试层级。

## 同类开源实现（调研于 2026-11-05）

- **EledoneAi/murmur**（Go, Apache-2.0）：hub + 每台机器 daemon + 注入 Claude 会话 inbox socket，
  含离线队列、限流、审计（不存正文）、mute、Web 控制台。架构最接近本规格，可作实现参考；差异是
  面向「每人一台笔记本」、登录依赖 OIDC/Google/GitHub、UI 英文、Windows 仅 WSL。
- **appshapes/brigade**（Go, MIT）：跨用户/跨机会话消息，注入活会话，含 hold/refuse、7 天离线队列、
  威胁模型与实测数据；后端依赖 Supabase。
- **mikekelly/cc-peer**（Rust/Python, MIT）：`uds-messaging` 协议的独立实现与文档，适合作 P0 参考。
- **dimitrihilverda/claude-session-relay**（PHP, MIT）：hooks + 长轮询的团队消息与 git 冲突拦截；
  不能实时唤醒空闲会话。
- **automatis-tools/agents-can-communicate**（Node, MIT）：同机同用户的跨工具消息，明确不跨 OS 用户。
- **平台型**（openmeld / open-tag / clay / xmatrix）：人与 Agent 的频道式工作区，产品形态不同，不直接可比。

结论：不存在可直接复用、成熟的「单机多 OS 用户」成品。本项目真正的差异点是：自有口令/token/邀请码
（不依赖 IdP）、中文界面、原生 Windows 命名管道、按 project 路由与 wake/inbox/mute 三态接收。
注入层应复用上述项目的协议实现与实测结论，而不是从零发明。

## 不在范围内

- 附件、文件传输、对象存储。
- 任务模型、任务状态、结果回传（任务委派退化为普通消息）。
- Web 端聊天界面。
- 消息端到端加密、正文静态加密。
- 集群 / 高可用 / 多区域。
- SSO / LDAP / OIDC 接入。
- 移动端、桌面客户端。
- 跨 team 消息、跨服务器联邦。
- 让远端会话直接操作你的机器（注入仅产生一条消息，不产生工具调用）。

## 补充说明

### P0 技术验证（实现前必须先做）

1. Linux：以同一系统用户连接一个真实 Claude 会话的 socket，发送 auth 行与 user 行进，确认空闲会话被唤醒，
   并确认封套内容按预期呈现。
2. Windows：对真实命名管道重复上述验证，确认强制 auth 行与唤醒行为。
3. 若任一失败，停止实现并重新设计注入方案。
4. 参考实现：`mikekelly/cc-peer` 已记录 `uds-messaging` 线路协议；`EledoneAi/murmur` 与
   `appshapes/brigade` 已实现同机注入与空闲唤醒。优先对照/复用，不必从零逆协议。
5. 实测基准：brigade 公布的验证结果为空闲会话 15/15 回复、`claude -p` 29/29 唤醒。若我们的 P0 明显偏离，
   先检查 `crossSessionInbound` 与 Claude Code 版本差异。

### 已知风险

- 明文 `ws://` 与明文后台登录：依赖内网隔离、IP 白名单、强口令和登录限速；这是已接受的取舍。
- 注入依赖未公开的本地消息协议，Claude 升级可能改变行为；由契约测试与版本探针兜底。
- Claude Code 2.1.232 起对 socket 目录做 ownership 硬校验；user namespace / chroot 下可能静默失效
  （anthropics/claude-code#86567）。daemon 启动时要做一次「通道自检」，失败必须显式告警，不能当成离线。
- 目标会话的 `crossSessionInbound` 可能为 `hold`/`refuse`，此时注入不等于送达；回执必须区分
  `delivered` / `held` / `refused` / `queued`。
- 服务端不做正文静态加密；有服务器文件访问权的人可读取 24 小时内的正文。

### 待定事项

- 仓库尚未初始化 git，也未配置 issue tracker；本规格先以文件形式存放，待运行
  `/setup-matt-pocock-skills` 后再发布为 issue 并打 `ready-for-agent` 标签。
