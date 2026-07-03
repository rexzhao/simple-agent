# 配置设计

配置优先使用 YAML。第一阶段目标是让 provider 和 model profile 的边界清晰，避免把
所有服务和模型参数塞进一个大文件。

## 配置位置

`sai` 通过一个具体的根 YAML 配置文件启动配置解析：

1. 如果传入 `--config <file>`，使用该文件作为根配置文件。
2. 如果省略 `--config`，默认使用启动时当前工作目录下的 `.agents/${arg[0]}.yaml`。
   `${arg[0]}` 是可执行文件 basename；普通 `sai` 二进制默认读取 `.agents/sai.yaml`。

暂时不读取用户目录，也不向用户目录写入默认配置。

项目指令文件的配置入口是根配置里的 `agent.instruction_files`。省略该字段时保持当前
兼容行为，等价于只读取启动时当前工作目录下的 `$CWD/AGENTS.md`；`--config` 不改变
`$CWD` 的含义。

```text
sai --prompt "你好" --quit
sai --stdin --quit
sai --config ./config/sai.yaml --prompt "你好" --quit
sai chat --prompt "你好" --quit
sai chat --stdin --quit
sai chat --file prompt.md --quit
sai --config ./config/sai.yaml chat --prompt "你好" --quit
sai --config ./examples/paperhub/sai.yaml chat
sai config show --config ./config/sai.yaml
sai models list --config ./config/sai.yaml
sai mcp list --config ./config/sai.yaml
sai sessions list --config ./config/sai.yaml
sai doctor --config ./config/sai.yaml
```

## 文件布局

推荐布局：

```text
AGENTS.md  # 省略 agent.instruction_files 时的兼容默认项目指令文件
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
  subagents/
    reviewer.yaml
  mcp/
    local.yaml
  logs/
    20260702T030405.000000000Z-a1b2c3d4/
      sai.jsonl
  sessions/
    <session-id>/
      session.json
```

`.agents/sai.yaml` 是普通 `sai` 二进制的默认根配置文件。通过 `--config` 指定时，
所选文件本身就是入口，不再在目录下额外拼接固定的 `sai.yaml`。根配置文件负责默认
provider、默认 model、provider 目录、工具启用和 agent 通用参数。`providers/*.yaml`
每个文件描述一个 provider。默认 `skill_dirs: [skills]` 会扫描
`skills/<skill_id>/SKILL.md` 这种 M7 后的本地 skill 推荐布局；直接子目录下存在
`SKILL.md` 的本地 skill 默认会注入运行时，除非该文件的 frontmatter 设置
`disable-model-invocation: true`。
`mcp/*.yaml` 每个文件描述一个 MCP server；
MCP 是 M4 后能力，不属于 MVP 必需配置。
M19 规划中的 async subagents 可放在 `subagents/` 目录；该目录只是推荐组织方式，真正入口由
根配置文件的 `subagents` 映射决定。

根配置文件内的相对路径都基于该文件所在目录解析。对根配置文件来说，这包括
`provider_dir`、`auth_dir`、`skill_dirs`、`mcp_dir`、`logging.path` 和
`sessions.dir`。二级配置文件继续使用同一原则：相对路径基于写出该路径的 YAML 文件所在
目录解析，例如 provider 的 `auth_file` 相对 provider YAML 文件解析。
省略 `agent.instruction_files` 时，项目指令文件仍按兼容默认从启动时当前工作目录读取
`AGENTS.md`。

## 全局配置

```yaml
# sai.yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers
auth_dir: auth
skill_dirs: [skills]

# M19 规划，尚未实现
subagents:
  reviewer: subagents/reviewer.yaml

agent:
  # 省略时等价于 ["$CWD/AGENTS.md"]
  instruction_files:
    - $CWD/AGENTS.md
  max_turns: 8
  stream: true
  show_reasoning: false

tools:
  enabled: []

logging:
  path: logs/sai.jsonl
  level: info

# M13 后可选启用
sessions:
  enabled: false
  dir: sessions
  save_tool_results: true

# 会话压缩；默认关闭
compaction:
  enabled: false
  threshold_percent: 80
  summary_provider: ""
  summary_model: ""

# M4 后启用
mcp_dir: mcp
```

字段说明：

