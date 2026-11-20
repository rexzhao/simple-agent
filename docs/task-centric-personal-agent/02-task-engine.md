# Task Engine 详细设计文档

**版本：** V1.2  
**文档状态：** 模块详细设计。数据模型、状态枚举、事件和 API 以 `04-integration-and-mvp-plan.md` 为准，冲突时以 04 为准。  
**更新日期：** 2026-11-19  
**所属系统：** Task-oriented Personal Agent  
**模块：** Task Engine  
**设计目标：** 构建一个支持任务拆解、依赖管理、并行调度、暂停恢复和持久化执行的个人任务引擎。

---

## 1. 设计目标与职责边界

### 1.1 核心目标

Task Engine 是整个系统的业务任务管理核心，负责：

1. 创建和管理 Task。
2. 维护 Task 生命周期和状态机。
3. 将复杂任务拆解为子任务。
4. 管理 Task 之间的依赖关系。
5. 调度 Agent Execution。
6. 管理任务暂停、恢复、重试和取消。
7. 管理用户交互等待状态。
8. 处理任务并发和执行冲突。
9. 维护任务进度。
10. 将任务执行结果汇总为可交付结果。

### 1.2 职责边界

| 模块 | 负责内容 |
|---|---|
| Conversation Service | 用户消息和对话管理 |
| Main Agent | 理解用户意图、任务协调和结果汇总 |
| Task Engine | 任务状态、依赖、调度和生命周期 |
| Agent Runtime | 模型调用、工具调用和 Agent 执行 |
| Memory Service | 记忆提取、检索和更新 |
| Document Service | 任务文档和产物管理 |
| Tool Gateway | 工具权限与执行 |

Task Engine 不负责决定 Agent 的推理过程，也不负责直接生成模型回答。

**Task Engine 管理任务事实，Agent Runtime 执行任务。**

### 1.3 设计原则

- Task 是持久化业务实体。
- Execution 是一次运行过程。
- Task 状态不能由模型直接修改。
- 一个 Task 可以包含多个子任务。
- 一个 Task 可以产生多个 Execution。
- 一个 Task 可以暂停，但不能因为程序重启而丢失。
- 用户交互必须绑定具体 Task 和 Interaction Request。
- 所有关键状态变化必须可审计。
- 任务调度必须支持幂等和恢复。

---

## 2. 核心领域模型

### 2.1 Task

Task 是用户委托给个人 AI 助手的一项独立工作。

例如：

用户说：

> 帮我设计一个个人 AI 助手系统，并输出技术设计文档。

系统可以创建：

```text
Task: 设计个人 AI 助手系统
  |
  +-- Subtask 1: 需求分析
  |
  +-- Subtask 2: 总体架构设计
  |
  +-- Subtask 3: Agent Runtime 设计
  |
  +-- Subtask 4: Task Engine 设计
  |
  +-- Subtask 5: Memory & Document 设计
  |
  +-- Subtask 6: 最终方案整合
```

Task 不等同于用户的一条消息，也不等同于一个 Agent Session。

### 2.2 核心实体

| 实体 | 说明 |
|---|---|
| Task | 用户委托的业务任务 |
| Task Node | 任务中的一个可执行节点 |
| Task Dependency | 任务节点之间的依赖 |
| Task Session | 任务的一段执行会话 |
| Execution | Agent 的一次执行 |
| Checkpoint | 执行恢复快照 |
| Interaction Request | 等待用户参与的请求 |
| Task Event | 任务状态和过程事件 |
| Task Artifact | 任务产物 |
| Task Decision | 用户或 Agent 做出的重要决策 |

其中 Task Node 是 Task Engine 的内部执行单元，不必在第一阶段全部作为独立的 Task 对外展示。

### 2.3 实体关系

```text
Workspace
   |
   +-- Conversation
   |
   +-- Task
         |
         +-- Task Node
         |     |
         |     +-- Dependency
         |
         +-- Task Session
         |
         +-- Execution
         |
         +-- Interaction Request
         |
         +-- Decision
         |
         +-- Artifact
         |
         +-- Event
```

从 Phase 1 起，每个 Task 至少有一个 Task Node：简单任务是单节点计划，Phase 4 引入 Task Planner 后才生成多节点计划。`parent_task_id` 保留给未来用户主动创建的嵌套任务，MVP 不使用（04 §3.1）。

---

## 3. Task 状态机

### 3.1 状态定义

Task 状态采用以下枚举：

| 状态 | 说明 | 是否终态 |
|---|---|---|
| CREATED | 已创建 | 否 |
| PLANNING | 正在规划 | 否 |
| READY | 规划完成，等待执行 | 否 |
| RUNNING | 正在执行 | 否 |
| WAITING_USER | 等待用户输入或确认 | 否 |
| BLOCKED | 被依赖或外部条件阻塞 | 否 |
| PAUSED | 用户或系统主动暂停 | 否 |
| REVIEWING | 正在审核结果 | 否 |
| COMPLETED | 已完成 | 是 |
| FAILED | 执行失败 | 是 |
| CANCELLED | 已取消 | 是 |

