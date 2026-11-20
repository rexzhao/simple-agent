# Agent Runtime 详细设计文档

**版本：** V1.2  
**文档状态：** 模块详细设计。数据模型、状态枚举、事件和 API 以 `04-integration-and-mvp-plan.md` 为准，冲突时以 04 为准。  
**更新日期：** 2026-11-19  
**所属系统：** Task-oriented Personal Agent  
**模块：** Agent Runtime  
**设计目标：** 为统一主 Agent 和任务 Agent 提供可持久化、可恢复、可观测的运行时执行环境。

---

## 1. 设计目标与边界

### 1.1 设计目标

Agent Runtime 负责管理 Agent 的完整执行过程，包括：

1. 接收主对话消息或任务执行请求。
2. 选择合适的 Agent 类型和模型。
3. 装配 Agent 所需上下文。
4. 调用 LLM 并解析模型输出。
5. 执行工具调用。
6. 处理任务进度、用户交互和执行异常。
7. 保存执行记录和 Checkpoint。
8. 在任务恢复时重新构建执行上下文。
9. 将执行结果返回 Main Agent 或 Task Engine。

### 1.2 非目标

Agent Runtime 不负责：

- 管理 Task 的业务生命周期。
- 决定任务之间的依赖关系。
- 直接管理用户界面。
- 自行决定长期记忆的最终写入策略。
- 自行维护文档的最终版本。
- 绕过权限系统直接执行外部操作。

这些能力分别由 Task Engine、Conversation Service、Memory Service、Document Service 和 Tool Gateway 提供。

### 1.3 核心原则

| 原则 | 说明 |
|---|---|
| Agent 无状态化 | Agent 实例不承担跨请求的长期状态存储 |
| 上下文显式装配 | 每次执行都明确指定上下文来源 |
| 执行过程可持久化 | 关键状态和事件必须落库 |
| 工具调用受控 | 所有工具调用经过 Tool Gateway |
| 用户交互统一 | Task Agent 不直接向用户发起交互 |
| 任务与模型会话解耦 | 不依赖特定模型供应商的 Session |
| 执行可恢复 | 程序重启后可以恢复任务执行 |
| 关键决策可追溯 | 重要决策及其依据单独记录 |

---

## 2. 总体架构

### 2.1 模块划分

Agent Runtime 分为以下模块：

| 模块 | 职责 |
|---|---|
| Agent Registry | Agent 类型和能力注册 |
| Agent Dispatcher | 根据请求选择 Agent |
| Agent Executor | 执行 Agent 主循环 |
| Context Builder | 构建模型上下文 |
| Prompt Manager | 管理系统提示词和模板 |
| Model Gateway | 统一调用不同模型 |
| Tool Gateway | 统一执行工具 |
| Interaction Client | 调用 Interaction Service 创建用户交互请求 |
| Output Parser | 解析模型响应中的文本和工具调用，校验参数 |
| Checkpoint Manager | 保存和恢复执行快照 |
| Execution Event Manager | 记录执行事件 |
| Token & Cost Manager | 统计 Token、模型成本及预算 |

### 2.2 逻辑架构

```text
                  User
                   |
                   v
           Conversation Service
                   |
                   v
               Main Agent
                   |
          +--------+---------+
          |                  |
          v                  v
    Agent Dispatcher    Task Engine
          |                  |
          +--------+---------+
                   |
                   v
              Agent Runtime
                   |
       +-----------+-----------+
       |           |           |
       v           v           v
 Context Builder  Model     Tool Gateway
       |          Gateway       |
       |           |            |
       v           v            v
  Memory/Docs     LLM        MCP/Tools
       |           |
       +-----------+
                   |
                   v
            Output Parser
                   |
       +-----------+-----------+
       |           |           |
       v           v           v
    Response    Tool Call   Interaction
       |           |           |
       v           v           v
    Main Agent  Tool Gateway  Task Engine
```

### 2.3 关键职责边界

**Main Agent**

- 面向用户理解需求。
- 判断是否创建、继续或修改任务。
- 决定调用哪个 Task。
- 汇总多个任务的结果。
- 统一向用户提问和反馈。

**Task Agent**

- 面向具体任务执行。
- 根据任务目标制定和调整执行计划。
- 调用工具完成任务。
- 生成阶段性结果。
- 提交用户交互请求。
- 输出任务总结、决策和产物更新建议。

**Agent Runtime**

- 为两类 Agent 提供统一的执行环境。
- 不直接决定业务任务的状态转换。
- 不允许 Task Agent 越权访问其他任务的上下文。

---

## 3. Agent 定义与注册机制

### 3.1 Agent 类型

第一阶段只实现两种 Agent：

| Agent | 用途 | 生命周期 |
|---|---|---|
| Main Agent | 统一用户入口和任务协调 | 随 Conversation 请求执行 |
| General Task Agent | 执行具体任务 | 随 Task Execution 执行 |

未来可以扩展：

- Research Agent：资料检索和研究。
- Coding Agent：代码开发和调试。
- Document Agent：文档生成和编辑。
- Reviewer Agent：质量检查和结果审查。
- Planner Agent：复杂任务拆解和计划制定。

初期不建议为每种任务单独部署一个 Agent 服务。Agent 类型应当是运行时注册的能力配置，而不是独立微服务。

### 3.2 Agent 配置模型

