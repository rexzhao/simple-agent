# Mailbox 客户端插件设计（sai ↔ mailboxd）

> **里程碑 D 阶段 0（设计刷新，2026-10-15）**：本文档最初写于里程碑 B1/B2/C
> 之前，多处与现行架构脱节。本版逐点对齐（事件观察 → Stream/SessionEvent；
> serve 多会话 → sessionhost 生命周期；项目身份 → host-id 派生，修正
> mailbox.json 的错误记载；审批/归档/标题 → C 的裁定）。迁移步骤见 §10，
> 待拍板问题见 §11。**尚无代码**（internal/mailbox 待 D1 起）。

目标：sai 以 flag 启用 mailbox 客户端，接入既有 mailboxd 守护进程
（`F:\work\pi-mailbox\mailboxd`，WebSocket 协议 v2），使 sai 会话可以
收邮件。**这是 Go 进程侧的功能，与 web UI 无关**（页面不感知 mailbox）。

## 0. 决定（已定）

1. Go 代码侧插件，web UI 零涉及
2. 参考 pi-mailbox 的 TS 客户端实现（client/supervisor/daemon 解析/
   项目身份全部镜像；源文件与行号见 §2、§5）
3. 排队机制支持 **steer / followup**：run 空闲 → 新回合投递；run 活跃
   → 中途注入（steer）；settle 竞态窗口 → 落回 followup 重投
4. **不做自动回复**：后续加入 mailbox 工具（mailbox_list/read/send/
   reply/sessions，对齐 pi 的工具集）后由 agent 自行决定回复
5. **只对已打开的会话注册**（§2.1）：离线邮件留 daemon，重开后拉积压

## 1. 解耦架构（核心约束）

**agent 主逻辑零依赖 mailbox 代码**——不是少依赖，是 `agent`（公开包）
不 import 任何 mailbox 包；`sessionhost` 同样零依赖。依赖方向单向：

```
internal/mailbox ──(公共 API)──▶ agent
     ▲
cmd/sai ──(flag 装配，CLI 模式）
internal/web ──(Options 钩子装配，serve 模式）
```

插件与 agent 的全部交互走三个已存在的公共 API（§1 的"新增 Steer"已在
里程碑 A/B 落地，无需任何新 agent 能力）：

| 交互 | API | 性质 |
|---|---|---|
| 空闲时投递邮件 | `Session.Send(text)`（现有） | user.input 记录，持久化/单飞/fold 免费 |
| 繁忙时注入 | `Session.Steer(text) (ItemID, error)`（已实现） | mid-run 输入；非运行态返回 ErrNoRun，调用方回退 Send |
| 确认投递/观察 run 结束 | `Session.Stream() *StreamSubscription`（现有） | 单一有序事件流（SessionEvent） |

`internal/mailbox` 内部分层（镜像 pi-mailbox 的 src/）：

```
frames.go    最小 WS 帧编解码（client 掩码帧；test server 复用）
client.go    连接 + rid 关联 + 5s 请求超时 + notify 分发（≈ client.ts）
daemon.go    registry 解析 → ping/pong 探测（版本校验）→ 按需拉起
             mailboxd → 项目身份派生（≈ daemon.ts）
plugin.go    连接监督：generation 计数、错峰重连、(重)连后拉积压
             （≈ client.ts 的 MailboxConnection + index.ts 的监督）
inject.go    notify → 排队 → Send/Steer/重投；投递确认；标记已读
```

零依赖纪律：WS 客户端**手写**（§9）。测试用的最小 WS server 复用
frames.go（server 帧不掩码）。

## 2. 协议对接（v2）

以 pi-mailbox 源码为准（本节行号引用 F:/work/pi-mailbox/src/）：

- **一条连接一个会话**（protocol.ts:49"one connection per session"）：
  同一 session 的第二次 hello **替换**第一条连接——重连语义天然干净。
  一个 serve 进程持有 N 个打开会话即 N 条连接；协议 v2 **没有**一个
  连接复用多会话的机制，不发明。
- 寻址：`~/.pi/mailboxd.json`（`$PI_MAILBOXD_DIR` 可覆盖，
  daemon.ts:29-37）→ `{port, instanceId, remote, pid}`；探测
  `ping/pong` 校验 `protocolVersion == 2`（protocol.ts:50
  `PROTOCOL_VERSION = 2`）；daemon 不在 → 从 `$PI_MAILBOXD` 或
  包旁（我们固定 PATH）拉起 `mailboxd`（分离进程），等 registry 出现
  （200ms 轮询，上限镜像 pi 的 SPAWN_WAIT_MS，daemon.ts:155-167）
