# Server, Project, and Session Management

This document defines the unified management command model for server, project,
and session resources. It directly replaces the previous `--home`, top-level
`status` / `stop`, `servers`, scoped server cwd, and `project remove
--delete-data` behavior. No compatibility layer is required.

## Source of Truth

This document is the active implementation specification for this task. It
intentionally supersedes conflicting older M21 notes in `docs/milestones.md`,
`docs/checklist.md`, `docs/global-server-projects.md`, or older task checklists.
Those older documents are not modified by this docs-only change, and their
checklist status is not advanced here.

This is a backend and CLI management contract. Browser GUI, server-rendered web
UI, frontend routing, frontend state management, and styling are future work and
stay out of this document.

## Resource Model

Management commands follow:

```text
<resource> <action> [selector] [options]
```

Resources:

- `server`: a local loopback process for one server root.
- `project`: project metadata stored under the selected server root.
- `session`: session metadata and JSONL/blobs stored under the selected server
  root.

`server-root` is the namespace root for server registry and durable data. It is
independent of the current shell cwd. The selected server root is resolved as:

```text
--server-root PATH
${ARGV0_BASENAME}_SERVER_ROOT
user-config-dir / ${argv0 basename}
```

Examples:

```text
sai.exe -> SAI_SERVER_ROOT
my.tool.exe -> MY_TOOL_SERVER_ROOT
simple-agent -> SIMPLE_AGENT_SERVER_ROOT
```

`argv0 basename` comes from `argv[0]` after path separators are removed; a final
`.exe` is stripped case-insensitively for the namespace name. The environment
variable name is derived from that basename by uppercasing it and replacing
non-alphanumeric runs with `_`, then appending `_SERVER_ROOT`.

The default `user-config-dir` is the platform user config directory, not the
project cwd. Implementations should use the host platform's standard user
config directory lookup, such as Go `os.UserConfigDir()`, then append the
`argv0 basename`. If the user config directory cannot be resolved and no
explicit root or environment variable is provided, the command fails with a
clear error.

The old `--home` flag and `${BASENAME}_HOME` environment variable are removed.
They must not be accepted as aliases, migrations, or compatibility fallbacks.

## Server Root Storage

All server registry and project/session data lives under the selected
`server-root`:

```text
<server-root>/server/registry.json
<server-root>/data/projects/<project_id>/project.json
<server-root>/data/sessions/<session_id>/session.json
<server-root>/data/sessions/<session_id>/items.jsonl
<server-root>/data/sessions/blobs/<hash>
```

The server process always `chdir(server-root)` before serving.

Registry is a single-record process connection file and must not store
`cwd`, `server-root`, or `config_path`:

```json
{
  "base_url": "127.0.0.1:12345",
  "pid": 12345,
  "token": "...",
  "started_at": "2026-07-05T00:00:00Z",
  "version": "...",
  "requested_listen": "127.0.0.1:0"
}
```

`server status` output and the `GET /server` response use the same redaction
rule as the registry: they must not show `cwd`, `server-root`, or `config_path`.
Those values are not server identity in this design.

Server-root internal files should avoid absolute server-root references so the
directory can be moved. Project `root`, session `created_cwd`, and session
`config_path` still point to real external paths and remain canonical absolute
paths.

Project records contain only project metadata and lifecycle state:

```json
{
  "id": "project-...",
  "root": "C:\\repo\\example",
  "display_name": "example",
  "archived_at": null,
  "created_at": "2026-07-05T00:00:00Z",
  "updated_at": "2026-07-05T00:00:00Z"
}
```

Session records contain session metadata and lifecycle state. `config_path` is
session-owned, not project-owned, and each turn reloads that path.

```json
{
  "id": "session-...",
  "project_id": "project-...",
  "display_name": "Investigation",
  "created_cwd": "C:\\repo\\example",
  "config_path": "C:\\repo\\example\\.agents\\sai.yaml",
  "archived_at": null,
  "created_at": "2026-07-05T00:00:00Z",
  "updated_at": "2026-07-05T00:00:00Z",
  "last_used_at": "2026-07-05T00:00:00Z"
}
```

## Commands

Server:

```text
<cmd> server start [--background] [--port N | --listen ADDR]
<cmd> server status
<cmd> server stop [--wait] [--timeout-ms N]
```

