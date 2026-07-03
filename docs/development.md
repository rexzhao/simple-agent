# 开发说明

这个项目是一个简易的纯命令行 agent runner，命令名为 `sai`
（Simple Agent Interface）。MVP 优先把核心闭环做稳：
命令行输入、OpenAI-compatible streaming、tool call、打包和基础可用性。不要引入 TUI，
也不要提前实现还没有进入第一阶段的扩展能力。

## 目标

- 提供一个纯 CLI 的本地 agent 工具。
- 第一优先级支持 OpenAI-compatible Chat Completions。
- 支持 streaming 输出。
- 支持模型发起 tool call。
- MVP 后支持通过 MCP stdio server 暴露工具。
- 发布形态尽量保持为单文件可执行程序。

## 第一阶段假设

- 使用 Go 实现，目标是单文件可执行程序。
- 第一种模型协议是 OpenAI-compatible `/v1/chat/completions`。
- 第一阶段真实测试服务使用 PaperHub：
  - Base URL: `https://tc-paperhub.diezhi.net/v1`
  - Model: `glm-5.2`
  - API key 配置: `api_key: $PAPERHUB_API_KEY`
- skills 是后续开发项，v0.1 不作为 MVP 核心。M7 使用 `skill_dirs` 配置的本地目录
  发现直接子目录中的 `SKILL.md`，并支持 frontmatter opt-out 和 instruction composition；不读取用户目录，
  不实现 marketplace、递归 skill discovery 或复杂依赖解析。
- M5 已为 Anthropic Messages 添加 model profile type 配置识别、示例、文本 streaming
  和 tool use runtime adapter。
- M6 已为 OpenAI Responses 添加 model profile type 配置识别、文本 input mapping、
  semantic text streaming adapter，以及 function tools / function_call_output tool loop adapter。
- M16 为 Codex subscription auth 增加独立的 OAuth token 文件和 `openai-codex`
  model profile type；运行时复用 OpenAI Responses request / SSE / tool-call mapping，
  但用 provider 的 `auth_file` 读取和刷新 bearer token，而不是读取 `api_key`。
- MCP 不属于 MVP 核心；后续接入时第一种传输只做 stdio。
- 根配置文件默认是启动时当前工作目录下的 `.agents/${arg[0]}.yaml`，其中 `${arg[0]}`
  是可执行文件 basename；普通 `sai` 二进制默认读取 `.agents/sai.yaml`。也可以通过
  `--config <file>` 显式指定根配置文件。
- 默认读取启动目录下的 `AGENTS.md` 作为项目指令；文件不存在时继续执行。
- 只落盘 JSONL 日志；会话历史、上下文快照和其他状态暂不落盘。

## v0.1 非目标

- TUI 或全屏终端界面。
- Markdown 渲染。
- 浏览器自动化。
- multi-agent 编排。
- 长期记忆。
- skill 加载。
- OpenAI Responses custom tools、built-in web/code tools、stateful `previous_response_id`
  对话续写，以及 reasoning output item passthrough。
- MCP stdio。
- remote MCP over HTTP/SSE。
- 插件市场或插件生命周期管理。

## 后续路线边界

M11-M15 继续沿用纯 CLI 的产品形态，优先补可靠性、编辑工具、可恢复 session、上下文
窗口管理、输入体验和配置诊断。后续路线不改变 v0.1 的已完成边界：当前版本仍只落
JSONL 日志，不保存完整会话上下文、prompt、response 或 tool result 正文。

可恢复 session 是 M13 之后的独立能力，不是 JSONL 日志的增强开关。它默认关闭；只有用户
通过配置或命令显式启用后，才会保存完整 messages、assistant tool calls 和 tool result
messages，以及 provider/model/parameters、cwd、enabled tools/MCP、loaded skills、reasoning 和
注入指令快照或可重建信息。只有保存这些完整上下文，`resume` 才能可靠，而这也意味着
session 文件会包含敏感数据。

`sai run` 不回归。后续 stdin、file 和 multiline 输入都属于 `sai chat --quit` 或
`sai chat` 的输入能力，复用同一套 provider 选择、message 构造、工具启用、日志和错误
处理路径。

Markdown 渲染不是近期目标；如果后续需要，也应作为远期低优先级能力单独设计，不进入
下一阶段里程碑。

## 架构

核心原则：agent loop 使用项目内部事件，不直接依赖任何厂商的原始返回格式。

```text
cmd/sai
  CLI 命令和终端输出

internal/config
  YAML 配置加载、敏感配置值解析、provider/model profile

internal/agent
  对话循环、max turns、tool 执行循环

internal/context
  项目上下文加载，例如 AGENTS.md

internal/model
  provider interface 和统一 stream event 类型

internal/model/openai_chat
  OpenAI-compatible Chat Completions adapter

internal/model/anthropic_messages
  Anthropic Messages streaming and tool use adapter

internal/model/openai_responses
  OpenAI Responses text streaming and function tool adapter

internal/tools
  内置工具定义和执行

internal/mcp
  MCP stdio client 和 MCP tool adapter，MVP 后实现

internal/skills
  本地 skill 目录发现、SKILL.md frontmatter opt-out 和 instruction composition

internal/logging
  JSONL event log
```

