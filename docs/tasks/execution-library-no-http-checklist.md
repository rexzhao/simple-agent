# Execution Library / No HTTP Product Layer Checklist

这份清单用于跟踪 M22：删除当前产品路径里的 HTTP/WS client-server 层，并把 project/session
management 和 turn execution 收敛为进程内 execution library。M21 global-server 清单作为历史记录
保留，不再作为当前实现目标。

## Scope

- [x] 当前产品路径删除 HTTP/WS server、HTTP client、registry、bearer token 和 background
  server lifecycle。
- [x] CLI 直接调用 execution library。
- [x] execution library 以记录存储位置（home/storage root）作为唯一必需初始化参数。
- [x] execution library 拥有 project/session stores、nearest project discovery、session selection、
  turn lock、event persistence、compaction、runtime metadata 和 interrupted recovery。
- [x] 当前产品路径 presentation layer 与 execution layer 严格分离：CLI 只解析输入并渲染 DTO/events。
- [x] 当前产品路径 CLI 不直接操作 provider adapters、tool execution、project/session stores、blob store 或
  event projector。
- [x] 不恢复 hardcoded chat product entry，也不保留 hidden chat alias。
- [x] future HTTP adapter、GUI 和跨终端共享能力均为 out of scope；未来 HTTP 只能作为 thin adapter
  调用同一个 execution library。

## Implementation Slices

- [x] 抽出 execution service/library facade，并保持现有 runtime/session 行为不变。
- [x] 将 project create/list/show/rename/archive/remove 路径迁移为直接调用 execution library。
- [x] 将 session create/list/show/rename/archive/remove 路径迁移为直接调用 execution library。
- [x] 将 send --new/existing/latest session turn 路径迁移为直接调用 execution library events。
- [x] 将 attach --new/existing 交互事件路径迁移为直接调用 execution library events。
- [x] 将 manual compact 路径迁移为直接调用 execution library events。
- [x] 删除 CLI 的 server auto-start、registry discovery、HTTP client 和 WebSocket client product path。
- [x] 删除 `server` foreground/background/status/stop 产品命令。
- [x] 删除当前 HTTP handlers、registry 和 startup lock 代码；未来 adapter 需要时另建薄适配层。
- [x] 裸 `sai` 在当前 cwd 无已注册 project 时自动创建 project，然后进入 pending 新 session。
- [x] 裸 `sai` 在当前 cwd 有已注册 ancestor project 时复用 nearest project 并进入 pending 新 session。
- [x] 新增 `session resume <id>` 作为已有 session 的交互恢复入口。
- [x] 从当前产品 help/docs 中移除 top-level `attach` 入口；实现可保留 hidden compatibility。
- [x] 删除或隐藏 `send` 产品命令和 help 入口。
- [x] 清理 docs/help/errors 中把 HTTP server 作为当前产品路径的描述。

## Acceptance Criteria

- [x] CLI project/session commands 在没有 server、registry、HTTP client 或 WebSocket stream 的情况下工作。
- [x] CLI attach/send/new/compact 只通过 execution library DTO/events 渲染输出。
- [x] CLI 默认入口自动发现或创建 project，并创建新 session 进入交互。
- [x] CLI 测试覆盖 `session resume <id>`、保留 project commands、隐藏/删除 send 产品入口。
- [x] execution library 测试覆盖 storage root 初始化、project/session lifecycle、nearest project
  discovery、session selection、busy turn rejection、event streaming/persistence、compaction 和
  interrupted recovery。
- [x] `rg` 检查确认当前产品入口没有 `server` command、registry discovery、HTTP client 或 WebSocket
  attach 依赖。
- [x] `go test ./...` 通过。
- [x] `git diff --check` 通过。

## Smoke Evidence

在每个实现切片完成后记录命令、测试范围和关键行为证据。

- 2026-07-07 project command execution-library slice:
  `go test ./internal/execution`、`go test ./internal/cli -run "TestProject|TestCustomProgramBasenameInProjectGuidanceError"`、
  `go test ./internal/cli`、`go test ./internal/execution ./internal/projects ./internal/sessions`、
  `go test ./...` 和 `git diff --check` 通过。覆盖 project create/list/show/rename/archive/remove
  直接使用 home-backed execution service，不再通过 CLI HTTP client、registry discovery 或
  background server startup；execution service 覆盖 nearest active/archived project selection、
  archived project rename rejection、remove 前必须 archive，以及 remove archived project 时删除同
  project sessions。
