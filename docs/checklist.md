# 开发 Checklist

这份清单用于实现阶段逐项勾选。每个检查项都应对应可观察行为或测试。

## 范围约束

- [x] v0.1 保持纯 CLI，不引入 TUI。
- [x] MVP 先完成 OpenAI-compatible Chat Completions 核心路径（M0-M3）。
- [x] MCP 是 MVP 后的 M4 能力。
- [x] Anthropic Messages 是 M5 能力，在核心 OpenAI-compatible 路径稳定后接入。
- [x] OpenAI Responses 是 M6 能力，在核心 OpenAI-compatible 路径稳定后接入。
- [x] Skills 是 M7 能力，当前仅覆盖配置目录下的本地 skills。
- [x] 日志、verbose、resolved config 中不打印 API key、Authorization header 或其他敏感配置值的实际值。

## M0：项目骨架

- [x] 初始化 Go module。
- [x] 添加 `sai` CLI 入口。
- [x] 添加 version 命令。
- [x] 添加 config package。
- [x] 使用 YAML 作为第一配置格式。
- [x] 默认使用启动时当前工作目录下的 `.agents` 作为配置根目录，并读取 `sai.yaml`。
- [x] 支持通过 `--config-dir` 指定配置根目录。
- [x] 暂时不读取或写入用户目录配置。
- [x] 支持每个 provider 一个独立 YAML 文件。
- [x] 支持一个 provider 声明多个 model profile。
- [x] 支持每个 model profile 设置自己的请求参数。
- [x] 默认读取启动目录下的 `AGENTS.md`。
- [x] 缺失 `AGENTS.md` 时继续执行。
- [x] `--config-dir` 不改变 `AGENTS.md` 查找位置。
- [x] 暂时不读取用户目录中的 `AGENTS.md`。
- [x] 实现指令优先级：`sai` 内置基础约束 > `AGENTS.md` > 当前用户 prompt。
- [x] 支持 JSONL 日志配置。
- [x] 添加 provider interface。
- [x] 添加内部 stream event 类型。
- [x] 添加 `sai models list`。
- [x] 添加初始测试。
- [x] 验证 `go test ./...`。

## M1：Streaming

- [x] 实现 OpenAI-compatible request body。
- [x] 从 provider 配置加载 `base_url` 和 `api_key`。
- [x] 在配置读取阶段解析 `api_key` 的 `$ENV_NAME` 敏感配置值。
- [x] 移除 `openai-chat` adapter 的通用 `APIKeyEnv` / `lookupEnv` 配置入口；仅在需要时保留协议特定默认环境变量。
- [x] 从 model profile 加载模型 id 和模型参数。
- [x] 添加 PaperHub provider 配置示例。
- [x] 支持会话开始时通过 `--provider` 和 `--model` 选择模型。
- [x] 未指定模型时使用全局默认 provider/model。
- [x] 默认 provider/model 无效时给出可读错误和可选列表。
- [x] 会话进行中不支持切换模型。
- [x] 将 `AGENTS.md` 内容加入本次会话上下文。
- [x] 实现 SSE scanner。
- [x] 解析 `data: [DONE]`。
- [x] 解析 `choices[].delta.content`。
- [x] 解析 `choices[].delta.reasoning_content`。
- [x] 默认隐藏 reasoning 输出。
- [x] 添加 `--show-reasoning`。
- [x] `--show-reasoning` 输出 reasoning 结束后强制换行，避免和最终消息混行。
- [x] 解析 final `usage`。
- [x] 使用 `PAPERHUB_API_KEY` 验证 PaperHub smoke test。
- [x] 验证 non-streaming fallback，或明确记录暂不支持。

## M2：Tool Calls

- [x] 定义内部 tool schema。
- [x] 定义 tool executor interface。
- [x] 添加 tool registry。
- [x] 将内部 tools 转换为 OpenAI-compatible `tools` payload。
- [x] 内置 `list_files`。
- [x] 内置 `read_file`。
- [x] 内置 `shell`。
- [x] 默认不启用任何工具。
- [x] 通过配置 `tools.enabled` 启用工具。
- [x] 通过 `--enable-tools` 覆盖配置中的 enabled tools。
- [x] `shell` 默认在启动目录执行命令。
- [x] 累积 streamed tool call arguments。
- [x] 执行完整 tool call。
- [x] 追加 tool result messages。
- [x] tool result 后继续 model loop。
- [x] 添加 `max_turns`。
- [x] 添加 partial JSON argument chunks 测试。
- [x] 添加 malformed tool arguments 测试。
- [x] 执行 PaperHub tool call smoke test，或记录不兼容限制。

