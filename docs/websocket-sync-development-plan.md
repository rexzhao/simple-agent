# WebSocket 命令与数据同步彻底重构开发方案

> 本文采用 clean break：新协议不兼容现有 lifecycle SSE、per-run SSE、前端 lifecycle reducer 或 REST mutation 语义。开发完成后一次切换，不提供双写、shadow、feature flag 或旧协议回退。

## 1. 相对原方案的关键修改

彻底重构后，原渐进迁移方案需要做以下调整：

1. **不再把现有 `LifecycleHub` 当作新同步层基础。**它只提供 best-effort fan-out，没有统一序列、原子 snapshot barrier 和通用资源模型。新建独立 Sync Engine，业务层只发布 typed domain changes。
2. **不再复用 SSE wire event。**删除 `/api/events`、`/api/runs/{runID}/events` 以及前端 `streamLifecycle`、`streamRun`。旧事件可以作为 Go 内部适配输入短期存在，但不能进入新协议 DTO。
3. **不做 SSE/WS 双路并行。**双路会制造重复事件、两套恢复规则和两套权威 Store。新前端只能通过一个 WebSocket 实时连接驱动状态。
4. **不再让 Session `LastSeq` 同时承担同步投递序列。**新协议明确分离：
   - `sequence`：订阅流连续性、replay 和 ACK；
   - `resource_revision`：资源本身版本和乐观并发；
   - `run_cursor`：Run 瞬时事件恢复。
5. **不在 `App.tsx` 或页面组件内继续堆叠事件归并和订阅逻辑。**重建 transport、Sync Runtime、Local Replica、Repository 和 command facade；页面只读取领域 selector。
6. **不保留 REST mutation 作为正式路径。**项目、Session、Run、Provider 操作统一走 WebSocket command；HTTP 只保留静态资源、bootstrap、WS ticket、Blob 和确有必要的外部认证回调。
7. **不按现有 endpoint 逐个翻译命令。**先定义 application command registry，再让 UI 和业务服务依赖该 registry，避免 HTTP handler 结构进入新架构。
8. **大数据边界提前进入协议。**所有 snapshot/result 都必须在序列化前判断 inline/Blob，不能等到后期再改消息结构。
9. **重连不盲目重发所有命令。**同一 server epoch 内按 `request_id` 幂等重发；epoch 改变后先重建订阅并对账，只有声明为跨 epoch 安全的命令才自动重试。
10. **开发阶段仍分层交付，但产品只进行一次 cutover。**阶段之间用于编译和测试，不形成两套生产通信模式。

## 2. 目标架构

```text
┌──────────────────────────── Web ────────────────────────────┐
│ React Components                                            │
│      │ domain selectors / typed commands                    │
│      ▼                                                      │
│ Domain Repository / Command Facade                          │
│      ▲                                                      │
│      │                                                      │
│ Local Replica ◄── Resource Adapters ◄── Client Sync Runtime │
│      ▲                                      │               │
│ Application State ──► Interest Policy ──────┤               │
│                                             ▼               │
│                              Single WebSocket Transport      │
└─────────────────────────────────────────────┬───────────────┘
                                              │
┌──────────────────────────── Go ─────────────▼───────────────┐
│ WS Gateway                                                   │
│   ├── Protocol Decoder / Validator                           │
│   ├── Connection Writer Queue                               │
│   ├── Command Dispatcher                                    │
│   └── Subscription Coordinator                              │
│             │                                               │
│             ▼                                               │
│ Sync Engine                                                  │
│   ├── Resource Provider Registry                            │
│   ├── Atomic Snapshot Barrier                               │
│   ├── Per-resource Stream Journal                           │
│   ├── Replay / ACK / Resync                                 │
│   └── Blob Descriptor                                       │
│             │                                               │
│             ▼                                               │
│ Application Services / Session Projector / Run Coordinator  │
│             │                                               │
│             ▼                                               │
│ Durable Stores                                               │
└─────────────────────────────────────────────────────────────┘
                         │ HTTP
                         ▼
                  Immutable Blob Store
```

总体规则：

- WebSocket 是应用控制平面；
- HTTP 是 Blob 数据平面；
- Go durable store 是事实源；
- Sync Engine 是 Web 可消费投影及连续增量的唯一出口；
- transient event 不是 durable state，二者不能共享 revision；
- 一个浏览器标签页只有一个 WebSocket。

### 2.1 页面透明性原则

页面不能成为同步协议的参与者。理想依赖方向是：

```text
WebSocket / Blob
      -> Client Sync Runtime
      -> Local Replica（规范化本地数据副本）
      -> Domain Repository / Selector
      -> React Page
```

页面允许知道：

- 需要展示哪个 project/session；
- 当前领域数据是 loading、ready、stale 还是 error；
- 可以调用哪些领域 command。

页面不允许知道：

- subscription ID、resource key 和 subscribe/unsubscribe；
- stream epoch、sequence、ACK、replay 和 resync；
- 数据来自 snapshot、change、WebSocket 还是 Blob；
- WebSocket 是否发生过重连；
- durable change 与 transient event 如何归并。

