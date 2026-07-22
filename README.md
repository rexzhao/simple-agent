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

## Project setup

Add a local project directory from the Web UI. The project should contain a
root configuration at `.agents/sai.yaml` and provider profiles under the
configured `provider_dir`.

Minimal root configuration:

```yaml
# .agents/sai.yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

agent:
  max_turns: 8
  stream: true
  show_reasoning: false

tools:
  enabled: []

logging:
  path: logs/sai.jsonl
  level: info
```

Provider profile:

```yaml
# .agents/providers/paperhub.yaml
name: paperhub
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY

models:
  glm-5.2:
    id: glm-5.2
    context_window: 128000
    temperature: 0.6
    max_tokens: 4096
```

Relative paths in configuration files resolve from the file that declares
them. API keys can reference environment variables and are resolved only by the
Go process.

See [docs/configuration.md](docs/configuration.md) for the supported schema.

## Web application

The first release provides:

- Registered project navigation.
- Durable session creation and history.
- Streaming assistant and reasoning output.
- Tool requested/running/finished status.
- Run cancellation.
- Manual context compaction.
- Cursor-based history loading.

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
