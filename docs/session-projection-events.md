# Session projection and run event contract

**Status: stage 5 implementation contract.** This document
is a wire-contract decision, not a request to change the current product
behavior. The examples and compatibility notes deliberately describe the
implementation in this repository. Later stages may add fields or migrate an
event name, but must preserve the compatibility rules below.

## 1. Scope and sources of truth

There are two related but different streams:

1. `GET /api/sessions/{sessionID}/snapshot` and the session item APIs expose the
   durable session projection. This is the source of truth for conversation
   history.
2. `GET /api/runs/{runID}/events` exposes a bounded, in-memory stream for live
   rendering and recovery while one run is active. It is not a durable event
   log. A terminal run is retained only for a short recovery window.

`GET /api/events` is a third, process-wide lifecycle SSE stream. It carries
session and run lifecycle notifications, but it does not replace either the
durable snapshot or the per-run stream. In particular, a `run.settled` from
that stream and a `run.settled` from a per-run stream are related notifications,
not two turns.

The durable session store, not browser state and not transient run output, is
the canonical representation of a user or assistant item.

## 2. Durable projection identity

### 2.1 Session revision

The snapshot response has this shape (irrelevant fields omitted):

```json
{
  "session_id": "session-1",
  "revision": "42",
  "session": { "id": "session-1", "last_seq": 42, "revision": "42" },
  "history": {
    "items": [],
    "oldest_seq": 1,
    "newest_seq": 41,
    "has_more_before": false,
    "has_more_after": false
  }
}
```

`revision` is the decimal string form of the session store's global
`LastSeq`. It is a session-wide projection revision, not a run SSE cursor and
not necessarily the sequence of the newest visible item. Item updates,
compaction/history records, and other durable records can advance it without
adding a visible item. An empty session starts at `"0"`.

Clients MUST compare revisions numerically (`BigInt` or an equivalent integer
comparison), not lexicographically. Clients MUST NOT infer that a snapshot is
newer because its `history.newest_seq` is larger, or infer that the snapshot is
stale because no item was added.

The numeric `session.last_seq` in the ordinary session response and lifecycle
payloads is retained as a compatibility representation of the same durable
watermark. Ordinary Session DTOs now also carry `revision` as a decimal string;
the snapshot's string `revision` and `session.revision` are the same value. The
snapshot's string form is the precision-safe form for browser code. A snapshot
is loaded as one aggregate, so a client applies its session metadata and history
together when its revision is newer. Metadata-only
changes can keep the same revision; those metadata changes still apply, while
same-or-older history does not replace newer history.

### 2.2 Item sequence and item identity

Each returned `history.items[]` item has a stable `id` and an item `seq`:

- `id` is the stable identity of the projected item.
- `seq` is allocated when that item is created and remains the item's sequence
  when the item is later updated.
- `turn_id` groups items into a turn; it is not a cursor for either stream.
- `history.oldest_seq` and `history.newest_seq` are item-history page cursors.

An `item.updated` record changes the projection for the existing `item_id`; it
does not create a second item identity. Do not append an update notification as
a new history item.

There is an intentional naming detail in the current implementation: the
top-level `seq` in a persisted item notification is the sequence of the
persisted record that caused the notification. For an item creation this is
also the item's creation `seq`. For an update it is the new durable
record/event sequence, which differs from the item's original
`history.items[].seq`. The nested `item.seq` is always the item's creation
sequence, including for `item.updated`. Consumers identify the item by
`item_id` and must not assign the notification `seq` to the item.

### 2.3 Streaming assistant item lifecycle

The agent creates the durable assistant message at the first non-blank,
user-visible text delta for one logical assistant message. It does not create
an empty assistant placeholder at run admission or `turn.started`. The
creation is committed as the existing `item.appended` event. Subsequent
checkpoints use `item.updated` for the same `item.id`; the nested `item.seq`
(creation sequence) is unchanged while the notification `seq` and session
`revision` advance monotonically.

Checkpointing is cumulative and serialized through the session projector. The
first visible delta is flushed immediately. Later writes happen when either
75ms has elapsed since the previous checkpoint or at least 64 new Unicode
codepoints have accumulated. The terminal path always forces a final flush,
including a provider failure or cancellation after partial output. The final
assistant message event updates that same item when it adds final metadata or
tool calls; it never appends a second copy.