“打开 Session 才同步内容”仍然成立，但由独立的 Interest Policy Engine 观察应用导航状态后执行，不由 Session 页面在 mount effect 中手工订阅。对页面来说，`SessionRepository` 中的数据始终是唯一读取入口。

## 3. 协议中的三个版本概念

### 3.1 Subscription sequence

每个可恢复订阅流使用：

```json
{
  "stream_epoch": "01JSTREAM...",
  "sequence": "1939",
  "previous_sequence": "1938"
}
```

用途：

- 判断 change 是否连续；
- 断线 replay；
- ACK；
- 慢客户端检测。

特性：

- 在一个 stream epoch 内严格递增；
- 即使只是 metadata 变化也必须递增；
- 使用十进制字符串传输，前端用 `BigInt`；
- 进程重启可以产生新 epoch，客户端看到 epoch 改变后强制 snapshot；
- 不暴露为业务对象字段。

### 3.2 Resource revision

```json
{
  "resource_revision": "42"
}
```

用途：

- 资源新旧比较；
- command 的 `expected_revision`；
- Run settlement 的 durable watermark；
- 快照和业务状态一致性诊断。

Session 可以继续使用 `LastSeq` 的十进制字符串作为 `resource_revision`。项目索引、Provider 设置等资源可采用各自 revision；协议将其视为 opaque decimal/string token，不假设不同资源可以比较。

### 3.3 Run cursor

```json
{
  "run_id": "run_1",
  "run_cursor": "17"
}
```

仅用于同一个 Run 的 transient event replay。它与 subscription sequence、Session resource revision 都没有数值关系。

## 4. 通用资源模型

### 4.1 Resource key

```json
{
  "type": "session_index",
  "id": "project_1"
}
```

V1 资源目录：

| Resource type | Resource ID | 用途 |
|---|---|---|
| `project_index` | `server` | 项目导航、归档状态 |
| `session_index` | project ID | 项目内 Session 摘要、运行状态、未读状态 |
| `session_content` | session ID | Session metadata、最近历史、durable item、active run |
| `provider_settings` | `server` | Provider 配置及默认模型 |
| `model_catalog` | project ID | 当前项目可用 provider/model/reasoning options |
| `codex_login` | provider name | 设备登录状态；仅在相关 UI 打开时订阅 |

不为了“通用”而允许任意动态 JSON 资源。每种 resource type 都必须注册 typed provider、权限检查、snapshot DTO 和 change operation。

### 4.2 Resource provider contract

Go 内部接口建议为：

```go
type ResourceProvider interface {
    Type() string
    Authorize(ctx context.Context, principal Principal, key ResourceKey) error
    Open(ctx context.Context, key ResourceKey, resume *ResumeToken) (OpenedResource, error)
}

type OpenedResource struct {
    Snapshot         SnapshotPayload
    StreamEpoch      string
    Sequence         uint64
    ResourceRevision string
    Changes          <-chan ResourceChange
    Close            func()
}
```

`Open` 必须提供原子 snapshot barrier：

1. 在 provider owner/锁内捕获 immutable projection view；
2. 同时捕获 stream epoch 和 sequence；
3. 注册 sequence 之后的 live change；
4. 释放锁；
5. 在锁外序列化 immutable view；
6. 先发送 snapshot，再发送 barrier 后缓存的 changes。

禁止采用“先 List，再 Subscribe”，否则两步之间必然存在丢事件窗口。

### 4.3 Collection change

列表型资源 V1 只支持：

```json
{
  "operations": [
    { "op": "upsert", "key": "session_1", "value": {} },
    { "op": "remove", "key": "session_2" }
  ]
}
```

规则：

- `upsert.value` 是完整列表项，不是依赖旧缓存的 partial patch；
- 排序由客户端按 DTO 字段执行，不同步数组下标；
- 一条 change 可以包含多个原子 operations；
- 客户端只有在 `previous_sequence == local_sequence` 时应用。

### 4.4 Entity change

`session_content` 使用 typed operations：

```text
metadata.replace
item.upsert
item.remove
history.window.replace (legacy input only; D1 does not emit a full window)
history.window.descriptor.replace
active_run.replace
active_run.clear
compaction.replace
```

不采用无限制 JSON Patch。所有操作由 schema 校验，并以 stable item ID 定位，不能按文本或数组位置匹配。

D1 不在 live change 中重复发送可能很大的完整 history window。每个窗口变化先发送 `item.upsert`/`item.remove`，并在同一 Change 中发送有界的 `history.window.descriptor.replace`：该操作只替换 `before/after` cursor、`oldest/newest`、`has_more`、`limit` 等 descriptor 字段。客户端按 stable `(turn_id, agent_iteration, item_id)` 应用同一 Change 内的 item 操作，再原子替换 descriptor；不得从数组下标推导边界。`history.window.replace` 仅作为兼容性 schema 名称保留，D1 provider 不产生完整 window operation。

## 5. Wire protocol

### 5.1 Envelope

```json
{
  "version": 1,
  "type": "subscribe",
  "id": "msg_01J...",
  "timestamp": "2025-03-08T12:00:00Z",
  "trace_id": "trace_01J...",
  "payload": {}
}
```

