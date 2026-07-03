# 配置设计

配置优先使用 YAML。第一阶段目标是让 provider 和 model profile 的边界清晰，避免把
所有服务和模型参数塞进一个大文件。

## 配置位置

配置根目录只有两种来源：

1. 默认使用启动时当前工作目录下的 `.agents`。
2. 通过 `--config-dir` 显式指定目录。

暂时不读取用户目录，也不向用户目录写入默认配置。

`AGENTS.md` 不属于配置目录。它是项目上下文文件，v0.1 默认只从启动时当前工作目录读取。
`--config-dir` 不改变 `AGENTS.md` 的查找位置。

```text
sai --prompt "你好" --quit
sai --stdin --quit
sai --config-dir ./config --prompt "你好" --quit
sai chat --prompt "你好" --quit
sai chat --stdin --quit
sai chat --file prompt.md --quit
sai --config-dir ./config chat --prompt "你好" --quit
sai --config-dir ./examples/paperhub chat
```

## 文件布局

推荐布局：

```text
AGENTS.md
.agents/
  sai.yaml
  providers/
    paperhub.yaml
    local.yaml
    codex-work.yaml
  auth/
    codex-work.json
  skills/
    my-skill/
      SKILL.md
  mcp/
    local.yaml
  logs/
    20260702T030405.000000000Z-a1b2c3d4/
      sai.jsonl
  sessions/
    <session-id>/
      session.json
```

`.agents/` 是默认配置根目录；通过 `--config-dir` 指定时，配置根目录改为指定目录。
`sai.yaml` 是全局配置入口，负责默认 provider、默认 model、provider 目录、工具启用和
agent 通用参数。`providers/*.yaml`
每个文件描述一个 provider。`skills/<skill_id>/SKILL.md` 是 M7 后的本地 skill 推荐布局；
本地 skill 只有出现在 `skills.enabled` 或 `--enable-skills` 中才会注入运行时。
`mcp/*.yaml` 每个文件描述一个 MCP server；
MCP 是 M4 后能力，不属于 MVP 必需配置。

当配置根目录由 `--config-dir` 指定时，`sai.yaml` 和上述配置子目录从该目录读取；
`AGENTS.md` 仍从启动时当前工作目录读取。

## 全局配置

```yaml
# sai.yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers
auth_dir: auth
skill_dir: skills

agent:
  max_turns: 8
  stream: true
  show_reasoning: false

tools:
  enabled: []

skills:
  enabled: []

logging:
  path: logs/sai.jsonl
  level: info

# M13 后可选启用
sessions:
  enabled: false
  dir: sessions
  save_tool_results: true

# M4 后启用
mcp_dir: mcp
```

字段说明：

- `default_provider`：未通过命令行指定 provider 时使用。
- `default_model`：未通过命令行指定 model 时使用。
- `provider_dir`：provider 配置文件目录。相对路径基于配置根目录解析。
- `auth_dir`：M16 后的 OAuth token 文件目录。默认 `auth`，相对路径基于配置根目录解析。
- `skill_dir`：本地 skill 目录。默认是配置根目录下的 `skills`；相对路径基于配置根目录解析。
- `mcp_dir`：MCP 配置文件目录。M4 后启用；相对路径基于配置根目录解析。
- `agent.max_turns`：一次 agent loop 最多请求模型的轮数。
- `agent.stream`：默认是否启用 streaming。
- `agent.show_reasoning`：默认是否显示 reasoning stream。
  普通新 chat 中可用 `--show-reasoning=true/false` 显式覆盖配置；`--show-reasoning`
  等价于 `--show-reasoning=true`。
- `tools.enabled`：默认启用的工具列表。空列表表示不向模型暴露工具。
- `skills.enabled`：默认启用的本地 skill id 列表。空列表表示不读取或注入任何 skill。
- `logging.path`：JSONL 日志根/基准路径。相对路径基于配置根目录解析；例如
  `logs/sai.jsonl` 使用 `logs/` 作为 session root。空字符串表示禁用日志。
- `logging.level`：日志级别。
- `sessions.enabled`：M13 后的可恢复 session 开关。默认 `false`，不保存完整上下文。
  普通新 chat 中可用 `--save-session=true/false` 显式覆盖配置；`--save-session`
  等价于 `--save-session=true`。
