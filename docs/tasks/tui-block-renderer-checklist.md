# TUI Block Renderer Checklist

这份文档记录将 CLI 展示升级为可选 TUI block renderer 的后续开发方案。
它现在是 M24 首版 TUI renderer 的执行 checklist；首版显式 `--tui` 已实现。

当前稳定文档仍然将 v0.1/MVP 定义为纯 CLI：不引入 TUI、不引入第三方 CLI/TUI 框架；
M24 以显式 opt-in `--tui` 收窄这些历史边界。首版 TUI renderer 的库选择已定为
Bubble Tea，这是用户明确指定后的 M24 决策，只适用于 TUI 展示层；默认 plain CLI、
非 TTY、脚本路径和单文件发布目标不改变。后续代码实现仍必须从未完成的 Phase 4+
项目继续，并保持 `--tui` 显式 opt-in。

## Assumptions

- 本文档只定义后续方案，不实现代码。
- execution 层继续作为 project/session/turn/tool/persistence 的库边界，不包含终端布局、
  键盘交互、block 折叠状态或 TUI 组件状态。
- 展示层只消费结构化事件，不解析 provider raw stream，不读取 raw JSONL log。
- 第一版 TUI 必须显式启用，例如 `--tui`；非 TTY、脚本、管道和 CI 仍使用 plain renderer。
- 第一版 TUI 使用 Bubble Tea；plain renderer 不依赖 Bubble Tea 状态模型。
- 创建本文档不改变现有 mailbox MCP 行为：mailbox 任务仍串行执行，新 mailbox 任务不打断
  active turn，MCP 结果仍只暴露最终 assistant output 或错误。
- `append_active` 是 M24 的未来实现项。它改变当前 CLI/mailbox 不支持运行中应用层输入队列的约束，
  必须按 Phase 4 的 checkpoint 规则实现。

## First Implementation Slice

首个代码 slice 已完成：

- `model.UsageEvent` -> execution `usage.updated`。
- CLI 展示侧 Turn Block Aggregator。
- 显式 `--tui`，并在非 TTY、pipe、script、测试 writer 下回退 plain renderer。
- Bubble Tea block list、输入区和状态栏。
- Ctrl+C 语义：active turn 中取消当前 turn，idle 时退出。
- mailbox task start/end 在 TUI 中作为 mailbox/system block；MCP result 仍 final-output-only。
- 对 event contract、block aggregator 和 renderer fallback 的自动化测试。

`append_active` 和真正的 active-turn checkpoint 注入不属于首个 Bubble Tea renderer slice。
当前 serial stdin/TUI/mailbox idle 队列作为首版 `enqueue_turn` 语义。

## Current Stream Baseline

当前 execution session stream 已存在的事件基线应以代码为准：

```text
turn.started
turn.committed
turn.failed
text.delta
reasoning.delta
usage.updated
tool.started
tool.finished
item.appended
item.updated
compaction.created
active_history.replaced
```

说明：

- `text.delta`、`reasoning.delta`、`tool.started` 和 `tool.finished` 来自 model/runtime 事件映射。
- `item.appended`、`item.updated`、`compaction.created` 和 `active_history.replaced` 来自持久化事件。
- `turn.started`、`turn.committed` 和 `turn.failed` 由 session message orchestration 发出。
- `compact.failed` 目前是 CLI renderer 可处理的展示事件名，不是
  `internal/execution/session_events.go` 发出的 execution session stream 事件。
- `model.UsageEvent` 已映射为 execution `usage.updated`；状态栏可以直接消费该事件。

后续需要收紧的事件或字段：

```text
status.updated
tool.requested
tool.started
tool.progress
tool.started.started_at
tool.finished.duration_ms
tool.finished.preview
turn.failed.code
turn.failed.message
```

`tool.finished.preview` 必须是短、安全、可省略的展示摘要；第一版默认不展示 tool result body。
当前 `tool.started` 由完成的 model tool call 映射而来，只表示调用参数已生成，不表示工具已经通过
参数校验并开始执行。后续 contract 必须将它收紧为 `tool.requested`，并仅在 executor 实际开始时
发出 `tool.started`。`tool.progress` 是可选 transient event；没有增量能力的工具不需要伪造 progress。

## Target Architecture

目标结构：

```text
execution.Service
  -> SessionRun / TurnController
    -> PromptEvent queue + safe checkpoints
    -> durable projector
    -> execution events
      -> Turn Block Aggregator
        -> Plain Renderer
        -> TUI Renderer
      -> mailbox result adapter
```

边界规则：

