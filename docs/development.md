# Development

`sai` is a local agent client backed by an in-process Go library, with a
command-line interface. The long-term product surface is a Web UI rebuilt on
the same storage foundation (see [native-app.md](native-app.md) for the
design direction); the retired Web stack is preserved under the `webui` git
tag.

## Product boundary

- Running the executable with no arguments starts the CLI REPL.
- Sessions, provider credentials, and configuration stay under the server
  root (default: the OS user config directory under the app name).
- The single tool is the shell: Bash on every platform, Git Bash on Windows.
- The executable is a single-file runtime artifact. There is no daemon,
  registry, multi-user account system, or second chat implementation.

## Architecture

```
cmd/sai              CLI: JSONL record stream on stdout, spinner/control on stderr
internal/chatapp     orchestration: config + provider + tools + session runs
internal/sessionlog  append-only JSONL store under data/<project>/<session>/
internal/agent       transport-neutral agent loop (streaming, retries, tool calls)
internal/model       provider adapters: openai_chat, openai_responses,
                     anthropic_messages, httpstream
internal/tools       the shell tool
internal/config      root + provider YAML configuration
internal/eventbus    the durable-event contract between agent and sessionlog
internal/codexauth   Codex device login
```

Data flow: `agent.Stream` publishes conversation content through
`eventbus.Publisher`; the sessionlog publisher appends each event as one
JSONL record; the fold rebuilds the model request context; the CLI tails the
log and prints records as they are appended.

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
