# Personal Agent 实施任务清单（Implementation Backlog）

**版本：** V1.1  
**文档状态：** 可执行任务基线  
**更新日期：** 2026-11-19  
**上游规范：** `04-integration-and-mvp-plan.md`（以下简写为“04”），详细设计 01–03  
**产品形态：** Go 实现的独立可执行文件 `task-agent.exe`，按目录多实例运行（04 §2.4）

---

## 0. 使用说明

- 任务编号为 `M{阶段}-{序号}`，阶段定义见 04 §16。
- 每个任务包含：**依赖**、**规范**（验收依据的章节）、**内容**、**交付物**、**验收**。
- 规模估算（单人）：S ≤ 1 天，M 2–3 天，L 4–5 天。超过 L 的任务开工前必须继续拆分。
- 任务内容与规范冲突时，以规范为准，并同步修正本文。
- 实施中发现规范需要修改，先改 04（或对应的详细设计），再改任务。
- 建议把本文的任务逐条录入 Issue 跟踪系统，Issue 标题使用任务编号。

## 1. 开工前确认（前置决策）

以下事项需要在 M0 开工前确认。表中给出默认值；没有异议即按默认值执行。

| 编号 | 事项 | 默认值 | 影响任务 |
|---|---|---|---|
| D1 | 代码位置 | 放在 `simple-agent` 仓库、同一个 Go module：入口 `cmd/task-agent`，专用代码 `internal/taskagent/...`，复用 `internal/providers`、`internal/tools`、`internal/message`、`internal/markdown`、`internal/locale`、`internal/store`（目录锁） | 全部 |
| D2 | 程序名与数据目录名 | 可执行文件 `task-agent.exe`；实例数据目录 `.task-agent/` | M0-01、M0-02 |
| D3 | SQLite 驱动 | `modernc.org/sqlite`（纯 Go）。M0-03 必须验证 STRICT 表、`json_valid`、FTS5 trigram 分词器可用；trigram 不可用时退化为 `unicode61` 分词 + `LIKE` 兜底，并更新 04 §4.3 | M0-03、M3-06 |
| D4 | 不带参数启动 | 显示启动页：选择或输入目录（mygo 若提供原生目录对话框则使用）；P4 增加“最近打开的目录” | M0-02、M0-07 |
| D5 | 同一目录的第二个实例 | 提示“该目录已被另一个实例打开（PID x）”后退出；激活已有窗口放到 P5 | M0-02 |
| D6 | 配置格式 | `.task-agent/config.yaml`，供应商配置与 `simple-agent.yaml` 相同（04 §15.1） | M0-02、M1-01 |
| D7 | 修改共享包的原则 | 对 `internal/*` 共享包的修改必须保持 `simple-agent` 行为不变，并运行它的全部测试；需要较大改动时优先在 `internal/taskagent` 中包装 | 全部 |
| D8 | Token 计数 | 优先使用供应商返回的 usage；预算估算复用 `simple-agent` 已有的估算方法 | M1-07 |

## 2. 通用完成定义（DoD）

每个任务完成时必须满足：

1. `go vet ./...`、staticcheck、`go test ./...` 全部通过（包括 `simple-agent` 原有测试）。
2. 新增或修改的行为有自动化测试；涉及数据库的逻辑使用真实 SQLite（临时实例目录）测试，不用 mock。
3. 并发相关的包（`db`、`events`、`scheduler`、`runtime`）在支持的 CI 环境中通过 `go test -race`（race 检测需要 cgo，可在单独的 CI 任务中开启）。
4. 数据库变更以新的迁移文件提交（只增不改，04 §4.4 第 12 条）。
5. 状态、枚举、表结构与 04 §4、§5 一致；如有偏差，先更新规范。
6. 界面相关任务有 `ui.NewTester` 测试覆盖关键交互。
7. 日志和数据库中不出现密钥、Token 等敏感信息。

## 3. 关键路径

```text
M0-01 → M0-02 → M0-03 → M0-04 → M0-05 → M1-03 → M1-05 → M1-08 → M1-09 → M2-02 → M2-03 → M2-04 → M3-02 → M3-03 → M3-11
                                          ↘ M1-04 ↗        ↗
                              M1-01 ─────────────────────┘
```

Model Gateway（M1-01）、Tool Gateway（M1-06）、界面（M0-07、M1-11）可以与关键路径并行开发。

---

## 4. Phase 0：工程基础

| 编号 | 任务 | 依赖 | 规模 |
|---|---|---|---|
| M0-01 | 程序骨架 | D1、D2 | S |
| M0-02 | 实例目录、目录锁与配置 | M0-01、D4、D5、D6 | M |
| M0-03 | SQLite 接入与并发模型 | M0-02、D3 | M |
| M0-04 | 迁移器与基础表 | M0-03 | S |
| M0-05 | 事件日志、事件总线与持久化消费者 | M0-04 | M |
| M0-06 | 日志与脱敏 | M0-02 | S |
| M0-07 | 界面骨架 | M0-02 | M |
| M0-08 | Conversation Service 与对话界面 | M0-05、M0-07 | M |
| M0-09 | 测试工具与 CI | M0-03 | S |

