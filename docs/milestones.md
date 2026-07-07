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
- 支持默认从启动时当前工作目录下的 `.agents/${arg[0]}.yaml` 读取根配置文件；普通
  `sai` 二进制默认读取 `.agents/sai.yaml`。
- 支持通过 `--config <file>` 显式指定根配置文件。
- 支持从所选根配置文件读取全局配置。
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
- `sai config show --config ./example-config/sai.yaml` 能从指定根配置文件读取配置。
- `sai models list` 能列出配置中的 provider/model。
- `sai` 在启动目录存在 `AGENTS.md` 时能加载项目指令。

## M1：OpenAI-Compatible Streaming

目标：从 OpenAI-compatible Chat Completions endpoint 流式输出文本。

交付物：

- OpenAI-compatible Chat Completions adapter。
- SSE parser，支持 `data:` event 和 `[DONE]`。
- 处理 `delta.content`。
- 处理 `delta.reasoning_content`。
- `sai chat --prompt "prompt" --quit` 单轮命令形态。
- 会话开始时根据 `--provider` 和 `--model` 选择模型。
- 将启动目录的 `AGENTS.md` 内容加入本次会话上下文。
- `--show-reasoning` 参数，并在 reasoning 输出结束后换行再输出最终消息。

验证：

- `sai chat --quit --provider paperhub --prompt "你是谁？"` 能流式输出可见文本。
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

- Windows 单文件可执行程序能运行 `sai chat --prompt "prompt" --quit`。
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

- Anthropic model profile `type` 配置。
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

- Responses model profile `type` 配置。
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

- 配置层定义 `skill_dirs`，默认等价于 `skill_dirs: [skills]`；相对路径基于根配置文件所在目录解析，
  绝对路径保持不变。
- 推荐本地目录布局：`.agents/skills/<skill_id>/SKILL.md`。
- skill discovery，按配置顺序扫描多个目录，每个目录只发现包含 `SKILL.md` 的直接子目录。
- 同一目录内按确定性 discovery 顺序加载，跨目录保留配置的目录顺序。
- 重复 skill id 跨 `skill_dirs` 目录出现时作为配置错误。
- `SKILL.md` 读取，支持可选 YAML frontmatter 中的 `name`、`description` 和
  `disable-model-invocation`。
- 通过 `disable-model-invocation: true` 对单个 skill 关闭模型上下文注入；缺失或为
  `false` 时正常加载。
- instruction composition。

M7 当前实现只覆盖 `skill_dirs` 中配置的本地 skills：默认等价于 `skill_dirs: [skills]`，
按配置顺序扫描多个目录，每个目录只发现包含 `SKILL.md` 的直接子目录。同一目录内按确定性
discovery 顺序加载，跨目录保留配置的目录顺序；重复 skill id 是配置错误。发现到的 skill
默认将其 instructions 作为 developer message 注入在内置 system 和项目指令之后、用户
prompt 之前。M7 时项目指令是启动目录 `AGENTS.md`；M18 后，项目指令是
`agent.instruction_files` 成功加载的文件。若某个 `SKILL.md` frontmatter 设置
`disable-model-invocation: true`，该 skill 不注入模型上下文；缺失该字段或设置为 `false`
表示正常加载。M7 不读取用户目录，不实现
marketplace、递归 skill discovery、plugin lifecycle 或复杂依赖解析。

验证：

第一小步验证：

- discovery 单元测试能稳定列出 `skill_dirs` 指向目录下含 `SKILL.md` 的直接子目录，并保留
  配置的目录顺序。
- `SKILL.md` loader 能读取 frontmatter 中的 `name` / `description` /
  `disable-model-invocation` 和正文。
- 无 frontmatter 时，loader 使用 skill id 作为名称、description 为空、全文作为 instructions。
- config test 能证明 `skill_dirs` 默认值、自定义多目录路径和绝对路径解析正确。

最终态验证：

- 本地测试 skill 能改变模型 instructions。
- skill 可以通过 `disable-model-invocation: true` 关闭模型上下文注入。
- 缺失、格式错误或重复 id 的 skill 有清晰错误。
- CLI fake server 测试覆盖 skill 注入顺序、frontmatter opt-out 和 malformed frontmatter。

## M8：CLI Help / Discoverability

目标：为常见 CLI 入口提供可发现的 help/usage 行为，同时保持 `sai` 的纯 CLI 形态。

交付物：

- root help：`sai -h`、`sai --help`、`sai help`。
- simple command help：`sai version -h`、`sai version --help`、
  `sai help version`。
- command help：`sai chat -h`、`sai chat --help`、`sai help chat`，展示可选
  `--prompt` 和 `--quit`。
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
- CLI 单元测试覆盖 `run` 不可用、`chat --quit` 缺 `--prompt` 的错误提示。
- `go test ./...` 通过。
- `git diff --check` 通过。

## M9：Reasoning Output Styling

目标：启用 reasoning 输出时，用终端灰色/暗色样式区分 reasoning 和最终输出，同时保持
默认隐藏 reasoning 的行为不变。

交付物：

- `--show-reasoning` 和配置 `agent.show_reasoning: true` 显示 reasoning 时，在支持颜色
  的终端 stdout 上用 ANSI 暗灰色输出 reasoning。
- 可见 reasoning 不输出 marker；在已有正文未换行时先补换行。
- tool call 状态默认作为独立 stderr 行输出为 `tool: <name> [path]`；`read_file` /
  `list_files` 状态显示目标路径/目录，`shell` 和 MCP tool 不显示 arguments，也不打印
  tool result 正文。
- stdout 不是终端时不输出 ANSI，避免污染 pipe、redirect 和测试输出。
- `NO_COLOR` 环境变量存在且非空时禁用 ANSI 样式。
- 支持颜色的 stderr tool 状态显式使用 muted 样式并在每行后 reset，不依赖 reasoning
  状态泄漏。
- reasoning 切换到 tool 状态、最终 `text_delta`、error 或 stream end 前先 reset，再沿用
  已有 reasoning/final 换行逻辑。
