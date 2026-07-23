# Configuration

Configuration belongs to the launcher server root and is shared by all
registered projects. `--server-root` selects the namespace containing the root
configuration, Provider credentials, optional diagnostic logs, and durable
project/session data. Projects remain independent workspaces for `$CWD`,
`AGENTS.md`, and tool execution; they do not carry their own `.agents/sai.yaml`.

The browser never receives resolved API keys or authorization tokens.

## Layout

```text
<server-root>/
  <basename>.yaml
  providers/
    paperhub.yaml
    anthropic.yaml
  auth/
  mcp/
    local.yaml
  skills/
    reviewer/
      SKILL.md
  logs/                    # only when logging.path is configured
  data/
    projects/
    sessions/
```

The default server root is `<os.UserConfigDir()>/<basename>`. A custom
`--server-root PATH` takes precedence over the basename-derived
`<BASENAME>_SERVER_ROOT` environment variable. Relative paths in the root config
resolve from the server root.
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
request_timeout: 60s

models:
  glm-5.2:
    id: glm-5.2
    type: openai-chat
    context_window: 128000
    temperature: 0.6
    max_tokens: 4096
    reasoning_config:
      parameter: reasoning_effort
      default: high
      levels:
        high: high
        max: max
```

Supported model profile types:

- `openai-chat` (default)
- `openai-responses`
- `openai-codex`
- `anthropic-messages`

### OpenAI Responses cache and state

Responses profiles default to `store: false` and manual history replay. During a
session, SAI uses the stable session ID as `prompt_cache_key` (clamped to 64
Unicode characters) and sends matching session-affinity headers. The provider
combines that routing key with the actual prompt-prefix hash, so different
prefixes do not share a cache entry; an explicit key can group a known workload.

Adapter-only options live below `parameters.responses` and are not sent as a
literal `responses` API field:

```yaml
models:
  gpt-5.6:
    id: gpt-5.6
    type: openai-responses
    parameters:
      max_output_tokens: 4096
      responses:
        store: false                    # default
        state: manual                   # manual | previous_response_id
        cache:
          enabled: true                 # default
          key: shared-code-review       # optional; session ID is the default
          capability: auto              # auto | modern | legacy | disabled
          mode: explicit                # GPT-5.6+: implicit | explicit
          ttl: 30m                      # GPT-5.6+: currently only 30m
          breakpoint: instructions      # mark the last leading system/developer block
          session_affinity: auto        # auto | openai | openrouter | none
```

`mode: explicit` requires an actual cache breakpoint. Conversely, a breakpoint
is rejected unless explicit mode is active. For older Responses models, use
`cache.retention: in_memory` or `cache.retention: 24h`; modern cache fields are
not sent unless the model is detected or configured as `modern`. Set
`capability: disabled` for a compatible endpoint that rejects prompt-cache
fields. Top-level raw `prompt_cache_*` parameters remain available as an escape
hatch.

`state: previous_response_id` requires `store: true`. It sends only input added
after the latest matching stored response. If the provider rejects an expired
or unavailable ID, SAI retries once with the full input. Manual mode preserves
and replays Responses reasoning/encrypted reasoning, message IDs/phases,
function-call item IDs, and tool outputs so `store: false` and ZDR-style flows
retain model state without relying on server-side response storage.

Secrets beginning with `$` are read from the named environment variable. A
direct secret string is supported but should not be committed.

`request_timeout` controls how long each attempt waits for HTTP response
headers. Header timeouts are retried once before the turn fails. The default is
15 seconds when this option is omitted.

`reasoning_config` maps the reasoning names shown by SAI to the value expected
by that model. `parameter` is a dot-separated request path, `default` is the
level selected for a new session, and `levels` may contain string, number,
boolean, or object values. SAI follows Pi's common level vocabulary:
`off`, `minimal`, `low`, `medium`, `high`, `xhigh`, and `max`. Known GPT-5,
OpenRouter, GLM-5.2/DeepSeek-v4, and adaptive Claude models receive Pi-compatible
defaults when a model is saved without an explicit mapping. Unknown models are
left unconfigured.

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
    reasoning_config:
      parameter: reasoning.effort
      default: xhigh
      levels:
        minimal: low
        low: low
        medium: medium
        high: high
        xhigh: xhigh
```

The Web Server Root settings page can add and edit provider files, fetch model
IDs from compatible `/models` endpoints, change the shared default, and start or
clear a Codex device login. Codex access and refresh tokens stay in the current
server root's configured `auth_file`; the browser only receives status, account,
expiry, verification URL, and device-code metadata.

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

`skill_dirs` supports `$HOME`, `$REPO`, `$CWD`, and `$CONFIG` path placeholders;
`$USER` remains an alias for `$HOME`. `$CWD` is the durable session working
directory, `$REPO` is discovered upward from it, and relative entries continue
to resolve from the root configuration directory. For example:

```yaml
skill_dirs:
  - $REPO/.agents/skills
  - $HOME/.sai/skills
```

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

Project/session records live below the same server root as configuration. For a
binary named `sai`, override the default with `--server-root` or
`SAI_SERVER_ROOT`; renamed binaries use the corresponding basename-derived
environment variable.

Session segments are append-only JSONL. Large content may be stored as verified
blobs. The authenticated browser chat API materializes complete user and
assistant messages; blob-backed tool content remains limited to its safe
preview.

## Logging

Diagnostic JSONL logging is disabled by default. It is enabled only when the
root configuration explicitly contains a non-empty path:

```yaml
logging:
  path: logs/sai.jsonl
  level: info
```

A relative path resolves from the server root. Disabled logging creates no
`logs` directory or empty JSONL file. Logs contain operational metadata but
must not contain prompt, response, tool-result, API-key, or
authorization-header bodies. Durable session event records are independent of
this diagnostic logging switch.

## Context window and compaction

`context_window` supplies local budgeting metadata. If omitted, SAI uses a
conservative estimated default. The Web UI shows usage events and exposes manual
compaction. Automatic compaction follows the `compaction` configuration when
enabled.
