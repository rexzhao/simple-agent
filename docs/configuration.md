# Configuration

Configuration belongs to the server root and is shared by all sessions.
`--server-root` (or `SAI_SERVER_ROOT`) selects the namespace containing the
root configuration, provider credentials, and session data.

Provider credentials are resolved from environment references at startup
(`api_key: $PAPERHUB_API_KEY`); resolved keys never appear in any output.

## Layout

```text
<server-root>/
  sai.yaml                 # root configuration
  providers/
    paperhub.yaml          # provider profiles
  data/
    <project>/             # project id = hash of the workspace directory
      <session>/
        log.jsonl          # active segment: the current context's records
        log-0001.jsonl     # immutable archive of the history before each
                           # compaction (present only after compacting)
```

## Root configuration

`<server-root>/sai.yaml`:

```yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

agent:
  max_iterations: 8        # iteration budget per run (default 8)
  auto_compact: true       # compact the context automatically when the
                           # request approaches the model's budget (default true)

prompt:
  system_prompt: |         # appended after the built-in instructions
    Extra instructions here. Placeholders: {{cwd}}, {{config_dir}}, {{time.now}}.
```

## Provider profiles

`<server-root>/providers/<name>.yaml`:

```yaml
name: paperhub
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY
request_timeout: 60s       # optional, positive duration
http_proxy: http://127.0.0.1:7890    # optional
https_proxy: http://127.0.0.1:7890   # optional

models:
  deepseek-v4.1-flash:
    id: deepseek-v4.1-flash
    type: openai-chat
    context_window: 1000000  # total input+output capacity; drives the
                              # automatic compaction budget (default 32000)
    output_limit: 384000   # optional, injected as the output token cap
    reasoning_config:
      parameter: reasoning_effort
      default: high
      levels:
        low: low
        high: high
        max: max
    parameters:            # passed through into the request body verbatim
      temperature: 1.0
```

Supported `type` values: `openai-chat`, `openai-responses`, `openai-codex`,
`anthropic-messages`. For `openai-chat` models an optional `compatibility`
key selects wire quirks: `openai` (default) or `kimi` (replays
`reasoning_content` in assistant history and sends the session id as
`prompt_cache_key`, for Kimi's prefix caching).

`developer_role` (optional, `openai-chat` only) maps the developer message
role on the wire, e.g. `system` for providers that reject the newer role.

### Reasoning

`reasoning_config` injects a reasoning level into the request through the
configured `parameter` path:

- `type: effort` (default): `levels` maps names to string values
- `type: budget_tokens`: `levels` maps names to numbers

The `parameter` is the literal request field: a wrong name (for example the
nested `reasoning.effort` where the provider expects the top-level
`reasoning_effort`) is **silently ignored by the provider** — the model then
answers without thinking and no error is reported. Check the provider's API
reference for the exact field before configuring.

### Automatic compaction

When `agent.auto_compact` is on (the default), the request is checked
against a budget derived from `context_window` before each turn and after
each tool batch: at the soft limit the session is summarized into a
`context.compacted` record once enough new turns arrived since the last
compaction; at the hard limit it compacts immediately and fails the turn
if even the summary does not fit. The conversation before a compaction is
replaced by the summary.

### Legacy keys

`pricing`, `input`, and `input_limit` in a model profile are accepted and
ignored (web-stack leftovers); they are not sent to the provider.

## Codex authentication

`openai-codex` providers authenticate through a device login flow instead
of `api_key`; the token store location is `auth_file` (optional, defaults
under the server root).
