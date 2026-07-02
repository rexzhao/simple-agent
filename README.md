# simple-agent

`sai` is a small local CLI agent runner for OpenAI-compatible Chat
Completions. It reads project instructions from `AGENTS.md` when present,
streams model output, and can expose built-in tools when explicitly enabled.

## Configuration

By default, `sai` reads configuration from `.agents/sai.yaml` in the current
working directory. Use `--config-dir` to point at another configuration
directory. Provider files live under the configured `provider_dir`, usually
`.agents/providers/`.

Minimal provider setup:

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

```yaml
# .agents/providers/paperhub.yaml
name: paperhub
type: openai-chat
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY

models:
  glm-5.2:
    id: glm-5.2
    temperature: 0.6
    max_tokens: 4096
```

## Basic Usage

```sh
sai run --provider paperhub --model glm-5.2 "Hello"
```

Show CLI usage without loading configuration:

```sh
sai help
sai help version
sai help run
sai run -h
sai help config
sai help config show
sai help models
sai help mcp
```

Enable tools for a single run:

```sh
sai run --enable-tools list_files,read_file "List this project"
```

Write non-sensitive run diagnostics to stderr without changing streamed stdout:

```sh
sai run --verbose --provider paperhub --model glm-5.2 "Hello"
```

Other useful commands:

```sh
sai version
sai config show
sai models list
```

## Build

Windows:

```powershell
powershell -ExecutionPolicy Bypass -File scripts/build.ps1
```

Linux or macOS:

```sh
sh scripts/build.sh
```

Both scripts build single-file CLI binaries into `dist/`:

- `sai-windows-amd64.exe`
- `sai-linux-amd64`
- `sai-darwin-arm64`