终态 Task 不能通过普通状态转换离开终态。用户要求重新开展时，执行显式的 REOPEN 命令：Task 进入 PLANNING，`cycle` 加 1，并创建新 Session；上一周期的节点和执行记录保留为历史（04 §3.7，Phase 5 实现）。

### 3.2 状态转换

状态图和完整转换表以 04 §5.1 为准，这里给出主路径：

```text
CREATED
   |
   v
PLANNING ----------------------+
   |                           |
   v                           v
READY                    WAITING_USER（规划需澄清）
   |
   v
RUNNING <-------------------------------+
   |                                    |
   +---> WAITING_USER ---(回答)---------+
   +---> BLOCKED ------(解除)---> READY / RUNNING
   +---> PAUSED -------(恢复)---> READY / RUNNING
   |
   v
REVIEWING ---(返工)---> RUNNING
   |
   v
COMPLETED

任意非终态 ---> FAILED / CANCELLED（以转换表为准）
COMPLETED / FAILED / CANCELLED ---(仅 REOPEN 命令)---> PLANNING
```

### 3.3 状态转换规则

| 当前状态 | 允许转换 | 触发条件 |
|---|---|---|
| CREATED | PLANNING、CANCELLED | 规划开始或取消 |
| PLANNING | READY、WAITING_USER、BLOCKED、FAILED、CANCELLED | 规划完成、需要澄清或异常 |
| READY | RUNNING、PAUSED、CANCELLED | 启动执行 |
| RUNNING | WAITING_USER、BLOCKED、PAUSED、REVIEWING、FAILED、CANCELLED | 执行过程 |
| WAITING_USER | RUNNING、PLANNING、PAUSED、CANCELLED | 收到用户回答（规划阶段的等待回到 PLANNING）或取消 |
| BLOCKED | READY、RUNNING、PAUSED、CANCELLED、FAILED | 阻塞条件解除或无法解除 |
| PAUSED | READY、RUNNING、CANCELLED | 恢复执行 |
| REVIEWING | COMPLETED、RUNNING、FAILED、CANCELLED | 审核结果 |
| COMPLETED | PLANNING（仅 REOPEN 命令） | 终态 |
| FAILED | PLANNING（仅 REOPEN 命令） | 终态 |
| CANCELLED | PLANNING（仅 REOPEN 命令） | 终态 |

补充规则：

1. 状态转换只能通过 Task Engine 的领域服务完成。
2. 不允许 Agent 直接写 Task.status。
3. 每次状态变化必须生成 Task Event（`task.status_changed` 及对应的语义事件，04 §7.2）。
4. 每次转换必须校验当前状态和版本号。
5. 非法状态转换必须拒绝并记录。
6. REOPEN 不是普通转换，只能由 REOPEN 命令触发（`reopen_task`），普通 `transition()` 调用中终态没有出口。

### 3.4 状态转换实现

```go
var allowedTransitions = map[TaskStatus][]TaskStatus{
    TaskCreated:     {TaskPlanning, TaskCancelled},
    TaskPlanning:    {TaskReady, TaskWaitingUser, TaskBlocked, TaskFailed, TaskCancelled},
    TaskReady:       {TaskRunning, TaskPaused, TaskCancelled},
    TaskRunning:     {TaskWaitingUser, TaskBlocked, TaskPaused, TaskReviewing, TaskFailed, TaskCancelled},
    TaskWaitingUser: {TaskRunning, TaskPlanning, TaskPaused, TaskCancelled},
    TaskBlocked:     {TaskReady, TaskRunning, TaskPaused, TaskCancelled, TaskFailed},
    TaskPaused:      {TaskReady, TaskRunning, TaskCancelled},
    TaskReviewing:   {TaskCompleted, TaskRunning, TaskFailed, TaskCancelled},
    // 终态没有出口；REOPEN 走单独的命令（04 §3.7）
}

var semanticEvents = map[TaskStatus]string{ // 04 §7.2
    TaskWaitingUser: "task.waiting_user",
    TaskBlocked:     "task.blocked",
    TaskPaused:      "task.paused",
    TaskCompleted:   "task.completed",
    TaskFailed:      "task.failed",
    TaskCancelled:   "task.cancelled",
}
```

