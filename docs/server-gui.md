# Server 和 Web GUI 设计

> 历史说明：本文记录 M20 时期的 HTTP/WS server-owned session 与未来 Web GUI 设计背景。
> M22 已从当前产品路径删除 HTTP/WS server、registry、WebSocket stream 和 server 命令。
> 本文不再是当前 CLI/execution-library 行为规范；当前规范见 `docs/development.md`、
> `docs/configuration.md` 和 `docs/tasks/execution-library-no-http-checklist.md`。

本文记录未来 server + Web GUI 的独立功能设计。会话压缩和 session 存储的底层设计见
`docs/session-compaction.md`，本文只描述 server ownership、HTTP API、WebSocket 事件、
GUI 展示和交互语义。

M20 的实现目标只包含 server-owned sessions 和 CLI client。Web GUI UI 仍是本文的设计
背景和 API 约束，但不要求在 M20 中实现浏览器前端。

## 目标

最终使用形态不应依赖本地进程内 CLI chat loop。server 是会话 owner，CLI 和未来 Web GUI
都作为 server client 工作。M20 的产品形态应是：

```text
sai server --cwd <project> --config <file>
-> 默认前台运行，Ctrl+C 优雅退出
-> 提供 HTTP API
-> 提供 WebSocket stream
-> CLI 通过 attach REPL 或一次性命令使用
-> 未来 Web GUI 通过浏览器使用同一套 API
```

CLI client 和未来 GUI 都不直接读取 session 文件，也不一次性加载完整会话。server 是 session
owner，client 只通过 server 获取 filtered / paginated view。

## 非目标

MVP 不要求：

- 多用户账号系统。
- 复杂权限模型。
- 协同编辑。
- 离线 GUI 直接读本地 session 文件。
- GUI 直接修改 `ActiveHistory`。
- GUI 直接读 blob hash。
- M20 中实现浏览器 Web GUI UI。
- 跨机器远程 server 暴露。
- 多个 running turns 同时修改同一个 session。

## 架构原则

1. server 是唯一 session writer。
2. CLI 和未来 GUI 都是 server state 的视图，不直接操作 session 文件。
3. 历史记录通过分页 HTTP API 获取。
4. 实时输出通过 WebSocket 获取。
5. 正在运行的 turn 先通过 transient events 展示。
6. 只有成功 turn 才持久化为 session records。
7. GUI 默认展示 chat view，debug view 才展示 hidden/model/internal items。
8. `ActiveHistory` 是 server 内部模型上下文投影，GUI 不反推、不修改。
9. 同一 session 可以被多个 client 观察和控制，但同一时间最多只有一个 running turn。

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

CLI client / 未来 GUI 负责：

- 展示 session list。
- 展示一个 session 的 chat timeline。
- 发送用户输入。
- 展示 streaming 输出。
- 触发 command，例如 compact。
- 按需展开 tool result 或 blob 内容。
- 在 debug view 中展示 hidden/debug/internal 信息。

CLI client / 未来 GUI 不负责：

- 直接读取 session 文件。
- 直接读取 blob 文件。
- 直接拼接 provider messages。
- 修改 `ActiveHistory`。
- 决定哪些 items 进入模型上下文。

## Server Process 和 Discovery

server 默认作为本机 per-user 前台进程运行。CLI 可以显式启动 server，也可以按当前路径向上发现
已运行 server 并 attach；需要后台运行时显式传入 `--background`。

### 启动命令

建议命令：

```text
sai server --cwd F:\work\repo
sai server --cwd F:\work\repo --config .agents/sai.yaml
sai server --cwd F:\work\repo --port 8787
sai server --cwd F:\work\repo --listen 127.0.0.1:8787
sai server --cwd F:\work\repo --background
sai stop
sai stop --cwd F:\work\repo
```

规则：

- `--cwd` 是 server 的运行工作目录，也是 tools、项目指令、相对配置路径和相对 session 路径的
  项目基准目录。省略时使用启动命令当前目录。