- `default_provider`：未通过命令行指定 provider 时使用。
- `default_model`：未通过命令行指定 model 时使用。
- `provider_dir`：provider 配置文件目录。相对路径基于根配置文件所在目录解析。
- `auth_dir`：M16 后的 OAuth token 文件目录。默认 `auth`，相对路径基于根配置文件所在目录解析。
- `skill_dirs`：本地 skill 目录列表。默认等价于 `[skills]`；相对路径基于根配置文件所在目录解析，
  绝对路径保持不变。空列表 `[]` 表示不加载本地 skills。多个目录按配置顺序扫描，每个
  目录只发现其直接子目录中包含 `SKILL.md` 的本地 skills；目录内使用确定性 discovery
  顺序，跨目录保留配置顺序。重复 skill id 是配置错误。
- `subagents`：M19 规划的 async subagent 配置映射，尚未实现。key 是 parent agent 可引用的
  subagent id，value 是该 subagent 的 `sai` config 文件路径；相对路径基于写出该配置项的
  父配置文件所在目录解析。字段缺失或为空时，不向 parent agent 暴露任何 subagent tools。
- `mcp_dir`：MCP 配置文件目录。M4 后启用；相对路径基于根配置文件所在目录解析。
- `agent.max_turns`：一次 agent loop 最多请求模型的轮数。
- `agent.instruction_files`：项目指令文件列表；省略时等价于
  `["$CWD/AGENTS.md"]`。
- `agent.stream`：默认是否启用 streaming。
- `agent.show_reasoning`：默认是否显示 reasoning stream。
  普通新 chat 中可用 `--show-reasoning=true/false` 显式覆盖配置；`--show-reasoning`
  等价于 `--show-reasoning=true`。
- `tools.enabled`：默认启用的工具列表。空列表表示不向模型暴露工具。
- `logging.path`：JSONL 日志根/基准路径。相对路径基于根配置文件所在目录解析；例如
  `logs/sai.jsonl` 使用 `logs/` 作为 session root。空字符串表示禁用日志。
- `logging.level`：日志级别。
- `sessions.enabled`：M13 后的可恢复 session 开关。默认 `false`，不保存完整上下文。
  普通新 chat 中可用 `--save-session=true/false` 显式覆盖配置；`--save-session`
  等价于 `--save-session=true`。
- `sessions.dir`：M13 后的可恢复 session 存储目录。相对路径基于根配置文件所在目录解析。
- `sessions.save_tool_results`：M13 后启用 session 保存时是否保存完整 tool result messages。
  可靠 resume 需要保存 tool results；关闭后只能作为降级或诊断模式设计。
- `compaction.enabled`：会话压缩开关。默认 `false`；只有当前 chat 正在保存或恢复可恢复
  session 时才有意义。关闭时不执行 pre-turn 自动压缩，普通 REPL 中的 `/compact` 应给出
  可读错误。
- `compaction.threshold_percent`：pre-turn 自动压缩阈值，默认 `80`，表示估算值超过当前
  context window 的 80% 时先尝试压缩。
- `compaction.summary_provider`：summary 请求使用的 provider。空字符串表示使用当前
  session provider。
- `compaction.summary_model`：summary 请求使用的 model profile。空字符串表示使用当前
  session model profile；只配置 `summary_model` 时，在当前 provider 下解析该 profile。

## 项目指令文件配置

根配置字段 `agent.instruction_files` 用于配置项目指令文件列表。省略该字段时保持
当前行为，等价于：

```yaml
agent:
  instruction_files:
    - $CWD/AGENTS.md
```

列表条目按配置顺序处理。条目可以指向任意文件名，不限于 `AGENTS.md`；建议使用
placeholder 明确路径基准：

- `$CWD`：启动时当前工作目录。
- `$CONFIG`：根配置文件所在目录。
- `$USER`：用户 home 目录。
- `$REPO`：从 `$CWD` 向上查找得到的 git repository root。

如果某个条目使用 `$REPO` 但无法从 `$CWD` 解析出 git repository root，该条目会被跳过并
产生 warning。warning 只进入终端或诊断输出，不进入模型上下文。

不存在的非 glob 文件按当前缺失 `AGENTS.md` 的行为跳过，不作为错误。glob 条目支持普通
glob pattern，也支持 `**/*.md` 形式的递归 pattern。同一个 pattern 匹配到多个文件时，
这些匹配文件在该 pattern 内按稳定 path sort 顺序加载；pattern 之间仍保留列表顺序。

