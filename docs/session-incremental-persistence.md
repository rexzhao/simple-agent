# 会话增量持久化设计

本文记录会话过程中 tool call / tool result 等中间过程的持久化改造：从「turn 内内存累积、
turn 结束一次性落盘」改为「tool result 完成即落盘」，并引入事件总线 + projector 统一
持久化路径。它是详细设计来源；公开配置形态和高层运行时说明会同步摘录到
`docs/configuration.md` 和 `docs/development.md`。`docs/milestones.md` 与 `docs/checklist.md`
仍只用于里程碑级状态。任务勾选清单见 `docs/tasks/session-incremental-persistence-checklist.md`。

参考了 opencode（sst/opencode）的 event-sourced + projector 架构，但简单-agent 的 v2
JSONL 记录本身就是持久事件日志，因此不另设 EventTable，总线为内存层。

目标是保证即使未来在新的会话中继续开发，也能从本文恢复足够上下文并完成实现。

## 背景：当前机制与问题

- agent 主循环 `internal/agent/agent.go:58` `run` 完全内存态：`streamModelTurn` 把文本累积进
  `strings.Builder`、tool call 累积进 slice，每个 tool 执行完把结果 append 进内存 `messages`。
  整个 turn 不碰磁盘，只在退出时经 `TurnResult` 返回完整 `messages`。
- 落盘发生在 turn 边界：`internal/cli/cli.go:4303` `saveUpdatedMessages` →
  `internal/sessions/v2.go:741` `AppendItemsAndReplaceActiveHistory`，把整 turn 拼成一个
  `transaction.begin … item.appended(×N) … active_history.replaced … transaction.commit`
  事务，一次 `appendRecords` 写入 JSONL。server 路径同理（`internal/server/server.go:1145`
  `SaveTurn`）。
- replay 只 apply 完整提交的事务（`v2.go:1580` `replayCommittedTransaction`），中途崩溃则整
  turn 丢失，靠 `MarkRunningTurnsInterrupted`（`v2.go:354`）在 meta.json 标记 interrupted。
- streaming delta 只用于 UI / WebSocket 推送，从不落盘。

后果：

1. Esc / 断线 / 崩溃时整 turn 丢失，已完成的 tool result 不稳定保留。
2. tool call 与 tool result 是两条分离、不可变的 `SessionItem`，无法表达「tool 执行中」状态。
3. 持久化逻辑分散在 CLI（`cli.go:5154`）与 server（`server.go:1145`）两条重复路径。
4. WebSocket 重连只能补播已提交记录，无法重放 turn 内进度。

## 目标

- tool-result 持久化粒度从「整 turn」细化到「每个 tool result 完成即落盘」。
  **精确含义**：持久化点 = (a) 每个 model round 结束时 `AssistantReady` 一次性 append
  assistant + N 个 pending tool item；(b) 随后每个 tool result 到达时 `ToolResultReady` 即
  `UpdateItem` 落盘。**不是** per-streaming-delta、**也不是** per-`ToolCallDoneEvent`——单个
  tool call 的 delta 不落盘，pending item 在 round 结束、该 round 全部 tool call 已知后才创建。
- `active_history` 任意时刻保持 provider-valid（resume 校验 `cli.go:5511`
  `validateActiveHistoryToolExchanges` 始终通过）。
- tool-result item 作为单个可演化 item，`pending → completed/error/interrupted`，经新增
  `item.updated` 记录原地更新（append-only 日志保留可审计性）。
- 引入事件总线 + projector：agent 只发领域事件，projector 订阅并写 v2 store（单一持久化路径，
  消除 CLI/server 双路径）；UI/WebSocket 订阅同一总线拿实时流，catch-up 读持久记录。

## 非目标

MVP 不实现：

- 把 tool call 移出 assistant 消息、改为 opencode 式 parts 模型（B2）。会引入存储↔provider
  翻译层，风险大、收益不明确。
- 持久化 streaming delta。与 opencode 一致：delta 只发 UI，phase 边界（assistant 消息成型、
  tool 结果到达）落盘即可。
- 迁移到 SQLite 或另设 EventTable。v2 JSONL 记录即持久事件日志，append-only + `item.updated`
  已满足需求。
- blob GC。更新大内容会写新 blob、旧 blob orphan，与现状一致，可接受。
- 自动重跑未完成 tool。tool 多有副作用，崩溃后改为合成 interrupted 结果，由下一轮或用户决定
  是否重试。

## 核心原则

1. `Items` 仍是完整事实账本，append-only；`item.updated` 原地替换条目内容，不破坏 append-only
   语义（日志保留全部版本，replay 取最新）。
2. `ActiveHistory` 是发给模型的 ordered projection，任意时刻须是 provider-valid 前缀。
3. **演化的只是 tool-result item**。assistant 消息（含 `ToolCalls`）仍是一条不可变 item——文本在
   model-turn 结束时已知，无需提前创建；tool-call ID 由 provider 分配、turn 内稳定，结果到达前
   就已知，故可提前创建 pending tool item。存储 item 与 provider 消息格式同构，无需翻译层。
