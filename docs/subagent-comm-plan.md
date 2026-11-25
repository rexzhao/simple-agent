# 子会话通讯（subagent 组）：方案与接口命名

> 状态：**已实施（2026-11-25）**
>
> 目标：把现有「mailbox 邮件」从模型可见的工具里撤掉，改成**只服务于父子关系**的
> subagent 通道；同时取消「同一项目」的硬限制，让父子跨项目也能通信。

---

## 1. 需求与现状的差距

用户提的要求，逐条对照现状：

| # | 要求 | 现状 | 差距 |
|---|---|---|---|
| 1 | mailbox 取消「会话所属项目」的限制，可任意转发通信 | `mailboxHost.MailboxDeliver` 硬拒跨项目；`MailboxPeers` 只列同项目；存储目录 `projects/<id>/mailbox/` 按项目分 | 投递与存储都要与项目解耦 |
| 2 | 对 agent **隐藏所有 mailbox 接口** | `mailbox_send / mailbox_list / mailbox_read` 三个工具注册给所有会话 | 从 `BuiltinWith` 移除，模型不可见 |
| 3 | 对 agent 提供 **subagent 组**接口 | 只有 `new_session`（建），没有查父子、没有定向通知 | 新增 4 个工具 |
| 4 | 查询父会话（父会话没有） | `Session.parentID` / `SessionRecord.ParentID` 已存在；`SessionRef.ParentID` 已存在 | 基本只差一个工具壳 |
| 5 | 查询子会话列表（子会话没有） | 档案里有 `ParentID`，但没有「按父会话列出子会话」的查询 | 新增 host 侧查询 |
| 6 | 通知父会话 / 通知子会话 | 只有 `new_session` 的 prompt 一次性传递 + 已隐藏的 mailbox | 两个新工具 |

**关键结论：存储层几乎不用重写。** `internal/mailbox` 的 `Open(dir, targetID)`
已经接受任意 `dir`，"项目隔离"只体现在 host 侧传进来的目录上；改成
`<data_dir>/mailbox/<sessionID>.jsonl` 即可全局可用。真正要动的是 host 侧的
权限判断、目录来源、以及 tools 层的工具集合。

---

## 2. 接口（工具）命名

**三个**工具，沿用 `subagent_` 前缀，名字与语义一一对应：

| 工具名 | 谁能用 | 作用 | 主要参数 | 返回 |
|---|---|---|---|---|
| `subagent_list` | **仅父会话** | 列出自己的子会话 | `includeArchived`（可选，默认 false） | 子会话列表（id、标题、**活动明细**、未读通知数） |
| `subagent_notify_parent` | **仅子会话** | 给父会话发一条通知 | `subject`、`body` | 送达结果（是否唤醒） |
| `subagent_notify_child` | **仅父会话** | 给**一个**子会话发通知 | `to`（子会话 id，**一次一个**）、`subject`、`body` | 送达结果（目标活动明细、是否唤醒） |

> **活动明细**（`SubagentActivity`，**只有 `subagent_list` 返回**）：
> - `busy`：`idle` / `streaming`（在生成回答）/ `runningTool`（在执行工具）/
>   `awaiting`（等用户回答）/ `compacting`
> - `tool`：`runningTool` 时**正在执行的那个工具名**（`bash`、`write`…），其余情况为空
> - `elapsed`：本轮**已运行多久**（秒）。`idle` 时为 0；有值就说明"它从被派活
>   到现在一直在跑"，这是判断"卡住了还是在干活"的最直接依据
> - `queued`：还有几条排队中的用户插话（有用但可选，成本几乎为零）

**为什么没有 `subagent_parent`（子会话查父会话）**

用户的判断是对的：**子会话不需要知道父会话的状态**。

- 它要跟父会话说话，`subagent_notify_parent` 就够了——不需要先知道父会话的 id
  才能发（目标由 host 按 `parentID` 解析），也不需要知道父会话此刻在忙什么。
  "父在忙"不该改变子会话的行为：通知照样落盘，父会话下一轮自己会看到。
- 反过来，**父会话能查子会话是有用的**：一份 `subagent_list` 就能看出哪个子会话
  卡在 `bash` 里 4 分钟、哪个还空闲可以派活——这是"调度"需要的信息。
  子会话没有对应的调度需求（它只对自己的那一个父会话负责）。
- 少一个工具省下的不只是描述文本：每轮请求的工具 schema 都短一点，
  而且**模型不会再有"要不要先查一下父会话"这种无谓的犹豫**。