- 只有 reasoning、没有最终文本时，stream 结束前也 reset，避免终端颜色泄漏。
- 日志继续记录原始事件，不包含 ANSI 样式。
- 不引入 TUI、不引入第三方依赖，不新增 `--no-color` 或改动 help/usage。

验证：

- 现有 reasoning 隐藏/显示 CLI 测试继续通过，`bytes.Buffer` 默认输出无 ANSI。
- 单元测试覆盖强制 color option 时 reasoning 被 `\x1b[90m` 和 `\x1b[0m` 包裹，最终
  text 在 reset 后输出，且没有 marker，reasoning/final 换行正确。
- 单元测试覆盖 reasoning 后接 tool 时 stdout 先 reset，stderr tool 状态自己使用 muted
  样式并 reset；tool 后接 final 时 final 不继承灰色。
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
- `sai [chat] [flags] [--prompt "prompt"] [--quit]` 逐行读取 stdin；没有命令 token 时默认进入
  `chat`；空白行忽略，`/exit`、`/quit`
  或 EOF 正常退出。
- 有 `--prompt` 时先执行完整一轮 agent loop；无 `--quit` 则继续进入 REPL，带
  `--quit` 则完成后退出；`--quit` 无 `--prompt` 为用法错误。
- `sai chat` 会话开始时固定 provider/model/tools/MCP/loaded skills/show-reasoning，使用
  同一套配置选择规则和 flags：`--provider`、`--model`、`--show-reasoning`、
  `--verbose`、`--enable-tools`、`--enable-mcp`。
- 参数解析使用统一混排规则：跳过已知 flag 及其 value 后的第一个非 flag token 是命令；
  命令前后 flags 可混排，`--config <file>` 可放在命令后，`--` 后的 token 全部作为
  positional；没有命令 token 时默认执行 `chat`；chat 不把 positional 参数作为初始 prompt。
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
- CLI 单元测试覆盖 chat help 不加载配置、无命令默认 chat、`/exit` 正常退出、`--prompt` + `--quit`、
  `--prompt` 后继续 REPL、两轮 history、tool call history、prompt 写 stderr 和错误参数
  help hint。
- `gofmt -w internal/agent/agent.go internal/agent/agent_test.go internal/cli/cli.go internal/cli/cli_test.go` 通过。
- `go test ./...` 通过。
- `git diff --check` 通过。

## M11：Reliability

目标：补齐长时间运行和异常路径下的可靠性，使 `sai chat`、provider stream、MCP 和
本地子进程在取消、超时和可恢复错误后都有清晰边界。

交付物：

- Ctrl+C / interrupt 统一进入 context cancel 流程，当前模型请求、shell 工具和 MCP
  stdio 子进程都能感知并退出。
- `sai chat` / agent 正在执行一轮时，Ctrl+C 只取消当前 active turn；已完成的外部副作用不回滚，
  取消完成后回到 chat 输入状态，不退出整个 CLI 进程。
- 同一 active turn 取消完成前，短时间重复 Ctrl+C 仍聚焦于取消当前轮次，不直接退出 chat session；
  hard exit 或 session/process cancel 应保留给 idle 状态或单独显式行为。
- idle 输入状态下的 Ctrl+C 可继续沿用现有 CLI / terminal 退出行为，除非实现时发现项目已有更严格约定。
- `--quit` 单轮运行中 Ctrl+C 仍应取消当前 active turn；因为没有 REPL 输入状态可返回，取消后可以结束进程。
- 明确 chat runtime 的 context lifecycle：会话级 context、单次模型请求 context 和工具
  执行 context 不互相泄漏。
- HTTP request timeout，避免连接或首包长期挂起。
- stream idle timeout，避免 SSE 已建立但长时间没有事件时无限等待。
- 429 和 5xx 的有界 retry 策略，包含最大次数、退避和可读错误。
- `sai chat` 在可恢复请求错误后不伪造 assistant 历史，并回到 prompt 允许用户继续输入。
- MCP stdio server 在正常退出、错误退出、Ctrl+C 和 context cancel 时都关闭子进程。
- `shell` 工具在取消或超时时关闭子进程并回收资源。
- JSONL logger 在正常退出、错误退出和 Ctrl+C 时 flush / close；flush 失败有可诊断错误。

验证：

- 单元测试覆盖 context cancel 后 provider request、tool execution 和 logger close 的调用边界。
- CLI 测试覆盖交互式 `sai chat` active turn 收到 Ctrl+C 后取消当前轮次、回到 prompt，并且不追加成功
  assistant history。
- CLI 或集成测试覆盖同一 active turn 取消完成前短时间重复 Ctrl+C 不直接退出 chat session，取消完成后仍回到
  prompt。
- CLI 或集成测试覆盖 idle 输入状态 Ctrl+C 不被 active-turn cancel 逻辑误处理。
- CLI 或集成测试覆盖 `--quit` active turn Ctrl+C 取消后可以结束进程，并且不伪造成功 assistant history。
- fake HTTP server 测试覆盖 request timeout、stream idle timeout、429 retry 和 5xx retry。
- CLI 测试覆盖 `sai chat` 单轮可恢复错误后回到 prompt，且错误轮次不追加成功 assistant
  history。
- fake MCP server / shell 测试覆盖取消和退出时无遗留子进程。
- `go test ./...` 通过。
- `git diff --check` 通过。

## M12：Editing Tools

目标：在读取和 shell 之外，增加受显式启用控制的文件编辑能力，同时保持默认安全边界和
克制的终端状态输出。

交付物：

- 新增 `write_file` 工具，用于覆盖写入文件。
- 新增 `edit_file` 或 patch-style 编辑工具，用于局部编辑已有文件。
- 编辑工具默认不启用，只有出现在 `tools.enabled` 或 `--enable-tools` 中才暴露给模型。
- `sai tools list` 静态列出新增编辑工具，但不代表默认启用。
- 编辑工具状态继续写 stderr，保留简短安全状态；不打印完整写入内容、patch 正文或文件
  内容。
- 路径解析沿用现有工具的启动目录边界和错误风格；路径错误给出可读错误。
- 工具结果消息只包含模型继续推理所需的摘要和错误，不向终端泄露完整内容。

验证：