- `sessions.dir`：M13 后的可恢复 session 存储目录。相对路径基于配置根目录解析。
- `sessions.save_tool_results`：M13 后启用 session 保存时是否保存完整 tool result messages。
  可靠 resume 需要保存 tool results；关闭后只能作为降级或诊断模式设计。

## Provider 配置

每个 provider 一个 YAML 文件。

```yaml
name: paperhub
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY

models:
  glm-5.2:
    id: glm-5.2
    context_window: 128000
    temperature: 0.6
    max_tokens: 4096
  glm-5.2-fast:
    id: glm-5.2
    temperature: 0.2
    max_tokens: 2048
```

字段说明：

- `name`：provider 名称，必须和命令行 `--provider` 可选值一致。
- `base_url`：provider API base URL。`openai-chat` 不包含 `/chat/completions`；
  `anthropic-messages` 使用 Anthropic Messages API base，例如 `https://api.anthropic.com/v1`；
  `openai-responses` 不包含 `/responses`，例如 `https://api.openai.com/v1`。
- `api_key`：provider 的 API key 配置值，遵循敏感配置值的 `$ENV_NAME` 约定。
- `auth_file`：M16 后 `openai-codex` provider 使用的 OAuth token JSON 文件路径。相对路径
  基于 provider YAML 文件所在目录解析；不和 `api_key` 同时用于同一个 model profile。
- `models`：该 provider 下可选的模型配置。
- `models.<name>.id`：实际发送给 API 的模型 id。
- `models.<name>.type`：可选的模型协议/adapter 类型。未配置时默认 `openai-chat`。
  配置层识别 `openai-chat`、`anthropic-messages`、`openai-responses` 和 `openai-codex`。`sai chat`
  支持 `openai-chat`、`anthropic-messages` 的文本 streaming 和 tool use，以及
  `openai-responses` / `openai-codex` 的文本 streaming 和 function tool calling。
- `models.<name>.context_window`：可选的模型上下文窗口 token 数。未配置时，`sai`
  使用保守估算默认值 `32000`，并把来源记录为 `estimated`；显式配置时来源为
  `configured`。该字段是 `sai` 的本地元数据，不会透传给 provider。
- `models.<name>.*`：该 model profile 的请求参数。

model profile 的 key 是 CLI 选择时使用的名字。`id` 是实际传给模型服务的名称。这样可以
用同一个底层模型创建多个参数不同的 profile。
请求参数既可以继续写在 profile 顶层，也可以写在 `parameters` map 中；`id` 和
`type`、`context_window` 不属于请求参数。
旧配置中的 provider 顶层 `type` 不再作为运行时协议选择依据；迁移时请把协议 `type`
写到需要非默认协议的 model profile 下。

Anthropic Messages provider 使用同一套 provider/model profile 配置形态：

```yaml
name: anthropic
base_url: https://api.anthropic.com/v1
api_key: $ANTHROPIC_API_KEY

models:
  claude-sonnet-5:
    id: claude-sonnet-5
    type: anthropic-messages
    context_window: 200000
    max_tokens: 4096
  claude-haiku-4-5:
    id: claude-haiku-4-5
    type: anthropic-messages
    max_tokens: 2048
```

这类配置可以被加载、列入 `sai models list`，并参与模型解析；`sai chat` 可以使用
`anthropic-messages` 做文本 streaming，并支持 tool use / tool result 转换。

OpenAI Responses provider 使用同一套 provider/model profile 配置形态：

```yaml
name: openai
base_url: https://api.openai.com/v1
api_key: $OPENAI_API_KEY

models:
  default:
    id: gpt-5.1
    type: openai-responses
    context_window: 400000
    max_output_tokens: 4096
```

这类配置可以被加载、列入 `sai models list`，并参与模型解析；`sai chat` 会请求
`<base_url>/responses` 并转换 Responses semantic text streaming 事件和 streamed
function_call 事件。`openai-responses` 支持顶层 function `tools`、assistant 历史里的
`function_call` input item，以及 tool result 的 `function_call_output` input item。
`openai-responses` 使用 Responses API 的 `max_output_tokens`；
为兼容已有 profile，adapter 会把 legacy `max_tokens` 请求参数映射为
`max_output_tokens`，但不会覆盖显式配置的 `max_output_tokens`。`tool_choice`、
`parallel_tool_calls` 等普通 Responses function tool 参数可以透传。custom tools、
built-in web/code tools、stateful `previous_response_id` 对话续写、reasoning output item
passthrough 仍是后续项。