对应地，`SubagentPeer`（工具层视图）只用于"子会话"，父会话的信息不经过工具层。

**命名理由**

- 统一 `subagent_` 前缀：模型一看到就知道"这一组工具只和我的子/父会话打交道"，
  不会误以为可以给任意会话发消息（这正是 2 的要害）。
- `notify` 而不是 `send` / `message`：明确这是**单向通知**，不要求对方回复，
  从名字上就压住"两个 agent 互相回信把token烧光"的循环。回信要靠对方自己用
  `subagent_notify_parent`（子→父）或 `subagent_notify_child`（父→子），
  且工具描述里明确写"不需要行动就别回"。
- `subagent_list` 不叫 `subagent_children`：叫 list 是因为它列的是"我手下这一组"，
  加上返回的是带状态的清单，`list` 比 `children` 更贴切。
- **没有广播**：`subagent_notify_child` 一次只发一个 `to`（用户定的）。理由：
  `to: "all"` 会让模型一句话把 N 个子会话同时叫起来，每份都是真金白银的 token，
  而且它自己往往没意识到代价。要通知多个就调用多次——多次调用在工具结果里
  逐个给出"是否唤醒"，模型能看到自己花了多少。`subagent_list` + 活动明细
  （见 §7）配合使用，就能做到"挑空闲的那几个发"。
- 与现有 `new_session` 的关系：`new_session` 保持原名（它已经是稳定的对外契约，
  改名会连带 android / 界面文案），但**描述改成指向新的三个工具**（原文里那句
  "use mailbox_send / mailbox_read to coordinate" 必须改掉，否则模型会去找不存在的工具）。

**模型可见性的实现方式是"不注册"，不是"注册了报错"**（与现有 `Options.NoSpawn`
同一个理由，见 `internal/tools/registry.go:655`）：

```go
type Options struct {
    // ...
    // Role 决定注册哪几个 subagent 工具（取代原来的 NoSpawn）。
    Role SubagentRole
}

// SubagentRole 是会话在父子关系里的角色，决定工具集。
type SubagentRole int

const (
    // RoleRoot 是顶层会话：可 new_session + subagent_list + subagent_notify_child。
    RoleRoot SubagentRole = iota
    // RoleChild 是子会话：只可 subagent_notify_parent，
    // 且**不注册** new_session（不允许派生孙会话，规则不变）。
    RoleChild
)
```

> 名字用 `RoleRoot` / `RoleChild` 而不是 `SubagentNone` / `SubagentChild`：
> **顶层会话同时就是"父会话"**，`None` 会被读成"没有 subagent 能力"，而它其实
> 有（能列子会话、能通知子会话）。零值 = `RoleRoot` 也让"忘记设置"退化成
> 最保守的一侧以外——不对，这里要小心：**零值退化必须安全**。`RoleRoot`
> 多了 `new_session`/`subagent_list` 两个工具，漏设时子会话会多出本不该有的能力。
> 因此 `BuiltinWith` **不信任零值**：`Role` 由 host 在 `driverOptionsSpec` 里
> 显式赋值（那里本来就知道 `spec.ParentID != ""`），且加一条断言测试
> `TestNoSpawnNoLongerExists` 防止有人回头用旧开关。

---

## 3. 分层改动

### 3.1 `internal/mailbox`（存储层，改动最小）

- 目录由调用方给，**存储层不需要感知项目**：现有 `Open(dir, targetID)` 保持不变。
- 新增：`dir` 的来源从 `Catalog.MailboxDir(projectID)` 换成
  `Catalog.GlobalMailboxDir()`（新方法，返回 `<data_dir>/mailbox`）。
- 迁移：`projects/<id>/mailbox/*.jsonl` 里的旧邮件**不迁移、只读兼容**：
  读取时先看全局目录，没有再看各项目目录（按 `ProjectOfSession(sender)` 反查）。
  理由：邮件是短生命周期的协作数据，为它写迁移脚本不值得；但用户手上可能还有
  未读，直接丢弃会让"侧栏徽标还在、点开却没有"变成新 bug。
- 迁移开关：`mailbox.Unread` / `Box.List` 接受多个候选目录（`OpenAny(dirs, id)`），
  旧目录排在后面。**只读**——新邮件一律写全局目录，旧目录自然萎缩。

### 3.2 `internal/store`