完成 placeholder 展开和 glob 匹配后，`sai` 使用解析后的 canonical/clean 绝对文件路径判断
是否为同一个实际文件。重复指向同一文件时只保留第一次出现的加载项；第一次出现按列表顺序优先，
同一个 glob pattern 内按稳定 path sort 顺序判断。例如 cwd 就是 repository root 时，
`$REPO/AGENTS.md` 和 `$CWD/AGENTS.md` 解析到同一路径，只会加载一次。后续重复匹配静默跳过，
不产生 warning；`$REPO` 无法解析时跳过条目并输出 warning 的既有行为不变。

成功加载且去重后的文件注入在同一个项目指令位置：位于 `sai` 内置基础约束之后、loaded skills
和当前用户 prompt 之前。多个文件应优先作为多个独立 developer instruction source/message
注入，并保留各自文件来源，便于后续 session snapshot 或可重建信息记录到单个文件粒度。

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
    parameters:
      store: false
      reasoning:
        effort: high
```

`sai auth codex login --provider codex-work` 使用 Codex headless device flow 登录，并生成
`providers/codex-work.yaml` 与 `auth/codex-work.json`。生成的 `openai-codex` model profile
默认在 `parameters` 中写入 `store: false` 和 `reasoning.effort: high`；这是普通
Responses 请求参数透传，不新增顶层配置字段，字段名是 `effort` 而不是 `effect`。默认
provider 名称是 `codex`。
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
Codex 后端请求必须显式发送 `store: false`；这是 `openai-codex` runtime/request 层要求，
不是 API key 或 OAuth token 配置。M16 不读取、不迁移、不导入 `~/.codex/auth.json`。

`api_key` 是这次 provider 配置中的具体字段。其他需要脱敏的敏感配置值也可以采用同样
约定：字符串以 `$` 开头时，`$` 后面的内容作为环境变量名读取实际值；不以 `$` 开头时
表示直接配置值。这个解析发生在配置读取阶段；provider adapter 接收解析后的实际值。
除非某个 adapter 明确声明协议特定的默认环境变量，否则 adapter 不承担通用环境变量解析
职责。无论实际值来自环境变量还是直接配置，logs、verbose、resolved config 等输出都
不能泄露实际值。

## Skills 配置（M7 后）

本地 skill 目录由根配置文件的 `skill_dirs` 指定，默认等价于 `skill_dirs: [skills]`。
每个条目相对根配置文件所在目录解析，除非显式写成绝对路径；如果要关闭全部本地 skill 加载，
使用 `skill_dirs: []`。本项目推荐每个 skill 使用一个直接子目录，并在子目录下放置
`SKILL.md`：

```text
.agents/
  skills/
    my-skill/
      SKILL.md
```

可以配置多个目录：

```yaml
skill_dirs: [skills, team-skills, /opt/sai/skills]
```

`sai` 按配置顺序扫描这些目录。每个目录只发现直接子目录中包含 `SKILL.md` 的 skills，
不递归读取；同一目录内按确定性 discovery 顺序加载，跨目录保留配置的目录顺序。不同
skill 目录中出现重复 skill id 会作为配置错误处理。

`SKILL.md` 可以使用可选 YAML frontmatter：

```markdown
---
name: my-skill
description: short text
---
body...
```

如果没有 frontmatter，skill 名称默认使用目录名，description 为空，正文全文作为
instructions。`disable-model-invocation: true` 是每个 skill 的模型上下文注入 opt-out：
设置为 `true` 时，该 skill 会被发现但不会作为 developer message 注入模型上下文。

```markdown
---
disable-model-invocation: true
---
```

缺失该字段或设置为 `false` 都表示正常加载并注入。

根配置文件不配置 skill 选择或启用列表；是否注入某个本地 skill 由其 `SKILL.md`
frontmatter 决定。

已加载 skill 的 instructions 会作为 developer message 注入到模型请求中，顺序是：

```text
sai 内置基础约束 > project instruction files > loaded skills > 当前用户 prompt
```

多个 skill 使用 skill 目录顺序和目录内确定性 discovery 顺序注入。frontmatter 格式错误会让
命令失败并指出对应 `SKILL.md`。

## Async Subagents 配置（M19 规划）

async subagent runtime 尚未实现；本节记录目标配置形态，避免实现阶段引入临时 schema。
parent config 使用简单映射声明可用 subagents：

```yaml
subagents:
  reviewer: subagents/reviewer.yaml
  researcher: ../shared-agents/researcher.yaml
