# Global Singleton Server Projects Checklist

这份清单用于 M21 实现阶段逐项执行。所有项目前保持未勾选，直到对应行为有代码、测试或 smoke
evidence 证明。

## Scope

- [ ] 确认 M21 是直接替换，不保留旧 scoped server behavior 兼容层。
- [x] 确认本任务不恢复 hardcoded chat product entry，也不把它作为 hidden alias。
- [ ] 确认用户可见 docs/help/errors 使用 raw `argv[0]` basename，不硬编码具体命令名。

## Home Namespace and Command Name

- [x] 实现 `--home PATH` 最高优先级。
- [x] 从 raw basename 派生环境变量名。
- [x] 去除 `.exe` 等平台后缀后再规范化 env var 名。
- [x] 将非 `[A-Z0-9]` 字符规范化为 `_`。
- [x] 合并连续 `_`，trim 首尾 `_`，追加 `_HOME`。
- [x] 覆盖 `sai.exe -> SAI_HOME`。
- [x] 覆盖 `simple-agent.exe -> SIMPLE_AGENT_HOME`。
- [x] 覆盖 `my.tool -> MY_TOOL_HOME`。
- [x] 规范化为空时回退内置默认 user-level directory。
- [x] 验证不同 `--home` directories 是独立 singleton namespaces。

## Singleton Server and Registry

- [x] registry 默认写入 user-level home namespace。
- [ ] durable data store 默认写入 user-level home namespace。
- [ ] registry 记录 `pid`、`base_url`、`token`、`version` 和 `started_at`。
- [x] client 复用 registry 前 health-check。
- [x] stale registry health-check 失败后可覆盖。
- [ ] file lock 防止 auto-start / background start 并发双启动。
- [ ] `<cmd> server` 前台启动 namespace singleton，不绑定 cwd/project。
- [x] 已有健康 server 时 `<cmd> server` 提示 already running 并退出 0。
- [ ] `<cmd> server --background` 显式后台启动。
- [ ] `<cmd> server status` 不 auto-start。
- [ ] `<cmd> server stop` 不 auto-start。
- [ ] help/version/server foreground/server background/server status/server stop 都不 auto-start。
- [ ] bare attach/send/project/session commands 需要时 auto-start。
- [ ] 移除旧 scoped multi-server list 行为。

## Auth

- [ ] 默认 listen 只使用 loopback。
- [ ] token 存储在 home namespace 下。
- [ ] token 文件权限尽量限制为当前 OS user 可读写。
- [x] `GET /health` 是 public loopback discovery endpoint。
- [x] `GET /health` 只返回 minimal non-sensitive liveness。
- [x] 除 `GET /health` 外，所有 HTTP 请求要求 bearer token。
- [x] 所有 WebSocket 请求要求 bearer token。
- [ ] 第一版不实现多用户 login。
- [x] 新增 project endpoints 要求 registry bearer token。

## Projects

- [x] project identity 只使用 canonical cwd root。
- [x] project 必须显式创建。
- [x] project metadata 存在 user-level registry/data store，不写 project marker 文件。
- [x] `project create` 默认使用 effective cwd。
- [x] `project create --cwd PATH` 可指定 canonical root。
- [x] duplicate exact canonical root 返回已有 project info 并退出 0。
- [x] project name 只作为 display-only metadata。
- [x] 从 effective cwd 向上查找 nearest registered ancestor。
- [x] nested projects 允许并按 nearest ancestor 选择。
- [x] 无 project 时 bare attach 失败并提示创建 project。
- [x] 无 project 时 send/session create 失败并提示创建 project。
- [x] `project list` 可列出 projects。
- [x] `project show` 可按 current cwd discovery 或 explicit project id 展示 project。
- [x] `project show` 不接受 `--cwd`。
- [ ] `project remove` 默认 archive/hide。
- [ ] `project remove` 可按 current cwd discovery 或 explicit project id 选择 project。
- [ ] `project remove` 不接受 `--cwd`。
- [ ] `project remove --delete-data` 才执行真实数据删除。
- [ ] running sessions 阻止 remove。
- [ ] running sessions 阻止 delete-data。

## Sessions

