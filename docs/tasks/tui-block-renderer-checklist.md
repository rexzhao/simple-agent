# TUI Block Renderer Checklist

这份文档记录将 CLI 展示升级为可选 TUI block renderer 的后续开发方案。
它是一个 future implementation checklist，不代表本次已经开始实现，也不代表当前
`--tui` 可用。

当前稳定文档仍然将 v0.1/MVP 定义为纯 CLI：不引入 TUI、不引入第三方 CLI/TUI 框架；
M24 已作为未来可选 TUI / PromptEvent 方向记录，用于收窄后续实现边界。任何代码实现
仍必须从 Phase 3+ 开始逐项完成，并保持 `--tui` 显式 opt-in。

## Assumptions

- 本文档只定义后续方案，不实现代码。
- execution 层继续作为 project/session/turn/tool/persistence 的库边界，不包含终端布局、
  键盘交互、block 折叠状态或 TUI 组件状态。
- 展示层只消费结构化事件，不解析 provider raw stream，不读取 raw JSONL log。
- 第一版 TUI 必须显式启用，例如 `--tui`；非 TTY、脚本、管道和 CI 仍使用 plain renderer。
- 创建本文档不改变现有 mailbox MCP 行为：mailbox 任务仍串行执行，新 mailbox 任务不打断
  active turn，MCP 结果仍只暴露最终 assistant output 或错误。
- `append_active` 是 M24 的未来实现项。它改变当前 CLI/mailbox 不支持运行中应用层输入队列的约束，
  必须按 Phase 4 的 checkpoint 规则实现。

## Current Stream Baseline

当前 execution session stream 已存在的事件基线应以代码为准：

```text
turn.started
turn.committed
turn.failed
text.delta
reasoning.delta
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
- `model.UsageEvent` 当前存在，但还没有映射为 execution `SessionStreamEvent`，因此状态栏需要的
  usage 事件属于后续 event contract gap。

候选新增事件或字段：

```text
usage.updated
status.updated
tool.started.started_at
tool.finished.duration_ms
tool.finished.preview
```

`tool.finished.preview` 必须是短、安全、可省略的展示摘要；第一版默认不展示 tool result body。

## Target Architecture

目标结构：

```text
execution.Service
  -> SessionStreamEvent
    -> Turn Block Aggregator
      -> Plain Renderer
      -> TUI Renderer
```

边界规则：

- execution 只发出结构化事件并维护 session 状态。
- Turn Block Aggregator 是 CLI 展示侧的纯逻辑层，将 stream events 和输入事件折叠成可渲染 block
  state。
- Plain Renderer 和 TUI Renderer 都只消费 block state 或同一事件流，不重新实现 agent loop。
- 后续可以让 plain renderer 复用 block aggregator，但第一版可以先保持现有 plain renderer，
  避免为了 TUI 重写稳定路径。

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
- `append_active` 表示在当前 turn 的安全 checkpoint 追加到上下文。
- mailbox 新任务默认使用 `enqueue_turn`，不得打断 active turn。
- TUI/interactive 用户输入在 active turn 期间可以使用 `append_active`，但必须按 M24 Phase 4
  的 checkpoint 规则实现。
- 多个追加 prompt 应保存为同一 `turn_id` 下的多个独立 user item，不能静默合并成一段文本。
- 追加输入不应抢占正在进行的 provider request、tool call 或 shell command。

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
- `tool.started` 创建或更新 tool block 为 running。
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

- [ ] 将 `model.UsageEvent` 映射为 execution session stream 事件，例如 `usage.updated`。
- [ ] 为状态栏确定是否需要 `status.updated`，避免 renderer 推断过多 runtime 状态。
- [ ] 确定 tool duration 是否作为事件字段提供。
- [ ] 明确 tool preview 的脱敏、长度和 opt-in/默认隐藏规则。
- [ ] 添加 execution 层测试覆盖新增 stream event。

### Phase 4 - PromptEvent Controller

- [ ] 定义 PromptEvent 数据结构，包括 source、mode、content、关联 mailbox task id 或输入 id。
- [ ] 支持 `enqueue_turn` 队列语义。
- [ ] 设计 `append_active` checkpoint 读取流程。
- [ ] 确保 provider request、tool call 和 shell command 不被抢占中断。
- [ ] 确保追加输入落盘为同一 `turn_id` 下的独立 user item。
- [ ] 确保 mailbox 新任务默认 queued，不打断 active turn。
- [ ] 添加测试覆盖 active tool/shell 完成后追加输入被下一个 provider request 看到。
- [ ] 添加测试覆盖 running final provider response 期间输入不会强插，只在安全边界处理。

### Phase 5 - Turn Block Aggregator

- [ ] 新增 CLI 展示侧纯逻辑包，例如 `internal/cli/turnview`。
- [ ] 定义 TurnViewState、Block、BlockKind 和 StatusBarState。
- [ ] 实现 `Apply(SessionStreamEvent)`。
- [ ] 实现 input/system/mailbox block 的非 execution 事件适配。
- [ ] 单元测试覆盖 reasoning/text/tool/error/mailbox/status bar 更新。
- [ ] 单元测试覆盖 block 更新不泄露 tool result body。
- [ ] 单元测试覆盖 show_reasoning false 时 reasoning block 隐藏或折叠策略。

### Phase 6 - Explicit TUI Renderer

- [ ] 选择 TUI 库或确认自实现方案；若引入第三方库，更新 go.mod 并记录理由。
- [ ] 新增显式 `--tui` 开关。
- [ ] 非 TTY、管道和脚本场景继续 plain renderer。
- [ ] TUI 显示 block 列表、输入区和状态栏。
- [ ] Ctrl+C 语义与当前 CLI 保持一致：取消 active turn 或退出。
- [ ] mailbox start/end 作为 system/mailbox block 展示。
- [ ] MCP mailbox result 不包含 TUI block 或 separators。
- [ ] 添加最小端到端测试或可自动化的 renderer 状态测试。

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

## Future Acceptance Criteria

- 用户能通过显式 `--tui` 启动 TUI；非 TTY 或未指定 `--tui` 时行为仍为 plain CLI。
- TUI 能按 block 展示 input、reasoning、tool、assistant、error 和 mailbox/system 信息。
- 流式输出时，已有 block 被增量更新，而不是反复追加不可关联的纯文本行。
- 状态栏显示模型、session、turn、运行状态、耗时和 usage/context 摘要。
- `append_active` 按 M24 checkpoint 规则实现后，运行中用户输入能在安全 checkpoint 追加到当前 turn。
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