4. **round-commit 复用现有 `AppendItemsAndReplaceActiveHistory`**：一个事务内 append assistant
   + N 个 pending tool item，并把 `active_history` 推进到 `[...prev, assistantID, tool1..toolN]`。
   该前缀合法：校验只查 `ToolCallID` 配对、不查内容，故 N 条空内容 pending tool item 即满足
   `validateActiveHistoryToolExchanges`。
5. **唯一新存储原语**是 `RecordTypeItemUpdated` 记录 + `UpdateItem` 方法 + replay case。
6. **不另设 EventTable**：v2 JSONL 记录即持久事件；总线是内存层，durable 事件同步经 projector
   写盘、transient delta 异步扇出。
7. 崩溃恢复靠 materializer 合成：`Status=pending` 的 tool item 在 materialize 时合成 interrupted
   错误结果，保证 provider 请求合法；不自动重跑 tool。
8. turn running/interrupted 状态继续存 meta.json（`MarkTurnRunning`/`ClearRunningTurn`/
   `MarkTurnInterrupted`），不新增 turn 生命周期记录类型，**不复用** `RecordTypeTransactionCommit`
   表达 turn 生命周期。但这些 meta.json 生命周期写入**经 projector 派发**（`TurnStarted`/
   `TurnCompleted`/`TurnInterrupted` → projector 调对应 store 方法），server handler 不再直接写——
   见 SessionProjector。
9. **单写者/串行化是硬约束**：同一 session 的 durable event 必须严格串行落盘。当前 store 无任何
   锁（`internal/sessions` 内 `grep sync./Mutex/Lock` 零命中），`appendRecord` 是 replay 取
   `LastSeq` 后 append 的 read-then-write，靠「单 running turn + 单写点」隐含串行。增量后写点变多
   （`AssistantReady` + 每 tool `UpdateItem`），并发抓同一 `LastSeq` 的风险显化。落地手段（两层）：
   (a) **session-level turn lock**——创建 bus/projector 前必须先获取，保证同 session 同时最多一个
   active projector（server 复用 `beginSessionTurn`；多进程靠 store per-session 文件锁兜底）；
   (b) projector 作为**单一 goroutine** 消费 channel，bus 的 durable `Publish` 同步投递并等 ack——
   串行由单消费者保证该 turn 内有序。**任何实现都必须保证：同 session 并发/连续发多个 durable event，
   最终 seq 连续无空洞、replay 正常；且同 session 两个并发 turn 不会产生两个 projector 双写。**
10. **`MarkTurnRunning` 之后任何退出路径（成功除外）都必须发布 `TurnInterrupted` 并落盘**，不能留下
    running turn。见下方「失败与中断语义」。

## 数据模型变更

### `SessionItem` 增加 `Status` 字段

```go
type SessionItem struct {
    // ...既有字段...
    Status string `json:"status,omitempty"`
}

const (
    ItemStatusPending     = "pending"
    ItemStatusCompleted   = "completed"
    ItemStatusError       = "error"
    ItemStatusInterrupted = "interrupted"
)
```

- 空值 = legacy / 非 tool item，按 `completed` 处理（旧会话回归无影响，`omitempty` 不破坏旧读取）。
- 只有 tool-result item 使用 `pending`/`error`/`interrupted`；assistant / user / compaction item 不设。

### 新记录类型 `item.updated`

```go
RecordTypeItemUpdated = "item.updated"
```

复用 `v2Record.Item`（携带完整更新后的 `SessionItem`，其 `ID` 即目标），不加新字段。

### replay 与 seq 语义

`replayCommittedRecord`（`v2.go:1670` switch）加 case：按 `ID` 在 `state.Items` 切片中原地替换
**可变字段**（`Message`/`Content`/`Status`），找不到 → `corrupted`。事务内 / 外均自动生效（事务机
走同一函数）。`SessionItem` 当前无 `UpdatedAt` 字段（只有 `CreatedAt`），故可变字段不含它；若日后
想暴露最后更新时间，另加 `UpdatedSeq`/`UpdatedAt` 字段（见下，可选），不要塞进既有字段语义。

**seq 语义（写死，不可留白）**：

- `SessionItem.Seq` = **birth seq**（该 item 被 `item.appended` 创建时的 record seq），**跨
  `item.updated` 不可变**。replay 的 update case **显式保留 `existing.Seq`、`existing.ID`、
  `existing.CreatedAt`**，只替换可变字段。`UpdateItem` 不得把 `item.Seq` 设成 update record seq。
- update record 自身仍消耗一个 seq（`record.Seq = LastSeq+1`），用于推进 `state.LastSeq` 与
  catch-up 事件顺序，但**不写入 `item.Seq`**。
- `PersistedEvent.Seq` = update record seq（catch-up 按日志顺序排列更新事件）。
- 若需暴露「最后更新时间」，另加 `UpdatedSeq int64`（可选，MVP 可不加）。**不要**用覆盖 `item.Seq`
  的方式表达更新。

