# 远程执行端 / 展示端（relay）设计

> 状态：**评审中**（已按评审意见修订 D1–D4，见文末「修订记录」；用户确认后再开工）
>
> 相关文档：`docs/go-rebuild-plan.md`（实现现状与 §12 决策记录）、
> `docs/persistence-design.md`（JSONL 事件日志）、`docs/ui-redesign-plan.md`（界面）
>
> 本文里标「用户」的是用户已经明确拍板的决定（见 §0.2），标「建议」的是我提出、
> 需要评审/用户确认的点（汇总在 §11）。

---

## 0. 摘要

一次会话的"真相"现在完全在 `driver` 里：`driver.Driver` 持有一个
`conversation.Conversation`，所有状态变化都由它产生 `Effect`（`Record` 写 JSONL、
`StartStream` 开流、`RunTools` 跑工具……），`uiapp` 只负责"把 Effect 执行掉 + 把状态画出来"。
**这个结构正好可以直接搬去做远端**：把 `uiapp` 与 `driver` 之间的那层抽成
`host.Host` 接口，执行端用真实 driver 实现它，展示端用"走连接、转发命令、
接收事件"的实现填同一个接口，界面因此只写一套。

三层结构：

```
        ┌──────────── uiapp（唯一一套界面代码）────────────┐
        │ 读：Sessions / Conversation / Busy / Queued / …  │
        │ 写：Send / Stop / Answer / Compact / SelectModel │
        └───────────────────────┬──────────────────────────┘
                                │ host.Host（接口）
               ┌────────────────┴─────────────────┐
        localHost（进程内直接调用）        remoteHost（走连接）
               │                                  │
        ┌──────┴──────┐                    ┌──────┴───────┐
        │ driver      │                    │ wire.Conn    │
        │ store.Blobs │                    │ （包、分块、  │
        │ tools       │                    │  断线续传）   │
        └─────────────┘                    └──────┬───────┘
                                                  │
                                    ┌─────────────┴──────────────┐
                                    │ 执行端 execHost（同一个      │
                                    │ host.Host 实现，直接持有     │
                                    │ driver + store + tools）     │
                                    └────────────────────────────┘
```

**关键点：单机模式不走网络，但也走同一个接口。** 只有一个界面实现、一份渲染代码、
一份命令语义；远端不是"第二套实现"，而是同一个 `host.Host` 的另一个实例。

---

## 0.1 术语

| 词 | 含义 |
| --- | --- |
| **执行端（executor / agent host）** | 持有 driver、会话日志、blob 存储、工具执行、模型凭据的一端。可以有界面（本地模式）或无界面（`--headless`）。 |
| **展示端（viewer）** | 连接执行端、完整复现它的状态、并且能操作的界面。 |
| **relay** | 只做注册与转发的服务端；早期可以没有（展示端直连执行端）。 |
| **连接（conn）** | 一条按包收发、不丢不重保序的通道。直连、经 relay、或进程内管道都是它。 |
| **持久状态** | 已经写进 JSONL 事件日志的东西（消息、工具结果、提问、模型切换、快照、repairs…）。 |
| **瞬时状态** | 只存在于内存里的东西（流式增量、Busy、插话队列、压缩进度、显示型提示…）。 |

## 0.2 用户已确认的决定

1. **展示端可以操作**：发消息、插话、停止、回答提问、压缩、切换模型/档位、新建/删除/改名会话。
2. **多端可同时发消息，按到达顺序处理**，沿用现有插话语义（空闲→先到的先开一轮；
   忙→进插话队列，在安全边界投递）。插话队列与待回答的提问**所有端可见**；
   停止对所有端生效；同一提问多个端同时回答时**先到的为准**，后到的提示
   「已被其他端回答」。
3. **headless**：执行端可 `--headless` 无界面运行，只作为执行端。
4. **relay 自己做**，早期允许展示端直连执行端；**不依赖 HTTP**（不用 HTTP/WebSocket/长轮询）。
5. **先不做认证，但留口子**：连接可按用途创建（执行端/展示端/以后其它），握手预留凭据字段。
6. **API Key 与模型配置只在执行端**，展示端看到的模型列表来自执行端。
7. **展示端能看到执行端的全部会话**，界面与本地运行一致。
8. **界面状态不同步**：语言、侧栏宽度、折叠、草稿等只存本地。

## 0.3 传输层假设（用户确认）

- 按包发送（类似 UDP），但**不丢包、不重复、保序**；
- **单包有大小上限**（默认假设 64 KiB，可配置）：大块数据必须分块；
- **可以开多条连接**（控制/实时一条、大块数据另一条），避免大文件堵住实时输出；
- 协议层**不关心**底层是直连、经 relay、还是进程内。

> 由此得到一个硬性设计约束：**协议里的一切都必须能装进一个包，或者被分块层切开**。
> 所以下面所有"发一坨东西"的地方都写成"逻辑消息 + 分块层"，从不假设流式读取。

---

## 1. 分层与接口

### 1.1 `internal/wire`：连接抽象（包、角色、握手、多连接）

```go
// Packet 是连接上的一次收发单元。Body 是逻辑消息的 JSON（见 §3/§4/§5）。
type Packet struct {
    // Kind 是消息类型（"hello" / "event" / "cmd" / "blob.chunk" …）。
    Kind string
    // Conn 是逻辑连接 id（一条物理连接上可以复用的多路复用 id，见 §1.4）。
    Conn uint32
    Body []byte
}

type Conn interface {
    Send(Packet) error
    Recv() (Packet, error)
    // Close 关闭连接；对端 Recv 返回错误。
    Close() error
}
```

**方向**：一条连接是双向的（`Send` + `Recv`），角色由握手声明，不由连接方向决定
（展示端连执行端、执行端连 relay、relay 连展示端，都是双向同一条）。

**进程内实现**：`wire.NewPipePair()` 返回两条互相连通的 `Conn`（`chan Packet` 背后），
用来做"单进程里起执行端 + 展示端"的端到端测试；也用于单机模式的可选路径（见 §1.5）。

**分块层**（所有大 payload 共用）：

```go
type Fragmented struct {
    MsgID  string // 逻辑消息 id（同一个消息的所有分块相同）
    Index  int    // 0-based
    Count  int    // 总分块数
    Total  int    // 原始字节数
    SHA256 string // 原始 payload 的 sha256（小写 hex）
    Data   []byte
}
```

- 发送方：`wire.SendLarge(conn, kind, payload, maxPacket)` —— 按 `maxPacket - header` 切片，
  逐包发送；接收方用 `wire.Reassembler` 收齐、校验 sha256、再交给上层。
- `maxPacket` 来自配置（`connection.max_packet_bytes`，默认 `65536`）；
- 校验失败 → 丢弃整条消息并要发送方重发（重发由上层消息级重试做，分块层不自动重传：
  传输层已保证不丢包，校验失败只会是协议 bug 或内存错误）；
- 大消息**优先走 bulk 连接**（§1.4），没有 bulk 连接时退回 control 连接。

**握手**（每条物理连接一次）：

```go
// hello 由**发起方**发出：展示端连执行端时是展示端；执行端连 relay 时是执行端。
type Hello struct {
    Protocol    int    `json:"protocol"`     // 协议版本，当前 1
    Role        string `json:"role"`         // "viewer" / "executor" / "relay-client"（预留）
    Purpose     string `json:"purpose"`      // "control" / "bulk"
    DeviceName  string `json:"device_name"`  // 展示端可读的名字（日志/列表用）
    // Credentials 是**预留**字段：现在恒为 {"kind":"none"}，以后加 token / mTLS 指纹。
    Credentials struct {
        Kind  string `json:"kind"`
        Token string `json:"token,omitempty"`
    } `json:"credentials"`
}

type Welcome struct {
    Protocol   int      `json:"protocol"`
    HostID     string   `json:"host_id"`     // 执行端实例 id（重启会变）
    HostName   string   `json:"host_name"`   // 机器名/自定义名，列表里显示
    Capability []string `json:"capability"`  // 预留："bulk" / "rpc" / …（见 §9）
    // 版本不匹配时这里给原因，连接随即关闭
    Error string `json:"error,omitempty"`
}
```

- 版本不匹配 / 角色不允许 → `Welcome{Error}` + 关闭；
- 心跳：`ping` / `pong`（每 15s 一次，30s 无响应判死）；保活由 wire 层做，上层不感知。