The ordering for every visible item event is commit first, notification second:
the projector writes the item and obtains its durable record sequence before
the committed `item.appended` or `item.updated` DTO is published. A turn may
therefore have a low-latency transient tail after the most recent checkpoint;
that tail is not itself a durable claim. If the forced terminal checkpoint
fails, the authoritative item stops at the previous committed checkpoint and
the run enters the existing failed/resync path. The client must not report
that every transient delta it displayed was persisted.

Each transient `text.delta` now optionally carries additive `item_id`,
`durable_text_length`, and `durable_checkpointed` fields. The item projection
event optionally carries `assistant_text_length`. These fields explicitly
associate the run stream with the durable item; clients must not match text,
turn content, or timing to deduplicate. The frontend renders the durable item
as the message bubble and keeps only the uncheckpointed tail in `ActiveRun`. A
committed append/update clears or replaces that tail, so replay, a fast
append/update sequence, and duplicate projection events still produce one
bubble.

Reasoning deltas, provider items, and tool-internal events do not invoke the
assistant checkpoint path. If the run fails or is cancelled after an assistant
checkpoint, the partial item remains in history and the received tail is
force-flushed when possible; it is not rolled back. If no visible output has
arrived, no assistant item is created. A checkpoint write failure stops the
run before its corresponding transient delta is broadcast and the run enters
the normal failed/resync path; any already-visible but uncheckpointed tail is
not authoritative and the last committed partial item remains the source of
truth. If one turn contains multiple logical assistant messages, each gets
its own stable item id and checkpoint sequence.

## 3. Event envelopes and streams

### 3.1 Per-run SSE envelope

The per-run endpoint writes ordinary SSE frames:

```text
id: 17
data: {"type":"text.delta", ...}

```

Every replayable run event has a decimal SSE `id`. The ID is:

- allocated independently for each run, starting at 1;
- assigned in the order the run registry observes events;
- the replay cursor accepted by `?after=`; and
- unrelated to session `revision`, durable item `seq`, `turn_id`, or the
  process-wide lifecycle stream.

A run event ID is never a globally unique event ID. It is safe to persist it
only together with `run_id`. IDs already assigned to events remain stable when
there are multiple connections. A bounded buffer can remove old IDs, so the
retained IDs need not start at 1 and a reconnect must not assume that all
integer IDs from zero are still present.

`run.resync_required` is intentionally sent without an SSE `id`; it is a
control notification and must not advance the run cursor. Keepalive comments
also have no ID.

The current active-run buffer defaults are 2,048 events or 1 MiB, whichever
limit is reached first. These are implementation bounds, not durable history
limits. Terminal runs are retained in a separate bounded cache (currently 64
runs, up to 10 minutes by default).

### 3.2 Persisted projection notifications

The frozen logical event names and the current wire names are:

| Logical event | Current wire name | Required payload | Meaning |
| --- | --- | --- | --- |
| `item.created` | `item.appended` | `session_id`, optional `run_id`, `turn_id`, `seq`, `revision`, `item_id`, `item` | A new visible user-facing durable item is in the projection. |
| `item.updated` | `item.updated` | `session_id`, optional `run_id`, `turn_id`, `seq`, `revision`, `item_id`, `item` | An existing visible user-facing durable item changed. |

The current Go implementation emits `item.appended`, because that is also the
current storage record name. `item.created` is the frozen logical/canonical
name for a future compatible naming step; stage 2 does **not** rename the
emitted event or dual-emit it.

During migration:

- consumers MUST accept both `item.created` and `item.appended` and process
  them with the same semantics;
- a producer MUST NOT make a consumer require `item.created` until the legacy
  name has been retired by an explicit later migration; and
- a producer that sends both names for one record would cause duplicate work,
  so dual emission is not part of this contract unless a future version adds a
  stable event identity and an explicit deduplication rule.

Stage 2 notifications contain the committed, frontend-safe item projection so
the live stream can render the durable result without reconstructing it from a
request or transient model event. A representative creation notification is:

```json
{
  "type": "item.appended",
  "session_id": "session-1",
  "run_id": "run-1",
  "turn_id": "turn-1",
  "seq": 12,
  "revision": "19",
  "item_id": "msg-000001",
  "item": {
    "seq": 12,
    "id": "msg-000001",
    "turn_id": "turn-1",
    "created_at": "2025-01-01T00:00:00Z",
    "kind": "message",
    "visibility": "visible",
    "audience": "user",
    "message": {
      "role": "user",
      "content": {"inline": "hello"}
    }
  }
}
```

For an update, only the durable record sequence in the envelope advances; the
item's creation sequence remains unchanged:

```json
{
  "type": "item.updated",
  "session_id": "session-1",
  "run_id": "run-1",
  "turn_id": "turn-1",
  "seq": 17,
  "revision": "19",
  "item_id": "msg-000001",
  "item": {
    "seq": 12,
    "id": "msg-000001",
    "turn_id": "turn-1",
    "kind": "message",
    "visibility": "visible",
    "audience": "model",
    "status": "completed",
    "message": {
      "role": "assistant",
      "content": {"inline": "updated answer"}
    }
  }
}
```

`revision` is the session watermark after the committed transaction. When a
transaction writes several records, their visible item notifications may all
have the same revision. The envelope `seq` is still the causing durable record
sequence, while `item.seq` remains the creation sequence. `run_id` is omitted
when the bridge does not have a reliable run context; `turn_id` uses the
projected item's turn and falls back to the current committed turn context
only for legacy untagged items.

Only items passing the user-facing projection filter enter this stream:
visible `message` records with a user-audience user message, or a model-
audience assistant/tool message, plus visible user-audience compaction
dividers. Hidden, debug, internal/model-only runtime records, provider-private
messages, model-audience compaction summaries, and other non-message records
are skipped. Skipping a record does not renumber `revision`; it may therefore
jump. Missing transport notifications are detected by the run SSE event ID,
not by assuming consecutive item or revision values.

The `item` object uses the same safe `SessionItem` DTO as the snapshot/items
API. It contains display text or a bounded preview, image reference metadata
(hash/media type/size) rather than image bytes, and filtered tool arguments.
It never contains stored blobs, image data URLs, hidden reasoning, provider
request/response data, or other model-only private fields. The DTO is read
from the committed item projection after the durable event is observed; it is
not assembled from transient request parameters.

Other persisted stream names currently include `compaction.created` and
`active_history.replaced`. They can change the durable projection and therefore
also require a snapshot refresh, but this stage does not assign them a new
client-side optimistic behavior.

### 3.3 `run.resync_required`

On the per-run stream, the current payload is:

```json
{
  "type": "run.resync_required",
  "run_id": "run-1",
  "session_id": "session-1",
  "oldest_seq": 37,
  "oldest_stream_event_id": 37,
  "required_revision": "42"
}
```

It means the requested `after` cursor is older than the oldest event retained
in the run registry. It does **not** mean that durable session data is lost.
The client MUST load the session snapshot (and any needed item pages), then
reattach to the run stream using a current cursor if it still needs live
updates. `oldest_stream_event_id` is the unambiguous name for the oldest
retained per-run SSE ID. `oldest_seq` remains as a compatibility alias for
that same value; it is not a session revision or an item sequence.

When the registry can read the session store, `required_revision` is the
decimal-string session `LastSeq` observed while creating the resync control
frame. It is a repair hint for the snapshot watermark, not a promise that no
new durable records will be written after the frame. The field is omitted when
the adapter has no session service or cannot reliably read the session; clients
must not treat omission as revision zero. The server sets its internal replay
position to `oldest_stream_event_id - 1` and then sends every retained event,
including the control frame's following events; clients should not treat the
control frame as an ordinary run event ID.

The current `oldest_seq` name is a run-buffer ID, despite the name `seq`; it is
not a session revision or an item sequence. `session_id` is present on this
control event. Additive fields are allowed, but existing meanings cannot be
reused.

### 3.4 `run.settled`

The per-run terminal payload currently has these fields:

```json
{
  "type": "run.settled",
  "run_id": "run-1",
  "status": "committed",
  "turn_id": "turn-1",
  "last_seq": 42,
  "committed_revision": "42"
}
```