```

`reviewer`、`researcher` 是注入 parent prompt 并用于 subagent tools 的 id。右侧路径指向
完整的 `sai` 配置文件；相对路径基于 parent config 文件所在目录解析，而不是启动目录。配置
可以指向当前文件本身，也可以间接形成环，但 runtime 必须用最大递归深度、最大并发/累计 job
数量、job timeout 和取消机制防止无限递归或资源耗尽。

subagent config 文件使用和 main config 相同的 schema，例如：

```yaml
# subagents/reviewer.yaml
default_provider: codex-work
default_model: gpt-5.5
provider_dir: ../providers
auth_dir: ../auth
skill_dirs: [reviewer-skills]

agent:
  description: Read-only reviewer for scoped code and docs changes.
  max_turns: 6
  stream: true

tools:
  enabled:
    - read_file
    - list_files

prompt:
  system_prompt: |
    Review only the assigned scope and report findings first.
```

child agent 的 provider、model、tools、skills、MCP 和 prompt 都由 child config 决定，不继承
parent 的工具列表。child config 中的相对路径继续基于 child config 文件所在目录解析。

配置了 subagents 后，parent runtime 自动获得 subagent tools；未配置或配置为空时，不暴露
任何 subagent tools。parent prompt 会注入已配置 subagent 的 id 和短 description。短
description 计划来自 child config 的 `agent.description`；缺失时实现应使用空描述或清晰的
默认描述，而不是读取 prompt 正文。

`subagent_start` 还应允许调用方为单个 job 提供可选 display name / job name。这个名称是运行时
metadata，不是配置选择器：配置的 subagent id 仍决定使用哪个 child config、权限和工具集合。
display name 应出现在 status、mailbox 和 observability 输出中，并为未来 GUI label 提供友好
名称；它不能选择未配置的 agent，也不能覆盖 child config、tools、skills、MCP 或 prompt。
`subagent_close` 用于释放已经结束且完成通知已被消费的 job 记录；仍在运行的 job 需要先用
`subagent_cancel` 取消，避免 close 隐式丢弃仍可能回传的结果。

## Prompt 配置（规划）

未来 config schema 可以增加 prompt 配置，用于 main agent 和 subagent：

```yaml
prompt:
  system_prompt: |
    Prefer concise answers and mention uncertainty.