- 注册：`Hello{project, projectRoot, session, agent: "sai", meta:
  {_model: provider/model}}` → `Welcome`；版本不符 → 拒连报错。
  **`sessionDir` 不发送**（那是 pi 会话目录、daemon 生命周期 GC 的
  输入）——后果：已删除 sai 会话的 daemon 邮箱不会被 GC 回收
  （§11(7) 记录在案）
  **致命登录错误**（重连无意义，client.ts:45）：`mailbox_in_use /
  bad_name / login_failed / name_held / session_bound` → 停止重连
  循环并报错，其余错误继续错峰重连
- 保活：Ping 周期（镜像 pi 的节奏）；`notify` 推送 unread 计数
  （protocol.ts:221-225，无 rid 的服务端推送）
- 离线不丢：daemon 持久化邮件；(重)连后 `Inbox(unreadOnly)` 拉积压
  （InboxReply 带 `nextCursor` 游标分页，protocol.ts:195-206——
  列表**新→旧**排序，投递时反转为最旧先投；pi 以 `announced` 集合
  去重已通知的邮件，index.ts:525-560 的 flushNotifications——我们
  镜像该模式）
- 投递确认后 `Read` 标记已读；崩溃/未确认 → 邮件保持未读，重启重拉

### 2.1 serve 多会话与生命周期（sessionhost 对齐）

B1 后一个 serve 进程经 sessionhost Pool 托管多个会话，会话会被
**驱逐**。mailbox 的注册粒度 = **每个打开的会话一条连接**，生命周期
挂在池的既有钩子上：

| 池事件（sessionhost.Options） | mailbox 动作 |
|---|---|
| `OnOpened` | 该会话的连接启动（hello）+ 拉积压 |
| `OnClosed` / `OnEvicted` | 断开该会话的连接（邮件留 daemon 未读） |

- **插件不持 Lease**：持 Lease 会把 refs 压在 1 以上，**阻止驱逐**
  （§B1 语义）——不可取。插件持有 `*agent.Session` 本身不阻止驱逐
  （refs 只数 Lease）；驱逐时 session 关闭 → Stream 的 `Lost()` /
  操作报错 → 插件感知并断开。
- **驱逐的用户可见后果（P1 取舍，opus 裁定）**：池的默认空闲窗口
  **30s**（sessionhost/pool.go 的 defaultIdleWindow——无请求、无
  订阅者、无 run 即驱逐）。按"只对已打开会话注册"，serve 模式下
  **没有浏览器标签页盯着的会话约 30s 后下线**——实际效果是"只有
  正在被看的会话在线收信"；离线期间的邮件在 daemon 积压，重开
  （Acquire → OnOpened）后拉取。**邮件触发的 run 期间不会被驱逐**
  （驱逐条件含"无 run"），投递中的会话不受影响。P2 的改进选项见
  §11(6)。
- **OnOpened 不得阻塞在网络上**：OnOpened 在条目发布前、Acquire
  路径内执行（pool.go 的注释与代码路径）——mailbox 的 hello/拉积压
  必须**异步启动**：OnOpened 只登记（记下 id+session+projectPath，
  立即返回），连接在插件自己的 goroutine 里建；daemon 不可达只
  影响收信，不影响 Acquire 的延迟与成败。web 已占用 OnOpened 建
  hub——**组合方式**：web 的钩子闭包里调用插件的登记函数（链条式
  组合，不新增池选项）。
- **归档会话**（C5）：归档即不可打开 → 不注册、不投递；未读留
  daemon。**删除**：OnClosed 路径断开即可（daemon 侧邮箱按其自身
  GC 语义存续，与 pi 一致）。
- **在途打开窗口**：与 §17.4 同款接受——hello 与会话关闭竞态时，
  daemon 侧连接被替换/断开的既有语义兜底。
- 懒打开（Acquire 冷路径）同样经 OnOpened → 插件对"被看一眼"的
  会话也会注册。接受：多一条连接的成本远小于投递遗漏。

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
  （iteration == maxIterations）不排干、留队给下一个 run
- 残留条目不丢弃：下一个 run 的首个迭代顶部投递。**RunID 归属**：
  记录带的是**消费它的 run** 的 RunID，不是输入时刻的 run
