# Global Singleton Server Projects Checklist

这份清单用于 M21 实现阶段逐项执行。所有项目前保持未勾选，直到对应行为有代码、测试或 smoke
evidence 证明。

## Scope

- [ ] 确认 M21 是直接替换，不保留旧 scoped server behavior 兼容层。
- [ ] 确认本任务不恢复 hardcoded chat product entry，也不把它作为 hidden alias。
- [ ] 确认用户可见 docs/help/errors 使用 raw `argv[0]` basename，不硬编码具体命令名。

## Home Namespace and Command Name

- [ ] 实现 `--home PATH` 最高优先级。
- [ ] 从 raw basename 派生环境变量名。
- [ ] 去除 `.exe` 等平台后缀后再规范化 env var 名。
- [ ] 将非 `[A-Z0-9]` 字符规范化为 `_`。
- [ ] 合并连续 `_`，trim 首尾 `_`，追加 `_HOME`。
- [ ] 覆盖 `sai.exe -> SAI_HOME`。
- [ ] 覆盖 `simple-agent.exe -> SIMPLE_AGENT_HOME`。
- [ ] 覆盖 `my.tool -> MY_TOOL_HOME`。
- [ ] 规范化为空时回退内置默认 user-level directory。
- [ ] 验证不同 `--home` directories 是独立 singleton namespaces。

## Singleton Server and Registry

- [ ] registry 默认写入 user-level home namespace。
- [ ] durable data store 默认写入 user-level home namespace。
- [ ] registry 记录 `pid`、`base_url`、`token`、`version` 和 `started_at`。
- [ ] client 复用 registry 前 health-check。
- [ ] stale registry health-check 失败后可覆盖。
- [ ] file lock 防止 auto-start / background start 并发双启动。
- [ ] `<cmd> server` 前台启动 namespace singleton，不绑定 cwd/project。
- [ ] 已有健康 server 时 `<cmd> server` 提示 already running 并退出 0。
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
- [ ] `GET /health` 是 public loopback discovery endpoint。
- [ ] `GET /health` 只返回 minimal non-sensitive liveness。
- [ ] 除 `GET /health` 外，所有 HTTP 请求要求 bearer token。
- [ ] 所有 WebSocket 请求要求 bearer token。
- [ ] 第一版不实现多用户 login。

## Projects

- [ ] project identity 只使用 canonical cwd root。
- [ ] project 必须显式创建。
- [ ] project metadata 存在 user-level registry/data store，不写 project marker 文件。
- [ ] `project create` 默认使用 effective cwd。
- [ ] `project create --cwd PATH` 可指定 canonical root。
- [ ] duplicate exact canonical root 返回已有 project info 并退出 0。
- [ ] project name 只作为 display-only metadata。
- [ ] 从 effective cwd 向上查找 nearest registered ancestor。
- [ ] nested projects 允许并按 nearest ancestor 选择。
- [ ] 无 project 时 bare attach 失败并提示创建 project。
- [ ] 无 project 时 send/session create 失败并提示创建 project。
- [ ] `project list` 可列出 projects。
- [ ] `project show` 可按 current cwd discovery 或 explicit project id 展示 project。
- [ ] `project show` 不接受 `--cwd`。
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
- [ ] primary command shape 是 singular `session`。
- [ ] 如果保留旧 `sessions`，仅作为 list alias。
- [ ] `session create` 记录 project id。
- [ ] `session create` 记录 `created_cwd`。
- [ ] `session create` 记录 `config_path`。
- [ ] `session create` 记录 provider/model/tools/MCP/skills 等关键 metadata。
- [ ] 每个 turn 重新读取 session 的 `config_path`。
- [ ] `--config` 只允许创建新 session 时使用。
- [ ] attach existing session 时传 `--config` 报错。
- [ ] send existing session 时传 `--config` 报错。
- [ ] existing session cwd 固定为 `created_cwd`。
- [ ] `--cwd` 只允许 project create、session create 和 `--new`。
- [ ] attach existing session 时传 `--cwd` 报错。
- [ ] send existing session 时传 `--cwd` 报错。
- [ ] 第一版不提供 session config mutation command。
- [ ] `session list` 默认当前 project non-archived。
- [ ] `session list --project` 可列指定 project。
- [ ] `session list --all-projects` 可跨 project 列出。
- [ ] `session list --archived` 可列 archived sessions。
- [ ] `session list` 按 `last_used_at desc` 再 `created_at desc` 排序。
- [ ] `session show` 展示 metadata。
- [ ] `session rename` 更新 display name。
- [ ] `session archive` 隐藏 session。

## Attach and Send

- [ ] bare `<cmd>` 等价 attach。
- [ ] bare attach auto-start server。
- [ ] bare attach 从 cwd 向上找到 project。
- [ ] bare attach 无 project 时失败。
- [ ] bare attach 当前 project 无 session 时失败。
- [ ] bare attach 选择当前 project 最近 non-archived session。
- [ ] `<cmd> --new` 创建 session 后 attach。
- [ ] explicit session id 全局有效。
- [ ] 未指定 session id 时只从当前 project 选最近 non-archived session。
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
- [ ] 提供 `GET /projects`。
- [ ] 提供 `POST /projects`。
- [ ] 提供 project detail/remove endpoint。
- [ ] 提供 `GET /projects/{project_id}/sessions`。
- [ ] 提供 `POST /projects/{project_id}/sessions`。
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

- [ ] durable project/session store 与 server registry 分离。
- [ ] per-project directories 存在 user-level home namespace。
- [ ] per-session directories 存在 user-level home namespace。
- [ ] 默认不写 project repo。
- [ ] transcript 使用 append-only JSONL。
- [ ] 大内容存 hash-addressed blobs。
- [ ] global blob dedupe 可用或被明确实现。
- [ ] session seq 支持 pagination。
- [ ] hidden summary records 可被 GUI 过滤。
- [ ] debug records 可被 GUI 过滤。

## Validation

- [ ] 单元/集成测试覆盖 singleton per home namespace。
- [ ] 测试覆盖 different home dirs independent。
- [ ] 测试覆盖 explicit project create。
- [ ] 测试覆盖 explicit session create。
- [ ] 测试覆盖 nested discovery。
- [ ] 测试覆盖 config rejection for existing sessions。
- [ ] 测试覆盖 cwd rejection for existing sessions。
- [ ] 测试覆盖 direct replacement/no chat product entry。
- [ ] 测试覆盖 explicit project/session API paths。
- [ ] 测试覆盖 shutdown immediate/wait semantics。
- [ ] 测试覆盖 interrupted recovery。
- [ ] 测试覆盖 JSONL/blob pagination。
- [ ] `go test ./...` 通过。
- [ ] `git diff --check` 通过。
- [ ] 在本文件追加 smoke evidence，包含命令、日期、结果和任何已知限制。
