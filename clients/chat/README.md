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
  drops the files flat); `thread.aui.tsx` carries three local patches — the
  queued-steer rows in the composer (upstream has no queue rendering),
  `w-full` on the thread root (the registry root has no width class and
  collapses to zero width in a shrink-to-fit wrapper), and
  `turnAnchor="bottom"` (the nova preset anchors turns at the top and
  defaults autoScroll OFF — classic chat behavior wanted).
  `tooltip-icon-button.tsx` uses the Base UI `delay` prop name.

Session management (C6a+C6b): the sidebar's "+" (or an exact `/new`
in the composer — `/newfoo` sends as an ordinary message) opens the
new-session dialog — project (defaults to the current session's),
model (from `GET /api/models`, the marked default preselected,
keyed by list index), reasoning level (only when the selected model
has tiers); submitting POSTs `/api/projects/{pid}/sessions` and
navigates to the created id; error codes keep the dialog open with a
readable hint. Each sidebar row has archive and delete buttons —
delete sits behind a confirmation dialog (cancelling sends nothing),
archive is direct (it is reversible). The bottom of the sidebar is a
collapsed `archived (N)` section fed by
`GET /api/projects?include_archived=1` (the poll fetches the all
view once and partitions); clicking an archived row restores it.
`run_active`/`model_unavailable`/`session_locked` refusals surface a
hint in the status strip; the closing-window codes retry silently
(shared `opStep` loop — the busy flag spans the whole flow, so a
retry in flight cannot be double-clicked into a second loop). A deep
link to a deleted or unknown session probes `/state` first (an SSE
stream cannot read status codes), shows the hint, and bounces to the
list. The decision logic lives in `src/newsession.ts` and is pinned
by `internal/web/testdata/newsession_check.mjs` (node fixture, driven
by `TestNewSessionFixture`, 9 scenario blocks).

Known v1 gaps, on purpose: `context.compacted` / `session.created`
records have no chat rendering (the `/log` view is the record-level
truth), anchored queue moves are no-ops (the server queue is FIFO by
mint time), message edit/branch/regenerate are not wired (the wire
protocol has no branching),

Also known: a deep link to an UNKNOWN session id (or one this serve
cannot open) selects it, fails to connect, and stays an empty view
with a disabled composer — no explanation, no bounce to the list.
(The deleted-session deep link this paragraph used to name is fixed
in C6b: the /state probe, hint, and hash clear — the log view's
handling, copied. A deep link to an ARCHIVED session probes the same
way and presents "this session is archived — restore it from the
sidebar", staying on the view.)
## Sizes

Measured after the official-style switch: JS 1,073,593 B (333,255 B
gzipped), dist total 1,244,142 B including five Geist woff2 subsets —
React + assistant-ui + Tailwind + fonts. The Go binary serves it as
static files; nothing is embedded.

## The blank-view evidence chain

The "select a session, blank content" investigation ended with four
independently verified layers, each with its own fixture:

| Layer | Check | What it pins |
|---|---|---|
| server snapshot | TestFirstStreamSeesHistory | the first-ever /stream carries the history (the hub seeding race) |
| store state machine | store_check.mjs (TestChatStoreMirror) | mirror consistency with the log page's reducer, 14 scenarios |
| conversion | convert_check.mjs (TestConvertFixture) | every shape through the REAL runtime validator |
| render | render_check.mjs (TestRenderFixture) | the SSR probe: real records -> real Thread HTML, and the ADAPTER SHAPE (`useExternalStoreRuntime(adapter)`, not `{adapter}` — written wrong twice) |

Note: `dist/` is manually built and gitignored — after pulling server
or client changes, `npm run build` before (re)starting serve, or the
page may be an older bundle than the binary expects. This is REQUIRED
after a digest-chain version bump (e.g. sai/v1/ → sai/v2/): the chain
module is bundled into dist, so an un-rebuilt dist keeps folding with
the old prefix and permanently shows ❌ drift (every server digest
misses its chain). The release workflow does not build the chat dist
either — it ships server binaries only, so rebuild manually before
serving a released binary with /chat.