## M3：打包

- [x] 添加 Windows、Linux、macOS build 命令。
- [x] 构建单文件可执行程序。
- [x] 添加 `--verbose`。
- [x] 添加 JSONL 日志，每次 `chat` 预计算独立 session 路径，并在首个日志事件发生时写入独立 session 目录。
- [x] 除 JSONL 日志外，不落盘会话历史或上下文快照。
- [x] v0.1 不记录完整 prompt、response、tool result 正文。
- [x] missing API key 有可读错误。
- [x] HTTP failure 有可读错误。
- [x] invalid SSE chunk 有可读错误。
- [x] 添加最小 README。
- [x] 验证 fresh checkout build。

## M4：MCP

- [x] 添加 `mcp/` 配置目录。
- [x] 每个 MCP server 使用一个 YAML 文件。
- [x] 读取 MCP 文件中的 `enabled` 字段。
- [x] 通过 `--enable-mcp` 覆盖 MCP 文件中的 `enabled` 字段。
- [x] 启动 stdio MCP server 进程。
- [x] 发送 MCP initialize request。
- [x] 列出 MCP tools。
- [x] 将 MCP tools 转换为内部 tool schema。
- [x] MCP tool 名称固定为 `mcp.<server>.<tool>`。
- [x] MCP tools 仍受 enabled tools 列表控制。
- [x] 将 tool call route 到 MCP。
- [x] 将 MCP tool result 回传给模型。
- [x] `sai` 退出时关闭 MCP server 进程。
- [x] 添加 fake MCP server 集成测试。

## 后续协议

- [x] 配置层识别 `anthropic-messages` model profile type。
- [x] 添加 Anthropic Messages provider 配置示例。
- [x] 实现 Anthropic Messages 文本 streaming runtime adapter。
- [x] 添加 Anthropic text streaming fixture tests。
- [x] 实现 Anthropic Messages tool use adapter。
- [x] 添加 Anthropic tool call fixture tests。
- [x] 在 provider interface 后添加 OpenAI Responses adapter。
- [x] 添加 Responses semantic streaming fixture tests。
- [x] 添加 Responses function call fixture tests。

## 后续 Skills

- [x] 定义 skill 目录结构。
- [x] 读取 `SKILL.md`。
- [x] 添加显式 skill activation。
- [x] 将 skill instructions 组合进 system/developer messages。
- [x] 添加 malformed skill 错误处理。
- [x] 添加 skill selection 和 disablement 测试。

## M8：CLI Help / Discoverability

- [x] 添加 root help：`sai -h`、`sai --help`、`sai help`。
- [x] 添加 simple command help：`sai version -h`、`sai version --help`、`sai help version`。
- [x] 添加 chat help：`sai chat -h`、`sai chat --help`、`sai help chat`，展示可选 prompt 和 `--quit`。
- [x] 添加 group help：`sai config -h`、`sai models -h`、`sai mcp -h`。
- [x] 添加 nested command help：`sai config show -h`、`sai models list -h`、`sai mcp list -h`。
- [x] 添加对应的 group 和 nested `sai help ...` 入口。
- [x] 添加 `sai tools list`，静态列出内置工具且不加载配置。
- [x] 添加 `sai tools` / `sai tools list` help，help 不加载配置。
- [x] help 输出到 stdout，exit code 为 0。
- [x] help 不加载配置、不解析敏感配置值。
- [x] 未知命令和错误参数继续 exit code 1，并包含可读 help 提示。
- [x] root help 不再列 `run`。
- [x] `sai run ...`、`sai run -h` 和 `sai help run` 不再作为正常入口，且不展示 run 专用 usage。
- [x] `sai chat --quit` 无 prompt 时包含 chat usage 或 help 提示。
- [x] 不引入 TUI 或第三方 CLI 框架。
- [x] 添加 CLI help 测试并验证 `go test ./...`。

## M9：Reasoning Output Styling