- 单元测试覆盖 `write_file` 覆盖写入。
- 单元测试覆盖 `edit_file` 或 patch-style 工具的局部编辑。
- 单元测试覆盖路径不存在、路径非法或目录/文件类型不匹配时的错误。
- CLI / registry 测试覆盖未启用时不暴露编辑工具。
- CLI / registry 测试覆盖 `tools.enabled` 和 `--enable-tools` 启用后才暴露编辑工具。
- 状态输出测试覆盖不打印完整写入内容、patch 正文或 tool result 正文。
- `go test ./...` 通过。
- `git diff --check` 通过。

## M13：Resumable Sessions

目标：在现有 JSONL 日志之外，增加显式 opt-in 的完整会话保存和恢复能力；默认继续关闭，
避免无意保存敏感 prompt、response 和 tool result。

交付物：

- 明确区分 JSONL session log / transcript 和 resumable session：前者用于事件诊断，仍不
  记录完整 prompt、response 或 tool result 正文；后者用于可靠 resume，必须保存完整上下文。
- `sessions.enabled: false` 作为默认配置；未显式启用时不保存完整会话上下文。
- 启用后保存可恢复 session id、创建时间、更新时间和版本信息。
- 启用后保存 provider、model、model profile parameters、cwd、根配置文件路径和本次运行的关键
  runtime 选择。
- 启用后保存已启用 tools、MCP、loaded skills、reasoning 展示设置，以及对应的 CLI 覆盖来源。
- 启用后保存注入指令快照，或保存足以重建内置 system、项目指令和 loaded skills 的信息；
  若使用可重建信息，恢复时必须能检测源文件变化并给出清晰提示。M18 后，项目指令文件应按
  每个成功加载的文件记录独立 source/message 粒度。
- 启用后保存完整 messages：user messages、assistant final messages、assistant tool
  calls、tool result messages。
- 启用后保存完整 tool results，除非后续提供明确的不可恢复降级模式。
- 命令形态建议：`sai chat --save-session`、`sai chat --resume <id>`、
  `sai chat --continue`。
- session 管理命令：`sai sessions list`、`sai sessions show <id>`、
  `sai sessions delete <id>`、`sai sessions prune --keep N`。
- session 管理命令只输出元数据或删除确认，不打印完整 messages、prompt、assistant output
  或 tool result 正文。
- CLI、配置文档和错误信息都明确提示 resumable sessions 会保存敏感数据，包含完整
  prompt、assistant 输出和 tool result。

验证：

- 单元测试覆盖默认配置下不创建 resumable session。
- 集成测试覆盖 `--save-session` 保存完整 messages 后，`--resume <id>` 能继续同一上下文。
- 集成测试覆盖 `--continue` 选择最近的可恢复 session。
- 测试覆盖 provider/model/parameters、cwd、enabled tools/MCP、loaded skills、reasoning 和注入
  指令信息的保存与恢复。
- 测试覆盖 tool call history 和 tool result messages 恢复后能被 provider adapter 正确发送。
- CLI 测试覆盖 `sessions list/show/delete/prune` 的基本行为和敏感数据提示。
- `go test ./...` 通过。
- `git diff --check` 通过。

## M14：Context Window Management

目标：在多轮 chat 和可恢复 session 之后，增加上下文窗口管理，先采用保守策略，避免静默
丢弃关键 system、developer、tool schema 或 tool result 信息。

M14 当前保守第一版支持 model profile `context_window` 元数据；未配置时使用 `32000`
token 的估算默认窗口并记录来源为 `estimated`，显式配置时来源为 `configured`。每次
provider 请求前估算 request messages 和 tool schemas 的输入 tokens；达到窗口 80% 时向
stderr 输出一次只含 token 数/窗口的 warning，达到或超过窗口时拒绝发起 provider 请求。
provider usage event 优先用于 tracking；缺失时成功 stream 结束后记录 fallback estimate。
当前不会自动摘要、截断或丢弃任何上下文，后续摘要/截断策略必须单独设计并测试。
resumable session 保存 context management metadata，恢复后继续用该 metadata 判断预算。

交付物：

- token budget / usage tracking，优先使用 provider 返回的 usage；缺失时使用保守估算。
- 会话开始时记录模型 context window 配置或估算值。
- 接近 context window 时向 stderr 给出清晰警告。
- 达到预算前拒绝继续或要求用户选择处理方式，不静默截断关键上下文。
- 初始策略先保守：保留内置 system、项目指令、loaded skills、tool/MCP schema、
  全部 user/assistant 消息和 tool result，不自动摘要或截断。M18 后，项目指令是
  `agent.instruction_files` 成功加载的文件列表。
- 记录后续截断或摘要策略边界；摘要进入自动路径前必须有测试覆盖和可解释边界。
- resumable session 中记录 context management metadata，恢复后能继续判断预算。

验证：

- 单元测试覆盖 usage tracking 和预算计算。
- fake provider 测试覆盖 provider usage 缺失时的保守估算。
- CLI 测试覆盖接近窗口时的 stderr 警告。
- 测试覆盖不会静默丢弃 system/developer/tool schema 信息。
- 测试覆盖达到预算时给出可读错误或明确下一步提示。
- `go test ./...` 通过。
- `git diff --check` 通过。

## M14 后续待办：配置覆盖和 session 提示时机

这些需求已作为 M14 后续小任务完成：

- `agent.show_reasoning` 和 `sessions.enabled` 分别作为 reasoning 展示和 save-session 的
  配置默认值。普通新 chat 中，bool flag 支持 `--show-reasoning=true/false` 和
  `--save-session=true/false` 双向覆盖配置；不带 `=false` 时等价于启用。
- `--resume` 和 `--continue` 不允许这类覆盖：恢复时使用之前 session 保存的 provider、
  model、model parameters、tools、MCP、skills、show_reasoning、save-session 等关键参数；
  如果 CLI 传入冲突覆盖，继续拒绝，包括 `--show-reasoning=false` 或
  `--save-session=false` 这类关键语义变化。
- 启用 session 保存时，首次敏感数据提示在 CLI runtime 准备完成后、读取用户输入或发起
  provider 请求前输出；整个进程只提示一次，且不打印正文敏感内容。