统一事件建议先保持小而够用：

```text
text_delta
reasoning_delta
message_done
tool_call_delta
tool_call_done
tool_result
usage
error
```

CLI 默认只打印 `text_delta`。`reasoning_delta` 默认隐藏，后续通过
`--show-reasoning` 显示。发生 tool call 时，CLI 默认向 stderr 打印独立的简短工具状态
（例如 `tool: read_file docs/notes.md`），不需要 `--verbose`；`read_file`、`write_file`、
`edit_file` 和 `list_files` 状态可以显示目标路径/目录，其中 `list_files` 未提供 path
时显示 `.`。`shell` 和 MCP tool 状态只显示工具名，不显示命令参数或任意 arguments，
也不打印 tool result 正文，stdout 仍只承载模型可见输出。

## Agent Loop

第一版只需要一个串行 tool loop：

1. 将 messages 和可用 tools 发送给模型。
2. 将 assistant 文本流式输出到终端。
3. 累积 tool call delta，直到每个 tool call 完整。
4. 执行 tool call。
5. 将 tool result 追加回 messages。
6. 继续请求模型。
7. 当模型没有继续请求 tool call，或达到 `max_turns` 时结束。

v0.1 不需要并发执行 tool call。等真实场景需要时再加。

## CLI 形态

目标命令：

```text
sai
sai [--prompt "prompt"]
sai --stdin --quit
sai --prompt "prompt" --quit
sai chat [--prompt "prompt"]
sai chat --prompt "prompt" --quit
sai chat --stdin --quit
sai chat --file prompt.md --quit
sai chat --save-session --prompt "prompt" --quit
sai chat --resume <id> --prompt "prompt" --quit
sai chat --continue --prompt "prompt" --quit
sai sessions list
sai sessions show <id>
sai sessions delete <id>
sai sessions prune --keep 10
sai auth codex login --provider codex-work
sai tools list
sai models list
sai config show
sai doctor
sai mcp list  # M4
```

后续 stdin/file 输入也继续收敛到 `sai chat --quit`，不新增或恢复 `sai run` 入口。

Help/usage 是普通 CLI 行为，不引入 TUI 或第三方 CLI 框架。支持：

```text
sai -h
sai --help
sai help
sai version -h
sai help version
sai chat -h
sai help chat
sai config -h
sai help config
sai config show -h
sai help config show
sai models -h
sai help models
sai models list -h
sai help models list
sai doctor -h
sai help doctor
sai tools -h
sai help tools
sai tools list -h
sai help tools list
sai mcp -h
sai help mcp
sai mcp list -h
sai help mcp list
sai sessions -h
sai help sessions
sai sessions list -h
sai help sessions list
sai sessions show -h
sai help sessions show
sai sessions delete -h
sai help sessions delete
sai sessions prune -h
sai help sessions prune
```

help 输出写到 stdout，exit code 为 0。help 必须在配置加载前完成：不读取 `.agents`
配置、不解析 provider API key、不读取 skills 或 MCP 配置，也不能打印任何配置值
或 secrets。未知命令和错误参数仍以 exit code 1 失败，并给出可读错误和类似
`Run "sai help" for usage.` 的提示。

`sai doctor` 是本地配置健康检查命令。`sai doctor -h`、`sai doctor --help` 和
`sai help doctor` 只显示 help，不加载配置。真正执行检查时，它读取所选根配置文件、
provider 文件、`skill_dirs`、MCP 配置和 enabled tools，用简单的
`OK ...`、`WARN ...`、`ERROR ...` 行写到 stdout；只要出现 `ERROR`，退出码就是 1。
doctor 不发起 provider HTTP 请求、不启动 MCP server、不运行模型，也不能打印 API key、
直接 secret 值、环境变量实际值、MCP env 实际值或 Authorization 信息。日志检查只验证
`logging.path` 对应的 session root/父目录可创建可写，不创建真正的 session log 文件。

根层解析从 argv 左到右扫描，跳过已知 flag 及其 value；第一个真正的非 flag token 是命令。
没有命令 token 时默认执行 `chat`，并把已扫描到的 chat flags 交给 `chat` 解析，因此
`sai --model fast --prompt "hi" --quit` 等价于 `sai chat --model fast --prompt "hi" --quit`。
带值 flag 的 value 不参与命令识别，因此 `sai --model fast chat --prompt "hi" --quit` 中的
`fast` 不是命令。命令 token 之外的参数交给对应命令解析，命令前后的 flags 可以混排；
chat 初始 prompt 使用 `--prompt`，不使用 positional 参数；`sai "prompt"` 会把 `prompt`
识别为未知命令，而不是默认 chat 的初始提示词。全局 `--config <file>` 也可以放在
命令后，例如 `sai models list --config ./config/sai.yaml` 或
`sai chat --prompt "hi" --config ./config/sai.yaml --quit`。`-h` / `--help` 在命令范围内
优先显示 help，且不加载配置。`--` 终止 flag 解析；其后的 token 全部作为 positional，
不再被识别为 help、`--config` 或命令参数 flag。

