# Server 和 Web GUI 设计

本文记录未来 server + Web GUI 的独立功能设计。会话压缩和 session 存储的底层设计见
`docs/session-compaction.md`，本文只描述 server ownership、HTTP API、WebSocket 事件、
GUI 展示和交互语义。

## 目标

最终使用形态不应依赖 CLI REPL。CLI 可以继续作为 dev/debug 入口，但产品形态应是：

```text
sai server
-> 提供 HTTP API
-> 提供 WebSocket stream
-> Web GUI 通过浏览器使用
```

GUI 不直接读取 session 文件，也不一次性加载完整会话。server 是 session owner，GUI 只通过
server 获取 filtered / paginated view。

## 非目标

MVP 不要求：

- 多用户账号系统。
- 复杂权限模型。
- 协同编辑。
- 离线 GUI 直接读本地 session 文件。
- GUI 直接修改 `ActiveHistory`。
- GUI 直接读 blob hash。

## 架构原则

1. server 是唯一 session writer。
2. GUI 是 server state 的视图，不直接操作 session 文件。
3. 历史记录通过分页 HTTP API 获取。
4. 实时输出通过 WebSocket 获取。
5. 正在运行的 turn 先通过 transient events 展示。
6. 只有成功 turn 才持久化为 session records。
7. GUI 默认展示 chat view，debug view 才展示 hidden/model/internal items。
8. `ActiveHistory` 是 server 内部模型上下文投影，GUI 不反推、不修改。

## Session Ownership

server 负责：

- 创建 session。
- 读取和 replay session JSONL segments。
- 维护 `Items`。
- 维护 `ActiveHistory`。
- 执行 agent turn。
- 执行 compact command。
- 写入 blob。
- 做 blob 可达性校验。
- 提供分页 view。
- 推送 WebSocket events。

GUI 负责：

- 展示 session list。
- 展示一个 session 的 chat timeline。
- 发送用户输入。
- 展示 streaming 输出。
- 触发 command，例如 compact。
- 按需展开 tool result 或 blob 内容。
- 在 debug view 中展示 hidden/debug/internal 信息。

GUI 不负责：

- 直接读取 session 文件。
- 直接读取 blob 文件。
- 直接拼接 provider messages。
- 修改 `ActiveHistory`。
- 决定哪些 items 进入模型上下文。

## HTTP API

建议 API 形态：

```text
GET  /sessions
POST /sessions
GET  /sessions/{id}
GET  /sessions/{id}/items?before_seq=<seq>&limit=50&view=chat
GET  /sessions/{id}/items?after_seq=<seq>&limit=50&view=debug
POST /sessions/{id}/messages
POST /sessions/{id}/commands/compact
GET  /sessions/{id}/items/{item_id}/content
```

### Session List

`GET /sessions` 返回 session metadata，不返回完整 items。

建议字段：

```json
{
  "sessions": [
    {
      "id": "20260703T120000Z-abc123",
      "created_at": "2026-07-03T12:00:00Z",
      "updated_at": "2026-07-03T12:30:00Z",
      "provider": "codex",
      "model_profile": "default",
      "model_id": "gpt-5.5",
      "last_seq": 1234
    }
  ]
}
```

### Session Detail

`GET /sessions/{id}` 返回 metadata 和当前运行状态，不返回完整 timeline。

建议字段：

```json
{
  "id": "20260703T120000Z-abc123",
  "provider": "codex",
  "model_profile": "default",
  "model_id": "gpt-5.5",
  "status": "idle",
  "last_seq": 1234,
  "context": {
    "context_window": 400000,
    "last_request_tokens": 12000
  }
}
```

### Paginated Items

GUI 拉取历史时不一次加载全部内容。

```text
GET /sessions/{id}/items?before_seq=1234&limit=50&view=chat
GET /sessions/{id}/items?after_seq=1200&limit=50&view=debug
```

要求：