- 新增 `Catalog.GlobalMailboxDir() string`。
- 新增 `Catalog.ChildSessions(parentID string) []SessionRecord`
  （遍历 `ListProjects()`，按 `record.ParentID == parentID` 收集）。
  **不按项目过滤**：父子可能跨项目（见 1 的需求），这是取消项目限制后的必然结果。
- `SessionRecord.ParentID` 的注释要改：现在写的是"侧栏折叠用"，补一句
  "也是 subagent 通讯的权限依据（唯一依据）"。

### 3.3 `internal/host`

新增 `internal/host/subagent.go`，把现有 `mailbox.go` 里可复用的部分搬过来：

- `subagentClient(session)` 取代 `mailboxClient`：**不再要求 `ProjectID != ""`**，
  只要求 `Catalog != nil`（纯内存后端仍不可用）。
- 目录一律取 `GlobalMailboxDir()`，与项目无关。
- 新增 `Local.childrenOf(sessionID) []*Session`：内存里的会话列表优先，
  内存里没有（未加载的归档会话）再回落到 `Catalog.ChildSessions`。
- 保留但**降级为内部实现**：`wakeForMail`、`mailboxNotice`、`MailboxUnread`、
  `SessionRef.MailUnread` 全部沿用——它们对"通知"这件事依然必要，只是不再有
  对应的模型可见工具。改名的部分：
  - `mailboxClient` → `subagentClient`
  - `mailboxNotice` → `subagentNotice`
  - `MailboxUnread` → 保持公开名（界面/remote 用），内部实现指向 subagentClient。
  - **`SessionRef.MailUnread` 字段名不改**：wire 协议与 Android 端都在用，
    改名要动三处协议，与本次需求无关。
- 删掉 `MailboxForTest / MailboxNoticeForTest` 里的 `tools.MailboxHost` 依赖，
  改为 `SubagentForTest()`（测试要跟着改，见 §6）。
- **权限校验放在 host，不放在工具里**（工具层拿不到"谁是父谁是子"的权威数据）：
  - `notify` 目标必须是自己的父会话或子会话，否则报错"只能和父/子会话通信"。
  - 自己给自己发：拒绝了没有意义，直接拒绝（保持现有 `不能给自己发信`）。

### 3.4 `internal/tools`

- `registry.go`：`MailboxHost` / `MailboxPeer` / `MailboxMail` 三个类型**删掉**，
  换成 `SubagentHost`：

  ```go
  // SubagentHost 是 tools 包对"父子会话通讯"的最小要求（由 host 实现）。
  //
  // 刻意**没有** Parent()：子会话不需要知道父会话的状态（见 §2），
  // 通知目标由 host 按自己的 parentID 解析，工具层不参与。
  type SubagentHost interface {
      // Children 返回自己的子会话（父会话才有；子会话返回空）。
      Children(includeArchived bool) []SubagentPeer
      // NotifyParent 给自己父会话发一条通知（子会话才有）。
      NotifyParent(subject, body string) (SubagentDelivery, error)
      // NotifyChild 给**一个**指定子会话发通知（父会话才有）。
      NotifyChild(to, subject, body string) (SubagentDelivery, error)
  }

  type SubagentPeer struct {
      ID       string
      Title    string
      Archived bool
      // Activity 是活动明细（idle 时会话也能有 queued，字段都填零值）。
      Activity SubagentActivity
  }

  // SubagentActivity 是"这个会话现在在干什么"。
  //
  // 为什么要细分到工具名：父会话最常见的决策是"它到底卡住了还是在干活"。
  // 只说"忙"回答不了这个 —— 跑了 4 分钟的 bash 是正常，跑了 4 分钟的
  // streaming 大概率是模型或网络出了问题。
  type SubagentActivity struct {
      // Busy 取值见 SubagentBusy* 常量。
      Busy   string
      // Tool 是 runningTool 时正在执行的工具名（其余情况为空）。
      Tool   string
      // ElapsedMS 是本轮已运行时长（idle 时为 0）。
      ElapsedMS int64
      // Queued 是排队中的用户插话条数。
      Queued int
  }

  const (
      SubagentBusyIdle        = "idle"
      SubagentBusyStreaming   = "streaming"
      SubagentBusyRunningTool = "runningTool"
      SubagentBusyAwaiting    = "awaiting"
      SubagentBusyCompacting  = "compacting"
  )

  type SubagentDelivery struct {
      ID      string
      Title   string
      MailID  string
      Woken   bool   // 是否真的把它叫起来了（会花钱）
      Skipped string // 没唤醒的原因（运行中、等待回答等），只用于展示
      // Activity 是**投递那一刻**的活动明细。
      //
      // 只有 NotifyChild 会填（父会话据此理解"为什么没唤醒"）；
      // NotifyParent 不填 —— 子会话拿不到也不该依赖父会话的状态。
      Activity *SubagentActivity
  }
  ```