常用参数：

```text
--provider paperhub
--model glm-5.2
--config ./config/sai.yaml
--base-url https://tc-paperhub.diezhi.net/v1
--show-reasoning
--max-turns 8
--enable-tools read_file,list_files,shell
--verbose
--quit
--enable-mcp local  # M4
--stdin
--file prompt.md
```

v0.1 暂不支持 non-streaming fallback，`sai chat` 当前强制使用 streaming；后续如要支持再引入 `--no-stream` 或 adapter 非流式路径。

## CLI Chat REPL

M10 后，`sai chat` 是一个克制的逐行 REPL：启动时固定 provider、model、tools、MCP、
skills 和 reasoning 展示设置；会话进行中不支持模型切换或额外 slash commands。它支持
可选初始 prompt 和 `--quit`：

```text
sai
sai --prompt "只跑这一轮然后退出" --quit
sai --stdin --quit
sai --provider paperhub --model glm-5.2
sai chat --prompt "先回答这个问题，然后进入 REPL"
sai chat --prompt "只跑这一轮然后退出" --quit
sai chat --stdin --quit
sai chat --file prompt.md --quit
sai chat --provider paperhub --model glm-5.2
sai chat --enable-tools list_files,read_file
sai chat --enable-mcp local --enable-tools mcp.local.some_tool
sai chat --save-session --prompt "保存这一轮" --quit
sai chat --resume 20260702T030405.000000000Z-a1b2c3d4 --prompt "继续" --quit
sai chat --continue --prompt "继续最近会话" --quit
```

无初始 prompt / stdin / file 输入时，交互式输入从 stdin 逐行读取，空白行忽略。
一行只包含 `"""` 时进入多行收集模式，再遇到一行只包含 `"""` 时结束，并把中间内容
按原样保留换行作为一条 user message；空的多行内容忽略。`/exit`、`/quit` 或 EOF
正常退出，但退出命令只在普通单行模式生效，多行内容里的 `/exit` 和 `/quit` 按普通文本
发送。普通单行模式中输入 `/usage` 不发送给模型，而是向 stderr 打印当前 context window
和最近 usage 摘要：context window、来源、warning threshold、last request/input/output/
total tokens 和 last usage source；尚未请求模型时 token 显示 0，usage source 显示
`(none)`。该摘要不包含 prompt、assistant output 或 tool result 正文，也不产生 JSONL
日志事件。多行内容里的 `/usage` 按普通文本发送。
有 `--prompt` 且没有 `--quit` 时，先完整执行该 prompt 的一轮 agent loop，成功后补齐
必要换行并进入同一 REPL，会话历史包含初始 prompt、assistant 消息和 tool messages。
有 `--prompt` 且带 `--quit` 时，只执行这一轮然后退出，不进入 REPL。`--stdin` 和
`--file` 只能和 `--quit` 一起使用，分别读取 stdin 或指定文件的完整内容作为一次 prompt，
执行一轮后退出；它们和 `--prompt` 三者互斥。`--quit` 没有 `--prompt`、`--stdin` 或
`--file` 时作为用法错误处理。`chat` 命令的 flags 可以混排，例如
`sai chat --prompt "hi" --model fast --quit`；positional 参数不再作为初始 prompt。REPL
prompt 写到 stderr（例如 `> `），模型输出继续
通过现有 `writeStream` streaming 到 stdout，包括 reasoning 的隐藏、显示和终端样式规则。

未启用 resumable sessions 时，chat 会话历史只保存在当前进程内。每一轮成功后，agent 返回 updated messages，供下一轮
请求复用；其中包括原有 messages、当前 user message、assistant tool calls、tool result
messages 和最终 assistant 文本。model stream error 仍通过 error event 失败，不伪造成功
assistant 历史，也不继续下一轮。默认除 JSONL 日志外，chat 不落盘会话历史、上下文快照或
prompt/response/tool result 正文。MCP stdio server 在 chat 会话开始时启动，退出时关闭。

M13 后，`sai chat --save-session` 或配置 `sessions.enabled: true` 会在每个成功 turn 后
把完整 updated messages 保存到 `sessions.dir/<id>/session.json`。这包含完整用户输入、
assistant 输出、assistant tool calls 和 tool result messages，属于显式 opt-in 的敏感
内容落盘能力。`sai chat --resume <id>` 从指定 session 恢复 messages，`sai chat
--continue` 等价于恢复最近更新的 session；二者互斥。恢复后可以带 `--prompt` 和 `--quit`
继续一轮，也可以不带 `--prompt` 进入 REPL。