Codex subscription auth provider 使用 OAuth token 文件，而不是 API key：

```yaml
name: codex-work
base_url: https://chatgpt.com/backend-api/codex
auth_file: ../auth/codex-work.json

models:
  gpt-5.5:
    id: gpt-5.5
    type: openai-codex
    context_window: 400000
```

`sai auth codex login --provider codex-work` 使用 Codex headless device flow 登录，并生成
`providers/codex-work.yaml` 与 `auth/codex-work.json`。默认 provider 名称是 `codex`。
`--force` 可以覆盖已有的生成文件；未传 `--force` 时，只要目标 provider YAML 或 token
JSON 已存在，命令会在开始登录前失败。多个 provider 使用多个独立 auth 文件，因此
`codex`、`codex-work` 和 `codex-personal` 可以共存并分别刷新 token。
默认登录流程会向 issuer 的 `/api/accounts/deviceauth/usercode` 请求用户码，轮询
`/api/accounts/deviceauth/token` 取得 `authorization_code` 和 `code_verifier`，再向
`/oauth/token` 交换 access / refresh token。usercode 请求成功后，CLI 打印服务返回的
verification URL / user code；如果服务没有返回 verification URL，则使用 issuer 的
`/codex/device` 作为 Codex device page fallback。deviceauth authorization-pending
错误响应会继续轮询直到批准或过期。deviceauth usercode/token 请求使用 JSON body，
`/oauth/token` authorization-code exchange 和 refresh-token exchange 使用 form
encoding。测试或私有部署可通过登录命令的 endpoint flags 指向 fake issuer。

`openai-codex` 运行时复用 OpenAI Responses adapter 的 request、SSE 和 function tool
mapping，请求 `<base_url>/responses`。它从 `auth_file` 读取 access token，发送
`Authorization: Bearer <access>`；token 文件中存在 account id 时，同时发送
`ChatGPT-Account-Id`。access token 过期时，运行时使用 refresh token 刷新并写回同一
auth 文件。token 文件内容不会出现在 `sai config show`、verbose、日志或 HTTP 错误中。
M16 不读取、不迁移、不导入 `~/.codex/auth.json`。

`api_key` 是这次 provider 配置中的具体字段。其他需要脱敏的敏感配置值也可以采用同样
约定：字符串以 `$` 开头时，`$` 后面的内容作为环境变量名读取实际值；不以 `$` 开头时
表示直接配置值。这个解析发生在配置读取阶段；provider adapter 接收解析后的实际值。
除非某个 adapter 明确声明协议特定的默认环境变量，否则 adapter 不承担通用环境变量解析
职责。无论实际值来自环境变量还是直接配置，logs、verbose、resolved config 等输出都
不能泄露实际值。

## Skills 配置（M7 后）

本地 skill 目录由 `skill_dir` 指定，默认是配置根目录下的 `skills`。本项目推荐每个
skill 使用一个直接子目录，并在子目录下放置 `SKILL.md`：

```text
.agents/
  skills/
    my-skill/
      SKILL.md
```

`SKILL.md` 可以使用可选 YAML frontmatter：

```markdown
---
name: my-skill
description: short text
---
body...
```

如果没有 frontmatter，skill 名称默认使用目录名，description 为空，正文全文作为
instructions。启用方式有两种：

```yaml
skills:
  enabled:
    - my-skill
```

```text
sai chat --quit --enable-skills my-skill,other-skill --prompt "prompt"
sai chat --quit --disable-skills --prompt "prompt"
```

`--enable-skills` 覆盖配置中的 `skills.enabled`，不是追加；`--disable-skills` 本次运行
禁用所有 skills。二者同时出现时命令会失败。空 enabled 列表表示不发现、不读取、不注入
任何 skill。

已启用 skill 的 instructions 会作为 developer message 注入到模型请求中，顺序是：

```text
sai 内置基础约束 > AGENTS.md > enabled skills > 当前用户 prompt
```

多个 skill 按配置或 CLI 中的 enabled 顺序注入。未知 skill id 会报错并列出当前
`skill_dir` 下可用的 skill id；frontmatter 格式错误会让命令失败并指出对应
`SKILL.md`。

## MCP 配置（M4 后）

MCP 使用单独目录，不放入 provider 配置。每个 MCP server 一个 YAML 文件。

```yaml
# mcp/local.yaml
id: local
enabled: true
command: example-mcp-server
args: []
env: {}
```

字段说明：