### M0-01 程序骨架

- **依赖：** D1、D2
- **规范：** 04 §13、§15.2
- **内容：**
  - 新增 `cmd/task-agent/main.go` 和 `internal/taskagent/` 下的包结构（04 §13）。
  - 命令行：`task-agent [目录]`、`--help`、`--version`；版本号通过 `-ldflags` 注入。
  - 启动流程骨架：解析参数 → 打开实例（M0-02）→ 启动服务 → `mygo.App.Run`；退出流程骨架（04 §14.2「程序退出」）。
- **交付物：** `cmd/task-agent/`、`internal/taskagent/` 包骨架
- **验收：** `go build ./cmd/task-agent` 生成可执行文件；`--version` 输出版本；`simple-agent` 构建和测试不受影响。

### M0-02 实例目录、目录锁与配置

- **依赖：** M0-01、D4、D5、D6
- **规范：** 04 §2.4、§15.1、§18.6
- **内容：**
  - `instance.Open(dir)`：解析为绝对路径；不存在则创建；建立 `.task-agent/` 目录布局。
  - 目录锁：参数化 `internal/store` 的 `AcquireDirLock`（锁文件名、错误提示），锁文件为 `.task-agent/instance.lock`；`simple-agent` 继续使用原文件名。
  - 加锁成功后写 `instance.json`（PID、版本、启动时间）；加锁失败时读取 PID 并提示后退出。
  - 不使用 mygo 的全局单实例锁。
  - `config.yaml`：首次打开时生成带注释的默认配置；加载时复用 `internal/config` 的供应商配置结构；配置中直接写入 `api_key` 时记录告警。
  - 关闭：释放锁、关闭数据库（顺序见 04 §14.2）。
- **交付物：** `internal/taskagent/instance/`、`internal/store` 锁的参数化改动
- **验收：**
  - 测试中启动子进程：同一目录的第二个进程失败并得到持有者 PID；不同目录的两个进程都能成功。
  - 强制结束持锁进程后，可以立即再次打开同一目录。
  - 目录中只出现 `.task-agent/`（以及以后的 `exports/`），没有写到用户配置目录或程序目录。

### M0-03 SQLite 接入与并发模型

- **依赖：** M0-02、D3
- **规范：** 04 §4.3（连接参数）、§4.4、§14.3
- **内容：**
  - 打开 `.task-agent/agent.db`，每个连接执行 04 §4.3 的 PRAGMA。
  - 一个写连接 + 读连接池；`db.Write(ctx, func(tx *Tx) error)` 串行执行写事务（`BEGIN IMMEDIATE`），支持事务提交后回调（`tx.AfterCommit(fn)`）。
  - `ids`：UUIDv7 生成、固定宽度 UTC 时间格式。
  - 乐观锁辅助函数（影响 0 行返回 `ErrVersionConflict`）。
  - 能力自检：启动时验证 STRICT、`json_valid`、FTS5 trigram，不满足则拒绝启动并给出明确错误。
  - 定期和退出时执行 `wal_checkpoint(TRUNCATE)`。
- **交付物：** `internal/taskagent/db/`、`internal/taskagent/ids/`
- **验收：**
  - 50 个 goroutine 并发写入不出现 `SQLITE_BUSY`；写入期间读操作不被阻塞。
  - 事务回滚时不执行 AfterCommit 回调。
  - 能力自检测试通过（D3）。

### M0-04 迁移器与基础表

- **依赖：** M0-03
- **规范：** 04 §4.2（P0 表）、§4.3、§2.4（版本保护）、§15.3
- **内容：**
  - 迁移器：`go:embed` 的 `NNNN_name.sql`，按版本顺序执行，每个迁移一个事务，记录 `schema_migrations`。
  - 数据库版本高于程序支持的版本时拒绝打开。
  - 对已有数据的数据库执行迁移前，先用 `VACUUM INTO` 备份到 `.task-agent/backups/`。
  - 首个迁移：`schema_migrations`、`instance_meta`、`settings`、`conversations`、`messages`、`events`、`consumer_offsets`。
  - 首次初始化写入 `instance_meta`（instance_id、name、created_at、版本）。
- **验收：** 在空目录上初始化成功；重复打开不重复执行迁移；模拟“数据库版本更高”时拒绝打开；非法枚举值写入被 CHECK 拒绝。

### M0-05 事件日志、事件总线与持久化消费者

- **依赖：** M0-04
- **规范：** 04 §7.1、§7.3、§12.3
- **内容：**
  - `events.Append(tx, ...)`：在写事务中追加事件（04 §7.1 字段）。
  - 事件总线：`AfterCommit` 时发出通知；订阅通道带缓冲，满时合并通知；支持 `message.delta` 临时通知。
  - 持久化消费者框架：`consumer_offsets` 续读；只写数据库的处理器在同一事务中推进 `last_seq`；失败退避重试，连续失败 5 次跳过并写 `consumer.event_failed`。
  - 启动时所有消费者从 `last_seq` 继续。
- **交付物：** `internal/taskagent/events/`
- **验收：**
  - 提交后、通知前终止进程：重启后消费者补处理该事件。
  - 同一事件重复投递，只写数据库的处理器副作用只发生一次。
  - 订阅者处理慢时不丢失事件（以 `seq` 续读验证）。