普通新 chat 中，`agent.show_reasoning` 可以从配置启用或关闭 reasoning 展示；命令行 bool
flag 支持双向覆盖，`--show-reasoning` 等价于 `--show-reasoning=true`，而
`--show-reasoning=false` 可以关闭本次 reasoning 展示。`sessions.enabled` 是 save-session
默认值入口；`--save-session` 等价于 `--save-session=true`，而 `--save-session=false`
可以关闭本次完整 session 保存，不另设重复配置字段。

恢复时优先使用 session 文件中保存的 provider、model profile、model id、model parameters、
enabled tools、enabled MCP、loaded skills、show_reasoning 和保存行为来准备 runtime，避免
“恢复了 messages 却发给不同模型或工具集合”。如果本次命令显式传入了冲突的
`--provider`、`--model`、`--enable-tools`、`--enable-mcp`、`--show-reasoning=true/false`，
或试图用 `--save-session=false` 改变
恢复后的保存语义，命令会失败并给出可读错误。可靠保存和恢复要求
`sessions.save_tool_results: true`；设为 `false` 时，`sai chat` 会拒绝启用保存或恢复。

一旦本次 chat 会保存完整敏感 session，CLI 会在 runtime 准备完成后、读取用户输入或发起
provider 请求前输出一次敏感数据提示。提示只说明保存风险，不包含 prompt、assistant output、
tool result 或注入指令正文。

`sai sessions list`、`sai sessions show <id>`、`sai sessions delete <id>` 和
`sai sessions prune --keep N` 使用配置解析后的 `sessions.dir`。即使
`sessions.enabled: false`，这些管理命令也可以查看或清理已有 session 文件。`list` 只输出
ID、更新时间、provider 和 model/profile 等元数据；`show` 只输出 session 元数据和敏感
数据风险提示，不打印完整 messages、prompt、assistant output、tool result 正文或注入指令
正文。`prune` 按 `updated_at` 保留最新的 N 个 session，`--keep` 必须显式提供且 N 必须
大于等于 0；命令输出删除数量和被删除的 session ID，不做额外确认。

## 配置形态

配置优先使用 YAML。根配置文件默认是启动时当前工作目录下的
`.agents/${arg[0]}.yaml`，其中 `${arg[0]}` 是可执行文件 basename；普通 `sai`
二进制默认读取 `.agents/sai.yaml`。也可以通过 `--config <file>` 指向一个具体的
根 YAML 配置文件。所选配置文件本身就是入口，不再在目录下额外拼接固定的 `sai.yaml`；
暂时不读取或写入用户目录。
根配置文件内的相对路径都基于该文件所在目录解析，包括 `provider_dir`、`auth_dir`、
`skill_dirs`、`mcp_dir`、`logging.path` 和 `sessions.dir`。二级配置文件沿用同一原则：
相对路径基于写出该路径的 YAML 文件所在目录解析，例如 provider 的 `auth_file` 相对
provider YAML 文件解析。

全局配置只保存默认选择、provider 文件目录和 agent 默认参数。
每个 provider 使用一个独立 YAML 文件；一个 provider 文件内可以声明多个模型，每个模型
可以有自己的参数。

```yaml
# sai.yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers
auth_dir: auth
skill_dirs: [skills]

agent:
  max_turns: 8
  stream: true
  show_reasoning: false

tools:
  enabled: []

logging:
  path: logs/sai.jsonl
  level: info

# M4 后启用
mcp_dir: mcp
```

```yaml
# providers/paperhub.yaml
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

model profile 的 `type` 是协议/adapter 类型，未配置时默认 `openai-chat`。配置层识别
`openai-chat`、`anthropic-messages`、`openai-responses` 和 `openai-codex`。当前 `sai chat` 支持
`openai-chat`，也支持 `anthropic-messages` 的文本 streaming 和 tool use，并支持
`openai-responses` / `openai-codex` 的文本 streaming 和 function tool calling。`id`、`type` 和
`context_window` 是本地元数据，不会作为请求参数透传。

```yaml
# providers/anthropic.yaml
name: anthropic
base_url: https://api.anthropic.com/v1
api_key: $ANTHROPIC_API_KEY

models:
  claude-sonnet-5:
    id: claude-sonnet-5
    type: anthropic-messages
    max_tokens: 4096
  claude-haiku-4-5:
    id: claude-haiku-4-5
    type: anthropic-messages
    max_tokens: 2048
```

```yaml
# providers/openai.yaml
name: openai
base_url: https://api.openai.com/v1
api_key: $OPENAI_API_KEY

models:
  default:
    id: gpt-5.1
    type: openai-responses
    max_output_tokens: 4096
