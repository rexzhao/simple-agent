# simple-agent

`sai` is a local Web agent application packaged as a single executable. Start
the executable and it opens a browser UI for managing projects, sessions, model
streaming, tool activity, cancellation, and context compaction.

The browser never reads session files or provider credentials directly. A
loopback-only Go process owns the execution library, storage, tools, and model
connections; the embedded React application is only the presentation layer.

## Run

Double-click `sai-windows-amd64.exe`, or start it from a terminal:

```powershell
./dist/sai-windows-amd64.exe
```

The default behavior is to listen on a random `127.0.0.1` port and open the Web
application automatically. Runtime use does not require Node.js or a separate
Web server.

Launcher options:

```text
sai --listen 127.0.0.1:0
sai --server-root C:\path\to\sai-data
sai --cwd F:\work\project
sai --no-open
sai --version
sai --help
```

The former interactive CLI, TUI, mailbox server, and project/session CLI
commands are not part of the product anymore.

## Server root and project setup

`--server-root` selects one namespace for configuration, Provider credentials,
optional diagnostic logs, projects, sessions, and blobs. It defaults to the OS
user configuration directory under the executable basename. The root config is
`<server-root>/<basename>.yaml`; the launcher ensures its core resource
directories and a ready-to-sign-in Codex provider exist. Existing providers
and complete defaults are preserved.

Add local project directories from the Web UI. Projects are workspaces and do
not need their own `.agents/sai.yaml`; every project managed by the running
application uses the current server-root configuration.

Minimal root configuration:

```yaml
# <server-root>/sai.yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

agent:
  max_turns: 8
  stream: true
  show_reasoning: false

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
# Optional provider-wide proxies:
# http_proxy: http://127.0.0.1:7890
# https_proxy: http://127.0.0.1:7890

models:
  glm-5.2:
    id: glm-5.2
    context_window: 128000
    temperature: 0.6
    max_tokens: 4096
```

Relative paths in the root configuration resolve from the server root. API keys
can reference environment variables and are resolved only by the Go process.
Diagnostic logging is disabled unless a non-empty `logging.path` is explicitly
configured.

See [docs/configuration.md](docs/configuration.md) for the supported schema.

## Web application

The first release provides:

- Registered project navigation.
- Server-root Provider/model configuration and Codex device login.
- Durable session creation and history.
- Streaming assistant and reasoning output.
- Tool requested/running/finished status.
- Run cancellation.
- Safe message append while a run is active.
- Project and session rename, plus confirmed project-wide deletion.
- Archived session restore and permanent deletion.
- Manual context compaction.
- Cursor-based history loading.
- Active-run recovery and durable-history resync after a browser refresh.

The server binds only to loopback, generates a random capability token for each
start, validates Host and Origin, and does not enable CORS. Static assets are
embedded in the executable with `go:embed`.

## Build

Building requires Go, Node.js, and npm. Runtime distribution is still one
executable.

Windows:

```powershell
powershell -ExecutionPolicy Bypass -File scripts/build.ps1
```

Linux or macOS:

```sh
sh scripts/build.sh
```

The build first runs the Vite production build, embeds its output, and then
cross-compiles:

- `dist/sai-windows-amd64.exe`
- `dist/sai-linux-amd64`
- `dist/sai-darwin-arm64`

Frontend checks can be run independently:

```powershell
cd web
npm ci
npm run build
```

Go validation:

```powershell
go test ./...
```

The active release-hardening roadmap is tracked in
[docs/tasks/v0.1-release-hardening-checklist.md](docs/tasks/v0.1-release-hardening-checklist.md).
