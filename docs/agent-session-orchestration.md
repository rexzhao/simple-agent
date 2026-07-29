# Agent session orchestration

SAI exposes sessions to an agent as durable, named execution resources. A
session is the long-lived history and configuration snapshot; its active run is
closer to a process; a turn is one command sent to that process. The session
tools query and manage those resources without giving the agent direct access
to session storage.

## Enable the tools

Session tools are capabilities, not implicit agent powers. Enable only the
operations a model should be allowed to call:

```yaml
tools:
  enabled:
    - session_models
    - session_start
    - session_search
    - session_get
    - session_history
    - session_send
    - session_wait
    - session_stop
```

All operations are scoped to the caller's project. A session ID from another
project produces `session_forbidden`, and searches never enumerate another
project. Provider credentials, authorization data, and raw configuration are
never included in tool results.

## Tool summary

| Tool | Purpose | Important limits |
| --- | --- | --- |
| `session_models` | List configured provider/model profiles that `session_start` can select. | No arguments; secrets are omitted. |
| `session_start` | Create a leaf-worker child session and asynchronously start its first turn. | Name: 120 Unicode characters; the child cannot start or send to sessions. |
| `session_search` | Match canonical session names with a Go RE2 regular expression. | Default 20 results; maximum 100. |
| `session_get` | Read session state and its latest relevant persisted assistant item. | Default output limit 64 Ki characters; maximum 256 Ki. |
| `session_history` | Read a page of persisted, user-visible conversation items. | Default 50 items; maximum 200. |
| `session_send` | Strictly steer the current turn or queue an independent next turn. | `mode` is `steer` or `queue`. |
| `session_wait` | Wait for the run active when the call begins, then inspect the session. | Default and maximum timeout: 30 seconds. |
| `session_stop` | Request cancellation of a session's active run. | Self-stop is rejected; the durable session is not deleted. |

JSON Schema validates normal model tool calls. The executor repeats required
type, enum, and numeric range checks at its own boundary, so non-model callers
cannot bypass the contract.

## Starting sessions and model selection

A minimal start inherits the caller's exact frozen runtime snapshot:

```json
{
  "prompt": "Review the database migration and report risks.",
  "name": "Migration review"
}
```

When `provider`, `model`, and `reasoning_level` are all omitted, the child
inherits the caller's provider, model ID, model parameters, MCP servers, skills,
reasoning visibility, context-window limit, working directory, and enabled
tools except `session_start` and `session_send`. It starts with fresh history
and fresh token usage.

To use another configured model, first call `session_models`, then provide both
profile selectors:

```json
{
  "prompt": "Find a minimal reproducer.",
  "name": "Reproducer",
  "provider": "paperhub",
  "model": "glm-5.2",
  "reasoning_level": "high"
}
```

`provider` and `model` must appear together. Supplying only `reasoning_level`
uses the caller's provider/profile names but resolves that profile from the
current server-root configuration; it is therefore an explicit configured
selection rather than exact snapshot inheritance.

Every tool-created session records:

- `parent_session_id`: the calling session;
- `root_session_id`: the root of the lineage;
- `spawn_depth`: root is 0 and a normal tool-created leaf child is 1;
- `created_by: "agent"`.

Agent-created children are deliberately leaf workers. Both inherited-model and
explicit-model children have `session_start` and `session_send` removed from
their frozen capability snapshot. The runtime repeats this filtering for child
sessions saved by older releases, so stale metadata cannot restore either
tool. The existing maximum-depth check remains as defense for legacy data and
non-tool service callers.

The result returns `session_id` and `run_id` immediately; completion is observed
with `session_get` or `session_wait`. If durable session creation succeeds but
run admission fails (for example, global run capacity is full), the error
result includes `created: true` and the recoverable `session_id`. The session is
kept rather than destructively rolled back.

## Searching and reading output

`session_search` applies `name_regex` to the display name, or to the generated
`Session <suffix>` fallback when no display name exists:

```json
{
  "name_regex": "(?i)review|audit",
  "statuses": ["running", "idle", "interrupted"],
  "include_archived": false,
  "limit": 20
}
```

`session_get` returns an `inspection` object containing session metadata and an
optional `output`. Output selection is based only on durable assistant message
items:

- `running`: newest persisted assistant item in the current turn, with
  `kind: "intermediate"` and `complete: false`;
- `idle`: newest persisted assistant item, with `kind: "final"` and
  `complete: true`;
- `interrupted`: newest persisted assistant item in the interrupted turn, with
  `kind: "partial"` and `complete: false`.