```

`openai-responses` 使用 Responses API 的 `max_output_tokens`。为兼容已有 profile，
adapter 会把 legacy `max_tokens` 请求参数映射为 `max_output_tokens`；如果显式配置了
`max_output_tokens`，则保留显式值。
`openai-responses` 支持顶层 function `tools`、streamed function_call 事件，以及
`function_call_output` input item。`tool_choice`、`parallel_tool_calls` 等普通
Responses function tool 参数可以透传。custom tools、built-in web/code tools、
stateful `previous_response_id` 对话续写、reasoning output item passthrough 仍是后续项。

`openai-codex` 使用同一套 Responses request / SSE / function tool 转换，但 provider
配置使用 `auth_file` 而不是 `api_key`：

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

`auth_file` 相对 provider YAML 文件解析，推荐指向根配置文件所在目录下的 `auth/`。`sai auth
codex login --provider codex-work` 会生成或更新 `providers/codex-work.yaml` 和独立的
`auth/codex-work.json`，并在生成的 `openai-codex` model profile `parameters` 中默认写入
`store: false` 和 `reasoning.effort: high`。这些值通过现有 Responses 参数透传，不新增
顶层配置字段；reasoning 字段名是 `effort` 而不是 `effect`。因此 `codex`、`codex-work`
和 `codex-personal` 可以共存。token
文件包含 OAuth access / refresh token，属于敏感数据；`sai config show`、verbose 和日志
不能打印其中的 token。access token 过期时运行时用 refresh token 刷新，并写回同一
auth 文件。M16 不读取或导入 `~/.codex/auth.json`；首版登录使用 Codex headless device
flow：先用 JSON body 调 `/api/accounts/deviceauth/usercode`，成功后打印返回的
verification URL / user code；如果服务没有返回 verification URL，则回退到 issuer 的
`/codex/device` 页面。再用 JSON body 轮询 `/api/accounts/deviceauth/token`，其中
authorization-pending 类错误响应继续按 pending 处理，直到用户批准或设备码过期；
最后的 `/oauth/token` authorization-code exchange 继续使用 form encoding。不实现浏览器
OAuth 回调。

`openai-codex` runtime 请求 Codex 后端时必须显式发送 `store: false`。这是 request 层
要求，不是用户 API key、`auth_file` 或 OAuth token 设置。

命令行参数覆盖配置文件。`api_key` 是 provider 配置中的具体字段。对需要脱敏的敏感
配置值，统一使用简单字符串约定：值以 `$` 开头时，`$` 后面的内容作为环境变量名读取
实际值；不以 `$` 开头时表示直接配置值。这个解析发生在配置读取阶段；provider adapter
接收解析后的实际值。除非某个 adapter 明确声明协议特定的默认环境变量，否则 adapter
不承担通用环境变量解析职责。无论实际值来自环境变量还是直接配置，日志、verbose 输出
和 resolved config 都不能打印实际值。
model profile 可选 `context_window` 作为本地上下文窗口元数据，不透传给 provider。未配置
时使用保守估算默认值 `32000`，来源记录为 `estimated`；显式配置时来源为
`configured`。请求参数可继续写在 profile 顶层，也可写在 `parameters` map 中。
`provider_dir` 相对根配置文件所在目录解析，除非显式写成绝对路径。
`auth_dir` 是 M16 后的 OAuth token 文件目录，默认是根配置文件所在目录下的 `auth`，同样
相对根配置文件所在目录解析；它只保存 `sai auth codex login` 生成的独立 token 文件。
`skill_dirs` 是 M7 后的本地 skill 目录列表，默认等价于 `skill_dirs: [skills]`；每个
条目相对根配置文件所在目录解析，除非显式写成绝对路径。空列表 `skill_dirs: []` 表示不加载本地
skills。推荐布局是 `.agents/skills/<skill_id>/SKILL.md`。`sai` 按配置顺序扫描多个
目录；每个目录只发现包含 `SKILL.md` 的直接子目录，不递归读取，也不读取用户目录。同一
目录内按确定性 discovery 顺序加载，跨目录保留配置的目录顺序。不同 skill 目录中出现重复
skill id 是配置错误。默认情况下，发现到的 skill 会注入模型上下文；如果某个 `SKILL.md`
的 YAML frontmatter 设置 `disable-model-invocation: true`，该 skill 不注入模型上下文。
缺失该字段或设置为 `false` 表示正常加载。skill 是否注入模型上下文只由其 `SKILL.md`
frontmatter 决定。
`logging.path` 相对根配置文件所在目录解析，除非显式写成绝对路径。它兼容旧配置形态，但运行时
解释为日志根/基准路径：例如 `logs/sai.jsonl` 会使用其目录 `logs/` 作为 session root；
空字符串表示禁用日志。`mcp_dir` 是 M4 后启用的 MCP 配置目录，同样相对根配置文件所在目录解析。

模型选择发生在会话开始时：

```text
sai chat --quit --provider paperhub --model glm-5.2 --prompt "你是谁？"
sai chat --provider paperhub --model glm-5.2
```

如果命令行没有指定模型，则使用全局默认值。若默认值缺失或无效，CLI 应给出可选
provider/model 列表并停止。v0.1 不支持会话进行中切换模型。

## 项目上下文

`sai` 默认读取启动目录下的 `AGENTS.md`，并把其中的内容作为项目指令加入本次会话的
system/developer context。`AGENTS.md` 缺失时不报错。

v0.1 只读取启动目录的 `AGENTS.md`：

```text
AGENTS.md
.agents/
  sai.yaml
  providers/
  mcp/