## M15：Input UX and Doctor

目标：改善输入和配置诊断体验，但继续保持纯 CLI，不恢复 `sai run`，不引入 TUI，也不把
Markdown 渲染纳入近期目标。

当前第一部分已实现输入 UX：多行 REPL、`--stdin` 和 `--file` 都作为 `sai chat`
能力提供。`sai chat --quit --stdin` / `sai --quit --stdin` 会读取完整 stdin 作为一次
prompt，`sai chat --quit --file prompt.md` 会读取完整文件作为一次 prompt；两者都复用
同一套 chat runtime、日志、工具/MCP/skills、session 保存和上下文窗口路径。交互式
REPL 的普通单行模式支持 `/usage`，只向 stderr 打印 context window / usage 元数据，不
请求模型、不记录 JSONL 事件、不打印正文敏感内容；多行块内 `/usage` 仍作为普通文本。
`sai run` 继续不可用。

当前最后一部分已实现配置健康检查命令 `sai doctor`，不提供别名或 `config check`。
`sai doctor` 只做本地配置、provider/model/API key、skills、MCP 配置、enabled tools
和日志目录可写性检查；它不发 provider HTTP 请求、不启动 MCP server、不运行模型。输出为
stdout 上的 `OK` / `WARN` / `ERROR` 行，任何 `ERROR` 都让退出码为 1，且输出必须脱敏。

交付物：

- 多行输入支持属于 `sai chat` 能力，和现有 REPL / `--quit` 语义兼容。
- stdin 输入和 file 输入都走 `sai chat --quit`，不恢复 `sai run`。
- 明确命令形态，例如 `sai chat --quit --stdin`、`sai chat --quit --file prompt.md`
  或等价的克制 CLI 设计。
- stdin/file 输入进入同一套 message 构造、provider 选择、tools/MCP 启用、skills loading 和日志路径。
- stdin/file 输入不改变 JSONL 日志默认边界；仍不记录完整 prompt、response 或 tool result。
- REPL `/usage` 展示 context window / usage 元数据，不请求 provider，不记录正文敏感内容。
- 增加配置健康检查命令 `sai doctor`，不新增别名或 `sai config check`。
- 健康检查覆盖所选根配置文件、provider 文件、默认 provider/model、API key 环境变量是否存在、
  skill_dirs、mcp_dir、enabled tools/MCP、skills discovery、重复 skill id 和日志目录可写性。
- 健康检查输出脱敏，不打印 API key 或其他敏感配置值实际值。
- 健康检查不发 HTTP 请求、不启动 MCP server、不运行模型。
- 不引入 TUI、不做 Markdown 渲染；Markdown 渲染最多作为远期低优先级非目标记录。

验证：

- CLI 测试覆盖多行输入。
- CLI 测试覆盖 stdin 输入通过 `sai chat --quit` 执行。
- CLI 测试覆盖 file 输入通过 `sai chat --quit` 执行。
- CLI 测试覆盖 `sai run` 仍不可用。
- CLI 测试覆盖 `sai doctor` 的成功、警告和错误输出。
- 测试覆盖健康检查不泄露敏感配置值。
- `go test ./...` 通过。
- `git diff --check` 通过。

## M16：Codex OAuth Multi-Provider Login

目标：支持 Codex subscription auth 的多 provider 登录和运行时调用，同时继续保持纯 CLI、
不读取用户目录里的 Codex 文件、不引入浏览器回调或无关功能。

交付物：

- 增加 `auth_dir` 配置，默认指向根配置文件所在目录下的 `auth`。
- 配置层识别 `openai-codex` model profile type。
- provider 配置支持 `auth_file`，相对 provider YAML 文件解析。
- `sai auth codex login --provider <name>` 使用 device flow 创建命名 provider。
- 默认 provider 名称是 `codex`。
- `--force` 才允许覆盖已有生成 provider YAML 或 auth token JSON；未传时开始登录前失败。
- login 生成 provider YAML 到 `provider_dir`，生成独立 token JSON 到 `auth_dir`。
- `codex`、`codex-work` 和 `codex-personal` 等 provider 可以共存，互不覆盖 token 文件。
- `openai-codex` 运行时复用 OpenAI Responses request / SSE / function tool mapping。
- `openai-codex` 运行时用 `auth_file` 中的 access token 发送 bearer auth，不使用 `api_key`。
- token 文件中存在 account id 时发送 `ChatGPT-Account-Id`。
- access token 过期时用 refresh token 刷新，并写回同一 token 文件。
- `config show`、verbose、日志和 HTTP 错误不泄露 access token、refresh token 或
  Authorization header。
- 不读取、不导入 `~/.codex/auth.json`。

验证：

- `sai auth codex login -h` 和 `sai help auth codex login` 不加载 provider secrets。
- CLI 测试使用 fake device/token endpoints，验证命名 provider 的 provider/auth 文件生成。
- 配置测试覆盖 `openai-codex` type、`auth_dir` 和 provider `auth_file` 解析。
- provider 测试覆盖 `Authorization: Bearer <access>`、`ChatGPT-Account-Id`、HTTP 错误脱敏
  和过期 token refresh 写回。
- 既有 `openai-responses` 测试继续不变。
- `gofmt`、`go test ./...` 和 `git diff --check` 通过。

## M17：Local Tool Ergonomics and Discovery

目标：在现有本地工具边界内补齐更好用的文件读取、发现、搜索和 shell 输出控制能力，帮助
agent 在大工作区内安全定位文本内容，同时不默认启用任何工具、不扩大工作区边界。

交付物：

- 增强 `read_file`，继续只读取工作区内文本文件。
- `read_file` 支持可选 `start_line`、`line_count` 和 `max_bytes` 参数：
  `start_line` 是 1-based 行号，`line_count` 必须大于 0，`max_bytes` 必须大于 0。