规则：

- `version` 是协议大版本；
- `id` 在发送方范围唯一；
- `timestamp` 仅用于日志，不能排序；
- 未知可选字段忽略；
- 未知 type、非法必填字段、超大消息返回稳定错误码；
- 控制消息只使用 JSON text frame；Blob 不使用 binary frame。

V1 消息集合：

```text
hello
welcome
ping
pong

command
command_accepted
command_result

subscribe
subscribed
unsubscribe
unsubscribed
snapshot
change
subscription_event
ack
resync_required

error
```

### 5.2 握手与鉴权

保留最小 HTTP 基础设施：

```text
GET  /api/bootstrap
POST /api/ws-ticket
GET  /api/ws?ticket=...
GET  /api/blobs/{blobID}
HEAD /api/blobs/{blobID}
```

浏览器先用当前 Bearer capability 调用 `/api/ws-ticket`，再使用 30 秒有效、一次性 ticket Upgrade。不能把长期 capability token 放在 WS URL 中。

客户端首帧：

```json
{
  "version": 1,
  "type": "hello",
  "id": "hello_1",
  "payload": {
    "supported_versions": [1],
    "client_id": "tab_1"
  }
}
```

服务端：

```json
{
  "version": 1,
  "type": "welcome",
  "id": "welcome_1",
  "payload": {
    "selected_version": 1,
    "connection_id": "conn_1",
    "server_epoch": "server_epoch_1",
    "heartbeat_interval_ms": 15000,
    "max_message_bytes": 262144
  }
}
```

`server_epoch` 表示命令幂等缓存和进程级恢复边界；每个 resource 的 `stream_epoch` 仍独立存在。

### 5.3 Subscribe

```json
{
  "version": 1,
  "type": "subscribe",
  "id": "sub_request_1",
  "payload": {
    "subscription_id": "session-index:project_1",
    "resource": { "type": "session_index", "id": "project_1" },
    "resume": {
      "stream_epoch": "stream_1",
      "sequence": "1938"
    }
  }
}
```

响应：

- resume 可覆盖：`subscribed` 后 replay `change`；
- resume 不可覆盖：`subscribed` -> `resync_required` -> `snapshot`；
- 首次订阅：`subscribed` -> `snapshot`；
- 无权访问：`error`，不创建订阅。

### 5.4 Snapshot

```json
{
  "version": 1,
  "type": "snapshot",
  "id": "server_msg_1",
  "payload": {
    "subscription_id": "session-index:project_1",
    "resource": { "type": "session_index", "id": "project_1" },
    "stream_epoch": "stream_1",
    "sequence": "1938",
    "resource_revision": "718",
    "content": {
      "inline": { "sessions": [] }
    }
  }
}
```

大快照：

```json
{
  "content": {
    "blob": {
      "id": "blob_1",
      "url": "/api/blobs/blob_1",
      "content_type": "application/json",
      "size": 28734912,
      "sha256": "...",
      "etag": "...",
      "expires_at": "2025-03-08T12:10:00Z"
    }
  }
}
```

客户端获取 Blob 期间，服务端继续缓冲 snapshot sequence 之后的 changes。客户端只有在 Blob 校验、snapshot apply 和后续 changes apply 完成后才 ACK 最新 sequence。

### 5.5 Durable change

```json
{
  "version": 1,
  "type": "change",
  "id": "server_msg_2",
  "trace_id": "trace_run_1",
  "payload": {
    "subscription_id": "session-index:project_1",
    "resource": { "type": "session_index", "id": "project_1" },
    "stream_epoch": "stream_1",
    "sequence": "1939",
    "previous_sequence": "1938",
    "resource_revision": "719",
    "operations": [
      {
        "op": "upsert",
        "key": "session_2",
        "value": {
          "session_id": "session_2",
          "run_id": "run_9",
          "status": "completed",
          "has_unread_result": true
        }
      }
    ]
  }
}
```

### 5.6 Transient subscription event

```json
{
  "version": 1,
  "type": "subscription_event",
  "id": "server_msg_3",
  "payload": {
    "subscription_id": "session-content:session_2",
    "resource": { "type": "session_content", "id": "session_2" },
    "event": {
      "type": "text.delta",
      "run_id": "run_9",
      "run_cursor": "17",
      "item_id": "item_3",
      "durable_text_length": 128,
      "delta": "..."
    }
  }
}
```

规则：

- transient event 不推进 subscription sequence；
- durable `item.upsert` 最终覆盖同 item 的 transient tail；
- 断线后只在 Run replay buffer 仍覆盖 cursor 时补发；
- 无法补发时发送 `resync_required`，客户端重新加载 Session snapshot 和 active run descriptor；
- 未订阅 `session_content` 的连接不接收该 Session 的 transient event。

