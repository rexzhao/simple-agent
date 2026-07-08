# Mailbox Task Board Checklist

这份文档记录 mailbox 从“一次性 prompt 队列”演进为任务看板的后续开发任务。
它是 M23 Mailbox MCP Input Adapter 之后的 proposal / checklist，不代表本次已经开始实现，
也不推进 `docs/checklist.md` 或现有 M23 清单的完成状态。真正实现前，应为该能力创建或确认
对应的后续里程碑，例如 M24，并先更新稳定的产品文档。

## Assumptions

- 现有 M23 mailbox MCP 工具和状态语义保持兼容，不能因为看板能力破坏
  `mailbox_post`、`mailbox_get`、`mailbox_wait`、`mailbox_cancel` 的当前用途。
- 看板任务第一版仍运行在当前前台 CLI 进程内，由单一 CLI worker 处理；不引入后台 daemon、
  project/session 管理 API 或远程服务。
- 本文中的 task event / revision / checkpoint 设计是待实现时与 M22 execution library 和
  M23 mailbox runtime 对齐的候选设计，不是已经落地的运行时契约。
- 除非后续里程碑明确批准持久化，否则 task event log 和 task snapshot 都是进程内非持久状态，
  与 M23 memory queue 边界一致。

## Core Rules

- mailbox 可以作为任务看板的当前状态层，而不仅是原始消息列表。
- agent 执行非终态任务时，可以更新任务状态和进度。
- 用户对非终态任务追加或修改需求时，变更进入该任务的事件流，并在 agent 的下一个 checkpoint
  通知 agent。
- 所有任务变更都在命令执行间隙处理；已经开始的 provider request、tool call 或 shell command
  自然结束后，agent 才读取任务变更。
- 用户取消正在执行的任务也是协作式取消：取消请求先进入事件流，agent 在下一个 checkpoint
  观察到后停止后续工作。
- agent 处理取消请求时，将任务转入 `cancelled`，并向本次运行的可见输出打印：

```text
任务已取消
```

- 取消后的提示是 run output，不写回已取消任务的评论，因为终态任务完全只读。
- 任何 terminal task 都完全只读。后续需求必须创建新任务，可以通过新任务的 relation 指向旧任务，
  但不能修改旧任务本身。

## Status Model

第一版应优先复用 M23 现有状态词，避免引入过宽的工作流：

```text
queued
running
completed
failed
cancelled
```

推荐不变量：

- `queued` 可以转入 `running`、`cancelled`。
- `running` 可以转入 `completed`、`failed`、`cancelled`。
- `completed`、`failed`、`cancelled` 都是 terminal status。
- terminal status 之后禁止任何 mutation，包括标题、描述、状态、需求、评论、优先级、负责人、
  metadata 和 relation。
- `blocked`、`needs_input` 等看板状态暂不进入第一版，除非实现计划证明它们是完成当前需求所必需。

## Event And Revision Model

候选设计：

- 每个任务有 current snapshot，用于看板展示和 MCP query。
- 每个非终态任务有 append-only event stream，用于记录用户变更、agent 进度和状态变化。
- 每个 accepted event 增加 task `revision`。
- agent 开始处理任务时记录 `last_seen_revision`。
- agent 在每个 checkpoint 读取最新 snapshot / events。
- 如果发现新 revision 且任务非终态，agent 合并新需求后继续。
- 如果发现 `cancel_requested`，agent 转入取消收尾：停止后续工作、打印 `任务已取消`、将任务置为
  `cancelled`。
- 如果任务已经 terminal，任何新增 event 都必须被拒绝，不能追加到该任务事件流。

Checkpoint 至少应出现在：

- 开始执行任务前。
- 每个工具或 shell 命令完成后。
- 每次 provider turn 完成后。
- 将任务标记为 terminal 前。

实现时需要确认这些 checkpoint 如何接入 M22 execution library 的 turn lock、event persistence
和 M23 mailbox 单 worker 机制。

## MCP Surface

兼容性规则：