If the relevant turn has no persisted assistant item yet, `output` is absent or
`null`; an older turn is not presented as current progress. Streaming text or
reasoning deltas, in-memory buffers, tool results, and hidden items never count.
This ensures an agent sees only state that can be recovered after a restart.
`max_output_chars` truncates by Unicode characters and marks the output with
`truncated: true`.

`session_history` accepts a durable `session_id` and returns the same visible
conversation items used by the Web history API. The newest page is returned by
default. Pass `cursor: oldest_seq` with `direction: "before"` to page backward
or `cursor: newest_seq` with `direction: "after"` to read newer items;
`direction` is required whenever `cursor` is present. The retired
`before_seq`/`after_seq` parameters remain accepted for compatibility with
agents started before this contract, but are no longer advertised in the tool
schema; conflict errors point at the cursor/direction contract.
Resolve names with `session_search` first because display names are not unique.
Hidden items and sessions outside the caller's project are never returned.

## `steer` versus `queue`

`session_send` deliberately exposes two different concurrency semantics.

### Strict `steer`

```json
{
  "session_id": "session-id",
  "mode": "steer",
  "message": "Prioritize correctness over compatibility."
}
```

Steer targets only the currently active turn. It succeeds only while that
turn's steer gate is open. If the run is idle, the turn has completed, or the
gate closes during the call, it returns `session_not_steerable`. It never falls
back to a queued turn. This strict behavior is specific to the agent tool; the
Web append endpoint retains its existing no-loss append/queue behavior.

### `queue`

```json
{
  "session_id": "session-id",
  "mode": "queue",
  "message": "After that, write the regression tests."
}
```

Queue creates an independent next turn. With an active run it enters that run's
FIFO turn queue and returns `delivery: "queued"`; with an idle session it starts
a run immediately and returns `delivery: "started"`. Admission races are
retried internally, and an unresolved race returns `session_busy` instead of
silently dropping the message.

A queued turn belongs to the active process lifecycle: canceling or failing the
run also settles its accepted-but-unstarted queue entries. Once a queued turn
starts, its user and assistant items are persisted in the normal session
ledger.

Archived sessions can be inspected but cannot receive input.

## Waiting and stopping

`session_wait` captures the run that is active at invocation time. It does not
switch to a later run that happens to start while the tool is waiting. If the
session is already idle, it returns immediately with `completed: true`. On
timeout it returns `timed_out: true`, includes the latest persisted inspection,
and leaves the run active. A timeout of zero is a non-blocking observation.
Canceling the tool call itself returns `canceled` and also does not stop the
target.

`session_stop` requests cancellation and returns without waiting for complete
settlement. Calling it for an idle session is a successful idempotent no-op.
Use `session_wait` or `session_get` afterward when the caller needs the final
persisted partial state. A session cannot wait for or stop its own active run,
which prevents direct self-deadlock and self-cancellation through the toolset.

## Result and error contract

Success results are JSON objects with `ok: true`. Expected operation failures
are returned as tool errors with a machine-readable envelope rather than as
unstructured tool-execution failures:

```json
{
  "ok": false,
  "code": "session_not_steerable",
  "error": "session has no active turn accepting steer messages"
}
```

Stable error codes include:

| Code | Meaning |
| --- | --- |
| `invalid_arguments` | Missing, malformed, out-of-range, or unsupported input. |
| `session_not_found` | No durable session exists for the ID. |
| `session_forbidden` | The target belongs to another project. |
| `session_archived` | Input was attempted on an archived session. |
| `session_not_steerable` | No active turn currently accepts strict steer input. |
| `session_busy` | A run-boundary race prevented safe queue/start admission; retry. |
| `run_capacity_reached` | The application-wide active-run limit is full. |
| `spawn_depth_exceeded` | The caller is already at the maximum lineage depth. |
| `self_wait_forbidden` | The caller tried to wait for itself. |
| `self_stop_forbidden` | The caller tried to stop itself. |
| `coordinator_unavailable` | Shared run management is unavailable or closing. |
| `canceled` | The tool operation's context was canceled. |
| `session_operation_failed` | Storage, configuration, model selection, or another internal operation failed. |

Callers should branch on `code`, not on human-readable `error`. Internal errors
that do not have a safe specialized mapping use `session_operation_failed`.

## Web visibility

Agent-created sessions use the same global run coordinator as Web-created
runs. Their stream events, cancellation, active-run recovery, and replay are
therefore visible through the existing Web runtime. The workspace navigation
renders `parent_session_id` relationships as a tree, labels agent-created
nodes, allows every parent branch to collapse or expand its children, and
preserves orphaned nodes as roots if an older/missing parent cannot be loaded.
