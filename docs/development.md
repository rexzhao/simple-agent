# Development

`sai` is a local agent client packaged as a single Go executable with a
command-line interface. The architecture baseline for the ongoing destructive
refactor is [design.md](design.md); that document is authoritative — when code
and design disagree, fix the design first, then the code.

## Product boundary

- Running the executable with no arguments starts the CLI REPL.
- Sessions, provider credentials, and configuration stay under the server
  root (default: the OS user config directory under the app name).
- The single tool is the shell: Bash on every platform, Git Bash on Windows.
- The executable is a single-file runtime artifact. There is no daemon,
  registry, multi-user account system, or second chat implementation.

## Architecture (target)

```
cmd/sai              startup: read config once → agent.New → REPL
internal/agent       state layer + agent loop (merges the old chatapp):
                     session owns State, commit is the only write path
internal/storage     record schema + Fold() + Store/Session interfaces
  jsonl/             file layout, ProjectID, torn-tail recovery
  memory/            in-memory map for tests and ephemeral sessions
model/               provider adapters: openai_chat, openai_responses,
                     anthropic_messages, httpstream — public (B2)
internal/tools       the shell tool
internal/config      startup-time config parsing (cmd-only dependency)
internal/codexauth   Codex device login (used by cmd/sai provider assembly)
```

Dependency rule: `agent` imports `{model, storage, tools}` and never
`config`. All agent parameters are injected at construction; nothing reads
configuration at runtime. See [design.md](design.md) for the record schema,
identity system, and invariants.

## Workflow

```sh
go build ./...     # compile everything
go test ./...      # run the suite
go vet ./...
sh scripts/build.sh    # cross-compile dist/ (PowerShell: scripts/build.ps1)
```

Configuration lives in `<server-root>/sai.yaml` with provider profiles in
`providers/`; see [configuration.md](configuration.md). Examples are under
`examples/`.
