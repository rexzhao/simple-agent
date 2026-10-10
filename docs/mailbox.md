# Mailbox 客户端插件设计（sai ↔ mailboxd）

目标：sai 以 flag 启用 mailbox 客户端，接入既有 mailboxd 守护进程
（`F:\work\pi-mailbox\mailboxd`，WebSocket 协议 v2），使 sai 会话可以
收邮件。**这是 Go 进程侧的功能，与 web UI 无关**（页面不感知 mailbox）。

## 0. 决定（已定）

1. Go 代码侧插件，web UI 零涉及
2. 参考 pi-mailbox 的 TS 客户端实现（client/supervisor/daemon 解析/
   项目身份全部镜像）
3. 排队机制支持 **steer / followup**：run 空闲 → 新回合投递；run 活跃
   → 中途注入（steer）；settle 竞态窗口 → 落回 followup 重投
4. **不做自动回复**：后续加入 mailbox 工具（mailbox_list/read/send/
   reply/sessions，对齐 pi 的工具集）后由 agent 自行决定回复

## 1. 解耦架构（核心约束）

**agent 主逻辑零依赖 mailbox 代码**——不是少依赖，是 `internal/agent`
不 import 任何 mailbox 包。依赖方向单向：

```
internal/mailbox ──(公共 API)──▶ internal/agent
     ▲
cmd/sai ──(flag 装配)
```

插件与 agent 的全部交互走两个已存在的公共 API + 一个新增的**正交**
agent 能力：

| 交互 | API | 性质 |
|---|---|---|
| 空闲时投递邮件 | `Session.Send(text)`（现有） | user.input 记录，持久化/单飞/fold 免费 |
| 繁忙时注入 | `Session.Steer(text)`（新增，见 §3） | 正交的 mid-run 输入能力，与 mailbox 无关 |
| 确认投递/观察 run 结束 | `Session.Subscribe()`（现有） | 记录流观察 |

`internal/mailbox` 内部分层（镜像 pi-mailbox 的 src/）：

```
frames.go    最小 WS 帧编解码（client 掩码帧；test server 复用）
client.go    连接 + rid 关联 + 5s 请求超时 + notify 分发（≈ client.ts）
daemon.go    registry 解析 → ping/pong 探测（版本校验）→ 按需拉起
             mailboxd → 项目 UUID（复用 <cwd>/.pi/mailbox.json，
             与 pi 同目录互操作）（≈ daemon.ts）
plugin.go    连接监督：generation 计数、200–1000ms 错峰重连、
             (重)连后拉积压未读（≈ index.ts 的 MailboxConnection）
inject.go    notify → 排队 → Send/Steer/重投；投递确认；标记已读
```

零依赖纪律：WS 客户端**手写**（client 侧握手 + 掩码文本帧 +
ping/pong + close 约 200 行；消息小、纯文本、无分片）。测试用的
最小 WS server 复用 frames.go（server 帧不掩码）。

## 2. 协议对接（v2）

- 寻址：`~/.pi/mailboxd.json`（`$PI_MAILBOXD_DIR` 可覆盖）→
  `{port, instanceId, remote, pid}`；探测 `ping/pong` 校验
  `protocolVersion == 2`；daemon 不在 → 从 `$PI_MAILBOXD` 或 PATH
  拉起 `mailboxd`（分离进程），等 registry 出现（10s 上限）
- 注册：`Hello{project: <项目UUID>, projectRoot: cwd, session:
  <sai会话ID>, agent: "sai", meta: {_model: provider/model}}` →
  `Welcome`；版本不符 → 拒连报错
- 项目身份：读写 `<cwd>/.pi/mailbox.json` 的 projectId（UUID），
  mkdir-lock 序列化首次创建——**与 pi 会话同目录即同项目**，
  sessions 列表互通、可直接互发
- 保活：Ping 周期（镜像 pi 的节奏）；notify 推送 unread 计数
- 离线不丢：daemon 持久化邮件；(重)连后 `Inbox(unreadOnly)` 拉积压，
  按 NS 升序（最旧先投）
