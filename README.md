# simple-agent

`sai` is a local agent application packaged as a single Go executable with a
native UI. Start the executable and a window opens for chatting with a model
that can read, search, and edit files and run shell commands in your project.

The window never reads session files or provider credentials directly. The
Go process owns the execution library, storage, tools, and model connections;
the native window is only the presentation layer.

## Storage model

Every durable fact about a session is one line in an append-only JSONL log:

```
<server-root>/sessions/<session-id>/log.jsonl
```

There is no update and no delete. Session state is a fold of the log,
rebuilt on open and advanced incrementally as records are appended. See
[docs/native-app.md](docs/native-app.md) for the event vocabulary, the fsync
policy, and crash recovery.

## Run

```powershell
go run ./cmd/sai-native
```

Options:

```text
--server-root dir   configuration namespace root (default: user config dir/sai)
--cwd dir           working directory for tools (default: process cwd)
--provider name     override default_provider
--model name        override default_model (model profile name)
--full-access       let file tools read and write outside the cwd
--version           print version
```

The window shows the conversation (streaming assistant output included) and
takes input at the bottom. Slash commands:

- `/clear` — start a new session
- `/resume` — pick and reopen a stored session
- `/stop` — stop the active run
- `/help` — list commands

## Server root and configuration

`--server-root` selects one namespace for configuration, provider
credentials, and sessions. Resolution order:

1. the `--server-root` flag
2. the `SAI_SERVER_ROOT` environment variable
3. a `.sai` directory in the current working directory (when it contains
   `sai.yaml`), so a checkout can carry its own configuration
4. the OS user configuration directory under `sai` (the default)

The root config is `<server-root>/sai.yaml`:

```yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

agent:
  max_turns: 8

tools:
  enabled:
    - list_files
    - read_file
    - glob_files
    - grep_files
    - write_file
    - edit_file
    - apply_patch
    - shell
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

Building requires Go 1.27.1+ (the toolchain downloads automatically).

Windows:

```powershell
powershell -ExecutionPolicy Bypass -File scripts/build.ps1
```

Linux or macOS:

```sh
sh scripts/build.sh
```

The build cross-compiles:

- `dist/sai-native-windows-amd64.exe`
- `dist/sai-native-linux-amd64`
- `dist/sai-native-darwin-arm64`

Pushing a `v*` tag runs the release workflow: it validates the backend, builds
all three binaries, and publishes them with a `SHA256SUMS` manifest.

Go validation:

```powershell
go test ./...
```

## Project layout

```
cmd/sai-native       native UI (mygo window)
internal/sessionlog  append-only JSONL session store + fold
internal/chatapp     minimal orchestration (provider, tools, runs)
internal/agent       transport-neutral agent loop
internal/model       provider adapters (OpenAI Chat/Responses, Anthropic)
internal/tools       file and shell tools
internal/config      root + provider configuration
```
