# Session Incremental Persistence Task Checklist

This checklist tracks the remaining implementation slices for
`docs/session-incremental-persistence.md`. It is intentionally smaller than the design
doc and does not advance `docs/checklist.md` or `docs/milestones.md` status.

Design baseline: the v2 store is append-only JSONL + content-addressed blobs; turn
persistence is currently a single atomic transaction at turn end
(`AppendItemsAndReplaceActiveHistory`, `internal/sessions/v2.go:741`). This change adds
incremental tool-result persistence via a new `item.updated` record and an event-bus +
projector architecture.

Out of scope for this work: moving tool calls out of the assistant message (B2 / parts
model), persisting streaming deltas, migrating to SQLite or adding an EventTable, blob
GC, and auto-rerunning interrupted tools.

## Phase 1 — Storage foundation

- [x] Add `Status string` field to `SessionItem` (`internal/sessions/v2.go:53`) with
  constants `ItemStatusPending/Completed/Error/Interrupted`. Empty = legacy/completed.
- [x] Add `RecordTypeItemUpdated = "item.updated"` constant.
- [x] Add `item.updated` case to `replayCommittedRecord` (`v2.go:1670`): replace the
  matching-`ID` item's **mutable fields only** (`Message`/`Content`/`Status` — `SessionItem`
  has no `UpdatedAt` today) in place (preserve slice order); **explicitly keep
  `existing.Seq`, `existing.ID`, `existing.CreatedAt`** (birth seq immutable); unknown ID →
  `corrupted`.
- [x] Add `V2Store.UpdateItem(sessionID, item)` method (mirror `AppendItem`,
  `v2.go:508`): replay for `LastSeq` → `blobifySessionItemContent` → write a single
  non-transactional `item.updated` record with `record.Seq = LastSeq+1`, but **do NOT
  overwrite `item.Seq`** (it stays the birth seq) → return updated item.
- [x] Materializer synthesis in `materializeActiveHistory` (`v2.go:143`) and store
  variant (`v2.go:504`): for `role:tool` items with `Status` pending/interrupted,
  synthesize `Content="[tool execution interrupted]"`, `IsError=true` in-memory (do
  not mutate persisted item).
- [x] Catch-up: add `item.updated` case to `persistedEventFromRecord` (`v2.go:1633`)
  emitting `{Seq=record.Seq, Type:"item.updated", ItemID}`.
- [x] Tests: append→UpdateItem→replay **and `item.Seq` stays the birth seq**; update
  unknown ID→corrupted; update inside and outside a transaction; blobified update (new
  content >4KB writes a new blob, `ReadBlob` resolves); materializer synthesis for
  pending/interrupted; **`item.Seq` monotonicity after update** (slice still ordered by
  birth seq, `paginateSessionItems` `AfterSeq`/`BeforeSeq` cursors and
  `sessionItemPageSeqBounds` remain correct); legacy session (no Status, no updated
  records) replays unchanged.

## Phase 2 — Event bus + SessionProjector

- [x] Extract **small** planning helpers into a shared package (`internal/sessionplan`
  or `sessions`) for CLI + server reuse: ID allocator (`nextSessionItemID`, `cli.go:5575`),
  `sessionItemFromMessage` (`cli.go:5588`), metadata refresh, and active-history
  append/replace helpers. **Do NOT** promote `sessionSavePlan` (`cli.go:5175`) as a shared
  API — its positional `messages[len(activeItemIDs):]` diff is the old turn-end model; the
  projector creates/updates specific items per event, not by diffing the full message
  slice. Let `sessionSavePlan` be progressively deprecated rather than reused.
- [x] New package `internal/eventbus`: `Event` interface; durable domain events
  (`TurnStarted{TurnID}`, `CompactionRequested{TurnID, ...}`,
  `TurnInputReady{TurnID, Message}`, `AssistantReady{TurnID, Message}`,
  `ToolResultReady{TurnID, Result}`, `TurnCompleted{TurnID}`, `TurnInterrupted{TurnID}`);
  transient wrapper for `model.Event`; `Bus` with synchronous durable `Publish` (blocks
  until projector writes+acks), async transient fan-out, and `Subscribe() <-chan Event`.
  `TurnInterrupted` carries **only `TurnID`** — the pending-tool set is the projector's
  own state (it created those items), not passed by orchestration (avoids under-marking
  when orchestration lacks the full pending set).