- execution 只发出结构化事件并维护 session 状态。
- `SessionRun`（最终名称可按代码风格确定）是 execution 层的一次 active run handle，统一拥有
  append/enqueue、cancel、wait、状态和事件生命周期；CLI、TUI 和 mailbox 不各自实现运行队列。
- Turn Block Aggregator 是 CLI 展示侧的纯逻辑层，将 stream events 和输入事件折叠成可渲染 block
  state。
- Plain Renderer 和 TUI Renderer 都只消费 block state 或同一事件流，不重新实现 agent loop。
- 后续可以让 plain renderer 复用 block aggregator，但第一版可以先保持现有 plain renderer，
  避免为了 TUI 重写稳定路径。

## Execution Runtime Direction

后续实现应参考通用 agent runtime 的 active-run、steering 和 follow-up 语义，但保留当前 execution
library 的 project/session ownership、append-only persistence、session write lock、projector 和
interrupted recovery。不得用只存在于进程内的 transcript 取代 durable session state。

`SessionRun` / `TurnController` 的最小职责：

- 表示某个 session 当前唯一的 active run，并暴露明确的 running/settled 状态。
- 接收 `PromptEvent`，按 mode 进入 active append queue 或 next-turn queue。
- 提供 `Cancel()`、`Wait()` 和只读 event stream；Ctrl+C 与 mailbox cancel 复用同一个 cancel 边界。
- 在安全 checkpoint 读取 append queue；renderer 和 mailbox adapter 不直接修改 agent context。
- durable projector 继续作为 session 真相来源；presentation observer 不参与持久化决定。

事件交付需要分开 durability barrier 与 presentation observer：

- assistant/tool/user item 的 durable commit 仍是同步 barrier，后续工具或 provider 请求不能越过失败的
  持久化步骤。
- TUI/plain renderer 是 presentation observer，不得因为终端刷新速度反向阻塞 provider stream 或
  tool execution。
- 连续 `text.delta` / `reasoning.delta` 可以按 turn/block 合并刷新，但不能丢失文本；tool terminal、
  turn terminal 和失败事件必须可靠送达。
- 内部新增事件优先使用有类型的 Go payload；现有 `SessionStreamEvent map[string]any` 可以保留为
  renderer/兼容适配边界，不继续承担所有 execution 内部状态转换。

工具执行保持串行为默认值。shell、write、edit 和多数 MCP 调用可能产生副作用，本阶段不照搬
全局 parallel tool execution。未来若需要并发，只允许按明确的 per-tool capability 对只读工具启用，
并另立任务定义顺序、取消和持久化规则。

失败事件必须包含可安全展示的稳定分类和简短原因。日志可以保留更详细的诊断，但 execution 不应只
发出固定的 `turn failed`，也不得把 provider response body、Authorization 或 tool result 正文泄露给
renderer。

## Prompt Event Model

后续输入应抽象为 prompt event，而不是绑定到 stdin、TUI 输入框或 mailbox。

候选来源：

```text
stdin
tui
mailbox
mcp_task_update
```

候选语义：

```text
enqueue_turn
append_active
```

推荐规则：

- `enqueue_turn` 表示作为下一轮独立输入排队。
- `append_active` 表示在当前 agent run 的安全 checkpoint 追加到上下文；它不取消 provider 或工具，
  语义等价于安全 steering，而不是抢占式 interrupt。
- mailbox 新任务默认使用 `enqueue_turn`，不得打断 active turn。
- TUI/interactive 用户输入在 active turn 期间可以使用 `append_active`，但必须按 M24 Phase 4
  的 checkpoint 规则实现。
- 多个追加 prompt 应保存为同一 `turn_id` 下的多个独立 user item，不能静默合并成一段文本。
- 追加输入不应抢占正在进行的 provider request、tool call 或 shell command。
- active run 已进入 terminal barrier 后到达的 `append_active` 必须稳定降级为 `enqueue_turn` 或返回
  明确状态，不能因竞态静默丢失；具体选择由首个 `SessionRun` API slice 固定并测试。

`append_active` checkpoint 应与 `docs/tasks/mailbox-task-board-checklist.md` 的 checkpoint 模型对齐：

- 开始执行任务前。
- 每个工具或 shell 命令完成后。
- 每次 provider turn 完成后。
- 将任务标记为 terminal 前。

## Mailbox Semantics

mailbox task start/end 是展示层 system block，不是 execution 的特殊 turn 类型。

推荐展示流：

```text
mailbox task started
input block
reasoning/tool/assistant blocks
mailbox task completed|failed|cancelled
```

执行语义：

- mailbox prompt 进入和普通 prompt 相同的 execution/session 路径。
- mailbox 任务串行处理；active stdin/TUI/mailbox turn 运行时，新 mailbox task 保持 queued。
- mailbox 特殊点只有来源 metadata 和结果回填：关联 turn 完成后，将最终 assistant output 填入
  mailbox result。