### M0-06 日志与脱敏

- **依赖：** M0-02
- **规范：** 04 §19、§18.6
- **内容：** `log/slog` JSON 日志写入 `.task-agent/logs/`，按天滚动，保留 30 天；从 context 自动带上 correlation_id、task_id、execution_id；脱敏处理（API Key、Bearer Token、password 等模式）。
- **验收：** 脱敏测试覆盖常见密钥格式；日志文件按天滚动、过期删除。

### M0-07 界面骨架

- **依赖：** M0-02
- **规范：** 04 §12.1、02 §15、D4
- **内容：**
  - mygo 窗口：标题显示实例名和目录；左侧栏（会话 / 任务），主区域（对话）。
  - 无参数启动时的启动页（选择或输入目录）。
  - 后台调用与 `win.Update` 的封装；错误提示组件。
  - 参考并尽量复用 `internal/uiapp` 中的 composer、Markdown 渲染等组件，不依赖它的会话模型。
- **交付物：** `internal/taskagent/ui/`
- **验收：** `ui.NewTester` 测试：窗口渲染、侧栏切换、启动页选择目录后打开实例。

### M0-08 Conversation Service 与对话界面

- **依赖：** M0-05、M0-07
- **规范：** 04 §6.1、§12.2（P0 操作）、§4.3 messages
- **内容：** `ConversationService`；消息 `sequence` 在同一事务内分配；`message.created` 事件；对话界面（会话列表、消息列表、发送）；助手回复暂用占位实现（M1-10 替换）。
- **验收：** 并发发送 20 条消息，`sequence` 连续且不重复；界面测试覆盖发送和显示。

### M0-09 测试工具与 CI

- **依赖：** M0-03
- **规范：** 04 §17
- **内容：**
  - `testkit`：临时实例目录、可控时钟、子进程启动辅助（用于锁和崩溃测试）、故障注入钩子框架（`TASK_AGENT_FAULT=<point>` 时在指定位置退出进程）。
  - CI：构建两个可执行文件，运行 vet、staticcheck、全部测试；单独的 race 任务；`tests/eval` 默认不运行。
- **验收：** CI 在 PR 上自动运行并通过；构建产物包含 `task-agent.exe`。

---

## 5. Phase 1：单任务执行闭环

| 编号 | 任务 | 依赖 | 规模 |
|---|---|---|---|
| M1-01 | Model Gateway 与脚本化测试模型 | M0-02 | M |
| M1-02 | Agent Registry 与 Prompt 管理 | M0-04 | S |
| M1-03 | Task 数据模型与状态机 | M0-05 | M |
| M1-04 | TaskService 与命令表 | M1-03 | M |
| M1-05 | Execution 数据模型 | M1-03 | S |
| M1-06 | Tool Gateway v1 | M1-05 | M |
| M1-07 | Context Builder v1 | M1-02、M1-04、D8 | M |
| M1-08 | Agent 执行循环与内置工具 | M1-01、M1-06、M1-07 | L |
| M1-09 | Task Scheduler 基础版 | M1-04、M1-08 | L |
| M1-10 | Main Agent v1 | M0-08、M1-08、M1-04 | M |
| M1-11 | 对话流式显示与任务界面 | M1-09、M1-10 | M |
| M1-12 | Phase 1 端到端测试 | M1-09、M1-10、M1-11 | S |

### M1-01 Model Gateway 与脚本化测试模型

- **依赖：** M0-02
- **规范：** 01 §7、§13.1、§13.2
- **内容：**
  - `ModelGateway.Generate`：模型别名（main / task / fast）→ `config.yaml` 中的供应商和模型 → `internal/providers` 的 Provider。
  - 汇总流式事件：按 Index 合并工具调用；收到 `EventDone` 时结束所有缓冲中的调用（01 §7.1）。优先从 `internal/driver` 抽取已有实现。
  - 错误标准化为 01 §13.1 的分类；可重试错误按 01 §13.2 退避重试。
  - 用量与费用（`cost_micros`，价格表来自配置）。
  - `testkit.ScriptedProvider`：实现 Provider 接口，按脚本依次返回文本、工具调用、错误。
- **交付物：** `internal/taskagent/modelgw/`、`internal/taskagent/testkit/`
- **验收：** 用固定的 OpenAI 和 Anthropic 流式样本测试汇总（含重复 ToolStart、无 ToolEnd）；错误映射和重试测试；带 `live` 构建标签的真实供应商测试（默认不运行）。

### M1-02 Agent Registry 与 Prompt 管理

- **依赖：** M0-04
- **规范：** 01 §3、§6、04 §4.3 agent_definitions
- **内容：**
  - `agents/main.yaml`、`agents/general_task.yaml`（01 §3.2）；`prompts/{agent_type}/{version}.md`，核心约束见 01 §6.3、§6.4，并加入 04 §18.4 的不可信数据规则。
  - `go:embed` 嵌入；启动时写入 `agent_definitions`，同一版本内容哈希不一致时拒绝启动。
  - 迁移：`agent_definitions`。