- `read_file` 不提供 byte offset / byte count 模式。
- `max_bytes` 同时适用于默认读取和行范围读取。
- 默认读取从文件开头开始，返回最多 `max_bytes` 字节。
- 只提供 `start_line` 时，从该行读取到 `max_bytes` 或 EOF。
- 只提供 `line_count` 时，从第 1 行开始最多返回指定行数。
- 同时提供 `start_line` 和 `line_count` 时，最多返回指定行数，同时仍受 `max_bytes` 限制。
- 行范围读取或因 `max_bytes` 导致内容不完整时，tool result 必须在正文前包含简短
  metadata，至少包含 path、有效 `start_line`、`lines_returned`、`max_bytes` 和
  `truncated=true/false`；截断时还必须告诉 agent 下一步如何继续读取。
- 行范围读取应尽量返回完整行；如果单行超过 `max_bytes`，返回该行前缀并标记
  `line_truncated=true`，同时提示 agent 增大 `max_bytes` 并从同一行重试。
- 小文件、非范围且未截断的完整读取可以继续返回原始文件内容，以保持兼容。
- 新增 `glob_files`，仅在工作区内执行 glob 搜索，返回稳定相对路径，支持
  `max_results`，并在结果截断时返回明确 metadata。
- 新增 `grep_files`，仅在工作区内执行文本搜索，支持 include / exclude globs；默认 literal
  搜索，可选 regex、大小写敏感和 context lines；支持 `max_results` 和 snippet limits，
  并在结果或 snippet 截断时返回明确 metadata。
- 增强 `shell`，支持可选 `timeout_ms` 和 `max_output_bytes`。
- `shell` 输出被 `max_output_bytes` 截断时，tool result 必须明确说明截断。
- `shell` 的 CLI tool status 行继续只显示工具名，不显示命令参数或任意 arguments。
- `sai tools list` 和 tool registry / schema 测试覆盖新增工具和新增参数。

验证：

- 聚焦单元测试覆盖 `read_file` 默认读取、行范围读取、`max_bytes` 截断、
  `line_truncated=true` 单长行场景和非法参数。
- 聚焦单元测试覆盖 `glob_files` 的工作区边界、稳定相对路径、`max_results` 和截断 metadata。
- 聚焦单元测试覆盖 `grep_files` 的 literal 默认行为、可选 regex、大小写敏感、include /
  exclude globs、context lines、snippet limits 和截断 metadata。
- 聚焦单元测试覆盖 `shell` 的 `timeout_ms`、`max_output_bytes`、输出截断 metadata，以及
  status 行不泄露命令参数。

## M18：Project Instruction Files

目标：把项目指令文件从固定启动目录 `AGENTS.md` 扩展为根配置字段
`agent.instruction_files`，同时保持省略配置时的兼容默认行为。

M18 当前已实现：根配置支持 `agent.instruction_files`，省略时兼容默认
`["$CWD/AGENTS.md"]`；显式空列表不加载项目指令；条目按列表顺序处理，支持任意文件名、
`$CWD` / `$CONFIG` / `$USER` / `$REPO` placeholder、普通 glob 和递归 `**/*.md` glob。
单个 pattern 的多个匹配按稳定 path sort 顺序加载，缺失文件跳过，无法解析 `$REPO` 的条目
跳过并向 stderr 输出不进入模型上下文的 warning。每个成功加载的项目指令文件作为独立
developer message 注入在内置基础约束之后、loaded skills 之前；resumable session 的
`instructions_snapshot` 保留这些独立 message 粒度，`instruction_sources` 保留对应来源。
完成 placeholder 展开和 glob 匹配后，多个 `agent.instruction_files` 条目或重叠 glob
匹配到同一个实际文件时，会按 canonical/clean 绝对文件路径去重，只加载第一次出现的文件；
后续重复匹配静默跳过，不输出 warning。

交付物：

- 根配置新增 `agent.instruction_files` 列表字段。
- 省略 `agent.instruction_files` 时，行为等价于 `["$CWD/AGENTS.md"]`。
- 列表条目按配置顺序加载，条目可指向任意文件名，不限于 `AGENTS.md`。
- 支持 placeholder：`$CWD` 为启动时当前工作目录，`$CONFIG` 为根配置文件所在目录，
  `$USER` 为用户 home 目录，`$REPO` 为从 `$CWD` 向上发现的 git repository root。
- 使用 `$REPO` 但无法解析 repository root 时，跳过该条目并输出 warning；warning 不进入
  模型上下文。
- 缺失的非 glob 文件按当前缺失 `AGENTS.md` 行为跳过。
- glob 支持普通 glob pattern 和 `**/*.md` 递归 pattern。
- 单个 pattern 匹配多个文件时，在该 pattern 内按稳定 path sort 顺序加载；不同 pattern
  之间保留列表顺序。
- 完成 placeholder 展开和 glob 匹配后，以解析后的 canonical/clean 绝对文件路径作为同一文件
  身份去重。
- 重复指向同一实际文件时只加载第一次出现的文件；第一次出现按列表顺序优先，同一个 glob
  pattern 内按稳定 path sort 顺序判断。
- 后续重复匹配静默跳过，不输出 warning；`$REPO` 无法解析时的 warning 行为不变。
- 成功加载的文件注入在原项目指令位置：`sai` 内置基础约束之后、loaded skills 和当前用户
  prompt 之前。
- 每个成功加载且去重后的唯一文件优先作为独立 developer instruction source/message 注入；session
  snapshot 或可重建信息也按单个文件记录来源。
- `sai config show`、verbose、日志和 warning 不打印项目指令正文。

验证：

- 配置测试覆盖省略 `agent.instruction_files` 时等价于 `$CWD/AGENTS.md`。
- 配置测试覆盖 `$CWD`、`$CONFIG`、`$USER` 和 `$REPO` placeholder 解析。
- 测试覆盖 `$REPO` 无法解析时跳过该条目、输出 warning，且 warning 不进入模型 context。
- 测试覆盖缺失非 glob 文件跳过。
- 测试覆盖普通 glob 和递归 `**/*.md` glob。
- 测试覆盖单个 pattern 多文件匹配时按稳定 path sort 加载。
- 测试覆盖 `$REPO/AGENTS.md` 与 `$CWD/AGENTS.md` 解析为同一路径时只加载一次。
- 测试覆盖重叠 glob 匹配同一实际文件时 first occurrence wins，后续重复匹配静默跳过且不输出
  warning。