**为什么不可覆盖 `item.Seq`**：分页与 catch-up 假设 `state.Items` 切片顺序 == `item.Seq` 单调
递增。`paginateSessionItems`（`server.go:2288`）用 `AfterSeq`/`BeforeSeq` 游标,
`firstSessionItemIndexAfterSeq`（`server.go:2309`）线性扫 `item.Seq > seq`,`sessionItemPageSeqBounds`
（`server.go:987/2281`）取首尾 seq 作页边界。若 update 把 `item.Seq` 改成更大的 update-record seq,
slice 将出现「前项 seq > 后项 seq」,游标分页错乱、页边界倒退。例:

```
append asst        seq=10  item.Seq=10
append toolA(pend) seq=11  item.Seq=11
append toolB(pend) seq=12  item.Seq=12
update toolA→done  update-record seq=13
  若 item.Seq:=13 → slice=[asst(10), A(13), B(12)]  ← A.Seq>B.Seq 但 A 在 B 前 → 分页坏
  正确：item.Seq 保持 11 → slice=[asst(10), A(11), B(12)]  ← 单调保持
```

### tool-result item 生命周期

- 创建（round-commit 时）：`role:tool`、`ToolCallID=toolCall.ID`、`Content=""`、`Status=pending`。
- 完成（结果到达）：`item.updated` → `Content=result.Content`、`IsError=result.IsError`、
  `Status=completed`/`error`。
- 中断（turn 失败/取消未完成）：projector `TurnInterrupted` handler **写 `item.updated`** 把
  `Status=pending` 置 `interrupted`（盘上诚实，**非 optional**）；materializer 合成 interrupted 结果
  作为 SIGKILL 兜底（handler 未跑时，pending 仍能被 resume 合成）。

## 事件总线与 projector

### 事件总线（新包 `internal/eventbus`）

```go
type Event interface{ kind() string }
```

- **领域事件（durable）**：
  - `TurnStarted{TurnID}`、`TurnCompleted{TurnID}`、`TurnInterrupted{TurnID}`
    —— **turn 生命周期**，由**编排层**（orchestration，非 `agent.run`）发布，包裹整个
    compaction + agent 序列。`TurnInterrupted` **只带 `TurnID`**——pending tool item 集合由
    projector 自身状态决定（它创建了这些 item、知道其 ID 与 `Status`），编排层不必、也不应传递，
    否则编排层一旦不知完整 pending 集合会漏标。
  - `CompactionRequested{TurnID, ...}` —— pre-turn compaction，编排层在 `TurnStarted` 之后、
    `TurnInputReady` 之前发布（见「compaction 与 projector 的共存」）。
  - `TurnInputReady{TurnID, Message}`（role=user）—— 当前 turn 的 **user prompt**，编排层在
    compaction 之后、`agent.run` 之前发布，projector append user item 并推进 `active_history`。
  - `AssistantReady{TurnID, Message}`（role=assistant，含 Content+ToolCalls）—— **由 `agent.run`**
    在 model-round 结束发布。
  - `ToolResultReady{TurnID, Result}` —— **由 `agent.run`** 在每个 tool 完成发布。
- **瞬态事件（transient）**：包裹现有 `model.Event`（TextDelta / ToolCallDelta / ToolCallDone /
  ToolResult 等）。

**作用域（明确）**：bus 是 **per-session/per-turn** 绑定对象（每个 turn 构造一个 bus + projector），
因此事件只需 `TurnID`、**不带 `SessionID`**。projector 单 goroutine 串行化该 session 写盘。
server 的 **process-level WebSocket stream hub**（既有，按 session ID 路由）作为 per-turn bus 的
**一个订阅者**桥接出去做进程级 live 扇出；catch-up 仍走 `PersistedEventsAfter`。不采用 process-wide
bus + 事件带 `SessionID` 的方案——那会引入跨 session 的 projector 路由复杂度，无收益。

**session-level 单写者边界（硬前提，per-turn projector 不自足）**：per-turn projector 只保证「该
turn 内单 goroutine 写盘」，**不**保证「同 session 同时只有一个 projector」。若同一 session 被并发触发
两个 turn，会出现两个 per-turn projector 各自单 goroutine、合起来却是双写者，并发抓同一 `LastSeq`。
因此**创建 bus/projector 之前必须先获取 session-level turn lock**：

- server 路径复用既有 `beginSessionTurn`（`server.go:1361`，`p.mu` + `runningTurns[sessionID]` map）：
  同 session 已有 running turn 时返回 `beginTurnBusy` 拒绝，保证同 session 同时最多一个 active
  projector。`beginSessionTurn` 在 `TurnStarted`/`MarkTurnRunning` 之前调用，是外层门闸。
- CLI 路径单进程单 turn，天然不并发；但**多进程同 session**（如两个 `sai --resume` 同一 session、
  或 CLI 与 server 同 session）下 `beginSessionTurn` 不跨进程。故 store 层应有 **per-session 写锁**
  （如文件锁）作兜底——这是 defense in depth，覆盖多进程场景。
- 现有 `MarkTurnRunning`（meta.json 的 `running_turn_id`）是**持久化的 running 标记**，供跨进程/
  崩溃恢复检测（启动扫描），但**不**用作并发互斥锁（meta.json 读改写非原子的 read-then-write，
  不能防并发）；并发互斥靠 `beginSessionTurn` 内存锁 + store 文件锁。

