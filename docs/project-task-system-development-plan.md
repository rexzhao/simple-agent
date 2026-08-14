# 项目级任务系统与 `task_run` 编排——分阶段开发计划

> 状态：待评审设计稿，尚未实现。
>
> 本文描述当前 Web 产品中的项目级持久化任务系统。它不延续历史 mailbox task board 提案，也不改变普通会话现有的 Session 编排能力。实现前应为各阶段建立独立 checklist，并在每个阶段通过验收门后再推进下一阶段。

## 1. 背景

当前任务意图、worker 关联和后续动作主要存在于会话历史中。上下文压缩、进程重启、根会话同时处理多个 worker，或者用户从界面修改任务时，都可能让模型忘记下一步，或依据过期信息继续操作。

本项目增加一个独立于会话历史的项目级任务系统。用户可以在 Web UI 查看、创建、编辑、暂停、归档和排序任务；启用任务系统的根会话可以管理任务，并通过领域工具 `task_run` 启动与指定任务绑定的子会话。

核心职责边界如下：

- 根会话负责分析任务、决定是否调研或执行，并编写 worker 提示词。
- 任务系统负责 Task、TaskRun、Review 和内部 Session 的持久关联与状态推进。
- worker 只负责完成收到的工作，不拥有任务或 Session 编排工具。
- worker 完成后的结果归档、根会话唤醒和执行结果 Review 必须由系统触发，不能依赖根会话在启动时传回调参数，也不能依赖会话记忆。

## 2. 已确定的产品决策

以下内容是实现约束，不作为阶段内可自由更改的细节。

### 2.1 会话能力

创建**用户根会话**时，用户可以显式选择是否启用任务系统：

```text
task_system_enabled = true | false
```

规则：

1. 只有 `spawn_depth == 0`、`created_by == user` 的根会话可以启用任务系统。
2. 该选择写入会话的冻结能力快照；后续项目配置变化不能静默改变已有会话的能力。
3. 启用任务系统的根会话注入任务工具，但**不注入任何 `session_*` 工具**。
4. 未启用任务系统的普通根会话继续遵循现有 `tools.enabled` Session 工具配置，保持兼容。
5. 所有 `task_run` 子会话都不注入 `task_*` 或 `session_*` 工具，也不能继续创建子会话。
6. 上述限制由运行时过滤和执行器鉴权共同强制，不能只写在系统提示中。

### 2.2 唯一的子会话启动入口

启用任务系统的根会话只能通过：

```text
task_run(task_id, kind, prompt, ...)
```

启动任务子会话。它不能调用 `session_start`、`session_send`、`session_wait`、`session_get` 或 `session_stop`。

`task_run` 的 `task_id` 必须显式提供：

- 不从提示词解析；
- 不从当前对话主题推断；
- 不使用隐式“当前任务”；
- 不允许先创建子会话再补绑任务。

### 2.3 TaskRun 类型

首版支持两类 TaskRun：

```text
research  调研运行：阅读、检索、分析、比较方案；完成后返回根会话继续规划
execute   执行运行：产生交付物；完成后必须进入根会话 Review
```

`kind` 是必填结构化字段，不能从 prompt 猜测。

### 2.4 自动续接和 Review

- `research` 完成后，系统持久化结果并自动向原根会话投递调研结果工作项。
- `execute` 完成后，系统持久化结果、创建 Review，并自动向原根会话投递 Review 工作项。
- worker 正常结束不等于任务完成。
- 任务只有在根会话提交 `accept` Review 后才能进入 `done`。
- 根会话不需要也不能通过 `on_settle`、callback、`session_wait` 等参数建立该流程。
- 根会话暂时忙碌、进程重启或上下文压缩都不能丢失待投递工作项。

### 2.5 根会话系统提示自动注入工作流说明

当且仅当根会话启用了任务系统，运行时必须在其基础系统提示之后自动追加一段**版本化、平台维护的任务工作流说明**。这段说明与任务工具 Schema 一起由 capability policy 注入，不由用户手工粘贴，不写入普通用户消息，也不依赖模型从工具名称自行推断流程。

要求：

