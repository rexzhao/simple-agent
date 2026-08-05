# Session Content 同步 Trace 与回放开发方案

> **状态：后续任务，暂不实施。**本任务在 `docs/websocket-sync-development-plan.md` 的 WebSocket、Sync Engine、Local Replica、Resource Adapter 和 Repository 重构完成并验收后启动。本任务不得反向影响当前同步协议第一阶段的范围。

## 1. 目标

为特定 Session 提供显式 Debug 开关，完整记录其会话数据同步链路，并能够将记录重新输入正式前端 Sync Runtime，以重现和定位页面中的以下问题：

- 消息缺失或重复；
- assistant 流式文本重复、闪回或尾部丢失；
- durable item 与 transient tail 归并错误；
- Tool 状态停留在 running、结果重复或归并到错误 Turn；
- Run settled 后 overlay 未清除或过早清除；
- 历史分页重复、丢失或出现半个 Turn；
- compaction 后 history/active history 状态异常；
- WebSocket 断线、replay、resync 或 Blob snapshot 后页面状态错误。

本任务不是普通日志增强，而是：

> **Session-scoped deterministic content trace and replay。**

## 2. 前置依赖

必须先完成主同步重构的以下能力：

1. 单 WebSocket Transport；
2. `session_index/{projectID}` 与 `session_content/{sessionID}` Resource Provider；
3. atomic snapshot barrier；
4. subscription sequence、resource revision、run cursor 分离；
5. durable change 与 transient `subscription_event` 分离；
6. 前端 Local Replica；
7. Resource Adapter；
8. Interest Policy 与 Subscription Manager；
9. typed Command Facade；
10. Blob descriptor 和 Blob client。

Trace Replay 必须复用上述正式组件，不能创建第二套 Session 数据归并实现。

## 3. 设计原则

### 3.1 Session content 优先

主要记录对象是：

```text
session_content/{sessionID}
```

`session_index` 只补充记录目标 Session 的摘要、运行状态和未读状态，不记录同项目其他 Session 的完整数据。

### 3.2 页面透明

正常页面不能知道 Trace Recorder 是否启用。Trace 属于独立诊断模块：

```text
Go Session Trace Recorder
    -> Trace Bundle
    -> TraceReplayInput
    -> 正式 Sync Runtime
    -> 正式 Resource Adapter
    -> Local Replica
    -> Repository / Selector / Page
```

### 3.3 有基线才能回放

开启 Trace 时必须生成原子 Session baseline，并从同一个 barrier 之后记录增量。禁止只记录后续 delta。

### 3.4 顺序概念分离

每条 Trace record 使用独立、严格递增的 `trace_sequence`。不得用以下字段替代：

- subscription `sequence`；
- Session `resource_revision`；
- `run_cursor`；
- item creation seq；
- wall clock timestamp。

### 3.5 不静默丢失

Recorder 无法完整记录时必须：

- 写入 `trace.incomplete`；
- 在 manifest 标记 `complete: false`；
- 向诊断 UI 显示原因；
- 停止 Trace 或显式降级。

不能丢失记录后仍声称 Trace 可完整回放。

## 4. Debug 配置

建议领域配置：

```typescript
interface SessionDataTraceSettings {
  enabled: boolean
  capture: 'metadata' | 'content' | 'content_and_wire'
  includeReasoning: boolean
  includeToolPayloads: boolean
  blobMode: 'reference' | 'pin' | 'copy'
  maxBytes: number
  retentionHours: number
}
```

推荐完整诊断预设：

```json
{
  "enabled": true,
  "capture": "content_and_wire",
  "includeReasoning": true,
  "includeToolPayloads": true,
  "blobMode": "copy",
  "maxBytes": 536870912,
  "retentionHours": 24
}
```

默认必须关闭。开启 full/content 模式时，UI 必须提示 Trace 可能包含用户输入、模型输出、reasoning、工具参数、本地路径和文件内容。

## 5. Trace 层次

### 5.1 Canonical Session Content Trace

这是稳定回放主体，记录类型化、按 Session 过滤的规范事件：

```text
trace.started
trace.baseline
trace.checkpoint
trace.final_state
trace.stopped
trace.incomplete

session.metadata.replace
session.item.upsert
session.item.remove
session.history.page
session.active_history.replace
session.compaction.*
session.transient.*
session.tool.*
session.run.*
session.blob.*
session.index_summary.upsert
session.index_summary.remove
```

Canonical Trace 不依赖某个具体浏览器 connection 或 subscription ID。

### 5.2 Wire Delivery Trace

`content_and_wire` 模式额外记录：