`SubagentActivity.Busy` 是 `driver.Busy` 的映射，但常量定义在 tools 包
（tools 不能依赖 driver），映射在 host 侧一处完成：

| `driver.Busy` | `SubagentActivity.Busy` | 父会话据此怎么用 |
|---|---|---|
| `BusyIdle` | `idle` | 可以通知，会被唤醒 |
| `BusyStreaming` | `streaming` | 可发，但**不会**被打断，它下一轮自己看 |
| `BusyRunningTools` | `runningTool` | 同上；`Tool` 字段告诉你它在跑什么 |
| `BusyAwaitingAnswer` | `awaiting` | 可发，不唤醒（等用户回答） |
| `BusyCompacting` | `compacting` | 可发，不唤醒 |

- `mailbox.go` 整体删除，新增 `subagent.go`（三个工具 + 参数校验）。
- `ToolContext.Mailbox` → `ToolContext.Subagent SubagentHost`。
- `BuiltinWith`：
  - 移除 `mailboxSendTool / mailboxListTool / mailboxReadTool`。
  - `Options.NoSpawn` **保留**（子会话不能派生孙会话这条规则没变），
    但它与 `Subagent` 角色合并成一个 `Role` 更好：不再需要两个开关表达同一件事。
  - 按 `Role` 追加：`RoleRoot` → `subagent_list` + `subagent_notify_child`；
    `RoleChild` → 仅 `subagent_notify_parent`。

### 3.5 `internal/driver`

- `Options.MailboxNotice` → `Options.SubagentNotice`（`SetMailboxNotice` 同步改名）。
- buildRequest 里注入的那条通知消息：
  - `MessageID: "mailbox-notice"` → `"subagent-notice"`
    （**只影响新请求**；旧日志里已有的这个消息 ID 不参与任何判断，安全）。
  - 文案改成"你有 N 条来自父/子会话的通知"，并明确"用 subagent_* 处理"。
- **注意**：`SubagentNotice` 对子会话和父会话**都要挂**（父会话也要能被通知）。

### 3.6 `internal/host/childsession.go`

- `attachSessionTools` 改名为 `attachSubagent`：不再"只挂给顶层"，
  而是**按角色挂不同能力**（子会话也要挂，否则 `subagent_notify_parent` 无从实现）。
- `NewChildSession` 里那句 `mailbox_send 发给它` 的返回文案要改。

### 3.7 活动明细的数据来源（判定：**简单，做**）

结论先说：四个字段里三个白送，第四个只在本机加两行。**不碰 `ViewState`、
不碰 wire 协议**，所以这一条从"有点复杂"降到"顺手"。

| 字段 | 来源 | 工作量 |
|---|---|---|
| `busy` | `session.Busy()` 已有，映射成 `SubagentBusy*` | 0 新增状态 |
| `elapsed` | `roundStartedAt` 已有，加一个导出方法 | 1 个方法 |
| `queued` | `Driver.Queued()` 已有 | 0 |
| `tool` | `runTools` 循环里记一个字符串 | 约 5 行 |

#### 1. `tool`：只在 host 侧记，不进 driver

`RunTools` effect 里的 `Calls []tools.Call` 是"交给调用方执行"的一次性值，
driver 自己不存——**这是对的，不要为此给 driver 加状态**。全项目的工具执行点
只有一处（`internal/host/session.go:1097` 的 `runTools`），在它的串行循环里
维护一个字段即可：

```go
// 新增在 Session 上（由主线程写、subagent_list 读，加锁保护）
var runningToolMu sync.Mutex
runningTool string // 空表示当前没在跑工具

// runTools 的循环里：
s.setRunningTool(strings.Join(namesOf(calls), ", ")) // 循环开始前
defer s.setRunningTool("")                           // 整批结束清空
```

两个必须注意的点：

- **写要从主线程走**。工具本体跑在后台 goroutine，但这个字段要在
  `runTools`（主线程）里设置与清空，不要在 goroutine 内部写——
  `Driver` 不线程安全，而这个字段的读者是另一个 agent 的工具调用。
- **并发读取是安全的**，因为读它的 `subagent_list` 只读一次快照，
  加一把独立的小锁即可（不要复用 driver 的任何锁）。