- 测试覆盖去重后每个唯一项目指令文件仍是独立 developer message/source。
- CLI / fake server 测试覆盖多个项目指令文件按列表顺序注入，且每个文件是独立 developer
  message/source。
- 测试覆盖 loaded skills 仍注入在全部项目指令文件之后、当前用户 prompt 之前。
- 若启用 resumable session，测试覆盖项目指令文件 snapshot 或可重建信息保留单文件来源。
- `go test ./...` 通过。
- `git diff --check` 通过。

## M19：Async Subagent-as-Tool Runtime

目标：在保持纯 CLI 和现有单 agent loop 稳定的前提下，把配置好的 child agents 作为 parent
agent 的异步工具暴露出来。M19 只交付 async subagent runtime，不交付完整 multi-agent
orchestration。

交付物：

- 根配置文件支持 `subagents` 映射，形态为 `id -> relative config file path`。
- `subagents` 中的相对路径基于写出该配置项的父配置文件所在目录解析。
- subagent config 文件使用和 main config 相同的 `sai` schema，并通过类似 main agent 的
  runtime 准备路径启动。
- subagent config 可以指向自身或形成环，但 runtime 必须有递归深度、job 数量、等待时间和
  取消保护。
- 未配置 subagents 时，不向 parent agent 暴露任何 subagent tools。
- 配置 subagents 后，parent agent 自动获得 subagent tools，不需要把每个 subagent tool 再写进
  `tools.enabled`。
- parent prompt 注入已配置 subagent id 和短 description 列表。
- child agent 的 provider、model、tools、skills、MCP 和 prompt 来自自己的 config，不继承
  parent tools。
- subagent job 语义覆盖 `subagent_start`、send、cancel、close、status 和 wait。
- `subagent_start` 可选接受用户或模型提供的 display name / job name。
- 配置的 subagent id 仍是权限、配置文件和 runtime 能力选择依据；display name 只是 job
  metadata，不能选择未配置 agent 或改变 config/tools。
- display name 出现在 status、mailbox 和 observability 输出中，并可用于未来 GUI label。
- child job 异步运行时，parent 仍可继续接收用户输入并执行自己的 turns。
- child completion 通过 parent mailbox/runtime event 投递，在 parent turn 结束后交付；parent
  idle 时可自动 wake up 处理完成事件。
- 预留 config schema 中的 `prompt.system_prompt` 和 placeholder 能力；自定义 system prompt
  只能追加到内置约束之后，placeholder 必须来自固定白名单。

后续项：

- 从 async subagent runtime 演进到完整 multi-agent orchestration。
- DAG/workflow 执行模型。
- shared state / blackboard。
- structured result protocol。
- global budgets 和跨 agent 权限控制。
- conflict arbitration。
- observability。
- persistence/resume。

验证：

- 配置测试覆盖 `subagents` 映射解析、相对路径基准、缺失/空配置时不暴露 subagent tools。
- 配置测试覆盖 child config 使用同一 schema，并且 child 相对路径基于 child config 文件解析。
- runtime 测试覆盖配置 subagents 后 parent 自动获得 subagent tools。
- prompt composition 测试覆盖 parent prompt 注入 subagent id 和短 description。
- runtime 测试覆盖 child 不继承 parent tools，而是使用自己的 tools/skills/MCP/prompt。
- job 测试覆盖 `subagent_start`、send、status、wait、cancel、close 和错误状态。
- job 测试覆盖 `subagent_start` 可选 display name、display name 不影响 subagent id/config
  选择，以及不能借 display name 调用未配置 agent。
- status、mailbox 和 observability 测试覆盖 display name 透出。
- 并发/REPL 测试覆盖 child running 时 parent 仍能接收输入。
- mailbox 测试覆盖 child completion 在 parent turn 后交付，以及 idle auto wakeup。
- safeguard 测试覆盖 self-referential config、递归深度限制、job 数量限制和 wait timeout。
- prompt 配置测试覆盖 custom system prompt 追加语义和 placeholder 白名单拒绝未知占位符。
- `go test ./...` 和 `git diff --check` 通过。

## M20：Server-Owned Sessions and CLI Client

状态说明（2026-07-07）：M20 的 HTTP/WS server-owned session 产品路径已由 M22 删除。此节作为
历史设计和已完成切片记录保留；当前目标见 M22。

目标：把会话运行形态从本地进程内 `sai chat` 切换为 server-owned sessions。server 是唯一
session writer，CLI client 通过 HTTP API 和 WebSocket 操作会话。HTTP / WebSocket 语义保留
未来 Web GUI 需要的 filtered / paginated view，但 M20 不交付浏览器 Web GUI UI。

交付物：

- 新增 `sai server`，默认以前台进程启动本地 server，提供 HTTP API 和 WebSocket stream。
- 前台 `sai server` 阻塞到 server 退出，并支持 Ctrl+C 优雅关闭 listener、flush 必要 metadata、
  移除 registry 后退出 0。
- `sai server --background` 启动本地后台 server；父进程等待子进程完成 listen、写入 registry 且
  `/health` 可用后退出 0。
- `--background` 子进程 stdout/stderr 不长期占用调用它的终端，运行日志走 server 日志或诊断日志
  路径。
- `sai server` 支持 `--cwd` 指定 server 运行工作目录；省略时使用当前目录。
- `sai server` 支持 `--config`，相对路径基于 `--cwd` 解析。
- server identity 使用 canonical `cwd + config_path`。
- server 默认监听 `127.0.0.1:0`，由 OS 分配随机端口。
- `sai server` 支持 `--port N` 监听 `127.0.0.1:N`，`--port 0` 表示随机端口。
- `sai server` 支持高级 `--listen host:port`；MVP 默认只支持或只推荐 loopback 地址。
- 启动成功后写入 per-user registry，记录 canonical cwd、config path、addr、pid、token、
  started_at 和 version。
