# Global Singleton Server, Projects, and Sessions

本文定义 M21 的直接替换设计：server 不再按目录或配置文件 scoped 启动多个实例，而是在同一
OS user 的同一 home namespace 下保持一个全局 singleton server。server 可以管理多个项目目录，
project 和 session 都必须显式创建。本文是独立功能文档，旧 scoped server 行为不提供兼容层。

## 目标

- 每个 OS user、命令 basename 和 home namespace 只有一个健康 server。
- 同一 server 管理多个项目目录和这些项目下的 sessions。
- client 命令需要时自动启动 server；也保留手动 foreground start 和显式 background start。
- project 和 session 生命周期显式化，不隐式创建 project 或 session。
- CLI、HTTP API、持久化数据都以 explicit project/session identity 为核心。
- 不再维护旧的 scoped server discovery、multi-server list 或 hardcoded chat product entry。

## 非目标

- 不兼容旧的 `cwd + config_path` server identity。
- 不提供旧 scoped server registry 的迁移或兼容读取。
- 不实现多用户登录、远程暴露或非 loopback 默认监听。
- 不实现 session-history query tool；它仍是低优先级后续项。
- 不写入项目 repo marker 文件。

## Command Name and Home Namespace

所有用户可见 docs、help 和错误信息都必须使用当前进程 `argv[0]` 的 raw basename，不应硬编码具体
命令名。文档示例统一写成：

```text
<cmd>
<cmd> --new
<cmd> server --background
<cmd> project create
```

home namespace 选择优先级：

1. `--home PATH`
2. raw basename 派生的环境变量
3. 内置默认 user-level directory

环境变量名从 raw basename 派生：

1. 去掉平台后缀，例如 `.exe`。
2. 转成 uppercase。
3. 把非 `[A-Z0-9]` 字符规范化为 `_`。
4. 合并连续 `_`。
5. 去掉首尾 `_`。
6. 追加 `_HOME`。
7. 如果规范化结果为空，回退到内置默认 home directory，不使用空环境变量名。

示例：

```text
sai.exe -> SAI_HOME
simple-agent.exe -> SIMPLE_AGENT_HOME
my.tool -> MY_TOOL_HOME
```

不同 home directory 是独立 singleton namespace。`--home A` 和 `--home B` 可以各自拥有一个
singleton server、registry、token、projects、sessions 和 data store。

## Server Singleton

server registry 和 durable data store 默认位于 user-level home namespace 下。手动 `--home`
选择另一个 namespace 时，registry 和 data store 都进入该 namespace。
server 进程启动后必须主动 `chdir` 到该 home namespace；它不继承调用者所在项目目录作为运行 cwd。
`--config` 不参与 server 启动和 registry。

registry 记录：

```json
{
  "cwd": "C:\\Users\\rex\\AppData\\Roaming\\sai",
  "pid": 12345,
  "base_url": "http://127.0.0.1:49321",
  "token": "random-local-secret",
  "version": "...",
  "started_at": "2026-07-03T12:00:00Z"
}
```

client 复用 registry 前必须先 health-check。health-check 失败的 stale registry 可以被覆盖。
server auto-start 和 explicit background start 必须用 file lock 避免并发双启动。

server 命令：

```text
<cmd> server
<cmd> server --background
<cmd> server status
<cmd> server stop
```

`<cmd> server` 以前台启动当前 namespace 的 singleton server，不绑定 cwd、project 或 config；
server 运行 cwd 固定为 home namespace。若已有健康 server，应提示 already running 并退出 0。
`<cmd> server --background` 显式启动后台
server。`server status` 和 `server stop` 不因为没有 server 而 auto-start。旧的 scoped
multi-server list 行为移除。

以下命令不会 auto-start：

- help / usage
- version
- `server`
- `server --background`
- `server status`
- `server stop`

以下命令在需要 server 时 auto-start：

- bare attach
- send
- project commands
- session commands