#### 2. `elapsed`：加一个导出方法

```go
// RoundElapsed 返回当前回合已运行时长；空闲时返回 0。
func (d *Driver) RoundElapsed() time.Duration
```

`roundStartedAt` 已有，只是没导出（现在只在 `recordRoundFinished` 内部用）。
**口径要选对**：用 `roundStartedAt` 而不是 `Round.startedAt` —— 前者覆盖
`Send`/`Retry`/`SendWithAttachments` 三个入口，语义一致。空闲时必须返回 0，
否则会显示成"一直在跑"（已列入测试清单）。

#### 3. host 侧组装

```go
func (h *subagentHost) activityOf(s *Session) tools.SubagentActivity {
    return tools.SubagentActivity{
        Busy:      mapBusy(s.Busy()),
        Tool:      s.RunningTool(),           // 空字符串 = 没在跑工具
        ElapsedMS: s.drv.RoundElapsed().Milliseconds(),
        Queued:    len(s.drv.Queued()),
    }
}
```

#### 4. 展示端（镜像会话）**不做**

镜像 `Session` 没有 `drv`，本来就没有 `RoundElapsed`，也不会有 `runningTool`。
**不往 `ViewState` 加 `RunningTools` / `RoundStartedAt`** —— 理由：

- 这个信息的唯一消费者是父子会话，而父子会话永远在同一个执行端进程里
  （`Local.sessions`），拿不到的情况根本不存在；
- 侧栏/手机端不需要显示"那个会话在跑 bash"，那是给 agent 看的决策依据，
  不是给用户看的状态。用户要看的是"在忙"，而 `Busy` 早就有了；
- 一旦加进 `ViewState`，就要动 wire 协议 + Android 端的降级处理，
  这一条就会从"5 行"变成"两处协议 + 一个客户端"，不值得。

> 以后真要在界面显示工具名，是独立需求，届时补也不难（`ViewState` 是纯增量字段）。

### 3.8 子会话的模型：会话级设置 + `/subagent` 命令

现状（`internal/host/childsession.go:44-50`）是**写死继承父会话**：

```go
Selection: parent.Selection(),
Effort:    parent.Effort(),
```

**默认行为保持这样**（用户定的）：不设置就跟父会话一致。下面加的是"可选覆盖"。

#### 3.8.1 存储：会话级设置

缓存在**内存**（`Session` 上），持久化用**已有的** `Catalog.UpdateSession`
写回 `SessionRecord` —— 那里已经有 `ParentID` 这类会话属性和现成的读写路径，
不需要新的存储层。

```go
// store.SessionRecord 新增（omitempty，旧档案读出来是零值 = 继承父会话）
ChildModel  string `json:"child_model,omitempty"`  // 空 = 与父会话一致
ChildEffort string `json:"child_effort,omitempty"` // 空 = 与模型默认档位一致
```

> 为什么存在**父会话**的档案上：这个设置是"我派生出去的子会话用什么模型"，
> 语义归属父会话。子会话自己的档案不记它（子会话的模型在创建那一刻就定死了，
> 之后再改设置不影响已经存在的子会话）。

**不做 `provider` 字段**：选择本 provider 内的另一个模型就够了
（`ProviderChoice` 里 `ProviderID` 只有一个值，换 provider 意味着换 Key/端点，
那是"另一个方案"级别的决定，不是这条命令该干的事）。

#### 3.8.2 `/subagent` 命令

`internal/uiapp/commands.go` 里加一条，框架现成（`slashCommands` 表 + 候选列表
+ `/help` 自动生效，`commands.go:18` 就是为此设计的）：

```
/subagent              查看当前设置（没设置就显示"跟随本会话 <模型名>"）
/subagent model        弹出模型选择列表（项目内已有选择器可复用）
/subagent model <名>   直接设置
/subagent model off    清除，回到"跟随父会话"
```

- **模型列表从哪来**：`l.cfg.Models []ProviderChoice`（`host.go:200`）已经是
  "当前 provider 下可选的模型 + 显示名 + 各自支持的档位"，界面本来就用它，
  这里直接用同一份，不新增数据源。
- **校验**：`/subagent model xxx` 只接受列表里存在的 id（或显示名）；
  不存在时**立即报错并列出可选值**，不要等到建子会话时才失败。
