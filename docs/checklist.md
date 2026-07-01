# 开发 Checklist

这份清单用于实现阶段逐项勾选。每个检查项都应对应可观察行为或测试。

## 范围约束

- [ ] v0.1 保持纯 CLI，不引入 TUI。
- [ ] v0.1 聚焦 OpenAI-compatible Chat Completions。
- [ ] v0.1 不实现 skills。
- [ ] MVP 不实现 MCP；MCP 从 M4 开始。
- [ ] 核心 OpenAI-compatible 路径稳定前，不实现 Anthropic Messages。
- [ ] 核心 OpenAI-compatible 路径稳定前，不实现 OpenAI Responses。
- [ ] 日志、verbose、resolved config 中不打印 API key、Authorization header 或其他敏感配置值的实际值。

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
- [ ] 会话进行中不支持切换模型。
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

- [ ] 定义内部 tool schema。
- [ ] 定义 tool executor interface。
- [ ] 添加 tool registry。
- [ ] 将内部 tools 转换为 OpenAI-compatible `tools` payload。
- [ ] 内置 `list_files`。
- [ ] 内置 `read_file`。
- [ ] 内置 `shell`。
- [ ] 默认不启用任何工具。
- [ ] 通过配置 `tools.enabled` 启用工具。
- [ ] 通过 `--enable-tools` 覆盖配置中的 enabled tools。
- [ ] `shell` 默认在启动目录执行命令。
- [ ] 累积 streamed tool call arguments。
- [ ] 执行完整 tool call。
- [ ] 追加 tool result messages。
- [ ] tool result 后继续 model loop。
- [ ] 添加 `max_turns`。
- [ ] 添加 partial JSON argument chunks 测试。
- [ ] 添加 malformed tool arguments 测试。
- [ ] 执行 PaperHub tool call smoke test，或记录不兼容限制。

## M3：打包

- [ ] 添加 Windows、Linux、macOS build 命令。
- [ ] 构建单文件可执行程序。
- [ ] 添加 `--verbose`。
- [ ] 添加 JSONL 日志。
- [ ] 除 JSONL 日志外，不落盘会话历史或上下文快照。
- [ ] v0.1 不记录完整 prompt、response、tool result 正文。
- [ ] missing API key 有可读错误。
- [ ] HTTP failure 有可读错误。
- [ ] invalid SSE chunk 有可读错误。
- [ ] 添加最小 README。
- [ ] 验证 fresh checkout build。

## M4：MCP

- [ ] 添加 `mcp/` 配置目录。
- [ ] 每个 MCP server 使用一个 YAML 文件。
- [ ] 读取 MCP 文件中的 `enabled` 字段。
- [ ] 通过 `--enable-mcp` 覆盖 MCP 文件中的 `enabled` 字段。
- [ ] 启动 stdio MCP server 进程。
- [ ] 发送 MCP initialize request。
- [ ] 列出 MCP tools。
- [ ] 将 MCP tools 转换为内部 tool schema。
- [ ] MCP tool 名称固定为 `mcp.<server>.<tool>`。
- [ ] MCP tools 仍受 enabled tools 列表控制。
- [ ] 将 tool call route 到 MCP。
- [ ] 将 MCP tool result 回传给模型。
- [ ] `sai` 退出时关闭 MCP server 进程。
- [ ] 添加 fake MCP server 集成测试。

## 后续协议

- [ ] 在 provider interface 后添加 Anthropic Messages adapter。
- [ ] 添加 Anthropic streaming fixture tests。
- [ ] 添加 Anthropic tool call fixture tests。
- [ ] 在 provider interface 后添加 OpenAI Responses adapter。
- [ ] 添加 Responses semantic streaming fixture tests。
- [ ] 添加 Responses function call fixture tests。

## 后续 Skills

- [ ] 定义 skill 目录结构。
- [ ] 读取 `SKILL.md`。
- [ ] 添加显式 skill activation。
- [ ] 将 skill instructions 组合进 system/developer messages。
- [ ] 添加 malformed skill 错误处理。
- [ ] 添加 skill selection 和 disablement 测试。