### 1.2 `host.Host`：uiapp 眼里的"一个执行端"

接口按"读"和"写"分两组（读用起来像现有 `Session` 的 getter，写就是命令）。

```go
// Host 是 uiapp 唯一依赖的执行端接口。localHost 与 remoteHost 都实现它。
type Host interface {
    // ---- 身份 / 能力 ----
    Info() HostInfo // 名字、是否本地、执行端版本、是否支持 blob 拉取等

    // ---- 会话（读） ----
    Sessions() []SessionRef                    // 稳定顺序同本地：按 UpdatedAt
    Subscribe(sessionID string, fn func(Change)) // 变化通知（合并成"下一帧请重绘"）
    Conversation(sessionID string) *conversation.Conversation
    Runtime(sessionID string) SessionRuntime   // Busy/Queued/AskGroups/Usage/Counts/Selection/Compacting…
    SessionMeta(sessionID string) SessionMeta  // 标题、工作区、createdAt、keptFromSeq…

    // ---- 会话（写 / 命令）：一律**异步**，立即返回 CmdID ----
    //
    // 为什么不是同步返回 CmdResult：uiapp 的所有调用都在界面主线程上，remoteHost
    // 同步等一次网络往返（慢网络/断线 = 界面卡死）。M4 已经踩过"开流阻塞主线程"
    // 的坑，这里不能再犯。结果统一通过 Subscribe 的 Change{Kind:"cmdResult"} 投递
    // 回界面线程；localHost 也走同样的语义（Post 回主线程），测试才覆盖得到。
    Submit(cmd Command) (CmdID, error)

    // ---- 历史 / 归档 ----
    LoadOlder(sessionID string, cursor *store.ArchiveCursor) (OlderPage, error)

    // ---- blob ----
    Blobs() BlobService  // Put/Open/Has/Stat，远端实现按需拉取 + 本地缓存

    Close() error
}
```

`Submit` 只做三件事：校验参数、生成 `CmdID`、把命令塞进执行端队列。
**它不等待执行**（本地实现也不等待，统一 Post 到主线程后立即返回）。

```go
type Command struct {
    CmdID     string          `json:"cmd_id"`
    SessionID string          `json:"session_id,omitempty"`
    Kind      string          `json:"kind"`        // send / enqueue / stop / answer / …
    Payload   json.RawMessage `json:"payload"`
}

type CmdID string

// 结果以 Change 的形式回到界面线程（与其它状态变化同一条通道）：
type Change struct {
    SessionID string  // 可能是空（例如新建会话）
    Kind      string  // "conversation" / "runtime" / "sessions" / "blobs" / "cmdResult"
    Cmd       CmdID   // 仅 Kind=="cmdResult" 时有值
    Result    CmdResult
}
```

- **读接口全部读本地镜像**（执行端的 driver / 展示端的 mirror），不产生任何网络等待；
  只有 `Submit` 和 `Blobs()`/`LoadOlder` 可能触发异步请求。
- `SessionRuntime` 是把现在 `uiapp.Session` 里"来自 driver 的只读状态"打包成一个值：
  `Busy driver.Busy`、`Queued []string`、`Asks []*driver.AskGroup`、`Usage driver.Usage`、
  `Counts driver.Counts`、`Selection providers.Selection`、`Effort string`、
  `Compacting bool`、`CanSteer bool`、`Vision bool`、`ContextWindow int`、`TokensInUse int`。
- `CmdResult{OK bool, Error string, Notice string, Data any}`：
  失败原因要能原样显示（"已被其他端回答"就是 `Notice`）。
- `Change` 是**粗粒度**通知（`{SessionID, Kind}`：`conversation` / `runtime` / `sessions` / `blobs`），
  界面下一帧整体重绘——与现在"`Changed` effect 只是提示重绘"的做法一致，不搞细粒度订阅。

### 1.3 `host/localHost`：单机模式的唯一实现

`localHost` 直接持有现有对象，**没有任何序列化**：

```go
type localHost struct {
    sessions []*Session   // 就是现在 uiapp.Session 去掉界面字段后的东西
    catalog  *store.Catalog
    blobs    *store.Blobs
    rt       Runtime      // 现有的 OpenStream / RunTools
    ...
}
```

它内部执行 `driver` 产生的 `Effect`（`Record` 写日志、`StartStream` 开流、
`RunTools` 跑工具），也就是现在 `uiapp.applyEffects` 的搬家版。

**这一步（P1）只搬代码、不改行为**：所有现有测试（`internal/uiapp` 约 2000 行断言）
必须原样通过，`scripts/check.ps1` 全绿。

### 1.4 多条连接与多路复用

- 每条物理连接声明 `Purpose`：
  - `control`：握手、命令、事件、状态更新（小包、要求低延迟）；
  - `bulk`：blob 分块、归档分页、初次全量快照（大包、允许排队）。
- 一条物理连接上一个包里带 `Conn uint32` 做逻辑通道。**不用 `sessionID` 的哈希**：
  哈希会碰撞，两个会话撞到同一个通道就会互相串包。改成**握手之后由接受方分配的自增 id**
  （`welcome` 里附一个起始值，之后 `openConn`/`closeConn` 分配/回收），
  与 `sessionID` 的映射由接收方维护（`map[Conn]sessionID`）。
  这样"多会话同时流式"不会互相阻塞（分块层也用它做 reassembler key）。
- 展示端默认开 2 条连接（control + bulk）；执行端可限制每端连接数
  （`connection.max_conns_per_viewer`，默认 4，超过拒绝握手）。

### 1.5 单机模式为什么不直接调 driver

两种方案：

| 方案 | 做法 | 取舍 |
| --- | --- | --- |
| **A（建议）** | uiapp 只依赖 `host.Host`；单机用 `localHost`（进程内直接调用） | 一份界面代码；远端与本地共用命令语义与状态机；测试可在同一进程里起两个 host 对比 |
| B | 单机也走 `wire.PipePair`（序列化一遍） | 能顺便验证序列化，但每帧都有 JSON 编解码开销，且调试栈变长 |

**建议 A**，但把 `wire` 的序列化路径用在测试里：`TestRemoteMatchesLocal` 用 B 的方式跑一遍
"同一串操作"，断言两端 `Conversation` 与界面文本完全一致（§10）。

---

### 1.6 线程模型（硬性约束）

- **执行端**：命令与流事件都在同一个 event loop 上执行（就是现在 `Options.Post`
  那一套单线程模型）。工具执行、blob 写盘仍在 goroutine 里，结果 Post 回主线程。
- **展示端**：`wire.Conn.Recv` 跑在独立的网络 goroutine 里，但**它只做解码和排队**：
  所有对 mirror（Conversation / Runtime / 会话列表）的修改都必须 `Post` 到界面线程，
  在那里统一应用。原因：`conversation.Conversation` 不是并发安全的，而且界面每帧都在读它。
- 网络 goroutine 允许做的事：解码包、组装分块、写 blob 缓存（文件 IO）、更新
  "网络状态"（连接中/已断开）这些不影响 mirror 的字段。

### 1.7 命令是异步的，界面怎么用

```go
cmdID, err := a.host.Submit(host.Command{
    SessionID: id, Kind: "send",
    Payload: mustJSON(host.SendPayload{Text: text, Attachments: refs}),
})
if err != nil { /* 参数错误/队列满：立即提示 */ }
// 成功提交只代表"排队了"；真正的结果随后以 Change{Kind:"cmdResult"} 回来：
//   OK=false + Notice="已被其他端回答"  → Toast
//   OK=false + Error="..."              → 会话内错误行（与本地一致）
```

界面上的"发送失败"提示因此比现在**晚一帧**（命令先在队列里跑），这是可接受的；
本地模式与远程模式表现一致，不会出现"本地同步、远程异步"的双重语义。

## 2. uiapp 的重构方案

### 2.1 现状盘点

`internal/uiapp` 现在直接碰 driver 的地方：**约 81 处**（不含测试）。按 API 计数：