```go
// transition 是 Task 状态的唯一写入入口；调用方必须处在写事务中。
func (e *Engine) transition(tx *db.Tx, t *Task, to TaskStatus, reason any) error {
    if !slices.Contains(allowedTransitions[t.Status], to) {
        return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.Status, to)
    }

    from := t.Status
    t.Status = to
    t.StatusReason = reason
    t.UpdatedAt = e.clock.Now()
    if to == TaskCompleted {
        t.CompletedAt = &t.UpdatedAt
    }

    // 乐观锁：WHERE id = ? AND version = ?，影响 0 行返回 ErrVersionConflict
    if err := e.repo.UpdateWithVersion(tx, t); err != nil {
        return err
    }

    events := []events.New{taskEvent(t, "task.status_changed", map[string]any{
        "from": from, "to": to, "reason": reason,
    })}
    events = append(events, semanticEventsFor(t, from, to)...) // task.started / task.resumed / semanticEvents
    return e.events.Append(tx, events...)                       // 同一事务写入 events 表
}
```

任务状态更新和事件写入必须在同一个写事务中完成；事务提交后才通知事件总线。

## 4. 任务拆解机制

### 4.1 为什么需要任务拆解

简单任务不需要拆分。

例如：

> 帮我把这段话翻译成英文。

这类请求可以由 Main Agent 直接完成。

复杂任务则需要拆分：

> 帮我调研市场、比较竞品、制定产品方案并输出商业计划书。

这种任务可以拆分为多个有依赖关系的节点。

### 4.2 拆解策略

建议 Main Agent 负责决定是否拆解任务，Task Planner 负责生成具体执行计划。

```text
User Request
     |
     v
Main Agent
     |
     v
Complexity Assessment
     |
     +---- Simple Task
     |        |
     |        v
     |    Direct Execution
     |
     +---- Complex Task
              |
              v
         Task Planner
              |
              v
        Task Decomposition
              |
              v
        Dependency Graph
              |
              v
         Task Engine
```

### 4.3 Task Plan 数据结构

```json
{
  "task_id": "task-001",
  "goal": "完成产品技术方案",
  "nodes": [
    {
      "node_key": "node-1",
      "title": "需求分析",
      "description": "整理系统目标和核心约束",
      "completion_criteria": "输出需求清单和约束列表",
      "dependencies": [],
      "required": true,
      "weight": 1
    },
    {
      "node_key": "node-2",
      "title": "总体架构",
      "description": "设计系统模块和技术架构",
      "completion_criteria": "输出模块划分和架构图",
      "dependencies": ["node-1"],
      "required": true,
      "weight": 2
    },
    {
      "node_key": "node-3",
      "title": "Runtime 设计",
      "description": "设计 Agent Runtime",
      "completion_criteria": "输出 Runtime 详细设计",
      "dependencies": ["node-2"],
      "required": true,
      "weight": 2
    }
  ]
}
```

节点是否可以并行由依赖关系决定：没有未完成前置依赖的节点都可以并行，实际并发度由调度器配置控制。

Task Planner 的输出必须经过结构校验。

不能直接把模型生成的任务列表当作有效执行计划。

### 4.4 任务拆解粒度

拆解时遵循以下规则：

- 一个节点应有明确的输入和输出。
- 节点应具备可验证的完成条件。
- 节点不宜过小，否则调度成本过高。
- 节点不宜过大，否则难以恢复和追踪。
- 只有存在真实依赖关系时才建立依赖。
- 能够独立完成的节点可以并行执行。

建议第一阶段把 Task Node 数量控制在每个复杂任务 3～15 个，超出时再考虑分层拆解。这是工程初始建议，不是硬性限制。

---

## 5. 任务依赖管理

### 5.1 DAG 模型

任务依赖关系采用有向无环图（DAG）。

```text
           A
          / \
         v   v
         B   C
          \ /
           v
           D
          / \
         v   v
         E   F
```

含义：

- A 完成后，B 和 C 可以并行执行。
- B、C 都完成后，D 才能执行。
- D 完成后，E 和 F 可以并行执行。

### 5.2 依赖关系类型

第一阶段仅支持：

**FINISH_TO_START**

即前置节点完成后，后置节点才能启动。

后续可扩展：

- START_TO_START。
- FINISH_TO_FINISH。
- 条件依赖。

第一阶段不要引入过多复杂调度语义。

### 5.3 依赖校验

创建依赖时必须检查：

1. 两个节点属于同一 Task、同一周期。
2. 不能依赖自身。
3. 不允许形成循环依赖。
4. 前置节点必须存在。
5. 依赖变更必须通过事务提交。

循环检测可使用 DFS 或拓扑排序：

```go
// hasCycle 检查以 nodeKey 为键、后继列表为值的有向图是否有环。
func hasCycle(graph map[string][]string) bool {
    const (
        unvisited = iota
        visiting
        done
    )
    state := make(map[string]int, len(graph))

    var visit func(n string) bool
    visit = func(n string) bool {
        switch state[n] {
        case visiting:
            return true
        case done:
            return false
        }
        state[n] = visiting
        for _, next := range graph[n] {
            if visit(next) {
                return true
            }
        }
        state[n] = done
        return false
    }

    for n := range graph {
        if visit(n) {
            return true
        }
    }
    return false
}
```