- 排干提交的文本是**规范化后**的（与 Append 同一规范化），wire
  历史/折叠/磁盘不分叉

pi 的等价物是 `sendUserMessage(..., {deliverAs: "steer"})`
（index.ts:551-557——"deliverAs steer is always safe: pi applies it
only while streaming and ignores it otherwise"，settle 竞态窗口里
steer 而非被拒）：我们的 Steer/Send/重投坍缩与其同构。

mailbox 通知注入直接用 Steer/Send，无需恰好一次协议：通知丢失无害
（下次 notify 重试），邮件本体由 agent 经工具拉取（P2）或内容注入
（P1，§4）。

竞态窗口：settle 与 Steer 之间——`Steer` 返回 ErrNoRun 或入队后未
消费，都收敛到 followup 重投，幂等。

## 4. 注入格式与投递确认（P1：内容注入）

```
[mailbox] from <project>/<session> re: <subject> (<mail-id>)
<body>
```

v1 没有 mailbox 工具，邮件**内容**直接注入是唯一通路；agent 的回答
落在会话里（操作者可见），不外发。P2 加入工具后切换为 pi 的通知式
注入（"[mailbox] You have N new mail(s); use mailbox_list"），由
agent 自行读信与回复——工具按连接状态激活/停用（对齐 pi）。

**投递语义（唯一口径：至多一次注入 + 至少一次标记已读）**：

- **注入前去重**：投递前按 `<mail-id>` 扫描会话**已落盘的记录**
  （`Stream().Initial()` 的记录视图；mail-id 内嵌于注入文本，
  记录里子串可查——mail-id 全局唯一、用户手打同串概率可忽略，
  若要更严可要求 `(<mail-id>)` 含括号整段匹配）。命中 = 该邮件
  已注入过 → 跳过注入、直接补 `Read`。**这是确认状态在活跃段内
  的权威来源**（opus 裁定），不是内存标志——重连/重开/崩溃后从
  记录推导，覆盖"已落盘但 `Read` 未发出"的窗口。
  **边界（接受，opus 裁定）**：内存记录流只保留**活跃段**（压缩
  后截断，里程碑 A 第 5 步）——`Initial()` 看不到压缩点之前的
  记录。残余窗口：注入 → 落盘 → `Read` 未成功 → **压缩** → 断开
  重连 → 未读重拉 → 去重扫不到 → 重复注入。需要"Read 丢失"与
  "中间发生压缩"同时成立，窗口窄；回读磁盘归档段的代价大。P1
  接受，D4 用一条**记录性测试**钉住该已知行为（不是修它）。
- **投递确认**（§1 的事件观察）：经 `Session.Stream()` 订阅
  SessionEvent；确认 = `RecordCommitted` 且 Record 为 user.input 且
  身份匹配：
  - Steer 路径：`Record.ItemID == Steer 返回的 ItemID`（入队即铸，
    消费时同 ID 落盘）
  - Send 路径：Send 不返回 ItemID，以注入文本中内嵌的 `<mail-id>`
    作关联键（Content 子串匹配；误匹配分析：mail-id 唯一 + §3 的
    规范化不变性保真；如需更严用括号整段匹配）
- 确认到达后 `Read` 标记已读——**`Read` 允许重复发**（幂等），
  丢失无害（未读 → 下次拉积压 → 记录去重兜住）。
- **未确认**（会话关闭/驱逐/崩溃，含 §6 的"Ask 挂起期间发生驱逐
  重连"场景）→ 邮件未读，下次拉积压 → **先记录去重再注入**。
- **run 结束观察**：终端 `Status{Running:false}` 事件触发"未消费
  steer 的 followup 重投"检查（重投同样先过记录去重）。

**标题规则（C1 裁定维持现状）**：会话首条输入若是邮件注入，标题取
注入首行（"[mailbox] from … re: <subject>"）——**接受**：与"首条
user.input 即标题"的既有规则一致，且格式本身可读（含 subject）。

## 5. 项目身份（host-id 派生，修正旧记载）

**旧文档的 `<cwd>/.pi/mailbox.json` projectId (UUID) 记载是错的**，
现行 pi 实现（daemon.ts:230-250）：

- `hostId`：`~/.pi/mailbox-host-id`（`$PI_MAILBOXD_DIR` 覆盖下同
  目录），16 hex 随机，每机一次（区分不同机器共享同一路径的场景）