| 调用 | 次数 | 归类 |
| --- | --- | --- |
| `Busy` | 10 | 读（Runtime） |
| `CanSteer` / `HasQueued` / `Queued` | 4 / 3 / 1 | 读（Runtime） |
| `Compacting` | 3 | 读（Runtime） |
| `Vision` / `SetVision` | 3 / 2 | 读写（模型能力，随 Selection 同步） |
| `Selection` / `SetSelection` / `SetReasoningEffort` | 2 / 1 / 2 | 读写（模型选择） |
| `TakeRecords` | 2 | 写（把 Record effect 落到日志，属于 host 内部职责） |
| `Rename` | 2 | 写（命令） |
| `TokensInUse` / `EffectiveContextWindow` / `SetContextWindow` / `SetMaxOutputTokens` | 各 1 | 读（Runtime）/ 写（host 内部，随 Selection 同步） |
| `SubmitAsks` / `Stop` / `Enqueue` / `SendWithAttachments` | 各 1 | 写（命令） |

另外 `session.drv.Conversation()`、`session.drv.ApplyEvent`、`ToolResult`、`ToolBatchDone`、
`ApplyCompact` 等出现在 `applyEffects`/`startStream`/`runTools` 里，这些**整体搬进 host**，
uiapp 不再接触（它们属于"执行端内部如何推进状态"，展示端不该看到）。

### 2.2 拆分后的 `uiapp.Session`

`uiapp.Session` 只保留**界面状态**（本地、不同步）：

```go
type Session struct {
    ID string

    // 以下都来自 host（读接口/订阅），不再持有 driver
    // conversation 通过 host.Conversation(id) 取

    // 纯界面状态
    list        ui.ListState // 滚动
    expanded    map[string]bool // 展开的过程组/工具结果
    askTab      int
    askDrafts   map[string]*string
    renaming    bool
    renameDraft string
    draft       string       // 输入框草稿
    sidebarCollapsed bool    // 全局，实际在 App 上
    ...
}
```

`App` 增加 `host host.Host` 字段；`a.Current()` 返回 `*Session`（界面状态）+
`host.Conversation(id)`。所有 `session.drv.X()` 改写成 `a.host.Runtime(id).X` 或
`a.host.<Cmd>(...)`。

### 2.3 界面侧要改的点（逐项）

| 现在 | 改成 | 说明 |
| --- | --- | --- |
| `session.drv.Busy()` | `a.host.Runtime(id).Busy` | 每帧取一次即可（现在也是每帧多次，改成帧内缓存） |
| `session.drv.Conversation()` | `a.host.Conversation(id)` | 只读 |
| `a.applyEffects(session, effects)` | 删除（搬进 host） | 展示端没有 effect |
| 各处直接调 `driver.Send/Stop/Answer…` | `a.host.Submit(host.Command{…})` | **异步**：结果经 `Change{Kind:"cmdResult"}` 回界面线程（§1.7） |
| `a.startStream` / `a.runTools` / `a.startSummary` | 搬进 `host/localHost` | 执行端职责 |
| `session.journal.Sync()` | `host` 内部 | uiapp 不再碰日志 |
| `session.askTab/askDrafts` | 保留（本地界面状态） | 但 `AskGroups` 来自 host |
| `NotifyDisplay` | `host.NotifyDisplay(sessionID, text)` | 显示型提示要同步到所有端（见 §3.4） |
| 归档 `loadOlder` | `host.LoadOlder` | 远程走 `log.older` 消息 |
| 图片读取（`a.opts.Blobs`） | `host.Blobs()` | 远程按需拉取 + 本地缓存 |
| 语言/侧栏宽度/折叠/草稿 | **不变** | 本地状态（用户决定 8） |

**改动量估算**：`internal/uiapp` 里真正要动的文件：`app.go`（约 2600 行，改 ~250 处引用，
新增 ~200 行适配）、`attachments.go`（blob 走 host，~40 行）、`images.go`（改 blob 读取入口，~30 行）、
`turnview.go`/`layout.go`/`composer*.go`（只改 `session.drv.X` 的调用点，~40 处）。
新增：`internal/host` 约 1200 行（local + 接口 + 事件应用），`internal/wire` 约 800 行，
`internal/remote`（remoteHost + 协议编解码 + reassembly 使用）约 900 行。

**渲染代码零改动**是这次重构的验收标准之一：`turnview.go` / `markdown.go` /
`process_activity.go` 里只应改"取数据的地方"，不改渲染逻辑。

---

## 3. 状态同步

### 3.1 原则

1. **持久状态永远来自 JSONL 事件**，不发明第二套"状态快照"格式——重放逻辑只有一份
   （`driver.Replay`），所以"展示端看到的东西"与"执行端重启后看到的"天然一致。
2. **瞬时状态走独立的 delta/snapshot 消息**，不写日志、不参与重放。
3. **展示端不做推断**：执行端说 "busy=runningTools"，展示端就显示 runningTools，
   不自己根据事件推测。

### 3.2 持久状态：日志快照 + 增量

执行端每个会话的 `store.SessionStore` 已经能 `Load()`（最新文件全部事件）与
`LoadOlder(cursor)`（归档分页）。展示端接上时的流程：

```
viewer                                   executor
  │  attach{sessionID, cursor{generation, seq}?}  ──▶
  │                                        │ cursor 的 generation 与当前一致、且 ring 覆盖 → 发增量
  │                                        │ 否则 → 从新 generation 的快照起全量
  │  ◀──── log.begin{sessionID, epoch, generation, fromSeq, keptFromSeq, truncated}
  │  ◀──── log.events{sessionID, generation, events[]}   （分块，bulk 连接）
  │  ◀──── log.end{sessionID, generation, toSeq}
  │  ◀──── state.snapshot{sessionID, …}      （瞬时状态，见 §3.3）
```

**游标是 `(generation, seq)`，不能只有 seq**：`store` 在压缩滚动时会把快照的 seq 重置为 0，
新文件里的后续事件从 1 开始（`jsonl.go` 的 Compact 路径）。如果展示端记的是
`lastSeq=500`，而执行端这期间压过一次，新文件里 `seq=3` 就会被误判成"已收到"（漏事件）
或"有缺口"（无谓全量）。

- `generation` = 快照事件的 `archived_as`（没有快照时为 `0`，即首次创建）；
  展示端**按 generation 比较**：不同 → 这个会话的持久状态从新 generation 的快照开始重建
  （全量，`truncated=true` 视 ring 而定），归档部分仍可用 `loadOlder` 按需拿；
- **全量同步的游标要用 `eventSeq`（协议序号），不是日志文件的 seq**：
  全量事件只用来重建状态、**不推进游标**；`log.end` 带 `to_seq`（生成这批全量事件时的
  `eventSeq`），展示端据此一次性落定游标。否则"从磁盘恢复了一个有 300 条事件的会话"会让
  游标停在 300，而 ring 里新事件的序号只有 1..k，重连时被误判成"没有缺口"。
- `epoch` 是执行端本次运行的随机 id（重启会变，与 generation 无关）：
  `epoch` 变了 → 展示端清空所有本地镜像，全部重新 attach；游标是
  **`(epoch, generation, seq)`**，`attach` 时带上 epoch，不一致就一律全量
  （重启后 `eventSeq` 从 1 重新开始，只比 seq 会把新进程的 1..k 当成已收到）；
- **内存后端**（执行端没有配置 `data_dir`）给不出完整历史：`attach` 只能返回瞬时快照 +
  之后的增量，长断线后重建不了历史。远程模式建议执行端使用 JSONL 日志。
- **断线续传**：展示端记录每会话 `(generation, seq)`。重连时 `attach{cursor}`：
  执行端从内存 ring（每会话最近 **2000 条事件 / 4 MB**，先到先限）取出
  `seq > cursor.seq` 的事件；ring 覆盖不到（断线太久）→ `truncated=true`，改发全量
  （分块，走 bulk）。**不需要全量重来就能续上**是硬性要求；
- 增量续传后，`state.snapshot` 里的**瞬时序号**同样带 generation（见 §3.3）：
  两者必须来自同一个 generation，否则会出现"持久状态是新的、瞬时状态是旧的"。
- 展示端把收到的事件喂给 `driver.Replay` 的**增量版**（P2 增加
  `driver.ReplayAppend(state, events) (Replayed, applied int)`：把 `Replay` 里
  "按事件类型 switch"的那段抽成可复用的 `applyEvent`，`Replay` 与增量续传共用同一份代码，
  避免两套重放逻辑）；