- [ ] **Single-writer hard constraint**: projector is a single goroutine consuming a
  channel; durable `Publish` synchronously enqueues and waits for ack, so same-session
  durable events are strictly serialized. (Current store has no locks —
  `internal/sessions` has zero `sync.Mutex`/`Lock`; `appendRecord` is replay-then-write.
  Serialization must come from the single-consumer projector, optionally plus a
  per-session store write lock as defense.)
- [ ] **Session-level single-writer boundary (hard precondition — per-turn projector is
  not sufficient)**: creating a bus/projector **must** be gated by acquiring a
  session-level turn lock first, so at most one active projector exists per session at
  a time. Server path reuses `beginSessionTurn` (`server.go:1361`, `p.mu` +
  `runningTurns[sessionID]` → `beginTurnBusy` if already running), called before
  `TurnStarted`/`MarkTurnRunning`. CLI is single-turn-per-process. **Multi-process**
  (two `sai --resume` on the same session, or CLI + server on the same session) is not
  covered by the in-process mutex — add a **per-session store write lock** (file lock) as
  a backstop. `MarkTurnRunning` (meta.json `running_turn_id`) is the persistent running
  marker for crash recovery, **not** a concurrency mutex (meta.json read-then-write is
  non-atomic).
- [ ] New `SessionProjector` subscribing to the bus — the **sole** writer to both the
  JSONL log and meta.json lifecycle. Event order: orchestration emits `TurnStarted` →
  `CompactionRequested`? → `TurnInputReady`, then `agent.run` emits `AssistantReady` →
  `ToolResultReady` ×N → (next round) …, then orchestration emits `TurnCompleted` (or
  defer `TurnInterrupted`). Handlers: `TurnStarted` → `MarkTurnRunning` (before
  pre-turn compaction); `CompactionRequested` → `SaveCompactedTurn` (single tx),
  **then refresh the projector's cached `LastSeq` + `Items` + `ActiveHistory`** from the
  `SaveCompactedTurn` result (compaction advanced `LastSeq`, replaced `active_history`,
  appended summary/checkpoint) before handling `TurnInputReady` — otherwise later writes
  use a stale `LastSeq` (seq clash) or stale `ActiveHistory`;
  `TurnInputReady` → append user item + advance active_history (legal prefix ending in
  a user message); `AssistantReady` → `AppendItemsAndReplaceActiveHistory` (assistant +
  N pending tool items, advance active_history to legal prefix, maintain
  `toolCallID→itemID` map); `ToolResultReady` → `UpdateItem` (completed/error);
  `TurnCompleted` → `ClearRunningTurn` + metadata (**no compaction** — only pre-turn
  compaction exists); `TurnInterrupted{TurnID}` → projector uses its own state to find
  current-turn pending tool items and **writes `item.updated`** marking them
  `interrupted` (required, not optional) + `MarkTurnInterrupted` (pending set is the
  projector's, not carried in the event). The server handler no longer calls
  `MarkTurnRunning`/
  `ClearRunningTurn`/`MarkTurnInterrupted` directly — all lifecycle writes go through
  the projector.
- [ ] Tests: fake bus + fake store; per round `AssistantReady` precedes that round's
  `ToolResultReady`s; single `TurnCompleted`; multi-round map + active_history growth;
  durable `Publish` is synchronous (record on disk before return); **same session
  concurrent/sequential durable events → final seqs are contiguous (no gaps, no
  duplicates) and replay is correct**; **after `CompactionRequested`, projector cached
  state (`LastSeq`/`Items`/`ActiveHistory`) is refreshed before `TurnInputReady`** (a
  following write does not collide on `LastSeq` or target the pre-compaction
  `ActiveHistory`).
- [ ] **Session-level single-writer test**: two concurrent turns on the **same**
  session → the second is rejected/serialized by the session turn lock (server:
  `beginSessionTurn` returns `beginTurnBusy`), never two projectors writing
  concurrently. (Distinct from the within-one-bus concurrency test above, which does
  not cover the two-turn/two-projector case.)

## Phase 3 — Agent bus integration + CLI path

- [x] Add optional `Publisher eventbus.Publisher` + `TurnID string` to
  `agent.Options` (`agent.go:20`); nil publisher preserves current buffer-and-return
  behavior, and `TurnID` is required only when a publisher is configured.
- [x] **Event split — `agent.run` only emits `AssistantReady` (model-round end, ~line
  75-79, **not per `ToolCallDoneEvent`**, before tool execution) and `ToolResultReady`
  (after each `executeToolCall` ~line 87). Publisher error → emit `ErrorEvent` and
  return. `TurnStarted`/`TurnCompleted`/`TurnInterrupted` are **not** in `agent.run`.
- [ ] **Orchestration layer** (CLI `runChatTurn` ~`cli.go:4231`, server
  `runServerOwnedSessionTurn` ~`cli.go:5973`) emits turn lifecycle: `TurnStarted`
  **before** pre-turn compaction → `CompactionRequested` (if `autoCompactBeforeTurn`/
  `planAutoCompactBeforeTurn` decides to) → `TurnInputReady` (after compaction, before
  starting `agent.run`, carrying the user prompt) → run agent → `TurnCompleted` (success)
  or defer `TurnInterrupted` (any non-success exit). This matches existing code order
  (compaction → append user message → agent turn).
- [ ] Rendering adapter: `writeStreamWithOptions` (`cli.go:6456`) subscribes to bus
  transient events via a **bus→channel bridge** feeding the existing `events` channel
  (decided — not "dual-fan-out", to keep a single event path per 建议 3).
- [ ] **Bus scope**: bus is **per-session/per-turn** (events carry `TurnID` only, no
  `SessionID`). UI/renderer subscribes directly. Server's process-level WebSocket
  stream hub (existing, routes by session ID) subscribes to the per-turn bus as a
  bridge for process-wide live fan-out; catch-up still via `PersistedEventsAfter`.
- [ ] CLI assembly in `runChatMessagesInTurnWithEventHook` (`cli.go:4266`): construct
  bus + projector, pass publisher to agent, subscribe renderer. Remove end-of-turn
  `saveUpdatedMessages`/`SaveTurn` (`cli.go:4304/5166`); `TurnResult` still updates
  in-memory state.
- [ ] Tests: CLI multi-tool turn, per-tool on-disk item status pending→completed;
  mid-turn kill leaves completed results on disk.

## Phase 4 — Server path + catch-up

- [x] Server assembly: `serverAgentTurnRunner` (`cli.go:5889`) injects publisher;
  projector writes storage (replaces `runtime.saveSessions=false` + end-of-turn
  `SaveTurn`).
- [x] WebSocket: clients subscribe to the **existing process-level stream hub** (by
  session ID, unchanged); the hub is the **sole bridge subscriber** of the per-turn bus,
  forwarding `text.delta`/`tool.started`/`tool.finished` and durable
  `item.appended`/`item.updated` to clients (replaces `publishModelTurnEvent`,
  `server.go:1657`). Per-turn bus is not subscribed by WebSocket clients directly.
- [x] Catch-up: add `item.updated` mapping in `sessionStreamEventFromPersistedEvent`
  (`server.go:1636`) emitting `{Seq, Type:"item.updated", ItemID}`.
- [x] **`item.updated` client refetch (explicit)**: since `item.Seq` is the immutable
  birth seq and `PersistedEvent.Seq` is the update-record seq, clients **cannot**
  re-fetch the mutated item via `/items?after_seq=...`. Add a **`GET
  /sessions/{id}/items/{itemID}`** endpoint returning the **blob-resolved item** (real
  persisted `Status` + `Message` + blob-resolved content); on `item.updated` catch-up,
  clients fetch that one item by ID. Event carries only `ItemID` (not the full item — may
  be blob-backed/large). `item.appended` still uses `after_seq` paging (birth-seq cursor
  valid). **The endpoint returns the real `Status` — it does NOT synthesize
  pending→interrupted** (that synthesis is `MaterializeActiveHistory`'s provider/resume
  semantics only); a still-running tool item shows as `pending`.
- [ ] Remove end-of-turn `SaveTurn`/`SaveCompactedTurn` (`server.go:1145-1150`) and
  post-commit batch `item.appended` publish (`server.go:1161-1179`).
- [x] **Compaction/projector coexistence (explicit)**: compaction is dispatched
  **through** the projector as a `CompactionRequested` domain event → projector calls
  `SaveCompactedTurn` (one atomic tx). This keeps the projector the sole writer (no
  second writer racing the incremental stream). **Only pre-turn compaction exists**
  (matches code — `autoCompactBeforeTurn`/`planAutoCompactBeforeTurn`, no end-of-turn
  compaction); it fires between `TurnStarted` and `TurnInputReady`, before the first
  `AssistantReady`. `TurnCompleted` does **not** do compaction (only `ClearRunningTurn`).
  **`SaveCompactedTurn` is narrowed**: it no longer carries the turn's items (they are
  persisted incrementally during the turn); it commits only summary + checkpoint +
  active_history replacement. **Behavior change**: compaction now persists pre-turn; if
  the turn fails after compaction commits, the compaction stands (acceptable — items
  retained in the ledger, no data loss; resume sees [compacted history, user prompt]
  which is legal). Compaction failure fails the turn before `agent.run` (`TurnInterrupted`),
  same as today.