1. 每个模型 turn 都使用冻结会话能力决定是否注入；上下文压缩、恢复和自动续接后仍然存在。
2. 普通根会话不注入；`task_run` worker 不注入。
3. 说明只描述稳定工作流和硬约束，不注入完整任务列表或可能过期的任务状态；实际状态必须通过 `task_list` / `task_get` 读取，系统工作项所需快照除外。
4. 说明必须明确：先分析任务，再显式指定 `task_id` 调用 `task_run`；`research` 用于调研，`execute` 用于交付；execute 完成后必须 Review；worker 完成不等于任务完成。
5. 说明必须明确：根会话没有通用 Session 工具，不应尝试等待、轮询或通过自然语言伪造 Session 操作；worker 完成后系统会自动续接。
6. Review 工作项到达时，根会话必须检查验收标准和证据，并使用 `task_review_submit` 给出 `accept`、`reject` 或 `blocked`；不得通过普通 `task_update` 绕过 Review。
7. 提示文本应有独立版本号，例如 `project_task_workflow/v1`，并保存到会话的 instruction snapshot/source metadata，便于审计和回归测试。
8. 工具执行器和状态机仍是安全边界；系统提示只帮助模型正确使用流程，不能替代权限校验。

建议的首版系统提示正文见 §7.1。

## 3. 目标与非目标

### 3.1 目标

1. 项目级、可恢复、与会话上下文无关的任务列表。
2. 用户和任务根会话均可创建、编辑、暂停、恢复、排序任务。
3. 明确绑定 Task、TaskRun、根会话和内部 worker session。
4. 调研和执行使用不同的完成流程。
5. 执行结果经过根会话 Review 后才改变任务最终状态。
6. 用户可以从任务面板看到运行、结果、Review 和关联子会话。
7. 所有 mutation 支持并发检测、审计和幂等恢复。

### 3.2 首版非目标

- 自动从队列无限循环选择并执行任务；
- 任务依赖图、周期任务、标签和复杂负责人系统；
- 多个 execute worker 并行修改同一个任务；
- 根会话向运行中 worker 追加消息；
- worker 自行创建任务或更新任务状态；
- 从自然语言自动判断 TaskRun 类型；
- 物理删除仍有关联 Run、Review 或审计记录的任务；
- 用任务系统替换普通会话现有 Session 编排工具。

## 4. 领域模型

建议新建独立的 `internal/tasks` 包和 SQLite 表。Task 不写入 Session JSON，也不以会话消息作为事实来源。

### 4.1 Task

```go
type Task struct {
    ID                 string
    ProjectID          string
    Title              string
    Description        string
    AcceptanceCriteria []string

    Status             string // pending, in_progress, review, paused, blocked, done, cancelled
    Rank                string
    Priority            string // optional: low, normal, high
    Version             int64

    ActiveExecutionRunID string
    PendingReviewID      string

    CreatedByType       string // user, root_session
    CreatedByID         string
    CreatedAt           time.Time
    UpdatedAt           time.Time
    CompletedAt         *time.Time
    ArchivedAt          *time.Time
}
```

面向用户的状态：

```text
pending      待处理
in_progress  执行中
review       待审核
paused       已暂停
blocked      被阻塞
done         已完成
cancelled    已归档/取消
```

内部实现可以有 `dispatching`、`settling` 等瞬时状态，但 API 必须映射为稳定的用户状态。

### 4.2 TaskRun

```go
type TaskRun struct {
    ID              string
    ProjectID       string
    TaskID          string
    RootSessionID   string
    WorkerSessionID string // 内部关联；模型工具结果不必暴露

    Kind            string // research, execute
    Status          string // starting, running, succeeded, failed, cancelled
    Sequence        int64

    Prompt          string
    TaskVersion     int64
    TaskSnapshot    []byte

    ResultSummary   string
    ResultBlobHash  string
    ErrorSummary    string

    CreatedAt       time.Time
    StartedAt       *time.Time
    SettledAt       *time.Time
}
```

关键不变量：

- TaskRun 创建后 `TaskID`、`RootSessionID`、`WorkerSessionID` 和 `Kind` 不可变。
- 保存根会话实际提供的 prompt 和当时的 Task 快照。
- `TaskRun.succeeded` 只表示 worker 正常结算，不表示 Task 完成。
- 每个任务最多有一个 active execute TaskRun；首版可以允许多个 active research TaskRun，但应有项目级并发上限。

### 4.3 Review

```go
type TaskReview struct {
    ID              string
    ProjectID       string
    TaskID          string
    TaskRunID       string
    RootSessionID   string
    TaskVersion     int64

    Status          string // pending, delivered, reviewing, accepted, rejected, blocked, cancelled
    Decision        string // accept, reject, blocked
    Summary         string
    Feedback        string

    DeliveryID      string
    StartedRootRunID string
    CreatedAt       time.Time
    ResolvedAt      *time.Time
}
```

同一个 execute TaskRun 最多创建一个 Review。数据库应使用唯一约束保证：

