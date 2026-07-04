# 开发 Checklist

这份清单用于实现阶段逐项勾选。每个检查项都应对应可观察行为或测试。

## 范围约束

- [x] v0.1 保持纯 CLI，不引入 TUI。
- [x] MVP 先完成 OpenAI-compatible Chat Completions 核心路径（M0-M3）。
- [x] MCP 是 MVP 后的 M4 能力。
- [x] Anthropic Messages 是 M5 能力，在核心 OpenAI-compatible 路径稳定后接入。
- [x] OpenAI Responses 是 M6 能力，在核心 OpenAI-compatible 路径稳定后接入。
- [x] Skills 是 M7 能力，当前仅覆盖根配置文件配置的本地 skills。
- [x] 日志、verbose、resolved config 中不打印 API key、Authorization header 或其他敏感配置值的实际值。

## M0：项目骨架

- [x] 初始化 Go module。
- [x] 添加 `sai` CLI 入口。
- [x] 添加 version 命令。
- [x] 添加 config package。
- [x] 使用 YAML 作为第一配置格式。
- [x] 默认使用启动时当前工作目录下的 `.agents/${arg[0]}.yaml` 作为根配置文件；普通 `sai`
  二进制默认读取 `.agents/sai.yaml`。
- [x] 支持通过 `--config <file>` 指定根配置文件。
- [x] 暂时不读取或写入用户目录配置。
- [x] 支持每个 provider 一个独立 YAML 文件。
- [x] 支持一个 provider 声明多个 model profile。
- [x] 支持每个 model profile 设置自己的请求参数。
- [x] 默认读取启动目录下的 `AGENTS.md`。
- [x] 缺失 `AGENTS.md` 时继续执行。
- [x] `--config` 不改变 `AGENTS.md` 查找位置。
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

- [x] 定义 `skill_dirs` 多目录结构。
- [x] 读取 `SKILL.md`。
- [x] 添加 `skill_dirs` 直接子目录 skill discovery、目录顺序保留和
  `disable-model-invocation: true` frontmatter opt-out。
- [x] 将 skill instructions 组合进 system/developer messages。
- [x] 添加 malformed skill 和重复 skill id 错误处理。
- [x] 添加 skill discovery、frontmatter opt-out 和重复 skill id 测试。

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
- [x] `sai chat` 会话开始时固定 provider/model/tools/MCP/loaded skills/show-reasoning。
- [x] `sai chat` 支持 `--provider`、`--model`、`--show-reasoning`、`--verbose`、`--enable-tools`、`--enable-mcp` 语义。
- [x] 有 `--prompt` 且无 `--quit` 时，先跑完初始 prompt 再进入 REPL，历史保留首轮 user、assistant 和 tool messages。
- [x] 有 `--prompt` 且有 `--quit` 时，跑完这一轮后退出，不进入 REPL。
- [x] 根层用“跳过已知 flag 及其 value 后的第一个非 flag token”识别命令；命令前后 flags 可混排，全局 `--config <file>` 可放在命令后，没有命令 token 时默认执行 chat，help 混排不加载配置，`--` 后的 token 全部作为 positional，chat 初始 prompt 使用 `--prompt`。
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

