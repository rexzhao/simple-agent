# Execution Library / No HTTP Product Layer Checklist

这份清单用于跟踪 M22：删除当前产品路径里的 HTTP/WS client-server 层，并把 project/session
management 和 turn execution 收敛为进程内 execution library。M21 global-server 清单作为历史记录
保留，不再作为当前实现目标。

## Scope

- [ ] 当前产品路径删除 HTTP/WS server、HTTP client、registry、bearer token 和 background
  server lifecycle。
- [ ] CLI 直接调用 execution library。
- [ ] execution library 以记录存储位置（home/storage root）作为唯一必需初始化参数。
- [ ] execution library 拥有 project/session stores、nearest project discovery、session selection、
  turn lock、event persistence、compaction、runtime metadata 和 interrupted recovery。
- [ ] presentation layer 与 execution layer 严格分离：CLI 只解析输入并渲染 DTO/events。
- [ ] CLI 不直接操作 provider adapters、tool execution、project/session stores、blob store 或
  event projector。
- [ ] 不恢复 hardcoded chat product entry，也不保留 hidden chat alias。
- [ ] future HTTP adapter、GUI 和跨终端共享能力均为 out of scope；未来 HTTP 只能作为 thin adapter
  调用同一个 execution library。

## Implementation Slices

- [ ] 抽出 execution service/library facade，并保持现有 runtime/session 行为不变。
- [ ] 将 project create/list/show/remove 路径迁移为直接调用 execution library。
- [ ] 将 session create/list/show/remove 路径迁移为直接调用 execution library。
- [ ] 将 attach/send/new/compact 路径迁移为直接调用 execution library events。
- [ ] 删除 CLI 的 server auto-start、registry discovery、HTTP client 和 WebSocket client product path。
- [ ] 删除 `server` foreground/background/status/stop 产品命令。
- [ ] 删除当前 HTTP handlers、registry 和 startup lock 代码；未来 adapter 需要时另建薄适配层。
- [ ] 清理 docs/help/errors 中把 HTTP server 作为当前产品路径的描述。

## Acceptance Criteria

- [ ] CLI project/session commands 在没有 server、registry、HTTP client 或 WebSocket stream 的情况下工作。
- [ ] CLI attach/send/new/compact 只通过 execution library DTO/events 渲染输出。
- [ ] execution library 测试覆盖 storage root 初始化、project/session lifecycle、nearest project
  discovery、session selection、busy turn rejection、event streaming/persistence、compaction 和
  interrupted recovery。
- [ ] `rg` 检查确认当前产品入口没有 `server` command、registry discovery、HTTP client 或 WebSocket
  attach 依赖。
- [ ] `go test ./...` 通过。
- [ ] `git diff --check` 通过。

## Smoke Evidence

在每个实现切片完成后记录命令、测试范围和关键行为证据。