```go
type AgentDefinition struct {
    AgentType     string          `yaml:"agent_type"`
    Version       string          `yaml:"version"`
    Description   string          `yaml:"description"`
    PromptVersion string          `yaml:"prompt_version"`
    ModelPolicy   ModelPolicy     `yaml:"model_policy"`
    ContextPolicy ContextPolicy   `yaml:"context_policy"`
    BuiltinTools  []string        `yaml:"builtin_tools"`
    ToolPolicy    ToolPolicy      `yaml:"tool_policy"`
    Execution     ExecutionLimits `yaml:"execution"`
}

type ExecutionLimits struct {
    MaxIterations       int `yaml:"max_iterations"`
    MaxToolCalls        int `yaml:"max_tool_calls"`
    MaxExecutionSeconds int `yaml:"max_execution_seconds"`
}
```

示例（`internal/taskagent/agents/general_task.yaml`）：

```yaml
agent_type: general_task
version: "1.0"
prompt_version: "1.0"

model_policy:
  model: task              # 对应实例 config.yaml 中的 models.task
  fallback: fast

context_policy:
  include_user_profile: true
  include_task_summary: true
  include_task_decisions: true
  include_task_artifacts: true
  include_other_tasks: false

builtin_tools:             # 见 04 §3.6
  - report_progress
  - complete
  - fail
  - request_user_input
  - submit_document_update
  - create_artifact
  - propose_decision
  - retrieve_context

tool_policy:
  allowed_tools:           # 外部工具白名单，支持通配符
    - web_fetch
    - read
    - glob
    - grep
  max_permission_level: WRITE   # 超过此级别的工具不对该 Agent 暴露
  require_confirmation_for:
    - EXTERNAL_ACTION

execution:
  max_iterations: 30
  max_tool_calls: 50
  max_execution_seconds: 1800
```

配置中的模型使用别名（main / task / fast），由 Model Gateway 按实例的 `config.yaml` 映射到实际的供应商和模型，不直接绑定供应商 API。

### 3.3 Agent Registry 接口

```go
type Registry interface {
    // Get 返回指定版本；Execution 恢复时使用它固定的版本。
    Get(agentType, version string) (AgentDefinition, error)
    // Current 返回当前程序内置的版本，用于创建新的 Execution。
    Current(agentType string) (AgentDefinition, error)
    List() []AgentDefinition
}
```

每次 Execution 创建时固定 Agent Definition 版本，避免执行过程中配置变化造成行为不一致。

Agent Definition 和 Prompt 以 YAML / Markdown 文件形式嵌入可执行文件（`//go:embed`）。启动时把当前版本写入 `agent_definitions` 表；同一版本内容不可修改，修改配置必须升版本（04 §4.3）。程序升级后，旧 Execution 恢复时从表中读取它当时的定义。

## 4. Agent Execution 数据模型

Agent Runtime 最重要的设计是将 Agent、Session 和 Execution 区分开。

### 4.1 核心概念

| 对象 | 含义 |
|---|---|
| Agent | 执行角色和能力定义 |
| Task | 业务任务 |
| Task Session | 一段任务执行会话 |
| Execution | 一次实际运行过程 |
| Execution Step | 一次模型调用、工具调用或状态处理 |
| Checkpoint | 可恢复的执行快照 |

它们之间的关系：

```text
Task
 |
 +-- Task Session A
 |     |
 |     +-- Execution 1
 |     |      +-- Step 1
 |     |      +-- Step 2
 |     |      +-- Checkpoint 1
 |     |
 |     +-- Execution 2
 |
 +-- Task Session B
       |
       +-- Execution 3
```

Task Session 可以被重建，Execution 可以多次运行，但 Task 的身份和业务历史保持稳定。

### 4.2 Execution 状态

状态枚举和转换表以 04 §5.3 为准：

```text
PENDING
  |
  v
RUNNING <------------------+
  |                        |
  +----> WAITING_USER -----+  （收到回答后恢复）
  |                        |
  +----> PAUSED -----------+  （用户恢复）
  |
  +----> COMPLETED
  |
  +----> FAILED
  |
  +----> CANCELLED
```

说明：

- PENDING：已创建，等待执行。
- RUNNING：正在执行，工具调用在执行内同步完成（不设“等待工具”状态，工具过程记录在 `tool_calls`）。
- WAITING_USER：等待用户回答，恢复后继续同一个 Execution。
- PAUSED：被主动暂停，恢复后继续同一个 Execution。
- COMPLETED / FAILED / CANCELLED：终态。重试会创建新的 Execution，`retry_of_execution_id` 指向原执行。

Execution 状态与 Task 状态分开维护。

例如，某次 Execution 失败，不代表 Task 必须进入 FAILED。Task Engine 根据错误类型决定重试、重新规划或等待用户处理（映射规则见 04 §5.2）。

### 4.3 数据库设计

本模块拥有以下表，完整 DDL 见 04 §4.3：

| 表 | 用途 | 引入阶段 |
|---|---|---|
| agent_definitions | Agent 配置版本快照（嵌入程序的 YAML） | P1 |
| agent_executions | 一次 Execution：关联、版本、状态、预算、用量、取消/暂停标志、上下文快照 | P1 |
| agent_execution_steps | 执行步骤：CONTEXT_BUILD、LLM_CALL、TOOL_CALL、CHECKPOINT、ERROR | P1 |
| tool_calls | 工具调用记录，同时是审计日志和幂等记录（Tool Gateway 写入） | P1 |
| agent_checkpoints | 恢复快照 | P2 |

关键约束：