- `projectId = base36(sha256(hostId + "\\n" + normalizeProjectPath(
  projectRoot)))[0:8]`——确定性派生，**无项目内状态文件**
- `sessionId = base36(sha256(agent + "\\n" + sessionUuid))[0:10]`，
  agent 固定传 `"sai"`——与 pi 会话同项目即互通（sessions 列表
  互见、可直接互发），但各自独立邮箱
- daemon 在**本地模式**把数据存 `projectRoot/.pi/mailbox/` 下
  （HelloMsg.projectRoot，protocol.ts:59-60）——项目目录只被写入
  `.pi/mailbox/` 数据，不读不写任何 mailbox.json
- `normalizeProjectPath`：平台大小写与分隔符归一（镜像
  daemon.ts 的实现细节，D2 落地时逐行核对）

**路径来源**：CLI 模式 = 进程 cwd；serve 模式 = 会话所属项目的
注册表路径（池的 `Options.Locate` 已返回 projectPath；`OnOpened`
签名当前只有 (id, session)——**装配侧**（internal/web）在钩子里
自行查表 ns→path，sessionhost 不为 mailbox 改签名）。

## 6. 审批交互（B2 对齐）

邮件触发的 run 遇 Ask 策略：**照常 pending，不自动批准**（无人值守
也一样）。serve 模式下 web 审批卡可答；CLI 模式下操作者在 REPL 答。
风险写明：无人值守 + Ask 策略 → run 挂起至有人批准，邮件发件方
看不到任何回复（P1 的回答本来就只在会话内，不外发——风险面小）。
P2 的 mailbox 工具引入"批准/拒绝"邮件的能力时再议自动化。
**对本插件自身的影响**：挂起期间投递永不确认——§4 的记录去重
正是为此（挂起 + 驱逐重连后同封邮件不会二次注入）。

## 7. CLI / serve flag

```
sai -mailbox                 # 本地模式：解析/拉起 daemon
sai -mailbox-remote h:p      # 远程模式（固定地址 + token）
sai -mailbox-token <token>
```

- serve 模式：`-mailbox` 对**所有打开的会话**生效（§2.1 的每会话
  连接）；REPL/CLI 模式只对当前会话。**注意**：serve 下无人查看的
  会话约 30s 即驱逐下线（§2.1）——"只有正在被看的会话在线收信"
  是 P1 的用户可见取舍
- **远程模式的信任前提**：`-mailbox-remote` 的地址必须是 loopback
  或已建立的加密隧道（如 ssh 端口转发）——`-mailbox-token` 明文走
  argv（同机 `ps` 可见），此前提下"能读到 argv 的同机用户"即视为
  可信（威胁模型明示）。非信任网络的远端 token 走文件
  （`PI_MAILBOX_TOKEN_FILE`）留待 P2
- 环境变量与 pi 兼容：`PI_MAILBOXD`（二进制路径）、`PI_MAILBOXD_DIR`
  （registry 目录，host-id 同目录）
- **与 `-server-root` 无关**：mailbox 状态（host-id、registry）在
  `~/.pi`（或 `$PI_MAILBOXD_DIR`），不在 serve 的数据根下
- REPL 模式下邮件触发的 run 与用户输入共享单飞槽（会话固有语义）

## 8. 验证

- **协议**：最小 WS test server（复用 frames.go）驱动握手/rid 关联/
  超时/notify/重连 generation 语义；ping/pong 版本拒绝；致命登录码
  停止重连
- **Steer**：既有 agent 层测试已钉（里程碑 A/B）；本步不重测
- **活跃段内至多一次注入**：集成测试模拟 notify → steer 消费 /
  steer 未消费 → followup，断言活跃段记录中每封邮件至多一条
  user.input；含"记录已落盘 → 断开 → 重连 → 不重投"与"未落盘 →
  重投"两向；另有一条**记录性用例**：注入 → 落盘 → 压缩 →
  Read 丢失 → 重连 → 重拉 → 断言**已知会重复注入**（§4 边界的
  钉子，不是修它）

## 9. 零依赖手写 WS（保持）

RFC6455 子集：client 握手（HTTP/1.1 Upgrade + `Sec-WebSocket-Key`/
Accept 计算）；**client→server 帧必须掩码**（RFC 规定），server 帧
不掩码；opcode 只用 text(1)/ping(9)/pong(10)/close(8)；消息小、纯
文本、**无分片**（不实现 continuation）；无压缩扩展。约 200 行。
test server 复用 frames.go + 自算 Accept。