- 投递确认后 `Read` 标记已读；崩溃/未确认 → 邮件保持未读，重启重拉

## 3. Steer：正交的 agent 能力（已实现，与 mailbox 无关）

steer = "尽快进入会话的用户输入"，一个队列两个排干点（pi 的
steering + followUp 两队列坍缩为一个语义）：

- `Session.Steer(text) (ItemID, error)`：**入队即铸 ItemID**（身份先于
  持久化），非运行态返回 ErrNoRun（调用方回退 Send）。队列在瞬态区
  （`State.Steered`，Snapshot/snapshot 首帧可见），可经
  `UpdateSteer`/`WithdrawSteer` 编辑/撤回
- run 循环**每次迭代顶部**（建请求前）排干：每条 commit 一条
  `user.input` 记录（**同一 ItemID**）并追加进 `messages`——提交与
  消费原子，Fold 顺序 = 记录顺序，mid-run 压缩重建时两侧同步
- **最终响应分支**排干非空则续轮（= followup）；预算边缘
  （iteration == maxIterations）不排干、留队给下一个 run——**这是
  队列残留的主要正常路径**（每轮都 steer 的用户会把预算用尽，
  MaxIterations=N 至多吸收 N-1 次续轮）
- 残留条目不丢弃：下一个 run 的首个迭代顶部投递（顺序在该 run
  触发输入之后）。**RunID 归属**：记录带的是**消费它的 run** 的
  RunID，不是输入时刻的 run——按 RunID grep 时注意这个语义
- 排干提交的文本是**规范化后**的（与 Append 同一规范化），wire
  历史/折叠/磁盘不分叉

mailbox 通知注入直接用 Steer/Send，无需恰好一次协议：通知丢失无害
（下次 notify 重试），邮件本体由 agent 经工具拉取。

竞态窗口：settle 与 Steer 之间——`Steer` 返回运行态错误或入队后未
消费，都收敛到 followup 重投，幂等。

## 4. 注入格式（v1：内容注入）

```
[mailbox] from <project>/<session> re: <subject> (<mail-id>)
<body>
```

v1 没有 mailbox 工具，邮件**内容**直接注入是唯一通路；agent 的回答
落在会话里（操作者可见），不外发。P2 加入工具后切换为 pi 的通知式
注入（"[mailbox] You have N new mail(s); use mailbox_list"），由
agent 自行读信与回复——工具按连接状态激活/停用（对齐 pi）。

## 5. CLI

```
sai -mailbox                 # 本地模式：解析/拉起 daemon，注册当前会话
sai -mailbox-remote h:p      # 远程模式（固定地址 + token）
sai -mailbox-token <token>
```

环境变量与 pi 兼容：`PI_MAILBOXD`（二进制路径）、`PI_MAILBOXD_DIR`
（registry 目录）。REPL 与 serve 模式同样适用（插件只需要
`*agent.Session`）。REPL 模式下邮件触发的 run 与用户输入共享单飞槽
（会话固有语义，非插件引入）。

## 6. 验证

- **协议**：最小 WS test server（复用 frames.go）驱动握手/rid 关联/
  超时/notify/重连 generation 语义；ping/pong 版本拒绝
- **Steer**：agent 层单测——run 中途 Steer 后下一迭代的请求包含注入
  文本且记录按序落盘；最终消息期间 Steer → 不消费 → 插件重投路径
- **恰好一次**：集成测试模拟 notify → steer 消费 / steer 未消费 →
  followup，断言记录流中每封邮件恰一条 user.input
- **daemon 解析**：registry 缺失/端口死/版本不符/拉起超时
- **冒烟**：真实 mailboxd + 两个 sai 会话（或一个 pi 会话）互发

## 7. 分期

1. **P1（本次）**：frames/client/daemon/plugin/inject + agent 的
   Steer 能力 + `-mailbox` flag + 测试
2. **P2**：mailbox 工具集（list/read/send/reply/sessions，连接态激活），
   注入切换为通知式；远端模式完善
3. **P3**：web UI 状态区显示 mailbox 连接态（只读展示，页面仍不参与
   协议）