### 5.4 节点就绪条件

Task Node 满足以下条件时才能进入 READY：

- 自身尚未完成。
- 所有前置节点均已达到允许的完成状态。
- Task 未被暂停或取消。
- 没有阻塞条件。
- 执行资源和权限可用。

对于失败的前置节点，默认不能视为完成。

如果允许跳过失败节点，必须由任务策略明确授权，并记录原因。

---

## 6. 调度器设计

### 6.1 调度器职责

Task Scheduler 负责：

- 找出可执行节点。
- 判断资源是否可用。
- 创建 Execution。
- 启动 Agent Runtime。
- 监听执行结果。
- 处理重试。
- 更新 Task Node 状态。
- 触发后续依赖节点。

### 6.2 调度流程

```text
Task Engine
    |
    v
Find READY Nodes
    |
    v
Check Dependencies
    |
    v
Check Task Status
    |
    v
Check Execution Capacity
    |
    v
Create Execution
    |
    v
Dispatch Agent Runtime
    |
    v
Wait for Execution Result
    |
    v
Update Node Status
    |
    v
Schedule Next Nodes
```

### 6.3 调度策略

第一阶段建议采用：

- FIFO：先创建的任务优先。
- Priority：用户明确标记为紧急的任务优先。
- Fairness：避免单个复杂任务长期占用全部资源。
- Dependency First：只调度依赖已经满足的节点。

初始优先级：

| 优先级 | 数值 |
|---|---:|
| LOW | 10 |
| NORMAL | 50 |
| HIGH | 80 |
| URGENT | 100 |

默认 NORMAL。

用户可以明确调整任务优先级，但系统不应仅凭模型对任务重要性的推测就频繁调整优先级。

### 6.4 并发控制

配置项（实例 `config.yaml`，04 §15.1）：

```yaml
scheduler:
  max_global_executions: 3        # 本实例同时运行的 Execution 上限
  max_executions_per_task: 1      # P4 起默认 2
```

模型和工具的并发由 Model Gateway、Tool Gateway 自行控制。

以上数值是单实例的初始配置。多个实例各自独立计数，彼此之间不协调；同时打开很多实例时，供应商的限流由 Model Gateway 的重试处理。

特别注意：

**一个 Task 可以有多个并行节点，但同一个 Task 的共享文档不能被多个 Execution 无协调地同时覆盖。** 文档写入由 Document Service 的 `base_version` 校验串行化，冲突时 Agent 读取最新版本后重新提交（03 §11.3）。

## 7. Task 与 Execution 协同机制

### 7.1 状态分离

Task 状态和 Execution 状态不能合并。

例如：

```text
Task A: RUNNING
   |
   +-- Execution 1: COMPLETED
   |
   +-- Execution 2: RUNNING
   |
   +-- Execution 3: PENDING
```

Task 仍然处于 RUNNING。

又例如：

```text
Task B: WAITING_USER
   |
   +-- Execution 1: WAITING_USER
   |
   +-- Execution 2: COMPLETED
```

Task 等待用户输入，但已经完成的执行结果仍然保留。

### 7.2 状态映射

| Execution 结果 | Task Node 处理 | Task 处理 |
|---|---|---|
| COMPLETED | 标记节点完成 | 检查后续节点 |
| FAILED | 标记节点失败或待重试 | 根据策略决定 |
| WAITING_USER | 标记节点等待用户 | 通常进入 WAITING_USER |
| PAUSED | 标记节点暂停 | 根据暂停范围决定 |
| CANCELLED | 标记节点取消 | 检查剩余节点 |

Task 状态不能简单地等于所有 Execution 状态的汇总。

### 7.3 多节点完成判定

Task 的完成条件：

1. 所有必需节点完成。
2. 所有必要产物已提交。
3. 所有阻塞问题已解决。
4. 最终审核通过（如果任务需要审核）。

满足后：

```text
RUNNING
   |
   v
REVIEWING
   |
   v
COMPLETED
```

所有任务都经过 REVIEWING。不需要审核的任务，由调度器在同一次处理中立即判定通过并转为 COMPLETED；需要审核的任务（Phase 5 的 Reviewer Agent）停留在 REVIEWING 直到审核结束。

---

## 8. 用户交互和任务恢复

### 8.1 核心要求

用户始终通过 Main Agent 交互。

Task Engine 不向用户直接暴露内部 Agent，也不要求用户进入某个子任务会话。

### 8.2 Interaction Request 绑定

每个交互请求必须关联：

- interaction_id
- task_id
- task_node_id（可选）
- execution_id
- conversation_id
- question_type（BLOCKING / CONFIRMATION / IMPORTANT / OPTIONAL）
- status