`Bus`：

- `Publish(e Event)`：durable 事件**同步**经 projector 写盘并确认后返回（保证 tool result 落盘后
  agent 才继续，对齐 opencode 在 publish 内提交持久事件）；transient 事件异步扇出到 live 订阅。
- `Subscribe() <-chan Event`：live 订阅（UI / server stream hub 桥接）。
- 持久事件 seq = projector 写出的 v2 记录 seq；catch-up 仍走 `PersistedEventsAfter`，无需独立
  EventTable。

### SessionProjector（`internal/sessionprojector`）

订阅总线，翻译领域事件 → v2 记录，是**唯一**写存储者（含 JSONL 日志与 meta.json 生命周期）。事件
顺序由编排层 + `agent.run` 共同保证：

```
编排层: TurnStarted → [CompactionRequested?] → TurnInputReady → 启动 agent.run
agent.run: AssistantReady → ToolResultReady ×N → (下一 round) AssistantReady → ...
编排层: TurnCompleted（或 defer: TurnInterrupted）
```

- `TurnStarted` → `MarkTurnRunning`（meta.json）。**编排层**在 pre-turn compaction **之前**发，先于
  任何 `AssistantReady`。
- `CompactionRequested` → `SaveCompactedTurn`（单事务，见「compaction 与 projector 的共存」）。
  **handler 后必须刷新 projector 缓存**：compaction 推进了 `LastSeq`、替换了 `active_history`、
  追加了 summary/checkpoint items，故 projector 处理完 `CompactionRequested` 后须用
  `SaveCompactedTurn` 的返回（或重新 replay）更新缓存的 `LastSeq + Items + ActiveHistory`，**再**
  处理 `TurnInputReady`——否则后续 user/assistant/tool 写入会基于旧 `LastSeq`（seq 冲突）或旧
  `ActiveHistory`（写到被 compaction 替换前的历史上）。
- `TurnInputReady` → append user item + 推进 `active_history`（合法前缀：以 user 消息结尾合法）。
  **编排层**在 compaction 之后、`agent.run` 之前发。
- `AssistantReady` → `AppendItemsAndReplaceActiveHistory`（assistant item + N 个 pending tool
  item），`active_history` 推进到合法前缀。维护 `toolCallID → itemID` 表。**`agent.run`** 发。
- `ToolResultReady` → `UpdateItem`（对应 tool item → completed/error）。**`agent.run`** 发。
- `TurnCompleted` → `ClearRunningTurn` + 刷新 metadata（**不做 compaction**——只有 pre-turn
  compaction，见「compaction 与 projector 的共存」）。**编排层**发。
- `TurnInterrupted` → **写 `item.updated`** 把 pending tool item 置 `interrupted`（必须，非 optional）
  + `MarkTurnInterrupted`。**编排层 defer** 发。materializer 合成仅作 SIGKILL 兜底（handler 没跑时）。

「唯一写者」覆盖 meta.json 生命周期（`MarkTurnRunning`/`ClearRunningTurn`/`MarkTurnInterrupted`），
即 server handler **不再直接**调这些 store 方法——一律经 `TurnStarted`/`TurnCompleted`/
`TurnInterrupted` 事件由 projector 落盘。这样消除「server handler 是第二写者」的边界破口。

复用既有：`UpdateItem`、`AppendItemsAndReplaceActiveHistory`、`MarkTurnRunning`/`ClearRunningTurn`/
`MarkTurnInterrupted`、`sessionItemFromMessage`、`nextSessionItemID`（后两者从 `cli.go` 迁入共享包）。

### 与既有事件流的关系

- agent 现有 `events chan<- model.Event`（被 `writeStreamWithOptions` 渲染、server
  `publishModelTurnEvent` 转发）改为经总线：delta 作为瞬态事件扇出，渲染器订阅总线，经
  **bus→channel 桥接**喂回既有 `events` channel（避免重写 `writeStreamWithOptions` 渲染器）。
  不采用「保留 channel + publisher 双扇出」——那会留两条并行事件路径，违背建议 3 单一总线目标。
- WebSocket clients 订阅**既有 process-level stream hub**（按 session ID 路由，不变）；hub 内部
  作为 per-turn bus 的**唯一桥接订阅者**把 live 事件扇出给 clients，替代 `publishModelTurnEvent`。
  即 per-turn bus 不被 WebSocket client 直接订阅，只被桥接器订阅。catch-up 经 `PersistedEventsAfter`
  （加 `item.updated` case，见下「`item.updated` 客户端取数」）。

## 分阶段设计

### Phase 1 — 存储基础（纯存储，可独立验证）

文件：`internal/sessions/v2.go`