- `--config` 如果是相对路径，基于 `--cwd` 解析。
- server identity 使用 canonical `cwd + config_path`。
- `sai server` 默认前台运行，命令阻塞到 server 退出。
- 前台 server 收到 Ctrl+C 时优雅关闭 listener、flush 必要 metadata、移除 registry，并退出 0。
- `sai server --background` 启动后台进程；父进程等待子进程完成 listen、写入 registry 且
  `/health` 可用后退出 0。
- `--background` 启动的子进程 stdout/stderr 不应长期占用调用它的终端；运行日志走 server 日志或
  诊断日志路径。
- 启动 server 时只按 canonical `cwd + config_path` 精确匹配已有 server；不会因为父目录已有
  server 就复用。
- 同一 `cwd + config_path` 已有健康 server 且监听参数一致时，提示 already running、addr、
  pid，并退出 0。
- 同一 `cwd + config_path` 已有健康 server 但请求端口或 listen 不一致时，返回冲突错误并退出
  非 0。
- 指定端口被其他进程占用时，启动失败并退出非 0。
- `sai stop` 默认从当前目录向上发现最近健康 server，向 server 发送 shutdown 请求并等待退出。
- `sai stop --cwd X` 从 canonical `X` 向上发现最近健康 server 并停止。
- stop 成功后移除对应 registry 记录；如果进程已不存在，清理 stale registry 并退出 0。
- stop 不删除 sessions、logs 或 blobs。

### Listen 地址

默认监听随机本地端口：

```text
127.0.0.1:0
```

OS 分配端口后，server 把最终地址写入 registry。随机端口用于避免冲突，不作为安全机制。

支持：

- `--port N`：监听 `127.0.0.1:N`。
- `--port 0`：随机端口，等价于默认。
- `--listen host:port`：高级用法。MVP 默认只支持或只推荐 loopback 地址。

默认不监听 `0.0.0.0`。如果未来允许非 loopback 地址，必须重新设计认证、权限提示和安全边界。

### Registry

server 启动成功后写入 per-user registry。建议记录：

```json
{
  "cwd": "F:\\work\\repo",
  "config_path": "F:\\work\\repo\\.agents\\sai.yaml",
  "addr": "127.0.0.1:49321",
  "pid": 12345,
  "token": "random-local-secret",
  "started_at": "2026-07-03T12:00:00Z",
  "version": "..."
}
```

要求：

- registry 文件权限尽量限制为当前用户可读写。
- 每个 server 生成随机 token。
- CLI / 未来 GUI 发起写操作、debug 读取和 blob content 读取时必须带 token。
- client 发现 registry 记录后先请求 `/health`。健康检查失败时，可以清理 stale 记录并继续查找。

### 向上查找

`sai`、`sai attach`、`sai status` 等 client 命令默认从目标 cwd 向上查找最近 server。

示例：当前目录为 `F:\work\repo\internal\cli` 时，查找顺序为：

```text
F:\work\repo\internal\cli
F:\work\repo\internal
F:\work\repo
F:\work
F:\
```

匹配规则：

- 每一级使用 canonical path 和 registry 中的 canonical `cwd` 比较。
- 找到最近的健康 server 即停止。
- 如果传入 `--cwd X`，则从 canonical `X` 开始向上查找。
- 找不到 server 时，提示用户先运行 `sai server --cwd <path> --config <file>`。

## CLI Client

移除或隐藏独立的进程内 `sai chat` 产品入口。CLI 默认作为 server client 工作。

建议命令：

```text
sai                         # 等价于 sai attach，向上发现 server
sai --cwd F:\work\repo       # 从指定 cwd 向上发现 server 并 attach
sai attach                   # 选择或继续一个 session
sai attach <session-id>      # 进入指定 session
sai attach --new             # 创建新 session 并进入
sai status                   # 查询最近 server 后退出
sai stop                     # 停止最近 server 后退出
sai servers list             # 列出 registry 中的本地 server 后退出
sai sessions list            # 通过最近 server 查询 sessions 后退出
sai sessions show <id>       # 通过最近 server 查询 session metadata 后退出
sai send <session-id> --prompt "继续"  # 发一轮，输出结果后退出
sai send --new --prompt "开始"         # 创建 session，发一轮，输出结果后退出
```