- registry 文件权限尽量限制为当前用户可读写；每个 server 生成随机 token。
- 写操作、debug 读取和 blob content 读取必须带 registry token。
- 重复启动同一 `cwd + config_path` 且监听参数一致时，提示 already running 并退出 0。
- 重复启动同一 `cwd + config_path` 但监听参数冲突时，返回冲突错误并退出非 0。
- 指定端口被其他进程占用时启动失败并退出非 0。
- client 发现 registry 记录后先调用 `/health`；stale 记录会被忽略或清理。
- `sai` 默认等价于 `sai attach`，从当前目录向上查找最近健康 server 并进入 attach REPL。
- `sai --cwd <path>` 从指定 cwd 向上查找最近健康 server。
- `sai attach <session-id>` 进入指定会话。
- `sai attach --new` 创建新会话并进入。
- `sai status` 查询最近 server 的 cwd、config、listen、pid、version、uptime、session 数和
  running turn 数后退出。
- `sai stop` 从当前目录向上查找最近健康 server，发送 shutdown 请求，等待退出并移除 registry。
- `sai stop --cwd <path>` 从指定 cwd 向上查找最近健康 server 并停止。
- stop 不删除 sessions、logs 或 blobs。
- `sai servers list` 列出 registry 中的本地 server 后退出。
- `sai sessions list` 和 `sai sessions show <id>` 通过 server API 查询，不直接读取 session 文件。
- 新增 `sai send <session-id> --prompt ...` 和 `sai send --new --prompt ...`，发起一轮后退出。
- 移除或隐藏独立进程内 `sai chat` 产品入口；server-owned session 是唯一推荐会话路径。
- server 提供 `GET /health`、`GET /server`、`POST /server/shutdown`、`GET /sessions`、
  `POST /sessions`、`GET /sessions/{id}`、`GET /sessions/{id}/items`、
  `POST /sessions/{id}/messages`、`POST /sessions/{id}/commands/compact`、
  `GET /sessions/{id}/items/{item_id}/content` 和 `WS /sessions/{id}/stream`。
- session metadata API 不返回完整 items；items API 支持 `before_seq`、`after_seq`、`limit`
  和 `view=chat|debug`。
- item content API 只能读取当前 session 中可达的 item content，不提供裸 blob hash 读取。
- 多个 client 可以同时连接同一个 session stream，并看到同一 running turn 的 transient events
  和 persisted events。
- 同一 session 同时只允许一个 running turn；busy 时再次发送 message 返回 conflict。
- shutdown 在 running turn 时返回明确错误或 conflict。
- attach REPL 中的 `/compact` 调用 server command API；多行文本中的 `/compact` 仍作为普通文本。
- CLI client 不直接读取 session 文件、blob 文件或修改 `ActiveHistory`。
- Web GUI UI、浏览器 routing、前端状态管理和样式实现属于未来工作，不属于 M20 交付物。

验证：

- API 测试覆盖 `/health`、`/server` 和 session metadata API 不返回完整 items。
- API 测试覆盖 `POST /server/shutdown` 在无 running turn 时触发优雅退出。
- API 测试覆盖 running turn 时 shutdown 返回明确错误或 conflict。
- API 测试覆盖 item pagination 的 `before_seq`、`after_seq`、`limit` 和 `view`。
- API 测试覆盖 `view=chat` 不返回 hidden compaction summary，`view=debug` 可返回 debug metadata。
- API 测试覆盖 `POST /sessions/{id}/messages` 在 session busy 时返回 conflict。
- API 测试覆盖 `POST /sessions/{id}/commands/compact` 只在 idle session 成功。
- WebSocket 测试覆盖 turn running 时多个 client 都收到 transient text deltas、tool status 和
  persisted item events。
- WebSocket 测试覆盖失败 turn 推送 `turn.failed`，且刷新后不出现失败 turn items。
- registry 测试覆盖 `sai server --cwd X` 写入 canonical cwd、config、addr、pid 和 token。
- registry 测试覆盖前台 `sai server --cwd X` 收到 Ctrl+C 后优雅退出并移除 registry。
- registry 测试覆盖 `sai server --cwd X --background` 后父进程返回且后台 server 健康可连接。
- registry 测试覆盖重复启动同一 server 的 already running 行为和端口冲突行为。
- discovery 测试覆盖 `sai` 从当前目录向上发现最近 server。
- discovery 测试覆盖 `sai --cwd X` 从 X 向上发现最近 server。
- CLI 测试覆盖 `sai attach <session-id>`、`sai attach --new`、`sai status`、
  `sai stop`、`sai servers list`、`sai sessions list/show` 和 `sai send`。
- CLI 测试覆盖 `sai stop` 移除 registry 且不删除 sessions、logs 或 blobs。
- CLI 测试覆盖没有可用 server 时给出启动提示。
- CLI 测试覆盖 stale registry 记录在 health check 失败后被忽略或清理。
- 安全测试覆盖非公开内容读取和写操作需要 registry token。
- Blob access 测试覆盖 item content endpoint 可按大小读取，且裸 hash 不能读取 blob。
- 不要求浏览器 Web GUI UI 测试；未来 GUI 必须复用 M20 API / WebSocket。
- `go test ./...` 和 `git diff --check` 通过。

## M21：Global Singleton Server and Explicit Project/Session Lifecycle

状态说明（2026-07-07）：后续实现不再继续推进 M21 的 HTTP/WS global server 产品路径。
本节作为历史设计和已完成切片记录保留；当前目标见 M22。

目标：直接替换 M20 的 scoped server identity。server 在同一 OS user、raw command basename 和
home namespace 下全局唯一，可以管理多个 project。project 和 session 都必须显式创建；CLI
client 需要 server 时 auto-start，并通过 explicit project/session API 操作 durable user-level
store。详细设计见 `docs/global-server-projects.md`，执行清单见
`docs/tasks/global-server-projects-checklist.md`。

交付物：

- 移除旧 scoped server behavior，不提供兼容层。
- 所有用户可见 help/docs/errors 使用 raw `argv[0]` basename；文档示例使用 `<cmd>`。
- 支持 `--home PATH`，并按 raw basename 派生 env var 后选择 home namespace；不同 home
  directories 是独立 singleton namespaces。
- server registry 和 durable data store 默认位于 user-level home namespace。
- registry 记录 `pid`、`base_url`、`token`、`version` 和 `started_at`；client 复用前
  health-check，stale registry 可覆盖，file lock 避免并发双启动。