- `repairs`（`Replayed.Repairs`）**只在执行端算**并作为普通事件发出去，展示端不修日志。

**为什么不直接发"状态快照"**：会话内容里有大量结构（消息、part、状态、失败原因、
快照摘要、keptFromSeq、归档游标），自己定义一份快照格式等于把 `Replay` 复制一遍，
一旦两边不一致就是"展示端和本地看到的不一样"——这正是需求里最不能出的问题。
日志事件已经是为持久化设计过的完整表示，直接复用最省事也最可靠。

### 3.3 瞬时状态：`state.snapshot` + `state.delta`

```go
type StateSnapshot struct {
    SessionID  string `json:"session_id"`
    Epoch      string `json:"epoch"`
    // Generation + Seq 是**瞬时**状态自己的游标：与 stream 增量共用同一个计数，
    // 展示端用 Seq 把"快照里的流式内容"与"随后的增量"接起来（见 §3.3.1）。
    Generation uint64 `json:"generation"`
    Seq        uint64 `json:"seq"`
    Busy       string `json:"busy"`    // "idle" / "running" / "runningTools" / "compacting" / "awaitingAnswer"
    Queued     []string `json:"queued"`
    Asks       []AskView `json:"asks"`
    Usage      UsageView `json:"usage"`
    Counts     CountsView `json:"counts"`
    Selection  SelectionView `json:"selection"`  // provider/model/effort/vision/window
    Compacting bool `json:"compacting"`

    // Inflight 是**正在流式输出**的消息的当前完整内容（parts 全量，深拷贝）。
    Inflight []InflightMessage `json:"inflight,omitempty"`
    // Notices 是本会话已经显示过的**显示型**提示（NotifyDisplay，不落盘）。
    Notices []Notice `json:"notices,omitempty"`
}

type InflightMessage struct {
    MessageID string           `json:"message_id"`
    Parts     []message.Part   `json:"parts"`   // 已输出的文字/推理/tool_use（含 raw_args）
    Status    string           `json:"status"`  // "streaming"
}

type StateDelta struct {
    SessionID string `json:"session_id"`
    Generation uint64 `json:"generation"`
    Seq       uint64 `json:"seq"`
    // 只发变化的部分：字段为指针/nil 表示"没变"
    Busy      *string `json:"busy,omitempty"`
    Queued    *[]string `json:"queued,omitempty"`
    Asks      *[]AskView `json:"asks,omitempty"`
    Usage     *UsageView `json:"usage,omitempty"`
    Counts    *CountsView `json:"counts,omitempty"`
    Selection *SelectionView `json:"selection,omitempty"`
}
```

#### 3.3.1 中途连上/重连：必须补"已经输出的那一半"

流式输出的文字/推理/工具参数**不写日志**：日志里只有 `message.appended{streaming}`
这个空壳。如果展示端在模型输出到一半时连上来，只做日志同步就只拿到空壳，
之后的增量从"现在"开始接——**之前已经流出的内容就永久丢了**，这不是"完整复现"。
断线重连也一样（ring 里只有持久事件，断线期间的增量同样不在其中）。

因此 `state.snapshot` 携带 `Inflight`（每个正在流式输出的消息的**当前完整 parts**，深拷贝）
与 `Notices`（已有的显示型提示）。展示端的处理顺序固定为：

1. 先按日志重建持久状态（此时消息是空壳，`status=streaming`）；
2. 应用 `state.snapshot`：对每个 `Inflight`，用 `ReplaceInflight(messageID, parts, status)`
   **替换**对应消息的 parts（没有就新建）；`Notices` 按 `Notice.ID` 去重后补上
   （执行端有、本地没有的才加）；记录 `snapshot.Seq`；
3. 应用 `stream` 增量时**只接受 `seq > snapshot.Seq`** 的包（衔接靠同一个 `Seq` 计数），
   避免把"快照里已有的内容"再加一遍。

**接管时还要恢复"这一轮的增量元数据"**（评审 D2 的延伸）：仅仅拿到 parts 不够，
镜像必须在流的中途继续把增量写回**同一个** part：

- 推理 part 的下标（`reasoningPart`）；
- 每个工具 part 对应的 **provider 下标**——Anthropic 的 `Index` 是 content block 下标，
  **不是"第几个工具"**（前面有一个 text 块时，第一个工具就是 `Index=1`）；按出现顺序
  重新编号会让接管后的增量落到一个新 part 上，界面上出现两张工具卡片；
- 每个工具的缓冲区（`id`、`name`、已收到的参数前缀）——空缓冲区会让接管后的第一个
  参数增量用空 id/name 覆盖整张卡片；参数按 `clipRawArgs` 的**前缀**语义恢复即可
  （part 里存的就是截断后的前缀，接着追加再截一次结果相同）。

做法：执行端写 `tool_use` part 时把 provider 下标写进 `data["stream_index"]`
（Anthropic 的 block 下标 / OpenAI 的 tool 下标），镜像接管时按它恢复 `toolParts`，
并从 part 的 `id`/`name`/`raw_args` 预填缓冲区。镜像侧的 `ReplaceInflight` 会丢弃旧的
Turn，下一次增量到来时按新 parts 重建。

`Inflight` 为什么不在 ring 里保存增量：一次长回答的增量可能有上千条、几十 KB，
全部保留会挤掉持久事件的续传窗口；而"当前完整内容"只需要一次拷贝（同一份 parts），
成本是 O(消息长度) 且只在 attach/重连时发生。

`Notices` 同理：显示型提示不落盘（设计如此，避免"每次加载都多一条"），
但后连上来的端也应该看到它，所以放进 snapshot 而不是日志。

**流式增量**：执行端把 driver 收到的 `providers.Event` 连同 `messageID` **原样**转发：

```go
type StreamDelta struct {
    SessionID string `json:"session_id"`
    Generation uint64 `json:"generation"`
    Seq       uint64 `json:"seq"`      // 与 state.snapshot.Seq 同一个计数
    MessageID string `json:"message_id"`
    Event     providers.Event `json:"event"`   // 复用现有结构（含 Delta/RawArgs）
}
```

**只转发增量类事件**，`EventDone` / `EventError` **不转发**：终态完全由持久事件
（`message.finalized` / `message.status`）与 `state.delta(busy)` 表达，避免两处各说一遍、
也避免实现者误以为展示端要自己收尾。

mirror 只调用 `ApplyStreamDelta`（§3.3.2），它处理的是"模型已经说出来的内容"：
文字、推理、`tool_use` 的 name 与 `raw_args` 增量。这样"流式文字/推理/tool args 的显隐规则"
与执行端完全一致（包括 `raw_args` 增量、"最后一个 tool 参数还没到"这类细节）——
转发**加工过的 UI 字符串**会让两套实现很快分叉。

事件类型对照：

| provider 事件 | 执行端 | 转发 `stream`？ | mirror 做什么 |
| --- | --- | --- | --- |
| `EventText` | `ApplyStreamDelta` | ✅ | 追加文字 |
| `EventReasoning` | `ApplyStreamDelta` | ✅ | 追加推理 |
| `EventToolStart` / `EventToolArgs` | `ApplyStreamDelta` | ✅ | 写 tool_use 的 name / raw_args |
| `EventUsage`（若中间上报） | 只改用量 | ❌（走 `state.delta(usage)`） | — |
| `EventDone` / `EventError` | `FinishStream` | ❌（走持久事件 + `state.delta`） | — |

#### 3.3.2 mirror 只能应用"纯增量"，回合推进必须留给执行端

**不能用 `ApplyEvent` 做镜像**。`ApplyEvent` 在 `EventDone` / `EventError` 时会走
`finishStream` / `failStream`，直接改 Conversation：
`EndStream`/`FailStream`（改消息状态）、`SettleUnfinishedToolCalls`（补写错误结果）、
`AddNotice`、非法参数时写错误结果、`deliverQueued`（用**新生成的随机 id**追加 user 消息）、
`autoCompactEffects`（改 busy）、`recordUsage`（改用量与计数）……

执行端做完这些事会**再发出持久事件**（`message.finalized`、`tool.results`、`notice`、
新 user 消息的 `message.appended`、`selection.changed`…）。展示端如果也跑一遍同样的逻辑，
就会出现两条 id 不同的 user 消息、重复的工具结果、翻倍的通知——**丢掉 effect 拦不住**，
因为问题出在"直接改 Conversation"，不是 effect。

