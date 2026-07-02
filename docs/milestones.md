# 里程碑

每个里程碑都应该结束于一个可运行状态，并配套一个明确的验证方式。MVP 路径是
M0 到 M3：先把 `sai` 跑起来、能调用模型、能执行基础 tool call，并能单文件分发。
MCP 放到 MVP 之后。

## M0：项目骨架和关键决策

目标：建立最小项目结构，并锁定 v0.1 范围。

交付物：

- 初始化 Go module。
- 创建 `sai` CLI 入口。
- 记录并 stub YAML 配置结构。
- 支持默认从启动时当前工作目录下的 `.agents` 读取配置。
- 支持通过 `--config-dir` 显式指定配置根目录。
- 支持从配置根目录的 `sai.yaml` 读取全局配置。
- 支持全局配置和每个 provider 一个配置文件的目录布局。
- 支持 provider 下声明多个 model profile。
- 支持 `tools.enabled` 配置，默认不启用工具。
- 支持读取启动目录下的 `AGENTS.md`，缺失时继续执行。
- 明确项目指令优先级：`sai` 内置基础约束 > `AGENTS.md` > 当前用户 prompt。
- 支持 JSONL 日志配置。
- 定义 provider interface 和内部事件类型。
- 记录 PaperHub provider profile。

验证：

- `go test ./...` 通过。
- `sai config show` 能输出不含 API key 或其他敏感配置值实际值的解析后配置。
- `sai config show --config-dir ./example-config` 能从指定配置根目录的 `sai.yaml` 读取配置。
- `sai models list` 能列出配置中的 provider/model。
- `sai` 在启动目录存在 `AGENTS.md` 时能加载项目指令。

## M1：OpenAI-Compatible Streaming

目标：从 OpenAI-compatible Chat Completions endpoint 流式输出文本。

交付物：

- OpenAI-compatible Chat Completions adapter。
- SSE parser，支持 `data:` event 和 `[DONE]`。
- 处理 `delta.content`。
- 处理 `delta.reasoning_content`。
- `sai chat --quit "prompt"` 单轮命令形态。
- 会话开始时根据 `--provider` 和 `--model` 选择模型。
- 将启动目录的 `AGENTS.md` 内容加入本次会话上下文。
- `--show-reasoning` 参数，并在 reasoning 输出结束后换行再输出最终消息。

验证：

- `sai chat --quit --provider paperhub "你是谁？"` 能流式输出可见文本。
- `sai chat --provider paperhub --model glm-5.2` 在会话开始时固定模型。
- 缺失 `AGENTS.md` 时命令仍可正常运行。
- reasoning 内容默认隐藏。
- `--show-reasoning` 能单独显示 reasoning 内容，且 reasoning 和最终消息之间有换行分隔。
- 单元测试覆盖普通 chunk、reasoning chunk、usage chunk 和 `[DONE]`。

## M2：Tool Call Loop

目标：支持模型请求 function call，并在执行工具后继续对话。

交付物：

- 内部 tool schema。
- tool registry。
- tool call delta 累积。
- tool result message 构造。
- `max_turns` 保护。
- 三个内置工具：`list_files`、`read_file`、`shell`。
- 工具默认不启用。
- `tools.enabled` 控制默认暴露给模型的工具。
- `--enable-tools` 覆盖配置中的 enabled tools 列表。
- `shell` 工具默认在启动目录执行命令。
- JSONL 日志记录工具调用事件。

验证：

- fixture test 能正确重建 streamed tool call arguments。
- fake OpenAI-compatible server 能请求工具并收到工具结果。
- 未启用工具时，请求 payload 不包含 tools。
- `--enable-tools list_files,read_file` 只暴露这两个工具。
- PaperHub tool call smoke test 已执行，或将不兼容情况记录为已知限制。
- 达到 `max_turns` 时 agent 给出清晰错误并停止。

## M3：打包和可用性