- `agent_executions.task_id` 为空表示 Main Agent 的执行，此时必须有 `conversation_id`。
- 每次 Execution 记录 `agent_version`、`prompt_version`、`model_id`，用于复盘（§6.2）。
- 模型原始请求和工具返回结果按敏感信息策略脱敏后保存；默认对凭据、密钥和敏感字段脱敏。

## 5. 上下文装配机制

### 5.1 上下文分层

Agent 的上下文不能简单地等同于 Conversation History。

建议拆分为五个层次：

| 层次 | 内容 | 是否默认加载 |
|---|---|---|
| System Context | Agent 角色、规则和约束 | 是 |
| User Context | 用户偏好和已确认信息 | 按需 |
| Conversation Context | 当前对话历史 | Main Agent 使用 |
| Task Context | 当前任务目标、计划、决策和摘要 | Task Agent 使用 |
| Execution Context | 当前运行状态和近期执行记录 | 是 |

Task Agent 不默认加载全部历史对话，也不加载其他 Task 的完整上下文。

### 5.2 Context Builder

```go
func (b *Builder) Build(ctx context.Context, exec Execution, agent AgentDefinition, cp *Checkpoint) (*AgentContext, error) {
    system, err := b.prompts.Render(agent.AgentType, agent.PromptVersion)
    if err != nil {
        return nil, err
    }

    user, err := b.loadUserContext(ctx, agent.ContextPolicy) // P3 起包含记忆
    if err != nil {
        return nil, err
    }

    var task *TaskContext
    if exec.TaskID != "" {
        if task, err = b.loadTaskContext(ctx, exec.TaskID, agent.ContextPolicy); err != nil {
            return nil, err
        }
    }

    history, err := b.loadExecutionHistory(ctx, exec.ID, cp) // 从 Checkpoint 和执行步骤重建
    if err != nil {
        return nil, err
    }

    c := &AgentContext{System: system, User: user, Task: task, History: history}
    b.budget.Fit(c, agent) // 按 04 §10.5 分配预算，超出时按 §5.5 压缩
    return c, nil
}
```

### 5.3 Task Context 结构

```json
{
  "task": {
    "id": "task-id",
    "title": "设计个人 Agent 系统",
    "goal": "完成可实现的技术架构",
    "status": "RUNNING"
  },
  "summary": {
    "content": "已完成总体架构设计，正在细化 Agent Runtime"
  },
  "plan": {
    "current_step": "设计执行协议",
    "remaining_steps": [
      "定义模型调用接口",
      "定义工具调用协议",
      "定义恢复机制"
    ]
  },
  "decisions": [
    {
      "decision": "Main Agent 与 Task Agent 分离",
      "reason": "统一用户交互，同时实现任务上下文隔离"
    }
  ],
  "artifacts": [
    {
      "artifact_id": "artifact-id",
      "name": "技术架构设计文档"
    }
  ]
}
```

实际加载时应采用 Token Budget 控制，避免把所有文档和决策完整塞入上下文。

### 5.4 Context Budget

Token 预算的分配比例以 04 §10.5 为准（先扣除输出预留，再按比例分配输入预算）。

预算比例是初始配置，不是固定约束，需要根据模型上下文窗口、任务类型和实际效果调整。

### 5.5 上下文压缩

当上下文接近预算上限时，按以下顺序处理：

1. 删除无关的历史工具输出。
2. 将已完成的执行步骤压缩为摘要。
3. 保留用户明确要求和已确认的决策。
4. 保留当前任务目标和未完成计划。
5. 将长文档转为检索引用，不直接全部注入。

禁止仅通过截断最早的消息来压缩上下文，因为这可能丢失用户决策和任务约束。

---

## 6. Prompt 管理

Prompt 不应该散落在业务代码中。

### 6.1 Prompt 组成

最终发送给模型的提示词由以下部分组合：

```text
[System Role]
Agent 身份和工作目标

[Global Rules]
系统级行为约束

[Agent Rules]
当前 Agent 的职责和能力

[User Context]
用户偏好和相关背景

[Task Context]
任务目标、计划和决策

[Execution Context]
当前执行状态

[Output Contract]
模型必须遵守的输出协议
```

### 6.2 Prompt 版本管理

每次 Execution 应记录：

- Agent Definition Version。
- System Prompt Version。
- Output Schema Version。
- Model Identifier。
- Tool Schema Version。

这样才能在未来复盘某次执行为什么产生特定结果。

Prompt 变更不应自动修改正在运行的 Execution 配置。

### 6.3 Main Agent System Prompt 核心约束

```text
你是用户唯一可见的个人 AI 助手。

你负责：
1. 理解用户意图。
2. 管理用户与任务之间的关系。
3. 决定何时创建或继续任务。
4. 统一处理用户需要参与的决策。
5. 汇总任务结果并向用户解释。

你不应：
1. 将内部 Agent 的存在暴露为复杂的技术概念。
2. 让用户分别与多个 Agent 交互。
3. 在缺乏依据时假设用户已经做出决定。
4. 将未确认的推断写入已确认的用户记忆。
5. 随意修改任务已有的关键决策。
```

### 6.4 Task Agent System Prompt 核心约束

```text
你是一个执行具体任务的内部 Agent。

你必须：
1. 围绕明确的 Task Goal 工作。
2. 遵守 Task Context 中的用户约束和已确认决策。
3. 通过 Tool Gateway 使用工具。
4. 将关键决策及其依据提交给系统记录。
5. 遇到必须由用户决定的问题时，提交 User Interaction Request。
6. 定期提交进度和可恢复的 Checkpoint。

你不得：
1. 直接向用户发送消息。
2. 擅自访问其他 Task 的完整上下文。
3. 将推测当作用户已确认的事实。
4. 绕过权限控制执行外部操作。
5. 将工具调用成功等同于业务任务完成。
```