1. `SessionItem` 加 `Status` + 常量。
2. 新记录类型 `RecordTypeItemUpdated`。
3. replay case：按 ID 原地替换**可变字段**，保留 birth `Seq`/`ID`/`CreatedAt`，找不到 → corrupted。
4. `UpdateItem` 方法（仿 `AppendItem`）：replay 取 `LastSeq` → `blobifySessionItemContent` →
   写单条非事务 `item.updated` 记录（`record.Seq = LastSeq+1`，但**不覆盖 `item.Seq`**）→
   返回更新后 item。**API 边界（与 Phase 6 projector 缓存配套）**：`UpdateItem` 内部 replay 取
   `LastSeq` 是默认实现；projector 为避免每个 tool result 全量 replay 需复用缓存的 `LastSeq`，故需
   明确使用方案 (a)：`internal/sessions` 暴露带缓存状态的写入 API（如
   `AppendItemsAndReplaceActiveHistoryFromState`、`UpdateItemFromState`），由
   `internal/sessionprojector` 调用并提供 cached state；**不能让外部 projector 自行 replay 后又调
   `UpdateItem` 再 replay 一次**——那既无收益又放大竞态面。该 cached-state 写路径**同时覆盖 `UpdateItem` 与
   `AppendItemsAndReplaceActiveHistory`**（两者底层都是 `appendRecords`），即多 round turn 里
   `AssistantReady` 的 append+replace 也复用缓存 seq，不只优化 `UpdateItem`。
5. materializer 合成（`materializeActiveHistory` 与 store 变体）：`role:tool` 且
   `Status` 为 pending/interrupted → 合成 interrupted 消息（in-memory，不改盘）。
6. catch-up（`persistedEventFromRecord`）：`item.updated` → `{Seq=record.Seq, Type, ItemID}`。
7. 测试：append→UpdateItem→replay 且 `item.Seq` 保持 birth seq；未知 ID→corrupted；事务内/外；
   blobified 更新；materializer 合成；**`item.Seq` 单调性**（update 后 slice 仍按 birth seq 单调）；
   旧会话回归。

### Phase 2 — 事件总线 + SessionProjector

1. 抽出**小粒度** planning 辅助函数（ID 分配 `nextSessionItemID`、`sessionItemFromMessage`、
   metadata 刷新、active-history append/replace helper）从 `cli.go` 迁到共享包，供 CLI 与 server
   共用——消除双路径的前提。**不**把 `sessionSavePlan` 整体作为共享 API：它的位置 diff
   （`messages[len(activeItemIDs):]`）是旧 turn-末模型；projector 按事件创建/更新具体 item，不做全量
   diff。`sessionSavePlan` 逐步废弃，不复用。
2. 事件总线 `internal/eventbus`：`Event` 接口、领域 / 瞬态事件、`Bus`（durable 同步、transient
   异步、`Subscribe`）。**单写者硬约束**：projector 为单一 goroutine 消费 channel，durable
   `Publish` 同步投递并等 ack，保证同 session durable event 严格串行（见核心原则 9）。
3. SessionProjector：订阅总线，翻译领域事件 → v2 记录，**唯一**写存储者。
4. 测试：fake 总线 + fake store，事件顺序与多 round 增长正确；durable Publish 同步落盘语义；
   **同 session 并发/连续发多个 durable event → 最终 seq 连续无空洞、replay 正常**。

### Phase 3 — agent 接入总线 + CLI 路径

文件：`internal/agent/agent.go`、`internal/cli/cli.go`

1. **事件发布分工**：
   - `agent.Options` 加 `Publisher eventbus.Publisher`（可选；nil = 现状缓冲返回 `TurnResult`）。
     **`agent.run` 只发** `AssistantReady`（model-round 结束）与 `ToolResultReady`（每个 tool 完成）。
   - **编排层**（CLI 的 `runChatTurn` / server 的 `runServerOwnedSessionTurn` 附近，即
     `autoCompactBeforeTurn`/`planAutoCompactBeforeTurn` 这一层）发 turn 生命周期事件：`TurnStarted`
     （pre-turn compaction **之前**）→ `CompactionRequested`（若需）→ `TurnInputReady`（compaction
     之后、启动 `agent.run` 之前）→ `TurnCompleted`（成功）；外层 defer 在非成功退出发 `TurnInterrupted`。
   - 这与现有代码顺序一致（`cli.go:4231/5973` 先 compaction、`4235/5981` 再 append user message、
     再进 agent turn）。
2. 渲染适配：`writeStreamWithOptions` 改订阅总线瞬态事件（bus→channel 桥接）。
3. CLI 装配：`runChatMessagesInTurnWithEventHook` 构造 bus + projector；**移除末尾
   `saveUpdatedMessages`/`SaveTurn`**——持久化由 projector 在 turn 内完成。`TurnResult` 仍用于
   更新内存态。
4. 测试：CLI 跑多 tool turn，逐 tool 查盘 item 状态 pending→completed；中途 kill 时已完成 result
   已在盘。

### Phase 4 — server 路径 + catch-up

文件：`internal/server/server.go`、`internal/cli/cli.go`（server runner ~5889）

1. server 装配 bus + projector：`serverAgentTurnRunner` 注入 publisher；projector 写存储（替代
   `runtime.saveSessions=false` + 末尾 `SaveTurn`）。
2. WebSocket：clients 订阅既有 process-level stream hub；hub 作为 per-turn bus 的**唯一桥接订阅者**，
   转发 `text.delta`/`tool.started`/`tool.finished` 与 durable `item.appended`/`item.updated`。