交互状态（完整转换表见 04 §5.4）：

```text
PENDING
   |
   v
PRESENTED
   |
   v
ANSWERED（包括用户拒绝：answer.type = decline）
   |
   v
CONSUMED
```

其他状态：

- EXPIRED：超过 `expires_at` 未回答。
- CANCELLED：所属 Task 或 Execution 被取消。
- REJECTED：系统拒绝该请求本身（无效、重复、越权），不向用户呈现。

### 8.3 多任务等待

例如：

```text
Task A -> WAITING_USER
Task B -> RUNNING
Task C -> WAITING_USER
```

Main Agent 应将多个待处理问题汇总，而不是分别创建多个用户会话。

但对于具有安全风险或操作授权性质的问题，不应为了减少打扰而合并到一个含糊的确认中。

### 8.4 用户回答路由

```text
User Message
     |
     v
Main Agent
     |
     v
Resolve Interaction
     |
     v
Find interaction_id
     |
     v
Validate Request Status
     |
     v
Persist User Answer
     |
     v
Resume Correct Execution
```

优先使用明确的 Interaction ID。

如果用户通过自然语言回答，Main Agent 应结合当前待处理问题判断目标；存在歧义时先向用户澄清，不能随意选择一个 Task。

### 8.5 任务恢复策略

用户回答后：

1. 保存原始回答。
2. 更新 Interaction Request。
3. 更新 Task 决策记录（如回答构成决策）。
4. 检查 Task 当前状态。
5. 验证依赖和资源条件。
6. 创建恢复命令。
7. 从最新有效 Checkpoint 恢复 Execution。
8. 更新 Task 和 Task Node 状态。

恢复不是简单地将 status 改成 RUNNING。

---

## 9. 暂停、取消和重试

### 9.1 暂停

暂停分为两种：

**Task 级暂停**

暂停该 Task 下所有可暂停的活动节点。

**Execution 级暂停**

暂停单次 Execution，但不影响同一 Task 中其他节点。

第一阶段用户界面只提供 Task 级暂停；Execution 级暂停作为内部能力。

暂停操作应支持协作式取消：

- 设置取消或暂停标志。
- 等待 Agent Runtime 在安全边界检查。
- 保存 Checkpoint。
- 停止新的工具调用。
- 更新相关状态。

对于已经开始的不可撤销外部操作，不能假设暂停可以回滚。

### 9.2 取消

取消是终态。

取消 Task 时：

1. 停止调度新的节点。
2. 向运行中的 Execution 发送取消信号。
3. 撤销尚未执行的任务节点。
4. 保留已经产生的文档和执行记录。
5. 记录取消原因。

取消不能简单删除任务数据。

### 9.3 重试

失败重试必须创建新的 Execution，而不是覆盖旧 Execution。

```text
Task Node
   |
   +-- Execution 1: FAILED
   |
   +-- Execution 2: FAILED
   |
   +-- Execution 3: COMPLETED
```

每次重试都需要记录：

- retry_count
- retry_reason
- previous_execution_id
- retry_policy
- next_retry_at

建议错误分为：

| 类型 | 策略 |
|---|---|
| 临时网络错误 | 自动重试 |
| 模型限流 | 延迟重试 |
| 工具权限错误 | 等待用户处理 |
| 输入缺失 | 请求用户补充 |
| 业务逻辑错误 | 重新规划 |
| 预算耗尽 | 暂停或请求批准 |
| 用户取消 | 不重试 |

建议对可重试错误采用指数退避，并设置最大重试次数。

---

## 10. 数据库详细设计

本模块拥有以下表，完整 DDL 见 04 §4.3：

| 表 | 用途 | 引入阶段 |
|---|---|---|
| tasks | Task 主表：状态、优先级、周期（cycle）、当前 Session、进度、乐观锁版本 | P1 |
| task_sessions | Task 的逻辑会话 | P1 |
| task_nodes | 可调度的执行节点：状态、是否必需、权重、重试次数与时间、输入输出 | P1 |
| task_commands | 用户操作命令（取消、暂停、恢复、回答等），按幂等键去重 | P1 |
| task_dependencies | 节点依赖（仅 FINISH_TO_START） | P4 |

相关表：

- 任务时间线使用共享的 `events` 表（按 `task_id` 查询），不再单独建 `task_events`。
- Execution 使用 Agent Runtime 的 `agent_executions`，通过 `task_id`、`task_node_id` 关联。
- 用户交互使用 `user_interaction_requests`，由 Interaction Service 管理。

命令类型：

- START_TASK
- PAUSE_TASK
- RESUME_TASK
- CANCEL_TASK
- REOPEN_TASK
- START_NODE
- RETRY_NODE
- PROCESS_USER_ANSWER
- APPEND_INSTRUCTION