- **命令发给谁**：这是一条会话级命令，走 `host.Command{Kind: "setSubagentModel", ...}`
  （与 `setEffort` 同一条路径，见 `local.go:412`），由执行端写进自己会话的状态并
  `UpdateSession` 回写档案。**不要**在界面层直接改——远程展示端下它改不到执行端。

#### 3.8.3 创建子会话时应用

```go
// childsession.go
selection, effort := parent.Selection(), parent.Effort()
if model := parent.subagentModel(); model != "" {   // 会话级设置优先
    selection = providers.Selection{ProviderID: selection.ProviderID, Model: model}
    effort = ""                                      // 档位按新模型的默认值来
}
```

档位单独处理的原因：父会话的档位可能在新模型上不存在（`effortsFor(sel)` 是按模型
给的）。`newSessionWith` 里 `effort == ""` 已经会回落到 `l.effortFor(selection)`，
所以传空即可，不用额外写判断。

#### 3.8.4 `new_session` 工具**不加** model 参数

agent 不该越权决定用哪个模型花钱；这件事由用户在会话里用 `/subagent` 定。
工具的参数仍是 `title` + `prompt` 两个。

### 3.9 让 agent 知道自己是子会话（提示词）

**机制已经现成**：`ToolDef.Guidance` 会被 `tools.GuidanceSection` 拼进系统提示，
而且**只在工具真的注册时出现**（`internal/tools/guidance.go:12` 那条性质）。
这正好是需要的效果——子会话才注册 `subagent_notify_parent`，所以"你是子会话"
这句话只会出现在子会话的提示里，顶层会话看不到一句多余的话。

**两条一起用，各管一段：**

1. **工具描述里（必须）**——模型在决定要不要调这个工具时看的就是这段，
   这是最不容易被忽略的位置：

   ```
   subagent_notify_parent.Description:
     "You are a CHILD session spawned by another session. Use this to report
      a result, ask for a decision, or hand back a finding. The parent is
      woken by this (or sees it at its next turn). Do not notify for
      pleasantries — every notify costs the parent a turn."

   subagent_list.Description:
     "List YOUR child sessions (you are their parent), including what each is
      doing right now: streaming, which tool it is running, and how long the
      current round has been going."

   subagent_notify_child.Description:
     "Notify ONE of your child sessions. Check subagent_list first — if it is
      already streaming or running a tool, the message is queued for its next
      turn instead of waking it. One target per call."
   ```

2. **`Guidance` 里（推荐，一句话**）**——因为它只在工具可见时进提示，天生
   就是"按角色定制的系统提示"：

   ```go
   // subagent_notify_parent 的 Guidance（只在子会话可见）
   Guidance: "You were spawned by another session and normally cannot see its " +
       "conversation. Use subagent_notify_parent to report results, ask for a " +
       "decision, or escalate a blocker. When your task is done, notify the " +
       "parent instead of just ending — otherwise it is left waiting for you."

   // subagent_list / subagent_notify_child 的 Guidance（只在父会话可见）
   Guidance: "These tools talk to the child sessions you spawned. Check " +
       "subagent_list first: a child that is streaming or running a tool will " +
       "read your notice at its next turn, not immediately."
   ```

**不要写进 `Config.SystemPrompt`。** 那是用户在配置文件里手写的全局追加，
所有会话共享；把"你是子会话"塞进去会让顶层会话也看到，是错的。

**初始 prompt 里不要重复这件事。** `new_session` 的 `prompt` 参数是**任务说明**
（"读一下 internal/relay 并修掉 XX"），模型已经在工具描述与 Guidance 里知道了
自己是谁。往任务说明里塞身份介绍有两个坏处：一是每个子会话都多一份重复内容
占 token，二是父子两处说法可能不一致。**唯一值得加的一句**是任务完成后的约定，
放在 Guidance 末尾（上面已写："notify the parent instead of just ending"）。

> 落地检查：`internal/tools/guidance_test.go` 有一条"每个可见工具都要有 Guidance"
> 的断言（第 39 行）。三个新工具都填了 Guidance，正好满足；但**别为了过测试
> 写空话**——那段测试注释本身就说了"新增工具时别忘了填"。

### 3.10 `internal/uiapp` / `internal/host/remote.go`

- 侧栏徽标（`SessionRef.MailUnread`）语义变成"未读通知数"，字段名不动。
- `Change{Kind: "mail"}` 事件名建议保留（Android 已在监听），只在注释里说明
  "现在的 mail 指 subagent 通知"。
- **不动 `ViewState`**：`RunningTools` / `RoundStartedAt` 不往 wire 上加，
  见 §3.7 第 4 点。