- [x] 默认继续隐藏 reasoning 输出。
- [x] `--show-reasoning` 和 `agent.show_reasoning: true` 显示 reasoning 时可使用终端暗灰色样式。
- [x] 可见 reasoning 不输出 marker，并保持 reasoning/final 换行边界。
- [x] tool call 状态以 `tool: <name> [path]` 独立写到 stderr。
- [x] `read_file` / `list_files` 状态显示目标路径/目录；`shell` 和 MCP tool 不显示 arguments。
- [x] tool 状态不打印 tool result 正文，stdout 不包含 tool 状态。
- [x] stdout 不是终端时不输出 ANSI。
- [x] `NO_COLOR` 存在且非空时不输出 ANSI。
- [x] 支持颜色的 stderr tool 状态显式使用 muted 样式并在每行后 reset。
- [x] 切换到 tool 状态、最终 `text_delta`、error 或 stream end 前先 reset，最终输出不继承 reasoning 颜色。
- [x] 只有 reasoning、没有最终文本时，stream 结束前 reset。
- [x] JSONL 日志继续记录原始事件，不包含 ANSI 样式。
- [x] 不引入 TUI、不引入第三方依赖，不新增 `--no-color` 或改动 help/usage。
- [x] 添加 reasoning 样式测试并验证 `go test ./...`。

## M10：CLI Chat REPL

- [x] 保留 `agent.Stream` 兼容内部单轮调用。
- [x] 新增 agent streaming result API，成功一轮结束后返回 updated messages。
- [x] 无工具单轮 result messages 追加 assistant final message。
- [x] tool call 单轮 result messages 包含 assistant tool call、tool result 和最终 assistant text。
- [x] model stream error 不伪造成功 assistant 历史，继续通过 error event 失败。
- [x] 添加 `sai [chat] [flags] [--prompt text] [--quit]`，没有命令 token 时默认进入 chat。
- [x] `sai chat` 会话开始时固定 provider/model/tools/MCP/skills/show-reasoning。
- [x] `sai chat` 支持 `--provider`、`--model`、`--show-reasoning`、`--verbose`、`--enable-tools`、`--enable-skills`、`--disable-skills`、`--enable-mcp` 语义。
- [x] 有 `--prompt` 且无 `--quit` 时，先跑完初始 prompt 再进入 REPL，历史保留首轮 user、assistant 和 tool messages。
- [x] 有 `--prompt` 且有 `--quit` 时，跑完这一轮后退出，不进入 REPL。
- [x] 根层用“跳过已知 flag 及其 value 后的第一个非 flag token”识别命令；命令前后 flags 可混排，全局 `--config-dir` 可放在命令后，没有命令 token 时默认执行 chat，help 混排不加载配置，`--` 后的 token 全部作为 positional，chat 初始 prompt 使用 `--prompt`。
- [x] stdin 逐行读取用户输入，空白行忽略。
- [x] `/exit`、`/quit` 和 EOF 正常退出。
- [x] prompt 写到 stderr，不污染 stdout。
- [x] 每轮继续用 `writeStream` streaming 到 stdout。
- [x] chat 成功轮次在 assistant 输出缺少换行时补换行，避免下一个 REPL prompt 和模型输出粘在一起。
- [x] 会话历史只保存在进程内，不落盘 chat history。
- [x] JSONL 日志继续不记录完整 prompt、response、tool result 正文。
- [x] MCP server 在 chat 会话开始时启动，退出时关闭。
- [x] chat 单轮出错后 exit code 1，不继续下一轮。
- [x] root help 增加 `chat`。
- [x] 支持 `sai chat -h`、`sai chat --help`、`sai help chat`，help 不加载配置。
- [x] chat 未知/错误参数包含 `Run "sai help chat" for usage.`。
- [x] 添加可注入 stdin 的 CLI 测试入口。
- [x] 添加默认 chat、chat help、退出、多轮 history、tool history、prompt stderr、错误参数测试。

## M11：Reliability

- [x] Ctrl+C / interrupt 统一进入 context cancel 流程。
- [x] 明确 chat runtime 的 session/request/tool context lifecycle。
- [x] HTTP request 支持 timeout。
- [x] streaming SSE 支持 idle timeout。
- [x] 429 支持有界 retry 和可读最终错误。
- [x] 5xx 支持有界 retry 和可读最终错误。
- [x] `sai chat` 可恢复请求错误后不追加成功 assistant history。
- [x] `sai chat` 可恢复请求错误后回到 prompt 允许继续输入。
- [x] MCP stdio server 在正常退出、错误退出、Ctrl+C 和 cancel 时关闭子进程。
- [x] `shell` 工具在取消或超时时关闭子进程并回收资源。
- [x] JSONL logger 在正常退出、错误退出和 Ctrl+C 时 flush / close。
- [x] 添加 timeout、idle timeout、retry、cancel 和 logger flush 测试。

## M12：Editing Tools

