# Configuration

Configuration belongs to the launcher server root and is shared by all
registered projects. `--server-root` selects the namespace containing the root
configuration, Provider credentials, optional diagnostic logs, and durable
project/session data. Projects remain independent workspaces for `$CWD`,
`AGENTS.md`, and tool execution; they do not carry their own `.agents/sai.yaml`.

The browser never receives resolved API keys or authorization tokens.

When opening a Server Root, SAI creates a `codex` provider when that provider
name is not already configured. It includes a ready-to-use `default` model
profile and is selected as the shared default only when `default_provider` or
`default_model` is still empty. Existing provider files and a complete existing
default are never replaced.

On Unix-like systems SAI creates the server-root core directories with mode
`0700`, and creates or rewrites root/provider configuration files with mode
`0600`. Manually managed provider YAML files should use the same restrictive
permissions. Windows access control continues to rely on the owning user's ACL.

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
  - $USER/.agents/skills
  - $REPO/.agents/skills
  - $CWD/.agents/skills
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
    - glob_files
    - grep_files
    - write_file
    - edit_file
    - apply_patch
    - shell

compaction:
  enabled: false
  threshold_percent: 80
  reserved: 0
  max_request_bytes: 716800
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
# Optional cap on in-flight HTTP requests across the whole process:
# max_concurrent_requests: 5
# Optional provider-wide proxies:
# http_proxy: http://127.0.0.1:7890
# https_proxy: http://127.0.0.1:7890

models:
  glm-5.2:
    id: glm-5.2
    type: openai-chat
    input: [text, image]
    context_window: 128000
    output_limit: 4096
    temperature: 0.6
    max_tokens: 4096
    reasoning_config:
      parameter: reasoning_effort
      default: high
      levels:
        high: high
        max: max

  deepseek-v4-flash:
    id: deepseek-v4-flash
    type: openai-chat
    input: [text, image]
    context_window: 128000
    output_limit: 8192
    temperature: 0.4
    max_tokens: 8192
    # Optional prices, in currency units per 1,000,000 tokens.
    # Cache-write pricing is optional and defaults to cache-miss pricing.
    pricing:
      currency: USD
      input_cache_hit: 0.15
      input_cache_miss: 1.50
      cache_write: 1.50
      output: 7.50
    reasoning_config:
      parameter: reasoning_effort
      default: high
      levels:
        low: low
        high: high
        max: max
```

Supported model profile types:

- `openai-chat` (default)
- `openai-responses`
- `openai-codex`
- `anthropic-messages`

### Model pricing and session cost

`pricing` is optional and belongs to a model profile. Values are per one
million tokens, so the example above charges `1.50` currency units for one
million uncached input tokens and `0.15` for one million cache-hit tokens.
`input_tokens` in SAI usage events is the uncached input bucket. `cache_write`
is optional for backwards compatibility and defaults to the cache-miss price
when omitted. `output` usage already includes any provider-reported reasoning
output and is not charged a second time.

For providers with different short- and long-context prices, the top-level
prices are the short-context prices and `long_context` is selected when the
request input exceeds `long_context_threshold`:

```yaml
pricing:
  currency: USD
  input_cache_hit: 0.50
  input_cache_miss: 5.00
  cache_write: 6.25
  output: 30.00
  long_context_threshold: 272000
  long_context:
    input_cache_hit: 1.00
    input_cache_miss: 10.00
    cache_write: 12.50
    output: 45.00
```

The Web UI shows cost, API request count, token count, and the cumulative
session cost when a price is configured. A session stores the price snapshot
from creation, so changing the model price does not rewrite historical
session costs.

`input` declares the modalities accepted by a model profile. Omit it (or use
`[text]`) for text-only models; add `image` to enable image attachments in the
Web application:

```yaml
input: [text, image]
```

Some OpenAI-compatible Chat providers do not accept the newer `developer`
message role. Configure a model-specific wire-role mapping without changing
the durable session history:

```yaml
developer_role: system
```

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

`http_proxy` and `https_proxy` are optional provider-level proxy URLs. They
apply to every model profile in that provider, including model discovery;
Codex token refreshes triggered by model requests use the same proxy client.
`http_proxy` handles requests whose destination URL uses HTTP, while
`https_proxy` handles HTTPS destinations. The proxy URL itself may use either
`http://` or `https://` (an HTTPS API commonly uses an `http://` CONNECT proxy).
When either provider-level field is set, an omitted counterpart connects
directly. When both are omitted, the standard Go transport remains in use, so
the process-level `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` environment
variables keep their normal behavior.

`request_timeout` controls how long each attempt waits for HTTP response
headers. Header timeouts are retried once before the turn fails. The default is
15 seconds when this option is omitted.

`max_concurrent_requests` caps how many HTTP requests to this provider may be
in flight at once across the whole process. The limit is shared by every
session, subagent run, and compaction summary using the provider, and it also
covers auxiliary calls such as model discovery and Codex token refreshes. Once
the budget is exhausted, further requests wait for a free slot instead of
failing; a streaming request holds its slot until the stream ends. Omit the
option (or set it to 0) for no limit.

OpenAI Responses and Codex can also report transient `server_error` or
`server_is_overloaded` failures inside an already-open SSE stream. SAI retries
those failures up to two times with 1s/2s backoff, but only when that provider
attempt produced no text, reasoning, usage, or tool-call events. Once any model
progress is observed, the request is never replayed automatically.