所以 P1 就把 driver 的 provider 事件处理拆成两半：

```go
// 纯增量：只把"模型已经说出来的内容"写进 part，其它什么都不做。
//  - EventText / EventReasoning  → 追加到对应 part（含状态 Pending→Streaming）
//  - EventToolStart / EventToolArgs → 写 tool_use part 的 name / raw_args（含增量）
//  - 不碰 Busy、不补结果、不加 notice、不写日志、不投递插话
func (d *Driver) ApplyStreamDelta(messageID string, ev providers.Event) error

// 回合推进：Done/Error 之后的全部动作（结束消息、补结果、投递插话、自动压缩、
// 计 usage…）。执行端专用。
func (d *Driver) FinishStream(messageID string, ev providers.Event) ([]Effect, error)
```

`ApplyEvent` 保留为两者的组合（执行端照旧调用它；内部改成
`ApplyStreamDelta` + 仅在终态时 `FinishStream`），因此执行端的现有行为与测试不变。

**mirror**（展示端）只调用 `ApplyStreamDelta`；消息结束、工具结果、通知、新消息、
Busy 状态**一律只来自执行端的持久事件与 `state.delta`**，具体是：

| 展示端看到的东西 | 来源 |
| --- | --- |
| 流式文字 / 推理 / 工具参数 | `stream` 包 → `ApplyStreamDelta` |
| 消息结束（status、失败原因、tokens） | 持久事件 `message.finalized` / `message.status` |
| 工具结果（含图片引用） | 持久事件 `tool.results` |
| 插话产生的 user 消息 | 持久事件 `message.appended`（执行端生成 id） |
| 显示型提示 | `state.snapshot.Notices` + `notice` 包（**不**由 mirror 自己生成） |
| Busy / 队列 / 提问 / 用量 / 选择 | `state.snapshot` + `state.delta` |
| 压缩进度 | `state.delta`（`busy=compacting`）+ 持久事件 `compact.snapshot` |

持久事件在展示端用 `driver.ApplyPersisted(event)` 应用——复用 `replay.go` 里已有的
`appendToolResults` / `settleAsk` / 计数等函数（与 `ReplayAppend` 同一份代码），
**不重跑任何 driver 决策逻辑**。

P1 的验收测试：喂同一串 provider 事件给"执行端的增量部分"和"mirror"，
两者的 Conversation 在**每个事件之后**都深相等（`TestStreamDeltaMatchesExecutor`）；
并断言 mirror 在 `EventDone` 之后没有新增 notice、没有多出 user 消息、没有改 Busy。

### 3.4 显示型提示（Notice）也要同步

`NotifyDisplay`（不写日志的一次性提示：图片被替换、模型不支持图片、窗口未知…）
现在直接写 Conversation。远端要能显示，所以 host 接口里加：

```go
type Notice struct {
    // ID 由执行端生成、**会话内唯一**（单调计数即可）。展示端按 ID 去重：
    // 没有它，每次重连合并 snapshot.Notices 都会多出一条同样的提示。
    ID    string `json:"id"`
    Level string `json:"level"`  // info / warn
    Text  string `json:"text"`
}
```

- 执行端：`host.NotifyDisplay(sessionID, notice)` → 写本地 Conversation + 广播 `notice{sessionID, level, text}`；
- 展示端：收到后写自己的镜像 Conversation（不写日志，与本地语义一致）。

### 3.5 顺序与一致性

- 同一个会话的持久事件、瞬时 delta 与 stream 增量共用 **一个发送队列**（每会话一条 FIFO），
  保证"先看到 text 增量、后看到 finalized"；
- 不同会话之间可以并发（各自的逻辑 `Conn`）；
- **两套序号各自独立、各自检测缺口**：
  - 持久：`(generation, seq)`（日志 seq）；
  - 瞬时：`(generation, state.seq)`（`state.snapshot` 与 `stream` 共用同一个计数，
    见 §3.3.1）；
  任一序号出现缺口 → 展示端丢弃该会话的镜像，重新 `attach{cursor}`（宁可贵一次，
  不要显示错状态）；`generation` 不一致 → 直接全量重建。

---

## 4. 命令

### 4.1 消息形状

```go
type Command struct {
    CmdID     string `json:"cmd_id"`      // 展示端生成（uuid），用于对回执
    SessionID string `json:"session_id,omitempty"`
    Kind      string `json:"kind"`        // "send" / "enqueue" / "stop" / "answer" / …
    Payload   json.RawMessage `json:"payload"`
    // 展示端时间戳，仅用于日志/调试，执行端**不**据此排序
}

type CommandResult struct {
    CmdID  string `json:"cmd_id"`
    OK     bool   `json:"ok"`
    Error  string `json:"error,omitempty"`
    Notice string `json:"notice,omitempty"` // 例如「已被其他端回答」
}
```

### 4.2 每种界面操作对应的命令

| 界面操作 | 命令 | 执行端做什么 | 其它端看到什么 |
| --- | --- | --- | --- |
| 发送（文字 + 图片） | `send{text, attachments[]{blob, media_type, width, height}}` | `driver.Send` / `SendWithAttachments`（图片用 blob 引用） | `event`s + `stream` 增量 + `state.delta(busy)` |
| 插话 | `enqueue{text}` | `driver.Enqueue` | `state.delta(queued)` |
| 删除队列项 | `dropQueued{index}` | `driver.DropQueued` | `state.delta(queued)` |
| 停止 | `stop{}` | `driver.Stop` | `state.delta(busy)` + `notice`（"已被其他端停止"不提示给发起端） |
| 回答提问 | `answer{messageID, callID, selections[][], customs[]}` | `driver.SubmitAsks` | `state.delta(asks)` + 事件 |
| 关闭提问 | `dismissAsk{messageID, callID}` | `driver.DismissAsks` | 同上 |
| 压缩 | `compact{}` | `driver.BeginCompact` + 开流摘要 | `state.delta(busy=compacting)` |
| 取消压缩 | `cancelCompact{}` | `driver.CancelCompact` | 同上 |
| 切模型/档位 | `selectModel{provider, model, effort}` | `driver.SetSelection` + `syncModelOptions` | `state.delta(selection)` |
| 新建会话 | `newSession{workspace, title}` | `catalog.Create` + 新 driver | `session.created` 广播 |
| 重命名 | `rename{title}` | `driver.Rename` | 事件 + 列表刷新 |
| 删除会话 | `deleteSession{}` | 与本地同一条路径（含 JSONL 目录删除） | `session.deleted` 广播（**展示端必须丢弃该会话的本地状态**） |
| 加载更早历史 | `loadOlder{afterCursor}` | `journal.LoadOlder` | `log.older` 分块回包 |
| 拉 blob | `blob.get{hash}` | 读 `store.Blobs` | `blob.chunk`s |
| 推 blob | `blob.put{hash, size}` → `blob.chunk`s → `blob.commit{hash}` | 校验 sha256 后 `Put` | 无需广播（引用随消息出现） |

### 4.3 排队与"后到被拒"

- 执行端给**每个会话**一个命令队列（goroutine + channel），严格 FIFO：
  "先到的先处理"就天然成立，也天然满足"空闲时先到的先开一轮"（`driver.Send` 在
  `Busy != Idle` 时返回 `ErrBusy`，执行端把**纯文字**的 `send` 在忙时**自动转成 `enqueue`**，
  与本地界面"忙时自动插话"的行为一致）。
- **带附件的 `send` 在忙时不转插话**：插话队列只存文字（现在的 `driver.queued []string`
  就是这么定的），图片无处可放。执行端直接回
  `CmdResult{OK:false, Error:"插话暂不支持附件，请等这一轮结束再发送"}`，
  与本地界面"附件 + 忙 → 发送按钮禁用"的语义一致（本地也不会把附件塞进队列）。
- **提问竞争**：`answer` 到达时若该 `callID` 已被回答 → 回
  `CommandResult{OK:false, Notice:"已被其他端回答"}`；**不改**界面状态以外的任何东西。
  展示端把这条 Notice 显示成一次性提示（Toast）。
- **停止**：幂等；`stop` 在 idle 时返回 OK 但无副作用。
- 所有命令都在执行端**主线程语义**上执行（`localHost` 用现有 `Post` 单线程模型，
  `execHost` 用同一把互斥/同一个 event loop），因此命令与流事件天然串行，
  不存在"命令与事件交错"的数据竞争。