Project:

```text
<cmd> project create [--cwd PATH] [--name NAME]
<cmd> project list [--archived]
<cmd> project show [PROJECT_ID]
<cmd> project rename [PROJECT_ID] <NAME>
<cmd> project archive [PROJECT_ID]
<cmd> project remove [PROJECT_ID]
```

Session:

```text
<cmd> session create [--cwd PATH]
<cmd> session list [--project PROJECT_ID | --all-projects] [--archived]
<cmd> session show <SESSION_ID>
<cmd> session rename <SESSION_ID> <NAME>
<cmd> session archive <SESSION_ID>
<cmd> session remove <SESSION_ID>
```

Removed commands:

```text
<cmd> status
<cmd> stop
<cmd> servers ...
<cmd> server --cwd
<cmd> --home
<cmd> project remove --delete-data
```

`--server-root` is a global flag and may appear before, after, or between
commands where global flags are accepted:

```text
<cmd> --server-root F:\a project list
<cmd> project list --server-root F:\a
<cmd> project --server-root F:\a list
```

## Selection Rules

- `server start/status/stop` always operate on the selected server root.
- Client commands auto-start only the selected server root's server.
- Current shell cwd never changes server-root selection.
- `project show/rename/archive/remove` may omit `PROJECT_ID`; omitted ID uses
  current effective cwd to find the nearest registered project.
- If `PROJECT_ID` is supplied, cwd discovery is not used.
- `session show/rename/archive/remove` require explicit `SESSION_ID`.
- `session list` keeps `--project` and `--all-projects` because those are
  filters, not target selectors.

## Attach and Send

Existing attach/send behavior remains in scope:

- Bare `<cmd>` defaults to attach: it auto-starts the selected server-root
  server if needed, discovers the current project from the effective cwd, and
  attaches the most recent non-archived session for that project.
- `<cmd> attach <SESSION_ID>` attaches an explicit global session id.
- `<cmd> attach --new` creates a session for the current project and attaches
  it.
- `<cmd> send <SESSION_ID> --prompt ...` sends one turn to an explicit global
  session id and exits.
- `<cmd> send --new --prompt ...` creates a session for the current project,
  sends one turn, and exits.
- Existing session attach/send reject `--cwd` and `--config`; those options are
  only valid when creating a new project or session.
- Multiple observers may attach to the same session stream.
- A session allows only one active turn; same-session send/attach work that
  would start another turn returns `session_busy` and does not select a
  fallback session.
- Different sessions may run turns concurrently.

## Session Open Display Snapshot

When attaching or opening an existing session, clients should load a recent page
of the current session's display transcript before, or as part of, opening. This
snapshot is for GUI/CLI display only. It must not affect model context; model
context continues to use ActiveHistory/resume mechanics.

Snapshot scope and filtering:

- Load only the selected current session. No cross-session or cross-project
  history is inherited.
- Use chat view by default (`view=chat`), excluding hidden summary, debug, and
  internal records.
- Default recent page size is 50 items. An explicit `limit` may adjust that
  size within the existing item API bounds.
- Large content remains blob/metadata-first. Item pages return metadata and blob
  references; bodies are read on demand through the content endpoint.
- Page responses expose pagination cursors including `oldest_seq`,
  `newest_seq`, `has_more_before`, and `has_more_after`. `has_more_before`
  means older items exist before `oldest_seq`; `has_more_after` means newer
  persisted items exist after `newest_seq`.

Recommended open flow:

1. Fetch `GET /sessions/{session_id}/items?view=chat&limit=50` over HTTP and
   render it as the initial display state.
2. Connect `WS /sessions/{session_id}/stream` with `after_seq` set to the HTTP
   page's `newest_seq`.
3. The server streams persisted events with `seq > after_seq` before live
   events, so no persisted events are missed between the snapshot and the live
   connection.

## Server Lifecycle

- Commands that need a server first health-check the selected server root's
  registry. A healthy registry is reused.
- Stale registry records are ignored and cleared before starting a replacement
  server.
- Startup takes a server-root lock so concurrent auto-start/background-start
  attempts cannot create two servers for the same selected root.
- `server status` and `server stop` do not auto-start.
- Shutdown is immediate by default: stop accepting work, cancel running turns,
  clean up process resources, remove the registry record, and exit.