```

`prompt.system_prompt` 是追加内容，不替换 `sai` 内置基础约束，也不能削弱工具、安全、日志或
权限边界。prompt 注入顺序仍应保持内置约束优先；自定义 system prompt 只是在内置约束之后
追加本 agent 的额外行为说明。

placeholder 也属于未来 schema，但必须来自固定白名单，例如由实现明确允许的 agent id、cwd、
config dir 或 subagent 列表等值。placeholder 不支持任意表达式求值、任意环境变量展开、文件
读取或 shell 执行。

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
glob_files
grep_files
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
    - glob_files
    - grep_files
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
arguments；`subagent_start` 状态显示 subagent id 和可选 display/job name，但不显示
prompt 正文。状态也不包含 tool result 正文，stdout 仍只输出模型文本。

`shell` 工具默认在启动目录执行命令。v0.1 不提供 `--workdir` 配置。

M17 的本地工具易用性任务不新增工具启用配置字段；新增或增强的工具仍通过
`tools.enabled` 或 `--enable-tools` 暴露给模型，默认关闭。该任务计划增加
`glob_files` 和 `grep_files`，并增强 `read_file` 与 `shell`：

- `read_file` 继续只允许读取工作区内文本文件。它支持可选 `start_line`（1-based）、
  `line_count`（大于 0）和 `max_bytes`（大于 0），不支持 byte offset / byte count。
  `max_bytes` 同时限制默认全文件读取和行范围读取；默认读取从文件开头开始，最多返回
  `max_bytes`。只提供 `start_line` 时，从该行读取到 `max_bytes` 或 EOF；只提供
  `line_count` 时，从第 1 行读取指定行数；同时提供 `start_line` 和 `line_count` 时，
  最多返回指定行数且仍受 `max_bytes` 限制。
- `read_file` 对任何行范围读取，或任何因 `max_bytes` 返回不完整的读取，都必须在正文前
  添加简短 metadata，至少包含 path、有效 `start_line`、`lines_returned`、`max_bytes` 和
  `truncated=true/false`。截断时还必须包含下一步读取建议。行范围读取尽量返回完整行；
  若单行超过 `max_bytes`，返回该行前缀、标记 `line_truncated=true`，并提示增大
  `max_bytes` 后从同一行重试。小的、非范围且未截断的完整读取可以继续返回原始文件内容以保持兼容。
- `glob_files` 在工作区内执行 glob，返回稳定相对路径，并用 `max_results` 与截断
  metadata 控制大结果集。
- `grep_files` 在工作区内执行文本搜索，支持 include / exclude globs；默认 literal 搜索，
  可选 regex、大小写敏感和 context lines；支持 `max_results` 与 snippet limits，并在
  结果或片段截断时返回明确 metadata。
- `shell` 支持可选 `timeout_ms` 和 `max_output_bytes`；输出截断必须显式说明，状态行仍不
  展示命令参数。

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
2. 所选根配置文件默认值。
3. 如果仍无法确定，打印可选 provider/model 并停止。

根配置文件选择优先级：

1. `--config <file>`。
2. 启动时当前工作目录下的 `.agents/${arg[0]}.yaml`。

根层解析从 argv 左到右扫描，跳过已知 flag 及其 value；第一个真正的非 flag token 是命令。
没有命令 token 时默认执行 `chat`，并把已扫描到的 chat flags 交给 `chat` 解析，例如
`sai --model fast --prompt "hi" --quit` 等价于 `sai chat --model fast --prompt "hi" --quit`。
带值 flag 的 value 不参与命令识别，因此 `sai --model fast chat --prompt "hi" --quit` 中的
`fast` 不是命令。命令 token 之外的参数交给对应命令解析，命令前后的 flags 可以混排；
chat 初始 prompt 使用 `--prompt`，不使用 positional 参数；`sai "prompt"` 会把 `prompt`
识别为未知命令，而不是默认 chat 的初始提示词。`--config` 是全局 flag，
可以出现在命令前，也可以出现在命令或子命令后，例如 `sai --config ./config/sai.yaml models list`
和 `sai models list --config ./config/sai.yaml` 等价。`-h` / `--help` 在命令范围内优先显示 help，且不加载配置。`--` 终止 flag
解析；其后的 token 全部作为 positional，不再被识别为 help、`--config` 或命令参数
flag。

v0.1 不支持会话进行中切换模型。`sai chat` 进入会话后，provider/model 固定到会话
结束。

`sai chat` 使用同一套会话启动时配置与命令行覆盖规则。以下 flags 在 chat 会话开始时
解析一次，并固定到会话结束：`--provider`、`--model`、
`--show-reasoning`、`--verbose`、`--enable-tools`、`--enable-mcp`、`--save-session`、
`--resume` 和
`--continue`。`--resume` 与 `--continue` 互斥。初始输入来源只能是 `--prompt`、
`--stdin` 或 `--file` 三者之一；`--stdin` 和 `--file` 必须和 `--quit` 一起使用，读取
完整 stdin 或文件内容作为一次 prompt，完成该轮后退出。chat 不支持会话进行中切换模型、
工具、MCP 或 skills。

交互式 chat 的普通单行模式支持 `/usage`，用于向 stderr 打印当前 context window 和最近
usage 摘要。该命令不请求 provider、不写 JSONL 日志，也不打印 prompt、assistant output
或 tool result 正文；多行输入块里的 `/usage` 按普通文本发送。

## 配置健康检查

`sai doctor` 用于本地检查配置健康状态，可以和全局 `--config` 混排使用，例如
`sai --config ./config/sai.yaml doctor` 或 `sai doctor --config ./config/sai.yaml`。它输出简单的
`OK ...`、`WARN ...`、`ERROR ...` 行到 stdout；发现任何 `ERROR` 时退出码为 1，只有
`OK` / `WARN` 时退出码为 0。

检查范围包括：

- 所选根配置文件是否存在且可读。
- provider 文件是否可加载，默认 provider/model 是否能解析。
- 默认 provider 的 API key 是否配置可用：`$ENV_NAME` 会按 `ResolveModel` 的同一逻辑检查
  环境变量是否存在且非空，直接配置 API key 也算通过。
- `skill_dirs`、`mcp_dir` 是否可加载，discovered skills 是否可解析，重复 skill id 是否报错，
  以及默认 enabled MCP 是否存在且可选择。
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
实际 session root 是根配置文件所在目录下的 `logs/`；如果配置为空字符串，则禁用日志。每次
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
results 或历史消息。当前策略是保守保留全部上下文：接近窗口时警告，超预算时拒绝。会话
压缩是后续独立能力，按 `compaction` 配置和 `docs/session-compaction.md` 的边界执行，仍
必须证明不会静默丢弃关键指令、工具 schema 或必要 tool result。

## Sessions 配置（M13 后）

M13 后，resumable sessions 是独立于 JSONL 日志的 opt-in 能力。默认关闭：

```yaml
sessions:
  enabled: false
  dir: sessions
  save_tool_results: true