目标：让 v0.1 成为一个可实际使用的单文件 CLI。

交付物：

- 跨平台 build 命令。
- version 命令。
- 可读错误信息。
- `--verbose` 诊断信息，且不泄露 API key 或其他敏感配置值的实际值。
- JSONL 日志，每次 `chat` 预计算独立 session 路径，并在首个日志事件发生时写入
  独立 session 目录，且不泄露 API key、Authorization header 或其他敏感配置值的实际值。
- 除 JSONL 日志外，不保存会话历史、上下文快照或其他状态。
- 最小 README 使用说明。

验证：

- Windows 单文件可执行程序能运行 `sai chat --quit`。
- 能产出目标平台构建产物。
- missing API key、bad base URL、invalid model response 都有可读错误。

## M4：MCP Stdio Tools

目标：在 MVP 可运行、可分发之后，将 MCP stdio server 的 tools 暴露给模型调用。

交付物：

- 单独的 `mcp/` 配置目录。
- 每个 MCP server 一个 YAML 文件。
- `--enable-mcp` 覆盖 MCP 文件中的 `enabled` 字段。
- MCP stdio 进程启动和关闭。
- MCP initialize 流程。
- MCP tool listing。
- MCP tool call routing。
- MCP tools 使用 `mcp.<server>.<tool>` 命名，并通过 enabled tools 控制是否暴露给模型。
- `sai mcp list` 命令。

验证：

- 本地 fake MCP server 能出现在 `sai mcp list` 中。
- `--enable-mcp local` 只启动 `local` MCP server。
- fake model tool call 能到达 MCP server 并返回结果。
- `sai` 退出时 MCP 进程一并退出。

## M5：Anthropic Messages Adapter

目标：核心循环稳定后，增加 Anthropic Messages provider adapter。

交付物：

- Anthropic provider 配置。
- Anthropic streaming event 转换。
- Anthropic tool call 转换。
- adapter 相关测试。

验证：

- 既有 OpenAI-compatible 测试仍然通过。
- Anthropic fixture tests 能映射到同一套内部事件流。

## M6：OpenAI Responses Adapter

目标：增加 OpenAI Responses provider adapter。先接入文本 streaming，再接入 Responses
function tools / function_call_output，使现有 agent tool loop 能通过 `openai-responses`
跑通 function tools。

交付物：

- Responses provider 配置。
- Responses 文本 input mapping。
- semantic text streaming event 转换。
- function tools request mapping，使用顶层 `{type:"function", name, description, parameters}`。
- 非法内部 tool 名称的请求内稳定 alias mapping，并在 stream 返回时映射回内部名。
- assistant `ToolCalls` 到 `function_call` input item 的转换。
- tool result message 到 `function_call_output` input item 的转换。
- function call argument delta / done / output_item.done event 处理。

后续项：

- custom tools。
- built-in web/code tools。
- stateful `previous_response_id` 对话续写。
- reasoning output item passthrough。

验证：

- Responses text fixture tests 能映射到同一套内部事件流。
- fake Responses server 能验证 `openai-responses` 请求 `<base_url>/responses` 并流式输出文本。
- Responses function call fixture tests 能重建 streamed arguments，并只发出一个完整
  `ToolCallDoneEvent`。
- fake Responses server 能请求 `read_file`，收到 `function_call` 和
  `function_call_output` input items 后继续输出最终文本。
- OpenAI-compatible adapter 不需要为 Responses 做特殊改动。

## M7：Skills

目标：在协议和工具基础稳定后，增加本地 skill 加载。

交付物：

- 配置层定义 `skill_dir`，默认指向配置根目录下的 `skills`。
- 推荐本地目录布局：`.agents/skills/<skill_id>/SKILL.md`。
- skill discovery，只发现 `skill_dir` 下包含 `SKILL.md` 的直接子目录。
- `SKILL.md` 读取，支持可选 YAML frontmatter 中的 `name` 和 `description`。
- skill 显式启用或选择。
- instruction composition。