3. catch-up：`sessionStreamCatchUpPayloads` + `sessionStreamEventFromPersistedEvent` 加
   `item.updated` 映射；客户端按 `ItemID` 经 `GET /sessions/{id}/items/{itemID}` 取数（见「`item.updated`
   客户端取数」）。
4. 移除 `handleSessionMessage` 末尾 `SaveTurn`/`SaveCompactedTurn` 与 post-commit 批量
   `item.appended` 发布——职责移入 projector。
5. **compaction 边界（见「compaction 与 projector 的共存」节）**：只有 pre-turn compaction，
   经 projector 派发（`CompactionRequested` → `SaveCompactedTurn`，单事务，**不再带 turn items**）；
   在 `TurnStarted` 之后、`TurnInputReady` 之前完成，增量 assistant/tool 持久化只发生在其后。
   `TurnCompleted` 不做 compaction。projector 单 goroutine 保证 compaction 与增量流互斥串行。
6. 测试：turn 中途断开重连 → catch-up 含 `item.appended`+`item.updated`；末尾无重复 SaveTurn；
   pre-turn compaction 与后续增量流不交错。

### `item.updated` 客户端取数（明确）

`item.updated` 的 catch-up 事件是 `{Seq=update record seq, Type:"item.updated", ItemID}`。因
`item.Seq` 是 birth seq（不变）、`PersistedEvent.Seq` 是 update record seq，客户端**不能**靠
`/items?after_seq=...` 重新分页取到该 item（分页按 birth seq 游标）。取数策略定为：

- catch-up 事件只带 `ItemID`（不带完整 item——item 可能 blob-backed、体积大）。
- 客户端收到 `item.updated` 后，按 `ItemID` 调 **`GET /sessions/{id}/items/{itemID}`**（**新增**单 item
  取数端点，返回 **blob-resolved item**：真实持久化的 `Status`/`Message` + blob 解析后的内容）刷新
  本地缓存。注意区别于既有 `GET /sessions/{id}/items/{item_id}/content`（只返回原始 content/blob
  字节，无 item metadata）——本端点返回完整 item 状态，`item.updated` 需要它来更新 `Status`。
- **该端点不做 pending→interrupted 合成**：返回 item 的**真实** `Status`（仍在 running 的 tool item
  就是 `pending`，如实展示）。pending/interrupted 合成是 `MaterializeActiveHistory` 的 provider/resume
  历史语义（构造发给模型的 messages 时用），不应污染单 item 取数端点——否则客户端会把运行中的
  tool 误显示成 interrupted。
- 不采用「事件带完整 item」（catch-up 体积、blob）或「整页刷新」（过重）。

`item.appended` 仍可由客户端按 `after_seq` 分页取得（birth seq 游标有效）；只有 `item.updated` 走
按 ID 取数。

### Phase 5 — 中断恢复

- materializer 合成（Phase 1）即 SIGKILL 安全网：崩溃 → `MarkRunningTurnsInterrupted`（仅 meta.json）→
  resume 时 `MaterializeActiveHistory` 对 pending 合成 interrupted → 校验通过、provider 请求合法。
- projector `TurnInterrupted` handler **必须写 `item.updated`** 把 pending 置 `interrupted`（非 optional，
  盘上诚实）；materializer 合成仅用于 handler 没跑（SIGKILL）的兜底。
- 测试：round-commit 后、results 到达前崩溃 → resume 成功、历史合法、可继续新 turn。

### Phase 6 — 集成与回归测试

- 全链路：CLI 与 server 各跑多 tool turn，逐 tool 校验盘上状态；中途 kill 校验 resume。
- active_history 合法性：每个 hook 点后 `validateActiveHistoryToolExchanges` 通过。
- 回归：旧会话（无 Status / 无 updated 记录）load/resume/compaction 不受影响；
  `SaveCompactedTurn` 路径仍工作。
- 性能：durable `Publish` 同步落盘会把磁盘 IO 串进 tool 执行路径——每个 tool result 都要等写盘
  才继续。注意 `UpdateItem` / `AppendItemsAndReplaceActiveHistory` 当前每次都 replay 全 session 取
  `LastSeq`（与 `AppendItem` 同），长会话 + 多 tool turn 下这可能比预期更早成为体感瓶颈。
  **关键缓解（projector 自带）**：projector 是单写者、单 goroutine，可在 turn 内缓存当前 session
  的 replayed state（`LastSeq` + `Items` + `ActiveHistory`），`UpdateItem` **和**
  `AppendItemsAndReplaceActiveHistory` 都复用缓存的 `LastSeq+1` 直接 append、不再每次全量 replay
  （两者底层都是 `appendRecords`，cached-state 写路径**同时覆盖 append + replace**，不只 `UpdateItem`）；
  仅 turn 开始时 replay 一次。这同时强化单写者约束（缓存是 projector 私有态）。若极端长会话仍成
  问题，再加 seq→segment 索引（本设计不做）。

## 失败与中断语义

`MarkTurnRunning` 之后，**任何退出路径（成功除外）都必须发布 `TurnInterrupted` 并落盘**，不能留下
running turn。`MarkRunningTurnsInterrupted`（`v2.go:354`）的启动扫描是崩溃的最后防线，但正常失败
路径不得依赖它——必须由 agent run 外层的 defer/finally 兜底。