- `limit` 有默认值和最大值。
- `before_seq` 用于向上滚动读取更早内容。
- `after_seq` 用于读取某个 seq 之后的新内容。
- `view=chat` 默认只返回普通可见聊天 items。
- `view=debug` 可以返回 hidden/debug/internal items。
- 大内容只返回 preview 和 content ref，不返回完整 blob。

返回示例：

```json
{
  "items": [
    {
      "seq": 1201,
      "id": "item_1201",
      "turn_id": "turn_10",
      "created_at": "2026-07-03T12:20:00Z",
      "kind": "message",
      "visibility": "visible",
      "audience": "user",
      "message": {
        "role": "user",
        "content": {
          "inline": "继续上一步"
        }
      }
    }
  ],
  "has_more_before": true,
  "has_more_after": false
}
```

实际 Go 内部仍可使用 `model.Message`，HTTP 输出可以包装成适合 GUI 的 DTO，避免直接暴露内部
结构限制。

### Send Message

`POST /sessions/{id}/messages` 提交一条用户消息。

建议请求：

```json
{
  "content": "继续实现下一步"
}
```

语义：

- server 启动一个 turn。
- turn 运行中通过 WebSocket 推送 transient events。
- turn 成功后，server append persisted records，并通过 WebSocket 推送 committed events。
- turn 失败时，server 推送 failure event，不持久化失败 turn。

MVP 可以限制同一 session 同时只能运行一个 turn。

### Compact Command

`POST /sessions/{id}/commands/compact` 触发手动 compact。

语义：

- 不发起用户 turn。
- 成功后 append hidden summary item、compaction record、`active_history.replaced`。
- 失败时状态不变。
- 通过 WebSocket 推送 compact started / completed / failed events。

### Read Item Content

`GET /sessions/{id}/items/{item_id}/content` 用于按需读取完整内容或 blob 内容。

要求：

- 只能读取当前 session 中可达的 item content。
- 不允许通过裸 hash 读取 blob。
- 支持 `offset` / `max_bytes` 或类似分页读取参数。
- 默认 chat view 可以隐藏 hidden/debug/internal item content。
- debug view 需要 server 侧权限检查。

## WebSocket API

建议 endpoint：

```text
WS /sessions/{id}/stream
```

WebSocket 推送两类事件：

- transient events: 运行中状态，不一定持久化。
- persisted events: 已写入 session log 的事实。

### Transient Events

运行中事件示例：

```json
{"type":"turn.started","turn_id":"turn_12"}
{"type":"text.delta","turn_id":"turn_12","text":"正在"}
{"type":"tool.started","turn_id":"turn_12","name":"read_file","preview":"docs/session-compaction.md"}
{"type":"tool.finished","turn_id":"turn_12","name":"read_file","is_error":false}
{"type":"compact.started","reason":"user_requested"}
{"type":"turn.failed","turn_id":"turn_12","message":"context window exceeded"}
```

这些事件用于 UI 即时反馈。失败 turn 不持久化时，GUI 可以显示临时错误 toast 或临时 timeline
状态，但刷新后不会作为历史 item 出现。

### Persisted Events

成功写入 session log 后推送：

```json
{"type":"item.appended","seq":1300,"item_id":"item_1300"}
{"type":"active_history.replaced","seq":1301}
{"type":"compaction.created","seq":1302,"compaction_id":"compact_3"}
{"type":"turn.committed","turn_id":"turn_12","last_seq":1305}
```

GUI 收到 persisted events 后，可以：

- append 新 item 到当前列表。
- 或根据 `after_seq` 拉取最新 items。
- 更新 session metadata。

## Turn Lifecycle

用户发送消息：

```text
POST /sessions/{id}/messages
-> server validates session is idle
-> server starts turn
-> WS turn.started
-> model streaming events become transient WS events
-> tool status becomes transient WS events
-> turn succeeds
-> server appends session records
-> server flushes records
-> WS item.appended / turn.committed
```

失败：

```text
POST /sessions/{id}/messages
-> server starts turn
-> WS transient output
-> model/tool/compact failure
-> WS turn.failed
-> no persisted user/assistant/tool items
-> ActiveHistory unchanged
```