V1 的默认 text frame 上限由 `protocol.DefaultMaxMessageBytes` 与 gateway
共享（当前为 256 KiB）。Resource provider 在写入 journal 前按完整
`protocol.ChangeMessage` envelope（含 subscription/resource/epoch/sequence、
revision 和 envelope ID 的合法上界）做 preflight；`MaxChangeMessageBytes`
必须不大于 journal byte bound。超过上限的 durable mutation 只使该 resource
进入 bounded resync/error，不得让 gateway 因发送超大 frame 而关闭整条连接。

### 5.7 Command

```json
{
  "version": 1,
  "type": "command",
  "id": "command_msg_1",
  "trace_id": "trace_1",
  "payload": {
    "name": "session.rename",
    "schema_version": 1,
    "request_id": "request_01J...",
    "expected_revision": "42",
    "arguments": {
      "session_id": "session_2",
      "display_name": "新标题"
    }
  }
}
```

命令状态：

```text
command -> command_accepted -> command_result
command -> command_result
```

`command_result` 只包含：

- succeeded/failed；
- 创建对象的稳定 ID；
- 小型一次性结果；
- 大型结果的 Blob descriptor；
- typed error。

资源权威状态必须通过对应订阅的 change/snapshot 体现，不能要求前端把 command result 手工写入 Store。

命令注册表必须定义：

```go
type CommandDefinition struct {
    Name                 string
    SchemaVersion        int
    CrossEpochRetrySafe  bool
    Validate             func(json.RawMessage) error
    Execute              CommandHandler
}
```

同 server epoch 内，`request_id` 缓存执行中 promise 和最终 result。相同 request ID 携带不同 command fingerprint 返回 `idempotency_conflict`。

跨 epoch 规则：

- `rename/set/archive/restore/cancel` 等天然幂等命令可以声明安全重试；
- `create/start/append` 必须使用客户端生成的稳定 entity/operation ID，或在 durable store 中记录 request ID 后才能声明安全重试；
- 未声明安全的 pending command 在 epoch 变化后不自动重发，先通过订阅 snapshot 对账；仍无法判断时显示明确的 `outcome_unknown`，不能静默重复执行。

## 6. Session 资源设计

### 6.1 `session_index/{projectID}` 是常驻订阅

它由前端 Project/Session coordinator 自动声明，不依赖“当前打开 Session”。只要项目在 UI 导航范围内，就维持订阅。

完整 summary：

```text
session_id
project_id
parent_session_id
display_name
archived
status                 idle | queued | running | completed | failed | interrupted
run_id
resource_revision
updated_at
has_unread_result
```

状态 change 必须包含完整 summary 和 run ID。旧 Run 的迟到事件只有在匹配 summary 当前 run ID 时才能改变运行状态。

完成事件处理：

```text
Session B 未打开
  -> session_index upsert(B, completed, unread=true)
  -> SessionIndexStore 更新
  -> UI 列表、badge、通知更新
  -> 不加载 B 的完整 conversation
```

### 6.2 `session_content/{sessionID}` 按需订阅

打开 Session 时订阅，离开时取消。snapshot 包含：

```text
session metadata
resource_revision
最近一页 durable history
分页 cursor
active run descriptor（可选）
compaction/context summary
```

持续 change：

```text
metadata.replace
item.upsert
item.remove
active_run.replace
active_run.clear
history.window.descriptor.replace
history/compaction revision change
```

持续 transient event：

```text
text.delta
reasoning.delta
tool.requested
tool.running
tool.progress
tool.finished
prompt queue changes that are not durable projection state
```

如果 Session snapshot 超过 64 KiB，返回 Blob descriptor。历史向前分页使用幂等 `session.history.read` command；结果小则 inline，大则 Blob。分页结果按 stable item ID/seq 归并，不推进 live subscription sequence。

### 6.3 未读状态

`has_unread_result` 建议成为服务端状态，而不是单标签页本地状态。打开 Session 后发送：

```text
session.mark_read(session_id, run_id)
```

随后通过 `session_index` change 向所有标签页更新。多标签页桌面通知去重由前端 BroadcastChannel/Web Lock 负责，但 Store 状态来自服务端。

## 7. 后端彻底重构

### 7.1 新目录

```text
internal/protocol/
  envelope.go
  messages.go
  errors.go
  validate.go

internal/webapp/ws/
  ticket.go
  handler.go
  connection.go
  reader.go
  writer.go
  heartbeat.go

internal/syncengine/
  engine.go
  provider.go
  subscription.go
  journal.go
  snapshot.go
  blob.go
  resources/
    project_index.go
    session_index.go
    session_content.go
    provider_settings.go
    model_catalog.go
    codex_login.go

internal/commands/
  registry.go
  dispatcher.go
  idempotency.go
  project.go
  session.go
  run.go
  provider.go
```

### 7.2 不继续扩展 `execution.LifecycleHub`

新执行链路调整为：

```text
Application command
  -> domain service mutation
  -> durable commit
  -> typed committed domain change
  -> Sync Engine resource projector
  -> allocate subscription sequence
  -> journal
  -> WebSocket subscribers
```

内部 change 必须是 typed struct，禁止继续用 `map[string]any + json.RawMessage` 在业务层传播。