- `/subagent` 命令（§3.8）：走 `setSubagentModel` 命令提交给执行端，
  不要只改界面状态（远程展示端下改不到执行端）。

---

## 4. 投递与唤醒策略（保持现有语义，只换名字）

现有 `wakeForMail` 的三条规则不变，因为它们已经是踩过坑的结论：

1. 运行中（streaming / tools）→ 不唤醒，下一轮请求自带通知。
2. 空闲 → 去抖 3 秒（`wakeDelay`）后唤醒，同一会话冷却 30 秒（`wakeCooldown`）。
3. 等待回答 / 压缩中 → 不唤醒，等回到 Idle。

**注意：没有扇出。** `subagent_notify_child` 一次只投一个目标（用户定的，见 §2），
所以不存在"一次叫起 N 个"的问题，也就不需要扇出上限这类闸门。父会话要通知
多个子会话就多次调用工具——每次的返回值都明确报告"是否唤醒了它"，模型能直接
看到自己花掉了几次唤醒。**`subagent_list` 返回的活动明细是配套的省钱手段**：
父会话可以先看 `busy` / `tool` / `elapsed`，只对空闲的那几个发通知，运行中的
等它自己跑完（通知照样落盘，它下一轮请求会自动看到）。

---

## 5. 兼容与迁移

| 项 | 处理 |
|---|---|
| 旧邮件数据 | 只读兼容（按 §3.1 的候选目录列表），不迁移、不删除 |
| 模型可见工具 | `mailbox_*` 三个**直接消失**。这是有意的破坏性变更：需求就是"对 agent 隐藏" |
| 已写进历史日志的工具调用 | 重放时不再有对应 `ToolDef`。需要确认 `ExecuteAll` 对未知工具的处理是"返回错误结果"而不是 panic（现有注释写的是前者，落地前要跑一遍旧日志） |
| `SessionRef.MailUnread` | 字段名与 wire 格式不变 |
| `tools.MailboxHost` 等公开类型 | 删除（tools 包被谁外部依赖？只有 host 与测试，可控） |
| `new_session` | 名字不变，描述与返回文案更新 |
| 子会话模型（新增） | `SessionRecord` 加两个 `omitempty` 字段，旧档案读出零值 = 跟随父会话，**行为不变** |

---

## 6. 实施顺序（建议的 5 个提交）

1. **store**：`GlobalMailboxDir` + `ChildSessions` + 注释更新。（纯增量，可单测）
2. **mailbox 包**：`OpenAny`（多目录只读兼容）。（纯增量）
3. **host**：`subagent.go`（搬 `mailbox.go` + 去项目限制 + 父子校验 + `activityOf`），
   保留旧 `mailbox.go` 让两边共存一版，`attachMailbox` 与 `attachSubagent` 同时挂着。
   同批做 `Session.runningTool`（含 `RunningTool()` 读取）与 `driver.RoundElapsed()`。
4. **tools**：加 `SubagentHost` 与三个工具（含 Description / Guidance）、
   删 `mailbox_*` 注册、`ToolContext` 换字段、`driver` 的 `SubagentNotice` 改名。
   **不动 `ViewState` / wire 协议。**
5. **清理**：删 `host/mailbox.go` 的旧接口与 `tools.MailboxHost`，
   改测试（`mailbox_test.go` / `mailbox_notice_test.go` / `mailbox_wake_test.go`
   / `remote_fields_test.go` 全部改名到 `subagent_*`），跑全量。

### 需要新增的测试（每条都对应一个真实失败模式）

- `TestSubagentNotifyCrossProject`：两个不同项目的父子能互相通知（**本次需求的核心**）。
- `TestSubagentNotifyRejectsStranger`：同项目的无关会话不能被通知（权限边界没被放宽）。
- `TestSubagentToolsByRole`：子会话的工具列表里**只有** `subagent_notify_parent`
  （没有 `subagent_list` / `subagent_notify_child` / `new_session`）；
  顶层有 `subagent_list` + `subagent_notify_child`、**没有** `subagent_notify_parent`。
  断言的是**列表本身**（同 `ToolsForTest` 的现有做法）。
- `TestSubagentIdentityOnlyInChildPrompt`：`GuidanceSection(childTools)` **含**
  "spawned by another session"，`GuidanceSection(rootTools)` **不含**
  —— 身份提示不能漏到顶层会话。