**存储故障下的最后兜底（诚实声明）**：若失败原因正是 projector/store 不可用，`TurnInterrupted` 的
publish 也可能失败。此时：(a) defer 内 best-effort 直接调 `store.MarkTurnInterrupted`（绕过总线，
meta.json 单文件重写，不涉及 `LastSeq` 竞态，较可能成功）；(b) 若连 meta.json 也写不进，则**无法
在本次运行内清除 running turn**——「不留 running turn」的验收在存储故障场景下**不可证明**，只能依赖
下次启动 `MarkRunningTurnsInterrupted` 扫描兜底。这是显式的剩余风险，不掩饰。

**user prompt 落盘边界（明确）**：`TurnInputReady` 由编排层在 compaction 之后、`agent.run` 之前发，
projector 此时 append user item 并推进 `active_history`（合法前缀：以 user 消息结尾）。

- turn 在 `TurnInputReady` **之前**失败（如 compaction 失败、`TurnStarted` 后立即崩）→ user prompt
  **不落盘**，failed turn 仍 transient（与现状一致）。
- turn 在 `TurnInputReady` **之后**、首个 `AssistantReady` **之前**失败 → user prompt **已落盘**，
  active_history 以 user 消息结尾（合法）；`TurnInterrupted` cleanup 标记 interrupted。resume 时历史
  合法、可继续新 turn（这比现状略好——user prompt 不丢；是可接受的行为变化）。

| 触发 | 盘上状态 | cleanup 责任 |
|---|---|---|
| `TurnInputReady` publish 失败 | user item 未写或原子未写 | 编排层 defer 发 `TurnInterrupted` |
| `AssistantReady` publish 失败 | `AppendItemsAndReplaceActiveHistory` 原子，要么全写要么没写；若已写则留 pending items | agent 发 `ErrorEvent` 返回；外层 defer 发 `TurnInterrupted` → projector 把 pending 置 interrupted + `MarkTurnInterrupted` |
| `ToolResultReady` publish 失败 | 该 tool item 留 pending | 同上，`TurnInterrupted` |
| tool 执行返回 error result | **正常路径**，非 publish 失败 | `ToolResultReady(IsError=true)` 正常发，item 置 `error`，turn 继续 |
| 用户 Esc / `ctx` cancel | 已写 items 保留 | 编排层 defer 发 `TurnInterrupted` |
| server handler 返回错误 | 已写 items 保留 | handler defer 兜底发 `TurnInterrupted` |
| 进程崩溃（SIGKILL） | 留 pending/running | 启动时 `MarkRunningTurnsInterrupted` 扫描 + resume 时 materializer 合成 interrupted |

`TurnInterrupted` 的 projector handler：把当前 turn 所有 `Status=pending` 的 tool item 置
`interrupted`（**写 `item.updated`**，盘上诚实），然后 `MarkTurnInterrupted`。无论哪条路径，resume
时 `MaterializeActiveHistory` 对 pending/interrupted 合成错误结果，保证
`validateActiveHistoryToolExchanges` 通过、provider 请求合法。

**持久化失败时的内存/盘一致性（明确，不留模糊）**：durable publish 失败时，turn **立即中止**——
agent 发 `ErrorEvent`，外层 defer 发 `TurnInterrupted`，不再继续执行后续 tool / model round。**内存
`messages` 不回滚也不前进**：直接丢弃，turn 终止；**盘是权威态**（由 projector 维护），下次 resume
以盘为准。理由：持久化失败后内存若继续前进会造成「内存领先于盘」的不可收场分歧；回滚内存到上次已
落盘前缀代价高且无收益（turn 已失败）。唯一例外是 tool 执行返回 error result——那是**正常路径**
（`ToolResultReady(IsError=true)` 正常落盘、turn 继续），不是持久化失败。

## compaction 与 projector 的共存

compaction **经 projector 派发**，而非「projector 之外的独立写者」——否则 compaction 与增量流会
出现两个写者，违反单写者硬约束（核心原则 9）。具体：

- compaction 以领域事件形式进入总线（`CompactionRequested`），projector 订阅后调用
  `SaveCompactedTurn`（其内部 `appendCompactionAndItemsReplaceActiveHistory` 仍是**一个事务**提交
  summary + checkpoint + active_history，原子边界不变）。
- **只有 pre-turn compaction**（与现有代码一致——`cli.go:4231/5973` 的 `autoCompactBeforeTurn`/
  `planAutoCompactBeforeTurn`；不存在 end-of-turn compaction）。由编排层在 **`TurnStarted` 之后、
  `TurnInputReady` 之前**判定并发 `CompactionRequested`。`TurnCompleted` handler **不做 compaction**，
  只 `ClearRunningTurn` + 刷新 metadata。
- 因 projector 是单一 goroutine，compaction 与 `AssistantReady`/`ToolResultReady` 天然互斥串行，
  不会并发写盘。增量 assistant/tool 持久化只发生在 compaction 事务提交之后。