```text
UNIQUE(task_run_id)
UNIQUE(delivery_id)
```

### 4.4 TaskEvent

所有变更写入 append-only 审计表：

```go
type TaskEvent struct {
    ID          string
    ProjectID   string
    TaskID      string
    TaskRunID   string
    ReviewID    string
    ActorType   string // user, root_session, worker_runtime, system
    ActorID     string
    Type        string
    BeforeJSON  []byte
    AfterJSON   []byte
    CreatedAt   time.Time
}
```

至少记录：

```text
task.created
task.updated
task.reordered
task.paused
task.resumed
task.cancelled
task.completed
run.created
run.started
run.settled
run.cancelled
review.created
review.delivered
review.accepted
review.rejected
review.blocked
```

## 5. 状态机和并发规则

### 5.1 主流程

```text
pending
  ├─ research run（Task 状态可保持 pending，并显示“调研中”辅助状态）
  └─ execute run → in_progress
                      ↓ worker settled
                    review
                   ├─ accept  → done
                   ├─ reject  → pending
                   └─ blocked → blocked

pending / blocked ↔ paused（恢复后回到暂停前允许的稳定状态）
未完成状态 → cancelled
```

首版不允许直接通过通用 `task_update` 把任务写成 `done`。`done` 只能由 Review accept 转换产生。

### 5.2 乐观并发

所有用户和模型 mutation 必须携带 `expected_version`。成功 mutation 在同一事务中：

1. 检查项目归属；
2. 检查当前状态；
3. 比较版本；
4. 更新 snapshot；
5. `version + 1`；
6. 写入 TaskEvent；
7. 提交后发布同步事件。

版本不一致返回稳定错误 `task_conflict`，并附当前版本和最小可安全展示的当前状态，不静默覆盖。

### 5.3 运行并发

- `research`：每任务默认最多 3 个 active run，项目总量受现有全局 run capacity 约束。
- `execute`：每任务最多 1 个 active run，使用数据库条件更新/唯一约束原子认领。
- 首版建议 active execute 存在时拒绝新 TaskRun，包括 research，以避免新信息无法可靠注入已运行 worker。
- `task_run` 必须接受 idempotency key（工具执行内部可由 `session_id + tool_call_id` 派生），防止重放创建两个 worker。

### 5.4 暂停、取消和归档

首版采用明确且容易恢复的语义：

- `task_pause` 阻止创建新 Run和自动返工。
- 若存在 active Run，默认让其结算，但完成结果只持久化，不自动改变 paused 状态；创建的 Review/调研结果工作项保持 suspended，恢复后再投递。
- UI 可以另提供“暂停并取消运行”，内部调用任务领域取消接口，而不是 Session API。
- `cancelled` 为软删除；保留 TaskRun、Review、事件和会话关联。
- 已归档任务的迟到 worker 结果仍保存，但不得复活任务或覆盖状态。

## 6. 工具契约

### 6.1 注入任务根会话的首版工具

```text
task_list
task_get
task_create
task_update
task_reorder
task_pause
task_resume
task_run
task_cancel_run
task_review_submit
```

`task_review_submit` 只应在存在分配给当前根会话的 pending Review 时可执行；它不是任意状态编辑工具。

### 6.2 `task_run`

建议输入 Schema：

```json
{
  "task_id": "task-...",
  "kind": "research",
  "prompt": "检查相关实现并报告调用链，不修改文件。",
  "name": "调查登录重定向",
  "provider": "optional-provider",
  "model": "optional-model-profile",
  "reasoning_level": "optional-level"
}
```

必填：`task_id`、`kind`、`prompt`。

校验规则：

- 调用者必须是启用任务系统的用户根会话；
- `task_id` 必须属于调用者项目；
- Task 不能是 paused、done 或 cancelled；
- `kind` 只能为 `research` 或 `execute`；
- prompt 去除首尾空白后不能为空，并设置长度上限；
- provider/model 必须同时出现；省略时继承根会话冻结的模型运行快照；
- 显式模型选择复用现有 Session 模型解析和权限校验；
- 调用者不提供 callback、`on_settle`、parent ID、worker session ID 或任务完成状态。

返回任务领域结果，不要求模型管理底层 Session：

```json
{
  "ok": true,
  "task_id": "task-...",
  "task_run_id": "task-run-...",
  "kind": "research",
  "status": "running"
}
```

### 6.3 `task_review_submit`