## 10. 迁移步骤（每步独立送审 + commit）

| 步骤 | 内容 | 回归保护 | doc-sync |
|---|---|---|---|
| D1 | frames.go + client.go + 最小 WS test server | 握手/掩码/rid/超时/notify/致命码/重连 generation 的表驱动测试；变异：掩码丢一位→server 拒；rid 错配→超时 | 本文 §9 |
| D2 | daemon.go：registry 解析/探测/拉起 + host-id/项目身份派生 | registry 缺失/死端口/版本不符/拉起超时；**固定输入集交叉测试**（同一组 hostId+路径喂 Go 与 TS 两实现，断言同 projectId/sessionId） | 本文 §5 |
| D3 | plugin.go：会话生命周期接线（OnOpened/OnClosed/OnEvicted）+ 错峰重连 + 积压拉取 | sessionhost 钩子驱动的注册/断开/重开；驱逐不被 Lease 阻止（既有池测试不改断言）；**daemon 不可达时 Acquire 延迟不变**（OnOpened 只登记不联网） | 本文 §2.1 |
| D4 | inject.go：Send/Steer/followup + 投递确认（ItemID/mail-id）+ Read | **活跃段内至多一次注入**集成测试：notify → 注入 → 落盘确认；**记录已落盘 → 断开 → 重连 → 不重投**（记录去重）；未落盘 → 断开 → 重连 → 重投；Ask 挂起期间驱逐重连 → 不重投；**压缩后重连 → 重复注入（记录性，§4 边界）** | 本文 §3/§4 |
| D5 | `-mailbox` flag（CLI + serve 装配）+ serve 的 ns→path 接线 | CLI/serve 装配测试（真实 flag 路径） | 本文 §7 |
| D6 | 冒烟：真实 mailboxd + sai↔sai（或 pi）互发 | 冒烟记录（截图/日志路径）进送审邮件 | README |

## 11. 待拍板问题（由 opus 裁定，裁定记入本节）

1. **无人值守审批**（§6）：照常 pending 不自动批准。**opus 裁定
   （mik6z0ttd4）：按推荐。**
2. **邮件注入标题**（§4）：接受（注入首行含 subject，可读）。
   **opus 裁定（mik6z0ttd4）：按推荐。**
3. **懒打开即注册**（§2.1）：一次"看一眼"（Acquire 冷路径）也会
   hello + 拉积压。接受（成本一条连接，收益无投递遗漏）；D3 顺带
   量化连接数。**opus 裁定（mik6z0ttd4）：按推荐。**
4. **P1 注入样式**：内容注入先行（P2 工具集是独立大步）。
   **opus 裁定（mik6z0ttd4）：按推荐。**
5. **远端模式的 token 存储**（§7）：`-mailbox-token` flag 明文
   （argv，同机 `ps` 可见）vs 读文件（`PI_MAILBOX_TOKEN_FILE`）。
   **flag + 信任前提限定**——远程地址必须 loopback 或加密隧道，
   威胁模型"能读到 argv 的同机用户即视为可信"明示于 §7；非信任
   网络场景的文件式留待 P2。（旧稿"本机 loopback 语义内"盖不住
   `-mailbox-remote h:p`——已修正。）**opus 裁定（mik6z0ttd4）：
   按推荐。**
6. **无人查看会话的在线窗口**（§2.1，P1 裁定 + P2 选项）：
   **P1 = 只在打开期间在线**（opus，mik6z0ttd4）——30s 驱逐后
   下线，邮件 daemon 积压。P2 评估：(A) 维持现状；(B) mailbox
   经 EvictGuard 否决驱逐——会话永不驱逐、长期持 OS 锁、CLI 无法
   打开同一会话（代价写明）；(C) 收信唤醒——serve 对已注册项目
   的未归档会话保持轻连接，notify 时 Acquire 冷打开、投递、随后
   正常驱逐。**P2 评估 (C)**（opus 意向）。
7. **删除 sai 会话时 daemon 邮箱的清理**：Hello 不发 sessionDir →
   daemon 的生命周期 GC 不覆盖 sai 会话，删除后邮箱留存。
   **P1 不处理**（记录在案，opus 口径）；P2 与工具集一起议
   （删除钩子主动清或 daemon 侧按活跃度回收）。
