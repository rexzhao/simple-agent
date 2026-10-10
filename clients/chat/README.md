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
  the unanchored steer-lane `move` ("run now") stops the live run and
  leaves the item queued for the next drain point.
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
mint time), and message edit/branch/regenerate are not wired (the wire
protocol has no branching).

## Sizes

The built bundle is ~1.07 MB (fonts included; ~370 KB gzipped) —
React + assistant-ui + Tailwind + Geist. The Go binary serves it as
static files; nothing is embedded.