- 2026-07-07 session metadata command execution-library slice:
  `go test ./internal/execution`、`go test ./internal/cli -run "TestSession(Create|List|Show|Rename|Archive|Remove)|TestCustomProgramBasenameInProjectGuidanceError"`、
  `go test ./internal/cli`、`go test ./...` 和 `git diff --check` 通过。覆盖 session
  create/list/show/rename/archive/remove 直接使用 home-backed execution service，不再通过 CLI HTTP
  client、registry discovery 或 background server startup；execution service 覆盖 session
  lifecycle、project scoped/all-project listing、archived filtering、missing/archived project
  rejection，以及 stale running turn metadata 不再被当作 live active turn。
- 2026-07-07 send command execution-library slice:
  `go test ./internal/execution`、`go test ./internal/cli -run "TestSend|TestCustomProgramBasenameInProjectGuidanceError"`、
  `go test ./internal/cli`、`go test ./internal/server`、`go test ./...` 和 `git diff --check`
  通过。覆盖 send existing/latest/--new 直接使用 home-backed execution service 的 session
  turn events，不再通过 CLI HTTP client、registry discovery 或 background server startup；
  execution service 覆盖 bounded write lock busy rejection、auto compaction before turn、
  incremental event persistence、committed LastSeq 与存储一致，以及 runner/provider failure
  不泄露 prompt、assistant/tool/provider secret。
- 2026-07-07 HTTP product layer removal slice:
  `go test ./internal/cli -run TestChatSaveSessionProcessKillKeepsCompletedToolResult -count=1`、
  `go test ./internal/cli`、`go test ./internal/execution`、`go list ./...`、`go test ./...`、
  `git diff --check` 和
  `rg -n "internal/server|github.com/gorilla/websocket|localserver|WebSocket|HTTP client|HTTP handlers|registry discovery|server command|server status|server stop|auto-start|server-owned session|nearest healthy local server|local HTTP server|WS /sessions" internal\cli\cli.go internal\execution go.mod go.sum`
  通过。覆盖 attach --new/existing、pending attach、send、manual compact 直接调用
  execution service DTO/events；删除 `internal/server`、gorilla/websocket 依赖、server
  前台/后台/status/stop 产品命令、registry/HTTP client/WebSocket product path 和 HTTP
  handlers。当前仍有一个后续分层项未收敛：agent runtime/provider/tool runner 仍作为
  execution runner adapter 留在 CLI 包中，后续若要更严格的库边界，应迁入 execution 层。
- 2026-07-07 session-first CLI surface slice:
  `go test ./internal/cli -run "Test(RootHelpWritesUsageWithoutConfig|SessionHelpWritesUsageWithoutConfig|SendCommandIsUnsupported|AttachCommandIsUnsupported|BareNewFlagIsUnsupported|BareDefaultAttach|SessionResume|PendingAttachNoIDUsesConfigAndCWDOverridesOnFirstPrompt)"`、
  `go test ./internal/cli`、`go test ./...` 和 `git diff --check` 通过。覆盖裸
  `sai` 自动创建缺失 project、复用 nearest registered project 进入 pending 新 session、
  `sai session resume <id>` 恢复已有 session 的交互 REPL、top-level `attach`/`send`
  产品命令与 help topic 被移除，以及 `sai --new` 不再作为兼容入口。
- 2026-07-08 docs/help/errors cleanup slice:
  `rg -n "sai sessions|sai chat|--show-reasoning|--prompt|--stdin|--file|--verbose|without starting a chat|chat 启动|sai send|sai attach|server status|server stop|local HTTP server|nearest healthy local server" README.md docs\development.md docs\configuration.md docs\tasks\execution-library-no-http-checklist.md`
  只剩明确标注为 M22 后删除当前入口、M10-M19 历史背景或旧 smoke evidence 的命中；
  `git diff --check` 通过。覆盖 README 当前用法、配置/开发日志说明、reasoning 展示说明和
  本清单状态，不修改旧 M20/M21 历史记录。
- 2026-07-08 execution runner boundary slice:
  `go test ./internal/execution`、`go test ./internal/cli`、`go test ./...` 和
  `git diff --check` 通过。覆盖 CLI attach/session turn product path 通过
  execution-owned agent runner 组装 provider、tools、MCP、skills、compaction 和 event persistence；
  CLI 删除旧 execution runner adapter，仅保留普通 chat/子 agent harness 的历史 runtime 代码。