- `TestSubagentActivityReportsRunningTool`：子会话卡在 `bash` 里时，
  `subagent_list` 返回 `busy=runningTool, tool=bash`，且 `elapsed > 0`。
- `TestSubagentActivityIdleHasNoElapsed`：空闲子会话的 `elapsed == 0`（否则会
  显示成"一直在跑"）。
- `TestSubagentListIncludesArchivedFlag`：归档子会话默认不列出，`includeArchived` 才出现。
- `TestMailboxToolsGone`：`BuiltinWith` 的结果里不存在任何 `mailbox_` 前缀的工具。
- `TestLegacyMailboxDirStillReadable`：旧 `projects/<id>/mailbox/*.jsonl` 的未读仍能读出。
- `TestChildSessionInheritsModelByDefault`：不设 `/subagent` 时，子会话的
  `Selection` / `Effort` 与父会话完全一致（**默认行为不能被新功能改掉**）。
- `TestChildSessionUsesSubagentModel`：设了 `/subagent model X` 后，新建的子会话
  用 X，且档位落到 X 的默认值（不是父会话那个可能不存在的档位）。
- `TestSubagentModelRejectsUnknown`：`/subagent model 不存在的名字` 立即报错并列出
  可选值，**不建出会话**（避免"建了才发现跑不起来"）。
- `TestSubagentModelPersists`：`/subagent model X` 后重启（重读档案），设置还在。

---

## 7. 已拍板的决定（留档，避免以后反复讨论）

1. **不做 `to: "all"` 广播**：`subagent_notify_child` 一次一个目标。
   理由见 §2 —— 一句话叫起 N 个子会话的 token 代价模型往往感觉不到。
2. **`subagent_list` 暴露活动明细**（生成中 / 在跑哪个工具 / 跑了多久 / 排队数）。
   这是父会话决定"现在要不要打扰"的依据，也给"它是不是卡住了"提供证据。
   **范围限定在执行端进程内**：只走 `Session.runningTool` + `driver.RoundElapsed()`，
   不往 `ViewState` / wire 协议上加字段（判定见 §3.7）。
3. **子会话不查父会话状态，没有 `subagent_parent`**（用户定的）。
   理由见 §2：通知目标由 host 按 `parentID` 解析，子会话不需要知道父会话的 id
   或状态；"父在忙"也不该改变子会话的行为。工具集因此是三个而不是四个。
4. **身份提示走工具 Description + Guidance，不写进全局 SystemPrompt，
   也不塞进子会话的初始 prompt**（见 §3.9）。
5. **子会话默认继承父会话的模型与档位**；要换用会话级设置 + `/subagent` 命令，
   由用户在会话里定，`new_session` 工具不收 model 参数（见 §3.8）。

---

## 8. 实施记录（落地时的偏差，值得记住）

1. **`attachMailbox` 还兼管"填 ProjectID"**。换成 `attachSubagent` 时漏了这一步，
   导致所有子会话创建失败（`attachSessionTools` 要求有项目）。教训：那个函数
   注释里写的"这些事必须一起做"是有原因的，拆的时候要逐条对照。
2. **父子的工作区必然相同**（子会话继承父会话工作区），所以"跨项目"在正常路径
   下不会发生。测试改成**抹掉/改写 ProjectID** 来构造真实会出现的跨项目情形
   （项目被移除、会话没登记）。旧代码在这些情形下通讯直接不可用。
3. **测试里模型列表要两处都设**：执行端 `Config.Models`（校验）与界面
   `opts.Providers`（展示）。生产环境由同一个 `providerChoices(registry)` 填，
   不会不一致；测试各自注入就得手动对齐。
4. **`runningTool` 在 `runTools` 里设一次整批名字**（而不是逐个更新）：
   工具串行执行，逐轮更新收益很小，而少一次状态写就少一类竞态。
5. 实施中新出现又删掉的死代码：`tools.SubagentNoticePreview` 与
   `host.toToolMailPreview`（写完发现没有调用者）。

---

## 9. 附带发现（与本次需求无关，但建议顺手修）

- `internal/host/session.go:113-114` 有重复的注释行（`// Archived 报告会话是否已归档。`
  写了两遍）。**已修**。
- `internal/host/childsession.go:103` 的注释引用 `attachMailbox`，
  本方案落地后会失效，需一并改。
- `Driver.roundStartedAt` 与 `Round.startedAt` 两个字段语义重叠
  （一个算总耗时、一个算回合结束行耗时），落地 §3.7 时确认能否合并。