- [x] Ctrl+C / interrupt 已接入现有 context cancel 流程。
- [x] 交互式 `sai chat` active turn 中 Ctrl+C 取消当前轮次并回到 prompt，不退出整个 CLI 进程。
- [x] 同一 active turn 取消完成前，短时间重复 Ctrl+C 不直接退出 chat session，取消完成后仍回到 prompt。
- [x] idle 输入状态 Ctrl+C 保持现有 CLI / terminal 行为，且不被 active-turn cancel 逻辑误处理。
- [x] `--quit` active turn 中 Ctrl+C 取消当前轮次后可以结束进程，且不伪造成功 assistant history。
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
- [x] 启用后保存 provider、model、model profile parameters、cwd 和根配置文件路径。
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
- [x] 保守保留内置 system、项目指令（当前兼容默认为 `AGENTS.md`）、loaded skills、
  tool/MCP schema、全部消息和 tool result。
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
- [x] stdin/file 输入复用同一套 message 构造、provider 选择、tools/MCP 启用、skills loading 和日志路径。
- [x] stdin/file 输入不改变 JSONL 日志默认边界。
- [x] REPL `/usage` 输出 context window / usage 元数据，不请求 provider，不泄露正文敏感内容。
- [x] 增加 `sai doctor`。
- [x] 健康检查覆盖所选根配置文件和 provider 文件。
- [x] 健康检查覆盖默认 provider/model 和 API key 环境变量是否存在。
- [x] 健康检查覆盖 skill_dirs、mcp_dir、enabled tools/MCP、skills discovery 和重复 skill id。
- [x] 健康检查覆盖日志目录可写性。
- [x] 健康检查输出脱敏，不打印 API key 或其他敏感配置值实际值。
- [x] 不引入 TUI。
- [x] 不做 Markdown 渲染。

## M16：Codex OAuth Multi-Provider Login

- [x] 增加 `auth_dir` 配置默认值和路径解析。
- [x] 配置层识别 `openai-codex` model profile type。
- [x] provider 配置支持 `auth_file`，并相对 provider YAML 文件解析。
- [x] `sai auth codex login --provider <name>` 支持 device flow。
- [x] `sai auth codex login` 默认 provider 名称为 `codex`。
- [x] `--force` 才允许覆盖已有生成 provider YAML 或 auth token JSON。
- [x] login 生成 provider YAML 到 `provider_dir`，生成独立 token JSON 到 `auth_dir`。
- [x] 多个 Codex provider 可以共存并使用独立 auth 文件。
- [x] `openai-codex` 运行时复用 OpenAI Responses request / SSE / function tool mapping。
- [x] `openai-codex` 运行时用 auth file access token 发送 bearer auth，不使用 `api_key`。
- [x] token 文件中存在 account id 时发送 `ChatGPT-Account-Id`。
- [x] 过期 access token 使用 refresh token 刷新并写回 token 文件。
- [x] `config show`、verbose、日志和 HTTP 错误不泄露 OAuth token 或 Authorization header。
- [x] 不读取、不导入 `~/.codex/auth.json`。
- [x] CLI 测试使用 fake OAuth endpoints 覆盖命名 provider 文件生成。
- [x] provider 测试覆盖 bearer auth、account id、HTTP 错误脱敏和刷新写回。

## M17：Local Tool Ergonomics and Discovery

- [x] `read_file` 继续只读取工作区内文本文件。
- [x] `read_file` 支持可选 `start_line`（1-based）、`line_count`（大于 0）和
  `max_bytes`（大于 0）。
- [x] `read_file` 不支持 byte offset / byte count 模式。
- [x] `max_bytes` 同时适用于默认读取和行范围读取。
- [x] 默认 `read_file` 从文件开头读取，最多返回 `max_bytes`。
- [x] 只提供 `start_line` 时，`read_file` 从该行读取到 `max_bytes` 或 EOF。
- [x] 只提供 `line_count` 时，`read_file` 从第 1 行开始最多返回指定行数。
- [x] 同时提供 `start_line` 和 `line_count` 时，`read_file` 最多返回指定行数且仍受
  `max_bytes` 限制。
- [x] `read_file` 对任何行范围读取或因 `max_bytes` 返回不完整的读取，在正文前包含
  path、有效 `start_line`、`lines_returned`、`max_bytes` 和 `truncated=true/false` metadata。
- [x] `read_file` 因 `max_bytes` 返回不完整内容时，tool result 明确包含下一步读取建议。
- [x] 行范围读取尽量返回完整行。
- [x] 单行超过 `max_bytes` 时，`read_file` 返回该行前缀、标记 `line_truncated=true`，
  并提示增大 `max_bytes` 后从同一行重试。