---

## 5. 大数据

### 5.1 blob 双向传

- 展示端**显示图片**：`renderBlobImage` 现在直接 `blobs.Open(hash)`。改成
  `host.Blobs().Open(hash)`：
  1. 本地缓存（展示端也有 `<data_dir>/blobs`，只读缓存）命中 → 直接用；
  2. 未命中 → `blob.get{hash}` → 分块（bulk 连接）→ 校验 sha256 → 写本地缓存 → 返回字节；
  3. 执行端没有这个 blob（被清理）→ `blob.error{hash, "missing"}` → 界面显示「[图片丢失]」。
- **缓存上限**做成配置：`viewer.blob_cache_mb`（默认 **512**，LRU 淘汰；
  0 表示不缓存、每次现拉）。缓存目录与执行端一样用内容寻址结构，
  但**只读**：展示端不 `Put` 本地文件，缓存条目可随时被淘汰。
- 展示端**粘贴/拖入图片**：现在 `attachImages` 直接 `blobs.Put`。改成先算 sha256、
  `host.Blobs().Put(data)`：
  - 远端实现：`blob.put{hash,size}` + 分块 + `blob.commit`；执行端校验通过后入库并回 OK；
  - 上传完成前附件保持 `attachPending`（已有状态机），失败就是 `attachFailed`。
- **sha256 已经是内容寻址的天然校验**：`store.Blobs.Put` 会按内容算 hash，
  所以"传输校验"与"存储去重"是同一件事，不需要额外的校验和字段。

### 5.2 归档按需加载

`log.older{sessionID, cursor}` → 执行端 `journal.LoadOlder(cursor)` →
`log.olderPage{sessionID, entries[], nextCursor, done}`（分块，bulk）。
现有 `archivedEntry`（带 status/reason/createdAt）原样传，展示端渲染逻辑不变。

### 5.3 初次同步的大快照

会话很长时 `log.events` 可能有几 MB：走 bulk 连接分块（`maxPacket` 64 KiB → 一个 5 MB
的日志约 80 个包）。分块时**按事件边界**切（一个事件不跨块）：
`log.events` 的 payload 就是 `{"events":[...]}` 的子集，保证单事件最大不超过一个包
——超过的情况（单条工具输出 > 64 KiB）走 `log.event.large{sessionID, seq}` + 分块承载
单事件的 JSON。

---

## 6. 展示端本地的行为

| 功能 | 展示端的处理 |
| --- | --- |
| 工具执行 | **必须在执行端**（执行端的工作区与权限）。展示端不跑任何工具。 |
| 文件对话框（`+` 附件） | 本地选择 → 读字节 → 算 sha256 → `blob.put` 上传 → 以 blob 引用发送 |
| 拖放 | 同上（拖入的是展示端本地文件） |
| 剪贴板粘贴 | 同上（位图或文件都上传） |
| 图片显示 | 从执行端按需拉取 + 本地缓存（§5.1） |
| 语言 / 侧栏宽度 / 折叠 / 草稿 / 滚动 | 完全本地（用户决定 8） |
| 单实例锁 | **展示端不加锁**（可以同时开多个展示端连同一个执行端）；执行端保留锁，`--headless` 时按端口占用判断 |
| 数据目录 | 展示端仍需要本地目录，只用于 blob 缓存与界面偏好；不存会话日志 |
| 模型选择器 | 列表来自执行端（`Welcome` 之后 `catalog` 消息带全部 provider/model/effort/vision/window） |
| 状态条 / 用量 | 来自 `state.snapshot` / `state.delta`（不是本地估算） |
| 自动压缩 | 只在执行端触发；展示端只显示进度 |
| 重启后重连 | 记住最近一次 `host:port`（本地配置），启动时自动重连；断线显示"重连中…"横幅 |

**离线时的界面**：断线后展示端进入只读"最后已知状态"，输入卡禁用并显示「已断开，正在重连…」，
3s 重试（指数退避到 30s）。这是新增的界面状态，需要 1 个新 locale 键组。

**断线期间的草稿**：输入框内容属于本地界面状态（用户决定 8），断线**不清空、不自动发送**；
重连成功后草稿原样保留，需要用户自己再按一次发送。附件清单同理：断线时"待发送附件"
留在本地（已上传的 blob 引用继续有效），重连后可以接着发；上传到一半的附件标记为失败并允许重试。

---

## 7. relay

### 7.1 早期：直连

- 执行端：`--listen 127.0.0.1:7801`（默认只监听回环；`--listen 0.0.0.0:7801` 需显式）；
- 展示端：`--connect host:port`；
- 这就是 §1 的 `wire.Conn`（TCP 之上跑"包 + 分块"，不引入 HTTP）。

### 7.2 relay 形态

```
viewer ──wire──▶ relay ──wire──▶ executor
```

- **relay 只做三件事**：执行端注册（`Hello{Role:"executor"}` → 记录 hostID/名字/能力）、
  给展示端列出在线执行端（`relay.list` → `[{hostID, name, sessions? 不暴露}]`）、
  按 `hostID` 转发包（`relay.route{hostID}` 之后，后续包带 `Route: hostID` 原样透传）。
- relay **不解析**会话协议（不认识 `event`/`cmd`），只做帧转发 + 心跳 + 连接数限制；
  这样协议演进不需要同步改 relay。
- 展示端切换：`wire.Dial(addr, purpose, route)`，`route` 为空即直连。
  **上层 `remoteHost` 完全不变**——这就是"早期直连、以后换 relay 不改上层"。

---

## 8. headless

- 启动：`simple-agent --headless --listen 127.0.0.1:7801 [--data-dir …]`；
- 不创建 `mygo` 应用、不建窗口；直接构造 `execHost`（`localHost` 去掉 UI 部分）；
- 生命周期：
  - 收到 SIGINT/SIGTERM → 停止在跑的回合（`driver.Stop`）→ `journal.Sync()` → 关闭监听 → 退出；
  - 标准输出/错误打日志（`log`），每回合结束打一行摘要（session/耗时/是否成功）；
  - 退出码 0 正常、1 启动失败（端口占用、数据目录不可写）；
- 单实例：`--headless` 用"监听端口"代替文件锁（同一端口只能有一个进程）；
  有界面的执行端同样可以 `--listen`（本地用 + 远程展示端同时可用）；
- 无界面时的提示（`NotifyDisplay`）只写进会话（远程端能看到），不丢。

---

## 9. 安全的预留

- **relay 固定 token（已实现）**：`simple-agent relay --listen ADDR --token 口令`。
  执行端注册（`relay.register`）与展示端路由（`relay.route`）都要带同一个 token，
  中间的 `relay.list` 也要带。展示端的 hello 探测用 `wire.Hello.Credentials.Token`
  携带，relay 在回执行端列表**之前**校验（否则未认证的人发一条 hello 就能拿到列表）。
- **token 的边界（用户决定）**：它只保护 relay 这一段；直连（`--listen`）仍无认证。
  也**不加密流量**——它的作用是"别人扫到端口也连不上"，不是防中间人。
  需要更强保证时用隧道（Tailscale/WireGuard）。
- **更强的认证（未实现）**：`Hello.Credentials{Kind}` 支持 `token` / `mTLS` 指纹；
  服务端侧是 `execHost` 的 `Authenticator` 接口，在 `Hello` 之后、`Welcome` 之前调用，
  失败直接 `Welcome{Error}`。位置已经留好。
- **加密**：在 `wire.Conn` 之下插一层（Noise/TLS 风格），上层包格式不变；
  relay 转发时是密文（端到端），relay 只看到路由头。
- **授权**：`Capability`（`Welcome`）与命令级权限（以后加 `cmd` 白名单：只读展示端
  只能发 `blob.get`/`loadOlder`）。现在所有已连上的展示端都是全权限（用户决定 1）。
- **工具权限只由执行端决定**：`allow_shell`、工作区、`vision`、模型凭据都在执行端配置里；
  展示端没有任何字段能改它们（命令里也没有对应参数）。

---

## 10. 分期计划与验收

### P1 — 重构：单机走 `host.Host`，功能不变

- 新增 `internal/host`：`Host` 接口 + `localHost`（搬 `applyEffects`/`startStream`/`runTools`/
  `startSummary`/日志同步）；
