# Session Compaction Task Checklist

This checklist tracks the remaining implementation slices for
`docs/session-compaction.md`. It is intentionally smaller than the design doc and
does not advance `docs/checklist.md` or `docs/milestones.md` status.

Known baseline: low-level v2 session store primitives were committed in `1613b4f`.

Out of scope: the future session-history query tool.

## Implementation Slices

- [x] Config and CLI plumbing: add `compaction.enabled`, `threshold_percent`,
  `summary_provider`, and `summary_model` with documented defaults and validation.
- [ ] V2 session runtime: persist successful turns as append-only `Items` plus
  `ActiveHistory`, while keeping `sessions.save_tool_results: true` required for
  reliable save/resume/compaction.
- [ ] Resume path: materialize model messages only from `ActiveHistory`, use saved
  runtime metadata, and report corrupted sessions for invalid refs or illegal tool
  history.
- [ ] Manual `/compact`: support the command only in normal single-line REPL mode,
  perform compaction without starting a user turn, and leave state unchanged on
  failure.
- [ ] Summary lifecycle: resolve the summary provider/model, send summary requests
  without tools or an agent loop, check context limits, and handle oversized
  summary input by trimming only summary input.
- [ ] Atomic checkpoint write: append the hidden model-facing summary item, append
  compaction checkpoint metadata, append `active_history.replaced`, flush, then
  update in-memory `ActiveHistory`.
- [ ] Replacement history selection: keep saved instruction/runtime items, recent
  complete visible turns, and the summary item; never keep half of a tool-call
  exchange.
- [ ] Pre-turn auto compact: estimate `ActiveHistory + pending user message + tool
  schemas` before saving the pending user message; on compact failure, fail the
  turn without requesting the main model.
- [ ] Privacy and views: keep old visible `Items` available for future server/GUI
  pagination, hide compaction summaries from the default chat view, and preserve
  blob reachability rules.

## Acceptance Points

- [ ] Manual compact preserves visible history and replaces only model-facing
  `ActiveHistory`.
- [ ] Auto compact inserts the new user message after the summary only after
  compaction succeeds.
- [ ] Resume after compact sends only materialized `ActiveHistory` to the provider.
- [ ] Compaction failures are atomic: no summary item, checkpoint, pending user
  item, or active history replacement is persisted.
- [ ] Tests cover config defaults, manual `/compact`, pre-turn auto compact,
  replacement history legality, resume materialization, and failure paths.
- [ ] `go test ./...` and `git diff --check` pass for each implementation slice.