- [x] 小文件、非范围且未截断的完整 `read_file` 读取保持可返回原始文件内容的兼容行为。
- [x] 新增 `glob_files`，只在工作区内执行 glob 搜索。
- [x] `glob_files` 返回稳定相对路径，支持 `max_results`，并在截断时返回 metadata。
- [x] 新增 `grep_files`，只在工作区内执行文本搜索。
- [x] `grep_files` 支持 include / exclude globs。
- [x] `grep_files` 默认 literal 搜索，可选 regex、大小写敏感和 context lines。
- [x] `grep_files` 支持 `max_results` 和 snippet limits，并在结果或 snippet 截断时返回
  metadata。
- [x] `shell` 支持可选 `timeout_ms` 和 `max_output_bytes`。
- [x] `shell` 输出截断时，tool result 明确说明截断。
- [x] `shell` status 行继续不显示命令参数或任意 arguments。
- [x] `sai tools list` 和 tool registry / schema 测试覆盖新增工具和新增参数。
- [x] 聚焦单元测试覆盖 `read_file`、`glob_files`、`grep_files` 和 `shell` 新行为。
- [x] 验证 `go test ./...`。
- [x] 验证 `git diff --check`。

## M18：Project Instruction Files

- [x] 根配置新增 `agent.instruction_files` 列表字段。
- [x] 省略 `agent.instruction_files` 时，行为等价于 `["$CWD/AGENTS.md"]`。
- [x] 列表条目按配置顺序加载，条目可指向任意文件名，不限于 `AGENTS.md`。
- [x] 支持 `$CWD`、`$CONFIG`、`$USER` 和 `$REPO` placeholder。
- [x] `$REPO` 从 `$CWD` 向上发现 git repository root；无法解析时跳过该条目并输出 warning。
- [x] `$REPO` 解析 warning 不进入模型上下文。
- [x] 缺失的非 glob 文件按当前缺失 `AGENTS.md` 行为跳过。
- [x] glob 支持普通 glob pattern 和递归 `**/*.md` pattern。
- [x] 单个 pattern 匹配多个文件时，在该 pattern 内按稳定 path sort 顺序加载。
- [x] 不同 pattern 之间保留 `agent.instruction_files` 列表顺序。
- [x] 完成 placeholder 展开和 glob 匹配后，以解析后的 canonical/clean 绝对文件路径作为同一文件身份去重。
- [x] 重复指向同一实际文件时只加载第一次出现的文件；第一次出现按列表顺序优先，同一个 glob pattern 内按稳定 path sort 顺序判断。
- [x] 后续重复匹配静默跳过，不输出 warning；`$REPO` 无法解析时的 warning 行为不变。
- [x] 成功加载的文件注入在 `sai` 内置基础约束之后、loaded skills 和当前用户 prompt 之前。
- [x] 每个成功加载的文件优先作为独立 developer instruction source/message 注入。
- [x] 去重后每个唯一项目指令文件仍优先作为独立 developer instruction source/message 注入。
- [x] resumable session 的项目指令 snapshot 或可重建信息保留单文件来源。
- [x] `sai config show`、verbose、日志和 warning 不打印项目指令正文。
- [x] 配置测试覆盖默认值、显式空列表和配置值；context 测试覆盖 placeholder 解析、缺失文件、glob 和稳定排序。
- [x] context 测试覆盖 `$REPO/AGENTS.md` 与 `$CWD/AGENTS.md` 解析为同一路径时只加载一次，以及重叠 glob 的 first occurrence wins。
- [x] CLI / fake server 测试覆盖后续重复项目指令匹配静默跳过且不输出 warning，每个唯一文件仍是独立 developer message/source。
- [x] CLI / fake server 测试覆盖多项目指令文件注入顺序和 skill 注入位置。
- [x] 验证 `go test ./...`。
- [x] 验证 `git diff --check`。

## M19：Async Subagent-as-Tool Runtime

