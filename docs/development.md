# 开发说明

这个项目是一个简易的纯命令行 agent runner，命令名为 `sai`
（Simple Agent Interface）。第一阶段优先把核心闭环做稳：
命令行输入、OpenAI-compatible streaming、tool call、MCP stdio。不要引入 TUI，
也不要提前实现还没有进入第一阶段的扩展能力。

## 目标

- 提供一个纯 CLI 的本地 agent 工具。
- 第一优先级支持 OpenAI-compatible Chat Completions。
- 支持 streaming 输出。
- 支持模型发起 tool call。
- 支持通过 MCP stdio server 暴露工具。
- 发布形态尽量保持为单文件可执行程序。

## 第一阶段假设

- 使用 Go 实现，目标是单文件可执行程序。
- 第一种模型协议是 OpenAI-compatible `/v1/chat/completions`。
- 第一阶段真实测试服务使用 PaperHub：
  - Base URL: `https://tc-paperhub.diezhi.net/v1`
  - Model: `glm-5.2`
  - API key 环境变量: `PAPERHUB_API_KEY`
- skills 是后续开发项，v0.1 不实现。
- Anthropic Messages 和 OpenAI Responses 后续作为独立 provider adapter 接入。
- MCP 第一阶段只做 stdio。
- 配置默认从启动目录读取，也可以通过 `--config-dir` 指定。
- 只落盘 JSONL 日志；会话历史、上下文快照和其他状态暂不落盘。

## v0.1 非目标

- TUI 或全屏终端界面。
- 浏览器自动化。
- multi-agent 编排。
- 长期记忆。
- skill 加载。
- OpenAI Responses adapter。
- Anthropic Messages adapter。
- remote MCP over HTTP/SSE。
- 插件市场或插件生命周期管理。

## 架构

核心原则：agent loop 使用项目内部事件，不直接依赖任何厂商的原始返回格式。

```text
cmd/sai
  CLI 命令和终端输出

internal/config
  YAML 配置加载、环境变量覆盖、provider/model profile

internal/agent
  对话循环、max turns、tool 执行循环

internal/model
  provider interface 和统一 stream event 类型

internal/model/openai_chat
  OpenAI-compatible Chat Completions adapter

internal/tools
  内置工具定义和执行

internal/mcp
  MCP stdio client 和 MCP tool adapter

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
`--show-reasoning` 显示。

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
sai mcp list
sai models list
sai config show
```

常用参数：

```text
--provider paperhub
--model glm-5.2
--config-dir ./config
--base-url https://tc-paperhub.diezhi.net/v1
--api-key-env PAPERHUB_API_KEY
--no-stream
--show-reasoning
--max-turns 8
--enable-tools read_file,list_files,shell
--enable-mcp local
--verbose
```

## 配置形态

配置优先使用 YAML。配置根目录默认为启动时的当前工作目录，也可以通过
`--config-dir` 指定。暂时不读取或写入用户目录。

全局配置只保存默认选择、provider 文件目录和 agent 默认参数。
每个 provider 使用一个独立 YAML 文件；一个 provider 文件内可以声明多个模型，每个模型
可以有自己的参数。

```yaml
# config.yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers
mcp_dir: mcp

agent:
  max_turns: 8
  stream: true
  show_reasoning: false

tools:
  enabled: []

logging:
  path: logs/sai.jsonl
  level: info
```

```yaml
# providers/paperhub.yaml
name: paperhub
type: openai-chat
base_url: https://tc-paperhub.diezhi.net/v1
api_key_env: PAPERHUB_API_KEY

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

命令行参数覆盖配置文件。环境变量用于提供密钥，但日志中不能打印密钥值。
`provider_dir` 相对配置根目录解析，除非显式写成绝对路径。
`mcp_dir` 和 `logging.path` 也相对配置根目录解析，除非显式写成绝对路径。

模型选择发生在会话开始时：

```text
sai run --provider paperhub --model glm-5.2 "你是谁？"
sai chat --provider paperhub --model glm-5.2
```

如果命令行没有指定模型，则使用全局默认值。若默认值缺失或无效，CLI 应给出可选
provider/model 列表并停止。v0.1 不支持会话进行中切换模型。

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
内部事件，但默认不打印。

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

v0.1 内置三个工具：

```text
list_files
read_file
shell
```

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

## MCP

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

默认启用 `enabled: true` 的 MCP server。如果命令行传入 `--enable-mcp`，本次运行的
MCP server 启用列表完全由该参数决定，忽略 MCP 文件中的 `enabled` 字段。

```text
sai run --enable-mcp local --enable-tools mcp.local.some_tool "使用 MCP 工具"
```

v0.1 中 MCP server 进程生命周期由当前 agent 进程管理。后台常驻管理后续再做。

## 日志和落盘

v0.1 只落盘 JSONL 日志，不保存会话历史或上下文快照。日志路径来自
`logging.path`，相对路径基于配置根目录解析。

每行日志是一个 JSON object，至少包含：

```text
time
level
event
provider
model
```

工具调用、usage、HTTP 错误、MCP 错误可以写入日志。API key、Authorization header
和其他密钥不能写入日志。默认日志不记录完整 prompt、response、tool result 正文。

## 测试策略

测试分三层：

- 单元测试：stream parser、event 转换、config 加载、tool call delta 累积。
- 集成测试：本地 fake OpenAI-compatible server、本地 fake MCP server。
- 手动 smoke test：使用 `PAPERHUB_API_KEY` 调 PaperHub。

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