进程崩溃边界：当前产品是单进程 loopback 服务。进程崩溃会同时关闭全部 WebSocket；重启后生成新 stream epoch，客户端通过 durable snapshot 恢复。因此 V1 不要求跨进程持久保存 WS journal。但必须满足：

- durable commit 成功后 projector 失败：对应 resource 标记 invalid，所有订阅者收到 resync；
- projector 不允许阻塞执行主路径；
- resource snapshot 永远从 durable source/已验证 projection 重建；
- 未来多进程化时，再把 committed change/outbox 持久化。

### 7.3 SessionIndex projector

- 启动时从 durable project/session stores 构建 immutable projection；
- 每个 project 一个 owner、stream epoch、sequence 和 bounded journal；
- 所有 committed Session/Run lifecycle change 在 owner 中串行应用；
- snapshot 捕获 immutable map pointer 和 sequence 后在锁外编码；
- 默认 journal 上限：4096 changes 或 8 MiB；
- 超出 replay 范围返回 resync，不为单个慢客户端永久保留数据。

### 7.4 SessionContent provider

- Session durable record projector直接生成 typed `item.upsert`；
- item identity 延续 `(turn_id, agent_iteration, item_id)`；
- Session `LastSeq` 只作为 resource revision；
- Run coordinator 把 transient event 投递给 Sync Engine，不再拥有 HTTP SSE renderer；
- provider 维护 sessionID -> subscribers 索引，无订阅者时不进行 Web 协议 JSON 编码；
- run replay buffer 可保留，但通过 provider 暴露，不再暴露 SSE cursor endpoint。

### 7.5 Connection 模型

每连接固定：

- 一个 reader goroutine；
- 一个 writer goroutine，独占 socket 写；
- 一个 connection context；
- 一个有界 byte-aware queue；
- 一个 subscription map；
- 一个 inflight command map。

默认限制：

```text
inbound message             256 KiB
outbound queued data        8 MiB / 1024 messages
subscriptions               64
inflight commands           16
heartbeat                   15s
pong timeout                45s
inline payload threshold    64 KiB
```

queue 满时不能丢 change 后继续发送后续 sequence。处理顺序：

1. 标记相关 subscription desynced；
2. 清除其未发送 change；
3. 尝试发送 `resync_required`；
4. 控制消息也无法入队则关闭连接。

WebSocket 实现建议封装 `github.com/coder/websocket`，协议和业务层不能直接依赖第三方 connection 类型。

## 8. 前端彻底重构：Local Replica 优先

### 8.1 分层和依赖约束

前端不直接建立“页面 -> Subscription Manager”的依赖，而采用六层结构：

```text
1. Transport
   WebSocket、ticket、重连、frame 收发

2. Sync Runtime
   subscribe、snapshot、change、ACK、replay、resync、Blob

3. Resource Adapter
   把 wire resource operation 转成客户端领域 mutation

4. Local Replica
   规范化实体、索引、history window、transient overlay、availability

5. Domain Repository / Selector
   面向页面提供稳定查询和领域 command facade

6. React Page
   只读取 selector 结果并触发领域意图
```

依赖只能向下。Transport/Sync Runtime 不能 import React；React page 不能 import protocol、transport 或 sync 包。建议通过 ESLint import boundary 或目录约定在 CI 中强制该规则。

### 8.2 新目录

```text
web/src/protocol/
  types.ts
  decode.ts
  errors.ts
  sequence.ts

web/src/transport/
  websocketClient.ts
  ticketClient.ts
  reconnect.ts

web/src/sync/
  runtime.ts
  subscriptionManager.ts
  interestPolicy.ts
  resourceAdapters.ts
  snapshotLoader.ts
  blobClient.ts

web/src/replica/
  database.ts
  transaction.ts
  availability.ts
  entities.ts
  indexes.ts
  transient.ts

web/src/repositories/
  projectRepository.ts
  sessionRepository.ts
  providerRepository.ts
  selectors.ts
  react.ts

web/src/commands/
  commandClient.ts
  projectCommands.ts
  sessionCommands.ts
  runCommands.ts
  providerCommands.ts
```

### 8.3 Local Replica 是页面唯一数据源

Local Replica 采用规范化结构，而不是按 WebSocket 消息保存：

```typescript
interface ReplicaState {
  projectsByID: Record<string, Project>
  sessionsByID: Record<string, SessionSummary>
  sessionIDsByProject: Record<string, string[]>
  sessionContentByID: Record<string, SessionContent>
  activeRunsBySessionID: Record<string, ActiveRunOverlay>
  providersByName: Record<string, Provider>
  availabilityByDomainKey: Record<string, DataAvailability>
}
```

协议元数据单独由 Sync Runtime 保存：

```typescript
interface SyncState {
  subscriptions: Record<string, SubscriptionState>
  streamEpochs: Record<string, string>
  sequences: Record<string, string>
  connectionGeneration: number
}
```

`ReplicaState` 中不能出现 subscription ID、ACK 或 WebSocket connection。这样页面和领域 selector 不会意外依赖传输细节。

Snapshot 和一批 operations 必须在 replica transaction 中原子 apply；transaction 完成后再一次性通知 selector subscribers，避免页面看到一半更新后的 Session 树或 history。