```text
wire.connection.opened
wire.connection.closed
wire.subscription.opened
wire.subscription.closed
wire.message.queued
wire.message.sent
wire.message.received
wire.message.rejected
wire.queue.overflow
client.ack.received
sync.resync.required
```

用途是区分：

```text
Durable store 正确，projection 错误
Projection 正确，queue/delivery 错误
Delivery 正确，前端 Replica/selector 错误
```

Wire Trace 可包含多个标签页的重复投递，因此必须带 `connection_id` 和 connection generation。

## 6. 开启时的原子 Baseline

开启顺序：

```text
1. 注册 paused trace observer
2. 捕获 immutable Session projection view
3. 捕获当前 resource revision、stream sequence、active run cursor
4. 在锁外序列化 baseline
5. 写 trace.started
6. 写 trace.baseline
7. 按 trace_sequence 写 barrier 之后已缓冲事件
8. 切换为 live recording
```

Baseline 至少包含：

```text
Session metadata
Session resource revision
最近一页 durable history
history oldest/newest cursor
has_more_before/after
active history
compaction/context summary
active run descriptor
当前 run cursor
当前 durable checkpoint 信息
目标 Session 的 index summary
```

示例：

```json
{
  "trace_version": 1,
  "trace_id": "trace_session_123_01J...",
  "trace_sequence": "1",
  "recorded_at": "2025-03-08T12:00:00.123456Z",
  "elapsed_ns": "0",
  "stage": "trace.baseline",
  "session_id": "session_123",
  "project_id": "project_1",
  "resource_revision": "42",
  "payload": {
    "session": {},
    "history": {
      "items": [],
      "oldest_seq": 120,
      "newest_seq": 180,
      "has_more_before": true,
      "has_more_after": false
    },
    "active_history": {},
    "active_run": {
      "run_id": "run_9",
      "run_cursor": "17",
      "status": "running"
    }
  }
}
```

大 Baseline 保存为 Trace Blob，但 manifest/record 必须带大小和 SHA-256。

## 7. 必须记录的 Session 数据

### 7.1 Command 和用户输入

```text
command.received
command.accepted
command.completed
command.failed
```

只记录 target 为该 Session 的命令：

```text
session.rename
session.mark_read
session.compact
session.history.read
run.start
run.continue
run.cancel
run.prompt.append/remove/steer/move
run.tool.cancel
```

字段至少包括：

```text
request_id
command name/schema version
trace_id
expected_revision
run_id/turn_id（如有）
result status/error
```

### 7.2 Durable Item

记录：

```text
item.created
item.updated
item.removed
active_history.replaced
compaction committed records
```

必须明确区分：

```text
item_id             稳定 Item 身份
item_seq            Item 创建序列
record_seq          这次 durable record 序列
resource_revision   Session 当前 durable 版本
trace_sequence      Trace 回放顺序
```

代表记录：

```json
{
  "stage": "session.item.upsert",
  "trace_sequence": "28",
  "session_id": "session_123",
  "run_id": "run_9",
  "turn_id": "turn_3",
  "agent_iteration": 1,
  "item_id": "item_7",
  "item_seq": "132",
  "record_seq": "145",
  "resource_revision": "147",
  "payload": {
    "item": {}
  }
}
```

### 7.3 流式文本和 Reasoning

记录：

```text
text.delta
reasoning.delta
assistant durable checkpoint
terminal flush
checkpoint failure
```

文本 delta 至少携带：

```text
run_id
run_cursor
turn_id
agent_iteration
item_id
durable_text_length
durable_checkpointed
delta
```

这些字段用于验证 durable item 到达后 transient tail 是否被正确截断，以及同一 assistant item 是否出现重复 bubble。

### 7.4 Tool 生命周期

记录完整生命周期：

```text
tool.requested
tool.running
tool.progress
tool.finished
tool.cancelled
tool.failed
durable tool item committed
```

每条至少包含：

```text
session_id
run_id
run_cursor
turn_id
agent_iteration
tool_call_id
tool_name
item_id（如有）
```

metadata 模式只保存工具 payload 的大小和哈希；content 模式按配置保存正文并执行 secret redaction。

### 7.5 Run 生命周期和 Settlement

记录：

```text
run.started
run.settling
run.settled
settlement durable watermark
对应 durable change generated
对应 delivery/ACK
```

代表记录：

```json
{
  "stage": "session.run.settled",
  "trace_sequence": "95",
  "session_id": "session_123",
  "run_id": "run_9",
  "payload": {
    "status": "committed",
    "committed_revision": "173",
    "last_run_cursor": "81"
  }
}
```

回放必须验证：只有 Local Replica 已应用的 Session resource revision 覆盖 `committed_revision` 后，才清除该 Run 的 transient overlay。