- `id`：MCP server id，必须和文件名或命令行 `--enable-mcp` 可选值一致。
- `enabled`：默认是否启用该 MCP server。
- `command`：启动 stdio MCP server 的命令。
- `args`：启动参数。
- `env`：传给 MCP server 的环境变量。

MCP tools 会转换成内部 tool schema，但仍然需要出现在 enabled tools 列表中才会暴露给
模型。MCP tool 名称必须使用 `mcp.<server>.<tool>` 形式，避免和内置工具冲突。

默认启用 `enabled: true` 的 MCP server。如果传入 `--enable-mcp`，本次运行的 MCP server
启用列表完全由命令行决定，忽略各 MCP 文件中的 `enabled` 字段。

```text
sai chat --quit --enable-mcp local --prompt "只启用 local MCP server"
sai chat --quit --enable-mcp local,git --prompt "使用多个 MCP 服务"
sai chat --quit --enable-mcp local --enable-tools mcp.local.some_tool --prompt "暴露 MCP 工具给模型"
```

## 工具启用

当前内置工具：

```text
list_files
read_file
write_file
edit_file
shell
```

默认不启用任何工具：

```yaml
tools:
  enabled: []
```

启用示例：

```yaml
tools:
  enabled:
    - list_files
    - read_file
    - write_file
    - edit_file
    - shell
    - mcp.local.some_tool
```

命令行可以覆盖配置中的 enabled tools：

```text
sai chat --quit --enable-tools list_files,read_file --prompt "看看当前目录"
```

`--enable-tools` 是覆盖，不是追加。`shell`、`write_file` 和 `edit_file` 不需要额外
flag；只要它被启用，就按普通工具暴露给模型。

`sai tools list` 可以在不加载配置、不解析 provider API key 的情况下列出内置工具。发生
tool call 时，`sai chat` 默认向 stderr 打印 `tool: <name>` 形式的独立简短状态；支持
颜色且未设置 `NO_COLOR` 时，状态行使用 muted 样式并在行尾 reset。`read_file`、
`write_file` 和 `edit_file` 会追加目标文件路径，`list_files` 会追加目标目录且未提供
path 时显示 `.`。`shell` 和 MCP tool 状态只显示工具名，不显示命令参数或任意
arguments；状态也不包含 tool result 正文，stdout 仍只输出模型文本。

`shell` 工具默认在启动目录执行命令。v0.1 不提供 `--workdir` 配置。

注意：`--enable-mcp` 只决定启动哪些 MCP server；`--enable-tools` 决定哪些工具暴露给
模型。一个 MCP server 被启用后，它的工具仍需要出现在 enabled tools 列表中才会被模型
看到。

## 选择规则

会话开始时确定 provider 和 model：

```text
sai chat --quit --provider paperhub --model glm-5.2 --prompt "你是谁？"
sai chat --provider paperhub --model glm-5.2
```

选择优先级：

1. 命令行参数。
2. 全局 `sai.yaml` 默认值。
3. 如果仍无法确定，打印可选 provider/model 并停止。

配置目录选择优先级：

1. `--config-dir`。
2. 启动时当前工作目录下的 `.agents`。

根层解析从 argv 左到右扫描，跳过已知 flag 及其 value；第一个真正的非 flag token 是命令。
没有命令 token 时默认执行 `chat`，并把已扫描到的 chat flags 交给 `chat` 解析，例如
`sai --model fast --prompt "hi" --quit` 等价于 `sai chat --model fast --prompt "hi" --quit`。
带值 flag 的 value 不参与命令识别，因此 `sai --model fast chat --prompt "hi" --quit` 中的
`fast` 不是命令。命令 token 之外的参数交给对应命令解析，命令前后的 flags 可以混排；
chat 初始 prompt 使用 `--prompt`，不使用 positional 参数；`sai "prompt"` 会把 `prompt`
识别为未知命令，而不是默认 chat 的初始提示词。`--config-dir` 是全局 flag，
可以出现在命令前，也可以出现在命令或子命令后，例如 `sai --config-dir ./config models list`
和 `sai models list --config-dir ./config` 等价。`-h` / `--help` 在命令范围内优先显示 help，且不加载配置。`--` 终止 flag
解析；其后的 token 全部作为 positional，不再被识别为 help、`--config-dir` 或命令参数
flag。

v0.1 不支持会话进行中切换模型。`sai chat` 进入会话后，provider/model 固定到会话
结束。