## Auth and Listen Boundary

第一版默认只监听 loopback。`GET /health` 是 public loopback discovery endpoint，只返回最小
非敏感 liveness 信息，例如 status、version 和 pid，不返回 token、project、session、path 或
配置详情。除 `GET /health` 外，所有 HTTP 和 WebSocket 请求都必须带 bearer token。

token 存在 home namespace 下，权限尽量限制为当前 OS user 可读写。第一版不实现多用户登录。
如果未来支持非 loopback 地址，必须重新设计认证、权限提示和安全边界。

## Projects

project identity 只由 canonical cwd root 决定。project 必须显式创建，并保存在 user-level
registry / data store 中；不会在项目目录写 marker 文件。

命令：

```text
<cmd> project create [--cwd PATH] [--name NAME]
<cmd> project list
<cmd> project show [--project PROJECT_ID]
<cmd> project remove [--project PROJECT_ID]
<cmd> project remove --delete-data [--project PROJECT_ID]
```

规则：

- `project create` 默认使用 effective cwd，也可用 `--cwd PATH` 指定 root。
- canonical root 完全重复时，返回已有 project info 并退出 0。
- project name 只用于展示，不参与 identity。
- 嵌套项目允许存在。
- 运行命令时从 effective cwd 向上查找最近的已注册 ancestor project。
- `project show` 和 `project remove` 未传 `--project` 时使用 current cwd discovery；它们不接受
  `--cwd`。
- 找不到 project 时，bare attach/send/session create 等需要 project 的命令必须失败并提示显式
  `project create`。
- `project remove` 默认 archive / hide，不删除真实数据。
- 真实数据删除必须显式传 `--delete-data`。
- 仍有 running sessions 时，remove 和 delete-data 都必须被阻止。

## Sessions

session 必须显式创建。不会隐式创建 project 或 session。`--new` 保留为显式 shortcut，等价于
创建 session 后 attach。

命令：

```text
<cmd> session create
<cmd> session list
<cmd> session show <session-id>
<cmd> session rename <session-id> <name>
<cmd> session archive <session-id>
```

旧的 `<cmd> sessions` 可以作为 list alias 保留给实现选择，但 primary command shape 是 singular
`session`。

`session list` 默认列出当前 project 的 non-archived sessions，支持：

```text
<cmd> session list --project <project-id>
<cmd> session list --all-projects
<cmd> session list --archived
```

排序为 `last_used_at desc`，再按 `created_at desc`。

## Session Config and CWD

config 属于 session，不属于 project。创建 session 时记录：

- `config_path`
- provider / model profile / model id
- model parameters
- tools / MCP / skills metadata
- project id
- `created_cwd`

每个 turn 都重新读取该 session 的 `config_path`。`--config` 只允许在创建新 session 时使用；用于
existing session attach/send 时必须报错。第一版不提供 session config mutation command。

existing session 的 cwd 固定为 `created_cwd`。`--cwd` 只允许用于 project create、session create
和 `--new`。attach/send existing session 时传 `--cwd` 必须报错，避免同一 session 的工具边界和
项目上下文在不同目录间漂移。

## Attach and Send

bare `<cmd>` 等价于 attach：

1. auto-start server if needed。
2. 从 effective cwd 向上查找最近 registered project。
3. 无 project 时失败。
4. 当前 project 无 non-archived session 时失败。
5. 否则 attach 最近使用的 session。

```text
<cmd>
<cmd> --new
<cmd> attach <session-id>
<cmd> send <session-id> --prompt "..."
<cmd> send --prompt "..."
```

`<cmd> --new` 创建 session 后 attach。`send --prompt` 未指定 session id 时使用当前 project
最近 non-archived session。

selection 规则：

- 显式 session id 是 global，不受 cwd/project 限制。
- 未指定 id 时，只在当前 project 中选择最近 non-archived session。
- 多个 observers 可以 attach 同一 session。
- 同一 session 同时只能有一个 active turn。
- session busy 时 send 返回 `session_busy`，不能自动选择另一个 session。
- 不同 sessions 可以并发运行。