命令幂等键格式：`{command_type}:{task_id}:{discriminator}`，例如 `PROCESS_USER_ANSWER:{task_id}:{interaction_id}`。

## 11. 工作流引擎设计

### 11.1 定位

产品是单进程桌面程序，同一目录只有一个实例在运行，因此不引入外部工作流服务。长任务的推进、等待、重试和程序重启后的恢复，由进程内的 **Task Scheduler** 完成（04 §14.2）。

设计原则：

- **数据库状态是唯一依据。** Scheduler 不在内存中保存“执行到哪一步”，每次被唤醒都根据数据库状态计算下一步（reconcile）。
- **串行推进。** 所有 Task / 节点状态变更都由 Scheduler 的单个 goroutine 执行；Runner 只执行 Agent 并回报结果。
- **幂等。** 同一个 reconcile 执行多次，结果相同。

### 11.2 结构

```text
Task Scheduler（单 goroutine）
    |
    +-- 命令处理：task_commands（暂停、恢复、取消、回答……）
    |
    +-- 结果处理：Runner 回报的 ExecutionResult
    |
    +-- reconcile(task)：对每个非终态 Task 计算下一步
    |
    +-- 定时：next_retry_at、expires_at
    |
    +-- Runner 登记表：map[executionID]cancelFunc
           |
           +-- Runner A（goroutine）→ Runtime.Execute / Resume
           +-- Runner B
```

### 11.3 调度循环示意

```go
func (s *Scheduler) Run(ctx context.Context) {
    s.startupReconcile(ctx) // 04 §14.2「启动对账」

    for {
        s.processCommands(ctx)           // 1. PENDING 命令
        s.drainResults(ctx)              // 2. Runner 回报
        for _, id := range s.openTaskIDs(ctx) {
            s.reconcile(ctx, id)         // 3. 每个非终态 Task
        }

        select {                         // 4. 等待下一次唤醒
        case <-ctx.Done():
            s.shutdown()                 // 取消所有 Runner，Execution 保持 RUNNING
            return
        case <-s.bus.Wake():             // 事件总线：新命令、新事件
        case res := <-s.results:         // Runner 完成
            s.pending = append(s.pending, res)
        case <-s.timer(s.nextDue(ctx)):  // 最早的重试或过期时间
        }
    }
}

func (s *Scheduler) reconcile(ctx context.Context, taskID ID) {
    view := s.tasks.LoadView(ctx, taskID)

    switch {
    case view.Status == TaskCreated:
        s.plan(ctx, view) // P1：单节点计划；P4：Task Planner。CREATED → PLANNING → READY

    case view.Status == TaskPaused:
        return            // 等待 RESUME_TASK 命令

    default:
        for _, n := range view.RunningNodes {
            if !s.runners.Has(n.ExecutionID) { // 程序重启后的孤立执行
                s.startRunner(ctx, n.ExecutionID, resume)
            }
        }
        for _, n := range view.RetryDueNodes { // FAILED 且 next_retry_at 已到
            s.tasks.MarkNodeReady(ctx, n.ID)
        }
        for _, n := range s.capacity.Take(view.ReadyNodes) { // 遵守并发上限
            execID := s.tasks.StartNode(ctx, n.ID) // 一个事务：创建 Execution、节点 → RUNNING、Task → RUNNING
            s.startRunner(ctx, execID, fresh)
        }
        if view.AllRequiredNodesDone() {
            s.tasks.Finalize(ctx, taskID) // → REVIEWING → COMPLETED / FAILED
        }
    }
}
```

这是逻辑示意。实现要点：

- `startRunner` 必须在创建 Execution 的事务**提交之后**调用。
- `reconcile` 中的每一步都是独立的短事务，任何一步之后退出程序都能继续。
- **运行中暂停和取消：** 命令处理时先设置 Execution 的 `pause_requested` / `cancel_requested`（取消还会调用 Runner 的 `cancel()`），执行循环在迭代边界检查后返回，结果经 Runner 回报给 Scheduler。

### 11.4 用户交互等待

当 Task 进入 WAITING_USER：

- Interaction Request 已持久化，Execution 处于 WAITING_USER，Runner 已结束。
- Scheduler 不需要“挂起等待”，Task 在数据库中保持 WAITING_USER 即可；程序可以关闭，等待可以持续任意时长。
- 用户回答后写入 PROCESS_USER_ANSWER 命令并唤醒 Scheduler。
- Scheduler 校验回答，调用 `Runtime.Resume`，Interaction → CONSUMED，节点和 Task → RUNNING。

## 12. 可靠性和一致性设计

### 12.1 事务与事件

以下数据必须在同一个写事务中写入：

- Task 状态更新。
- 事件（`events` 表）。
- Task Command 创建或状态更新。
- Execution 创建。