- `<cmd> server` 前台启动 singleton，不绑定 cwd/project；`<cmd> server --background` 显式
  后台启动；`server status/stop` 不 auto-start。
- bare attach、send、project 和 session commands 需要 server 时 auto-start；help/version/server
  control commands 不 auto-start。
- project identity 只使用 canonical cwd root；project 必须显式创建，存入 user-level registry /
  data store，不写 project marker 文件。
- 从 effective cwd 向上查找 nearest registered ancestor project；nested projects 允许；duplicate
  exact canonical root 返回已有 project info 并退出 0。
- project name 只用于展示；project remove 默认 archive/hide，真实删除需要 `--delete-data`；
  running sessions 阻止 removal/deletion。
- session 必须显式创建；`--new` 是 create-and-attach shortcut；不隐式创建 project 或 session。
- config 属于 session，不属于 project；session create 记录 `config_path` 和关键 metadata，每个
  turn 重新读取该 `config_path`。
- `--config` 只允许创建新 session；existing session attach/send 传 `--config` 必须报错。
- existing session cwd 固定为 `created_cwd`；`--cwd` 只允许 project/session create 和 `--new`；
  existing session attach/send 传 `--cwd` 必须报错。
- 移除 hardcoded chat product entry，不作为 hidden alias。
- bare `<cmd>` 等价 attach：auto-start server，按 cwd 找 project，无 project/session 时失败，
  否则 attach 当前 project 最近 non-archived session；`<cmd> --new` 创建并 attach。
- primary session command shape 是 singular `session`；旧 `sessions` 最多作为 list alias。
- `session list` 默认当前 project non-archived，支持 `--project`、`--all-projects` 和
  `--archived`，按 `last_used_at desc`、`created_at desc` 排序。
- attach/send 支持 global explicit session id；未指定 id 时只选当前 project 最近 non-archived
  session；多个 observers 可 attach；同一 session 只有一个 active turn，busy 返回
  `session_busy` 且不选择其他 session；不同 sessions 可并发。
- shutdown 默认 immediate stop/cancel/cleanup；`--wait` drain 已开始 calls/turns、停止接受新
  turns 并支持 timeout；signals/Ctrl+C 使用 immediate stop；restart 后 running turns/sessions
  标记 interrupted，不自动 replay。
- API 使用 explicit project/session paths：`/projects`、`/projects/{project_id}/sessions`、
  `/sessions/{session_id}`、`/sessions/{session_id}/messages`、
  `/sessions/{session_id}/items`、`/sessions/{session_id}/content/{blob_hash}` 和
  `WS /sessions/{session_id}/stream`，另有 server-level health/server/shutdown。
- HTTP/WS 默认 loopback；`GET /health` 是 public loopback discovery endpoint，只返回 minimal
  non-sensitive liveness；其他 HTTP/WS endpoint 都要求 bearer token；第一版不实现多用户 login。
- durable store 使用 per-project 和 per-session directories；默认不写项目 repo；transcript 使用
  append-only JSONL，大内容使用 hash-addressed blobs，global blob dedupe 可接受，session seq
  支持 pagination，hidden summary/debug records 可被 GUI 过滤。
- future session-history query tool 仍是低优先级 out of scope。

验证：

- 测试覆盖 singleton per home namespace，以及不同 home directories independent。
- 测试覆盖 explicit project/session create、duplicate project root、nested discovery 和 missing
  project/session 失败。
- 测试覆盖 existing session 下 `--config` / `--cwd` rejection。
- 测试覆盖 direct replacement/no chat product entry 和 user-facing command basename/env var
  derivation。
- API 测试覆盖 explicit project/session paths、public minimal `/health`、其他 HTTP/WS endpoint
  的 bearer token requirement、session busy 和 concurrent different sessions。
- registry 测试覆盖 health-check reuse、stale overwrite 和 file lock double-start prevention。
- shutdown 测试覆盖 immediate、`--wait` drain、timeout、signal semantics 和 interrupted recovery。
- persistence 测试覆盖 JSONL/blob pagination、blob reachability 和 hidden/debug filtering。
- `go test ./...` 通过。
- `git diff --check` 通过。
- task checklist 中记录 smoke evidence。

## M22：Execution Library and Direct CLI (No HTTP Product Layer)

目标：删除当前产品路径里的 HTTP/WS client-server 层，将 project/session lifecycle 和 turn
execution 收敛为进程内 execution library。CLI 是展示层，直接调用 execution library；未来若需要跨
终端访问，可以另做薄 HTTP adapter，但该 adapter 必须复用同一个 execution library，且不属于本阶段。

交付物：

- execution library 以记录存储位置（home/storage root）作为唯一必需初始化参数。
- execution library 拥有 project store、session store、nearest project discovery、session
  selection、turn lock、event persistence、compaction、runtime metadata 和 interrupted recovery。
- CLI 只负责参数解析、交互输入和渲染，调用 execution library 的 DTO/event 接口。
- CLI 不直接操作 provider adapters、tool execution、project/session stores、blob store 或
  event projector；这些都由 execution library 拥有。
- 删除当前产品路径中的 `server` 前台/后台命令、auto-start、registry、bearer token、HTTP client
  helper、HTTP handlers 和 WebSocket stream。
- 保留展示/执行边界所需的 session event DTO；attach/send/new/project/session 命令使用同一套
  execution library API。
- 不恢复 hardcoded chat product entry，也不保留 hidden chat alias。
- future HTTP adapter、GUI 和跨终端共享能力均为 out of scope。

验证：

- CLI 测试覆盖 project/session create/list/show/remove、attach/send/new/compact，且不启动 server。
- 测试覆盖 CLI product path 不访问 registry、HTTP client、HTTP handlers 或 WebSocket stream。
- execution library 测试覆盖 storage root 初始化、project/session lifecycle、nearest project
  discovery、session selection、busy turn rejection、event streaming/persistence、compaction 和
  interrupted recovery。
- `rg` 检查确认产品入口没有 `server` command、registry discovery、HTTP client 和 WebSocket attach。
- `go test ./...` 通过。
- `git diff --check` 通过。
- task checklist 中记录 smoke evidence。