M7 当前实现只覆盖配置目录下的本地 skills：通过 `skills.enabled` 或 `sai chat
--enable-skills` 显式启用，`--enable-skills` 覆盖配置，`--disable-skills` 本次运行禁用
所有 skills。已启用 skill 的 instructions 作为 developer message 注入在内置 system
和 `AGENTS.md` 之后、用户 prompt 之前；多个 skill 按 enabled 列表顺序注入。M7 不读取
用户目录，不实现 marketplace、递归 skill discovery、plugin lifecycle 或复杂依赖解析。

验证：

第一小步验证：

- discovery 单元测试能稳定列出 `skill_dir` 下含 `SKILL.md` 的直接子目录。
- `SKILL.md` loader 能读取 frontmatter 中的 `name` / `description` 和正文。
- 无 frontmatter 时，loader 使用 skill id 作为名称、description 为空、全文作为 instructions。
- config test 能证明 `skill_dir` 默认路径和自定义路径都相对配置根目录解析。

最终态验证：

- 本地测试 skill 能改变模型 instructions。
- skill 可以关闭。
- 缺失或格式错误的 skill 有清晰错误。
- CLI fake server 测试覆盖 skill 注入顺序、CLI 覆盖配置、disablement、unknown id 和
  malformed frontmatter。

## M8：CLI Help / Discoverability

目标：为常见 CLI 入口提供可发现的 help/usage 行为，同时保持 `sai` 的纯 CLI 形态。

交付物：

- root help：`sai -h`、`sai --help`、`sai help`。
- simple command help：`sai version -h`、`sai version --help`、
  `sai help version`。
- command help：`sai chat -h`、`sai chat --help`、`sai help chat`，展示可选
  prompt 和 `--quit`。
- group help：`sai config -h`、`sai models -h`、`sai tools -h`、`sai mcp -h`，
  以及对应的 `sai help config`、`sai help models`、`sai help tools`、
  `sai help mcp`。
- nested command help：`sai config show -h`、`sai models list -h`、
  `sai tools list -h`、`sai mcp list -h`，以及对应的 `sai help config show`、
  `sai help models list`、`sai help tools list`、`sai help mcp list`。
- `sai tools list` 静态列出内置工具，不加载配置或 provider/API key。
- help 输出写到 stdout，exit code 为 0。
- help 在配置加载前完成，不读取 `.agents` 配置、不解析 API key，也不泄露 secrets。
- 未知命令和错误参数继续 exit code 1，并给出可读错误和 help 提示。
- root help 不列 `run`；`sai run ...`、`sai run -h` 和 `sai help run` 不再作为正常入口，
  返回 unknown command/topic 或不支持错误，且不展示 run 专用 usage。

验证：

- CLI 单元测试覆盖 root help、simple command help、group help、chat help、
  nested command help 和无需配置文件的 help 路径。
- CLI 单元测试覆盖 `sai tools list` 无需配置文件。
- CLI 单元测试覆盖未知命令错误提示。
- CLI 单元测试覆盖 `run` 不可用、`chat --quit` 缺 prompt 的错误提示。
- `go test ./...` 通过。
- `git diff --check` 通过。

## M9：Reasoning Output Styling

目标：启用 reasoning 输出时，用终端灰色/暗色样式区分 reasoning 和最终输出，同时保持
默认隐藏 reasoning 的行为不变。

交付物：

- `--show-reasoning` 和配置 `agent.show_reasoning: true` 显示 reasoning 时，在支持颜色
  的终端 stdout 上用 ANSI 暗灰色输出 reasoning。
- 可见 reasoning 块前输出独立 ASCII 标记行 `? reasoning`，且在已有正文未换行时先补
  换行。
- tool call 状态默认作为独立 stderr 行输出为 `! tool: <name>`；`read_file` / `list_files`
  状态显示目标路径/目录，`shell` 和 MCP tool 不显示 arguments，也不打印 tool result 正文。