```

`--config` 只影响根配置文件位置以及该文件内相对路径的解析基准，不影响 `AGENTS.md` 位置。
`sai` 暂时不读取用户目录里的 `AGENTS.md`，也不做多层目录向上/向下查找。

项目指令优先级：

```text
sai 内置基础约束 > AGENTS.md > loaded skills > 当前用户 prompt
```

用户 prompt 不应覆盖 `sai` 的基础安全和执行约束，也不应隐式覆盖 `AGENTS.md` 中的项目
约定。后续如需临时忽略项目指令，应增加显式参数，而不是通过普通 prompt 实现。
已加载 skill 的 instructions 作为 developer message 追加在 `AGENTS.md` 之后、用户
prompt 之前；多个 skill 使用 skill 目录顺序和目录内确定性 discovery 顺序注入。

## OpenAI-Compatible Streaming 注意点

PaperHub smoke test 已确认 `glm-5.2` 返回 OpenAI Chat Completions 风格的
SSE chunk：

```text
data: {"object":"chat.completion.chunk", ...}
data: [DONE]
```

流中可能同时存在：

```text
choices[0].delta.reasoning_content
choices[0].delta.content
```

默认只把 `delta.content` 作为用户可见输出。`delta.reasoning_content` 需要解析成
内部事件，但默认不打印。启用 `--show-reasoning` 时，CLI 可以直接打印 reasoning 输出，
不额外输出 marker；如果前面已有正文且没有换行，先补齐换行。当事件流从 reasoning
输出切换到最终消息输出时，也必须保持换行分隔，避免 reasoning 和最终消息混在同一行。

## Reasoning 输出样式

M9 后，reasoning 样式只属于 CLI stdout 展示层，不改变内部 stream event，也不改变
JSONL 日志。`reasoning_delta` 仍按原始事件记录；ANSI 控制符不能写入日志。

只有 reasoning 被显式显示时才考虑上色：命令行 `--show-reasoning` 或配置
`agent.show_reasoning: true` 生效后，`writeStream` 在支持颜色的终端 stdout 上使用
ANSI 暗灰色显示 reasoning。当前样式是 `\x1b[90m` 开始、`\x1b[0m` reset。
如果 stdout 不是终端，例如 pipe、redirect 或测试中的 `bytes.Buffer`，不输出 ANSI。
如果环境变量 `NO_COLOR` 存在且非空，即使 stdout 是终端也不输出 ANSI。

从 reasoning 切换到任何非 reasoning 输出或事件前必须先 reset，包括 tool 状态、最终
`text_delta`、error 和 stream end，再沿用原有 reasoning/final 换行规则，确保最终输出
不是灰色。若 tool 后又继续 reasoning，reasoning 可以重新进入灰色，但状态必须重新显式
建立。若 stream 只有 reasoning 没有最终文本，函数结束前也必须 reset，避免调用方终端
颜色泄漏。M9 不引入 TUI、不引入第三方依赖，也不新增 `--no-color`；若以后需要显式颜色
开关，应作为单独里程碑设计。

Tool 状态是 stderr 展示层：支持颜色且未设置 `NO_COLOR` 时，状态行本身使用同一 muted
样式并在每一行后 reset，不能依赖 stdout/reasoning 的颜色状态泄漏。状态格式是
`tool: <name> [path]`，没有符号前缀；连续 tool status 每个独立成行。JSONL 日志继续记录
原始事件，不包含 ANSI 控制符。

## Tool Calling

内部 tool schema 建议保持稳定：

```text
name
description
input_schema
executor
```

OpenAI-compatible adapter 将内部 tool schema 转换成：

```text
tools: [
  {
    type: "function",
    function: {
      name,
      description,
      parameters
    }
  }
]
```

OpenAI Responses adapter 将内部 tool schema 转换成 Responses 顶层 function tools：

```text
tools: [
  {
    type: "function",
    name,
    description,
    parameters
  }
]
```

Responses function tool 名称只发送字母、数字、下划线和连字符组成的合法名；例如
`mcp.local.search` 会在单次请求内稳定映射为 `tool_0`，stream 返回时再映射回内部名。
assistant 历史里的 tool calls 会转换成 `function_call` input item，tool result 会转换成
`function_call_output` input item，并使用 `call_id` 关联。

当前内置工具：

```text
list_files
read_file
write_file
edit_file
shell
```

可用 `sai tools list` 静态列出上述内置工具。该命令和 help 路径一样在配置加载前完成，
不读取 `.agents` 配置、不解析 provider API key。

工具默认不启用。只有出现在配置文件 `tools.enabled` 中，或通过命令行
`--enable-tools` 指定时，才会暴露给模型。

```yaml
tools:
  enabled:
    - list_files
    - read_file
    - write_file
    - edit_file
    - shell
```

```text
sai chat --quit --enable-tools list_files,read_file --prompt "列出当前目录"
```

`--enable-tools` 覆盖配置文件中的 enabled tools 列表，而不是追加。`shell`、`write_file`
和 `edit_file` 不需要额外审核 flag；只要它被启用，就按普通工具处理。

`shell` 工具默认在启动目录执行命令。v0.1 不提供 `--workdir`，也不做复杂沙箱；后续如需
改变执行目录，再增加显式参数。

M17 计划补齐本地工具易用性和发现能力，但不改变工具默认关闭的边界。`read_file` 仍只读取
启动目录工作区内的文本文件；它新增可选参数 `start_line`、`line_count` 和 `max_bytes`：
`start_line` 是 1-based 行号，`line_count` 必须大于 0，`max_bytes` 必须大于 0。不提供
byte offset / byte count 模式。默认读取从文件开头开始，并最多返回 `max_bytes` 字节。
如果只提供 `start_line`，则从该行读取到 `max_bytes` 或 EOF；如果同时提供 `start_line`
和 `line_count`，则最多返回指定行数，同时仍受 `max_bytes` 限制。`max_bytes` 同时适用于
默认读取和行范围读取。

`read_file` 的兼容边界是：小文件、非范围且未因 `max_bytes` 截断的读取，可以继续直接返回
原始文件内容。只要结果因 `max_bytes` 不完整，tool result 必须明确包含 `truncated=true`，
并告诉 agent 下一步应如何继续读取。行范围读取应尽量返回完整行；如果单行本身超过
`max_bytes`，返回该行前缀并标记 `line_truncated=true`，同时提示 agent 增大
`max_bytes` 并从同一行重试。

M17 还会增加只在工作区内运行的发现/搜索工具：

- `glob_files`：使用 workspace-local glob 查找文件，返回稳定的相对路径；支持
  `max_results`，结果被截断时返回明确的 truncation metadata。
- `grep_files`：使用 workspace-local 文本搜索，支持 include / exclude globs；默认按
  literal 搜索，可选 regex、大小写敏感和 context lines；支持 `max_results` 和 snippet
  limits，命中或片段被截断时返回明确的 truncation metadata。

`shell` 工具在 M17 增加可选 `timeout_ms` 和 `max_output_bytes`。输出被
`max_output_bytes` 截断时，tool result 必须明确说明截断；CLI 的 tool status 行继续只显示
`tool: shell`，不显示命令参数或任意 arguments。

## MCP

MCP 不属于 MVP 必需能力。先完成 `sai chat --quit`、streaming、tool call loop、错误处理、
JSONL 日志和单文件构建，再实现 MCP。

MCP 使用单独目录配置，不放进 provider 配置。每个 MCP server 一个 YAML 文件，和
`providers/` 的组织方式保持一致。

```yaml
# mcp/local.yaml
id: local
enabled: true
command: example-mcp-server
args: []
env: {}
```

MCP 在第一阶段作为一种 tool source：

1. 读取 `mcp_dir` 下的 MCP server 配置文件。
2. 完成 MCP initialize。
3. 获取 MCP tools 列表。
4. 将 MCP tool schema 转换为内部 tool schema。
5. 只有 enabled tools 中包含对应 MCP tool 时，才暴露给模型。
6. 模型调用对应工具时，转发给 MCP server。

MCP tool 名称必须使用 `mcp.<server>.<tool>` 形式，避免和内置工具冲突。实现中应固定该
命名规则，而不是把 MCP 原始 tool name 直接暴露给模型。

默认启用 `enabled: true` 的 MCP server。如果命令行传入 `--enable-mcp`，本次运行的
MCP server 启用列表完全由该参数决定，忽略 MCP 文件中的 `enabled` 字段。

```text
sai chat --quit --enable-mcp local --enable-tools mcp.local.some_tool --prompt "使用 MCP 工具"
```

v0.1 中 MCP server 进程生命周期由当前 agent 进程管理。后台常驻管理后续再做。

## 日志和落盘

v0.1 只落盘 JSONL 日志，不保存会话历史或上下文快照。日志路径来自
`logging.path`，相对路径基于根配置文件所在目录解析。`logging.path` 解释为日志根/基准路径：
如果配置为 `logs/sai.jsonl`，实际 session root 是 `logs/`；如果配置为空，禁用日志。
每次 `sai chat` 启动 runtime 时预先确定唯一 session JSONL 路径，供
`--verbose` 显示；但 log root、session 目录和 `sai.jsonl` 只在第一条日志事件发生时
创建。chat 启动后直接 `/exit`、`/quit` 或 EOF 且没有模型请求时，不产生日志 session。

```text
<log-root>/<timestamp>-<short-random>/sai.jsonl
```

每行日志是一个 JSON object，至少包含：

```text
time
level
event
provider
model
```

工具调用、usage、HTTP 错误、MCP 错误可以写入日志。API key、Authorization header
和其他敏感配置值的实际值不能写入日志。v0.1 不记录完整 prompt、response、
tool result 正文，也不提供开启正文日志的配置。

M13 后的 resumable sessions 使用独立的 `sessions` 配置和独立存储目录，不复用
`logging.path`。JSONL session log / transcript 继续服务于诊断和审计，默认仍不记录完整
正文；resumable session 则服务于可靠恢复，启用后必须保存完整 messages 和 tool result
messages。二者的用途、默认值和敏感数据风险都应在 CLI 和文档中明确区分。

resumable session 默认关闭。启用后，每个 session 至少保存 provider、model、model
profile parameters、cwd、根配置文件路径、启用 tools/MCP、loaded skills、reasoning、注入指令快照或
可重建信息，以及完整 user messages、assistant final messages、assistant tool calls 和
tool result messages。缺少这些信息时，只能得到 transcript 或诊断日志，不能承诺可靠
resume。

当前 M13 已接入 `sai chat --save-session`、`sai chat --resume <id>`、
`sai chat --continue`、`sai sessions list`、`sai sessions show <id>`、
`sai sessions delete <id>` 和 `sai sessions prune --keep N`。管理命令只展示元数据或删除
文件，不打印完整 messages、prompt、assistant output 或 tool result 正文。

## Context Window Management

M14 的第一版只做保守上下文窗口管理，不做自动摘要、自动截断或交互式选择。每次 provider
请求前，运行时基于当前 request messages 和 tool schemas 做输入 token 估算；估算集中在
`internal/contextwindow`，当前按字符数保守估算，并包含固定 message / tool schema overhead。

模型 profile 可配置 `context_window`。未配置时使用 `32000` token 的保守估算默认值，来源
记录为 `estimated`；显式配置时来源记录为 `configured`。当估算输入达到窗口的 80% 时，
CLI 向 stderr 输出一次 warning，只包含 token 数和窗口，不包含 prompt、assistant output、
tool result、tool schema 或 API key。达到或超过窗口时，运行时拒绝发起 provider 请求，
并返回可读错误。

usage tracking 优先使用 provider stream 中的 `model.UsageEvent`。如果本次 stream 成功结束
但没有 usage event，则记录 fallback estimate。启用 resumable sessions 时，session 文件会
保存 context management metadata，包括窗口、来源、warning 阈值、最近 request estimate 和
usage source；恢复后继续使用这些 metadata 判断预算。`sai sessions show` 只展示这些数字和
source，不展示正文。

当前策略保守保留全部上下文：system/developer messages、loaded skill instructions、tool
/ MCP schemas、assistant tool calls、tool result messages 和历史 user/assistant messages
都不会被静默丢弃。后续若要加入摘要或截断策略，必须单独设计可解释边界和测试，证明不会
静默丢弃关键 system/developer/tool schema/tool result 信息。

## 测试策略

测试分三层：

- 单元测试：stream parser、event 转换、config 加载、tool call delta 累积。
- 集成测试：本地 fake OpenAI-compatible server、本地 fake MCP server。
- 手动 smoke test：使用 `PAPERHUB_API_KEY` 调 PaperHub。

M2 完成前需要额外做一次 PaperHub tool call smoke test，确认 `glm-5.2` 对
OpenAI-compatible `tools` / `tool_calls` 的真实兼容性。若服务不支持，保留协议层实现，
并将 PaperHub 的 tool calling 标记为已知限制。

2026-07-01 已执行 PaperHub tool call smoke test。命令形态为：
`go run ./cmd/sai --config <temp-config>/sai.yaml chat --quit --provider paperhub --model glm-5.2 --enable-tools list_files --prompt "<prompt>"`。
结果：PaperHub `glm-5.2` 成功返回 tool call，`sai` 执行 `list_files` 后继续输出最终文本。

手动测试命令：

```powershell
sai chat --quit --provider paperhub --prompt "你是谁？"
```

预期行为：

- 文本逐步输出。
- reasoning 内容默认不输出。
- 收到 `[DONE]` 后正常退出。
- `usage` 可在 verbose 输出或最终 metadata 中看到。

## 发布

优先单文件发布：

```text
sai-windows-amd64.exe
sai-linux-amd64
sai-darwin-arm64
```

第一版不做安装器、自动更新或 shell 集成。
