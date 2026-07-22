# Configuration

Configuration belongs to a registered project. The Web UI registers a project
directory; creating a session loads `<project>/.agents/sai.yaml` unless another
config path is provided through the Web API.

The browser never receives resolved API keys or authorization tokens.

## Layout

```text
project/
  .agents/
    sai.yaml
    providers/
      paperhub.yaml
      anthropic.yaml
    mcp/
      local.yaml
    skills/
      reviewer/
        SKILL.md
  AGENTS.md
```

Relative paths in `sai.yaml` resolve from the directory containing that file.
Relative paths in provider and MCP files resolve from their own file location.

## Root configuration

```yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers
auth_dir: auth
skill_dirs:
  - skills
mcp_dir: mcp

agent:
  instruction_files:
    - $CWD/AGENTS.md
  max_turns: 8
  stream: true
  show_reasoning: false

prompt:
  system_prompt: ""

tools:
  enabled:
    - list_files
    - read_file

logging:
  path: logs/sai.jsonl
  level: info

compaction:
  enabled: false
  threshold_percent: 80
```

`default_provider` selects a provider file by name. `default_model` selects a
profile key inside that provider. Both are required when the Web UI creates a
session.

`agent.instruction_files` defaults to `[$CWD/AGENTS.md]`. Supported placeholders
include `$CWD`, `$CONFIG`, `$USER`, and `$REPO`. Files are loaded in configured
order and glob matches use stable path ordering.

## Provider profiles

OpenAI-compatible Chat Completions:

```yaml
name: paperhub
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY

models:
  glm-5.2:
    id: glm-5.2
    type: openai-chat
    context_window: 128000
    temperature: 0.6
    max_tokens: 4096
```

Supported model profile types:

- `openai-chat` (default)
- `openai-responses`
- `openai-codex`
- `anthropic-messages`

Secrets beginning with `$` are read from the named environment variable. A
direct secret string is supported but should not be committed.

Codex subscription profiles use an OAuth token file:

```yaml
name: codex-work
base_url: https://chatgpt.com/backend-api/codex
auth_file: ../auth/codex-work.json

models:
  default:
    id: gpt-5.3-codex
    type: openai-codex
    context_window: 200000
```

The current Web release consumes an existing token file but does not expose a
browser login flow yet.

## Tools

Built-in tools are disabled unless listed in `tools.enabled`:

```yaml
tools:
  enabled:
    - list_files
    - read_file
    - write_file
    - edit_file
    - shell
```

All relative tool paths and shell working directories are based on the durable
session's creation directory.

## MCP tool sources

MCP currently acts only as a tool source for the agent runtime. A configured
stdio server can be enabled in the root configuration and referenced by its
tool names.

```yaml
# .agents/mcp/local.yaml
id: local
command: node
args: [server.js]
enabled: true
env:
  TOKEN: $LOCAL_MCP_TOKEN
```

The former CLI mailbox MCP input adapter has been removed with the CLI product.

## Skills

Each direct child of a configured `skill_dirs` directory may contain a
`SKILL.md`. Skills that allow model invocation are added to the session's
instruction snapshot and their IDs are recorded with the session.

## Sessions and storage

Web sessions are always durable because browser refresh and event recovery
depend on canonical persisted items. Session creation records:

- Project and creation working directory.
- Root config path.
- Provider, model profile, model ID, and parameters.
- Enabled tools, MCP servers, and skills.
- Instruction snapshot and sources.
- Context-window metadata.
- Whether reasoning is visible.

Project/session records live below the launcher storage root, which defaults to
the OS user configuration directory under `sai`. Override it with
`--server-root` or `SAI_SERVER_ROOT`.

Session segments are append-only JSONL. Large content may be stored as verified
blobs; browser chat APIs expose only inline text or a safe preview.

## Logging

An empty `logging.path` disables JSONL logging. A configured path is resolved
from `sai.yaml`. Logs contain operational metadata but must not contain prompt,
response, tool-result, API-key, or authorization-header bodies.

## Context window and compaction

`context_window` supplies local budgeting metadata. If omitted, SAI uses a
conservative estimated default. The Web UI shows usage events and exposes manual
compaction. Automatic compaction follows the `compaction` configuration when
enabled.
