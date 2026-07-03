# Session Compaction Task Checklist

This checklist tracks the remaining implementation slices for
`docs/session-compaction.md`. It is intentionally smaller than the design doc and
does not advance `docs/checklist.md` or `docs/milestones.md` status.

Known baseline: low-level v2 session store primitives were committed in `1613b4f`.

Out of scope for this closure: the future session-history query tool, all
GUI/server work, and standalone blob reachability acceptance beyond preserving
existing behavior.

## Implementation Slices

- [x] Config and CLI plumbing: add `compaction.enabled`, `threshold_percent`,
  `summary_provider`, and `summary_model` with documented defaults and validation.
- [x] V2 session runtime: persist successful turns as append-only `Items` plus
  `ActiveHistory`, while keeping `sessions.save_tool_results: true` required for
  reliable save/resume/compaction.
- [x] Resume path: materialize model messages only from `ActiveHistory`, use saved
  runtime metadata, and report corrupted sessions for invalid refs or illegal tool
  history.
- [x] Manual `/compact`: support the command only in normal single-line REPL mode,
  perform compaction without starting a user turn, and leave state unchanged on
  failure.
- [x] Summary lifecycle: resolve the summary provider/model, send summary requests
  without tools or an agent loop, check context limits, and handle oversized
  summary input by trimming only summary input.
- [x] Atomic checkpoint write: append the hidden model-facing summary item, append
  compaction checkpoint metadata, append `active_history.replaced`, flush, then
  update in-memory `ActiveHistory`.
- [x] Replacement history selection: keep saved instruction/runtime items, recent
  complete visible turns, and the summary item; never keep half of a tool-call
  exchange.
- [x] Pre-turn auto compact: estimate `ActiveHistory + pending user message + tool
  schemas` before saving the pending user message; on compact failure, fail the
  turn without requesting the main model.
- [x] CLI privacy/view closure: keep old visible `Items` available in storage
  across compact/resume, keep hidden compaction summaries out of default
  CLI/session metadata views, and avoid printing old visible message bodies by
  default.

## Acceptance Points

- [x] Manual compact preserves visible history and replaces only model-facing
  `ActiveHistory`.
- [x] Auto compact inserts the new user message after the summary only after
  compaction succeeds.
- [x] Resume after compact sends only materialized `ActiveHistory` to the provider.
- [x] `sai sessions show <id>` omits hidden compaction summary content and old
  visible sensitive message bodies by default.
- [x] Compaction failures are atomic: no summary item, checkpoint, pending user
  item, or active history replacement is persisted.
- [x] Tests cover config defaults, manual `/compact`, pre-turn auto compact,
  replacement history legality, resume materialization, and failure paths.
- [x] `go test ./...` and `git diff --check` pass for each implementation slice.