- [ ] session 必须显式创建。
- [ ] 不隐式创建 project。
- [ ] 不隐式创建 session。
- [ ] `--new` 等价于 session create 后 attach。
- [x] primary command shape 是 singular `session`。
- [ ] 如果保留旧 `sessions`，仅作为 list alias。
- [x] `session create` 记录 project id。
- [x] `session create` 记录 `created_cwd`。
- [x] API-level project session create records `config_path` when provided.
- [x] API-level project session create records provider/model/tools/MCP/skills metadata when provided.
- [ ] 每个 turn 重新读取 session 的 `config_path`。
- [ ] `--config` 只允许创建新 session 时使用。
- [x] attach existing session 时传 `--config` 报错。
- [x] send existing session 时传 `--config` 报错。
- [ ] existing session cwd 固定为 `created_cwd`。
- [ ] `--cwd` 只允许 project create、session create 和 `--new`。
- [x] attach existing session 时传 `--cwd` 报错。
- [x] send existing session 时传 `--cwd` 报错。
- [ ] 第一版不提供 session config mutation command。
- [x] `session list` 默认当前 project non-archived。
- [x] `session list --project` 可列指定 project。
- [x] `session list --all-projects` 可跨 project 列出。
- [ ] `session list --archived` 可列 archived sessions。
- [ ] `session list` 按 `last_used_at desc` 再 `created_at desc` 排序。
- [x] `session show` 展示 metadata。
- [ ] `session rename` 更新 display name。
- [ ] `session archive` 隐藏 session。

## Attach and Send

- [x] bare `<cmd>` 等价 attach。
- [x] bare attach auto-start server。
- [x] bare attach 从 cwd 向上找到 project。
- [x] bare attach 无 project 时失败。
- [x] bare attach 当前 project 无 session 时失败。
- [x] bare attach 选择当前 project 最近 non-archived session。
- [ ] `<cmd> --new` 创建 session 后 attach。
- [x] explicit session id 全局有效。
- [x] 未指定 session id 时只从当前 project 选最近 non-archived session。
- [ ] 多个 observers 可以 attach 同一 session。
- [ ] 同一 session 同时只有一个 active turn。
- [ ] session busy 时 send 返回 `session_busy`。
- [ ] session busy 时 send 不选择另一个 session。
- [ ] 不同 sessions 可以并发运行。

## Shutdown and Recovery

- [ ] default shutdown immediate stop/cancel/cleanup。
- [ ] immediate shutdown 停止接受新 turns。
- [ ] immediate shutdown 取消 running turns。
- [ ] immediate shutdown 清理 registry。
- [ ] `--wait` drain 已经开始的 calls/turns。
- [ ] `--wait` 停止接受 new turns。
- [ ] `--wait` 支持 timeout。
- [ ] `--wait` timeout 后执行 immediate stop。
- [ ] OS signals / Ctrl+C 使用 immediate stop 语义。
- [ ] restart 后 previously running turns/sessions 标记 interrupted。
- [ ] restart 后不自动 replay running turns。

## API

- [ ] 提供 `GET /health`。
- [ ] 提供 `GET /server`。
- [ ] 提供 `POST /server/shutdown`。
- [x] 提供 `GET /projects`。
- [x] 提供 `POST /projects`。
- [x] 提供 project detail endpoint：`GET /projects/{project_id}`。
- [ ] 提供 project remove endpoint。
- [x] 提供 `GET /projects/{project_id}/sessions`。
- [x] 提供 `POST /projects/{project_id}/sessions`。
- [ ] 提供 `GET /sessions/{session_id}`。
- [ ] 提供 session rename/archive endpoint。
- [ ] 提供 `POST /sessions/{session_id}/messages`。
- [ ] 提供 `GET /sessions/{session_id}/items`。
- [ ] 提供 `GET /sessions/{session_id}/content/{blob_hash}`。
- [ ] 提供 `WS /sessions/{session_id}/stream`。
- [ ] CLI 只负责 cwd 到 project 的映射。
- [ ] future GUI 可以直接分页 projects/sessions。
- [ ] blob content endpoint 校验 blob 可由该 session 到达。
- [ ] 不提供裸全局 blob hash 读取。
- [ ] items API 支持 session seq pagination。
- [ ] GUI 可过滤 hidden summary/debug records。

## Persistence

- [x] durable project store 与 server registry 分离。
- [ ] durable session store 与 server registry 分离。
- [x] per-project directories 存在 user-level home namespace。
- [ ] per-session directories 存在 user-level home namespace。
- [x] 默认不写 project repo。
- [ ] transcript 使用 append-only JSONL。
- [ ] 大内容存 hash-addressed blobs。
- [ ] global blob dedupe 可用或被明确实现。
- [ ] session seq 支持 pagination。
- [ ] hidden summary records 可被 GUI 过滤。
- [ ] debug records 可被 GUI 过滤。

## Validation

