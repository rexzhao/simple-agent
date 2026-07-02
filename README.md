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

`logging.path` is a log root hint, kept compatible with older configs. For
example, `logs/sai.jsonl` uses `.agents/logs/` as the root; each `sai run` or
`sai chat` process writes JSONL metadata to
`.agents/logs/<timestamp>-<random>/sai.jsonl`. Set `logging.path` to an empty
string to disable JSONL logging. Logs do not include prompt, response, tool
result, API key, or authorization header bodies.

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

Start a line-oriented chat session with the same provider/model selection rules:

```sh
sai chat --provider paperhub --model glm-5.2
```

In chat, type one message per line. Blank lines are ignored; `/exit`, `/quit`,
or EOF exits normally. Session history stays in memory for the current process
and is not written to disk.

Show CLI usage without loading configuration:

```sh
sai help
sai help version
sai help run
sai run -h
sai help chat
sai chat -h
sai help config
sai help config show
sai help models
sai help tools
sai help tools list
sai help mcp
```

Enable tools for a single run:

```sh
sai run --enable-tools list_files,read_file "List this project"
sai chat --enable-tools list_files,read_file
```

List built-in tools without loading configuration or provider credentials:

```sh
sai tools list
```

When a model calls a tool, `sai run` and `sai chat` print a short status line
such as `! tool: read_file docs/notes.md` to stderr. `list_files` similarly
shows the target directory, defaulting to `.`. Other tool arguments and all tool
results are not printed in that status line, and streamed model output remains
on stdout.

Write non-sensitive run diagnostics to stderr without changing streamed stdout:

```sh
sai run --verbose --provider paperhub --model glm-5.2 "Hello"
```

Show reasoning output, which is hidden by default:

```sh
sai run --show-reasoning --provider paperhub --model glm-5.2 "Hello"
```

Shown reasoning starts with a `? reasoning` marker line. When stdout is an
interactive terminal, the marker and reasoning are shown in gray/dark ANSI style
so they are easier to distinguish from the final answer. Pipes, redirected
output, and tests stay plain text. Set `NO_COLOR` to a non-empty value to disable
ANSI styling.

Other useful commands:

```sh
sai version
sai config show
sai models list
sai tools list
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