### 7.6 历史分页

记录 `session.history.read` 的请求和结果：

```text
before_seq/after_seq
limit
align_turn
returned oldest/newest seq
has_more_before/after
完整 items 或 Blob descriptor
```

用于重现 overlap merge、Turn 对齐、旧历史被新 snapshot 覆盖等问题。分页结果不推进 live subscription sequence，但必须按 `trace_sequence` 记录到正确交互位置。

### 7.7 Compaction

记录：

```text
compaction.command
compaction.started
summary committed
active_history replaced
resource revision advanced
compaction.settled
session change/snapshot
```

不能只记录页面可见 Item；active history、隐藏 summary 和 durable revision 都必须进入 Canonical Trace。

### 7.8 Blob 和图片

记录：

```text
blob ID
content type
size
SHA-256
ETag
引用它的 item/snapshot/history page
blob mode 和保存结果
```

模式：

- `reference`：只保存 descriptor；
- `pin`：Trace 存续期间禁止原 Blob GC；
- `copy`：复制到 Trace bundle，支持独立导出和回放。

无法保存 Blob 时必须记录 `session.blob.missing` 并把 Trace 标记为不完整。

## 8. Trace Record 格式

建议 append-only JSONL：

```json
{
  "trace_version": 1,
  "trace_id": "trace_session_123_01J...",
  "trace_sequence": "184",
  "recorded_at": "2025-03-08T12:00:00.123456Z",
  "elapsed_ns": "382771923",
  "stage": "sync.change.generated",
  "session_id": "session_123",
  "project_id": "project_1",
  "run_id": "run_9",
  "turn_id": "turn_3",
  "item_id": "item_7",
  "resource_revision": "173",
  "payload": {},
  "delivery": {
    "connection_id": "conn_3",
    "connection_generation": 2,
    "subscription_id": "session-content:session_123",
    "stream_epoch": "stream_1",
    "sequence": "416",
    "message_id": "msg_72"
  }
}
```

规则：

- `trace_sequence` 使用十进制字符串；
- 回放排序只依赖 `trace_sequence`；
- `recorded_at` 只用于人工诊断；
- `elapsed_ns` 用于原始时序回放；
- 可选字段为空时省略；
- payload 使用 typed stage-specific schema；
- 每个 Trace 格式有独立 `trace_version`，不直接等同于 wire protocol version。

## 9. 存储格式和生命周期

```text
<server-root>/logs/sync-traces/
  session_123/
    trace_01J.../
      manifest.json
      records.jsonl
      blobs/
        baseline-0001.json
        image-sha256...
        result-sha256...
      checkpoints/
        replica-0001.json
      checksums.sha256
```

Manifest：

```json
{
  "trace_version": 1,
  "protocol_version": 1,
  "application_version": "v0.2.0",
  "trace_id": "trace_session_123_01J...",
  "session_id": "session_123",
  "project_id": "project_1",
  "started_at": "...",
  "completed_at": "...",
  "complete": true,
  "record_count": 4928,
  "contains_sensitive_content": true,
  "redaction_policy": "default-v1",
  "blob_mode": "copy",
  "server_epoch": "..."
}
```

要求：

- 文件权限仅当前用户；
- 按 `maxBytes` 和 `retentionHours` 回收；
- Session 永久删除时默认同时删除 Trace；
- 支持手动停止、删除和导出 zip；
- bundle 使用 SHA-256 manifest；
- 不记录 capability token、WS ticket、Authorization、Provider API key。

## 10. 写入架构和完整性

建议内部接口：

```go
type SessionSyncTraceSink interface {
    Enabled(sessionID string) bool
    Record(ctx context.Context, record TraceRecord)
}
```

统一埋点位置：

```text
Command Dispatcher
Durable Session Projector
Run Coordinator
Sync Engine
SessionContent Resource Provider
Connection Queue / ACK Handler
Blob Manager
```

写入链路：

```text
producer
  -> byte-aware bounded trace queue
  -> single ordered writer
  -> JSONL / trace blobs
```

完整性策略：

1. Canonical content records 优先于 Wire metadata；
2. writer 单线程保证 `trace_sequence` 顺序；
3. queue/磁盘达到上限时不阻塞 Agent 主执行路径；
4. 无法继续完整记录时写 `trace.incomplete`；
5. 若连 incomplete marker 都无法落盘，关闭 writer 并在 Session 诊断状态暴露错误；
6. 不提供默认“同步落盘每条事件”的法证模式，避免 Debug 改变运行时序；如以后需要，作为独立模式评估。

## 11. Replay 架构

抽象正式 Sync Runtime 输入：