```json
{
  "task_id": "task-...",
  "review_id": "review-...",
  "expected_version": 8,
  "decision": "reject",
  "summary": "核心逻辑完成，但缺少 query/hash 回归测试。",
  "feedback": "补充三类 URL 回归测试后重新执行。"
}
```

规则：

- `task_id` 和 `review_id` 都必须显式提供；
- Review 必须属于该 Task、项目和当前根会话；
- `decision` 只能为 `accept`、`reject`、`blocked`；
- accept 自动完成 Task；reject 自动回到 pending；blocked 自动进入 blocked；
- 状态更新、Review 结算和事件记录在一个事务中完成；
- 重复提交相同决定幂等成功，冲突决定返回 `review_already_resolved`。

### 6.4 稳定错误码

至少定义：

```text
invalid_arguments
task_not_found
task_forbidden
task_conflict
task_not_runnable
task_paused
task_terminal
task_run_limit_reached
task_execution_already_active
task_run_not_found
task_run_not_active
review_not_found
review_forbidden
review_already_resolved
root_session_required
task_system_not_enabled
run_capacity_reached
coordinator_unavailable
```

## 7. 会话能力过滤与系统提示注入

任务系统不能只依赖配置文件中的 `tools.enabled`。建议增加一层运行时 capability policy：

```go
func toolsForSession(state sessions.SessionV2, configured []string) []string {
    if state.IsTaskWorker {
        return removeTaskAndSessionTools(configured)
    }
    if state.SpawnDepth == 0 && state.TaskSystemEnabled {
        return append(removeSessionTools(configured), enabledTaskTools()...)
    }
    return configured
}
```

同时在每个 task executor 边界再次检查会话状态，防止旧持久化快照或伪造工具调用绕过 Schema。

Session 建议增加：

```go
TaskSystemEnabled bool   `json:"task_system_enabled,omitempty"`
TaskWorker         bool   `json:"task_worker,omitempty"`
TaskID             string `json:"task_id,omitempty"`
TaskRunID          string `json:"task_run_id,omitempty"`
```

其中 worker 的 Task 关联也必须存在任务数据库；Session 字段只是查询和 UI 投影的冗余索引，不是唯一事实来源。

### 7.1 任务根会话工作流系统提示（v1）

建议以独立 instruction source 注入，例如：

```text
source: runtime_capability
path: builtin://project_task_workflow/v1
role: system
```

首版正文建议保持短而明确：

```text
[项目任务工作流]

本会话启用了项目任务系统。任务列表是项目级持久化状态，不依赖当前会话上下文。

工作规则：
1. 在处理、修改或运行任务前，使用 task_list 或 task_get 读取最新状态；不要把会话记忆当作任务状态来源。
2. 你负责分析任务并为 worker 编写清晰、范围受控、包含验收要求的提示词。
3. 启动 worker 只能调用 task_run，并且必须显式提供 task_id、kind 和 prompt。不得从对话推断或省略 task_id。
4. kind=research 用于调研和分析；调研完成后系统会自动把结果续接到本会话。调研完成不会完成任务。
5. kind=execute 用于产生交付物；执行完成后系统会自动创建 Review 工作项并续接本会话。worker 正常结束不等于任务完成。
6. 本会话没有通用 Session 编排工具。不要尝试启动、轮询、等待或控制底层 Session；TaskRun 生命周期由系统管理。
7. 收到执行 Review 工作项后，依据任务验收标准、worker 提示词、结果和证据进行审核，并调用 task_review_submit 提交 accept、reject 或 blocked。
8. 只有确有充分证据满足验收标准时才 accept；需要返工时 reject 并给出具体反馈；缺少用户输入或外部条件时 blocked。
9. 不要使用普通任务编辑绕过 Review，也不要仅因 worker 自称完成而标记任务完成。
10. 用户可能同时在界面修改、暂停、排序或归档任务。发生版本冲突时重新读取任务，不覆盖较新的状态。
```

实现要求：

- 提示构建使用结构化的 instruction composition，不在 `agent.go` 中散落字符串拼接。
- instruction source 和版本进入 `InstructionsSnapshot` / `InstructionSources`，确保同一会话恢复后语义稳定。
- 会话创建后默认冻结工作流版本；产品若需要升级已有会话，必须设计显式 migration，不能静默换文案造成行为漂移。
- 自动投递的 research/review 工作包是在该基础说明之外追加的运行时系统工作项，两者不能互相覆盖。
- 单元测试应断言提示存在性、角色、来源、顺序和精确版本，不建议只用易误判的关键词包含测试。

## 8. 系统工作项与自动续接