- MCP `mailbox_wait` / `mailbox_get` 仍只返回最终结果，不返回 block、separator、reasoning、
  tool event、raw execution event 或中间过程。
- 若未来支持对 running mailbox task 的用户更新，应通过明确的 task update / `append_active`
  语义实现，并受稳定文档和 checkpoint 规则约束。

## Block Model

Turn Block Aggregator 候选 block：

```text
input
reasoning
assistant
tool
error
system
mailbox
```

候选状态栏字段：

```text
provider/model profile/model id
session id
turn id
turn status
elapsed time
last usage / context usage
current tool count
mailbox listen address
mailbox queue/running task summary
```

block 更新规则：

- `reasoning.delta` 追加到当前 reasoning block；可见性遵循 session 保存的 `show_reasoning`，
  TUI 可以提供折叠/展开状态。
- `text.delta` 追加到 assistant block。
- `tool.requested` 创建 tool block；只有 `tool.started` 才更新为 running。
- `tool.progress` 原地更新同一个 tool block，不创建重复 block。
- `tool.finished` 更新 tool block 为 completed 或 failed。
- `turn.failed` 创建 error block 并更新状态栏。
- `item.appended` / `item.updated` 可用于后续补全持久化状态；第一版不要求展示完整 item body。
- system/mailbox block 只表达运行边界、task id 和 terminal status，不进入 MCP final result。

## Implementation Phases

### Phase 1 - Boundary Proposal Docs

- [x] 创建本文档，明确它是 proposal/checklist，不批准实现。
- [x] 记录当前 no-TUI 稳定边界。
- [x] 记录实现前必须进行稳定 docs / milestone reconciliation。

### Phase 2 - Stable Docs Reconciliation

- [x] 创建或确认后续里程碑，例如 M24+。
- [x] 更新 `docs/milestones.md` 或等效稳定 roadmap，明确 TUI / running input queue 的目标和边界。
- [x] 更新 `docs/checklist.md` 或等效稳定 checklist，明确 no-TUI invariant 如何被 supersede 或收窄。
- [x] 更新用户可见配置/命令文档，说明 `--tui`、plain fallback 和非 TTY 行为。
- [x] 在稳定 docs 合并前，不开始代码实现。

### Phase 3 - Event Contract Gaps

- [x] 将 `model.UsageEvent` 映射为 execution session stream 事件，例如 `usage.updated`。
- [x] 为状态栏确定是否需要 `status.updated`，避免 renderer 推断过多 runtime 状态；首版不新增该事件。
- [ ] 将当前 model tool-call 完成语义从 `tool.started` 收紧为 `tool.requested`。
- [ ] 在 executor 实际开始/更新/结束处提供 `tool.started`、可选 `tool.progress` 和 `tool.finished`。
- [ ] 为 `turn.failed` 定义稳定、安全的 error code 和简短 message，并保持详细诊断只进入日志。
- [ ] 将 durability barrier 与 presentation observer 分开，验证慢 renderer 不阻塞 provider/tool 执行。
- [ ] 为连续 text/reasoning delta 定义不丢文本的展示侧合并规则。
- [ ] 为新增 execution 内部事件使用有类型 payload，并在现有 map stream 边界做适配。
- [ ] 确定 tool duration 是否作为事件字段提供。
- [ ] 明确 tool preview 的脱敏、长度和 opt-in/默认隐藏规则。
- [ ] 添加 execution 层测试覆盖新增 stream event。

### Phase 4 - PromptEvent Controller

- [ ] 在 execution 层定义 `SessionRun` / `TurnController` active-run handle，拥有 cancel、wait、
  status、event stream 和 prompt queues。
- [ ] 定义 PromptEvent 数据结构，包括 source、mode、content、关联 mailbox task id 或输入 id。
- [ ] 支持 `enqueue_turn` 队列语义。
- [ ] 设计 `append_active` checkpoint 读取流程。
- [ ] 确保 provider request、tool call 和 shell command 不被抢占中断。
- [ ] 确保追加输入落盘为同一 `turn_id` 下的独立 user item。
- [ ] 确保 mailbox 新任务默认 queued，不打断 active turn。
- [ ] 添加测试覆盖 active tool/shell 完成后追加输入被下一个 provider request 看到。
- [ ] 添加测试覆盖 running final provider response 期间输入不会强插，只在安全边界处理。
- [ ] 添加测试覆盖 terminal race 下 prompt 不丢失，并验证降级/拒绝语义。
- [ ] CLI、TUI 和 mailbox 迁移到同一个 controller；adapter 不保留第二套 active-run 队列。