`message` is added for cancellation/failure (`"run cancelled"` or
`"run failed"`). `status` is the execution status, such as `committed`,
`failed`, or `cancelled`. `last_seq` is retained as the numeric compatibility
field, and `committed_revision` is the precision-safe decimal-string form of
that same final durable session watermark. When the session can be read, both
come from its current `LastSeq`; only a session read failure falls back to the
run result's `LastSeq`. Neither is the per-run SSE ID. The current per-run
payload does not include `session_id`; the endpoint and active-run descriptor
already bind the run to its session. A future optional `session_id` field may
be added, but clients must not require it today.

When the coordinator settles a run, the registry appends this event and then
compacts the terminal replay buffer to the last event (normally this
`run.settled`). Thus a late terminal replay can contain only `run.settled` and
can legitimately begin with a resync control frame if the requested cursor is
behind its original ID. The terminal event keeps its original run SSE ID.
Receiving `run.settled` is the signal to refresh durable session data; it is not
permission to construct the final history locally.

The process-wide `/api/events` lifecycle `run.settled` has the same logical run
completion but a different envelope. It currently includes compatibility
aliases `run`/`run_id` and `session`/`session_id`, plus `status`, `last_seq`,
`committed_revision`, and,
when available, `metadata`/`session_metadata` and `message`. Lifecycle frames
use an SSE `event: run.settled` line and do not use the per-run `id` cursor.

### 3.5 Transient run events

The per-run stream may also contain `turn.started`, text/reasoning deltas,
tool activity, usage/retry notifications, prompt queue notifications,
`turn.committed`, and `turn.failed`. These are live rendering signals. They
may be trimmed, coalesced, or absent after terminal compaction. The durable
items and the session snapshot win over any transient rendering state.

## 4. Replay and ordering rules

The following behavior is frozen by the current registry and characterization
tests:

1. `?after=0` means replay from the beginning of the **retained** run buffer.
   An omitted, malformed, or negative `after` is treated as zero (negative
   values are clamped to zero).
2. The cursor is exclusive: an event with ID `N` is replayed only when
   `after < N`. Supplying `after=oldest_seq - 1` replays the oldest retained
   event without resync. Supplying an already-consumed terminal ID returns an
   empty terminal replay.
3. If `after < oldest_retained_id - 1`, the server sends one
   `run.resync_required` frame without an ID and then the retained events. A
   buffer overflow therefore results in resync plus a durable snapshot refresh,
   not in fabricated missing events.
4. The shared coordinator first reserves the run handle and invokes its
   admission callback. The Web replay registry registers `managedRun` and its
   buffer in that callback, before the execution starter is invoked. Therefore
   no actual run event can be observed before the run is registered by
   `run_id`, including synchronous starter emissions and agent/session-tool
   starts. A run event can still be observed before the coordinator's `Start`
   call returns. Consumers must be able to connect after the HTTP 202 response
   and replay from zero; they must not assume that the first `run.started`
   arrives after the response.
5. Each connection has its own `after` cursor. Multiple connections replay the
   same retained event with the same run-local IDs; connecting twice does not
   renumber or consume events.
6. A live connection waits for new events and may receive a comment keepalive.
   A terminal connection ends after replaying its retained events. The stream
   is not a durable subscription and lifecycle hub delivery is best effort.
7. `run.settled` is terminal even if the run failed or was cancelled. A client
   must stop treating that run as live after processing it, then reconcile the
   durable projection.

The client stream reader must advance its `after` cursor only from a numeric
SSE `id` on a replayable event. It must not advance the cursor for
`run.resync_required`, comments, or an `event:` line from `/api/events`.

## 5. Commands and acceptance

“Accepted” describes admission/queueing, not durable completion. The current
wire does **not** emit a separate `command.accepted` SSE event: HTTP 202 and
its response body are the acceptance acknowledgment. A later stage may add an
explicit event only as an additive, separately versioned contract; clients
must not wait for a nonexistent `command.accepted` frame today.

| Command | Current response | Acceptance meaning |
| --- | --- | --- |
| `POST /api/sessions/{id}/runs` | HTTP 202; `run_id`, `session_id`, `status: "running"` | The run was admitted to the shared coordinator, and its registry/replay buffer is already queryable by `run_id`. Execution and durable item creation happen asynchronously. |
| `POST /api/sessions/{id}/continue` | HTTP 202; same shape | The interrupted/failed context was admitted to the shared coordinator, and its registry/replay buffer is already queryable by `run_id`; it does not append new content. |
| `POST /api/runs/{runID}/prompts` | HTTP 202; `run_id`, `status: "accepted"` | The prompt was accepted by the active run queue. It may be injected at a safe checkpoint or delivered in a follow-up turn. |
| `DELETE /api/runs/{runID}` | HTTP 202; `run_id`, current `status` | Cancellation was requested/applied to the run handle; durable settlement still arrives asynchronously. |