`reasoning_config` maps the reasoning names shown by SAI to the value expected
by that model. `parameter` is a dot-separated request path, `default` is the
level selected for a new session, and `levels` may contain string, number,
boolean, or object values. SAI follows Pi's common level vocabulary:
`off`, `minimal`, `low`, `medium`, `high`, `xhigh`, and `max`. Known GPT-5,
OpenRouter, GLM-5.2, DeepSeek-v4 (including the `deepseek-v4-flash` tier), and
adaptive Claude models receive Pi-compatible defaults when a model is saved
without an explicit mapping. Unknown models are left unconfigured.

The generated Codex subscription profile uses an OAuth token file and is
equivalent to:

```yaml
name: codex
base_url: https://chatgpt.com/backend-api/codex
auth_file: ../auth/codex.json
request_timeout: 60s

models:
  default:
    id: gpt-5.5
    type: openai-codex
    input: [text, image]
    context_window: 400000
    input_limit: 272000
    output_limit: 128000
    parameters:
      responses:
        compaction:
          mode: responses-compact
    reasoning_config:
      parameter: reasoning.effort
      default: xhigh
      levels:
        off: none
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
    - apply_patch
    - shell
```

All relative tool paths and shell working directories are based on the durable
session's creation directory. `apply_patch` accepts the OpenCode patch format. A
patch must start with `*** Begin Patch` and end with `*** End Patch`; it can
contain `*** Add File:`, `*** Update File:`, `*** Delete File:`, and optional
`*** Move to:` sections. Added-file content lines start with `+`; update hunks
start with `@@` (optionally followed by an anchor) and use space, `-`, and `+`
lines for context, removal, and addition. Paths must be workspace-relative. It
validates every file and hunk before writing, creates required parent
directories, and rejects binary files and paths outside the workspace.

`edit_file` matches LF and CRLF input equivalently, preserves a UTF-8 BOM, and
writes using the file's original line-ending style.

Durable session orchestration tools use the same explicit capability list:

```yaml
tools:
  enabled:
    - session_models
    - session_start
    - session_search
    - session_get
    - session_history
    - session_send
    - session_wait
    - session_stop
```

`session_start` creates an asynchronous child session in the same project and
records its parent/root lineage. It can inherit the caller's frozen
provider/model snapshot or select a provider and model profile returned by
`session_models`. `session_send` supports strict active-turn `steer` (which
fails once that turn stops accepting input), a separate next-turn `queue`, and
`on_settle=continue_parent` for delivering a message to a direct child session
and being durably woken when the child run settles. `session_get` and
`session_wait` return only persisted assistant items, never
uncommitted streaming deltas. `session_history` returns paginated,
user-visible persisted conversation items. Session tools cannot query sessions
in another project, wait for or stop their own run, or spawn beyond the bounded
child depth. Agent-created children are leaf workers: `session_start` and
`session_send` are removed from their capability snapshots even when those
tools are enabled for root sessions. See
[Agent session orchestration](agent-session-orchestration.md) for the complete
tool arguments, output semantics, concurrency behavior, limits, and stable
error codes.

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
to resolve from the root configuration directory. An entry that references
`$REPO` is skipped (without error) when the working directory is not inside a
repository. The default layers user, repository, and working-directory skills:

```yaml
skill_dirs:
  - $USER/.agents/skills
  - $REPO/.agents/skills
  - $CWD/.agents/skills
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
authorization-header bodies by default. Durable session event records are
independent of this diagnostic logging switch.

The legacy `logging.request_bodies` setting is used only as the initial value
for newly created sessions (and as a fallback for sessions created by older
versions). A saved per-session Debug setting takes precedence.

For temporary provider-protocol diagnosis, open the conversation's **Debug**
menu in the Web UI and enable **Capture provider request bodies**. The setting
is stored on that conversation and applies from its next turn; it does not
enable capture for other conversations. Each OpenAI Responses or Codex request
body is then written exactly as sent to a `responses-request-NNNNNN.json` or
`responses-compact-request-NNNNNN.json` file beside `sai.jsonl`. These captures
never include HTTP authorization headers, but they do contain complete prompts,
tool definitions, reasoning state, and tool outputs. Disable the option and
remove the captures after diagnosis.

## Context window and compaction

`context_window`, `input_limit`, and `output_limit` supply local model-budget
metadata; they are not sent as provider request parameters. If `context_window`
is omitted, SAI uses a conservative estimated default. The Web UI shows usage
events and exposes manual compaction.

When compaction is enabled, the next user turn checks the previous model
response usage. The provider's `total_tokens` is preferred; if it is absent,
SAI uses input + output + cache-read + cache-write tokens. With an
`input_limit`, the trigger is `input_limit - reserved`; an omitted or zero
`reserved` defaults to `min(20000, output_limit)`. Without an `input_limit`, the
trigger is `context_window - output_limit`. `threshold_percent` remains the
compatibility fallback for profiles that do not define output limits.

Compaction defaults to the provider-neutral local summary implementation. An
OpenAI Responses or Codex model profile can explicitly opt into the public
stateless `POST /responses/compact` endpoint:

```yaml
parameters:
  responses:
    compaction:
      mode: responses-compact
```

SAI stores the returned canonical output as opaque provider items and replays it
only to the same base URL and model. If the standalone compact request fails,
SAI rebuilds the complete history from the append-only session ledger and falls
back to the configured local summary model. Compatible third-party endpoints
are never assumed to support remote compaction. `context_management` and the
Codex-specific `compaction_trigger` protocol are not enabled by this option.
When compaction is enabled, `max_request_bytes` adds a pre-turn replay-pressure
guard for OpenAI Responses and Codex requests. SAI compacts before the turn when
either the token threshold or this serialized request-size threshold is reached.
Set it to `0` to disable the byte-size guard.