可复用现有 durable completion inbox 的设计思想，但不要直接复用当前需要模型调用 `session_get` 的普通 child-completion 文案。任务根会话没有 Session 工具，因此任务工作项必须包含足够的持久化结果。

### 8.1 Research 完成

worker settled 后，系统执行：

1. 根据 `worker_session_id + run_id` 幂等找到 TaskRun；
2. 保存 durable assistant 最终输出或失败摘要；
3. 将 TaskRun 标为 succeeded/failed/cancelled；
4. 创建 `task_research_result` delivery；
5. 若 Task 未暂停/取消，尝试启动根会话自动续接；
6. 若根会话忙碌，delivery 保持 pending，待根会话 idle 或进程重启后重试。

自动注入根会话的工作包至少包含：

- task ID、标题、当前版本；
- TaskRun ID、kind、结算状态；
- 根会话当时提供的 prompt；
- worker 结果或安全错误摘要；
- 明确说明这是调研结果，不是新用户请求。

结果较大时写 blob；工作包只带摘要和受限正文，禁止注入 raw provider payload、隐藏消息、工具结果正文或 secrets。

### 8.2 Execute 完成

worker settled 后，系统执行：

1. 幂等结算 TaskRun；
2. 保存 worker 结果；
3. 在任务仍允许处理时创建唯一 Review；
4. Task 进入 `review` 并写入 `pending_review_id`；
5. 创建 `task_execution_review` delivery；
6. 自动续接根会话。

Review 工作包包含：

- 最新 Task 和本次执行时的 Task 快照；
- 验收标准；
- TaskRun prompt；
- worker 最终结果；
- 可取得的修改/测试摘要；
- 明确要求根会话审核并调用 `task_review_submit`。

如果根会话 turn 未提交 Review 决定：

- 不把任务标记完成；
- Review 保持 pending；
- delivery 不丢失；
- 下一次根会话启动或独立 inbox 扫描时再次提示；
- 设置退避和最大自动重试频率，避免无效模型输出形成紧循环；
- 多次失败后在 UI 标记“等待人工审核”，而不是猜测 accept。

### 8.3 恢复和幂等

必须覆盖以下崩溃窗口：

- TaskRun 已创建但 worker Session 未创建；
- worker Session 已创建但 TaskRun 尚未写入 `running`；
- worker 已结算但结果尚未归档；
- Review 已创建但 delivery 尚未创建；
- delivery 已 claim 但根会话 run 尚未启动；
- Review 已提交但同步事件尚未发布。

实现原则：

- 数据库唯一约束承担去重，不依赖进程内 map；
- 使用稳定 ID/idempotency key；
- 启动时扫描 `starting`、已结算未处理 Run、pending Review 和 delivery；
- 发布采用 transaction 后可重建的 outbox/inbox 记录；
- 任意扫描可以重复执行并收敛到同一状态。

## 9. HTTP、同步协议和 UI

### 9.1 HTTP API 候选

项目作用域 API：

```text
GET    /api/projects/{project_id}/tasks
POST   /api/projects/{project_id}/tasks
GET    /api/projects/{project_id}/tasks/{task_id}
PATCH  /api/projects/{project_id}/tasks/{task_id}
POST   /api/projects/{project_id}/tasks/{task_id}/reorder
POST   /api/projects/{project_id}/tasks/{task_id}/pause
POST   /api/projects/{project_id}/tasks/{task_id}/resume
DELETE /api/projects/{project_id}/tasks/{task_id}        # 软删除
GET    /api/projects/{project_id}/tasks/{task_id}/runs
GET    /api/projects/{project_id}/tasks/{task_id}/events
POST   /api/projects/{project_id}/task-runs/{run_id}/cancel
```

Session 创建请求增加：

```json
{
  "task_system_enabled": true
}
```

服务端必须拒绝在 child Session 或 agent-created Session 上启用该字段。

### 9.2 同步事件

新增项目级事件（名称在协议评审时定稿）：

```text
task.created
task.updated
task.deleted
task.run.updated
task.review.updated
```

事件包含 `project_id`、资源 ID、版本和足够更新缓存的 snapshot。继续使用现有序列/重同步原则；客户端检测 revision gap 后重新拉取项目任务列表，不自行猜测缺失状态。

### 9.3 UI 信息架构

第一版在项目页面增加 Tasks 面板：

- 按 pending/in_progress/review/paused/blocked/done 分组；
- pending 支持拖拽排序；
- 显示标题、状态、优先级、活动 research 数、active execute、pending Review；
- 支持新建、编辑、暂停、恢复、软删除；
- 任务详情展示验收标准、TaskRun 历史、Review 结果和审计事件；
- TaskRun 可跳转到底层 worker Session，但 Session ID 不需要暴露给模型。

