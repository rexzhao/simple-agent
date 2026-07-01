# 里程碑

每个里程碑都应该结束于一个可运行状态，并配套一个明确的验证方式。不要在核心事件流
和 tool loop 稳定之前扩展太多协议。

## M0：项目骨架和关键决策

目标：建立最小项目结构，并锁定 v0.1 范围。

交付物：

- 初始化 Go module。
- 创建 `sai` CLI 入口。
- 记录并 stub YAML 配置结构。
- 支持从当前工作目录或 `--config-dir` 指定目录读取配置。
- 支持全局配置和每个 provider 一个配置文件的目录布局。
- 支持 provider 下声明多个 model profile。
- 支持单独的 `mcp/` 配置目录，每个 MCP server 一个 YAML 文件。
- 支持 `--enable-mcp` 覆盖 MCP 文件中的 `enabled` 字段。
- 支持 `tools.enabled` 配置，默认不启用工具。
- 支持 JSONL 日志配置。
- 定义 provider interface 和内部事件类型。
- 记录 PaperHub provider profile。

验证：

- `go test ./...` 通过。
- `sai config show` 能输出不含密钥的解析后配置。
- `sai config show --config-dir ./example-config` 能从指定目录读取配置。
- `sai models list` 能列出配置中的 provider/model。

## M1：OpenAI-Compatible Streaming

目标：从 OpenAI-compatible Chat Completions endpoint 流式输出文本。

交付物：

- OpenAI-compatible Chat Completions adapter。
- SSE parser，支持 `data:` event 和 `[DONE]`。
- 处理 `delta.content`。
- 处理 `delta.reasoning_content`。
- `sai run "prompt"` 命令。
- 会话开始时根据 `--provider` 和 `--model` 选择模型。
- `--show-reasoning` 参数。

验证：

- `sai run --provider paperhub "你是谁？"` 能流式输出可见文本。
- `sai chat --provider paperhub --model glm-5.2` 在会话开始时固定模型。
- reasoning 内容默认隐藏。
- `--show-reasoning` 能单独显示 reasoning 内容。
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
- JSONL 日志记录工具调用事件。

验证：

- fixture test 能正确重建 streamed tool call arguments。
- fake OpenAI-compatible server 能请求工具并收到工具结果。
- 未启用工具时，请求 payload 不包含 tools。
- `--enable-tools list_files,read_file` 只暴露这两个工具。
- 达到 `max_turns` 时 agent 给出清晰错误并停止。

## M3：MCP Stdio Tools

目标：将 MCP stdio server 的 tools 暴露给模型调用。

交付物：

- 单独的 `mcp/` 配置目录。
- 每个 MCP server 一个 YAML 文件。
- `--enable-mcp` 覆盖 MCP 文件中的 `enabled` 字段。
- MCP stdio 进程启动和关闭。
- MCP initialize 流程。
- MCP tool listing。
- MCP tool call routing。
- MCP tools 通过 enabled tools 控制是否暴露给模型。
- `sai mcp list` 命令。

验证：

- 本地 fake MCP server 能出现在 `sai mcp list` 中。
- `--enable-mcp local` 只启动 `local` MCP server。
- fake model tool call 能到达 MCP server 并返回结果。
- `sai` 退出时 MCP 进程一并退出。

## M4：打包和可用性

目标：让 v0.1 成为一个可实际使用的单文件 CLI。

交付物：

- 跨平台 build 命令。
- version 命令。
- 可读错误信息。
- `--verbose` 诊断信息，且不泄露 API key。
- JSONL 日志，且不泄露 API key 或 Authorization header。
- 除 JSONL 日志外，不保存会话历史、上下文快照或其他状态。
- 最小 README 使用说明。

验证：

- Windows 单文件可执行程序能运行 `sai run`。
- 能产出目标平台构建产物。
- missing API key、bad base URL、invalid model response 都有可读错误。

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

目标：增加 OpenAI Responses provider adapter。

交付物：

- Responses provider 配置。
- Responses input mapping。
- semantic streaming event 转换。
- function call event 处理。

验证：

- Responses fixture tests 能映射到同一套内部事件流。
- OpenAI-compatible adapter 不需要为 Responses 做特殊改动。

## M7：Skills

目标：在协议和工具基础稳定后，增加本地 skill 加载。

交付物：

- skill discovery。
- `SKILL.md` 读取。
- skill 显式启用或选择。
- instruction composition。

验证：

- 本地测试 skill 能改变模型 instructions。
- skill 可以关闭。
- 缺失或格式错误的 skill 有清晰错误。