- `uiapp` 改为只依赖 `host.Host`；`Options` 里与执行端相关的字段（`Catalog`/`Blobs`/
  `WindowFor`/`VisionFor`/`AllowShell`/`Workspace`…）搬进 `host.Config`；
- `driver` 拆出 `ApplyStreamDelta` / `FinishStream`（`ApplyEvent` 变成两者的组合），
  加 `Mirror` 模式 + `ApplyPersisted`，供 P2 用（P1 只有单测覆盖）。

**进展**：driver 拆分与 `internal/host`（`Host` 接口 + `Local`，命令异步 `Submit`）已完成，
uiapp 已只依赖 host 读写；单机行为不变。单机 `Submit` 默认**同步执行**（没有网络往返），
但接口语义是异步的：测试用 `Deferred` 模式（`Options.DeferredCommands`，命令排进 Post 队列
到下一次 drain 才执行）跑一组用例守住异步语义（发送成功 / 失败后还原草稿与附件 / 插话 /
停止 / 显示型提示只出现一次），这组用例就是 remoteHost 的前哨。

**验收**：现有全部测试通过（`go test ./...` + `scripts/check.ps1`），**零行为变化**；
新增：
- `TestApplyEventSplit`：对同一串 provider 事件，`ApplyEvent` 的结果与
  "`ApplyStreamDelta` + 终态时 `FinishStream`"完全一致（保证拆分没有改变执行端行为）；
- `TestStreamDeltaMatchesExecutor`：每个事件之后，mirror 的 Conversation 与执行端
  `ApplyStreamDelta` 的结果深相等；且 mirror 在 `EventDone` 后没有新增 notice、
  没有多出 user 消息、Busy 不变；
- `TestLocalHostMatchesDirectDriver`：同一串操作直接跑 driver vs 经 localHost，
  断言 Conversation 深相等。

### P2 — 连接层 + 直连 + 只读同步 + 断线续传（已实现，见 §12 第 63 条）

- `internal/wire`：包、分块、握手、心跳、`PipePair`；
- `execHost`（在 localHost 之上加广播）+ `remoteHost`（读接口 + 镜像）；
- 展示端**只读**：能看会话列表、看历史与流式输出、看状态条；命令类操作先禁用。

**验收**（单进程起两端，`wire.PipePair`）：
- `TestViewerSeesStreamingText`：执行端发一条消息，模型（假 runtime）流式吐字，
  展示端逐帧文本与执行端一致；
- `TestViewerSeesToolRun`：工具调用 + 结果 + 界面折叠标题一致；
- `TestViewerSeesInflightText`：在模型输出到一半时**才** attach，展示端能看到
  snapshot 里 `Inflight` 的已输出内容，并正确接上后续增量（不重复、不丢失）；
- `TestViewerTakeoverMidToolArgs`：按 **Anthropic 的 block 下标**（text=0、tool_use=1）
  在参数流到一半时接管，只有一张工具卡片，`id`/`name`/参数前缀都正确；
- `TestViewerTakeoverMidReasoning`：推理流到一半时接管，后续推理仍写进同一个 part；
- `TestViewerReconnectResumes`：中途断开连接、再连上（带 `(generation, seq)` 游标），
  展示端状态与执行端一致，且**没有**重放全部事件（断言收到的 `log.events` 只含增量）；
- `TestViewerReconnectAfterCompact`：断线期间执行端发生一次压缩（generation 变化），
  重连后展示端从新快照重建、两端一致（不能把新 `seq=3` 误判为已收到）；
- `TestViewerSnapshotFallback`：ring 覆盖不到时收到全量（`truncated=true`）并正确重建；
- `TestViewerSeesDisplayNotice`：执行端在展示端连上**之前**产生过显示型提示，
  后连上来仍能在 snapshot 的 `Notices` 里看到。

### P3 — 展示端操作 + blob 双向 + 归档按需

- 命令通道全部打通（§4.2 全表）；
- blob 拉取/推送（§5.1）、`loadOlder`（§5.2）。

**验收**：
- `TestTwoViewersSteerInOrder`：两个展示端先后发消息，执行端按到达顺序处理，
  第二个进插话队列；两端都看到队列内容；
- `TestStopFromViewerStopsAll`：A 端停止，B 端界面回到 idle；
- `TestAskAnsweredElsewhere`：A、B 同时回答同一提问，B 收到「已被其他端回答」；
- `TestViewerPasteImageReachesExecutor`：展示端粘贴图片 → 执行端 blob 里有该 hash →
  执行端消息带上引用 → 两个展示端都能显示；
- `TestViewerOlderArchive`：加载更早历史走分页，两端一致；
- `TestViewerSendWithAttachmentWhileBusyRejected`：忙时带附件的发送被拒并提示
  「插话暂不支持附件」，队列里不会出现附件（与本地语义一致）；
- `TestCommandIsAsync`：`Submit` 立即返回，结果通过 `cmdResult` 回到界面线程；
  用一个永不回包的假连接证明**界面线程不会被命令阻塞**（超时断言）。

### P4 — relay + headless

- relay 二进制（或 `simple-agent --relay`）+ 注册/列表/转发；
- `--headless` 执行端。

**验收**：
- `TestRelayRoutesPackets`：进程内起 relay + 执行端 + 展示端，展示端通过 relay 拿到
  与直连一致的状态；
- `TestHeadlessLifecycle`：`--headless` 启动、接受连接、SIGINT 干净退出（日志顺序断言）。

### P5 — 跳过（用户决定，v1 不做认证）

**用户决定：v1 不做认证，P5 跳过。** 原计划（共享密钥 + HMAC 质询、加密层位置）不实现。

- 预留保持原样：`Hello.Credentials{Kind:"none"}`、`Welcome.Error`、`Authenticator` 位置；
- 新增"未经认证的远程访问"提示：`--help` 与 `--listen` 说明里明确写出风险，
  执行端监听非回环地址时启动打警告日志；
- 以后要加认证时，位置就在 `Authenticator`（relay 注册与执行端 hello 两处）。

---

## 11. 风险与需要用户决定

### 11.1 风险

1. **重构面大**：uiapp ~250 处引用 + `applyEffects` 搬家，P1 若不小心会改坏现有行为。
   缓解：P1 只搬不改，测试不动（只修因搬家导致的编译错误），并加一条
   "直接 driver vs localHost" 的对照测试。
2. **镜像 driver 的副作用**：展示端的 driver 若误执行 effect（开流、跑工具、写日志），
   会造成"两端各跑一遍"。缓解：`Mirror` 模式在构造时就把 `Runtime`/`Journal`/`Blobs`
   设为空实现，`Record`/`StartStream`/`RunTools` 一律 panic（测试里断言不触发）。
3. **包大小与延迟**：工具输出可能几十 KB（`read` 上限 200KB）。
   缓解：单事件超包上限 → 走 `log.event.large` 分块；实时性优先的是文字增量（很小）。
4. **时钟与排序**：不用时间戳排序（命令按到达顺序），展示端显示的时间戳来自事件 `at`，
   不做本地时间推断。
5. **断线期的事件丢失**：ring 只保留最近 2000 条；断线太久 → 全量重放（可接受，
   但要给用户一个"正在同步…"的进度提示）。
6. **blob 缓存增长**：展示端缓存需要上限（建议 LRU 512 MB）+ 手动清理；
   执行端 blob 不删（与现在一致）。
7. **headless 的模型/工具配置错误**：没有界面就看不到启动错误。缓解：启动时把配置
   校验结果打日志，致命错误直接退出非 0。

### 11.2 用户已确认（已定，来源：用户）

| # | 问题 | 决定 |
| --- | --- | --- |
| 1 | 执行端默认监听范围 | **默认只监听回环 `127.0.0.1`**；需要对外时显式 `--listen 0.0.0.0:7801` |
| 2 | 展示端是否允许同时跑本地执行端 | **P4 之前不允许**：`--connect` 与本地模式互斥，展示端不跑本地会话 |
| 3 | 多端在线时"停止"是否给其它端额外提示 | **不加**额外提示，界面回到 idle 即可 |
| 4 | relay 的部署形态 | **`simple-agent --relay` 启动参数**，不另出独立程序 |
| 5 | 展示端是否缓存会话日志 | **不缓存会话日志**，只缓存 blob（LRU，`viewer.blob_cache_mb` 默认 512） |
| 6 | 单事件超过包上限 | **走 `log.event.large` 分块**，包上限不调大 |
| 63 | 认证（P5） | **v1 不做认证，P5 跳过**（用户决定）；保留 Credentials/`Welcome.Error`/`Authenticator` 预留，并在 `--help`、`--listen` 与启动日志里提示"能连上的人可以操作执行端" |