- stdout 不是终端时不输出 ANSI，避免污染 pipe、redirect 和测试输出。
- `NO_COLOR` 环境变量存在且非空时禁用 ANSI 样式。
- reasoning 切换到最终 `text_delta` 前先 reset，再沿用已有 reasoning/final 换行逻辑。
- 只有 reasoning、没有最终文本时，stream 结束前也 reset，避免终端颜色泄漏。
- 日志继续记录原始事件，不包含 ANSI 样式。
- 不引入 TUI、不引入第三方依赖，不新增 `--no-color` 或改动 help/usage。

验证：

- 现有 reasoning 隐藏/显示 CLI 测试继续通过，`bytes.Buffer` 默认输出无 ANSI。
- 单元测试覆盖强制 color option 时 reasoning 被 `\x1b[90m` 和 `\x1b[0m` 包裹，最终
  text 在 reset 后输出，且 reasoning/final 换行正确。
- 单元测试覆盖 `NO_COLOR` 和非终端 stdout 的颜色禁用判断。
- 单元测试覆盖只有 reasoning 没有最终文本时仍输出 reset。
- `gofmt -w internal/cli/cli.go internal/cli/cli_test.go` 通过。
- `go test ./...` 通过。
- `git diff --check` 通过。

## M10：CLI Chat REPL

目标：实现克制版 `sai chat`，让多轮命令行会话可用，同时继续保持纯 CLI、无 TUI、
无长期记忆的边界。

交付物：

- Agent 层新增成功一轮结束后返回 updated messages 的 streaming API，同时保留
  `agent.Stream` 兼容内部单轮调用。
- updated messages 包含原有 messages、当前 user message、assistant final text、
  assistant tool calls 和 tool result messages。
- `sai chat [flags] [--quit] ["prompt"]` 逐行读取 stdin；空白行忽略，`/exit`、`/quit`
  或 EOF 正常退出。
- 有初始 prompt 时先执行完整一轮 agent loop；无 `--quit` 则继续进入 REPL，带
  `--quit` 则完成后退出；`--quit` 无 prompt 为用法错误。
- `sai chat` 会话开始时固定 provider/model/tools/MCP/skills/show-reasoning，使用
  同一套配置选择规则和 flags：`--provider`、`--model`、`--show-reasoning`、
  `--verbose`、`--enable-tools`、`--enable-skills`、`--disable-skills`、`--enable-mcp`。
- 参数解析使用统一混排规则：跳过已知 flag 及其 value 后的第一个非 flag token 是命令；
  命令前后 flags 可混排，`--config-dir` 可放在命令后，`--` 后的 token 全部作为
  positional。
- 每轮模型输出继续 streaming 到 stdout；prompt 写到 stderr；chat 成功轮次在输出末尾
  缺少换行时补一个换行，避免下一个 REPL prompt 和模型输出粘在一起。
- 会话历史只保存在当前进程内；不落盘 chat history；JSONL 日志继续不记录完整
  prompt、response 或 tool result 正文。
- MCP stdio server 在 chat 会话开始时启动，并在退出时关闭。
- 支持 `sai chat -h`、`sai chat --help`、`sai help chat`，help 在配置加载前完成。

验证：

- Agent 单元测试覆盖无工具单轮追加 assistant final message。
- Agent 单元测试覆盖 tool call 后 result messages 包含 assistant tool call、tool result
  和最终 assistant text。
- CLI 单元测试覆盖 chat help 不加载配置、`/exit` 正常退出、初始 prompt + `--quit`、
  初始 prompt 后继续 REPL、两轮 history、tool call history、prompt 写 stderr 和错误参数
  help hint。
- `gofmt -w internal/agent/agent.go internal/agent/agent_test.go internal/cli/cli.go internal/cli/cli_test.go` 通过。
- `go test ./...` 通过。
- `git diff --check` 通过。
