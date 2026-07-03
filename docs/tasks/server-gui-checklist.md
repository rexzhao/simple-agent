# M20 Server-Owned Sessions and CLI Client Checklist

This is the active implementation checklist for M20. It splits
`docs/server-gui.md` into small server + CLI slices. Keep milestone-level status
in `docs/checklist.md`; update this file as implementation slices land.

M20 scope is server-owned sessions and CLI client behavior. Browser Web GUI UI is
future work and should not be required by any slice here.

## Scope Guardrails

- [ ] Treat `docs/server-gui.md`, `docs/milestones.md`, and `docs/checklist.md`
  as the behavior source for M20.
- [ ] Keep browser Web GUI UI, routing, frontend state management, and styling out
  of M20 implementation slices.
- [ ] Preserve API and WebSocket semantics needed by a future GUI without letting
  the CLI read session files, blob files, or `ActiveHistory` directly.
- [ ] Keep every slice small enough to validate independently with focused tests
  plus `git diff --check`.

## Implementation Slices

- [x] Server process foundation: add `sai server`, loopback HTTP listener,
  foreground blocking behavior, `GET /health`, `GET /server`, and
  `POST /server/shutdown`.
- [x] Foreground lifecycle: handle Ctrl+C by stopping listeners, flushing required
  metadata, removing registry entries, and exiting 0 when no running turn blocks
  shutdown.
- [ ] Background lifecycle: implement `sai server --background` so the parent waits
  for listen, registry write, and `/health` success before exiting 0, while the
  child detaches stdout/stderr from the caller terminal.
- [ ] Listen configuration: support default `127.0.0.1:0`, `--port N`,
  `--port 0`, and advanced loopback `--listen host:port`, with clear errors for
  occupied ports or unsupported addresses.
- [ ] Registry model: create the per-user registry with canonical cwd,
  config path, addr, pid, random token, started_at, version, current-user file
  permissions, duplicate-start handling, and stale-record cleanup.
- [x] Discovery model: implement upward discovery from current cwd or `--cwd`,
  health-check each candidate, ignore stale entries, and prefer the nearest
  healthy server.
- [x] CLI lifecycle commands: implement `sai status`, `sai stop`,
  `sai stop --cwd`, and `sai servers list` through registry discovery and server
  API calls.
- [x] Session metadata API: implement `GET /sessions`, `POST /sessions`, and
  `GET /sessions/{id}` with metadata-only responses and server-owned session
  creation.
- [x] Item pagination API: implement `GET /sessions/{id}/items` with
  `before_seq`, `after_seq`, `limit`, and `view=chat|debug`, including hidden
  compaction summary filtering in chat view.
- [x] Item content API: implement
  `GET /sessions/{id}/items/{item_id}/content` with token-gated non-public
  reads, offset/max byte support, session item reachability checks, and no bare
  blob hash endpoint.
- [ ] WebSocket stream: implement `WS /sessions/{id}/stream` with multi-client
  fanout for transient text deltas, tool status, persisted item events,
  compaction events, `turn.committed`, and `turn.failed`.
- [ ] Send message API: implement `POST /sessions/{id}/messages` so the server
  owns turn execution, rejects busy sessions with conflict, streams transient
  events, persists only successful turns, and leaves failed turns transient.
- [ ] Compact command API: implement `POST /sessions/{id}/commands/compact` for
  idle sessions, map attach REPL `/compact` to this endpoint, and keep multiline
  `/compact` as ordinary text.
- [ ] CLI attach REPL: make bare `sai` equivalent to `sai attach`, support
  `sai attach`, `sai attach <session-id>`, `sai attach --new`, streaming output,
  and shared-session observation through the server.
- [ ] CLI query/send commands: implement `sai sessions list`,
  `sai sessions show <id>`, `sai send <session-id> --prompt ...`, and
  `sai send --new --prompt ...` using only server APIs.
- [ ] Legacy entrypoint cleanup: remove or hide standalone in-process
  `sai chat` from the recommended product path and help text; keep any temporary
  compatibility path explicitly hidden or legacy.
- [x] Security and error shape: require registry token for writes, debug reads,
  blob content reads, and shutdown; return structured errors such as
  `session_busy`, `server_busy`, `permission_denied`, and `blob_not_found`
  without leaking prompt, response, tool result, blob content, or secrets.
- [ ] Final integration closure: verify CLI attach/send/status/stop flows against
  a local server, confirm stop does not delete sessions/logs/blobs, and keep the
  future Web GUI out of the implementation diff.

## Acceptance Criteria

- [ ] A user can run `sai server --cwd X`, discover it from `X` or a child
  directory, attach with bare `sai`, create or choose a session, send a message,
  receive streaming output, and stop the server cleanly.
- [ ] `sai server --background` returns only after the server is healthy and
  discoverable, and it does not keep the caller terminal attached to long-lived
  stdout/stderr.
- [ ] Registry duplicate-start, port conflict, upward discovery, stale cleanup,
  token usage, and stop semantics are covered by tests.
- [ ] Session metadata, paginated items, item content, send message, compact
  command, shutdown, and WebSocket fanout are covered by focused API tests.
- [ ] CLI tests cover `attach`, `status`, `stop`, `servers list`,
  `sessions list/show`, `send`, and the no-server startup hint.
- [ ] Failed turns remain transient, successful turns are persisted, and multiple
  clients observing the same running turn see consistent stream events.
- [ ] `view=chat` hides hidden compaction summaries, `view=debug` can expose debug
  metadata through token-gated server APIs, and bare blob hash reads are rejected.
- [ ] No implementation slice adds browser Web GUI UI.
- [ ] `go test ./...` passes before marking M20 complete.
- [ ] `git diff --check` passes before marking M20 complete.