- [x] 单元/集成测试覆盖 singleton per home namespace。
- [x] 测试覆盖 different home dirs independent。
- [x] 测试覆盖 explicit project create。
- [x] 测试覆盖 explicit session create。
- [x] 测试覆盖 nested discovery。
- [x] 测试覆盖 config rejection for existing sessions。
- [x] 测试覆盖 cwd rejection for existing sessions。
- [x] 测试覆盖 direct replacement/no chat product entry。
- [x] 测试覆盖 explicit project API paths。
- [x] 测试覆盖 explicit session API paths。
- [ ] 测试覆盖 shutdown immediate/wait semantics。
- [ ] 测试覆盖 interrupted recovery。
- [ ] 测试覆盖 JSONL/blob pagination。
- [x] `go test ./...` 通过。
- [x] `git diff --check` 通过。
- [x] 在本文件追加 smoke evidence，包含命令、日期、结果和任何已知限制。

## Smoke Evidence

- 2026-07-04 M21 attach --new slice: `go test ./internal/cli` 通过；覆盖
  `attach --new --cwd` 先 `GET /projects` 做 nearest project discovery，再带 session metadata
  `POST /projects/{project_id}/sessions`，随后连接 `WS /sessions/{id}/stream`，prompt 继续用 bearer
  token `POST /sessions/{id}/messages`，并确认 legacy global `POST /sessions` 未被使用。
- 2026-07-04 M21 attach --new slice: `go test ./internal/cli` 通过；覆盖
  `attach --new` 无 nearest registered project 时在 create/attach 前失败，并提示
  `run "sai project create"`。
- 2026-07-04 M21 bare attach project-scoped selection slice: `go test ./internal/cli` 通过；覆盖
  `attach` 无 session id 和 bare `<cmd>` 默认 attach 都先 `GET /projects` 做 nearest ancestor
  discovery，再 `GET /projects/{project_id}/sessions`，随后只连接当前 project 最近 session 的
  `WS /sessions/{id}/stream`，并确认未使用 legacy global `GET /sessions`。
- 2026-07-04 M21 bare attach project-scoped selection slice: `go test ./internal/cli` 通过；覆盖
  无 nearest registered project 时失败并提示 `run "sai project create"`，且不会访问 global
  `/sessions` list 或 stream。
- 2026-07-04 M21 bare attach project-scoped selection slice: `go test ./internal/cli` 通过；覆盖
  nearest project 存在但无 sessions 时提示 `session create` / `attach --new`，且不会 fallback 到
  其他 project 或 global session。
- 2026-07-04 M21 bare attach project-scoped selection slice: `go test ./...` 和 `git diff --check`
  通过；`git diff --check` 仅报告工作区 LF/CRLF normalization warnings。
- 2026-07-04 M21 send --new slice: `go test ./internal/cli` 通过；覆盖
  `send --new --cwd` 先 `GET /projects` 做 nearest project discovery，再带 created cwd/config/provider/model/
  tools/MCP/skills metadata `POST /projects/{project_id}/sessions`，随后用 bearer token
  `POST /sessions/{id}/messages` 发送 prompt，并确认 legacy global `POST /sessions` 未被使用。
- 2026-07-04 M21 send --new slice: `go test ./internal/cli` 通过；覆盖
  `send --new` 无 nearest registered project 时在 create/send 前失败，并提示
  `run "sai project create"`。
- 2026-07-04 M21 send no-id project selection slice: `go test ./internal/cli` 通过；覆盖
  `send --prompt` 无显式 session id 时先 `GET /projects` 做 nearest ancestor discovery，再
  `GET /projects/{project_id}/sessions`，随后只对该 project 最新 session 调用
  `POST /sessions/{id}/messages`，并确认不使用 global `/sessions` list 或其他 project sessions。
- 2026-07-04 M21 send no-id project selection slice: `go test ./internal/cli` 通过；覆盖
  无 nearest registered project 时提示 `run "sai project create"`，且不会访问 global `/sessions`
  list 或 send。
- 2026-07-04 M21 send no-id project selection slice: `go test ./internal/cli` 通过；覆盖
  nearest project 存在但无 sessions 时提示 `session create` / `send --new`，且不会 fallback 到
  其他 project 或 global session。
- 2026-07-04 M21 send no-id project selection slice: `go test ./internal/cli` 通过；覆盖
  `send [session-id] --prompt` help usage、explicit `send <session-id>` 继续直接使用全局
  `POST /sessions/{id}/messages` 且不要求 project list，以及 no-id send 仍按 existing-session
  规则拒绝 `--cwd` / `--config`。
- 2026-07-04 M21 slice 1: `go test ./internal/server` 通过；覆盖 home env var derivation、singleton registry
  upsert/discovery/stale cleanup。
- 2026-07-04 M21 slice 1: `go test ./internal/cli` 通过；覆盖 `--home`/env priority、different home
  registry independence、background child `--home` preservation、same-home singleton already-running behavior。