- [ ] Tests: mid-turn disconnect/reconnect → catch-up contains `item.appended` +
  `item.updated`; no duplicate end-of-turn SaveTurn; pre-turn compaction and the
  subsequent incremental stream do not interleave; compaction and `UpdateItem` never
  race (single writer); turn failing after compaction leaves a legal resumable
  compacted history.

## Phase 5 — Interruption & failure cleanup

- [ ] **Hard rule**: after `MarkTurnRunning`, every non-success exit path publishes
  `TurnInterrupted` and persists it — never leave a running turn. Enforce via a
  defer/finally around the agent turn (the `MarkRunningTurnsInterrupted` startup sweep,
  `v2.go:354`, is the crash last-resort only; normal failures must not rely on it).
- [ ] **Storage-failure last resort (honest)**: if the failure is the projector/store
  itself being unavailable, `TurnInterrupted` publish may also fail. Then: (a) the defer
  best-effort calls `store.MarkTurnInterrupted` directly (bypassing the bus — meta.json
  is a single-file rewrite, no `LastSeq` race, more likely to succeed); (b) if even
  meta.json is unwritable, the running turn **cannot** be cleared this run — the "no
  running turn" acceptance is **not provable** under storage failure and relies on the
  next-startup `MarkRunningTurnsInterrupted` sweep. State this as an explicit residual
  risk, do not paper over it.