---

## 7. 模型调用协议

### 7.1 Model Gateway

Model Gateway 包装仓库已有的 `internal/providers`（OpenAI 兼容协议、Anthropic、流式事件、工具声明），对 Agent Runtime 提供汇总后的一次调用结果：

```go
type ModelGateway interface {
    // Generate 发起一次流式调用并汇总结果；onDelta 把文本片段转发给界面（Main Agent 流式回复）。
    Generate(ctx context.Context, req GenerateRequest, onDelta func(text string)) (ModelResponse, error)
}

type GenerateRequest struct {
    ModelAlias string // main / task / fast，对应实例 config.yaml 的 models
    System     string
    Messages   []message.Message
    Tools      []providers.ToolSpec
    MaxTokens  int
}

type ModelResponse struct {
    Text         string
    ToolCalls    []ToolCall // 已合并、参数完整的工具调用
    FinishReason string
    Usage        Usage
    ProviderID   string
    ModelID      string
}
```

Model Gateway 负责：

- 模型别名解析和回退。
- 超时控制（复用 providers 的流空闲超时）。
- 重试策略（§13.2）。
- 汇总流式事件。必须遵守 `internal/providers` 的约定：同一 Index 的 `EventToolStart` 可能重复出现，要按 Index 合并；OpenAI 适配器不发 `EventToolEnd`，收到 `EventDone` 时要把所有缓冲中的调用视为结束。仓库 `internal/driver` 中已有同样的处理逻辑，优先抽取复用。
- Token 统计和费用计算。
- 错误标准化（§13.1）。

不允许业务代码直接构造供应商请求；供应商协议细节只存在于 `internal/providers`。

### 7.2 模型选择策略

模型选择采用能力和成本两层决策：

```text
Agent Definition
      |
      v
Task Complexity
      |
      v
Model Policy
      |
      v
Model Gateway
      |
      v
Selected Model
```

建议：

- 简单分类和意图识别使用低延迟模型。
- 普通任务执行使用通用模型。
- 复杂规划、推理和代码问题使用高能力模型。
- 工具调用失败或模型超时时，根据错误类型选择重试或降级。

模型降级不得改变任务目标和已确认决策。

---

## 8. Agent 执行循环

### 8.1 标准执行流程

```text
Receive Execution Request
          |
          v
Load Agent Definition (pinned version)
          |
          v
Load Latest Checkpoint (if any)
          |
          v
Build Context
          |
          v
Call Model  <-----------------------------+
          |                               |
          v                               |
Parse Response (text + tool calls)        |
          |                               |
     +----+-------------+                 |
     |                  |                 |
     v                  v                 |
 Text only         Tool calls             |
 (append,          (in order)             |
  count)                |                 |
     |        +---------+---------+       |
     |        |                   |       |
     |        v                   v       |
     |   Built-in tool       External tool|
     |   (app service)       (Tool Gateway)
     |        |                   |       |
     |        +---------+---------+       |
     |                  |                 |
     |       Append tool results          |
     |                  |                 |
     |       Terminal tool? --yes--> Return (COMPLETED / FAILED / WAITING_USER)
     |                  | no              |
     |       Save Checkpoint              |
     |                  |                 |
     +--------> Check cancel / pause / budget
                        |                 |
                        +-----------------+
```

### 8.2 输出协议

模型的结构化输出统一通过**原生 Tool Calling** 表达（ADR-SI-021），不另行定义自由文本协议。

原 V1.0 中的 output_type 与内置工具的对应关系：

| V1.0 output_type | V1.1 表达方式 |
|---|---|
| MESSAGE | 纯文本回复（Task Agent 视为中间说明，Main Agent 视为对用户的回复） |
| TOOL_CALL | 外部工具调用（经 Tool Gateway） |
| PROGRESS | 内置工具 `report_progress` |
| DECISION | 内置工具 `propose_decision` |
| USER_INTERACTION | 内置工具 `request_user_input` |
| ARTIFACT_UPDATE | 内置工具 `submit_document_update` / `create_artifact` |
| COMPLETION | 内置工具 `complete` |
| FAILURE | 内置工具 `fail` |

内置工具的完整清单、引入阶段和处理规则见 04 §3.6。

### 8.3 执行循环伪代码

