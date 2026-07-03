# M20 Final Integration Smoke Evidence

Date: 2026-07-03

Main agent ran a real-binary local smoke on this branch with a temporary fake
OpenAI-compatible provider. The smoke built `cmd/sai` into a temporary
`sai.exe`, created a temporary project containing `.agents/sai.yaml` and a fake
provider profile, then removed the temporary directory after the run.

Commands exercised:

- `sai server --background --cwd <project>`
- `sai --cwd <project> status`
- `sai --cwd <project> attach --new` with stdin `attach prompt`, then `/quit`
- `sai --cwd <project> send --new --prompt "send prompt"`
- `sai --cwd <project> sessions list`
- `sai --cwd <project> stop`

Observed evidence:

- Server startup printed `SERVER_ADDR`.
- Status output included the temporary project, config, and server address.
- Attach streamed `smoke response`.
- Send returned `STATUS committed` and `TURN_ID turn-000001`.
- Sessions list contained both created sessions.
- Stop returned `SERVER_STOPPED`.
- Before stop, `.agents/sessions/keep.txt`, `.agents/logs/keep.txt`, and
  `.agents/blobs/keep.txt` existed with content `keep`; after stop, all three
  files still existed with the same content.
- No browser Web GUI UI was implemented or required for M20.
- After the latest code/test slice, `go test ./...` and `git diff --check`
  passed; diff check emitted only CRLF conversion warnings.
