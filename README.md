# simple-agent

`sai` is a local agent application packaged as a single Go executable with a
command-line interface. Type a message, the agent answers; it can read and
edit files in your project — all through its single tool: a Bash shell.

While a run streams, the reply renders character by character as the model
generates it (reasoning lines marked with `·`); a dot spinner marks only
the quiet phases — waiting for the first token and running tools.
Committed records render as human-readable blocks — a common header line
(local time and record kind, e.g. `[10:06:21] tool shell · ok`) followed
by the content; an assistant message already streamed live is not printed
again. Control messages go to stderr.

## Storage model

Every durable fact about a session is one line in an append-only JSONL log:

```
<server-root>/data/<project>/<session-id>/log.jsonl
```

A project is the working directory the session runs in, identified by a
deterministic hash of its path, so sessions group by workspace with no
registration step.

There is no update and no delete. Session state is a fold of the log,
rebuilt on open and advanced incrementally as records are appended. See
[docs/design.md](docs/design.md) for the record schema, the identity
system, and the invariants.

## Run

```powershell
go run ./cmd/sai [session-id]
```

Options:

```text
--server-root dir   configuration namespace root (default: user config dir/<app>)
--session id        resume a stored session by id (exit if not found)
--provider name     override default_provider
--model name        override default_model (model profile name)
--version           print version
```

An optional positional argument resumes a stored session. Slash commands:

- `/clear` — start a new session (empty sessions are never written to disk)
- `/compact` — summarize the context and continue from the summary; the
  conversation before the compaction is replaced by it
- `/resume <id>` — reopen a stored session (history prints as the last
  20 records; the full log stays in the file)
- `/stop` — stop the active run (Ctrl+C does the same)
- `/quit` — exit (a second Ctrl+C when idle also exits)

The working directory is the process cwd: the shell tool runs where you
started `sai`. The session id prints at startup, and again at exit when the
session was persisted, so it can be resumed later with `--session`.

## Server root and configuration

`--server-root` selects one namespace for configuration, provider
credentials, and sessions. Resolution order:

1. the `--server-root` flag
2. the `SAI_SERVER_ROOT` environment variable
3. the OS user configuration directory under `<app>` (the default)

`<app>` derives from the executable's base name: everything before the first
`-`. `sai.exe` and `sai-windows-amd64.exe` both use `sai`; renaming the
executable to `myagent-v2.exe` switches it to its own `myagent` namespace.

The root config is `<server-root>/sai.yaml`:

```yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

agent:
  max_iterations: 8
```

Provider profile:

```yaml
# <server-root>/providers/paperhub.yaml
name: paperhub
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY

models:
  glm-5.2:
    id: glm-5.2
    type: openai-chat
    context_window: 128000
    output_limit: 4096
    parameters:
      temperature: 0.6
```

Relative paths in the root configuration resolve from the server root. API
keys can reference environment variables and are resolved only by the Go
process. See [docs/configuration.md](docs/configuration.md) for the supported
schema.

## Build

Building requires Go 1.22+.

Windows:

```powershell
powershell -ExecutionPolicy Bypass -File scripts/build.ps1
```

Linux or macOS:

```sh
sh scripts/build.sh
```

The build cross-compiles:

- `dist/sai-windows-amd64.exe` (with `dist/sai.exe` linked to it)
- `dist/sai-linux-amd64`
- `dist/sai-darwin-arm64`

Pushing a `v*` tag runs the release workflow: it validates the backend, builds
all three binaries, and publishes them with a `SHA256SUMS` manifest.

Go validation:

```powershell
go test ./...
```

## Project layout

The refactor baseline is [docs/design.md](docs/design.md); the layout below
is the target structure (see the design doc for invariants and the record
schema):

```
cmd/sai              command-line interface (human-readable record blocks on stdout)
internal/agent       state layer + agent loop (owns State, commit is the only write path)
internal/storage     record schema + Fold + Store/Session interfaces
  jsonl/             append-only JSONL file store (torn-tail recovery)
  memory/            in-memory store for tests and ephemeral sessions
internal/model       provider adapters (OpenAI Chat/Responses, Anthropic)
internal/tools       the shell tool (Bash on every platform, incl. Windows via Git Bash)
internal/config      startup-time config parsing (cmd-only dependency)
```