`sai chat` 使用同一套会话启动时配置与命令行覆盖规则。以下 flags 在 chat 会话开始时
解析一次，并固定到会话结束：`--provider`、`--model`、
`--show-reasoning`、`--verbose`、`--enable-tools`、`--enable-skills`、
`--disable-skills`、`--enable-mcp`、`--save-session`、`--resume` 和
`--continue`。`--resume` 与 `--continue` 互斥。初始输入来源只能是 `--prompt`、
`--stdin` 或 `--file` 三者之一；`--stdin` 和 `--file` 必须和 `--quit` 一起使用，读取
完整 stdin 或文件内容作为一次 prompt，完成该轮后退出。chat 不支持会话进行中切换模型、
工具、MCP 或 skills。

交互式 chat 的普通单行模式支持 `/usage`，用于向 stderr 打印当前 context window 和最近
usage 摘要。该命令不请求 provider、不写 JSONL 日志，也不打印 prompt、assistant output
或 tool result 正文；多行输入块里的 `/usage` 按普通文本发送。

## 配置健康检查

`sai doctor` 用于本地检查配置健康状态，可以和全局 `--config-dir` 混排使用，例如
`sai --config-dir ./config doctor` 或 `sai doctor --config-dir ./config`。它输出简单的
`OK ...`、`WARN ...`、`ERROR ...` 行到 stdout；发现任何 `ERROR` 时退出码为 1，只有
`OK` / `WARN` 时退出码为 0。

检查范围包括：

- 配置根目录和 `sai.yaml` 是否存在且可读。
- provider 文件是否可加载，默认 provider/model 是否能解析。
- 默认 provider 的 API key 是否配置可用：`$ENV_NAME` 会按 `ResolveModel` 的同一逻辑检查
  环境变量是否存在且非空，直接配置 API key 也算通过。
- `skill_dir`、`mcp_dir` 是否可加载，以及 `skills.enabled` / 默认 enabled MCP 是否存在且可选择。
- `tools.enabled` 中的内置工具是否注册；MCP 工具名是否指向已配置且本次默认启用的 MCP server。
- `logging.path` 为空时报告 disabled；非空时检查对应 session root/父目录可创建可写，
  不创建真正的 session log 文件，临时探测文件或目录会清理。

doctor 不发 provider HTTP 请求、不启动 MCP server、不运行模型。输出必须脱敏：不打印
API key、直接 secret 值、环境变量实际值、MCP env 实际值或 Authorization 信息。

## 参数合并

请求参数按以下顺序合并：

1. provider adapter 的安全默认值。
2. model profile 参数。
3. 命令行参数。

命令行参数只覆盖明确传入的值。`api_key` 和其他敏感配置值遵循上述 `$ENV_NAME`
约定；无论实际值来自环境变量还是直接配置，都不能写入 resolved config 输出，也不能
出现在 verbose 日志中。

## 日志

v0.1 使用 JSONL 日志。除此之外，不保存会话历史、上下文快照或其他运行状态。`sai chat`
的多轮 messages 只保存在当前进程内，用于下一轮请求；退出后不写入磁盘。

```yaml
logging:
  path: logs/sai.jsonl
  level: info
```

`logging.path` 兼容旧配置，但运行时解释为日志根/基准路径：如果配置为 `logs/sai.jsonl`，
实际 session root 是配置目录下的 `logs/`；如果配置为空字符串，则禁用日志。每次
`sai chat` 启动 runtime 时会预先确定唯一 session JSONL 路径，供
`--verbose` 的 `log_path` 显示；但 log root、session 目录和 `sai.jsonl` 只在第一条日志
事件发生时创建。

```text
<log-root>/<timestamp>-<short-random>/sai.jsonl
```

`sai config show`、help、list 等不启动 runtime 的命令不会创建 session 日志。chat 启动后
直接 `/exit`、`/quit` 或 EOF 且没有模型请求时，也不创建 session 日志。`--verbose` 中的
`log_path` 显示本次运行实际使用的 session JSONL 文件路径，禁用日志时显示 `(disabled)`。

每行日志是一个 JSON object。日志可以记录模型请求生命周期、工具调用、usage、HTTP 错误
和 MCP 错误。API key、Authorization header 和其他敏感配置值的实际值不能进入日志。
v0.1 不记录完整 prompt、response、tool result 正文，也不提供开启正文日志的配置。

## Context Window（M14 后）

