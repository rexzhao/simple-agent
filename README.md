# simple-agent

`sai` is a small local CLI agent runner for OpenAI-compatible Chat
Completions. It reads project instructions from `AGENTS.md` by default when
present, streams model output, and can expose built-in tools when explicitly
enabled.

## Configuration

By default, `sai` reads its root YAML config from `.agents/${arg[0]}.yaml` in
the current working directory, where `${arg[0]}` is the executable basename. For
the normal `sai` binary, that is `.agents/sai.yaml`. Use `--config <file>` to
point at a concrete root config file. Provider files live under the configured
`provider_dir`, usually `.agents/providers/`.

Relative paths in the root config file, including `provider_dir`, `auth_dir`,
`skill_dirs`, `mcp_dir`, `logging.path`, and `sessions.dir`, resolve relative to
the directory containing that config file. Relative paths in secondary config
files use the same rule; for example, provider `auth_file` is relative to the
provider YAML file.

Project-instruction configuration uses `agent.instruction_files`. If it is
omitted, behavior remains compatible with `["$CWD/AGENTS.md"]`. Entries load in
list order, may name files other than `AGENTS.md`, support `$CWD`, `$CONFIG`,
`$USER`, and `$REPO`, and glob matches load in stable path sort order within one
pattern. After expansion and glob matching, duplicate same-file matches are
deduplicated by canonical/clean absolute path: the first occurrence wins and
later duplicates are skipped silently. Successfully loaded unique files keep the
same instruction position: after built-in base instructions and before loaded
skills and the user prompt.

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

sessions:
  enabled: false
  dir: sessions
  save_tool_results: true
```

`logging.path` is a log root hint, kept compatible with older configs. For
example, `logs/sai.jsonl` uses `.agents/logs/` as the root; each runtime session
precomputes a future log path such as
`.agents/logs/<timestamp>-<random>/sai.jsonl`. The directory and file are created only when the first log event is
written, so a session that exits before making a request does not create a log
session. Set `logging.path` to an empty string to disable JSONL logging. Logs do
not include prompt, response, tool result, API key, or authorization header
bodies.

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

`context_window` is optional local metadata for budget checks. If it is omitted,
`sai` uses a conservative estimated default of `32000` tokens and records the
source as `estimated`; configured values are recorded as `configured`.
Model profile `type` is optional and defaults to `openai-chat`; set it under the
model profile for protocols such as `anthropic-messages` or `openai-responses`.

## Basic Usage

```sh
sai
sai --config ./config/sai.yaml
sai session resume <session-id>
sai --mailbox
```

When no command is provided, `sai` starts an interactive session for the current
directory. It finds the nearest registered project; if none exists, it registers
the current directory automatically. A durable session is created when the first
ordinary user message is sent.

Type one message per line. Blank lines are ignored; `/exit`, `/quit`, or EOF
exits normally. To enter a multiline message, type a line containing only `"""`,
then the message body, then another line containing only `"""`; newlines inside
the body are preserved and slash commands inside the body are sent as text.
Empty multiline messages are ignored.

Use `sai session resume <session-id>` to continue an existing session. It renders
the visible session snapshot and then resumes the interactive prompt using the
session's stored cwd, config, provider, model, tools, MCP, and skill metadata.

Before each provider request, `sai` estimates input tokens from the full message
history and tool schemas. At 80% of the model context window it writes a warning
to stderr with only token counts and the window size. At or above the window it
refuses to send the provider request. It does not silently truncate or summarize
system/developer instructions, tool schemas, tool results, or prior messages.

Check local configuration health without starting a runtime session:

```sh
sai doctor
sai --config ./config/sai.yaml doctor
```

`sai doctor` checks local config files, provider/default model resolution, API
key configuration, enabled tools/skills/MCP config, and log directory
writability. It does not send model requests or start MCP servers, and its
output is redacted so it does not print secrets. It writes `OK`, `WARN`, and
`ERROR` lines to stdout; any `ERROR` makes the command exit with code 1.

Resumable sessions are an explicit opt-in feature. They save full prompts,
assistant output, assistant tool calls, and tool results under `sessions.dir`,
so treat those files as sensitive:

```sh
sai project create
sai session create
sai session resume <id>
sai session list
sai session show <id>
sai session archive <id>
sai session remove <id>
```

Session management commands work even when `sessions.enabled` is `false`, so
existing files can be inspected after automatic saving is disabled. `list` and
`show` only print metadata and warnings; they do not print full messages, prompt
text, assistant output, or tool result content. Saved sessions also include
context window metadata and recent usage tracking, which `show` displays only as
numbers and source labels. `remove` only deletes archived sessions.

Show CLI usage without loading configuration:

```sh
sai help
sai help version
sai help project
sai help project create
sai help session
sai help session resume
sai help config
sai help config show
sai help models
sai help tools
sai help tools list
sai help mcp
```

List built-in tools without loading configuration or provider credentials:

```sh
sai tools list
```

When a model calls a tool, `sai` prints a short status line such as
`tool: read_file docs/notes.md` to stderr. `list_files` similarly shows the
target directory, defaulting to `.`. Other tool arguments and all tool results
are not printed in that status line, and streamed model output remains on
stdout.

Accept local MCP mailbox tasks from another agent while the foreground CLI is
idle:

```sh
sai --mailbox
sai --mailbox 127.0.0.1:39123
```

Without an explicit address, `sai` listens on `127.0.0.1` with an OS-assigned
port and writes discovery details to `.agents/${basename argv[0]}-mailbox.json`.
The mailbox is only an input adapter for the current foreground CLI process; it
does not provide project/session management APIs.

Set `agent.show_reasoning: true` in config to show reasoning output. Shown
reasoning is printed directly without a marker line. When stdout is an
interactive terminal, reasoning is shown in gray/dark ANSI style so it is easier
to distinguish from the final answer. Tool status lines use their own muted
stderr styling when supported, and every non-reasoning output resets the style
first so final answers never inherit gray. Pipes, redirected output, and tests
stay plain text. Set `NO_COLOR` to a non-empty value to disable ANSI styling.

Other useful commands:

```sh
sai version
sai config show
sai models list
sai mcp list
sai tools list
sai project list
sai session list
```

The same concrete config file flag can be mixed into diagnostics and list
commands:

```sh
sai config show --config ./config/sai.yaml
sai models list --config ./config/sai.yaml
sai mcp list --config ./config/sai.yaml
sai session list --config ./config/sai.yaml
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

If the convenience link does not already exist, the build scripts also create a
symbolic link for the current host: `dist/sai.exe` on Windows, or `dist/sai` on
Linux and macOS.