- **验收：** 修改 YAML 不升版本时测试失败；`Get(type, version)` 能读取表中的历史版本。

### M1-03 Task 数据模型与状态机

- **依赖：** M0-05
- **规范：** 04 §4.3（tasks、task_sessions、task_nodes）、§5.1、§5.2、§7.2
- **内容：**
  - 迁移上述 3 张表。
  - 状态常量与转换表；`transition()` / `transitionNode()`：版本校验，在同一事务中追加 `task.status_changed` 和语义事件（02 §3.4）。
- **交付物：** `internal/taskagent/task/`
- **验收：** 表驱动测试覆盖 04 §5.1、§5.2 中每一个允许和不允许的转换；版本冲突返回 `ErrVersionConflict`；事务回滚后没有状态变化，也没有事件。

### M1-04 TaskService 与命令表

- **依赖：** M1-03
- **规范：** 04 §6.3、§4.3 task_commands、§12.2（P1 操作）
- **内容：**
  - `Create`（Task + Session + task.created）、`View`、`List`、`Events`。
  - 迁移 `task_commands`；`Cancel` 写入 CANCEL_TASK 命令（幂等键），由 Scheduler 处理（M1-09）。
- **验收：** 重复取消只产生一条命令；对终态任务取消返回 `ErrInvalidTransition`。

### M1-05 Execution 数据模型

- **依赖：** M1-03
- **规范：** 04 §4.3（agent_executions、agent_execution_steps、tool_calls、user_interaction_requests）、§5.3
- **内容：** 迁移 4 张表（`user_interaction_requests` 只建表，P2 启用）；Execution 状态机；执行和步骤的 repository；`pause_requested` / `cancel_requested` 读写。
- **验收：** 状态转换表驱动测试；步骤 `sequence` 唯一递增。

### M1-06 Tool Gateway v1

- **依赖：** M1-05
- **规范：** 04 §6.9、01 §9、04 §18.4 第 1 条
- **内容：**
  - 包装 `internal/tools`：`read`、`glob`、`grep` 的工作区为实例目录；`web_fetch`；`test_echo`。
  - 路径限制：除 `internal/tools` 已有的工作区校验外，额外拒绝任何解析后（含符号链接）位于 `.task-agent/` 内的路径。
  - 权限检查、幂等键（`{execution_id}:{model_tool_call_id}`）、超时、`tool_calls` 审计（参数和结果脱敏、过大结果存 Blob 引用）。
  - 外部内容包装为不可信数据块。
  - P1 中 EXTERNAL_ACTION 一律返回 DENIED。
- **验收：** 读取 `.task-agent/agent.db`、通过符号链接或 `..` 指向 `.task-agent/` 的请求都被拒绝；相同幂等键第二次调用不重复执行；超时返回 `TOOL_TIMEOUT`。

### M1-07 Context Builder v1

- **依赖：** M1-02、M1-04、D8
- **规范：** 04 §10、01 §5
- **内容：** 按 Agent 类型装配上下文层（P1：System、Conversation 或 Task、Execution；用户记忆层留空接口）；按 04 §10.5 分配 Token 预算，超出时按 01 §5.5 前两步压缩；保存 Context Snapshot（04 §10.4）。
- **验收：** 超出预算时按顺序裁剪，任务目标和用户约束始终保留；Snapshot 写入 Execution。

### M1-08 Agent 执行循环与内置工具

- **依赖：** M1-01、M1-06、M1-07
- **规范：** 01 §8.3、§8.4、§13.3、04 §3.6、§6.4
- **内容：**
  - `Runtime.Execute`：按 01 §8.3 实现；内置工具 `report_progress`、`complete`、`fail`；04 §3.6 的全部处理规则。
  - 执行预算；迭代边界检查标志位和 `ctx`；区分“用户取消”和“程序退出”（后者保持 RUNNING）。
  - 每一步写入 `agent_execution_steps`（短事务，模型调用不在事务中）。
  - 以 `executionID` 幂等。
- **交付物：** `internal/taskagent/runtime/`
- **验收（全部使用 ScriptedProvider）：**
  - 工具调用 → `complete`：COMPLETED，输出被保存。
  - 参数非法：2 次以内修复成功；超过则 FAILED（`OUTPUT_INVALID`）。
  - 连续 3 次纯文本：先提醒，再违反则 FAILED。
  - 超过最大迭代次数：FAILED（`EXECUTION_BUDGET_EXCEEDED`）。
  - `complete` 之后的同批工具调用被拒绝。
  - 设置 `cancel_requested` 后变为 CANCELLED；取消 `ctx`（模拟程序退出）后保持 RUNNING。

### M1-09 Task Scheduler 基础版

- **依赖：** M1-04、M1-08
- **规范：** 04 §14.2、02 §11、04 §5.2（Execution 结果映射）
- **内容：**
  - 调度循环（02 §11.3）：唤醒来源、命令处理（CANCEL_TASK）、结果处理、`reconcile`。
  - 规划：P1 生成单节点计划（CREATED → PLANNING → READY）。
  - 启动节点：一个事务创建 Execution、节点 → RUNNING、Task → RUNNING，提交后启动 Runner。
  - 结果映射：完成 → REVIEWING → COMPLETED；可重试失败按 `next_retry_at` 重试；不可重试失败 → Task FAILED。
  - 启动对账：重启孤立的 Execution；关闭流程（04 §14.2）。
  - 并发上限（`config.yaml` 的 scheduler 配置）。