创建根会话的 UI 增加“启用项目任务系统”开关，并说明：

```text
启用后，该会话可管理项目任务并通过 task_run 启动任务 worker；
它不会获得通用 Session 编排工具。该选择在会话创建后不可更改。
```

运行中暂停或删除时必须明确显示 active Run 的处理方式，不能让用户误以为隐藏列表项等于终止进程。

## 10. 分阶段实施

每一阶段都应保持主分支可运行，并建立独立迁移、测试和回滚边界。

### Phase 0：契约定稿与测试骨架

**目标**：在写生产逻辑前锁定状态机、能力边界和错误语义。

工作项：

- [ ] 评审本文并冻结 Task、TaskRun、Review 的首版字段和状态。
- [ ] 冻结 `task_run`、`task_review_submit` Schema 和稳定错误码。
- [ ] 明确 pause、cancel、archive 和迟到结果语义。
- [ ] 设计 SQLite migration 与唯一约束。
- [ ] 为 `internal/tasks` 建立接口、fake store 和状态机表驱动测试骨架。
- [ ] 为 session capability policy 建立矩阵测试。
- [ ] 冻结 `project_task_workflow/v1` 系统提示正文、instruction source 和版本升级策略。
- [ ] 为系统提示注入建立存在性、顺序、来源和 worker 不注入的测试骨架。
- [ ] 确认项目级同步事件命名与 revision 规则。

验收门：

- 状态转换表无未定义路径；
- 明确证明任务根会话无 `session_*`、worker 无 `task_*`/`session_*`；
- 所有 crash window 都有恢复归属；
- 本阶段不改变用户可见行为。

### Phase 1：持久化领域层与 CRUD API

**目标**：先交付不依赖 agent 的可靠项目任务列表。

工作项：

- [ ] 新增 `internal/tasks` model/store/service。
- [ ] 创建 tasks、task_runs、task_reviews、task_events、task_deliveries 表及索引。
- [ ] 实现 Task CRUD、软删除、暂停/恢复、排序和 optimistic version。
- [ ] 实现项目归属和 archived project 检查。
- [ ] 实现 HTTP API 和项目级同步事件。
- [ ] 增加任务列表最小 UI：查看、新建、编辑、排序、暂停、恢复、归档。
- [ ] 增加 store 重启、migration、并发更新和排序冲突测试。

验收门：

- 重启后任务和顺序不丢失；
- 两个客户端并发编辑不会静默覆盖；
- 跨项目访问返回 forbidden/not found 的稳定边界；
- cancelled Task 保留审计历史且不能被普通 update 复活；
- 此阶段尚不向模型注入任务工具。

### Phase 2：会话 capability 与任务 CRUD 工具

**目标**：允许用户创建专用任务根会话，但暂不启动 worker。

工作项：

- [ ] Session 创建 API/UI 增加 `task_system_enabled`。
- [ ] 将能力写入 Session 冻结 snapshot 和 Web 类型。
- [ ] 在 instruction composition 中自动注入并冻结 `project_task_workflow/v1` 系统提示。
- [ ] 确保普通根会话和所有 child/worker 均不注入该工作流说明。
- [ ] 实现任务工具 Schema/Executor：list/get/create/update/reorder/pause/resume。
- [ ] 在 schema 注入和 executor 两层强制 root-only/project-only。
- [ ] 对任务根会话过滤全部 `session_*` 工具。
- [ ] 对 child/worker 过滤全部 `task_*` 和 `session_*` 工具。
- [ ] 增加旧 Session 兼容测试：缺少字段默认为 false。

验收门：

- 任务根会话可管理 Task，系统提示中存在冻结版本的任务工作流说明，且工具列表中不存在任何 `session_*`；
- 普通根会话和 worker 的系统提示中不存在该说明；
- 普通根会话行为不回退；
- 伪造工具调用不能绕过 capability；
- compaction 前后任务能力保持不变，任务事实从 store 读取而非摘要读取。

### Phase 3：`task_run` 与 TaskRun 生命周期

**目标**：通过唯一领域入口启动 research/execute worker。

工作项：

