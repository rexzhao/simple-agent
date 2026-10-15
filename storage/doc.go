// Package storage is the session persistence layer: the record
// schema, the fold that replays records into in-memory state, and the
// Store/Session interfaces with two implementations (jsonl, memory).
//
// # Public contract (v0)
//
// This package being public makes the on-disk format a contract:
//
//   - log.jsonl: one JSON record per line, append-only; a torn tail
//     (a partial last line) is recovered by discarding it on open.
//     A mid-file invalid line (external corruption or a manual edit)
//     is treated the same way — fail-safe over fail-stop: the file is
//     truncated FROM the first invalid line (the data after it is
//     lost), and the store reports the discarded bytes through its
//     logger.
//   - Segments: a compaction switches segments — the next segment is
//     staged in log.jsonl.next (fully written and synced), the active
//     log is archived, then the staged file is activated. Every
//     interruption point converges to "the compaction never took
//     effect" or "it fully did".
//   - The digest chain (sai/v2/): NOT a storage mechanism — the
//     serve host (internal/web) computes this per-segment consistency
//     digest over the records (h₀ = SHA256("sai/v2/" + sessionID),
//     length-prefixed field tuples, segments re-genesis from the
//     compaction's item id; see webui.md §4.1). Storage guarantees
//     the record format, the segment switching, and the ItemID
//     stability the chain builds on: changing a record field that
//     enters the digest tuple must be handled together with a chain
//     version bump.
//   - Locking (the jsonl implementation): one writer per session,
//     enforced by a lock file next to the log (flock / LockFileEx);
//     a second open fails with jsonl.LockHeldError.
//   - Archiving (§17.3): a marker file <root>/<ns>/<id>/archived parks
//     a session out of the working set — the directory tree, the path
//     invariant, and the lock file all stay put. Archive/Unarchive
//     take the session's own OS lock (an external holder refuses with
//     jsonl.LockHeldError); Open checks the marker INSIDE the same
//     lock and refuses with ErrArchived; both operations are
//     idempotent. Delete ignores the marker: archive ≠ delete, a
//     parked session deletes like any other.
//
// Compatibility: v0 — NO stability guarantees. Breaking changes land
// without notice until v1 is declared. If you embed this package, pin
// an exact commit.
package storage