- [x] 根配置文件支持 `subagents` 映射，形态为 `id -> relative config file path`。
- [x] `subagents` 相对路径基于写出该配置项的父配置文件所在目录解析。
- [x] subagent config 文件复用 main config 的 `sai` schema。
- [x] child agent 通过类似 main agent 的 runtime 准备路径启动。
- [x] self-referential 或环形 subagent config 有递归深度保护。
- [x] subagent runtime 有最大 job 数量、等待时间和取消保护。
- [x] 未配置 subagents 时不暴露任何 subagent tools。
- [x] 配置 subagents 后 parent agent 自动获得 subagent tools。
- [x] parent prompt 注入已配置 subagent id 和短 description 列表。
- [x] child agent 使用自己的 provider、model、tools 和 prompt。
- [x] child agent 的 skills 和 MCP 仍需 child-specific 测试覆盖。
- [x] child agent 不继承 parent tools。
- [x] parent-facing subagent tool schemas 定义在 `internal/subagents`，并已接入 CLI runtime。
- [x] subagent job 支持 `subagent_start`。
- [x] `subagent_start` 可选接受用户或模型提供的 display name / job name。
- [x] display name 是 job metadata，不影响配置的 subagent id、权限、config 或 tools 选择。
- [x] display name 不能选择未配置 agent 或改变 child config/tools。
- [x] subagent job 支持 send。
- [x] subagent job 支持 status。
- [x] status / wait tool result JSON 输出包含 display name / job name。
- [x] mailbox 和 observability 输出包含 display name。
- [x] subagent job 支持 wait。
- [x] subagent job 支持 cancel。
- [x] subagent job 支持 close 释放已结束且完成通知已消费的 job 记录。
- [x] child job 运行时 parent 仍可继续接收用户输入。
- [x] child completion 通过 parent mailbox/runtime event 在 parent turn 后交付。
- [x] parent idle 时 child completion 可触发 auto wakeup。
- [x] `prompt.system_prompt` 作为未来 schema 追加到内置约束之后，不替换内置约束。
- [x] prompt placeholders 使用固定白名单，并拒绝未知占位符。
- [x] 为未来 DAG/workflow orchestration 保留边界，但 M19 不实现完整编排。
- [x] 为未来 shared state / blackboard 保留边界。
- [x] 为未来 structured result protocol 保留边界。
- [x] 为未来 global budgets、permissions 和 conflict arbitration 保留边界。
- [x] 为未来 observability、persistence/resume 保留边界。
- [x] 添加配置解析、prompt 注入、job lifecycle、mailbox delivery 和 safeguard 测试。

## M20：Server-Owned Sessions and CLI Client

- [x] 新增 `sai server`，默认以前台进程启动本地 server，提供 HTTP API 和 WebSocket stream。
- [x] 前台 `sai server` 阻塞到 server 退出，并支持 Ctrl+C 优雅关闭 listener、flush metadata、
  移除 registry 后退出 0。
- [x] `sai server --background` 启动后台 server，父进程等待 listen、registry 写入和 `/health`
  可用后退出 0。
- [x] `--background` 子进程 stdout/stderr 不长期占用调用终端，运行日志走 server 日志或诊断日志路径。
- [x] `sai server` 支持 `--cwd`、`--config`、`--port N`、`--port 0` 和高级 `--listen host:port`。
- [x] server identity 使用 canonical `cwd + config_path`，默认监听 `127.0.0.1:0`。
- [x] 启动成功后写入 per-user registry，记录 canonical cwd、config path、addr、pid、token、
  started_at 和 version。
