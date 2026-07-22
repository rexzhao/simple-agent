# Responses Cache / Server Root Checklist

这份清单记录两个后续实现目标：补齐 OpenAI Responses 的 prompt cache、状态续写和 usage
观测能力；把启动时使用的配置及其关联资源收敛到现有 `--server-root` 所选择的统一 namespace。

本文只记录待办和验收边界，不表示任何条目已经实现。

## Scope Decisions

- [ ] 不新增 `--namespace` 参数；复用现有全局 `--server-root PATH` 作为配置、认证、日志和
  project/session 持久化数据的统一 namespace 根目录。
- [ ] 更新 `--server-root` help 文案，明确它是 configuration and durable data namespace root，
  不再只描述为 project/session storage root。
- [ ] command basename 从 `argv[0]` 去掉目录部分和平台可执行文件后缀（例如 `.exe`）后得到；
  后续示例统一写作 `${basename}`。
- [ ] 显式指定 `--server-root PATH` 时，server root 为该路径的绝对、clean 结果，根配置固定为
  `${serverRoot}/${basename}.yaml`。
- [ ] server root 选择优先级为 `--server-root PATH`、basename 派生的
  `${BASENAME}_SERVER_ROOT` 环境变量、默认 user config directory；普通 `sai` 继续兼容
  `SAI_SERVER_ROOT`。
- [ ] 未指定 flag/environment 时，通过 `os.UserConfigDir()` 取得用户配置目录，默认 server root 为
  `${UserConfigDir}/${basename}`，根配置为
  `${UserConfigDir}/${basename}/${basename}.yaml`。
- [ ] `os.UserConfigDir()` 失败且没有显式 server root 时，启动失败并返回可诊断错误，不回退到
  cwd、home 猜测路径或项目内 `.agents` 目录。
- [ ] server root 选择在任何配置、provider、auth、MCP、skill、logger 或 execution store 初始化之前
  完成；help/version
  等不需要配置的路径不得因此访问 namespace。
- [ ] 本次迁移后，默认配置加载不再依赖当前 cwd 下的
  `.agents/${basename}.yaml`；`--cwd` 和项目 cwd 仍只用于初始 workspace、项目发现、`AGENTS.md`
  和 `$CWD` 等运行时语义，不参与 server root 或根配置选择。

## Server Root Layout and Path Resolution

目标默认布局：

```text
${serverRoot}/
  ${basename}.yaml
  providers/
  auth/
  mcp/
  skills/
  logs/                 # 仅显式启用 logging.path 后才创建
  data/
    projects/
    sessions/
```

- [ ] 根配置所在目录就是 server root；`provider_dir`、`auth_dir`、`mcp_dir`、相对
  `skill_dirs` 和相对 `logging.path` 都基于该目录解析。
- [ ] provider YAML、Codex auth/token 文件、诊断 logs 及现有 `data/projects`、`data/sessions` 必须
  跟随 server root 切换；server root A 不得读取或写入 server root B 的对应资源。
- [ ] 配置中的绝对路径继续保持绝对路径语义，不重复拼接 server root。
- [ ] session 保存的 `config_path` 使用最终解析后的绝对根配置路径；resume/compaction/subagent
  重载配置时继续使用同一路径，不重新根据当时 cwd 猜测。
- [ ] 清理 execution、provider settings、Web 和 auth 路径中硬编码的
  `<project>/.agents/sai.yaml`，统一使用 `${serverRoot}/${basename}.yaml` 或已经解析并保存的绝对
  config path。
- [ ] 保持 project/session/blob 现有 server-root data store 布局；本任务只把配置资源并入同一个
  namespace，不另设第二个存储根。
- [ ] Web UI 注册或选择 project 时不再要求项目目录包含 `.agents/sai.yaml`；provider settings 和
  Codex login 操作统一读写当前 server root 的配置资源。

## Diagnostic Logging Defaults

- [ ] 删除 `logging.path` 的非空默认值；配置完全省略 `logging` 时，诊断 JSONL logger 为 no-op。
- [ ] 只有配置中显式提供非空 `logging.path` 时才写诊断日志。
- [ ] `logging.path: ""` 与未配置 `logging.path` 都表示禁用日志。
- [ ] 禁用日志时不创建 `logs` 目录、session log 子目录或空 JSONL 文件。
- [ ] 显式配置相对 `logging.path` 时，路径从 server root 解析；显式配置绝对路径时按绝对路径写入。
- [ ] 此开关只控制 diagnostic JSONL logging，不关闭 resumable session 所需的 append-only session
  event records，也不改变 session/tool-result 持久化契约。
- [ ] 文档和示例配置默认不再展示会导致日志自动落盘的 `logging.path`；需要日志的示例明确标注为
  opt-in。

## OpenAI Responses Cache and State

### Usage and Observability

- [ ] 扩展内部 `model.Usage`，至少保留 Responses
  `usage.input_tokens_details.cached_tokens` 和 `cache_write_tokens`。
- [ ] Responses SSE decoder 从 `response.completed` 解析缓存读取和写入 token；字段缺失时保持兼容，
  不把缺失误报为解析错误。
- [ ] Usage event、诊断日志和需要展示 usage 的接口能够携带缓存统计，同时保持现有敏感内容脱敏边界。
- [ ] 单元测试覆盖无 details、cache miss、cache read 和 cache write 四类 usage fixture。