Validation errors, capacity errors, a settled run, and missing resources are
not acceptance and use the existing error responses. A successful 202 does not
carry a user-item ID or a new session revision.

## 6. No optimistic user-message update

A browser MUST NOT insert a submitted user message into canonical
`history.items`, advance `session.last_seq`/`revision`, or synthesize an
`item.created`/`item.appended` notification merely because a command returned
202. The user item becomes authoritative only through the persisted projection
notification applied to the projection store (with a snapshot/items refresh
when required), or through a snapshot that already includes it. The same rule
applies to a prompt accepted into an active run: a queued prompt is not yet a
durable history item.

The composer owns only draft and submission state. On successful admission it
clears the draft; while the command is in flight it disables submission, and on
failure it keeps the draft so the command can be retried. Submitted text and
images are not copied into `ActiveRun`, a conversation row, or any other
history-like projection. In particular, an active-prompt path may render the
server's `run.prompt_queue`, but a queued prompt is not a local user message.
The user bubble appears only after the committed item event is applied to the
App-owned projection store.

On `run.settled`, `turn.committed`, `item.created`/legacy `item.appended`, or
`item.updated`, clients may schedule a refresh. On `run.resync_required`, a
refresh is mandatory before claiming that the local projection is current.
Refreshes must be revision-aware and must not let an older asynchronous response
overwrite newer history.

### 6.1 Stage 3 frontend consumption rules

The web client keeps one normalized session projection store owned by `App`;
the history hook receives that store rather than creating a second store. A
committed item event is dispatched to that store for the event's `session_id`.
For a cached history window, `item.appended` and `item.created` upsert by the
projected `item.id` and insert new items by `item.seq`; `item.updated` replaces
the matching item in place. An update for an item outside the cached page is
held until pagination loads that item and is never inserted at the live page's
end. Normal item events do not trigger a full-page refresh.

The store compares the decimal `revision` and durable event `seq` as integers.
Revision is a session-wide watermark, not an item cursor: several item events
from one transaction can share it. The store therefore tracks per-item applied
record sequences and snapshot coverage separately. A snapshot is authoritative
for the returned history-page items at its revision, while older paged-in
items remain eligible for same-revision event upserts until an older-page
response establishes coverage for them. An equal-revision event already
observed by the client is retained so a racing older snapshot cannot roll it
back. Events
for sessions without a cached snapshot may advance known metadata revision and
are retained in a non-rendered pending queue keyed by durable record sequence.
The first snapshot establishes the history base when its revision covers any
pending overflow; pending events newer than that snapshot are replayed in
sequence, while covered events are dropped. An older response after overflow
does not establish a base and remains non-rendered until resync.
The pending queue is bounded separately from the history LRU: background
sessions and each session's records have fixed caps, and the oldest pending
queues may be evicted because selecting the session later performs a fresh
snapshot. Snapshot-in-flight sessions retain only a revision watermark if
their bounded queue is evicted; an older initial response then triggers a
second authoritative snapshot rather than exposing an incomplete history.
The queue and its event payloads remain bounded even when many snapshots are
in flight. The queue does not create a partial history that looks complete,
and `clear` removes it with the session. When loading an older page, the client
captures the entry revision before the request; the response covers at least
that revision, so a replay at the same revision cannot roll the fetched item
back, while a higher-revision event still wins. Active-run state remains a
transient view of assistant deltas, reasoning, tools, and process status; the
full item DTO is applied to the shared store, while only its explicit
assistant `item.id`/durable-length ownership metadata is copied into the run.
Conversation identity is the backend `item.id`, not message text, content, or
turn matching.

### 6.2 Stage 4 frontend submit and stream orchestration

For a new composer submission, the frontend follows this order:

1. Treat the composer action as a submit command and keep the text/images in
   draft state while the request is in flight. Do not create a user item or an
   optimistic `ActiveRun` message row.
