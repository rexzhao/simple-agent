# sai chat client

The assistant-ui-based chat client for `sai -http`. It lives outside the
Go module on purpose: the embedded log view (`internal/web/static`,
served at `/log`) keeps the zero-dependency, no-build discipline; this
client is the chat-style face over the same wire contract, with its own
React + Vite build chain.

## Build & serve

```
npm install
npm run build                       # -> dist/
sai -http 127.0.0.1:7420 -http-static dist
```

The server mounts `dist/` at `/chat`, which is also the default landing
(`/` redirects there; the log view stays at `/log`).

## Dev

```
npm run dev                         # Vite on http://localhost:5173/chat/
```

The Vite dev server proxies `/api` to a running `sai -http` on
127.0.0.1:7433 (see `vite.config.ts`); the serve side answers CORS
preflights for loopback origins, so a direct connection works too.

## Architecture

- `src/store.ts` — the session state machine: snapshot-first SSE,
  digest-chain fold, in-flight live item, steered queue. The chain
  module is imported from `internal/web/static/chain.mjs` (single
  source, byte-identical to the Go fold).
- `src/convert.ts` — records → `ThreadMessageLike`: the JOIN where
  `tool.result` / `run.failed` records fold into the parent assistant
  message's parts and status (keyed by `tool_call_id`).
- `src/runtime.ts` — the `ExternalStoreRuntime` adapter. The queue
  adapter mirrors server truth: `steerItems` come from the session's
  steered queue (addressed by ItemID over SSE); `enqueue`/`steer`
  route to `/send` or `/steer` by run state with the 409 crossover
  fallback (Rule A); `edit`/`remove` map to `PUT`/`DELETE /steer`;
  the unanchored steer-lane `move` ("run now") stops the live run,
  withdraws the item, and starts a fresh run carrying its text — the
  steered queue only drains at a run boundary, so a bare stop would
  park the message forever.
- `src/App.tsx` — sidebar (projects poll), status strip (chain
  verification), and the official assistant-ui `Thread` — the
  official-site look: Tailwind v4 + shadcn base components + the
  r.assistant-ui.com registry components under `src/components/`
  (markdown text, reasoning, tool fallback, Geist fonts).

  The registry layout the components expect is reconstructed under
  `src/components/assistant-ui/elements/` and `src/utils/` (the CLI
  drops the files flat); `thread.aui.tsx` carries one local patch —
  the queued-steer rows in the composer (upstream has no queue
  rendering), and `tooltip-icon-button.tsx` uses the Base UI `delay`
  prop name.

Known v1 gaps, on purpose: `context.compacted` / `session.created`
records have no chat rendering (the `/log` view is the record-level
truth), anchored queue moves are no-ops (the server queue is FIFO by
mint time), message edit/branch/regenerate are not wired (the wire
protocol has no branching), and there is no session-delete affordance
in the chat client (the `/log` view has it; the runtime's onDelete is
unreachable until a thread list is rendered).

## Sizes

Measured after the official-style switch: JS 1,073,593 B (333,255 B
gzipped), dist total 1,244,142 B including five Geist woff2 subsets —
React + assistant-ui + Tailwind + fonts. The Go binary serves it as
static files; nothing is embedded.

## Page provider (agent-operated debugging)

The page can register itself as an mcp-provider-broker PROVIDER over
WebSocket (`ws://<broker>/hub/provider`) — the agent's MCP debug flow
discovers it and operates the page. Identity is the addressing
contract: `sai-chat-<session>` (the agent in session X drives the page
showing X; switching sessions re-registers). Duplicate-id semantics
(verified live): an active registration is not displaced; the id
carries no credential — the broker's unauthenticated LAN trust class
applies.

Three doors, in order of user intent:

- `sai.pageProvider(ws://host:port/hub/provider, {send: true})` —
  explicit DevTools action; the only path that persists (auto-connects
  on later loads). The URL is required — no baked-in default.
- `?page-provider=<ws-url>` — NOT persisted, NOT auto-connecting: the
  page shows a confirmation banner; confirming connects for this
  session only. A crafted link can never silently wire the page to a
  foreign broker.
- `sai.pageProviderOff()` — stop and clear.

Only loopback/private-network ws targets are accepted. The status bar
shows an active connection (`provider: <host>` + one-click disconnect).
`page_send` is opt-in separately — it drives the REAL composer (native
setter + input event + send click, with a bounded cleared-input
confirmation), which reaches the agent's shell tools; the tool
description states it is dispatched, not confirmed.

Other console tools: `sai.list()`, `sai.snap()`, `sai.messages()`,
`sai.select(id)` — live state inspection from DevTools.