语义：

- 裸 `sai` 默认 attach，而不是启动本地进程内 chat。
- `attach` 可以指定 session id；未指定时可以列出 sessions 并提示选择，或按产品策略继续最近
  session。
- `status`、`servers list`、`sessions list/show` 是查询后直接退出的命令。
- `stop` 是控制命令，按 `--cwd` / 向上发现定位最近 server，优雅停止后退出。
- `send` 是非交互式 server client 命令，用于脚本或一次性 prompt。
- `sessions list/show` 不直接读 session 文件，必须通过 server API。
- attach REPL 中的 `/compact` 调用 server command API；多行文本里的 `/compact` 仍作为普通文本。

兼容性取舍：

- 切换到 server-owned session 后，不再保留独立 `sai chat` 路径，避免 MCP lifecycle、session
  writer、resume、compact 和日志出现两套实现。
- 如果需要迁移期，可以临时保留 hidden/legacy 入口，但文档和 help 不再推荐。

## HTTP API

建议 API 形态：

```text
GET  /health
GET  /server
POST /server/shutdown
GET  /sessions
POST /sessions
GET  /sessions/{id}
GET  /sessions/{id}/items?before_seq=<seq>&limit=50&view=chat
GET  /sessions/{id}/items?after_seq=<seq>&limit=50&view=debug
POST /sessions/{id}/messages
POST /sessions/{id}/commands/compact
GET  /sessions/{id}/items/{item_id}/content
```

### Health

`GET /health` 用于 registry stale 检测和 client 连接前探测。

建议字段：

```json
{
  "status": "ok",
  "version": "...",
  "pid": 12345
}
```

### Server Status

`GET /server` 返回当前 server 状态，用于 `sai status`。

建议字段：

```json
{
  "cwd": "F:\\work\\repo",
  "config_path": "F:\\work\\repo\\.agents\\sai.yaml",
  "addr": "127.0.0.1:49321",
  "pid": 12345,
  "version": "...",
  "started_at": "2026-07-03T12:00:00Z",
  "uptime_seconds": 1800,
  "session_count": 3,
  "running_turns": 1
}
```

### Server Shutdown

`POST /server/shutdown` 用于 `sai stop`。请求必须带 registry token。

语义：