### 8.4 Data availability，而不是同步状态

异步数据不可能在首次访问前物理存在，因此页面仍需要可表达的数据可用性，但使用领域语义而不是协议语义：

```typescript
type DataAvailability =
  | { status: 'loading' }
  | { status: 'ready' }
  | { status: 'stale'; dataUpdatedAt: string }
  | { status: 'error'; error: DomainReadError }
```

页面可以展示 loading/error，但不能判断 `resyncing`、`replaying` 或 `blob_downloading`。这些状态由 repository 统一折叠为 loading/stale；如果本地已有数据，重连和 resync 期间优先返回 `stale + existing data`，避免页面闪空。

### 8.5 Interest Policy Engine

订阅策略由应用状态驱动，不由页面生命周期驱动：

```text
project_index/server
  -> 连接 ready 后永久需要

session_index/{projectID}
  -> project_index 中所有未归档/导航可见项目自动需要
  -> 保证非当前 Session 状态始终可见

session_content/{sessionID}
  -> selectedSessionID 改变时由 policy 自动切换
  -> active/pinned Session 可按策略额外保留

provider_settings/server
  -> 设置功能启用或首页需要模型信息时自动需要

codex_login/{provider}
  -> login workflow state 存在时自动需要
```

Policy 的输入是独立 `ApplicationState`（当前项目、当前 Session、打开的设置页、pinned sessions），输出是 desired resource set。Subscription Manager 只消费该 set。

这意味着页面从 A 切到 B 时只是更新 `selectedSessionID`；Policy 决定取消/保留 A、订阅 B。Session 页面自身不调用 subscribe/unsubscribe。

### 8.6 单连接状态机

```text
disconnected
  -> obtaining_ticket
  -> connecting
  -> handshaking
  -> ready
  -> reconnecting
```

要求：

- 每次连接有 generation；
- 旧 generation 的消息、promise 和 timer 不能写入新 Replica；
- 重连退避 200ms 到 2s，带 jitter；
- desired resource set 独立于 socket 生命周期；
- ready 后自动恢复全部 desired resources；
- server/stream epoch 改变时由 Runtime 放弃旧 sequence 并加载 snapshot；
- 页面和 Repository 不接收 reconnect 回调。

### 8.7 Subscription Manager

```typescript
interface DesiredSubscription {
  subscriptionID: string
  resource: ResourceKey
  phase: 'absent' | 'subscribing' | 'syncing' | 'ready' | 'resyncing'
  streamEpoch?: string
  sequence?: string
  generation: number
}
```

Manager 负责连接恢复、Blob snapshot 下载、连续性检查、ACK 和资源失效。该类型只存在于 `web/src/sync`，不从包边界导出给页面。

### 8.8 Resource Adapter

每个 wire resource type 对应一个 adapter：

```typescript
interface ResourceAdapter<TSnapshot, TOperation> {
  applySnapshot(tx: ReplicaTransaction, snapshot: TSnapshot): void
  applyOperations(tx: ReplicaTransaction, operations: TOperation[]): void
  applyTransient?(tx: ReplicaTransaction, event: SubscriptionEvent): void
  invalidate(tx: ReplicaTransaction, reason: string): void
}
```

Adapter 是协议 DTO 与领域实体之间唯一允许的转换层。页面 props、React component 和 Repository 不接收 raw wire DTO。

### 8.9 Repository 和页面 API

页面读取方式应类似：

```typescript
const sessionList = useProjectSessions(projectID)
const sessionView = useSessionView(sessionID)
const runState = useSessionRunState(sessionID)
```

这些 hooks 基于 repository selector 和 `useSyncExternalStore`，不发送网络请求。selector 应提供稳定引用和细粒度订阅，Session B 的状态变化不应导致 Session A conversation 全量重渲染。

非 React 代码使用：

```typescript
sessionRepository.get(sessionID)
sessionRepository.observe(sessionID, listener)
```

### 8.10 Store 边界

- Session index 只保存 summary/status/unread，不保存 conversation；
- Session content 保存 durable content 和 transient overlay；
- 取消 content 订阅后立即丢弃 transient overlay，durable window 可按 LRU 保留；
- durable item 根据 stable item ID 覆盖 transient tail；
- 旧 `App.tsx` lifecycle/run reducer 必须全部拆除；
- App 只维护导航/application state 和页面组合，不负责协议归并。

### 8.11 Command facade 与状态更新

页面不调用字符串型通用 dispatcher：

```typescript
await sessionCommands.rename(sessionID, displayName)
await runCommands.start(sessionID, input)
```

typed facade 在内部生成 request ID、expected revision 并调用 Command Client。成功只表示命令完成；页面不使用返回值手工 patch Replica，权威状态由 Sync Runtime 写入。

如果交互需要等待状态可见，由 facade 或 repository 提供领域方法：

```typescript
await sessionCommands.renameAndWait(sessionID, displayName)
```

内部观察 Replica predicate，不让页面接触 resource sequence，也不制造第二状态源。乐观 UI 如有需要，也由 command middleware 写入带 operation ID 的 overlay，并在 authoritative change 到达后统一清除。