- 2026-07-04 M21 slice 1 reviewer fix: `go test ./internal/cli` 通过；覆盖 existing singleton 在 cwd
  config missing 和 requested listen 不同时仍返回 `SERVER_ALREADY_RUNNING` 并退出 0。
- 2026-07-04 M21 slice 1: `git diff --check` 通过。
- 2026-07-04 M21 slice 2: `go test ./internal/projects` 通过；覆盖 canonical project root
  persistence、duplicate root 返回 existing metadata、non-archived list、nested nearest ancestor discovery。
- 2026-07-04 M21 slice 2: `go test ./internal/server` 通过；覆盖 authenticated `POST /projects`、
  duplicate 200/new 201、`GET /projects`、`GET /projects/{project_id}` 和 invalid root handling。
- 2026-07-04 M21 slice 2: `go test ./internal/cli` 通过；覆盖实际 `sai server` 启动时 project store
  写入 `<home>/data/projects`，且不写 project repo marker。
- 2026-07-04 M21 slice 2: `git diff --check` 通过。
- 2026-07-04 M21 slice 3: `go test ./internal/cli` 通过；覆盖 `chat`、`chat -h`、
  `chat --help`、`chat --bad`、`chat --quit --prompt hi` 均返回 unknown command，`help chat`
  返回 unknown help topic，且不输出 `usage: sai chat`。
- 2026-07-04 M21 slice 3: `git diff --check` 通过。
- 2026-07-04 M21 slice 4: `go test ./internal/cli` 通过；覆盖 `project create` effective cwd、
  `project create --cwd` duplicate 200 existing metadata、`project list`、`project show --project`、
  `project show` nearest registered ancestor matching、`project show --cwd` rejection、project help
  不列 unimplemented remove，以及 project command scoped auto-start without startup output。
- 2026-07-04 M21 slice 4: `git diff --check` 通过。
- 2026-07-04 M21 slice 5: `go test ./internal/sessions ./internal/server` 通过；覆盖 V2
  `project_id` / `created_cwd` metadata persistence，以及 authenticated
  `GET /projects/{project_id}/sessions` / `POST /projects/{project_id}/sessions` create/list/filter、
  missing/invalid/archived project errors 和 project-scoped client helpers。
- 2026-07-04 M21 slice 5: `go test ./...` 通过。
- 2026-07-04 M21 slice 5: `git diff --check` 通过。
- 2026-07-04 M21 slice 6: `go test ./internal/server` 通过；覆盖
  `POST /projects/{project_id}/sessions` 空 body 兼容、显式 session creation metadata
  持久化/返回，以及 request body 中冲突 `project_id` 被忽略、URL project id 生效。
- 2026-07-04 M21 slice 6: `go test ./...` 通过。
- 2026-07-04 M21 slice 6: `git diff --check` 通过。
- 2026-07-04 M21 slice 7: `go test ./internal/cli` 通过；覆盖 singular `session` help、
  `session create` project-scoped metadata/no nearest project error、`session list` default nearest
  project / `--project` / `--all-projects` behavior、`session show` global id、`--cwd` rejection，以及
  singular session auto-start 不输出 `SERVER_ADDR`。
- 2026-07-04 M21 slice 7: `go test ./...` 通过。
- 2026-07-04 M21 slice 7: `git diff --check` 通过。
- 2026-07-04 M21 slice 8: `go test ./internal/cli` 通过；覆盖 existing-session
  attach/send 拒绝 `--cwd` 和 root `--config` 且在 cwd/server discovery 之前失败，同时确认
  `attach --new --cwd` 和 `send --new --cwd` 仍到达 discovery/create/send 路径。
- 2026-07-04 M21 slice 8: `go test ./...` 通过。
- 2026-07-04 M21 slice 8: `git diff --check` 通过（仅出现工作区 LF/CRLF normalization warnings）。
- 2026-07-04 M21 auth hardening slice: `go test ./internal/server` 通过；覆盖 `GET /health`
  无 token 成功且只返回 minimal liveness，`GET /server` 和 session read/content endpoints
  无 token 返回 `permission_denied`，带 registry bearer token 成功，`WS /sessions/{id}/stream`
  无 token / 错 token握手返回 403 且有效 token 可连接。
- 2026-07-04 M21 auth hardening slice: `go test ./internal/cli` 通过；覆盖 session list/show
  和 attach stream client helper/call site 发送 registry bearer token，auto-start 后使用新 registry
  token。
- 2026-07-04 M21 auth hardening slice: `go test ./...` 和 `git diff --check` 通过；`git diff --check`
  仅输出工作区 LF/CRLF normalization warnings。
- Known limits: 当前 M21 slices 未实现 project remove/archive、GUI server work、session data store home
  migration 或 registry `base_url` schema rename。