```

`sessions.dir` 相对根配置文件所在目录解析，除非显式写成绝对路径。它和 `logging.path` 不同：
`logging.path` 用于 JSONL 事件日志，默认不记录完整 prompt、response 或 tool result；
`sessions.dir` 用于可恢复会话，启用后保存完整上下文，包含完整 messages、assistant
tool calls 和 tool result messages。

可恢复 session 文件会包含敏感数据，包括用户输入、assistant 输出、tool result、cwd、
provider/model/parameters、启用 tools/MCP、loaded skills、reasoning，以及注入指令快照或可重建
信息。M18 后，如果记录项目指令文件快照或可重建信息，应按每个成功加载的文件保留独立
source/message 粒度。因此 `sessions.enabled` 必须默认是 `false`，CLI 和文档都应提示用户这是显式
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

启用保存后，当前 M13 runtime 会在每个成功 turn 后写入
`sessions.dir/<id>/session.json`，其中包含完整 updated messages：user messages、
assistant final messages、assistant tool calls 和 tool result messages。`--resume <id>`
会从该 session 文件恢复，`--continue` 会选择 `sessions.dir` 下 `updated_at` 最新的
session。会话压缩实现目标会在此基础上使用 v2 session store：完整事实写入 append-only
`Items`，模型可见上下文由 `ActiveHistory` item id 列表表示，resume 从 `ActiveHistory`
materialize provider messages。

恢复时，runtime 使用 session metadata 中保存的 provider、model profile、model id、model
parameters、enabled tools、enabled MCP、loaded skills、show_reasoning 和保存行为。显式
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

## 会话压缩配置

详细设计来源是 `docs/session-compaction.md`；本节只记录公开配置形态和运行时边界。

```yaml
compaction:
  enabled: false
  threshold_percent: 80
  summary_provider: ""
  summary_model: ""
```

`compaction.enabled` 默认关闭。启用后，普通单行 REPL 中的 `/compact` 只执行一次压缩，
不发起用户 turn；多行输入里的 `/compact` 继续作为普通文本发送。手动压缩不受自动阈值
限制。成功时追加 hidden/model-facing summary item、compaction checkpoint 和
`active_history.replaced`，再替换内存中的 `ActiveHistory`；失败时向用户报错，session
状态和内存 `ActiveHistory` 都不变。

pre-turn 自动压缩在新用户消息真正加入 session 前执行检查，估算
`ActiveHistory + pending user message + tool schemas`。估算超过
`compaction.threshold_percent` 时，先执行 compact；compact 成功后才追加 pending user
message 并请求主模型。compact 失败时，本轮直接失败，不保存 pending user message，不更新
`ActiveHistory`，也不请求主模型。

summary 请求默认使用当前 session provider/model profile。`summary_provider` 和
`summary_model` 都为空时使用当前 session 选择；只配置 `summary_model` 时，在当前
provider 下解析该 profile；二者都配置时使用指定 provider/profile。summary 请求不传 tool
schemas，不允许 tool call，不进入 agent loop，只执行一次 summarization lifecycle。未来的
session-history 查询工具仍是 out of scope。