## Shutdown

默认 shutdown 是 immediate stop / cancel / cleanup：停止接受新请求，取消 running turns，关闭
HTTP/WS 和后台资源，清理 registry。

`--wait` 进入 drain 模式：server 停止接受新的 turns，等待已经开始的 calls / turns 完成，并支持
timeout。timeout 后按 immediate stop 处理。

OS signals 和 Ctrl+C 使用 immediate stop 语义。server restart 后，之前 running 的 turns /
sessions 标记为 interrupted，不自动 replay。

## HTTP and WebSocket API

API 使用 explicit project/session paths，CLI 只负责把 cwd 映射到 project。未来 GUI 可以直接分页
projects 和 sessions。

建议 endpoints：

```text
GET  /health
GET  /server
POST /server/shutdown

GET  /projects
POST /projects
GET  /projects/{project_id}
DELETE /projects/{project_id}

GET  /projects/{project_id}/sessions
POST /projects/{project_id}/sessions

GET  /sessions/{session_id}
PATCH /sessions/{session_id}
POST /sessions/{session_id}/archive
POST /sessions/{session_id}/messages
GET  /sessions/{session_id}/items
GET  /sessions/{session_id}/content/{blob_hash}
WS   /sessions/{session_id}/stream
```

`/sessions/{session_id}/content/{blob_hash}` 必须校验 blob 可由该 session 到达；不能提供裸全局
blob 读取。items API 支持基于 session seq 的分页，并支持过滤 GUI 不需要展示的 hidden
summary/debug records。

## Persistence

durable data store 位于 home namespace 下，和 server registry 分离。默认不写 project repo。

建议布局：

```text
<home>/
  server/
    registry.json
    token
  data/
    projects/
      <project-id>/
        project.json
    sessions/
      <session-id>/
        meta.json
        segments/
          000001.jsonl
    blobs/
      sha256/
        ab/
          <hash>.data
```

要求：

- project/session durable data 在 user-level namespace 下。
- session transcript 继续使用 append-only JSONL。
- 大内容写 hash-addressed blobs。
- global blob dedupe 可以接受，优先采用。
- session seq 支持 pagination。
- hidden summary/debug records 可被 GUI 过滤。
- server registry 只记录当前 singleton 进程连接信息，不作为 durable project/session 数据源。

## Acceptance Criteria

- 同一 OS user、raw command basename 和 home namespace 下最多复用一个健康 server。
- 不同 `--home` directories 形成独立 singleton namespaces。
- foreground server 和 explicit `server --background` 都可启动；client commands 可按需 auto-start。
- server 启动后 cwd 为 home namespace，不读取或记录 cwd 下的 config。
- project 必须显式 create；duplicate canonical root 返回已有 project info 并退出 0。
- nested projects 通过 nearest registered ancestor discovery 正确选择。
- session 必须显式 create；`--new` 是 create-and-attach shortcut。
- existing session attach/send 拒绝 `--config` 和 `--cwd`。
- config_path 属于 session，每个 turn 重新加载。
- old scoped server behavior 和 chat product entry 被直接替换，无兼容层。
- user-facing help/errors/docs 不硬编码命令名，环境变量按 raw basename 派生。
- explicit project/session HTTP paths 可用，CLI 能把 cwd 映射到 project。
- `GET /health` 只返回 public loopback minimal liveness；其他 HTTP/WS endpoint 都要求 bearer
  token。
- shutdown immediate / wait / timeout / signal 语义明确。
- restart 后 running turns/sessions 标记 interrupted，且不会自动 replay。
- JSONL/blob persistence 支持 seq pagination 和 blob dedupe。
- `go test ./...` 通过。
- `git diff --check` 通过。
- `docs/tasks/global-server-projects-checklist.md` 记录实现 smoke evidence。
