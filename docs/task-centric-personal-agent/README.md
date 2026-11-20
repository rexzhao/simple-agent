# Task-centric Personal Agent 设计文档

以任务为核心的个人 AI 助手：用户只面对一个对话入口，系统在内部把工作组织成可持久化、可恢复、可追溯的任务，并沉淀决策、文档和长期记忆。

**产品形态：** Go 实现的独立桌面程序 `task-agent.exe`（与 `simple-agent` 同仓库，复用其模型接入和工具代码）。每个实例打开一个目录，数据全部保存在该目录的 `.task-agent/` 中；不同目录可以同时打开多个实例，同一目录同时只能有一个实例。

## 文档列表

| 文档 | 定位 | 什么时候读 |
|---|---|---|
| [00-architecture-overview.md](00-architecture-overview.md) | 架构概览：背景、产品定位、核心理念、领域概念 | 第一次了解项目 |
| [01-agent-runtime.md](01-agent-runtime.md) | Agent Runtime 详细设计：执行循环、上下文、工具、用户交互、Checkpoint | 实现 Agent 执行相关任务 |
| [02-task-engine.md](02-task-engine.md) | Task Engine 详细设计：状态机、拆解、依赖、调度、工作流、可靠性 | 实现任务管理与调度相关任务 |
| [03-memory-and-document.md](03-memory-and-document.md) | Memory & Document 详细设计：记忆提取与检索、决策、文档版本、知识库 | 实现记忆、决策、文档相关任务 |
| [04-integration-and-mvp-plan.md](04-integration-and-mvp-plan.md) | **集成规范（Normative）**：实例与目录、SQLite 数据模型 DDL、状态机、Go 模块接口、事件、界面操作、持久化执行（Task Scheduler）、技术栈、阶段划分、安全 | 开发时的规范依据 |
| [05-implementation-backlog.md](05-implementation-backlog.md) | **可执行任务清单**：前置决策、完成定义、按阶段的任务（依赖、规范、交付物、验收） | 安排和执行开发工作 |

## 规范优先级

1. **04 是规范基线。** 数据模型、状态枚举与转换、事件名称、API、阶段划分都以 04 为准。
2. 01–03 提供模块内部的设计细节和设计理由；与 04 冲突时以 04 为准。
3. 00 只保留愿景、原则和概念，具体规范一律指向 04。
4. 05 的每个任务都引用 04 或 01–03 的章节作为验收依据。实施中发现规范需要修改时，先改规范，再改任务。

章节引用格式：`04 §5.1` 表示 04 文档的第 5.1 节。

## 术语表

| 术语 | 含义 |
|---|---|
| 实例（Instance） | 一个目录 + 一个运行中的 `task-agent.exe` 进程，相当于原设计中的 Workspace；数据全部在目录的 `.task-agent/` 中 |
| 目录锁 | `.task-agent/instance.lock` 上的操作系统文件锁，保证同一目录只有一个实例 |
| Conversation / Message | 用户与助手的对话和消息 |
| Main Agent | 用户唯一可见的助手角色，负责理解意图、管理任务、统一提问和汇报 |
| Task Agent | 执行具体任务节点的内部 Agent，不直接与用户对话 |
| Task | 用户委托的一项完整工作，有独立的生命周期（04 §5.1） |
| Task Node | Task 内可调度、可重试的执行步骤；每个 Task 至少一个 |
| Cycle | Task 的执行周期；REOPEN 后加 1 |
| Task Session | 组织同一 Task 下连续执行过程的逻辑会话，不是模型供应商的会话 |
| Execution | Agent 的一次实际运行；重试会创建新的 Execution |
| Checkpoint | Execution 的恢复快照 |
| Interaction | Task Agent 发起、由 Main Agent 呈现给用户的问题或确认请求 |
| Decision | 带来源和选择依据的决策记录，只能被替代，不能被覆盖 |
| Memory | 可在未来任务中复用的长期信息，带作用域、来源和状态 |
| Document | 任务过程的可读记录（Markdown），版本不可变 |
| Artifact | 任务的交付产物（文件） |
| 内置工具（Built-in Tool） | Agent Runtime 在进程内处理的工具，用于进度、完成、提问、决策、文档等结构化输出（04 §3.6） |
| 事件日志（events） | 与业务数据同事务写入的事件表；界面和持久化消费者按 `seq` 续读 |
| Task Scheduler | 进程内的调度循环，根据数据库状态推进任务、等待、重试，并在程序重启后恢复（04 §14.2） |
| Blob Store | 实例目录中保存文档版本和产物的不可变文件存储 |

## 变更记录

### V1.2（2026-11-19）

改为 Go 实现的独立桌面程序，按目录多实例运行：