- [ ] 实现 `task_run` Schema 和执行器。
- [ ] 强制显式 `task_id`、`kind` 和 prompt。
- [ ] 复用现有 Session 创建、模型选择、run coordinator 和 lineage 基础设施。
- [ ] 创建 worker 时写入 Task/TaskRun 关联和冻结 Task 快照。
- [ ] worker capability 强制移除任务和 Session 编排工具。
- [ ] 实现 research 并发上限和 execute 单活认领。
- [ ] 实现 `task_cancel_run`，不向模型暴露 `session_stop`。
- [ ] 增加 starting/running/settled 恢复扫描。
- [ ] UI 展示 TaskRun 和关联 worker Session。

验收门：

- 缺失或错误 task ID 的 Run 不能创建 Session；
- 同一工具调用重放只创建一个 TaskRun/worker；
- 同任务两个 execute 竞争只有一个成功；
- worker 无法调用 `task_run`、任务 CRUD 或任何 `session_*`；
- 创建失败不会留下无法解释的 in_progress Task。

### Phase 4：自动完成投递与 Research 续接

**目标**：research 完成后无需根会话 wait/get，系统自动恢复根会话。

工作项：

- [ ] 在 run settlement observer 中关联 TaskRun 并归档结果。
- [ ] 实现 durable `task_research_result` delivery。
- [ ] 实现根会话 idle admission、忙碌等待、稳定 run ID 和重启扫描。
- [ ] 工作包直接包含受限结果，不要求 `session_get`。
- [ ] 实现 paused/cancelled Task 的 delivery suspend/ignore 规则。
- [ ] UI 显示未读调研结果和失败状态。

验收门：

- fast-settle/register 竞态不丢通知；
- 根会话忙碌时不会启动第二个并发 run；
- 服务在 worker settled 后、parent admitted 前崩溃，重启后只续接一次；
- research 完成不会把 Task 标为 done 或进入 Review。

### Phase 5：Execute Review 闭环

**目标**：执行结果必须 Review，且流程由系统自动发起。

工作项：

- [ ] execute settlement 自动创建唯一 Review 和 delivery。
- [ ] Task 自动进入 review，保存 `pending_review_id`。
- [ ] 实现 Review 工作包和 `task_review_submit`。
- [ ] accept/reject/blocked 状态转换和审计事务化。
- [ ] reject 后自动回到 pending，但不自动复用旧 prompt 启动 worker。
- [ ] 未提交决定时保持 Review pending，并实现退避重投/人工提示。
- [ ] UI 增加 Review 卡片、决定、反馈和历史。
- [ ] 处理用户暂停/归档与 Review 并发提交。

验收门：

- worker succeeded 永远不能直接得到 Task.done；
- accept 是首版唯一正常完成入口；
- Review 重复投递不会重复完成或重复返工；
- 已 cancelled Task 的迟到 accept 不会复活任务；
- 根会话压缩或服务重启后仍能收到完整 Review 工作包。

### Phase 6：完整 UI、同步与可观测性

**目标**：使任务系统达到可日常使用和可诊断状态。

工作项：

- [ ] 完成任务分组、筛选、拖拽排序和详情页。
- [ ] 展示 active research/execute、pending Review、失败和人工介入状态。
- [ ] 实时同步 Task/Run/Review；revision gap 时全量恢复。
- [ ] 增加任务、Run、Review 结构化日志和指标。
- [ ] 日志中只记录 ID、状态和受限摘要，不记录完整 prompt、provider payload 或 secrets。
- [ ] 增加任务系统诊断导出，默认脱敏。
- [ ] 更新 README、用户文档和 `agent-session-orchestration.md`，明确两套根会话模式互斥。

验收门：

- 两个浏览器窗口和根会话同时操作时状态最终一致；
- 用户能从任务追溯 Run、Review 和关联 Session；
- 所有失败状态都有可理解的 UI 提示和恢复动作；
- 普通 Session UI 和编排能力无回归。

### Phase 7：恢复、压力与发布加固

**目标**：证明系统在异常和并发下不会丢任务或错误完成任务。

工作项：

- [ ] SQLite transaction/unique constraint/race tests。
- [ ] coordinator capacity、取消、归档和进程关闭测试。
- [ ] worker/result 大输出 blob 和截断测试。
- [ ] 崩溃点 fault-injection 测试。
- [ ] Web API、WebSocket 重同步和浏览器 E2E。
- [ ] 跨平台 Windows/Linux/macOS 文件与 SQLite 测试。
- [ ] 制定 migration rollback：可关闭 capability/UI，但不删除已落库数据。
- [ ] 小范围 feature flag 发布，观察后再默认开放。

验收门：