### Request Cache Controls

- [ ] 为 Responses 请求增加明确的 cache options 表达，不再只依赖无类型 model parameters 透传。
- [ ] 支持 `prompt_cache_key`、GPT-5.6+ 的 `prompt_cache_options.mode/ttl`，并保留旧模型
  `prompt_cache_retention` 的兼容路径。
- [ ] 增加 provider/model capability 判断：不向明确不支持的 Responses-compatible endpoint 或旧模型
  强制发送新字段；原始 parameters 继续作为兼容逃生口。
- [ ] cache key 在同一共享前缀/workload 内稳定，并允许显式配置；自动生成时至少纳入 provider、model、
  指令版本和 tool schema 版本，避免随机 per-request key。
- [ ] 对高流量场景保留稳定分片策略，避免所有不同前缀无条件共用一个固定 cache key。

### Explicit Cache Breakpoints

- [ ] 扩展 Responses input 表达，使 message content 能使用 `input_text`、`input_image`、
  `input_file` 等 content blocks，而不局限于纯字符串。
- [ ] 支持在可缓存 content block 上输出
  `prompt_cache_breakpoint: {"mode":"explicit"}`。
- [ ] 只有请求实际包含显式 breakpoint 时才允许配置 `prompt_cache_options.mode: explicit`；否则在
  本地返回清晰错误，避免意外关闭隐式缓存。
- [ ] 默认优先在稳定 instructions/examples/tool-related 前缀之后放置 breakpoint，动态用户内容放在
  breakpoint 之后。

### Response State Continuation

- [ ] 从 Responses stream 中保留 `response.id`，并通过内部事件/turn result 传递到 session state。
- [ ] 支持使用 `previous_response_id` 继续受支持的 stateful Responses 会话，不再把该参数列为无条件
  unsupported。
- [ ] stateful 模式只发送当前新增 input；manual replay 模式继续发送完整必要历史，两种模式不得在同一
  turn 中重复提交同一历史。
- [ ] `store:false`、ZDR 或不支持 stateful continuation 的 provider 使用 manual replay，并保存/回放
  完整 response output items，包括 reasoning/encrypted reasoning、function calls 和 tool outputs。
- [ ] compaction、resume、provider/model 切换或无法解析旧 response ID 时有明确降级规则，必要时回到
  full input replay，且不伪造 continuation 成功。

## Implementation Slices

- [ ] Slice 1：扩展现有 server-root resolver，补齐 basename 规范化、派生环境变量和默认
  UserConfigDir 路径，不引入第二个 namespace flag。
- [ ] Slice 2：根配置与 providers/auth/MCP/skills/logs 的 server-root-relative path plumbing，移除硬编码
  project `.agents/sai.yaml` 路径。
- [ ] Slice 3：验证既有 projects/sessions/blobs stores 与配置资源共享同一 server root 且相互隔离。
- [ ] Slice 4：logging 默认关闭、显式 opt-in 和 no-write 测试。
- [ ] Slice 5：Responses cache usage 数据模型、SSE parsing、日志/接口传播。
- [ ] Slice 6：cache key/options capability 和 request serialization。
- [ ] Slice 7：content blocks、显式 cache breakpoint 和校验。
- [ ] Slice 8：response ID、stateful continuation、manual full-output replay 和降级。

## Acceptance Criteria

- [ ] `--server-root X` 只读取 `X/${basename}.yaml`，并且 providers、auth、相对 logs、projects 和
  sessions 都位于 X 的解析边界内。
- [ ] 不新增或接受 `--namespace` alias；server root 是唯一的启动 namespace selector。
- [ ] 未传 `--server-root` 且没有对应环境变量时只读取
  `${UserConfigDir}/${basename}/${basename}.yaml`，不会因 cwd 不同而切换配置。
- [ ] 两个 server roots 使用相同 provider/project 名称时，其配置、Codex token、诊断日志、projects
  和 sessions 完全隔离。
- [ ] 未显式配置 `logging.path` 的正常请求、错误请求和 Ctrl+C 路径均不创建任何诊断日志文件或目录。
- [ ] 显式配置 `logging.path` 后，正常退出、错误退出和 Ctrl+C 仍正确 flush/close。
- [ ] Responses request fixture 覆盖 cache key、implicit/explicit options、旧 retention 和显式 breakpoint
  的 JSON shape。
- [ ] Responses stream fixture 能将 `cached_tokens`、`cache_write_tokens` 无损转换为内部 usage event。
- [ ] 增加确定性 fake-provider 合约测试：连续两次发送相同且超过 1,024 tokens 的稳定前缀，验证第二次
  请求保持相同 key/prefix，并能接收非零 `cached_tokens` fixture。
- [ ] 增加可选的真实 OpenAI 集成测试（默认跳过、需要显式环境开关）：连续两次发送相同长前缀，第二次
  响应断言 `cached_tokens > 0`；测试不得在默认 `go test ./...` 中产生网络调用或费用。
- [ ] stateful 测试验证第二轮发送首轮 `response.id`，manual `store:false` 测试验证完整 output items
  被回放。
- [ ] server root、logging 和 Responses 各 slice 的定向测试、`go test ./...` 与 `git diff --check`
  全部通过后才能勾选完成。