- **产品形态：** `task-agent.exe`，放在 `simple-agent` 仓库（`cmd/task-agent` + `internal/taskagent/...`），复用 `internal/providers`、`internal/tools`、`internal/message`、`internal/markdown`、`internal/store`（目录锁）等包；界面使用 mygo 原生窗口（ADR-SI-028、030）。
- **实例与目录：** 一个目录一个实例，数据全部在 `<目录>/.task-agent/`；同一目录由操作系统文件锁保证单实例；不同目录可同时运行；目录可整体复制迁移（04 §2.4，ADR-SI-029）。
- **存储：** PostgreSQL 改为 SQLite（`modernc.org/sqlite`，纯 Go，WAL，单写连接）；重写全部 DDL；去掉 `workspace_id`；`task_events` + `outbox_events` 合并为 `events`，`processed_events` 改为 `consumer_offsets`；pg_trgm 改为 FTS5 trigram；向量检索改为 Go 内存计算（04 §4、§7.3、§14.3）。
- **持久化执行：** Temporal 改为进程内的 Task Scheduler（基于数据库状态的协调循环），启动时恢复孤立的执行；任务只在实例运行时推进（04 §14.2）。
- **接口：** 取消 HTTP API 和 SSE，界面通过进程内 Go 应用服务和事件总线交互；接口定义改为 Go（04 §6、§12）。
- **配置与运行：** 每个实例一个 `.task-agent/config.yaml`（与 `simple-agent.yaml` 格式一致）；取消 Docker Compose；增加构建、备份和迁移说明（04 §15）。
- **工具：** 文件工具的工作区为实例目录，永远不能访问 `.task-agent/`；写用户文件默认逐次确认（04 §6.9）。
- **任务清单：** 05 按 Go 架构重写（61 个任务），`task_commands` 提前到 P1。
- **代码示例：** 01、02 中的 Python 示例改为 Go。

### V1.1（2026-11-19）

文档重命名并整理为“规范 + 详细设计 + 任务清单”结构，统一了各文档之间的冲突：

- **新增** `05-implementation-backlog.md`（可执行任务清单）和本 README。
- **文件重命名：** `design.md` → `00-architecture-overview.md`，`agent-runtime.md` → `01-agent-runtime.md`，`task-engine.md` → `02-task-engine.md`，`memory-and-document.md` → `03-memory-and-document.md`，`MVP-实施方案.md` → `04-integration-and-mvp-plan.md`。
- **标题层级统一：** 每个文档只有一个一级标题，章节使用二级标题。
- **架构决策变更（需要评审）：**
  - MVP 不引入 LangGraph，Agent 执行循环自研；Temporal 只负责编排（04 §3.5，ADR-SI-014）。
  - Agent 的结构化输出改为原生 Tool Calling + 内置工具，取消自定义的 output_type 协议（04 §3.6，ADR-SI-021）。
  - 每个 Task 至少一个 Task Node；依赖只存在于节点之间，只支持 FINISH_TO_START（04 §3.1）。
  - 终态 Task 只能通过用户显式的 REOPEN 进入新周期（04 §3.7）。
  - 取消 `/internal/*` HTTP 接口，模块之间使用进程内 Python 接口；对外 API 统一使用 `/api/v1`（ADR-SI-024）。
  - 文件存储是内部数据，用户通过 API 编辑；Markdown 工作区通过导出生成（04 §3.8）。
  - Temporal 从 Phase 1 接入（ADR-SI-026）。
  - 中文关键词检索使用 pg_trgm，pgvector 推迟到 Phase 5（ADR-SI-018）。
- **统一定义：**
  - 状态机：Task、Task Node、Execution（取消 WAITING_TOOL）、Interaction、Decision、Document、Memory（取消 CONFIRMED 状态，改由 source_type 表达）。
  - 表名与字段：`task_interactions` 统一为 `user_interaction_requests`；`tool_audit_logs` 统一为 `tool_calls`；补全了 V1.0 缺失的 DDL（personal_profiles、task_commands、document_operations、tool_calls、memory_suppressions、memory_relations、memory_embeddings、knowledge_items、processed_events）。
  - 事件命名、上下文分层与 Token 预算、决策字段（`decided_by` + `category`）、文档目录布局、并发配置。
  - 阶段划分：各文档各自的 Phase 规划合并为 04 §16 + 05。
- **新增内容：** Main Agent 多动作结果、Interaction Service 与 Tool Gateway 接口、外部操作确认流程、SSE 续传规则、评测集（04 §17.5）、提示词注入防护、代码执行沙箱和访问认证（04 §18.4–18.6）。
- **修正：** 00 正文与附录 ADR 编号不一致；02 中“不需要审核时”的矛盾描述；01 执行循环伪代码的问题（结果未回填、MESSAGE 未处理、变量覆盖）；数学公式改为 `$$` 语法以便 GitHub 渲染。

### V1.0

初始版本：架构设计、Agent Runtime、Task Engine、Memory & Document 三份详细设计，以及系统集成与 MVP 实施方案。