2. Await `POST /api/sessions/{id}/runs` (or the continue command) and its
   admitted response. HTTP 202 is the admission boundary; it is not durable
   item completion.
3. Read and validate the authoritative `run_id` (and the returned
   `session_id`) from that response.
4. Only after that response is available, call
   `connectRunStream(run_id, session_id)`. The first connection is to the
   run-specific replay stream, not a guessed URL, a session stream, or a
   lifecycle event. The App permits at most one *active* connection per run;
   after a reader failure, a later authoritative recovery may retry it. The
   run stream itself owns its run-event replay cursor and reconnect behavior.
5. The registered replay supplies early `run.started` and item events even if
   execution began immediately after admission. `run.started` creates or
   updates the transient `ActiveRun`; the admission response itself does not
   synthesize one. Item events (`item.appended`/`item.created`/`item.updated`) are
   applied directly to the App-owned session projection store, including when no
   `ActiveRun` exists yet. Render a submitted user message only from the
   committed item event, using its
   backend `item.id`; never fill a stream gap with an optimistic row or a
   text/turn deduplication guess.
6. Keep only the uncheckpointed assistant tail plus transient tool/process UI
   in `ActiveRun` for this
   stage. When the bound durable assistant item is loaded in the current page,
   the conversation row attaches that tail to the durable message by its
   explicit `item_id` and suppresses the assistant portion of the active
   process row; if it is not loaded yet, the active row is a temporary
   fallback. `run.prompt_appended` may add an anonymous transient process
   boundary so transient process output is not folded across a drained prompt;
   the before/after segments remain internally ordered. It never creates or
   renders a user item. A committed durable user item still belongs to the
   historical rows; this stage does not claim to place transient process
   segments precisely on both sides of that historical bubble. On settlement,
   retain the existing reconcile behavior and let an authoritative snapshot
   complete the durable projection. Durable assistant item events are applied
   to the shared projection store and explicitly update transient item
   ownership, never by text matching. Settlement/reconcile behavior remains
   unchanged for this stage.

`revision` in a snapshot or projection event is a session-wide snapshot and
completeness watermark. It is compared numerically and says which durable
projection state the snapshot/event covers; it is not a cursor for the run
stream. Replay order and gap detection continue to use the per-run SSE event
ID (and the backend's `after` replay mechanism), together with the run ID.
Neither `revision`, item `seq`, `turn_id`, nor optimistic UI may be used to
invent missing run events or to make the first stream connection before
`run_id` admission.

## 7. Migration compatibility checklist

The following aliases and differences are intentionally frozen for migration:

| Area | Current compatibility field/name | Rule for new code |
| --- | --- | --- |
| Snapshot watermark | `revision` string; `session.last_seq` numeric | Prefer `revision`; accept/use `last_seq` when talking to older endpoints, comparing numerically. |
| Item creation event | `item.appended` | Accept both `item.created` and `item.appended`; do not dual-emit without a later dedupe contract. |
| Item event body/cursor | `item` plus payload `seq` | `item` is the committed safe DTO; treat top-level `seq` as the causing durable record sequence and `item.seq` as creation sequence. |
| Session DTO watermark | `revision` string plus `last_seq` number | Prefer the decimal string; retain/read `last_seq` for older clients. |
| Run settlement watermark | `committed_revision` string plus `last_seq` number | Prefer the decimal string; retain/read `last_seq` for older clients. |
| Lifecycle run identity | `run` and `run_id`; `session` and `session_id` | Read either alias; new payloads should retain both until a migration says otherwise. |
| Lifecycle metadata | `metadata` and `session_metadata` | Read either alias; it is a point-in-time metadata copy, not a replacement for the snapshot. |
| Per-run terminal identity | `run_id`, no current `session_id` | Bind through the run endpoint/descriptor; treat a later optional `session_id` as additive. |
| Run cursor | per-run SSE `id` | Send as `after` only for the same `run_id`; never compare it to `revision` or item `seq`. |

Stage 2 does not rename storage records, change the HTTP status codes, add
optimistic history mutation, or add a second item event for the same record.
The frontend type declarations accept the logical `item.created` alias, but
the backend emits only the legacy-compatible `item.appended` wire name. Any
later protocol change must update this document and its characterization tests
in the same change.