```text
db.Write(BEGIN IMMEDIATE)
    |
    +-- Update Task / Node
    +-- Insert Execution（如需要）
    +-- Insert Events
    |
    v
Commit
    |
    v
通知事件总线 → 唤醒 Scheduler、刷新界面、驱动持久化消费者
```

通知只是“有新数据”的信号。即使通知在提交后、发出前程序退出，下次启动时 Scheduler 的启动对账和持久化消费者的 `last_seq` 也会补上（04 §7.3）。

### 12.2 幂等处理

所有命令必须有幂等键。

例如：

`PROCESS_USER_ANSWER:{task_id}:{interaction_id}`

同一个用户回答重复提交时，不能创建多个 Execution。

### 12.3 并发更新

- SQLite 只有一个写连接，所有写事务串行执行（04 §14.3），不会出现两个事务同时写同一行。
- 但读出数据到写回之间可能有其他事务修改，因此带 `version` 的表仍然使用乐观锁。
- Task / 节点状态只由 Scheduler 的 goroutine 写入（用户操作经命令表转交），进一步避免逻辑竞争。
- 唯一约束防止重复记录（命令幂等键、工具调用幂等键、节点 key）。

数据库是最终一致性判断依据。

### 12.4 故障恢复

程序启动时执行启动对账（04 §14.2）：

1. 扫描未完成的 Task，逐个执行 reconcile。
2. RUNNING / PENDING 的 Execution 没有对应 Runner（启动时全部如此）→ 从 Checkpoint 继续。
3. PROCESSING 状态的命令 → PENDING，重新处理。
4. 持久化消费者从各自的 `last_seq` 继续。
5. 中断的 Main Agent 回合 → 标记为 INTERRUPTED，界面提供重试。

因为同一目录只有一个实例，启动时可以确定没有其他进程在执行这些任务，不需要心跳超时判断。

## 13. API 设计

本模块不提供 HTTP 接口。界面和 Main Agent 通过 `TaskService`（04 §6.3）操作任务，完整的界面操作清单见 04 §12：

| 操作 | 方法 | 阶段 |
|---|---|---|
| 创建任务 | `TaskService.Create` | P1 |
| 任务列表 | `TaskService.List` | P1 |
| 任务详情 | `TaskService.View` | P1 |
| 任务时间线 | `TaskService.Events` | P1 |
| 取消 | `TaskService.Cancel` | P1 |
| 暂停 / 恢复 | `TaskService.Pause` / `Resume` | P2 |
| 回答问题 | `InteractionService.Answer` | P2 |
| 进度 | `TaskService.View`（含进度） | P4 |
| 重新打开 | `TaskService.Reopen` | P5 |

创建任务的参数（`CreateTaskCmd`）：

```go
CreateTaskCmd{
    ConversationID: convID,
    Title:          "设计个人 AI 助手",
    Description:    "完成技术架构设计",
    Goal:           "输出可落地的架构文档",
    Constraints:    []string{"使用 SQLite"},
    Priority:       50,
}
```

服务层负责输入验证；具体状态转换交由 Task Engine 和 Scheduler 完成。

## 14. 任务进度计算

### 14.1 为什么不能只用百分比

任务进度并不总是线性的。

例如：

- 前期研究可能耗时很长。
- 后期文档生成可能很快。
- 一个节点可能产生多个子任务。
- 用户等待时间不代表 Agent 正在工作。

因此应区分：

- 节点完成率。
- 执行阶段。
- 当前活动。
- 等待原因。
- 预计剩余工作。

### 14.2 计算方法

初期可以采用加权完成率：

$$
Progress = \frac{\sum_i w_i \cdot completed_i}{\sum_i w_i} \times 100\%
$$

其中：

- $w_i$ 是节点权重（`task_nodes.weight`）。
- $completed_i$ 为节点完成标志（COMPLETED 或 SKIPPED 记为 1）。

如果没有显式权重，则所有节点权重设为 1。

必须区分系统实际进度与模型估计进度。

前者来自已完成节点和实际执行事件；后者可以作为辅助信息展示，不能混为一谈。

---

## 15. 前端任务呈现

虽然用户始终只有一个主对话入口，但系统应提供任务状态的透明反馈。

建议 UI 包含：

### 15.1 主对话

用户在一个输入框中提交需求。

Main Agent 负责：

- 理解任务。
- 告知任务已开始。
- 汇报重要进展。
- 询问必要问题。
- 汇总结果。

### 15.2 任务面板

可以作为主对话的辅助视图，展示：

- 当前活动任务。
- 待处理问题。
- 已完成任务。
- 失败或暂停任务。
- 任务进度。
- 任务产物。

### 15.3 多任务状态示例

```text
正在进行
  |
  +-- 设计 Agent Runtime
  |      RUNNING
  |
  +-- 研究技术选型
  |      WAITING_USER
  |
  +-- 整理文档
         READY

已完成
  |
  +-- 初始需求分析
         COMPLETED
```

