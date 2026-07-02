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
- skills 是后续开发项，v0.1 不作为 MVP 核心。M7 使用配置目录下的本地
  `SKILL.md`，支持显式 activation 和 instruction composition；不读取用户目录，
  不实现 marketplace、递归 skill discovery 或复杂依赖解析。
- M5 已为 Anthropic Messages 添加 provider type 配置识别、示例、文本 streaming
  和 tool use runtime adapter。
- M6 已为 OpenAI Responses 添加 provider type 配置识别、文本 input mapping、
  semantic text streaming adapter，以及 function tools / function_call_output tool loop adapter。
- MCP 不属于 MVP 核心；后续接入时第一种传输只做 stdio。
- 配置根目录默认是启动时当前工作目录下的 `.agents`，也可以通过 `--config-dir` 指定。
- 默认读取启动目录下的 `AGENTS.md` 作为项目指令；文件不存在时继续执行。
- 只落盘 JSONL 日志；会话历史、上下文快照和其他状态暂不落盘。

## v0.1 非目标

- TUI 或全屏终端界面。
- 浏览器自动化。
- multi-agent 编排。
- 长期记忆。
- skill 加载。
- OpenAI Responses custom tools、built-in web/code tools、stateful `previous_response_id`
  对话续写，以及 reasoning output item passthrough。
- MCP stdio。
- remote MCP over HTTP/SSE。
- 插件市场或插件生命周期管理。

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
  本地 skill 目录发现、显式 activation 和 SKILL.md instruction composition

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
（例如 `! tool: read_file docs/notes.md`），不需要 `--verbose`；`read_file` 和
`list_files` 状态可以显示目标路径/目录，其中 `list_files` 未提供 path 时显示 `.`。
`shell` 和 MCP tool 状态只显示工具名，不显示命令参数或任意 arguments，也不打印 tool
result 正文，stdout 仍只承载模型可见输出。

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
sai run "prompt"
sai chat
sai tools list
sai models list
sai config show
sai mcp list  # M4
```

Help/usage 是普通 CLI 行为，不引入 TUI 或第三方 CLI 框架。支持：

```text
sai -h
sai --help
sai help
sai version -h
sai help version
sai run -h
sai help run
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
sai tools -h
sai help tools
sai tools list -h
sai help tools list
sai mcp -h
sai help mcp
sai mcp list -h
sai help mcp list
```

help 输出写到 stdout，exit code 为 0。help 必须在配置加载前完成：不读取 `.agents`
配置、不解析 provider API key、不读取 enabled skills 或 MCP 配置，也不能打印任何配置值
或 secrets。未知命令和错误参数仍以 exit code 1 失败，并给出可读错误和类似
`Run "sai help" for usage.` 的提示。

常用参数：

```text
--provider paperhub
--model glm-5.2
--config-dir ./config
--base-url https://tc-paperhub.diezhi.net/v1
--show-reasoning
--max-turns 8
--enable-tools read_file,list_files,shell
--enable-skills writing,review
--disable-skills
--verbose
--enable-mcp local  # M4
```

v0.1 暂不支持 non-streaming fallback，`sai run` 当前强制使用 streaming；后续如要支持再引入 `--no-stream` 或 adapter 非流式路径。

## CLI Chat REPL

M10 后，`sai chat` 是一个克制的逐行 REPL：启动时固定 provider、model、tools、MCP、
skills 和 reasoning 展示设置；会话进行中不支持模型切换或额外 slash commands。它和
`sai run` 共享同一套配置选择规则和 flags：

```text
sai chat --provider paperhub --model glm-5.2
sai chat --enable-tools list_files,read_file
sai chat --enable-mcp local --enable-tools mcp.local.some_tool
```

输入从 stdin 逐行读取，空白行忽略。`/exit`、`/quit` 或 EOF 正常退出。prompt 写到
stderr（例如 `> `），模型输出继续通过现有 `writeStream` streaming 到 stdout，包括
reasoning 的隐藏、显示和终端样式规则。chat 成功完成一轮后，如果 assistant 输出没有以
换行结束，CLI 只为 chat 补一个换行，避免下一个 prompt 和模型输出粘在一起；`sai run`
的成功输出语义不变。

chat 会话历史只保存在当前进程内。每一轮成功后，agent 返回 updated messages，供下一轮
请求复用；其中包括原有 messages、当前 user message、assistant tool calls、tool result
messages 和最终 assistant 文本。model stream error 仍通过 error event 失败，不伪造成功
assistant 历史，也不继续下一轮。除 JSONL 日志外，chat 不落盘会话历史、上下文快照或
prompt/response/tool result 正文。MCP stdio server 在 chat 会话开始时启动，退出时关闭。

## 配置形态

配置优先使用 YAML。配置根目录默认为启动时当前工作目录下的 `.agents`，也可以通过
`--config-dir` 指定。暂时不读取或写入用户目录。

全局配置只保存默认选择、provider 文件目录和 agent 默认参数。
每个 provider 使用一个独立 YAML 文件；一个 provider 文件内可以声明多个模型，每个模型
可以有自己的参数。

```yaml
# sai.yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers
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