- **交付物：** `internal/taskagent/scheduler/`
- **验收：**
  - 正常路径：Task COMPLETED，事件完整。
  - 可重试失败后第二次成功；不可重试失败时 Task FAILED。
  - 运行中取消：Task 和 Execution 都变为 CANCELLED。
  - 执行中关闭实例并重新打开（同一数据库上创建新的 Scheduler）：任务继续并完成，Execution 记录不重复（P1 没有 Checkpoint，从节点开头重新执行可以接受，因为只有 READ 工具）。

### M1-10 Main Agent v1

- **依赖：** M0-08、M1-08、M1-04
- **规范：** 04 §6.2、§9.2、§3.6（Main Agent 内置工具 P1 部分）、§8.2
- **内容：**
  - 持久化消费者：用户的 `message.created` → 启动一次 Main Agent 回合（以 message_id 幂等），替换 M0-08 的占位回复。
  - 内置工具：`query_tasks`、`create_task`、`cancel_task`。
  - 流式回复：文本片段作为 `message.delta` 推送；结束时保存助手消息、写 `message.completed`，`MainAgentResult` 写入消息 metadata。
  - 启动时把 STREAMING 消息改为 INTERRUPTED，对应执行标记为中断。
  - 消费 `task.completed` / `task.failed`：生成结果汇报发送到原会话。
  - 建立 `tests/eval/intent_actions.jsonl`（04 §17.5）。
- **验收：** 场景 A 不创建 Task；场景 B 创建 Task，完成后会话中出现汇报；评测集准确率 ≥ 90%（手动运行）。

### M1-11 对话流式显示与任务界面

- **依赖：** M1-09、M1-10
- **规范：** 04 §12、02 §15
- **内容：** 流式显示助手回复；中断消息的“重试”；任务列表（状态实时更新）；任务详情（节点、事件时间线、执行步骤和工具调用）；取消按钮；关闭窗口时如有运行中任务则提示（04 §2.4）。
- **验收：** `ui.NewTester` 测试覆盖流式显示、任务状态更新、取消。

### M1-12 Phase 1 端到端测试

- **依赖：** M1-09、M1-10、M1-11
- **规范：** 04 §17.3 场景 A、B（不含文档部分）、§16.4
- **内容：** 在临时实例目录上启动完整服务（ScriptedProvider），通过应用服务驱动场景 A、B；加一个界面冒烟测试。
- **验收：** 04 §16.4 的阶段验收全部满足。

---

## 6. Phase 2：任务等待与恢复

| 编号 | 任务 | 依赖 | 规模 |
|---|---|---|---|
| M2-01 | Interaction Service | M1-05 | M |
| M2-02 | Checkpoint Manager | M1-08 | M |
| M2-03 | request_user_input 与 Execution 等待/恢复 | M2-01、M2-02 | M |
| M2-04 | 调度器命令：暂停、恢复、处理回答 | M2-03、M1-09 | M |
| M2-05 | 暂停、恢复、取消的完整流程与界面 | M2-04 | M |
| M2-06 | Main Agent 交互处理与问题卡片 | M2-01、M1-10 | M |
| M2-07 | 工具确认、幂等与用户文件写入 | M2-03、M1-06 | M |
| M2-08 | 错误分类、节点重试与预算处理 | M2-04 | S |
| M2-09 | 启动对账完善 | M2-04 | S |
| M2-10 | Phase 2 端到端与故障注入测试 | M2-05 ~ M2-09 | M |

### M2-01 Interaction Service

- **依赖：** M1-05
- **规范：** 04 §5.4、§6.8、§4.3 user_interaction_requests
- **内容：** 状态机；`InteractionService` 全部方法；事件；过期处理（`expires_at`，由 Scheduler 定时触发）。
- **验收：** 状态转换表驱动测试；对非 PRESENTED 的问题回答返回 `ErrInvalidTransition`；重复回答不会产生第二条命令。

### M2-02 Checkpoint Manager

- **依赖：** M1-08
- **规范：** 01 §11、04 §4.3 agent_checkpoints、§3.5
- **内容：** 迁移；在 `AFTER_TOOL`、`BEFORE_WAIT`、`BEFORE_COMPLETE`、`PERIODIC` 时保存；`Execute` 有 Checkpoint 时从最新的继续；恢复时重新读取 Task Context 并处理版本不一致（01 §11.3）。
- **验收：** 执行循环中途取消 ctx（模拟程序退出）后再次 `Execute`，已完成的工具调用不再执行（检查 `tool_calls` 记录数）。

### M2-03 request_user_input 与 Execution 等待/恢复

- **依赖：** M2-01、M2-02
- **规范：** 04 §3.6、§8.5、§6.4（Resume）、§5.2 映射
- **内容：** `request_user_input` 内置工具；Execution → WAITING_USER，节点和 Task 联动；`Runtime.Resume` 把回答作为 tool result 注入。
- **验收：** ScriptedProvider 脚本：提问 → 等待 → 回答 → 继续 → 完成；后续步骤遵守回答内容。