- server 停止接受新的 turn。
- 正在运行的 turn MVP 可以拒绝 shutdown 并返回 conflict；后续可以支持 graceful drain 或 cancel。
- 没有 running turn 时，server 关闭 HTTP/WS listener，flush 必要 metadata，并退出进程。
- client 等待进程退出后删除 registry 记录。

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
  "reasoning_level": "high",
  "status": "idle",
  "last_seq": 1234,
  "context": {
    "context_window": 400000,
    "last_request_tokens": 12000
  }
}
```

`reasoning_level` 是创建会话时生效的统一 reasoning 档位（缺省回落到模型的
`reasoning_config.default`）；模型未配置 reasoning 或旧会话时该字段省略。

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

MVP 应限制同一 session 同时只能运行一个 turn。多个 client 可以同时观察同一个 session；
running turn 期间再次发送 message 返回 conflict。

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

多个 client 可以同时连接同一个 session stream。WebSocket 推送两类事件：

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

client 收到 persisted events 后，可以：

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

## Future Web GUI Views

本节定义未来浏览器 Web GUI 依赖的 API 视图语义。M20 不实现 Web GUI UI，但 server API
应保留这些过滤、分页和权限边界。

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

GUI command 与 CLI attach REPL slash command 对齐，但不必复用文本 slash 输入。

建议：

- CLI attach REPL 普通单行 `/compact` 触发手动 compact。
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
- 多个 client 可以同时连接同一个 session stream。
- running turn 时其他 client 可以看到 transient stream 和 persisted events。
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
- `server_busy`
- `session_corrupted`
- `context_limit`
- `compact_failed`
- `permission_denied`
- `blob_not_found`

错误响应不应包含 prompt、assistant output、tool result、blob content 或 secrets。

## Testing

### API

- `GET /health` 可用于 registry stale 检测。
- `GET /server` 返回 cwd、config、listen、pid、uptime、session 数和 running turn 数。
- `POST /server/shutdown` 在无 running turn 时触发 server 优雅退出。
- `POST /server/shutdown` 在 running turn 时返回明确错误或 conflict。
- `GET /sessions` 不返回完整 items。
- `GET /sessions/{id}/items` 支持 `before_seq`、`after_seq`、`limit` 和 `view`。
- `view=chat` 不返回 hidden compaction summary。
- `view=debug` 可返回 compaction metadata。
- `POST /sessions/{id}/messages` 在 session busy 时返回 conflict。
- `POST /sessions/{id}/commands/compact` 只在 idle session 成功。

### WebSocket

- 多个 client 同时连接同一 session stream 时都能收到 running turn events。
- turn running 时推送 transient text deltas。
- tool call 时推送 tool status，不泄露完整 tool result。
- turn 成功后推送 persisted item events。
- turn 失败后推送 `turn.failed`，且刷新后不出现失败 turn items。

### CLI / Registry

- `sai server --cwd X` 启动后写入 registry，记录 canonical cwd、config、addr、pid 和 token。
- 前台 `sai server --cwd X` 收到 Ctrl+C 后优雅退出并移除 registry。
- `sai server --cwd X --background` 后父进程返回且后台 server 健康可连接。
- 重复 `sai server --cwd X` 命中同一健康 server 时提示 already running 并退出 0。
- 重复启动同一 server 但指定不同端口时返回冲突错误。
- `sai` 从当前目录向上发现最近 server 并 attach。
- `sai --cwd X` 从 X 向上发现最近 server。
- `sai attach <session-id>` 进入指定 session，`sai attach --new` 创建新 session。
- `sai status` 查询 server 状态后退出。
- `sai stop` 停止最近 server，移除 registry，不删除 session/log/blob 数据。
- `sai servers list` 列出 registry server。
- `sai sessions list/show` 通过 server API 查询，不直接读取 session 文件。
- `sai send <session-id> --prompt ...` 和 `sai send --new --prompt ...` 发一轮后退出。
- 没有可用 server 时，CLI 给出启动提示。
- registry stale 记录会在 health check 失败后被忽略或清理。

### Blob Access

- item preview 不包含完整大内容。
- item content endpoint 可按大小读取 blob。
- 裸 hash 不能读取 blob。
- 无权限 view 不能读取 hidden/debug/internal item content。

### Server / CLI Integration

端到端场景：

```text
创建 session
-> CLI attach 或 send 发送 message
-> WS/CLI 收到 text deltas
-> turn committed
-> CLI 通过 server API 分页拉取 items
-> 触发 compact command
-> compact committed
-> chat view 仍能分页看到旧 visible items
-> debug view API 能看到 compaction metadata
```

### Future Web GUI

- 浏览器 Web GUI UI 不属于 M20 验收。
- 后续 Web GUI 应复用同一套 HTTP API / WebSocket stream，不直接读取 session、blob 或
  `ActiveHistory`。

## 建议实现顺序

1. server 前台进程、`--background`、health/status/shutdown API 和 session metadata API。
2. server registry、随机/指定端口、`--cwd`、向上发现和 stop。
3. CLI attach/status/stop/server list/session list 基础 client。
4. session item pagination API。
5. WebSocket transient stream 和多 client fanout。
6. send message API 和 CLI `send`。
7. persisted event notification。
8. compact command API 和 attach REPL `/compact`。
9. item content/blob read API。
10. Future Web GUI: basic chat/debug view。