# M4 后启用
mcp_dir: mcp
```

```yaml
# providers/paperhub.yaml
name: paperhub
type: openai-chat
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY

models:
  glm-5.2:
    id: glm-5.2
    temperature: 0.6
    max_tokens: 4096
  glm-5.2-fast:
    id: glm-5.2
    temperature: 0.2
    max_tokens: 2048
```

配置层识别的 provider type 包括 `openai-chat`、`anthropic-messages` 和
`openai-responses`。当前 `sai run` 支持 `openai-chat`，也支持
`anthropic-messages` 的文本 streaming 和 tool use，并支持 `openai-responses` 的
文本 streaming 和 function tool calling。

```yaml
# providers/anthropic.yaml
name: anthropic
type: anthropic-messages
base_url: https://api.anthropic.com/v1
api_key: $ANTHROPIC_API_KEY

models:
  claude-sonnet-5:
    id: claude-sonnet-5
    max_tokens: 4096
  claude-haiku-4-5:
    id: claude-haiku-4-5
    max_tokens: 2048
```

```yaml
# providers/openai.yaml
name: openai
type: openai-responses
base_url: https://api.openai.com/v1
api_key: $OPENAI_API_KEY

models:
  default:
    id: gpt-5.1
    max_output_tokens: 4096
```

`openai-responses` 使用 Responses API 的 `max_output_tokens`。为兼容已有 profile，
adapter 会把 legacy `max_tokens` 请求参数映射为 `max_output_tokens`；如果显式配置了
`max_output_tokens`，则保留显式值。
`openai-responses` 支持顶层 function `tools`、streamed function_call 事件，以及
`function_call_output` input item。`tool_choice`、`parallel_tool_calls` 等普通
Responses function tool 参数可以透传。custom tools、built-in web/code tools、
stateful `previous_response_id` 对话续写、reasoning output item passthrough 仍是后续项。

命令行参数覆盖配置文件。`api_key` 是 provider 配置中的具体字段。对需要脱敏的敏感
配置值，统一使用简单字符串约定：值以 `$` 开头时，`$` 后面的内容作为环境变量名读取
实际值；不以 `$` 开头时表示直接配置值。这个解析发生在配置读取阶段；provider adapter
接收解析后的实际值。除非某个 adapter 明确声明协议特定的默认环境变量，否则 adapter
不承担通用环境变量解析职责。无论实际值来自环境变量还是直接配置，日志、verbose 输出
和 resolved config 都不能打印实际值。
`provider_dir` 相对配置根目录解析，除非显式写成绝对路径。
`skill_dir` 是 M7 后的本地 skill 目录，默认是配置根目录下的 `skills`，同样相对配置
根目录解析；推荐布局是 `.agents/skills/<skill_id>/SKILL.md`。`skills.enabled`
保存默认启用的本地 skill id 列表，空列表表示不读取或注入任何 skill。`sai run
--enable-skills id1,id2` 覆盖配置中的 enabled skills，`--disable-skills` 本次运行禁用
所有 skills；二者不能同时使用。只读取 `skill_dir`，不读取用户目录。
`logging.path` 相对配置根目录解析，除非显式写成绝对路径。它兼容旧配置形态，但运行时
解释为日志根/基准路径：例如 `logs/sai.jsonl` 会使用其目录 `logs/` 作为 session root；
空字符串表示禁用日志。`mcp_dir` 是 M4 后启用的 MCP 配置目录，同样相对配置根目录解析。

模型选择发生在会话开始时：

```text
sai run --provider paperhub --model glm-5.2 "你是谁？"
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

`--config-dir` 只影响配置根目录和其中的 `sai.yaml` 位置，不影响 `AGENTS.md` 位置。
`sai` 暂时不读取用户目录里的 `AGENTS.md`，也不做多层目录向上/向下查找。

项目指令优先级：

```text
sai 内置基础约束 > AGENTS.md > enabled skills > 当前用户 prompt
```