- [x] Projector `TurnInterrupted` handler: **write `item.updated`** setting all
  current-turn `Status=pending` tool items to `interrupted` (required, not optional —
  on-disk honesty; materializer synthesis is only the SIGKILL fallback for when the
  handler never ran), then `MarkTurnInterrupted`.
- [ ] **Persistence-failure reconciliation (explicit)**: on durable `Publish` failure,
  the turn **aborts immediately** — agent emits `ErrorEvent`, the outer defer publishes
  `TurnInterrupted`, no further tool/model round runs. In-memory `messages` is
  **discarded** (not rolled back, not advanced); disk (via projector) is authoritative;
  next resume reads from disk. Tool-execution error result is **not** this case (normal
  path, `ToolResultReady(IsError=true)` persists and the turn continues).
- [ ] Cover each failure branch: `AssistantReady` publish failure (atomic write — either
  all or nothing; pending items left if written); `ToolResultReady` publish failure
  (tool item left pending); tool-execution error result (**normal path** — publish
  `ToolResultReady(IsError=true)`, item → `error`, turn continues, NOT a cleanup case);
  user Esc / `ctx` cancel; server handler error (handler defer publishes
  `TurnInterrupted`).
- [ ] Verify materializer synthesis (Phase 1) is the safety net: crash →
  `MarkRunningTurnsInterrupted` (meta.json only) → resume `MaterializeActiveHistory`
  synthesizes interrupted for pending → `validateActiveHistoryToolExchanges`
  (`cli.go:5511`) passes → provider request is valid.
- [ ] Tests: each failure branch leaves no running turn and a legal resumable history;
  `TurnInterrupted` handler writes `item.updated` (pending → interrupted) on disk, not
  just in-memory synthesis; persistence-failure abort discards in-memory `messages` and
  leaves disk authoritative; crash after round-commit before results → resume succeeds,
  history legal, new turn continuable, pending items surface as interrupted.
- [x] **Test the user-cancel window explicitly**: `TurnInputReady` has persisted the
  user prompt, then Esc / `ctx` cancel before the first `AssistantReady`. Assert: user
  item is on disk, no pending tool items exist (none created yet), active_history ends
  with the user message (legal), `TurnInterrupted` fires via the defer, no running turn
  remains, and resume can continue a new turn from the persisted user prompt. (Don't
  only test the "pending tool interrupted" case — this no-pending-yet window is
  distinct.)

## Phase 6 — Integration and regression

- [ ] End-to-end: CLI and server each run a multi-tool turn; verify per-tool on-disk
  status; mid-turn kill verifies resume.
- [ ] active_history legality: `validateActiveHistoryToolExchanges` passes after every
  hook point.
- [ ] Regression: legacy sessions (no Status / no updated records) load/resume/compact
  unchanged; `SaveCompactedTurn` path still works.