- [x] registry 文件权限尽量限制为当前用户可读写，每个 server 生成随机 token。
- [x] 写操作、debug 读取和 blob content 读取必须带 registry token。
- [x] 重复启动同一 `cwd + config_path` 且监听参数一致时提示 already running 并退出 0。
- [x] 重复启动同一 `cwd + config_path` 但监听参数冲突时返回冲突错误并退出非 0。
- [x] 指定端口被其他进程占用时启动失败并退出非 0。
- [x] client 发现 registry 记录后先调用 `/health`，stale 记录会被忽略或清理。
- [x] `sai` 默认等价于 `sai attach`，从当前目录向上查找最近健康 server 并进入 attach REPL。
- [x] `sai --cwd <path>` 从指定 cwd 向上查找最近健康 server。
- [x] `sai attach <session-id>` 进入指定会话，`sai attach --new` 创建新会话并进入。
- [x] `sai status` 查询最近 server 的 cwd、config、listen、pid、version、uptime、session 数和
  running turn 数后退出。
- [x] `sai stop` 和 `sai stop --cwd <path>` 停止最近健康 server，等待退出并移除 registry。
- [x] stop 不删除 sessions、logs 或 blobs。
- [x] `sai servers list` 列出 registry 中的本地 server 后退出。
- [x] `sai sessions list` 和 `sai sessions show <id>` 通过 server API 查询，不直接读取 session 文件。
- [x] `sai send <session-id> --prompt ...` 和 `sai send --new --prompt ...` 发起一轮后退出。
- [x] 移除或隐藏独立进程内 `sai chat` 产品入口，裸 `sai` 默认 attach。
- [x] server 提供 `GET /health`、`GET /server` 和 `POST /server/shutdown`。
- [x] server 提供 `GET /sessions`、`POST /sessions`、`GET /sessions/{id}` 和
  `GET /sessions/{id}/items`。
- [x] server 提供 `POST /sessions/{id}/messages` 和 `POST /sessions/{id}/commands/compact`。
- [x] server 提供 `GET /sessions/{id}/items/{item_id}/content`，不提供裸 blob hash 读取。
- [x] server 提供 `WS /sessions/{id}/stream`，支持多个 client 同时观察同一 session。
- [x] session metadata API 不返回完整 items，items API 支持 `before_seq`、`after_seq`、`limit`
  和 `view=chat|debug`。
- [x] 同一 session 同时只允许一个 running turn，session busy 时再次发送 message 返回 conflict。
- [x] shutdown 在 running turn 时返回明确错误或 conflict。
- [x] attach REPL 中的 `/compact` 调用 server command API，多行文本中的 `/compact` 仍作为普通文本。
- [x] CLI client 不直接读取 session 文件、blob 文件或修改 `ActiveHistory`。
- [x] M20 不实现浏览器 Web GUI UI；未来 GUI 必须复用同一套 HTTP API / WebSocket stream。
- [x] API 测试覆盖 health/status/shutdown、session metadata、item pagination、send message 和 compact command。
- [x] WebSocket 测试覆盖多 client fanout、transient events、persisted events 和 failed turn events。
- [x] registry/discovery 测试覆盖 foreground/background server、duplicate start、port conflict、upward discovery 和 stale cleanup。
- [x] CLI 测试覆盖 attach、status、stop、servers list、sessions list/show、send 和无可用 server 提示。
- [x] 安全和 blob access 测试覆盖 token requirement、item content range read 和裸 hash 拒绝。
- [x] 验证 `go test ./...`。
- [x] 验证 `git diff --check`。

## M21：Global Singleton Server and Explicit Project/Session Lifecycle