### M2-04 调度器命令：暂停、恢复、处理回答

- **依赖：** M2-03、M1-09
- **规范：** 02 §11.3、§11.4、04 §14.2
- **内容：** Scheduler 处理 PAUSE_TASK、RESUME_TASK、PROCESS_USER_ANSWER；回答 → 命令 → `Resume` → Interaction CONSUMED；所有命令按幂等键去重；PROCESSING 命令在启动时重置为 PENDING。
- **验收：** 同一回答重复提交不会产生两次恢复；等待用户期间关闭实例，重新打开后回答，任务正常恢复。

### M2-05 暂停、恢复、取消的完整流程与界面

- **依赖：** M2-04
- **规范：** 02 §9.1、§9.2、04 §6.3、§12.2
- **内容：** 协作式暂停（标志 → 迭代边界 → Checkpoint → PAUSED）；处于 WAITING_USER 的任务也可以暂停；取消时级联取消 Interaction 和 Execution；界面按钮和状态显示。
- **验收：** 暂停后不再启动新节点；暂停后关闭并重新打开实例，恢复后继续执行；取消后任务数据仍保留。

### M2-06 Main Agent 交互处理与问题卡片

- **依赖：** M2-01、M1-10
- **规范：** 04 §9.3、§9.4、02 §8.3、§8.4、01 §10.4、04 §18.4 第 5 条
- **内容：**
  - 待处理问题进入 Main Agent 上下文；展示后 `MarkPresented`。
  - 内置工具：`answer_interaction`、`pause_task`、`resume_task`。
  - 无法唯一确定回答对应的问题时进行澄清，不绑定。
  - 界面：问题卡片（选项、文本输入、批准 / 拒绝），CONFIRMATION 类卡片展示真实参数。
  - 建立 `interaction_binding.jsonl` 评测集。
- **验收：** 两个任务同时等待时，自然语言回答和卡片回答都绑定正确；有歧义的回答触发澄清；评测集中没有错误绑定。

### M2-07 工具确认、幂等与用户文件写入

- **依赖：** M2-03、M1-06
- **规范：** 04 §6.9、01 §9.4、04 §18.4 第 2、3 条、§15.1（`tools.file_write`）
- **内容：** `AWAITING_CONFIRMATION` 流程；批准后使用同一幂等键执行；拒绝回填 DENIED；中断的副作用调用记为 UNKNOWN；`write`、`edit` 工具（按 `file_write` 配置决定确认方式）；`test_external_action`；不可信内容出现后提升确认级别。
- **验收：** 批准后只执行一次；工具执行成功但结果保存前退出进程，恢复后不重复执行；`.task-agent/` 内的路径始终不可写。

### M2-08 错误分类、节点重试与预算处理

- **依赖：** M2-04
- **规范：** 01 §13、02 §9.3、04 §5.2
- **内容：** 按错误分类决定是否可重试；节点重试退避（`next_retry_at`，Scheduler 定时）；预算耗尽时创建 CONFIRMATION 类 Interaction 询问是否追加预算，批准则以 2 倍预算创建新 Execution，拒绝则节点 FAILED。
- **验收：** 每种错误分类都有处理测试；预算耗尽的两种分支都覆盖。

### M2-09 启动对账完善

- **依赖：** M2-04
- **规范：** 04 §14.2「启动对账」、02 §12.4
- **内容：** 覆盖 04 §14.2 列出的全部启动对账项；对账发现的不一致写入日志和告警事件。
- **验收：** 人为制造每一种不一致后，打开实例时都能修复。

### M2-10 Phase 2 端到端与故障注入测试

- **依赖：** M2-05 至 M2-09
- **规范：** 04 §17.3 场景 C、§17.4、§16.5
- **内容：** 场景 C 端到端测试；使用 `TASK_AGENT_FAULT` 钩子在子进程中覆盖 04 §17.4 的全部故障点和多实例场景；建立 `prompt_injection.jsonl` 评测集。
- **验收：** 04 §16.5 的阶段验收全部满足。

---

## 7. Phase 3：文档、决策与记忆（MVP 完成）

| 编号 | 任务 | 依赖 | 规模 |
|---|---|---|---|
| M3-01 | Blob Store | M0-02 | S |
| M3-02 | Document Service 核心 | M3-01、M1-03 | L |
| M3-03 | 任务文档模板与事件驱动更新 | M3-02、M0-05 | M |
| M3-04 | Artifact 服务与文档类内置工具 | M3-02、M1-08 | M |
| M3-05 | Decision Service | M3-02、M2-01 | M |
| M3-06 | Memory 数据模型与服务 | M1-03 | M |
| M3-07 | Memory 提取 v1 | M3-06、M1-01 | M |
| M3-08 | 记忆检索与上下文集成 | M3-06、M1-07 | M |
| M3-09 | 文档、决策与记忆界面 | M3-02、M3-05、M3-06、M1-11 | M |
| M3-10 | 导出与备份 | M3-02、M3-06 | S |
| M3-11 | Phase 3 端到端测试与 MVP 验收 | M3-03 ~ M3-10 | M |