## 9. HTTP/Blob 边界

重构完成后的正式 HTTP API：

```text
GET  /                         embedded web assets
GET  /api/bootstrap            版本和启动所需最小信息
POST /api/ws-ticket            WebSocket 一次性 ticket
GET  /api/ws                   WebSocket upgrade
GET  /api/blobs/{blobID}       Blob read
HEAD /api/blobs/{blobID}       Blob metadata
PUT/POST blob upload endpoints 仅在大型上传需求出现时增加
外部 provider auth callback     仅协议无法替代的浏览器跳转回调
```

原项目、Session、Run mutation REST 路由全部删除。普通状态查询通过 resource snapshot；一次性分页/发现请求通过 command result，过大时返回 Blob descriptor。

Blob 要求：

- immutable ID；
- Bearer capability 鉴权；
- `Content-Length`、`Content-Type`、`ETag`；
- Range；
- SHA-256；
- descriptor 过期和刷新；
- 未被引用的临时 Blob 回收；
- 不在日志记录 token 或完整 Blob 内容。

## 10. 开发执行顺序

本方案不做兼容迁移，但仍按依赖顺序开发。建议在独立重构分支完成，达到 cutover 条件后整体合并。

### 阶段 A：协议和 Sync Engine 内核

交付：

- Go/TS 协议类型；
- shared golden fixtures；
- resource provider contract；
- atomic snapshot barrier；
- stream journal、sequence、ACK、resync；
- inline/Blob content union；
- fake resource provider 测试。

此阶段不接 UI，不复用 SSE DTO。

### 阶段 B：WebSocket Gateway

交付：

- ticket；
- hello/welcome；
- reader/writer；
- heartbeat；
- queue limits；
- command/subscription dispatcher；
- 协议日志和指标。

使用 fake command/resource 完成端到端协议测试。

### 阶段 C：重建 Session Index

交付：

- typed Session summary；
- SessionIndex projector；
- Run started/settled 状态投影；
- 前端 SessionIndexStore；
- 自动常驻订阅；
- 未读状态命令和多标签页同步。

最先验证核心场景：当前打开 A，后台 B 完成，B 状态和未读立即更新。

### 阶段 D：重建 Session Content 和 Run 实时流

当前 D1 只交付后端 durable Session Content provider、严格 snapshot/typed durable
operations、replay/resync 与 HTTP Blob data plane；不包含 transient run stream 或前端
adapter。D2 再接入 bounded transient run stream、run cursor/settlement watermark 归并，
D3 再实现前端 SessionContentStore/Repository cutover。

交付：

- Session snapshot provider；
- durable item operations；
- active run recovery（D1 durable baseline）；
- transient run event（D2）；
- history Blob/分页 command（D2/D3，D1 只定义 bounded window/cursor contract）；
- 前端 SessionContentStore（D3）；
- settlement watermark 归并（D2）。

随后删除后端 per-run SSE renderer 和前端 run SSE parser，不保留双实现。

### 阶段 E：重建命令层

交付所有 typed commands：

```text
project.create/rename/archive/restore/delete
session.create/rename/archive/restore/delete/mark_read
session.set_full_access/set_debug/compact/history.read
run.start/continue/cancel
run.prompt.append/remove/steer/move
run.tool.cancel
provider.create/update/set_default/discover_models
codex_login.start/clear
```

高风险的 `create/start/append` 必须先完成稳定 operation ID 或 durable dedupe，再允许断线自动重试。

### 阶段 F：重写页面数据接入

- 删除 App lifecycle reducer；
- 删除轮询/重连 bootstrap 对账逻辑；
- 所有页面改用 resource hooks；
- 所有 mutation 改用 command client；
- 错误、loading、resync、offline UI 统一；
- 清理旧 `api.ts` 中除 bootstrap/ticket/blob 外的路径。

### 阶段 G：删除旧系统并 cutover

删除：

```text
GET /api/events
GET /api/runs/{runID}/events
internal/webapp/lifecycle_events.go
SSE frame writer/parser
web streamLifecycle
web streamRun
旧 lifecycle DTO/reducer
旧 REST mutation handlers/routes
仅服务旧传输的测试和文档
```

保留 domain service、durable Session projector、item identity 和已经验证的业务规则；删除的是 transport/sync 结构，不是重写 Agent 执行语义。

### 后续调试任务：固定项目的 `web.eval`

原先把 Session Content 同步 Trace/回放作为后续调试任务的方案已由
[`docs/web-eval-debug-tool-plan.md`](web-eval-debug-tool-plan.md) 取代。该任务独立于主同步
cutover，使用固定项目 `project-f25c5aac78f681b52aabf5c0`，只有服务端 Debug 总开关开启且
Session 属于该项目时才动态注册唯一的 Agent 工具 `web.eval`。