```go
var terminalBuiltins = map[string]bool{"complete": true, "fail": true, "request_user_input": true}

func (r *runtime) Execute(ctx context.Context, id ID) (ExecutionResult, error) {
    exec, err := r.execs.Get(ctx, id)
    if err != nil {
        return ExecutionResult{}, err
    }
    if exec.Status.Terminal() {
        return resultOf(exec), nil // 以 executionID 幂等
    }

    agent, err := r.registry.Get(exec.AgentType, exec.AgentVersion)
    if err != nil {
        return ExecutionResult{}, err
    }
    cp, err := r.checkpoints.Latest(ctx, id) // 首次执行时为 nil
    if err != nil {
        return ExecutionResult{}, err
    }
    actx, err := r.contexts.Build(ctx, exec, agent, cp)
    if err != nil {
        return ExecutionResult{}, err
    }
    budget := newBudget(exec)
    if err := r.execs.MarkRunning(ctx, &exec); err != nil {
        return ExecutionResult{}, err
    }
    textOnly := 0

    for {
        // 1. 迭代边界：程序退出 / 取消 / 暂停 / 预算
        if ctx.Err() != nil {
            return r.interrupted(exec, ctx) // 用户取消 → CANCELLED；程序退出 → 保持 RUNNING
        }
        flags, err := r.execs.Flags(ctx, id)
        if err != nil {
            return ExecutionResult{}, err
        }
        if flags.Cancel {
            return r.finish(ctx, exec, ExecCancelled, nil)
        }
        if flags.Pause {
            r.checkpoints.Save(ctx, exec, actx, CheckpointPeriodic)
            return r.finish(ctx, exec, ExecPaused, nil)
        }
        if reason, over := budget.Exceeded(); over {
            return r.fail(ctx, exec, "EXECUTION_BUDGET_EXCEEDED", reason)
        }

        // 2. 调用模型（不在数据库事务中）
        resp, err := r.models.Generate(ctx, actx.Request(agent, r.tools.Specs(agent)), r.deltaSink(exec))
        r.steps.Record(ctx, exec, StepLLMCall, resp, err)
        if err != nil {
            return r.handleModelError(ctx, exec, err) // §13：可重试错误已在 Gateway 内重试
        }
        budget.AddUsage(resp.Usage)
        actx.AppendAssistant(resp)

        // 3. 纯文本
        if len(resp.ToolCalls) == 0 {
            if agent.IsMain() { // Main Agent：文本即最终回复
                return r.complete(ctx, exec, map[string]any{"message": resp.Text})
            }
            textOnly++
            if textOnly > maxTextOnly {
                return r.fail(ctx, exec, "OUTPUT_INVALID", "no tool call")
            }
            if textOnly == maxTextOnly {
                actx.AppendReminder("必须调用 complete、fail 或 request_user_input 之一")
            }
            continue
        }
        textOnly = 0

        // 4. 按顺序处理工具调用
        var terminal *ToolCallResult
        for _, call := range resp.ToolCalls {
            if terminal != nil { // 终止型工具之后的调用一律拒绝
                actx.AppendToolError(call, "rejected: after terminal tool")
                continue
            }
            if verr := validateArgs(agent, call); verr != nil {
                budget.AddRepair()
                actx.AppendToolError(call, verr.Error())
                continue
            }

            var res ToolCallResult
            if r.builtins.Has(call.Name) {
                res = r.builtins.Handle(ctx, exec, actx, call)
            } else {
                res = r.tools.Invoke(ctx, call.Request(exec), r.toolContext(exec))
                budget.AddToolCall()
            }
            actx.AppendToolResult(call, res)

            if terminalBuiltins[call.Name] || res.Status == ToolAwaitingConfirmation {
                terminal = &res
            }
        }

        // 5. 保存 Checkpoint，处理终止
        if terminal != nil {
            r.checkpoints.Save(ctx, exec, actx, checkpointTypeFor(terminal)) // BEFORE_WAIT / BEFORE_COMPLETE
            return r.finishByTerminal(ctx, exec, terminal)
            // complete → COMPLETED；fail → FAILED；
            // request_user_input / AWAITING_CONFIRMATION → WAITING_USER（返回 InteractionID）
        }
        r.checkpoints.Save(ctx, exec, actx, CheckpointAfterTool)
    }
}
```

说明：

- 每个工具调用（包括内置工具）的结果都回填到上下文，模型不会因为“没有收到反馈”而重复提交。
- `budget.AddRepair()` 超过修复次数上限（默认 2）时，`budget.Exceeded()` 返回 `OUTPUT_INVALID`。
- 模型调用和工具调用都不在数据库事务中进行；每一步的记录在独立的短事务中写入（04 §14.3）。
- 程序退出时 `ctx` 被取消，Execution 保持 RUNNING，下次启动由 Task Scheduler 从 Checkpoint 继续（04 §14.2）。
- 以上是逻辑示意，省略了部分错误处理。

### 8.4 输出协议校验

不能完全信任模型输出。

所有结构化输出都必须经过：

1. JSON Schema 校验。
2. 必填字段校验。
3. 枚举值校验。
4. 权限校验。
5. 业务状态校验。

校验失败时，可将错误反馈给模型进行有限次数的修复；超过次数后进入 FAILED 或人工处理流程。

---

## 9. 工具调用机制

### 9.1 工具调用链路

```text
Task Agent
    |
    v
Tool Call Request
    |
    v
Tool Gateway
    |
    v
Permission Check
    |
    v
Idempotency Check
    |
    v
Tool Execution
    |
    v
Tool Result
    |
    v
Agent Runtime
```

### 9.2 Tool Call 结构

```json
{
  "tool_call_id": "call-123",
  "tool_name": "document.create",
  "arguments": {
    "title": "技术设计文档",
    "content": "..."
  },
  "execution_id": "execution-id",
  "task_id": "task-id",
  "idempotency_key": "execution-id-call-123"
}
```

### 9.3 权限等级

| 等级 | 说明 | 默认策略 |
|---|---|---|
| READ | 读取信息 | 允许策略范围内执行 |
| WRITE | 修改内部数据 | 按工具策略授权 |
| EXECUTE | 执行代码或命令 | 严格沙箱和权限控制 |
| EXTERNAL_ACTION | 对外产生实际影响 | 默认要求用户确认 |

例如：

- 搜索公开网页：READ。
- 创建任务文档：WRITE。
- 运行 Python 脚本：EXECUTE。
- 发送邮件：EXTERNAL_ACTION。

工具执行必须有超时、日志和结果记录。

### 9.4 幂等性

每个可能产生副作用的工具调用必须提供 Idempotency Key。