- [x] 新增 `write_file` 工具。
- [x] 新增 `edit_file` 或 patch-style 编辑工具。
- [x] 编辑工具默认不启用。
- [x] 编辑工具仅通过 `tools.enabled` 或 `--enable-tools` 暴露给模型。
- [x] `sai tools list` 静态列出新增编辑工具。
- [x] 编辑工具状态写 stderr，不打印完整写入内容、patch 正文或文件内容。
- [x] 工具结果消息只包含必要摘要和错误，不向终端泄露完整内容。
- [x] 覆盖写入测试通过。
- [x] 局部编辑测试通过。
- [x] 路径错误测试通过。
- [x] 未启用时 registry / request payload 不暴露编辑工具。
- [x] 启用后 registry / request payload 暴露对应编辑工具。

## M13：Resumable Sessions

- [x] 区分 JSONL session log / transcript 和 resumable session。
- [x] 默认 `sessions.enabled: false`，不保存完整会话上下文。
- [x] 启用后保存可恢复 session id、创建时间、更新时间和版本信息。
- [x] 启用后保存 provider、model、model profile parameters、cwd 和配置根目录。
- [x] 启用后保存 enabled tools、MCP、skills 和 reasoning 展示设置。
- [x] 启用后保存注入指令快照，或保存可重建信息并能检测源文件变化。
- [x] 启用后保存完整 user messages。
- [x] 启用后保存完整 assistant final messages。
- [x] 启用后保存完整 assistant tool calls。
- [x] 启用后保存完整 tool result messages。
- [x] `sai chat --save-session` 可保存完整可恢复 session。
- [x] `sai chat --resume <id>` 可恢复指定 session。
- [x] `sai chat --continue` 可继续最近 session。
- [x] `sai sessions list` 可列出 sessions。
- [x] `sai sessions show <id>` 可展示 session 元数据并提示敏感数据风险。
- [x] `sai sessions delete <id>` 可删除指定 session。
- [x] `sai sessions prune` 可清理旧 sessions。
- [x] 文档和 CLI 输出提示 sessions 保存完整 prompt、assistant 输出和 tool result。

## M14：Context Window Management

- [x] 增加 token budget / usage tracking。
- [x] 优先使用 provider 返回的 usage。
- [x] usage 缺失时使用保守估算。
- [x] 会话开始时记录模型 context window 配置或估算值。
- [x] 接近 context window 时向 stderr 警告。
- [x] 达到预算前拒绝继续或要求用户选择处理方式。
- [x] 不静默截断 system/developer/tool schema 信息。
- [x] 保守保留内置 system、`AGENTS.md`、enabled skills、tool/MCP schema、全部消息和 tool result。
- [x] 设计截断或摘要策略并记录边界。
- [x] resumable session 保存 context management metadata。
- [x] 添加 usage tracking、预算警告和关键上下文保留测试。

## M14 后续待办：配置覆盖和 session 提示时机

- [x] 配置文件支持 `show_reasoning` 和 save-session 默认值，普通新 chat 可由 CLI 显式覆盖。
- [x] `--resume` / `--continue` 使用已保存 session 的 provider、model、参数、tools、MCP、skills、show_reasoning、save-session 等关键参数，CLI 冲突覆盖继续拒绝。
- [x] 启用 session 保存时，首次敏感数据提示在 CLI 启动完成后、读取用户输入前输出，而不是等到第一次 provider 请求。

## M15：Input UX and Doctor

- [x] 多行输入作为 `sai chat` 能力实现。
- [x] stdin 输入走 `sai chat --quit`。
- [x] file 输入走 `sai chat --quit`。
- [x] 不恢复 `sai run`。
- [x] stdin/file 输入复用同一套 message 构造、provider 选择、tools/MCP/skills 启用和日志路径。
- [x] stdin/file 输入不改变 JSONL 日志默认边界。
- [x] REPL `/usage` 输出 context window / usage 元数据，不请求 provider，不泄露正文敏感内容。
- [x] 增加 `sai doctor` 或 `sai config check`。
- [x] 健康检查覆盖配置根目录和 provider 文件。
- [x] 健康检查覆盖默认 provider/model 和 API key 环境变量是否存在。
- [x] 健康检查覆盖 skill_dir、mcp_dir、enabled tools/MCP/skills。
- [x] 健康检查覆盖日志目录可写性。
- [x] 健康检查输出脱敏，不打印 API key 或其他敏感配置值实际值。
- [x] 不引入 TUI。
- [x] 不做 Markdown 渲染。