```typescript
interface SyncInput {
  start(consumer: SyncRecordConsumer): Promise<void>
  stop(): void
}
```

生产：

```text
WebSocketInput
  -> Sync Runtime
  -> Resource Adapter
  -> Local Replica
```

回放：

```text
TraceReplayInput
  -> Sync Runtime
  -> 同一 Resource Adapter
  -> 同一 Local Replica
```

禁止 Replay 直接修改 React state，禁止为 Replay 实现另一套 Item/History/Tool 归并逻辑。

### 11.1 确定性快速回放

- 忽略真实时间；
- 严格按 `trace_sequence`；
- 支持 step、continue、pause、seek checkpoint；
- 用于自动化测试和最终 Replica hash 对比。

### 11.2 原始时序回放

- 使用 `elapsed_ns`；
- 支持 0.5x、1x、2x、10x；
- 用于重现迟到事件、settlement 顺序、流式渲染和 Blob 下载期间 change。

## 12. 回放自动验证

### 12.1 Item

```text
同一个 item_id 只有一个最终 Item
item.updated 不创建第二个 bubble
Item 按稳定 item seq 排序
turn_id/agent_iteration/item_id 身份一致
```

### 12.2 Transient

```text
durable_text_length 以内不重复显示
durable item 到达后 transient tail 正确截断
settled watermark 追平后 overlay 清除
未追平前 overlay 不提前清除
```

### 12.3 Tool

```text
tool_call_id 唯一
terminal 后不能继续显示 running
durable tool item 到达后 transient row 被归并
工具不能进入错误 turn/iteration
```

### 12.4 History

```text
分页 overlap 不产生重复 item
新 snapshot 不删除已加载的有效旧窗口
align_turn 页面不出现半个 Turn
compaction 后窗口和 revision 一致
```

### 12.5 最终状态

Trace 停止时记录：

```json
{
  "stage": "trace.final_state",
  "payload": {
    "resource_revision": "173",
    "session_content_sha256": "..."
  }
}
```

回放结束后计算 Local Replica Session content canonical hash；不一致时报告首个产生差异的 `trace_sequence`。

## 13. 诊断 UI

独立入口，不混入正常 Session 页面数据流：

```text
Session Settings
  -> Sync diagnostics
      Start recording
      Stop recording
      Capture level
      Blob mode
      Current size
      Complete / incomplete
      Export trace
      Open replay
      Delete trace
```

正常 Session 页面仍只通过 Repository/Selector 读取 Local Replica。

## 14. 开发阶段

### 阶段 T1：Trace schema 与 Recorder

- Trace record typed schema；
- manifest；
- trace sequence；
- bounded ordered writer；
- retention、size 和 incomplete；
- Session 开关和状态查询；
- secret redaction。

### 阶段 T2：Session Content 埋点

- atomic baseline；
- command；
- durable item；
- transient text/reasoning；
- Tool；
- Run settlement；
- compaction；
- history page；
- Blob manifest。

### 阶段 T3：Wire Delivery 埋点

- connection/subscription generation；
- queue/send；
- ACK；
- reconnect/resync；
- 多标签页关联。

### 阶段 T4：Trace Bundle 与 ReplayInput

- bundle reader/validator；
- Blob resolver；
- fast/timed replay；
- step/checkpoint；
- 正式 Resource Adapter 接入。

### 阶段 T5：自动验证与诊断 UI

- Replica hash；
- Item/transient/tool/history invariants；
- 首个差异定位；
- 录制管理、导出、删除和回放 UI；
- 敏感信息确认流程。

## 15. 验收条件

1. 可为单个 Session 独立开启和停止 Trace；
2. 开启时 baseline 与后续事件之间没有竞态缺口；
3. 未打开 Session 的 index 状态和已打开 Session 的 content 数据均可关联；
4. durable item、transient delta、Tool、Run、history、compaction 和 Blob 均有记录；
5. 每条记录有独立、连续的 `trace_sequence`；
6. Trace 不完整时不会静默标记为完整；
7. 未开启 Debug 时主路径开销只有一次快速 `Enabled(sessionID)` 判断或等价优化；
8. Trace Recorder 不阻塞 Agent/Sync 主执行路径；
9. ReplayInput 复用正式 Sync Runtime、Resource Adapter 和 Local Replica；
10. 快速回放结果的 canonical hash 与录制最终状态一致；
11. 可以重现 duplicate bubble、transient tail、Tool terminal、settlement watermark 和 history overlap 测试场景；
12. Trace 不包含 token、ticket、Authorization 或 Provider secret；
13. size、retention、删除和导出策略生效；
14. 正常页面不 import 或依赖 Trace 模块。