如果同一调用因为程序重启而重试，系统必须能够识别此前是否已经执行成功，避免重复发送邮件、重复创建资源或重复执行其他外部操作。

对于无法确认执行结果的外部操作，不能简单地自动重试，应进入待核实状态（`tool_calls.status = UNKNOWN`，见 04 §6.9）。

EXTERNAL_ACTION 的确认流程：Tool Gateway 返回 `AWAITING_CONFIRMATION` 并创建 CONFIRMATION 类 Interaction，Execution 进入 WAITING_USER；用户批准后以同一个幂等键执行，拒绝则回填 DENIED（04 §6.9）。

---

## 10. 用户交互协议

这是整个 Agent Runtime 最重要的设计之一。

**Task Agent 可以发现问题，但只有 Main Agent 可以向用户发起交互。**

### 10.1 User Interaction Request

```json
{
  "interaction_id": "interaction-id",
  "task_id": "task-id",
  "execution_id": "execution-id",
  "question_type": "BLOCKING",
  "title": "需要确认技术选型",
  "question": "是否允许该任务使用外部在线服务？",
  "options": [
    {
      "id": "yes",
      "label": "允许"
    },
    {
      "id": "no",
      "label": "不允许"
    }
  ],
  "default_option": null,
  "reason": "当前方案需要访问外部 API",
  "deadline": null
}
```

### 10.2 问题优先级

| 类型 | 说明 | 处理方式 |
|---|---|---|
| BLOCKING | 没有答案无法继续 | 暂停当前执行 |
| CONFIRMATION | 需要用户批准操作 | 执行前等待确认 |
| IMPORTANT | 影响方案或质量 | 交由 Main Agent 决定何时询问 |
| OPTIONAL | 非必要偏好 | 允许使用合理默认值 |

### 10.3 交互流程

```text
Task Agent
    |
    v
request_user_input 内置工具
    |
    v
Interaction Service (PENDING)
    |
    v
Execution / Task Node / Task: WAITING_USER
    |
    v
Main Agent
    |
    v
User Conversation
    |
    v
User Answer
    |
    v
Main Agent (answer_interaction)
    |
    v
Interaction Service (ANSWERED)
    |
    v
Task Engine: PROCESS_USER_ANSWER 命令 -> Task Scheduler -> Resume
    |
    v
Task Agent
```

用户回答必须绑定原始 interaction_id，避免多个任务同时等待时回答错位。Interaction 的状态机见 04 §5.4。

### 10.4 Main Agent 的交互策略

Main Agent 收到问题后应判断：

- 是否必须立即询问？
- 是否可以根据用户已有偏好做出默认判断？
- 是否可以把多个相关问题合并？
- 是否只需要向用户确认最终方案，而不需要暴露内部技术细节？

例如，Task Agent 提交：

> 是否使用 SQLite 的 JSON 字段存储执行上下文？

Main Agent 可以转化为：

> 这个实现细节不会影响你的使用方式，我会采用 SQLite 的 JSON 字段作为默认实现。

而如果 Task Agent 提交：

> 是否允许向外部服务发送包含个人文件的内容？

Main Agent 必须向用户明确说明数据范围并等待授权。

---

## 11. Checkpoint 与恢复机制

### 11.1 为什么需要 Checkpoint

长任务可能运行数十分钟甚至数小时。

以下情况都可能中断执行：

- 程序重启。
- 模型 API 超时。
- 工具执行失败。
- 用户需要补充信息。
- 系统主动暂停。
- 执行预算耗尽。

因此不能依赖内存中的 Agent 对象保存任务状态。

### 11.2 Checkpoint 内容

```json
{
  "execution_id": "execution-id",
  "iteration": 8,
  "current_phase": "document_generation",
  "task_context_version": 12,
  "conversation_context_version": 4,
  "pending_tool_calls": [],
  "completed_steps": [
    "research",
    "analysis"
  ],
  "remaining_steps": [
    "generate_document",
    "review_document"
  ],
  "last_model_response_id": "response-id"
}
```

Checkpoint 保存的是恢复执行所需的状态和引用，不应将整个模型上下文无限制复制进去。

### 11.3 恢复流程

```text
Task Engine Resume
        |
        v
Load Execution
        |
        v
Load Latest Checkpoint
        |
        v
Load Task Context
        |
        v
Rebuild Context
        |
        v
Check Pending Tool Calls
        |
        v
Resume Agent Loop
```

恢复时需要重新读取当前 Task 的最新状态，并检查 Checkpoint 与 Task Context 的版本是否一致。

如果存在版本冲突，应优先执行上下文重建，而不是直接覆盖新的任务决策。

### 11.4 恢复原则

- 已完成的工具调用不重复执行。
- 未完成且无副作用的步骤可以重试。
- 具有外部副作用的步骤必须检查幂等记录。
- 已提交的用户决策不可被旧 Checkpoint 覆盖。
- 不依赖模型供应商的历史 Session 恢复。
- 不能恢复的执行进入可诊断的 FAILED 状态，并保留原始记录。

---

## 12. 事件系统

Agent Runtime 应输出标准化事件，供 Task Engine、Conversation Service 和前端订阅。

### 12.1 事件类型

Execution 事件的完整清单以 04 §7.2 为准：

execution.created、execution.started、execution.progress、execution.tool_started、execution.tool_completed、execution.waiting_user、execution.checkpoint_saved、execution.resumed、execution.completed、execution.failed、execution.cancelled

模型调用的明细记录在 `agent_execution_steps` 中，不单独发事件。