Phase 3/4 必须按最小独立 slice、依赖顺序提交：

1. typed execution lifecycle 与真实 tool requested/started/finished 语义。
2. durability/presentation observer 解耦和 delta 合并，不改变用户输入行为。
3. `SessionRun` foundation：cancel/wait/status/events，并通过现有 send API 的兼容适配保持行为不变。
4. `PromptEvent` enqueue/append checkpoint 与独立 user item persistence。
5. CLI/TUI/mailbox 迁移和后续 renderer usability（viewport、active input queue、状态栏）。

每个 slice 必须独立通过格式化和测试，不能在同一提交中混合 foundation、调用方迁移和 TUI 样式。

### Phase 5 - Turn Block Aggregator

- [x] 新增 CLI 展示侧纯逻辑包，例如 `internal/cli/turnview`。
- [x] 定义 TurnViewState、Block、BlockKind 和 StatusBarState。
- [x] 实现 `Apply(SessionStreamEvent)`。
- [x] 实现 input/system/mailbox block 的非 execution 事件适配。
- [x] 单元测试覆盖 reasoning/text/tool/error/mailbox/status bar 更新。
- [x] 单元测试覆盖 block 更新不泄露 tool result body。
- [x] 单元测试覆盖 show_reasoning false 时 reasoning block 隐藏或折叠策略；首版依赖 execution 不发送 hidden reasoning 事件。

### Phase 6 - Explicit TUI Renderer

- [x] 选择 TUI 库：首版使用 Bubble Tea；若引入第三方库，更新 go.mod 并记录理由。
- [x] 新增显式 `--tui` 开关。
- [x] 非 TTY、管道和脚本场景继续 plain renderer。
- [x] TUI 显示 block 列表、输入区和状态栏。
- [x] Ctrl+C 语义与当前 CLI 保持一致：取消 active turn 或退出。
- [x] mailbox start/end 作为 system/mailbox block 展示。
- [x] MCP mailbox result 不包含 TUI block 或 separators。
- [x] 添加最小端到端测试或可自动化的 renderer 状态测试。

### Phase 7 - Optional Plain Renderer Unification

- [ ] 评估 plain renderer 是否改为消费 Turn Block Aggregator。
- [ ] 如果统一，保持现有 stdout/stderr、reasoning、tool status 和 mailbox separator 行为兼容。
- [ ] 添加回归测试覆盖 plain 输出不变。

## Out Of Scope

- 本文档任务不实现代码。
- 不引入后台 daemon。
- 不恢复 HTTP 层。
- 不实现 task board。
- 不实现完整 Markdown renderer。
- 不展示 raw provider payload 或 raw JSONL log。
- 第一版不默认展示 tool result body。
- 第一版不把 TUI 设为默认。
- 不改变现有 mailbox MCP final-output-only 结果边界。
- 不把工具执行改为全局并行。
- 不把 transient text/reasoning delta 逐条写入 durable session；完整 assistant/tool/user item 仍在既有
  durable barrier 落盘，调试 delta 继续由日志承担。

## Future Acceptance Criteria

- 用户能通过显式 `--tui` 启动 TUI；非 TTY 或未指定 `--tui` 时行为仍为 plain CLI。
- TUI 能按 block 展示 input、reasoning、tool、assistant、error 和 mailbox/system 信息。
- 流式输出时，已有 block 被增量更新，而不是反复追加不可关联的纯文本行。
- 状态栏显示模型、session、turn、运行状态、耗时和 usage/context 摘要。
- `append_active` 按 M24 checkpoint 规则实现后，运行中用户输入能在安全 checkpoint 追加到当前 turn。
- 慢 renderer 不阻塞 provider stream 或 tool execution，且 delta 合并后文本完整。
- tool block 区分 requested、running 和 terminal 状态，失败 turn 显示安全且可诊断的原因。
- mailbox 任务继续串行执行；新 mailbox task 不打断 active turn。
- mailbox MCP 结果仍只有最终 assistant output 或错误。
- plain renderer 行为不回退。

## Validation

本文档任务验证：

```powershell
git diff --check -- docs/tasks/tui-block-renderer-checklist.md
rg -n "NewSessionStreamEvent\\(\"(turn\\.started|turn\\.committed|turn\\.failed|text\\.delta|reasoning\\.delta|tool\\.started|tool\\.finished|item\\.appended|item\\.updated|compaction\\.created|active_history\\.replaced)\"" internal/execution/session_events.go internal/execution/service.go
```

未来实现任务至少需要：

```powershell
gofmt -w <edited-go-files>
go test ./internal/execution
go test ./internal/cli
go test ./...
git diff --check
```