### 11.3 早期的候选建议（保留原文备查，均已被 §11.2 的决定取代）

1. **执行端默认监听范围**：默认 `127.0.0.1`（只能本机/隧道），还是要默认 `0.0.0.0`
   （局域网直接可见，无认证）？建议默认回环，需要局域网时显式 `--listen 0.0.0.0:7801`。
2. **展示端是否允许"本地同时开一个执行端"**（同一进程既跑本地会话又连远端）？
   建议 P4 之前不允许（界面复杂度高），`--connect` 与本地模式互斥。
3. **多个展示端同时在线时的"停止"确认**：现在决定是"停止对所有端生效"，
   是否需要"某端正在打字/正在回答时给个提示"？建议不加，保持简单。
4. **relay 的部署形态**：独立二进制还是 `simple-agent --relay` 子命令？建议子命令
   （少一个分发物）。
5. **展示端是否缓存会话日志**（离线可看历史）？建议不缓存（避免"看到的是旧状态"），
   只缓存 blob。
6. **单包里大事件（>64 KiB）**：走 `log.event.large` 分块（建议）还是直接抬高包上限？
   建议分块，包上限保持小，避免一个慢连接拖住所有会话。

---

## 12. 附：协议消息清单（汇总）

| Kind | 方向 | 载荷 | 说明 |
| --- | --- | --- | --- |
| `hello` | 双向（发起方→接受方） | `Hello` | 角色、用途、凭据（预留） |
| `welcome` | 接受方→发起方 | `Welcome` | 版本、hostID、能力、错误 |
| `ping` / `pong` | 双向 | 时间戳 | 保活 |
| `catalog` | exec→viewer | 会话列表 | 连接后首包之一 |
| `catalog.models` | exec→viewer | `[{provider_id,label,models,labels,efforts}]` | 执行端公布的模型/档位列表。**展示端必须用它**：API Key 与模型配置只在执行端，按本机配置显示会让选择器指向执行端不存在的模型 |
| `attach` | viewer→exec | `{session_id, cursor{generation, seq}}` | 订阅会话（续传游标） |
| `detach` | viewer→exec | `{session_id}` | 退订 |
| `log.begin` / `log.events` / `log.end` | exec→viewer | `{session_id, epoch, generation, fromSeq, truncated}` / `{generation, events[]}` / `{generation, toSeq}` | 持久状态同步（分块） |
| `log.event.large` | exec→viewer | 单事件（分块） | 单事件超过包上限 |
| `log.older` / `log.olderPage` | 双向 | 归档游标 / 页面 | 按需加载 |
| `state.snapshot` / `state.delta` | exec→viewer | `StateSnapshot`（含 `Inflight` + `Notices` + `Seq`）/ `StateDelta` | 瞬时状态；快照与 `stream` 用同一 `Seq` 衔接 |
| `stream` | exec→viewer | `StreamDelta{session_id, generation, seq, message_id, event}` | provider 事件原样转发（展示端只应用纯增量部分） |
| `notice` | exec→viewer | `{session_id, level, text}` | 显示型提示 |
| `cmd` / `cmd.result` | viewer→exec / exec→viewer | `Command` / `CommandResult` | 操作与回执（异步，界面不等） |
| `blob.get` / `blob.chunk` / `blob.put` / `blob.commit` / `blob.error` | 双向 | hash、分块 | 大数据 |
| `session.created` / `session.deleted` / `session.renamed` | exec→viewer | 会话元信息 | 列表变化（也由事件体现，这份是"快通知"） |
| `relay.list` / `relay.route` | viewer↔relay、exec↔relay | hostID | relay 专用 |

---

## 13. 附：与现有代码的对应关系（便于评审对号入座）

| 现有 | 去向 |
| --- | --- |
| `uiapp.applyEffects` | → `host/localHost`（执行端唯一 effect 执行者） |
| `uiapp.startStream` / `runTools` / `startSummary` | → `host/localHost` |
| `uiapp.Session.drv` 的 81 处调用 | → `host.Runtime(id)` / `host.<Cmd>` |
| `uiapp.Session.journal` / `keptFromSeq` / `archiveCursor` | → `host`（会话元信息 + `LoadOlder`） |
| `uiapp.Options.Catalog/Blobs/WindowFor/VisionFor/AllowShell/Workspace` | → `host.Config`（执行端配置） |
| `driver.Replay` 的事件 switch | 抽出 `applyEvent`，被 `Replay` 与 `ReplayAppend` 共用 |
| `driver.ApplyEvent`（provider 事件） | 拆成 `ApplyStreamDelta`（纯增量）+ `FinishStream`（回合推进）；`ApplyEvent` = 两者组合。执行端照旧，展示端只调 `ApplyStreamDelta` |
| `store.Blobs` | 执行端照旧；展示端加只读缓存包装 |
| `uiapp.Session.askDrafts/askTab/expanded/list/draft` | **留在 uiapp**（本地界面状态） |
| `cmd/simple-agent/main.go` 的单实例锁 | 执行端保留；展示端不加锁；headless 用端口占用 |

---

## 修订记录

### 第 2 版（按评审 D1–D4 修订）

- **D1（mirror 不能跑回合推进）**：`driver.ApplyEvent` 拆成
  `ApplyStreamDelta`（纯增量）+ `FinishStream`（回合推进），`ApplyEvent` 保留为两者组合。
  mirror 只调 `ApplyStreamDelta`；消息结束、工具结果、通知、新消息、Busy 一律只来自执行端的
  持久事件与 `state.delta`（§3.3.2，附来源对照表）。P1 新增
  `TestApplyEventSplit` / `TestStreamDeltaMatchesExecutor`。
- **D2（中途连上/重连丢已输出内容）**：`state.snapshot` 增加
  `Inflight`（正在流式输出的消息的完整 parts）与 `Notices`（已有显示型提示），
  并与 `stream` 增量共用同一个 `Seq` 衔接；展示端先用 `ReplaceInflight` 替换空壳，
  再只接受 `seq > snapshot.Seq` 的增量（§3.3.1）。新增 `TestViewerSeesInflightText`、
  `TestViewerSeesDisplayNotice`。
- **D3（压缩后 seq 重置）**：续传游标改成 `(generation, seq)`，`generation` 取快照的
  `archived_as`（无快照为 0）；generation 不同就按新快照重建，归档仍按需 `loadOlder`；
  `epoch`（执行端重启）单独一层（§3.2）。新增 `TestViewerReconnectAfterCompact`。
- **D4（命令同步会卡界面线程）**：`Host` 的写接口统一改成 `Submit(Command) (CmdID, error)`
  立即返回，结果经 `Change{Kind:"cmdResult"}` 投递回界面线程；localHost 同样异步语义（§1.7）。
  新增 §1.6 线程模型：网络 goroutine 只解码/排队，所有 mirror 修改必须 Post 回界面线程。
  新增 `TestCommandIsAsync`。

### 第 2 版（P2 小项）

- §1.4：逻辑通道 id 改成握手后分配的自增 id（不用 sessionID 哈希，避免碰撞）；
- §4.3：忙时**带附件**的 `send` 不转插话，直接拒绝并提示「插话暂不支持附件」（与本地一致）；
- §5.1：展示端 blob 缓存上限做成配置 `viewer.blob_cache_mb`（默认 512，LRU）；
- §6：断线期间草稿/附件保留、不自动发送；上传中断的附件标记失败可重试。

### 第 3 版（评审文字修正）

- §3.3.1 的"流式增量"一节改成与 D1 一致：**只转发增量类事件**，`EventDone`/`EventError`
  不转发（终态只由持久事件 + `state.delta` 表达），并补了一张
  「provider 事件 → 执行端 / 是否转发 / mirror 做什么」的对照表；
- `Notice` 增加执行端生成的 `ID`，snapshot 合并时按 id 去重（否则每次重连都多一条同样的提示）。