### 12.2 事件结构

所有事件使用 04 §7.1 的统一事件信封。示例：

```json
{
  "seq": 1842,
  "id": "event-id",
  "event_type": "execution.progress",
  "aggregate_type": "execution",
  "aggregate_id": "execution-id",
  "task_id": "task-id",
  "correlation_id": "request-id",
  "causation_id": null,
  "occurred_at": "2026-11-19T10:00:00Z",
  "payload": {
    "progress": 60,
    "message": "已完成核心模块设计"
  }
}
```

### 12.3 事件可靠性

重要事件不能只通过内存中的通道传递（04 §7.3）：

1. 事件与业务数据在同一个写事务中写入 `events` 表。
2. 事务提交后通知进程内事件总线。
3. 界面从自己读到的 `seq` 继续读取事件。
4. 持久化消费者按 `consumer_offsets` 续读，处理逻辑必须幂等。
5. `execution.progress` 之外的流式文本（`message.delta`）只走内存，不持久化。

## 13. 错误处理与重试

### 13.1 错误分类

| 错误类型 | 示例 | 处理 |
|---|---|---|
| MODEL_TIMEOUT | 模型超时 | 有限次数重试 |
| MODEL_RATE_LIMIT | 模型限流 | 指数退避 |
| MODEL_UNAVAILABLE | 模型服务不可用 | 尝试允许的备用模型 |
| TOOL_TIMEOUT | 工具超时 | 根据幂等性决定重试 |
| TOOL_PERMISSION_DENIED | 工具无权限 | 请求授权或终止 |
| OUTPUT_INVALID | 模型输出格式错误 | 有限次数修复 |
| CONTEXT_OVERFLOW | 上下文超限 | 压缩并重新构建 |
| EXECUTION_BUDGET_EXCEEDED | 执行预算耗尽 | 暂停或失败 |
| INTERNAL_ERROR | 内部异常 | 记录错误并进入恢复流程 |

### 13.2 重试策略

建议初始策略：

- 模型临时错误：最多重试 2 次。
- 模型限流：指数退避并加入随机抖动。
- 工具超时：先检查工具执行状态，再决定是否重试。
- 输出格式错误：最多进行 2 次修复。
- 权限错误：不自动重试。
- 用户取消：不重试。

重试次数和退避参数应配置化。

### 13.3 执行预算

每个 Execution 都需要独立预算：

```go
type ExecutionBudget struct {
    MaxIterations   int           `json:"max_iterations"`
    MaxToolCalls    int           `json:"max_tool_calls"`
    MaxTokens       int           `json:"max_tokens"`
    MaxCostMicros   int64         `json:"max_cost_micros"`
    MaxDuration     time.Duration `json:"max_duration"`
    MaxRepairs      int           `json:"max_repairs"` // 默认 2
}
```

预算耗尽后，不允许 Agent 无限循环。

MVP 的处理方式：Execution 以 `EXECUTION_BUDGET_EXCEEDED` 失败，Task Engine 创建 CONFIRMATION 类 Interaction 询问用户是否追加预算。批准则以 2 倍预算创建新的 Execution 重试该节点；拒绝则节点失败（见 05 M2-08）。

## 14. 并发与隔离

### 14.1 并发模型

同一个用户可以同时运行多个 Task：

```text
Main Agent
    |
    +---- Task A Agent Execution
    |
    +---- Task B Agent Execution
    |
    +---- Task C Agent Execution
```

不同 Task 的 Execution 可以并行运行。

### 14.2 并发约束

并发上限通过配置控制（04 §15.1）：

- 全局最大并发 Execution 数：`SCHEDULER_MAX_GLOBAL_EXECUTIONS`，默认 3。
- 每个 Task 最大并发 Execution 数：`SCHEDULER_MAX_EXECUTIONS_PER_TASK`，P1–P3 为 1，P4 起默认 2。
- 每个模型供应商的并发限制：由 Model Gateway 控制。
- 每个工具的并发限制：由 Tool Gateway 控制。

同一 Task 的多个 Execution 可以并行（P4 起），共享文档的写入由 Document Service 的版本校验串行化：写入冲突时 Agent 读取最新版本后重新提交（03 §11.3），而不是靠 Task 级的执行锁。

### 14.3 数据隔离

Task Agent 只获得当前 Task 的访问授权。

跨任务信息只能通过 Main Agent 或明确授权的 Context Retrieval 接口传递。

不要通过给所有 Agent 开放整个数据库查询权限来实现上下文共享。

---

## 15. API 设计

Agent Runtime 不提供 HTTP 接口。Task Scheduler 和 Main Agent 通过进程内 Go 接口调用 Agent Runtime（ADR-SI-024），接口定义见 04 §6.4：

| 能力 | 接口 |
|---|---|
| 运行 Execution | `Runtime.Execute(ctx, executionID)` |
| 恢复 Execution | `Runtime.Resume(ctx, executionID, interactionID)` |
| 请求暂停 | `Runtime.RequestPause(ctx, executionID)` |
| 请求取消 | `Runtime.RequestCancel(ctx, executionID)` |

Execution 的查询和执行记录通过 `TaskService.View` / `Events` 提供给界面。

## 16. 推荐技术实现

### 16.1 技术选型