### M3-01 Blob Store

- **依赖：** M0-02
- **规范：** 04 §14.4、§2.4
- **内容：** `blob.Store`（Put / Get / Delete / Exists）；临时文件 → fsync → 重命名；SHA-256；写后不可修改；孤儿文件清理任务。
- **验收：** 重复写入同一 key 被拒绝；写入中途退出不留下半个文件；清理任务只删除没有元数据引用的文件。

### M3-02 Document Service 核心

- **依赖：** M3-01、M1-03
- **规范：** 03 §10、§11、04 §4.3（documents、document_versions、document_operations）、§5.6、§6.7
- **内容：**
  - 迁移 3 张表。
  - `Create`、`Current`、`Version`、`Versions`、`Apply`（APPEND / 按 `## ` 章节定位的 PATCH / REPLACE）、`UserEdit`、`Rollback`、`Diff`（Myers diff，unified 格式）。
  - 按 `write_policy` 限制允许的操作；`base_version` 冲突返回 `ErrVersionConflict`；按 `idempotency_key` 幂等。
  - 用户编辑优先（03 §11.6）。
- **验收：** 两个基于同一版本的操作只有一个成功；APPEND_ONLY 文档拒绝 PATCH；用户编辑后，Agent 基于旧版本的写入被拒绝；回滚生成新版本。

### M3-03 任务文档模板与事件驱动更新

- **依赖：** M3-02、M0-05
- **规范：** 03 §9、§12、04 §11
- **内容：** 持久化消费者：`task.created` 时创建 5 份文档；RENDERED 文档（progress.md、decisions.md）在相关事件时重新渲染；summary.md 在 `task.waiting_user`、`task.paused`、`task.completed`、`task.failed` 时由模型生成结构化 PATCH；Session 文档追加。
- **验收：** 同一事件重放不产生重复内容；文档处理失败不影响 Task 状态，之后自动补齐（04 §8.7）。

### M3-04 Artifact 服务与文档类内置工具

- **依赖：** M3-02、M1-08
- **规范：** 04 §4.3 task_artifacts、§3.6、§12.2
- **内容：** 迁移；内置工具 `create_artifact`、`submit_document_update`；产物版本；界面“另存为”（把产物复制到用户选择的位置，默认实例目录）。
- **验收：** 同名产物生成新版本而不覆盖；`submit_document_update` 冲突时向 Agent 回填最新版本号。

### M3-05 Decision Service

- **依赖：** M3-02、M2-01
- **规范：** 03 §7、04 §4.3 task_decisions、§5.5、§6.6
- **内容：** 迁移；`Propose`、`Confirm`、`Reject`、`Supersede`；`propose_decision` 内置工具；需要确认时创建 Interaction（03 §7.4）；已确认决策进入 Task Context；decisions.md 由数据渲染；建立 `decision_extraction.jsonl` 评测集。
- **验收：** 没有来源的 USER 决策被数据库拒绝；替代后历史仍可查询；候选决策不会作为已确认决策注入上下文。

### M3-06 Memory 数据模型与服务

- **依赖：** M1-03
- **规范：** 03 §2、§5、§16、04 §4.3（memories、memories_fts、memory_sources、memory_suppressions）、§5.7、§6.5
- **内容：** 迁移（含 FTS5 表和同步触发器）；状态机；`MemoryService` 的增删改查、确认、拒绝；删除流程（清空内容、删除来源、写入抑制规则）。
- **验收：** 删除后内容在表和 FTS 索引中都查不到；抑制规则只保存哈希；FTS 与主表在增删改后保持一致。

### M3-07 Memory 提取 v1

- **依赖：** M3-06、M1-01
- **规范：** 03 §3、§2.6、04 §18.4 第 4 条
- **内容：** 持久化消费者处理用户消息：规则预筛（“记住”“以后”“我喜欢”等表达）→ 命中后用 fast 模型提取（03 §3.3）→ 校验（03 §3.4）→ 按 `content_hash` 去重 → 检查抑制规则；EXPLICIT → ACTIVE，INFERRED / OBSERVED → CANDIDATE；任务完成时提取经验进入 CANDIDATE；建立 `memory_extraction.jsonl` 评测集。
- **验收：** 临时任务约束（如“这次用 SQLite”）不会成为 USER_GLOBAL 记忆；删除后同一来源不会被重新提取；评测集精确率 ≥ 90%。

### M3-08 记忆检索与上下文集成

- **依赖：** M3-06、M1-07
- **规范：** 03 §6、§14、04 §10
- **内容：** 硬过滤（03 §6.3）+ FTS5 检索（短查询退化为 LIKE）+ 03 §6.4 评分（语义分为 0）；`retrieve_context` 内置工具；Context Builder 的用户记忆层，带来源标记（03 §14.3）；Main Agent 在合适时机请用户确认 CANDIDATE。
- **验收：** Task Agent 读取不到其他 Task 作用域的记忆；中文 1、2、3 个字符以上的查询都能命中；注入的每条记忆都带 ID 和来源类型。

### M3-09 文档、决策与记忆界面