任务面板是信息展示，不应成为用户与内部 Task Agent 交互的独立入口。

---

## 16. 测试和验收

### 16.1 状态机测试

- 非法状态转换被拒绝。
- 并发状态更新不会覆盖新状态。
- 终态任务不能被意外恢复。
- 状态事件与业务状态保持一致。

### 16.2 调度测试

- 前置节点完成后启动后置节点。
- 多个无依赖节点能够并行。
- 依赖循环被拒绝。
- 超过并发上限时不会启动新执行。
- 一个任务失败不会错误地影响其他独立任务。

### 16.3 交互测试

- 多任务同时等待用户输入。
- 用户回答只恢复对应任务。
- 重复提交回答不会重复启动执行。
- 用户拒绝授权后不会执行外部操作。
- 用户暂停任务后不再启动新节点。

### 16.4 恢复测试

- 任务执行中程序退出（正常关闭和强制结束），重新打开后继续。
- 等待用户期间程序退出，重新打开后回答，任务恢复。
- 写事务提交后、事件总线通知前程序退出。
- Execution 已创建、Runner 尚未启动时程序退出。
- 工具执行成功但结果保存前程序退出。
- 命令处于 PROCESSING 时程序退出。

### 16.5 核心验收指标

| 指标 | 验收要求 |
|---|---|
| 状态转换一致性 | 不出现非法状态 |
| 命令幂等性 | 重复命令不重复执行 |
| 任务隔离 | 不发生跨 Task 上下文污染 |
| 恢复能力 | 非终态任务可以恢复 |
| 依赖正确性 | 不提前启动未就绪节点 |
| 用户交互路由 | 回答准确绑定 Interaction |
| 调度可靠性 | 不丢失已提交的调度命令 |
| 事件追溯 | 关键状态变化均有记录 |

---

## 17. 实施路线

本模块的实施任务已合并到统一任务清单 `05-implementation-backlog.md`：

| 原阶段 | 对应任务 |
|---|---|
| 基础任务引擎 | M1-03 状态机、M1-04 TaskService 与命令表、M1-09 Task Scheduler 基础版 |
| 复杂任务管理 | M4-01 Task Planner、M4-02 调度策略、M4-03 并行执行、M4-06 进度 |
| 等待和恢复 | M2-01 ~ M2-05 |
| 可靠性和优化 | M0-05 事件日志与持久化消费者、M2-04 调度器命令、M2-08 重试、M2-09 启动对账、M2-10 故障注入 |

## 18. 架构决策记录

| 编号 | 决策 | 原因 |
|---|---|---|
| ADR-TE-001 | Task 是业务生命周期实体 | 与模型执行解耦 |
| ADR-TE-002 | Execution 独立管理 | 支持重试和恢复 |
| ADR-TE-003 | Task 使用明确状态机 | 防止状态混乱 |
| ADR-TE-004 | 复杂任务采用 DAG | 支持依赖和并行 |
| ADR-TE-005 | 第一阶段只支持 Finish-to-Start | 控制复杂度 |
| ADR-TE-006 | 任务调度由 Task Engine 管理 | 防止 Agent 自行控制业务状态 |
| ADR-TE-007 | （V1.2 修订）进程内 Task Scheduler 基于数据库状态推进长任务 | 单进程单实例，无需外部工作流服务 |
| ADR-TE-008 | 用户交互通过 Main Agent | 保持统一用户体验 |
| ADR-TE-009 | 命令采用幂等处理 | 避免重复执行 |
| ADR-TE-010 | 事件与状态更新采用事务保障 | 避免数据和调度不一致 |
| ADR-TE-011 | 第一阶段采用模块化单体 | 降低部署和维护成本 |
| ADR-TE-012 | 终态 Task 只能通过用户显式 REOPEN 进入新周期 | 保留历史任务语义 |
| ADR-TE-013 | 每个 Task 至少一个 Task Node | 所有阶段使用同一套调度模型 |

---

## 19. 最终架构结论

Task Engine 是整个个人 AI 助手的业务调度核心。

它负责将用户提出的目标转化为可以持续执行、暂停、恢复和追踪的任务。

最终需要保持以下职责分工：

- Main Agent：决定做什么、如何与用户沟通。
- Task Engine：决定任务何时执行、处于什么状态。
- Agent Runtime：负责实际推理和工具执行。
- Task Scheduler：负责推进、等待、重试和程序重启后的恢复。
- Memory Service：保存可复用的长期信息。
- Document Service：保存任务过程和最终产物。

**关键原则：Task Engine 不应该知道模型内部如何思考，但必须准确知道每项业务工作处于什么状态、为什么处于该状态，以及下一步允许发生什么。**