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
  embedded static assets, loopback security, minimal HTTP boundary, WebSocket gateway

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
3. The UI obtains a short-lived WebSocket ticket and opens the single ordered
   sync connection.
4. Project, session, provider, history, compaction, and run control operations
   use typed WebSocket commands/resources. Starting a run returns a command
   acknowledgement and the ordered resource stream carries its lifecycle.
5. Typed session-content events update transient text/reasoning/tool state;
   committed item projection events update the canonical session store.
6. Session Content owns transient replay and durable revision barriers. A
   reconnect uses the WebSocket snapshot/replay contract rather than an HTTP
   run cursor or a second event source.
7. Cancelling a run calls `SessionRun.Cancel` through the typed `run.cancel`
   command and interrupts only that run.

## Web API

The product HTTP surface is intentionally a clean break. It contains only
static assets/SPA fallback and these API routes:

```text
GET  /api/bootstrap
POST /api/ws-ticket
GET  /api/ws                       (upgrade, one-time ticket)
GET  /api/blobs/{blobID}
GET  /api/sessions/{sessionID}/images/{hash}
```

The Go `GET` registrations also provide the standard `HEAD` metadata response
for Blob/image reads; no additional mutation/query route is registered. Blob
`GET`/`HEAD` behavior is covered by the webapp route tests.

Every other `/api` path, including exact `/api`, returns a JSON 404 and never
participates in SPA fallback. API requests without the bearer capability remain
401. The WebSocket subscription contract includes typed `text.delta`,
`reasoning.delta`, tool lifecycle, prompt-queue, durable item, and settlement
notifications. Session Content owns the bounded transient replay and
resynchronization barrier; typed resource snapshots and command results are the
canonical project/session/provider/history/run application surface. The
standalone WebSocket ticket and Blob clients are separate from
`web/src/api.ts`; that ordinary API module contains only bootstrap and session
image reads.

## Security

- Only loopback listen addresses are accepted.
- Every `/api/` request requires the random bearer token.
- Host must resolve syntactically to localhost or a loopback IP.
- Non-empty Origin must exactly match the current HTTP origin.
- CORS is not enabled.
- Static responses use CSP, frame denial, no-referrer, and nosniff headers.
- Provider errors, API keys, hidden session items, and raw tool results are not
  added to WebSocket subscription payloads.

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

Pushing a `v*` tag runs `.github/workflows/release.yml`. The workflow repeats
the full validation suite, builds the three documented target binaries with the
tag injected into `webapp.Version`, verifies the version marker, generates
`SHA256SUMS`, and creates or updates the matching GitHub Release.

## Testing

- `go test ./internal/execution` covers project/session/run contracts.
- `go test ./internal/webapp` covers capability auth, embedded assets, project
  and configured session creation, API route boundaries, WebSocket sync, and
  durable results.
- `npm run build` performs strict TypeScript checking and a production bundle.
- `npm run test:e2e` covers onboarding, session creation, streaming commit,
  cancellation, run recovery, archive/restore, and history scrolling in Chromium.
- `go test ./...` is the full backend regression suite.
- Final releases should be opened in a real browser and smoke-tested from
  project registration through a committed assistant response.

The current implementation and release backlog is
`docs/tasks/v0.1-release-hardening-checklist.md`. M20-M24 server/CLI/mailbox
documents are historical records, not active product specifications.