用户 prompt 不应覆盖 `sai` 的基础安全和执行约束，也不应隐式覆盖 `AGENTS.md` 中的项目
约定。后续如需临时忽略项目指令，应增加显式参数，而不是通过普通 prompt 实现。
已启用 skill 的 instructions 作为 developer message 追加在 `AGENTS.md` 之后、用户
prompt 之前；多个 skill 按 `skills.enabled` 或 `--enable-skills` 中的顺序注入。

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
内部事件，但默认不打印。启用 `--show-reasoning` 时，CLI 可以打印 reasoning 输出，并在
每个可见 reasoning 块前输出独立标记行 `? reasoning`；如果前面已有正文且没有换行，
先补齐换行。当事件流从 reasoning 输出切换到最终消息输出时，也必须保持换行分隔，避免
reasoning 和最终消息混在同一行。

## Reasoning 输出样式

M9 后，reasoning 样式只属于 CLI stdout 展示层，不改变内部 stream event，也不改变
JSONL 日志。`reasoning_delta` 仍按原始事件记录；ANSI 控制符不能写入日志。

只有 reasoning 被显式显示时才考虑上色：命令行 `--show-reasoning` 或配置
`agent.show_reasoning: true` 生效后，`writeStream` 在支持颜色的终端 stdout 上使用
ANSI 暗灰色显示 `? reasoning` 标记和 reasoning。当前样式是 `\x1b[90m` 开始、
`\x1b[0m` reset。
如果 stdout 不是终端，例如 pipe、redirect 或测试中的 `bytes.Buffer`，不输出 ANSI。
如果环境变量 `NO_COLOR` 存在且非空，即使 stdout 是终端也不输出 ANSI。

从 reasoning 切换到最终 `text_delta` 前必须先 reset，再沿用原有 reasoning/final
换行规则，确保最终输出不是灰色。若 stream 只有 reasoning 没有最终文本，函数结束前也
必须 reset，避免调用方终端颜色泄漏。M9 不引入 TUI、不引入第三方依赖，也不新增
`--no-color`；若以后需要显式颜色开关，应作为单独里程碑设计。

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

v0.1 内置三个工具：

```text
list_files
read_file
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
    - shell
```

```text
sai run --enable-tools list_files,read_file "列出当前目录"
```

`--enable-tools` 覆盖配置文件中的 enabled tools 列表，而不是追加。`shell` 不需要额外
审核 flag；只要它被启用，就按普通工具处理。如果后续加入 `write_file`，也遵循同一规则。

`shell` 工具默认在启动目录执行命令。v0.1 不提供 `--workdir`，也不做复杂沙箱；后续如需
改变执行目录，再增加显式参数。

## MCP

MCP 不属于 MVP 必需能力。先完成 `sai run`、streaming、tool call loop、错误处理、
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
sai run --enable-mcp local --enable-tools mcp.local.some_tool "使用 MCP 工具"
```

v0.1 中 MCP server 进程生命周期由当前 agent 进程管理。后台常驻管理后续再做。

## 日志和落盘

v0.1 只落盘 JSONL 日志，不保存会话历史或上下文快照。日志路径来自
`logging.path`，相对路径基于配置根目录解析。`logging.path` 解释为日志根/基准路径：
如果配置为 `logs/sai.jsonl`，实际 session root 是 `logs/`；如果配置为空，禁用日志。
每次 `sai run` 或 `sai chat` 启动 runtime 时预先确定唯一 session JSONL 路径，供
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

## 测试策略

测试分三层：

- 单元测试：stream parser、event 转换、config 加载、tool call delta 累积。
- 集成测试：本地 fake OpenAI-compatible server、本地 fake MCP server。
- 手动 smoke test：使用 `PAPERHUB_API_KEY` 调 PaperHub。

M2 完成前需要额外做一次 PaperHub tool call smoke test，确认 `glm-5.2` 对
OpenAI-compatible `tools` / `tool_calls` 的真实兼容性。若服务不支持，保留协议层实现，
并将 PaperHub 的 tool calling 标记为已知限制。

2026-07-01 已执行 PaperHub tool call smoke test。命令形态为：
`go run ./cmd/sai --config-dir <temp-config> run --provider paperhub --model glm-5.2 --enable-tools list_files "<prompt>"`。
结果：PaperHub `glm-5.2` 成功返回 tool call，`sai` 执行 `list_files` 后继续输出最终文本。

手动测试命令：

```powershell
sai run --provider paperhub "你是谁？"
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