- **依赖：** M3-02、M3-05、M3-06、M1-11
- **规范：** 04 §12.2（P3 操作）
- **内容：** 文档查看与编辑（冲突提示）、版本历史与对比、回滚；决策列表与确认；记忆管理（查看、搜索、修改、删除、确认候选）。
- **验收：** `ui.NewTester` 测试覆盖上述主要操作。

### M3-10 导出与备份

- **依赖：** M3-02、M3-06
- **规范：** 04 §14.4、§15.3、§18.3、03 §17.2
- **内容：** 导出到 `<实例目录>/exports/{时间戳}/`（Markdown 工作区 + `data.json`，不含配置和日志）；运行中备份（`VACUUM INTO` + 打包 `blobs/`）；从备份恢复的说明。
- **验收：** 导出后可以离线阅读全部任务文档；关闭实例、把目录复制到另一位置后打开，数据完整且任务可以继续。

### M3-11 Phase 3 端到端测试与 MVP 验收

- **依赖：** M3-03 至 M3-10
- **规范：** 04 §17.3 场景 B（完整）、§16.6、§21
- **内容：** 场景 B 包含文档和产物的完整端到端测试；把 04 §21 的验收标准逐条写成测试或手工检查项。
- **验收：** 04 §21 全部满足，MVP 完成。

---

## 8. Phase 4：多任务和复杂调度

| 编号 | 任务 | 依赖 | 规模 |
|---|---|---|---|
| M4-01 | Task Planner 与计划校验 | MVP | L |
| M4-02 | 调度策略与并发控制 | M4-01 | M |
| M4-03 | 并行节点执行 | M4-02 | M |
| M4-04 | 任务路由与指令追加 | MVP | L |
| M4-05 | 多问题合并呈现 | MVP | S |
| M4-06 | 任务进度与任务面板 | M4-01 | M |
| M4-07 | MCP 适配器 | MVP | M |
| M4-08 | 最近打开的目录 | MVP | S |

- **M4-01 Task Planner 与计划校验**（02 §4、§5，04 §4.3 task_dependencies）：Planner 提示词；计划结构；校验（节点 key、依赖、无环、完成标准、节点数量 3–15）；迁移 `task_dependencies`；写入 plan.md；校验失败时重新规划或向用户澄清。
- **M4-02 调度策略与并发控制**（02 §6）：就绪节点选择；优先级、FIFO、公平性；`max_executions_per_task` 默认改为 2。
- **M4-03 并行节点执行**（02 §11）：多个 Runner 并发；文档写冲突时 Agent 读取最新版本后重新提交；非必需节点失败不影响其他节点。
- **M4-04 任务路由与指令追加**（04 §9.3、§3.6 `update_task`）：显式引用 → 当前对话中的任务 → 待处理问题 → 近期任务 → FTS 检索；置信度控制和澄清；APPEND_INSTRUCTION 命令在迭代边界注入；建立 `task_routing.jsonl` 评测集。
- **M4-05 多问题合并呈现**（02 §8.3）：合并非紧急问题，CONFIRMATION 类问题不合并。
- **M4-06 任务进度与任务面板**（02 §14、§15）：加权进度；任务面板界面。
- **M4-07 MCP 适配器**（01 §9）：MCP 客户端；MCP 工具映射为 `ToolDefinition`；按 MCP Server 配置权限级别，默认 EXTERNAL_ACTION；配置写在实例的 `config.yaml`。
- **M4-08 最近打开的目录**（04 §2.4）：启动页显示最近打开的目录（只保存路径，存于用户配置目录）。

**阶段验收：** 04 §16.7。

---

## 9. Phase 5：完整个人助手

P5 任务开工前需要先补充详细设计（以设计文档或本文中的子任务形式），再拆分为 M 或 L 规模的任务。

| 编号 | 任务 | 规范 / 需要补充的设计 |
|---|---|---|
| M5-01 | 语义检索与混合检索 | 03 §6、§17.3；确定 Embedding 模型和维度，向量在 Go 中计算 |
| M5-02 | 记忆去重与冲突处理 | 03 §3.5、§4 |
| M5-03 | Knowledge Base | 03 §13、04 §4.3 knowledge_items |
| M5-04 | Reopen | 04 §3.7 |
| M5-05 | Projects 与 PROJECT 作用域 | 需要新增 projects 数据模型设计 |
| M5-06 | Reviewer / Research Agent | 01 §3.1；REVIEWING 阶段的审核流程设计 |
| M5-07 | 上下文压缩与 Session 轮换 | 00 §6.3、01 §5.5；参考 `simple-agent` 的压缩实现（`docs/persistence-design.md`） |
| M5-08 | EXECUTE 沙箱 | 04 §18.5；评估复用 `internal/tools` 的 shell 与沙箱实现 |
| M5-09 | 成本分析与预算 | 需要设计任务级和月度预算 |
| M5-10 | 主动提醒与定时任务 | 需要单独设计；注意任务只在实例运行时推进 |
| M5-11 | 激活已打开实例的窗口 | 04 §2.4；本机 IPC（命名管道 / Unix socket）设计 |

**阶段验收：** 04 §16.8。