- [ ] Performance: durable `Publish` serializes disk IO into the tool-execution path
  (each tool result waits on `UpdateItem`). Since `UpdateItem`/`AppendItemsAndReplaceActiveHistory`
  replay the full session per call today, the **projector must cache the session's
  replayed state (`LastSeq` + `Items` + `ActiveHistory`) for the turn** and reuse
  `LastSeq+1` for **both** `UpdateItem` and `AppendItemsAndReplaceActiveHistory` (both
  reduce to `appendRecords`; cached-state write path covers append + replace, not just
  `UpdateItem`) — replay once at turn start, not per tool / per round. (This cached
  state is the projector's private state, reinforcing the
  single-writer constraint.) **API seam (decide in Phase 1)**: this requires either (a)
  a store API that accepts a caller-provided seq / cached state
  (`UpdateItemAtSeq` / exposed `AppendRecords`), or (b) co-locating the projector with
  the store (`internal/sessions` or `internal/sessionplan`) so it reuses internal
  `appendRecords` + cached `LastSeq` directly. Do **not** have an external projector
  replay then call `UpdateItem` (which replays again) — no gain and widens the race
  surface. Add a seq→segment index only if extreme sessions still suffer (not in scope).

## Acceptance Points

- [ ] A tool result is on disk with `Status=completed` the instant its tool finishes,
  before turn end, on both CLI and server paths. Persistence points = round boundary
  `AssistantReady` + per-result `ToolResultReady`; **not** per-delta, **not** per
  `ToolCallDoneEvent`.
- [ ] `item.updated` does not overwrite `item.Seq` (birth seq immutable); after update
  `state.Items` stays monotonic by birth seq — `paginateSessionItems`
  `AfterSeq`/`BeforeSeq` and `sessionItemPageSeqBounds` remain correct.
- [ ] `active_history` passes `validateActiveHistoryToolExchanges` at all times; a
  multi-tool assistant message's pending tool items are in the prefix and the prefix
  is legal before results arrive.
- [ ] Same-session concurrent/sequential durable events are strictly serialized; seqs
  are contiguous (no gaps/duplicates); replay is correct (single-writer hard constraint).
- [ ] **Session-level single-writer**: bus/projector creation is gated by a session turn
  lock (server `beginSessionTurn`); two concurrent turns on the same session never spawn
  two writers. Multi-process same-session is backstopped by a per-session store write
  lock. `MarkTurnRunning` is a crash-recovery marker, not a concurrency mutex.
- [ ] Projector cached state is refreshed after `CompactionRequested` before
  `TurnInputReady` (no stale-`LastSeq`/`ActiveHistory` writes post-compaction).
- [ ] After `MarkTurnRunning`, every failure/Esc/error exit publishes `TurnInterrupted`
  and persists it — no running turn left; crash is covered by the
  `MarkRunningTurnsInterrupted` sweep. `TurnInterrupted` handler **writes `item.updated`**
  (pending → interrupted) on disk; materializer synthesis is SIGKILL fallback only.
- [ ] On durable `Publish` failure the turn aborts immediately: in-memory `messages`
  discarded, disk authoritative, no rollback/continue. (Tool-execution error result is a
  normal path, not this case.)
- [ ] Resume after crash shows completed tool results; unfinished tool items surface as
  interrupted error results; history is legal and a new turn can continue; no tool is
  auto-rerun.
- [ ] Compaction is dispatched through the projector (`CompactionRequested` →
  `SaveCompactedTurn`, single atomic tx, **no turn items**), keeping the projector the
  sole writer; **only pre-turn compaction** exists (between `TurnStarted` and
  `TurnInputReady`, before the first `AssistantReady`); `TurnCompleted` does no
  compaction. Turn failing after compaction leaves a legal resumable compacted history.
- [ ] `TurnInterrupted{TurnID}` carries only `TurnID`; the projector derives the pending
  set from its own state (no under-marking).
- [ ] The projector caches the session replayed state per turn so `UpdateItem` **and**
  `AppendItemsAndReplaceActiveHistory` reuse `LastSeq+1` without re-replaying per
  tool/round.
- [ ] WebSocket clients subscribe to the existing process-level stream hub; the hub is
  the sole bridge subscriber of the per-turn bus. Reconnect catch-up includes
  `item.appended` and `item.updated` (`PersistedEvent.Seq` = record seq); `item.updated`
  is refetched via `GET /sessions/{id}/items/{itemID}` (not `after_seq` paging).
- [x] Test covers the user-cancel window: Esc after `TurnInputReady` persisted but
  before the first `AssistantReady` → user prompt on disk, no pending tools, legal
  history, no running turn, resume continuable.
- [ ] Legacy sessions (no `Status`, no `item.updated`) load/resume/compact correctly
  under the new code.
- [ ] Persistence has a single path (projector); CLI and server no longer maintain
  separate `sessionSavePlan` write branches.