- fault-injection 后不存在丢失 Review、重复 worker 或错误 done；
- `go test ./...`、race、前端 build/test、浏览器 smoke 全部通过；
- 旧数据升级无需手工迁移，降级时旧版本不会损坏任务表；
- 完成安全和隐私检查。

## 11. 测试矩阵

### 11.1 领域与存储

- Task 所有合法/非法状态转换；
- expected_version 成功、冲突、重试；
- 排序插入、移动、重平衡和并发；
- execute 单活、research 上限；
- terminal Task mutation 拒绝；
- Review 唯一性和幂等决定；
- migration 和重启恢复。

### 11.2 能力与安全

| 会话类型 | task tools | session tools | work tools |
| --- | ---: | ---: | ---: |
| 普通根会话 | 否 | 按原配置 | 按配置 |
| 任务根会话 | 是 | 否 | 按配置 |
| task_run worker | 否 | 否 | 按 worker 策略 |

测试必须同时覆盖 Schema 不注入和 executor 主动拒绝。

### 11.3 工作流

- research success/failure/cancel → 结果投递，不 Review；
- execute success/failure/cancel → 对应 Review/失败处理；
- parent busy、archived、interrupted；
- Task paused/cancelled while worker running；
- worker fast settlement；
- 重复 settlement event；
- delivery claim 后崩溃；
- accept/reject 与用户归档竞态；
- compaction 后工作项完整恢复。

### 11.4 Web

- CRUD、拖拽排序、冲突提示；
- Session 创建开关不可用于 child；
- Run/Review 实时更新；
- 刷新和断线重连；
- 大结果按需加载；
- 软删除确认和 active Run 警告；
- 键盘操作和基础可访问性。

## 12. 安全与隐私

- 所有任务资源按 project scope 鉴权；不能通过 ID 枚举其他项目。
- 工具结果、HTTP 错误和同步事件不得泄露绝对 server root、provider 凭据或 raw 配置。
- worker 结果只读取 durable、用户可见的 assistant 内容；不把 reasoning、隐藏 item、debug item 或 tool result body 自动注入根会话。
- Task prompt/结果可能含项目敏感信息，诊断日志默认不记录正文。
- 取消和归档检查必须在 settlement 和 Review commit 时再次执行，防止 TOCTOU 覆盖。
- TaskRun 模型选择沿用现有 provider credential 边界，工具结果不返回 secret。

## 13. 发布和回滚

建议使用 server-root feature flag：

```yaml
features:
  project_tasks: false
```

发布顺序：

1. 先部署 migration 和关闭状态下的后端代码；
2. 启用内部 CRUD/UI；
3. 启用任务根会话能力；
4. 启用 research TaskRun；
5. 启用 execute + Review；
6. 观察后再向全部用户开放。

回滚原则：

- 关闭 feature flag 后不再注入工具、不显示入口、不启动新 TaskRun；
- 已运行 worker 允许安全结算并持久化结果；
- 已有 Task/Run/Review 数据保留，不做破坏性 down migration；
- 恢复启用后从 durable inbox 和状态扫描继续。

## 14. 全局完成标准

只有同时满足以下条件，项目级任务系统才可视为完成：

1. 用户能在项目 UI 持久管理、暂停、归档和排序任务。
2. 创建根会话时能选择任务系统能力，且选择被冻结。
3. 任务根会话没有任何 Session 工具，只能以显式 `task_id` 调用 `task_run`。
4. 任务根会话自动获得版本化的工作流系统提示；普通根会话和 worker 不获得该提示。
5. research 和 execute 子会话都不拥有任务或 Session 编排能力。
6. research 结果自动续接根会话，不需要 wait/get。
7. execute 结果自动进入 Review，worker 完成不能直接完成 Task。
8. accept/reject/blocked 由受约束 Review 提交驱动，状态转换由系统事务化完成。
9. 压缩、刷新、断线、进程重启和重复事件都不会丢任务、丢 Review 或重复启动 worker。
10. 用户 UI 与根会话并发修改不会静默覆盖。
11. 普通会话现有 Session 编排能力和历史数据保持兼容。

## 15. 开发验证命令

每阶段至少运行：

```sh
gofmt -w <edited-go-files>
go test ./...
cd web && npm run build
cd web && npm test -- --run
git diff --check
```

涉及 coordinator、SQLite 并发或 inbox 时增加：

```sh
go test -race ./internal/tasks ./internal/execution ./internal/sessions
```

涉及完整 Web 流程时执行仓库现有浏览器 smoke/E2E 脚本，并为“创建任务根会话 → research → 自动续接 → execute → 自动 Review → accept”增加一条端到端主路径。