`sai` 在每次 provider 请求前，会基于当前 request messages 和 tool schemas 估算输入
tokens。估算集中在 context window helper 中，当前采用保守字符数估算；provider stream
返回 `model.UsageEvent` 时优先使用真实 usage 更新 tracking，若本次 stream 没有 usage
event，则成功结束后记录 fallback estimate。

当估算输入达到 context window 的 80% 时，CLI 向 stderr 输出一次 warning，只包含 token
数量和窗口大小，不包含 prompt、assistant output、tool result 或 tool schema 正文。估算
输入达到或超过窗口时，`sai` 拒绝发起 provider 请求，并给出可读错误。

M14 的第一版不会自动摘要、截断或丢弃 system/developer messages、tool schemas、tool
results 或历史消息。当前策略是保守保留全部上下文：接近窗口时警告，超预算时拒绝。后续
若加入摘要或截断策略，需要单独设计可解释边界，并用测试证明不会静默丢弃关键指令、工具
schema 或必要 tool result。

## Sessions 配置（M13 后）

M13 后，resumable sessions 是独立于 JSONL 日志的 opt-in 能力。默认关闭：

```yaml
sessions:
  enabled: false
  dir: sessions
  save_tool_results: true
```

`sessions.dir` 相对配置根目录解析，除非显式写成绝对路径。它和 `logging.path` 不同：
`logging.path` 用于 JSONL 事件日志，默认不记录完整 prompt、response 或 tool result；
`sessions.dir` 用于可恢复会话，启用后保存完整上下文，包含完整 messages、assistant
tool calls 和 tool result messages。

可恢复 session 文件会包含敏感数据，包括用户输入、assistant 输出、tool result、cwd、
provider/model/parameters、启用 tools/MCP/skills/reasoning，以及注入指令快照或可重建
信息。因此 `sessions.enabled` 必须默认是 `false`，CLI 和文档都应提示用户这是显式
opt-in 的落盘能力。

命令行也可以显式启用和恢复：

```text
sai chat --save-session --prompt "保存这一轮" --quit
sai chat --resume <id> --prompt "继续这一轮" --quit
sai chat --continue --prompt "继续最近 session" --quit
sai sessions list
sai sessions show <id>
sai sessions delete <id>
sai sessions prune --keep 10
```

启用保存后，`sai chat` 每个成功 turn 都会写入 `sessions.dir/<id>/session.json`，其中
包含完整 updated messages：user messages、assistant final messages、assistant tool
calls 和 tool result messages。`--resume <id>` 会从 `sessions.dir/<id>/session.json`
恢复，`--continue` 会选择 `sessions.dir` 下 `updated_at` 最新的 session。

恢复时，runtime 使用 session 文件中保存的 provider、model profile、model id、model
parameters、enabled tools、enabled MCP、enabled skills、show_reasoning 和保存行为。显式
CLI 覆盖如果和 session 文件冲突会失败，例如恢复时同时传入不同的 `--model`、不同的
`--enable-tools`、冲突的 `--show-reasoning=true/false`，或试图用 `--save-session=false`
改变恢复后的保存语义。`sessions.save_tool_results: false` 当前不提供可靠降级模式；只要启用保存或恢复，
CLI 会拒绝继续并提示必须设为 `true`。

本次 chat 如果会保存完整敏感 session，首次敏感数据提示会在 CLI runtime 准备完成后、读取
用户输入或发起 provider 请求前输出，并且整个进程只输出一次。提示不包含 prompt、assistant
output、tool result 或注入指令正文。

session 管理命令使用解析后的 `sessions.dir`，不要求 `sessions.enabled: true`，因此可以在
关闭自动保存时查看或清理已有文件。`sai sessions list` 只列出 ID、更新时间、provider 和
model/profile 等元数据。`sai sessions show <id>` 只展示元数据，并提示 session 文件包含
完整 prompt、assistant 输出和 tool result 等敏感内容；它不会打印完整 messages、tool
result 正文、prompt 正文或注入指令正文。M14 后，session 文件还保存 context management
metadata，例如 context window、来源、warning 阈值和最近 usage 统计；`show` 只展示这些
数字和 source，不展示正文。`sai sessions delete <id>` 删除指定 session；
不存在时给出可读错误。`sai sessions prune --keep N` 保留 `updated_at` 最新的 N 个
session，删除更旧的 session；`--keep` 必须显式提供，N 必须大于等于 0。