- `server stop --wait` stops accepting new turns and drains already-started
  turns until they finish or the timeout expires.
- If `--wait` times out, shutdown falls back to immediate stop/cancel/cleanup.
- OS signals and Ctrl+C use immediate stop semantics and clean the registry when
  possible.
- After restart, turns or sessions that were running during the previous server
  process are marked interrupted and are not replayed automatically.

## Lifecycle Rules

- `archive` is a soft hide and sets archive metadata without deleting data.
- `remove` is hard delete and only works on already archived targets.
- Active project/session remove fails with a message telling the user to archive
  first.
- Archived project/session rename fails. Restore will be a future feature.
- `project remove` deletes the project metadata and all sessions whose
  `project_id` matches the project.
- Running sessions/turns block archive/remove of the affected project/session,
  even if the target is otherwise eligible.
- Restore is intentionally reserved for future work. Do not add
  `project restore`, `session restore`, or `PATCH {"archived":false}` behavior
  in the first implementation.

List visibility:

- `project list` shows active projects.
- `project list --archived` shows archived projects only.
- `session list` shows active sessions.
- `session list --archived` shows archived sessions only.
- `show` can display archived targets by explicit ID.

## Output Shape

`list` commands use tabular output with a header.

All other management commands use key-value output:

```text
KEY<TAB>VALUE
```

Examples:

```text
STATUS	started
ADDR	127.0.0.1:12345
PID	12345
```

```text
STATUS	already_running
ADDR	127.0.0.1:12345
PID	12345
```

```text
STATUS	running
ADDR	127.0.0.1:12345
PID	12345
VERSION	dev
SESSION_COUNT	0
RUNNING_TURNS	0
UPTIME_SECONDS	12
```

```text
STATUS	stopped
ADDR	127.0.0.1:12345
PID	12345
```

```text
STATUS	removed
ID	project-xxx
REMOVED_SESSIONS	3
```

Representative list headers:

```text
ID	NAME	ROOT	ARCHIVED	CREATED_AT	UPDATED_AT
ID	PROJECT_ID	NAME	CREATED_CWD	ARCHIVED	LAST_USED_AT
```

Representative project/session command output:

```text
STATUS	created
ID	project-xxx
NAME	example
ROOT	C:\repo\example
```

```text
STATUS	archived
ID	session-xxx
```

```text
STATUS	removed
ID	session-xxx
```

## HTTP API

The HTTP API follows the same semantics with no compatibility aliases.

Server:

```text
GET  /health
GET  /server
POST /server/shutdown
```

`GET /health` is the public loopback discovery endpoint and returns only
minimal non-sensitive liveness. All other HTTP and WebSocket endpoints require
the bearer token from the selected server root's registry.

`GET /server` returns server process metadata only:

```json
{
  "pid": 12345,
  "base_url": "127.0.0.1:12345",
  "version": "...",
  "started_at": "2026-07-05T00:00:00Z",
  "uptime_seconds": 12,
  "project_count": 2,
  "session_count": 5,
  "running_turns": 0
}
```

Projects:

```text
POST   /projects
GET    /projects?archived=false|true
GET    /projects/{id}
PATCH  /projects/{id}
DELETE /projects/{id}
```

Sessions:

```text
POST   /projects/{project_id}/sessions
GET    /projects/{project_id}/sessions?archived=false|true
GET    /sessions?all_projects=true&archived=false|true
GET    /sessions/{session_id}
PATCH  /sessions/{session_id}
DELETE /sessions/{session_id}
GET    /sessions/{session_id}/items?before_seq=N&after_seq=N&limit=N&view=chat|debug
GET    /sessions/{session_id}/content/{blob_hash}
POST   /sessions/{session_id}/messages
POST   /sessions/{session_id}/commands/compact
WS     /sessions/{session_id}/stream
```

`PATCH {"display_name":"..."}` renames active targets. `PATCH
{"archived":true}` archives. `DELETE` removes archived targets only. `PATCH
{"archived":false}` is reserved for future restore work and should return an
unsupported operation error in the first implementation.

Session metadata endpoints do not return full items. Item list responses support
pagination and filtered views. Content reads are scoped to blobs reachable from
the requested session; the API does not provide unscoped blob hash reads.