- [ ] M21 作为直接替换实现，不保留旧 scoped server behavior 兼容层。
- [ ] 用户可见 help/docs/errors 使用 raw `argv[0]` basename，不硬编码具体命令名。
- [ ] `--home PATH`、basename-derived env var 和默认 user-level directory 的 namespace 优先级实现并测试。
- [ ] 不同 home directories 拥有独立 singleton server、registry、token、projects、sessions 和 data store。
- [ ] registry/data store 默认位于 user-level home namespace。
- [ ] registry 记录 `pid`、`base_url`、`token`、`version` 和 `started_at`。
- [ ] client 复用 registry 前 health-check，stale registry 可覆盖。
- [ ] file lock 避免 auto-start / background start 并发双启动。
- [ ] `<cmd> server` 前台启动 namespace singleton 且不绑定 cwd/project。
- [ ] `<cmd> server --background` 显式后台启动。
- [ ] help/version/server foreground/server background/server status/server stop 不 auto-start。
- [ ] bare attach/send/project/session commands 需要时 auto-start。
- [ ] project identity 只使用 canonical cwd root。
- [ ] project 必须显式创建并存入 user-level registry/data store，不写 project marker 文件。
- [ ] duplicate exact canonical project root 返回已有 project info 并退出 0。
- [ ] cwd upward discovery 选择 nearest registered ancestor，nested projects 正确工作。
- [ ] project show/remove 未传 `--project` 时使用 current cwd discovery，不接受 `--cwd`。
- [ ] project remove 默认 archive/hide，真实删除必须显式 `--delete-data`。
- [ ] running sessions 阻止 project removal/deletion。
- [ ] session 必须显式创建；不隐式创建 project 或 session。
- [ ] `--new` 等价 explicit session create 后 attach。
- [ ] config 属于 session；session create 记录 `config_path` 和关键 metadata。
- [ ] 每个 turn 重新读取 session `config_path`。
- [ ] `--config` 只允许创建新 session，existing session attach/send 传入时报错。
- [ ] existing session cwd 固定为 `created_cwd`。
- [ ] `--cwd` 只允许 project/session create 和 `--new`，existing session attach/send 传入时报错。
- [ ] 移除 hardcoded chat product entry，不作为 hidden alias。
- [ ] bare `<cmd>` 等价 attach，按 cwd 找 project，无 project/session 时失败。
- [ ] `<cmd> --new` 创建 session 并 attach。
- [ ] primary command shape 使用 singular `session`；旧 `sessions` 最多作为 list alias。
- [ ] `session list` 默认当前 project non-archived，支持 `--project`、`--all-projects` 和 `--archived`。
- [ ] `session list` 按 `last_used_at desc` 再 `created_at desc` 排序。
- [ ] explicit session id 全局有效；未指定 id 时只选当前 project 最近 non-archived session。
- [ ] 多个 observers 可 attach 同一 session。
- [ ] 同一 session 同时只有一个 active turn，busy 时 send 返回 `session_busy` 且不选择其他 session。
- [ ] 不同 sessions 可并发运行。
- [ ] shutdown 默认 immediate stop/cancel/cleanup。
- [ ] `--wait` drain 已开始 calls/turns，停止接受 new turns，并支持 timeout。
- [ ] OS signals/Ctrl+C 使用 immediate stop 语义。
- [ ] restart 后 previously running turns/sessions 标记 interrupted，且不自动 replay。
- [ ] API 使用 explicit project/session paths，包括 `/projects`、`/projects/{project_id}/sessions`、
  `/sessions/{session_id}`、`/sessions/{session_id}/messages`、`/sessions/{session_id}/items`、
  `/sessions/{session_id}/content/{blob_hash}` 和 `WS /sessions/{session_id}/stream`。
- [ ] HTTP/WS 默认 loopback。
- [ ] `GET /health` 是 public loopback discovery endpoint，且只返回 minimal non-sensitive liveness。
- [ ] 除 `GET /health` 外，所有 HTTP/WS endpoint 要求 bearer token。
- [ ] 第一版不实现多用户 login。
- [ ] durable store 使用 per-project 和 per-session directories，默认不写 project repo。
- [ ] transcript 使用 append-only JSONL，大内容使用 hash-addressed blobs。
- [ ] global blob dedupe 可用或明确实现。
- [ ] session seq 支持 pagination，hidden summary/debug records 可被 GUI 过滤。
- [ ] future session-history query tool 保持低优先级 out of scope。
- [ ] `docs/tasks/global-server-projects-checklist.md` 记录实现 smoke evidence。
- [ ] 验证 `go test ./...`。
- [ ] 验证 `git diff --check`。