| 领域 | 方案 | 说明 |
|---|---|---|
| 语言 | Go | 与 `simple-agent` 共用 module |
| Agent 执行循环 | 自研 | 见 04 §3.5 |
| 长任务执行 | 进程内 Task Scheduler + SQLite | 见 04 §14.2 |
| 数据库 | SQLite（`modernc.org/sqlite`，纯 Go） | 业务数据和执行记录 |
| 模型接入 | `internal/providers` | 复用 |
| 工具 | `internal/tools` + Tool Gateway；P4 起支持 MCP | 复用并增加权限、确认、幂等、审计 |
| 文件存储 | 实例目录内的 Blob Store | 文档和产物 |
| 界面通信 | 进程内事件总线 | 进度和流式回复 |

完整技术栈见 04 §14.1。

### 16.2 重要架构选择

各层职责不重叠（详见 04 §14.2）：

- Agent Runtime 管理单次 Execution 的执行循环、模型调用和工具调用。
- Task Scheduler 管理任务的推进、等待、重试和程序重启后的恢复，只依据数据库状态工作。
- Task Engine 管理业务 Task 状态。

单进程、单实例的前提下，不需要分布式调度框架或外部工作流服务。

### 16.3 推荐部署方式

单进程模块化单体，以独立的可执行文件 `task-agent.exe` 运行，每个实例打开一个目录。进程结构见 04 §2.3，实例目录见 04 §2.4。

逻辑模块独立（不同的 Go 包），但都在同一个进程内运行。

## 17. 测试方案

### 17.1 单元测试

- Agent Registry 版本选择。
- Context Builder 上下文装配。
- Output Parser 与工具参数校验。
- Model Gateway 错误映射。
- Tool Gateway 权限检查。
- Checkpoint 序列化和恢复。

### 17.2 集成测试

| 场景 | 预期结果 |
|---|---|
| Main Agent 创建任务 | 正确启动 Task Execution |
| Task Agent 调用工具 | 正确返回工具结果 |
| Task Agent 请求用户输入 | 暂停执行并通知 Main Agent |
| 用户回答问题 | 正确恢复对应 Execution |
| 模型调用超时 | 按策略重试 |
| 程序重启 | 从 Checkpoint 恢复 |
| 多 Task 并行 | 上下文互不污染 |
| 重复工具请求 | 不产生重复副作用 |
| Agent 配置更新 | 不影响已有 Execution |
| 用户取消任务 | 停止后续执行 |

### 17.3 关键验收标准

1. 所有 Execution 都有唯一 ID。
2. 每次模型调用和工具调用都可以追溯。
3. Task Agent 无法直接向用户发起交互。
4. 不同 Task 之间不存在隐式上下文共享。
5. 任务暂停后可以在程序重启后恢复。
6. 外部副作用工具具备幂等保护。
7. Agent 配置和 Prompt 版本可追溯。
8. 模型调用和工具调用均受预算约束。

---

## 18. 实施阶段

本模块的实施任务已合并到统一任务清单 `05-implementation-backlog.md`：

| 原阶段 | 对应任务 |
|---|---|
| 基础 Runtime | M1-01 Model Gateway、M1-02 Agent Registry、M1-05 Execution 数据模型、M1-07 Context Builder、M1-08 执行循环 |
| 工具和用户交互 | M1-06 Tool Gateway、M2-01 Interaction Service、M2-03 等待与恢复、M2-06 问题卡片 |
| 持久化和恢复 | M2-02 Checkpoint、M2-07 工具确认与幂等、M2-08 错误分类与重试 |
| 性能与扩展 | M4-03 并行执行、M5-06 专业 Agent、M5-07 上下文压缩 |

## 19. 本模块架构决策记录

| 编号 | 决策 | 原因 |
|---|---|---|
| ADR-AR-001 | Main Agent 与 Task Agent 分离 | 统一用户入口与任务执行解耦 |
| ADR-AR-002 | Agent Runtime 统一执行两类 Agent | 减少重复实现 |
| ADR-AR-003 | Agent 实例不承担长期状态 | 支持恢复和重建 |
| ADR-AR-004 | Task、Session、Execution 分离 | 避免依赖模型供应商 Session |
| ADR-AR-005 | Task Agent 不直接与用户交互 | 保持统一 Agent 体验 |
| ADR-AR-006 | 所有工具调用经过 Tool Gateway | 统一权限和审计 |
| ADR-AR-007 | 上下文显式装配 | 控制 Token、权限和上下文污染 |
| ADR-AR-008 | Checkpoint 持久化 | 支持长任务恢复 |
| ADR-AR-009 | （V1.2 修订）Agent 循环自研；长任务由进程内 Task Scheduler 基于 SQLite 持久化执行 | 单一事实来源，见 04 §3.5、§14.2 |
| ADR-AR-010 | （V1.1 修订）结构化输出采用原生 Tool Calling + 内置工具 | 降低模型自由文本带来的执行不确定性，见 04 §3.6 |

---

## 20. 最终结论

Agent Runtime 是整个个人 AI 助手的执行基础设施。

它不是简单的 LLM API 封装，也不是一个保存聊天历史的 Agent 对象，而是一个能够管理执行、上下文、工具、用户交互、状态和恢复过程的运行时系统。

最终架构应保持以下关系：

- Main Agent 负责用户交互和任务协调。
- Task Agent 负责具体任务执行。
- Agent Runtime 负责统一执行环境。
- Task Engine 负责任务生命周期。
- Memory Service 负责长期记忆。
- Document Service 负责文档和任务产物。
- Tool Gateway 负责工具权限和执行。

**最重要的工程原则：Task 是业务事实，Execution 是运行事实，Checkpoint 是恢复依据，Document 是可读的长期工作记录。**

四者不能互相替代。