这延续会话压缩设计中的决策：只保存成功 turn。

## GUI Views

### Chat View

默认 view：

- 显示 `visibility=visible` 的 user messages。
- 显示 `visibility=visible` 的 assistant messages。
- 显示 tool status 或 tool summary。
- 默认不展开大 tool result。
- 默认不显示 compaction summary。
- 默认不显示 system/developer/runtime context。

压缩发生后：

- 旧 visible items 仍可分页查看。
- chat view 可以显示一个轻量状态，例如“上下文已压缩”，但不展示 summary 正文。
- 如果不想打扰用户，也可以只在 session debug/status 区域展示。

### Debug View

debug view 可显示：

- hidden/model-facing compaction summary。
- compaction checkpoints。
- active history refs。
- runtime context。
- system/developer items。
- blob refs。
- context token metadata。

debug view 仍必须通过 server 权限控制。

## Blob 内容展示

GUI timeline 只拿 preview。

用户展开大内容时：

```text
GET /sessions/{id}/items/{item_id}/content?max_bytes=65536
```

server 校验：

1. item 属于当前 session。
2. 当前 view/权限允许读取该 item。
3. blob hash 可由该 item 到达。
4. 返回受限大小的内容和继续读取 cursor。

不要提供：

```text
GET /blobs/{hash}
```

裸 hash 读取会绕过 session 权限和可达性。

## Command Semantics

GUI command 与 CLI slash command 对齐，但不必复用文本 slash 输入。

建议：

- CLI 普通单行 `/compact` 触发手动 compact。
- GUI 使用显式按钮或 command palette 调 `POST /sessions/{id}/commands/compact`。
- 多行文本中的 `/compact` 永远是普通文本。

未来可扩展：

- stop current turn。
- retry failed transient turn。
- export session。
- open debug view。

## Concurrency

MVP 建议：

- 同一 session 同时只允许一个 running turn。
- running turn 时再次发送 message 返回 conflict。
- compact command 只能在 session idle 时执行。
- 如果未来允许排队，队列必须由 server 持久化或明确标记为 transient。

## Error Handling

HTTP error 返回结构建议：

```json
{
  "error": {
    "code": "session_busy",
    "message": "session is currently running a turn"
  }
}
```

常见错误：

- `session_not_found`
- `session_busy`
- `session_corrupted`
- `context_limit`
- `compact_failed`
- `permission_denied`
- `blob_not_found`

错误响应不应包含 prompt、assistant output、tool result、blob content 或 secrets。

## Testing

### API

- `GET /sessions` 不返回完整 items。
- `GET /sessions/{id}/items` 支持 `before_seq`、`after_seq`、`limit` 和 `view`。
- `view=chat` 不返回 hidden compaction summary。
- `view=debug` 可返回 compaction metadata。
- `POST /sessions/{id}/messages` 在 session busy 时拒绝。
- `POST /sessions/{id}/commands/compact` 在 idle 时触发 compact。

### WebSocket

- turn running 时推送 transient text deltas。
- tool call 时推送 tool status，不泄露完整 tool result。
- turn 成功后推送 persisted item events。
- turn 失败后推送 `turn.failed`，且刷新后不出现失败 turn items。

### Blob Access

- item preview 不包含完整大内容。
- item content endpoint 可按大小读取 blob。
- 裸 hash 不能读取 blob。
- 无权限 view 不能读取 hidden/debug/internal item content。

### Integration

端到端场景：

```text
创建 session
-> GUI 发送 message
-> WS 收到 text deltas
-> turn committed
-> GUI 分页拉取 items
-> 触发 compact command
-> compact committed
-> chat view 仍能分页看到旧 visible items
-> debug view 能看到 compaction metadata
```

## 建议实现顺序

1. server 进程和 session metadata API。
2. session item pagination API。
3. WebSocket transient stream。
4. send message API。
5. persisted event notification。
6. compact command API。
7. item content/blob read API。
8. basic Web GUI chat view。
9. GUI debug view。
10. stop/retry/export 等后续命令。