- 现有 M23 mailbox MCP tools 继续表示“一次性 prompt 任务”的兼容入口。
- 看板能力可以扩展现有 task 结构，但不得改变现有 tools 的必填参数、返回字段和 terminal status
  语义。
- 现有 `mailbox_cancel` 对 running M23 task 的 turn context cancel 行为保持兼容；看板任务的
  cooperative cancel 应使用明确的新工具或显式模式，不能静默改变旧工具的 running-cancel 语义。
- 如果需要 richer board operations，应优先新增显式工具，而不是让现有 `mailbox_post(prompt)`
  承担过多含义。

候选新增工具需要在实现前单独定稿，例如：

```text
mailbox_task_list()
mailbox_task_update(task_id, expected_revision, changes)
mailbox_task_comment(task_id, expected_revision, comment)
mailbox_task_cancel(task_id, expected_revision?)
```

其中任何 mutation tool 都必须：

- 校验 task 非终态。
- 校验 revision，避免覆盖 agent 尚未看到的用户更新。
- 对 terminal task 返回明确错误。
- 不泄露中间 stream events、hidden/debug item、tool result body 或 provider raw error。

## Out Of Scope

- 本文档任务不实现代码。
- 不做 GUI 看板。
- 不引入后台 daemon、registry、远程 mailbox service 或 project/session 管理 API。
- 不引入持久化队列或 durable event log，除非后续里程碑另行批准。
- 不在命令执行中途 kill provider request、tool call 或 shell command。
- 不允许 terminal task reopen。
- 不把完成后的新需求静默写回旧任务。

## Future Implementation Checklist

- [ ] 更新稳定产品文档，明确 mailbox task board 的公开行为、状态机和 MCP surface。
- [ ] 创建或确认后续里程碑，并从该里程碑引用本 task checklist。
- [ ] 保持 M23 `mailbox_post` / `mailbox_get` / `mailbox_wait` / `mailbox_cancel` 兼容。
- [ ] 定义 task snapshot 数据结构。
- [ ] 定义 task event 数据结构和 revision 递增规则。
- [ ] 定义 terminal immutability guard，并确保它由系统层强制，而不是只靠 agent 自觉。
- [ ] 定义 running task 的 cooperative cancel flow。
- [ ] 在 agent / execution loop 中加入 command-boundary checkpoint。
- [ ] 确保 checkpoint 能读取并处理用户新增需求。
- [ ] 确保 checkpoint 能读取并处理取消请求。
- [ ] 确保取消请求生效后打印 `任务已取消`。
- [ ] 确保取消后不再写 task comment、progress 或 metadata。
- [ ] 为新任务 relation 定义只写新任务、不改旧 terminal task 的规则。
- [ ] 添加 MCP tool tests 覆盖非终态更新、revision conflict 和 terminal update rejection。
- [ ] 添加执行测试覆盖命令运行期间的更新只在下一 checkpoint 生效。
- [ ] 添加执行测试覆盖 running task 取消不会中途 kill 命令，而是在命令结束后的 checkpoint 生效。
- [ ] 添加执行测试覆盖 cancellation output 为 `任务已取消`。
- [ ] 添加兼容性测试覆盖现有 M23 mailbox tools 行为不变。

## Acceptance Criteria

- 用户能通过 mailbox 查询任务列表或当前任务状态。
- agent 能在非终态任务执行过程中更新看板状态。
- 用户对非终态任务的需求变更能被 agent 在 checkpoint 看到并纳入后续执行。
- 用户取消 running task 后，agent 不抢占中断当前命令；当前命令结束后的 checkpoint 使任务进入
  `cancelled`，并输出 `任务已取消`。
- `completed`、`failed`、`cancelled` 任务完全只读，所有后续 mutation 请求都会失败。
- terminal task 的后续需求只能创建新任务，不能 reopen 或修改旧任务。
- 现有 M23 mailbox MCP tools 的兼容行为不回退。

## Validation

本文档任务的验证：

```powershell
git diff --check
```

未来实现任务至少需要：

```powershell
gofmt -w <edited-go-files>
go test ./internal/cli
go test ./...
git diff --check
```