- **`SaveCompactedTurn` 角色收窄（行为变化，需写明）**：现状代码（`server.go:1147`）把 pre-turn
  规划的 compaction 与 turn items 在 **turn 末尾一起**原子提交。增量下 turn items 改为 turn 内增量
  落盘，故 compaction 必须在 pre-turn **单独**提交（`SaveCompactedTurn` 不再带 turn items，只提交
  summary + checkpoint + active_history 替换）。后果：若 compaction 提交后、turn 在首个
  `AssistantReady` 前失败，**compaction 已落盘**（active_history 已替换为 summary + 保留项）。这是
  可接受的——compaction 是上下文管理、不丢数据（旧 items 仍在 `Items` 账本），resume 看到
  [compacted history..., user prompt]（`TurnInputReady` 已落盘）合法可续。compaction 失败则 turn
  在 `agent.run` 前即失败（`TurnInterrupted`），同现状。

## 复用的现有设施

- `AppendItemsAndReplaceActiveHistory`（`v2.go:741`）：round-commit 复用，无需新提交方法。
- `blobifySessionItemContent`/`WriteBlob`（`v2.go:1040/958`）：`UpdateItem` 复用，blob 不可变。
- `appendRecord`/`appendRecords`（`v2.go:1060/1082`）：单条非事务写。
- `sessionItemFromMessage`/`nextSessionItemID`（`cli.go:5588/5575`）：迁入共享包后 projector 复用。
- meta.json 生命周期：`MarkTurnRunning`/`ClearRunningTurn`/`MarkTurnInterrupted`
  （`v2.go:292/309/328`）承载 turn 状态。
- `PersistedEventsAfter`（`v2.go:930`）：加 `item.updated` case 即 catch-up。

## 验收标准

- tool result 在其完成的瞬间即落盘（`Status=completed`），无需等 turn 结束；CLI 与 server 两路径
  均如此。落盘点 = round 边界 `AssistantReady` + per-result `ToolResultReady`，**不含** per-delta、
  **不含** per-`ToolCallDoneEvent`。
- `item.updated` 不覆盖 `item.Seq`（birth seq 不可变）；update 后 `state.Items` 仍按 birth seq
  单调，`paginateSessionItems` 的 `AfterSeq`/`BeforeSeq` 游标与 `sessionItemPageSeqBounds` 正确。
- 任意时刻 `active_history` 通过 `validateActiveHistoryToolExchanges`；multi-tool assistant 消息的
  pending tool item 在结果到达前即在前缀中、且前缀合法。
- 同 session 并发/连续 durable event 严格串行，seq 连续无空洞、replay 正常（单写者硬约束）。
- `MarkTurnRunning` 后任何失败/Esc/错误退出都发布 `TurnInterrupted` 并落盘，不留 running turn；
  崩溃由 `MarkRunningTurnsInterrupted` 扫描兜底。
- 持久化失败时 turn **立即中止**（不继续、不回滚内存），内存 `messages` 丢弃，盘为权威态；
  `TurnInterrupted` 的 handler **写 `item.updated`** 置 interrupted（非 optional），materializer 合成仅作
  SIGKILL 兜底。
- 崩溃后 resume：已完成 tool result 可见；未完成 tool item 表现为 interrupted 错误结果；历史合法、
  可继续新 turn；不自动重跑 tool。
- compaction 经 projector 派发（`CompactionRequested` → `SaveCompactedTurn` 单事务，**不带 turn
  items**）；只有 pre-turn compaction，在 `TurnStarted` 之后、`TurnInputReady` 之前完成；
  `TurnCompleted` 不做 compaction。compaction 后 turn 失败时 compaction 已落盘（可接受，不丢数据）。
- **user prompt 经 `TurnInputReady` 落盘**（compaction 之后、`agent.run` 之前）；turn 在
  `TurnInputReady` 前失败则 user prompt 不落盘（transient，同现状），之后失败则已落盘且历史合法。
- bus 为 **per-session/per-turn**（事件只带 `TurnID`、不带 `SessionID`）；server process-level stream
  hub 桥接做 WebSocket live 扇出，catch-up 走 `PersistedEventsAfter`。
- WebSocket 重连 catch-up 含 `item.appended` 与 `item.updated`（`PersistedEvent.Seq` = record seq）。
- 旧会话（无 `Status`、无 `item.updated` 记录）在新代码下 load / resume / compaction 正常。
- 持久化路径单一（projector），CLI 与 server 不再各自维护 `sessionSavePlan` 写盘分支。

## 与 opencode 的对比

| 维度 | opencode | 本设计 |
|---|---|---|
| 存储 | SQLite + drizzle | JSONL append-only + blob（不变） |
| 持久事件日志 | 独立 `EventTable` | v2 JSONL 记录即事件（不另设） |
| 写入时机 | phase 边界事件增量 | phase 边界增量（assistant 成型 + tool 结果） |
| tool call + result | 同一 `AssistantTool` part 状态机 | 同一 tool-result item 状态机（`Status` 字段） |
| assistant 消息 | 一行原地 UPDATE | 一条不可变 item（文本成型即定） |
| delta 持久化 | 否 | 否 |
| 事件总线 | `EventV2` bus + projector | `eventbus.Bus` + `SessionProjector` |