工具只接收 `code` 和可选的 bounded `timeout_ms`；Agent 通过任意 JavaScript 使用统一的
`window.__SAI_DEBUG__` 入口、当前页面的同源/DOM 能力以及其可读取的数据自行诊断。Go 侧只
维护一个当前 Web debug executor，执行固定在单一连接；断线、刷新或 epoch 变化失败且不自动
重放。该任务包含 executor lease、调试入口、broker/tool、bounded serializer、现有 HTTP Blob
结果面和最小诊断日志，以及并发、断线、超时、权限、非目标项目过滤和任意 JS 切换
Session/检查 DOM 与 Replica 的 E2E 验收。

完整 deterministic sync trace/replay、baseline/barrier 录制和独立 replay runtime 不属于该
后续任务交付目标。

## 11. 测试矩阵

### 11.1 协议

- Go 和 TS 读取同一 fixtures；
- 非法首帧、未知版本、未知 type、超大 frame；
- ticket 过期、重放、并发消费、非法 Origin；
- writer 单写、连接关闭、heartbeat timeout；
- subscription ID 冲突；
- command ID/fingerprint 冲突。

### 11.2 Sync Engine

- snapshot barrier 与并发 mutation 不丢 change；
- sequence 连续；
- metadata-only 变更也推进 sequence；
- replay 命中/过期；
- epoch 改变强制 snapshot；
- Blob 下载期间缓存后续 change；
- queue 满后不会跳过 sequence 继续发送；
- projector 失败使资源 invalid 并 resync；
- 慢订阅者不阻塞执行。

### 11.3 Session

- 非当前 Session started/settled 可见；
- Session B 未订阅 content 时不收到 B 的 delta；
- 切换 A -> B 后 A 的迟到 event 不污染 B；
- durable item 覆盖同 item transient tail，无重复 bubble；
- settled revision 追上前不清除 overlay；
- archive/delete cascade 原子更新 index；
- 断线期间完成后 replay 或 snapshot 恢复；
- 进程重启后新 epoch 从 durable store 恢复。

### 11.4 Command

- 同 epoch result 丢失后重发不重复执行；
- request ID 参数冲突被拒绝；
- expected revision 冲突；
- create/start/append 的 operation ID 去重；
- epoch 变化时 unsafe command 不自动重发；
- command success 后订阅最终能观察到权威状态。

### 11.5 E2E 和压力

```text
100 connections
每连接 20 subscriptions
10 concurrent runs with high-frequency deltas
慢消费者
频繁打开/关闭 Session
网络断开/恢复
Blob Range 和 hash 校验
Go race detector
浏览器 refresh 和多标签页
```

CI：

```sh
go test ./...
go test -race ./...
cd web && npm run check && npm run test && npm run build
cd web && npm run test:e2e
```

## 12. 可观测性

至少提供：

```text
ws_connections_current
ws_messages_total{direction,type}
ws_bytes_total{direction}
ws_queue_bytes
ws_protocol_errors_total{code}
sync_subscriptions_current{resource_type}
sync_changes_total{resource_type}
sync_replay_total{result}
sync_resync_total{reason,resource_type}
sync_snapshot_duration{resource_type,inline_or_blob}
command_duration{name,status}
command_idempotent_replay_total{name}
slow_client_disconnect_total
blob_bytes_total
```

日志关联字段：

```text
connection_id
client_id
message_id
trace_id
subscription_id
resource_type/resource_id
stream_epoch/sequence
resource_revision
request_id
session_id
run_id/run_cursor
```

不得记录 capability token、ticket、Provider secret 或大型 payload 正文。

## 13. Cutover 条件

一次性切换前必须全部满足：

1. Web 页面只有一个实时 WebSocket；
2. `session_index` 不依赖打开 Session，后台完成不会遗漏；
3. `session_content` 未订阅时不会推送详细内容；
4. 页面代码不 import protocol/transport/sync，不直接订阅资源；
5. 页面只通过 Repository/Selector 读取 Local Replica，通过 typed facade 发命令；
6. 重连、replay、resync 和 Blob snapshot 不要求页面参与，已有数据在恢复期间保持 stale 可读；
7. sequence、resource revision、run cursor 已完全分离；
8. snapshot barrier 并发测试证明没有 subscribe race；
9. durable item 和 transient tail 不重复、不丢 terminal 内容；
10. 高风险命令具备可靠去重，unsafe 跨 epoch 命令不会盲重试；
11. 大型 snapshot/result 只通过 Blob descriptor；
12. 慢客户端不会阻塞 Go 执行或无限占用内存；
13. 进程重启后能从 durable store 重建所有资源；
14. 现有项目、Session、Run、工具、取消、compaction、历史、Provider、登录和图片 E2E 全部通过；
15. 仓库中不再存在产品路径上的 SSE 和旧 REST mutation 数据源。

## 14. 推荐结论

既然允许完全不兼容，最重要的不是简单把 SSE frame 换成 WebSocket frame，而是同时完成三项结构性修正：

1. 建立真正的 Resource Provider + atomic snapshot barrier；
2. 分离 subscription sequence、resource revision 和 run cursor；
3. 让前端 Store 只由订阅驱动，command result 不再成为第二状态源。

这样重构后的协议才是一套独立、可扩展的数据同步基础设施，而不是当前 SSE/REST 行为的 WebSocket 封装。
