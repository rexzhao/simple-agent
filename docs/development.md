# Development

`sai` is a local Web application backed by an in-process Go execution library.
The product has one presentation surface: the embedded browser UI. The previous
interactive CLI, optional TUI, mailbox input server, and HTTP daemon/registry
experiments are retained only in Git history and historical milestone notes.

## Product boundary

- Running the executable with no arguments starts the Web application.
- The server listens only on loopback and normally uses an OS-assigned port.
- Project and session data remain local to the Go process.
- Browser code never loads configuration, credentials, JSONL segments, blobs,
  or provider APIs directly.
- The executable remains a single-file runtime artifact.
- There is no background daemon, server registry, multi-user account system,
  remote listen mode, or second chat implementation.

## Architecture

```text
cmd/sai
  default Web launcher

internal/webapp
  embedded static assets, loopback security, JSON API, SSE run streams

internal/execution
  project/session lifecycle, configured session creation, run handles,
  cancellation, event persistence, compaction, provider/tool assembly

internal/eventbus + internal/sessionprojector
  ordered transient events and incremental durable session projection

internal/projects + internal/sessions
  local project metadata and append-only session/blob storage

web
  React + TypeScript + Vite source
```

The Web layer calls `execution.Service`; it does not duplicate model selection,
tool/MCP/skill loading, compaction, or storage logic.

## Runtime flow

1. The launcher creates a capability token and a loopback listener.
2. The browser receives the token in the URL fragment, moves it to tab-scoped
   session storage, and sends it as a bearer token.
3. Project and session management use JSON HTTP endpoints.
4. Starting a run returns a Web run ID immediately.
5. The UI reads its ordered server-sent event stream with `fetch`.
6. Transient text/reasoning/tool events update the active UI block.
7. `turn.committed` and `run.settled` trigger a refresh from durable session
   items, making the session store the canonical view.
8. Cancelling a run calls `SessionRun.Cancel` and interrupts only that run.

## Web API

Important routes:

```text
GET    /api/bootstrap
GET    /api/projects
POST   /api/projects
GET    /api/projects/{id}/sessions
POST   /api/projects/{id}/sessions
GET    /api/sessions/{id}
GET    /api/sessions/{id}/items?before_seq=&after_seq=&limit=
POST   /api/sessions/{id}/runs
GET    /api/runs/{id}/events
DELETE /api/runs/{id}
POST   /api/sessions/{id}/compact
```

The event contract includes `turn.started`, `text.delta`, `reasoning.delta`,
`tool.requested`, `tool.started`, `tool.finished`, `usage.updated`, persisted
item notifications, `turn.committed`, `turn.failed`, and `run.settled`.

## Security

- Only loopback listen addresses are accepted.
- Every `/api/` request requires the random bearer token.
- Host must resolve syntactically to localhost or a loopback IP.
- Non-empty Origin must exactly match the current HTTP origin.
- CORS is not enabled.
- Static responses use CSP, frame denial, no-referrer, and nosniff headers.
- Provider errors, API keys, hidden session items, and raw tool results are not
  added to Web run events.

## Build and generated assets

The Vite output directory is `internal/webapp/assets`. It is deliberately
checked in so ordinary Go tooling can compile the embedded application after a
checkout. Release scripts run `npm ci` and `npm run build` before Go builds.

```powershell
cd web
npm ci
npm run build
cd ..
go test ./...
powershell -ExecutionPolicy Bypass -File scripts/build.ps1
```

Do not edit generated files under `internal/webapp/assets` manually; edit
`web/src` and rebuild.

## Testing

- `go test ./internal/execution` covers project/session/run contracts.
- `go test ./internal/webapp` covers capability auth, embedded assets, project
  and configured session creation, SSE events, and durable results.
- `npm run build` performs strict TypeScript checking and a production bundle.
- `go test ./...` is the full backend regression suite.
- Final releases should be opened in a real browser and smoke-tested from
  project registration through a committed assistant response.
