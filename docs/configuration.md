# 配置设计

配置优先使用 YAML。第一阶段目标是让 provider 和 model profile 的边界清晰，避免把
所有服务和模型参数塞进一个大文件。

## 配置位置

配置根目录只有两种来源：

1. 默认使用启动时当前工作目录下的 `.agents`。
2. 通过 `--config-dir` 显式指定目录。

暂时不读取用户目录，也不向用户目录写入默认配置。

`AGENTS.md` 不属于配置目录。它是项目上下文文件，v0.1 默认只从启动时当前工作目录读取。
`--config-dir` 不改变 `AGENTS.md` 的查找位置。

```text
sai run "你好"
sai --config-dir ./config run "你好"
sai --config-dir ./examples/paperhub chat
```

## 文件布局

推荐布局：

```text
AGENTS.md
.agents/
  sai.yaml
  providers/
    paperhub.yaml
    local.yaml
  mcp/
    local.yaml
  logs/
    sai.jsonl
```

`.agents/` 是默认配置根目录；通过 `--config-dir` 指定时，配置根目录改为指定目录。
`sai.yaml` 是全局配置入口，负责默认 provider、默认 model、provider 目录、工具启用和
agent 通用参数。`providers/*.yaml`
每个文件描述一个 provider。`mcp/*.yaml` 每个文件描述一个 MCP server；MCP 是 M4 后
能力，不属于 MVP 必需配置。

当配置根目录由 `--config-dir` 指定时，`sai.yaml` 和上述配置子目录从该目录读取；
`AGENTS.md` 仍从启动时当前工作目录读取。

## 全局配置

```yaml
# sai.yaml
default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

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

字段说明：

- `default_provider`：未通过命令行指定 provider 时使用。
- `default_model`：未通过命令行指定 model 时使用。
- `provider_dir`：provider 配置文件目录。相对路径基于配置根目录解析。
- `mcp_dir`：MCP 配置文件目录。M4 后启用；相对路径基于配置根目录解析。
- `agent.max_turns`：一次 agent loop 最多请求模型的轮数。
- `agent.stream`：默认是否启用 streaming。
- `agent.show_reasoning`：默认是否显示 reasoning stream。
- `tools.enabled`：默认启用的工具列表。空列表表示不向模型暴露工具。
- `logging.path`：JSONL 日志文件路径。相对路径基于配置根目录解析。
- `logging.level`：日志级别。

## Provider 配置

每个 provider 一个 YAML 文件。

```yaml
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

字段说明：

- `name`：provider 名称，必须和命令行 `--provider` 可选值一致。
- `type`：provider adapter 类型。v0.1 只实现 `openai-chat`。
- `base_url`：OpenAI-compatible API base URL，不包含 `/chat/completions`。
- `api_key`：provider 的 API key 配置值，遵循敏感配置值的 `$ENV_NAME` 约定。
- `models`：该 provider 下可选的模型配置。
- `models.<name>.id`：实际发送给 API 的模型 id。
- `models.<name>.*`：该 model profile 的请求参数。

model profile 的 key 是 CLI 选择时使用的名字。`id` 是实际传给模型服务的名称。这样可以
用同一个底层模型创建多个参数不同的 profile。

`api_key` 是这次 provider 配置中的具体字段。其他需要脱敏的敏感配置值也可以采用同样
约定：字符串以 `$` 开头时，`$` 后面的内容作为环境变量名读取实际值；不以 `$` 开头时
表示直接配置值。无论实际值来自环境变量还是直接配置，logs、verbose、resolved config
等输出都不能泄露实际值。

## MCP 配置（M4 后）

MCP 使用单独目录，不放入 provider 配置。每个 MCP server 一个 YAML 文件。

```yaml
# mcp/local.yaml
id: local
enabled: true
command: example-mcp-server
args: []
env: {}
```

字段说明：

- `id`：MCP server id，必须和文件名或命令行 `--enable-mcp` 可选值一致。
- `enabled`：默认是否启用该 MCP server。
- `command`：启动 stdio MCP server 的命令。
- `args`：启动参数。
- `env`：传给 MCP server 的环境变量。

MCP tools 会转换成内部 tool schema，但仍然需要出现在 enabled tools 列表中才会暴露给
模型。MCP tool 名称必须使用 `mcp.<server>.<tool>` 形式，避免和内置工具冲突。

默认启用 `enabled: true` 的 MCP server。如果传入 `--enable-mcp`，本次运行的 MCP server
启用列表完全由命令行决定，忽略各 MCP 文件中的 `enabled` 字段。

```text
sai run --enable-mcp local "只启用 local MCP server"
sai run --enable-mcp local,git "使用多个 MCP 服务"
sai run --enable-mcp local --enable-tools mcp.local.some_tool "暴露 MCP 工具给模型"
```

## 工具启用

v0.1 内置工具：

```text
list_files
read_file
shell
```

默认不启用任何工具：

```yaml
tools:
  enabled: []
```

启用示例：

```yaml
tools:
  enabled:
    - list_files
    - read_file
    - shell
    - mcp.local.some_tool
```

命令行可以覆盖配置中的 enabled tools：

```text
sai run --enable-tools list_files,read_file "看看当前目录"
```

`--enable-tools` 是覆盖，不是追加。`shell` 不需要额外 flag；只要它被启用，就按普通工具
暴露给模型。如果后续加入 `write_file`，也遵循同一规则。

`shell` 工具默认在启动目录执行命令。v0.1 不提供 `--workdir` 配置。

注意：`--enable-mcp` 只决定启动哪些 MCP server；`--enable-tools` 决定哪些工具暴露给
模型。一个 MCP server 被启用后，它的工具仍需要出现在 enabled tools 列表中才会被模型
看到。

## 选择规则

会话开始时确定 provider 和 model：

```text
sai run --provider paperhub --model glm-5.2 "你是谁？"
sai chat --provider paperhub --model glm-5.2
```

选择优先级：

1. 命令行参数。
2. 全局 `sai.yaml` 默认值。
3. 如果仍无法确定，打印可选 provider/model 并停止。

配置目录选择优先级：

1. `--config-dir`。
2. 启动时当前工作目录下的 `.agents`。

v0.1 不支持会话进行中切换模型。`sai chat` 进入会话后，provider/model 固定到会话
结束。

## 参数合并

请求参数按以下顺序合并：

1. provider adapter 的安全默认值。
2. model profile 参数。
3. 命令行参数。

命令行参数只覆盖明确传入的值。`api_key` 和其他敏感配置值遵循上述 `$ENV_NAME`
约定；无论实际值来自环境变量还是直接配置，都不能写入 resolved config 输出，也不能
出现在 verbose 日志中。

## 日志

v0.1 使用 JSONL 日志。除此之外，不保存会话历史、上下文快照或其他运行状态。

```yaml
logging:
  path: logs/sai.jsonl
  level: info
```

每行日志是一个 JSON object。日志可以记录模型请求生命周期、工具调用、usage、HTTP 错误
和 MCP 错误。API key、Authorization header 和其他敏感配置值的实际值不能进入日志。
v0.1 不记录完整 prompt、response、tool result 正文，也不提供开启正文日志的配置。
