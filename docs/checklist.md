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
- [x] 添加 JSONL 日志。
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

- [x] 配置层识别 `anthropic-messages` provider type。
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
- [x] 添加 run help：`sai run -h`、`sai run --help`、`sai help run`。
- [x] 添加 group help：`sai config -h`、`sai models -h`、`sai mcp -h`。
- [x] 添加 nested command help：`sai config show -h`、`sai models list -h`、`sai mcp list -h`。
- [x] 添加对应的 group 和 nested `sai help ...` 入口。
- [x] help 输出到 stdout，exit code 为 0。
- [x] help 不加载配置、不解析敏感配置值。
- [x] 未知命令和错误参数继续 exit code 1，并包含可读 help 提示。
- [x] `sai run` 缺 prompt 时包含 run usage 或 help 提示。
- [x] 不引入 TUI 或第三方 CLI 框架。
- [x] 添加 CLI help 测试并验证 `go test ./...`。

## M9：Reasoning Output Styling

- [x] 默认继续隐藏 reasoning 输出。
- [x] `--show-reasoning` 和 `agent.show_reasoning: true` 显示 reasoning 时可使用终端暗灰色样式。
- [x] stdout 不是终端时不输出 ANSI。
- [x] `NO_COLOR` 存在且非空时不输出 ANSI。
- [x] 切换到最终 `text_delta` 前先 reset，最终输出不继承 reasoning 颜色。
- [x] 只有 reasoning、没有最终文本时，stream 结束前 reset。
- [x] JSONL 日志继续记录原始事件，不包含 ANSI 样式。
- [x] 不引入 TUI、不引入第三方依赖，不新增 `--no-color` 或改动 help/usage。
- [x] 添加 reasoning 样式测试并验证 `go test ./...`。
