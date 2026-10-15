# sai 最小 CLI：重构设计基准

> 状态：破坏性重构前的设计定稿。实现与本文冲突时，先改本文再改代码。
> 旧日志格式不做兼容迁移。

## 1. 目标与非目标

**目标**：最小版本的本地 CLI agent——单二进制、单工具（shell）、会话持久化为
append-only JSONL。为将来 WebUI 预留统一的 item 流模型，但不为它写任何代码。

**非目标（v0 立项时）**：List/会话选择器、旧日志兼容、并行工具执行、非输入触发的
run。（前两条后来 overturned：多会话 serve 已落地 ListSessions/SessionInfo 与
web.Manager——见 docs/multi-session.md 与 docs/webui.md；非目标条目保留作历史
记录。）

## 2. 包结构与依赖规则

```
cmd/sai/main.go     启动期：读一次配置/flag → agent.New → REPL
internal/
  agent/            合并原 agent + chatapp：会话状态层 + agent 循环
    agent.go        New(Options)：Provider/Storage/Tools 构造期注入
    session.go      状态层：commit / AppendDelta / Snapshot / Send / Stop
  storage/
    storage.go      接口：Store{Create, Open} + Session{Append, Records, ID, Close}
    record.go       Record 类型（schema，两实现共享）
    fold.go         Fold() 纯函数 + State + Summarize
    jsonl/          文件实现：目录布局、ProjectID、torn-tail 恢复
    memory/         内存实现：map + slice（测试与临时会话）
  model/            不变：Provider 接口 + openai_chat / openai_responses / anthropic_messages
  tools/            shell 工具
  config/           配置解析
```

依赖规则（硬约束）：

```
agent   → {model, storage, tools}    永不 import config
config  → 只被 cmd/sai 引用
运行期零配置读取：所有参数在 agent.New 时注入，构造后不可变
```

## 3. 分层

```
agent 循环（LLM 输出、工具结果、用户输入）
        │ 只走一条 commit 路径
agent.Session（状态层：owns State + Fold 推进 + 通知）     ①
        │ Record
storage.Session（jsonl / memory，纯记录存储）              ②
```

- **② storage 是笨存储**：只认记录。jsonl 管目录布局、段切换、
  torn-tail 恢复；memory 是 map + slice。两者都不维护状态、不发通知。
  jsonl 的会话目录是分段的：`log.jsonl` 为活跃段，`log-NNNN.jsonl` 为
  每次压缩前的不可变归档；storage 接口不变，段管理是 jsonl 内部实现。
- **① 状态层垄断写路径**：`commit` 是唯一写入口（落盘 → fold → 清
  transient，同一锁段）。Open 时全量重放记录重建状态。
- **eventbus 的 durable 词汇表和 sessionlog.Publisher 死亡**：agent 直接
  commit 记录，不再经过事件中转。

## 4. 记录 schema（终稿）

每条记录都是一个 item，都有 ItemID。RunID 出现在 run 范围的记录上
（session.created 无）。存储格式沿用 JSONL，逐行 fsync 策略不变。

```
session.created   {ItemID, provider, model, cwd}
user.input        {ItemID, RunID, Content}
assistant.message {ItemID, RunID, Content, Reasoning, ToolCalls[{ID, Name, Arguments}], Iter}
tool.result       {ItemID, RunID, ToolCallID, Name, Content, IsError, Iter}
run.failed        {ItemID, RunID, Error}
context.compacted {ItemID, Content}                  // 摘要替换全量历史，无 RunID（会话级）
```

字段归属（自足的事实 + wire 回放必需的数据，仅此而已）：

| 字段 | 归属 |
|---|---|
| `ItemID` | 展示身份，agent mint |
| `RunID` | 轮次分组 + grep（无状态消费者） |
| `ToolCallID` / `ToolCalls[].ID` | wire：provider 回放配对 |
| `Iter` | 诊断数据（可从 run 内 assistant message 计数派生，留着） |

旧 schema 的 `turn`、`seq` 字段删除。**行序由文件顺序保证**（单写者 +
append-only），torn-tail 恢复只依据行完整性与 JSON 合法性，**不做内容级
的行序校验**——外部工具插入/重排合法 JSON 行属于威胁模型之外。旧的带
seq 日志仍可读（Unmarshal 忽略未知字段）。

## 5. 身份体系

| 身份 | mint 时机 | 用途 |
|---|---|---|
| ItemID（每条记录） | 见下表 | UI upsert 唯一键；出生先于落盘 |
| RunID | Send 时 | 轮次分组；每条 run 内记录都带，`grep '"run":"run-…"'` 直查 |
| ToolCallID | provider 给（空缺时 agent mint 顶上） | wire 配对；UI 的派生 join 键 |

（seq 已删除：它是行号的冗余编码，唯一的消费者——按 seq 寻址的 tail——
已不存在。）

ItemID mint 时机（全部满足“身份先于落盘”）：

| 记录 | mint 时机 |
|---|---|
| session.created | NewSession 时（记录随第一条 commit 提交，空会话不落盘） |
| user.input | Send 时 |
| assistant.message | 迭代开始 |
| tool.result | **ToolStarted 时**（执行开始，结果身份先于结果存在） |
| run.failed | settle 时 |
| context.compacted | Compact 开始时（记录仅成功时落盘，失败/中断不留痕） |

session.created 的落盘由状态层完成（NewSession 备好记录，commitLocked
随第一条真实记录提交），store 只分配会话 ID 和目录——这保证**每条记录
都流经同一条写路径**，包括 session.created。

原则：**身份键 agent mint；引用键搭便车**。result 不存 ParentItemID/CallIndex
——result→call 的 join 由 UI 用 ToolCallID 派生，agent 不需要这个查找
（执行时手里同时有 call 和 result；fold 只追加；sanitizeContext 是 call→result
方向）。

## 6. State 与不变量

```go
type State struct {
    // durable 区：只由 Fold(records) 推进
    SessionID, Provider, Model, CWD, Title string
    CreatedAt, UpdatedAt time.Time
    Context []model.Message    // 下次请求的历史（wire 世界）

    // transient 区：只由 delta/生命周期事件推进，重启即失
    Streaming  *StreamingItem  // 生成中的 assistant message（含半成品 tool calls）
    Running    *RunningTool    // 执行中的 tool
    Compacting *CompactingItem // 进行中的上下文压缩（成功时落 context.compacted）
}

type RunningTool struct {
    ItemID    string           // 未来 tool.result 的 ItemID，出生即定
    ToolCall  model.ToolCall
    StartedAt time.Time        // UI spinner 用
}
```

不变量清单：

1. **`State 减去 transient == Fold(storage 全部记录)`**。Fold 是纯函数，
   永不知道 Streaming/Running 的存在。
2. **delta 永不进 Fold、永不落盘**。它们是尚未完成的记录的碎片，可能属于
   一次将失败重试的请求。
3. **commit 三步原子**：落盘 → fold → 清 transient，同一锁段。Snapshot
   永远不重复、不缺失。
4. **状态只被"已被 storage 接受的记录"推进**（先 Append 后 Fold；崩溃在
   两者之间无害，重放即恢复）。
5. **终态等价**：终态事件载荷 ≡ 落盘记录载荷（同 ItemID 同内容，出自同一
   对象）。UI 按 ItemID 幂等 upsert，谁先到都正确。
6. **单写者**：单飞约束（running 标志拒绝并发 Send）+ commit 垄断。

## 7. 流式模型

**（milestone A step 4 定稿）**：两 channel（Subscribe 推记录 + Events 推事件）
已由**单一有序会话流**替代（`SessionEvent`：RecordCommitted / Transient /
Status，seq 在 s.mu 内分配）。hub（step 3）与 CLI（step 4）迁移完成后，
旧 Subscribe/Events 路径与 per-run 事件通道**已整体删除**——瞬态事件只在
会话流上存在，run 不携带任何通道；`TestNoPerRunEventChannel` 源码扫描钉死
不复回归。快照接口 `Stream()` 返回 `StreamSnapshot`（records +
瞬态槽 + steer 队列 + 状态，同一锁段原子拷贝）；不带订阅的等价快照
`SnapshotStream()`（step 3：hub 的连接时 snapshot 帧由它构造，避免
Snapshot()+Running()+Compacting() 多次取锁的拼接窗口）与轻量变体
`SnapshotStreamState()`（不拷 records，给每次请求的调用方）。

- **单一累积点**：`session.AppendDelta` 写 `Streaming.Message`；
  `streamModelTurn` 的局部 builder 删除。Completed 时 commit 从 Streaming
  取内容拼记录。
- **重试语义**：重试 = 重置 Streaming + reset 信号。UI 是 replace 语义而非
  append 语义（Streaming 存全量累积内容）。CLI 终端可以直接 append 打印，
  两个 UI 语义不同没关系，数据源同一。
- **失败语义（接受的易失性）**：失败的半截内容只在失败事件里带给在场的
  UI；Snapshot 不保留，刷新即消失。中断的 run 就是 run.start 后无人应答，
  不需要收尾记录。
- **重连**：Stream() 的 StreamSnapshot + 继续事件流，无缝接上，零重拉。

## 8. tool call 生命周期

```
生成阶段   delta → Done          长在 Streaming.Message.ToolCalls，按 (ItemID, Index) 寻址
消息落盘   assistant.message     tool calls 随消息 commit（wire ID 在 ToolCalls[].ID）
执行开始   ToolStarted           mint result ItemID → Running 槽
执行中                          重连 UI 从 Snapshot 看到 Running 占位
执行结束   ToolResult ≡ 记录     同一 result 对象，同 ItemID 无缝 upsert
```

wire 世界（Context）继续用 provider ID 配对（openai_chat 的 `tool_call_id`、
anthropic 的 `tool_use_id`）；展示世界零依赖 provider ID。provider 返回空 ID
时 agent mint 顶上——provider 校验的是 assistant tool_calls 与 tool result 的
自洽性，不是 ID 格式。

`sanitizeContext`（补洞悬空 tool call）随合并进 agent：请求前修补 wire 上下文。

已知限制（与旧实现一致）：`ResponseState`（OpenAI Responses 的 reasoning
items 等 provider 内部状态）不落盘，事件也不转发 UI，resume 后丢失。

## 9. 术语表

```
session    会话：一个 log.jsonl
  └─ run   运行：一次输入 → 最终输出（不再需要 tool call）
       └─ iteration  迭代：一次逻辑请求（含其重试）+ 工具执行完毕
            ├─ assistant message（item）
            └─ tool results（item）
```

- **`turn` 一词废除**（现状：TurnID 指 run、MaxTurns 限的是 iteration，一词
  两义）。`MaxTurns → MaxIterations`。
- iteration 边界 = 一条 assistant message（带 tool calls）+ 它的 tool results。

## 10. 死亡清单

| 删除 | 替代 |
|---|---|
| internal/chatapp | 并入 agent |
| internal/eventbus durable 词汇表 + sessionlog.Publisher | agent 直接 commit |
| Store.List / Summary | 后来 overturned：storage.SessionInfo + jsonl.ListSessions 支撑多会话 serve（docs/multi-session.md）；Summarize/Summary 已删除 |
| storage 级 RecordsAfter / Subscribe | 通知上移到 agent.Session 的 commit 通知（携带记录本身） |
| State.LastSeq | 无，行序即文件序 |
| TurnID / turn 字段 / MaxTurns 命名 | RunID / MaxIterations |
| agent 的 config 依赖 | 参数全部构造期注入 |
| chatapp.Session.Close 的 5s 忙等 | 显式同步（cancel → 等 pump goroutine 退出） |

## 11. UI 更新模型

**（milestone A step 4 定稿）**：记录流与事件流已合并为单一有序会话流
（§7 修订）。CLI 的 parked/reconcileParked/liveShown/waitForRecords 与
hub 的 Wake/drain 补丁已随 step 3/4 迁移删除（顺序由 seq 全序结构保证，
无需任何暂存裁决）。

| 场景 | 通道 |
|---|---|
| 在场 | SessionEvent 流（seq 全序，ItemID 寻址，幂等 upsert） |
| 重连 | StreamSnapshot 一次 + 继续事件流 |
| 落盘 | RecordCommitted（对 UI 只是终态事件的等价副本） |

CLI 的在场渲染（step 4 已迁移）：send/compact 各自订阅会话流，在**单个
循环**里渲染 RecordCommitted（打印；live 渲染中的 assistant 消息按
started 时记录的 ItemID 跳过——seq 全序使裁决无竞态）、Transient（delta
逐字写终端，reasoning 带 `·` 行标记；压力/重试/压缩失败走 stderr）与
Status（终态即循环终点）。循环在终态 Status——run 的**最后一条流内
事件**——之后才返回，故下一个提示符必然在 run 全部输出之后（有字节级
测试钉死）。spinner 只标记静默阶段（等首 token、工具执行中），事件间隙
以 time.After 驱动，无轮询。/stop 经 CurrentRun+Done join，同样不忙等。

WebUI 的桥（已落地，internal/web）：hub 把记录流与事件流按因果序经 SSE 转发
（快照优先 + 摘要链断点续传，docs/webui.md），两个 UI 端（/log 零依赖页与
/chat 聊天客户端）按 ItemID upsert。step 3 将 hub 迁到 SessionEvent 流，删除
Wake/drainEvents/drainThenStatus。

## 12. 已知限制与已记录的未来需求

**自动压缩失败的双轨语义（§13 修订）**：软压力下的压缩失败（摘要调用出错、
摘要为空、软回收不足的 churn）**不再让 run 失败**——降级为一条
`compaction_failed` 提示事件（CLI 打 stderr、web 缓存为 notice 供
`/api/state`），run 用**原请求**继续；没有记录落盘（失败的压缩不产生
context.compacted）。硬压力下失败仍然让 run 失败（请求本就超限，发送
不安全）。**用户中断（ctx 取消）不算软失败**——按中断处理，既有机制
跳过 run.failed。**重试节奏**：软失败后进入与成功相同的冷却期（两个
新的 user.input），失败标记在内存（重启即清零，给一次新机会）；成功
后清除标记。防止每个迭代都重试失败的摘要调用、浪费请求。

**中断时正在执行的工具会留下一条取消结果（已记录的行为）**：Stop 中断 run
时，工具批次里尚未开始的调用被跳过，但正在执行的那一个会提交一条错误结果
（如 "shell command canceled"）——对用户可见"中断前进行到哪"有价值，保留。

**内存随会话生命周期增长（已知限制）**：agent.Session.records、web Hub 的
records/chain/index 都保留完整扁平记录流，压缩后不裁剪——与"成本 ∝ 有效上下文"
的目标相悖。冷却是唯一回头扫的点（softCooldownElapsed 只需回溯到最后一条
context.compacted），因此内存中的记录尾可以按冷却窗口截断；hub 镜像只需保留
窗口 + 摘要索引。尚未实施：先量化（长会话的常驻内存）再决定裁剪点。

## 13. 已记录的未来需求（未设计）

- ~~**/clear 只重置 context window，不新开 session**~~——需求已取消：
  新建会话命令改名 /new（原 /clear，里程碑 A 后），上下文重置由
  /compact 承担。

## 13. 上下文压缩（手动 + 自动）

手动 `/compact` 与自动压缩共享同一条路径：`compactContext`（mint 身份 →
Compacting 槽 → 带重试的摘要调用 → 软压回收检查 → commit），产出完全
相同的 `context.compacted` 记录，Fold 无需区分来源。

**段切换**：每次压缩 commit 时，jsonl 把当前 `log.jsonl` 归档为
`log-NNNN.jsonl`，新开 `log.jsonl`，首条为原 `session.created` 的逐字
副本（段头，保会话元数据与创建时间自包含），随后是
`context.compacted`。因此：**resume 只读活跃段，成本 ∝ 有效上下文而
非会话 lifetime**；压缩点之前的历史永不被重放，只供归档审计
（`grep ... log*.jsonl`）。Title 由活跃段首条 user.input 决定（压缩后
刷新）。切换是两段式改名：先在 staging 文件完整写入并 fsync 新段，
再归档旧段、激活新段——所有中断窗口都收敛到"压缩丢失、上下文原样"
或"完全生效"，无新崩溃语义。旧单文件会话即单段会话，首次压缩才
产生归档，向后兼容。

**预算推导**（budget.go，§15.2 修订：**按会话**——窗口与输出预留来自
该会话 resolved 的模型配置，非进程级 Agent）：`context_window − 输出预留
（output_limit 或 clamp(窗口/8, 4K–20K)，且不超过窗口/4）= 容量`；硬线 =
容量 − 安全边（2%，clamp 512–8K）；软线 = 硬线 − headroom（硬线的 20%，
clamp 2K–32K）。退化配置（微小窗口、输出预留 ≥ 窗口）回退到窗口的 3/4
作容量，预算永不为零——零预算会静默关闭自动压缩。token 估算：
ASCII/4 + 非 ASCII×1 + 消息头 8 + 请求头 16 + 工具 schema（名字/描述/
InputSchema 的 JSON）。

**触发点**：新轮次前（待处理输入已在 Context 内）与每个工具批次后。

**守卫**：Context 无 assistant 回答不压（硬压时发 ContextPressureEvent
告知用户超预算但无摘要可压、请求照发）；软压冷却（§14.4 双计数器）：
`inputsSinceCompacted`（compacted commit 清零、user.input commit 递增、
**Open 时从活跃段重建**——最后一条 compacted 之后的 user.input 数）与
`inputsSinceSoftAttempt`（纯内存，重启即复位——过去软失败的持久故事是
notice，不是楔子）；从未压缩过时 since-compacted 约束为空（首次软压
即刻尝试，与旧扫描语义一致），否则两者都需 ≥2；软压回收检查——摘要后
估算缩减不足 max(2K, 前/20) 则丢弃不落盘；硬压压缩后仍超硬线 →
run.failed（与旧版差异：记录先落盘再判定，下次 Send 会再压缩——窗口
容不下任何摘要时本就无解）。

**与旧版的差异**：待处理输入被吸收进摘要（旧版保留原文；新架构下
user.input 先落盘、compacted 后到，Fold 语义只能是整体替换）；无 usage
锚定（纯估算）；无 MaxRequestBytes；摘要调用带与普通回合相同的重试。

**摘要自描述框架**：`context.compacted` 的 Content 存的是框架前缀 + 摘要
正文——重放为唯一先前上下文时，模型把它当自己的工作记忆而非待验证的
用户断言。已知代价（有意接受）：框架随记录不可变（措辞不可演进）、
出现在 CLI/UI 的 compact 块显示中、重复压缩时作为先前摘要的一部分再
喂给摘要模型。替代方案（Fold 打标记 + 请求层包装）被否决：记录自包含
优先于请求层零知识。框架用英文书写（对模型的指令，同 system prompt）。
Title 由活跃段首条 user.input 决定（压缩后刷新；刚压缩完尚未有新输入
时为空——将来做会话选择器时如需稳定标题，把标题源写进段头）。

**transient 事件**：CompactionStartedEvent/CompletedEvent（带 ItemID）
随 run 事件流发布；手动 Compact 无事件流（CLI spinner 覆盖）。硬压下
Completed 与 run.failed 并置是有意行为（摘要已落盘，见上）。

## 14. 架构里程碑 A：Run 对象 + 单一有序会话流

（阶段 0 设计基线；只改文档，代码分阶段送审。修订 v2：吸收 review 的
A0–A4 四个设计漏洞——流的归属从 per-run 改为 session 级、终态进流、
阻塞策略带取消出口、截断通知与 /history 静默失败的处理、冷却计数的
重建语义——及 B 组六条文档统一。）

### 14.1 Run 对象

**现状问题**：`Session` 同时持有 `running/cancel/events/done` 四个 run 级
字段。Send 和 Compact 各自管理它们的赋值和清理，交叉修改导致"持已关闭
channel 发送"这类 panic（P0-1）、以及"Compact 拿槽但无事件流"这类边角。

**设计**：

```go
// Run 拥有自己的取消和等待信号。事件不走 Run——它们走会话级流（§14.2）。
type Run struct {
    session *Session
    ctx     context.Context  // derived from the SESSION's ctx, not the caller's
    cancel  context.CancelFunc
    done    chan struct{}   // close once, by the run goroutine
    err     error           // nil = success; ErrInterrupted = user-interrupted; else failure
    kind    RunKind         // RunTurn | RunCompact
}

// Session 侧
func (s *Session) Start(ctx context.Context, input string) (*Run, error)
func (s *Session) StartCompact(ctx context.Context) (*Run, error)
func (s *Session) CurrentRun() *Run  // nil when idle
func (r *Run) Stop()                 // cancel；不等 Done
func (r *Run) Done() <-chan struct{}
func (r *Run) Err() error
func (s *Session) Run(ctx context.Context, input string) error // 同步入口
```

- **ctx 语义**（N2 定案）：调用方的 ctx **只在 Start 返回之前有效**——
  Start 检查它（已取消则直接失败，**在 commit 之前**——不给
  一个从未启动的 run 留下 user.input 记录），然后**不再关联**。Run 的生命
  周期 ctx 从 Session 自身派生，只受 `Run.Stop()` 和
  `Session.Close()` 控制。web 的 handleSend 传 `r.Context()` 不会
  在 HTTP 请求返回时意外杀掉 run。调用方确实想绑定生命周期时，
  用显式的 `RunOption`（默认关闭，本里程碑不实现）。
- **中断入口**：R2 之后中断只有 `Run.Stop()` 和 `Session.Close()`
  两条路——CLI 的 Ctrl-C 信号处理与 web 的 `/api/stop` 都走
  `Run.Stop()`（stage 1 起 handleSend/handleStop 改为传 ctx 的
  Start/Stop）。
- **Err 语义**（N1）：`nil` = 成功；`ErrInterrupted`（哨兵，
  `errors.Is` 可判）= 被 Stop/中断；其他 = 失败（对应 run.failed）。
- **Run 不拥有事件 channel**：事件流是 session 级的（§14.2）。
- **单飞**：`Start` 在锁内检查 `currentRun != nil` → 拒绝。settle 在
  锁内清 currentRun ⟹ 观察到 nil 即安全（无需 isDone 辅助字段——
  拆卸序保证指针清除先于 done close，而 Start 只看指针）。
- **Run 结束的拆卸**：run goroutine 的 defer 序列：
  1. 清瞬态槽（Streaming/Running/Compacting 置 nil）；
  2. 在流内发终态 `Status{Running:false}`（保证路径，§14.2）；
  3. 锁内置 `session.currentRun = nil`；
  4. `close(done)`。
- **Done() 是补充信号**：消费方判定 run 结束必须 drain 到流内终态。
- **手动压缩**（N3）：`StartCompact` 产生的 run 也有事件流——
  CompactionStarted/Completed/Failed、Status{Compacting:true/false}
  都走流。这消除了 handleCompact 的 5ms/200ms 轮询（那些轮询正是因为
  手动压缩没有事件流才加的）。步骤 3 验收加"删除 handleCompact 轮询"。

### 14.2 单一有序会话流

**现状问题**：Subscribe（记录）和 Events（事件）是两个独立 channel，
消费方无法证明到达顺序。hub 靠 Wake/drainEvents/drainThenStatus 打
补丁，CLI 靠 parked/reconcileParked 暂存裁决。

**设计**：

```go
// SessionEvent 归属 agent（B2 步骤 5 起 public 顶层包）。
package agent

type SessionEvent interface{ isSessionEvent() }

// 三种具体事件：
// RecordCommitted{Record storage.Record}
// Transient{Event model.Event}
// Status{Running, Compacting bool}  // N4: 不含 Notice——那是 hub 的展示层概念

func (s *Session) Stream() *StreamSubscription

type StreamSubscription struct {
    Initial() StreamSnapshot
    Events() <-chan SessionEvent
    Lost() <-chan struct{}  // B3: 摘除信号；收到后调用方必须 Stop() 并重新 Stream()
    Stop()
}
```

**流的归属与生命周期**：
- **session 级**，一个 Session 一条流。**订阅 channel 永远不 close**
  （B1，原 a 方案）：结束信号用单独的 `Lost()` channel——被摘除时
  close(Lost)，订阅方收到后自行 Stop + 重连。这消除了"锁外多生产者
  发送 vs Close 关闭"的 send-on-closed 竞态（P0-1 同类）。
  Session.Close 按下文 R3 的五步顺序执行（取消 ctx → 等 run 排空 →
  通知 dispatcher 摘除全部订阅者 → 停 dispatcher → 关 storage）。
  订阅 channel 本身不关。dispatcher 在首次 Stream() 时懒启动（偏离 v5 设计的"NewSession/Open 时启动"：没有订阅方就没有消费方，入队只会让队列无限增长——enqueueStreamLocked 在 streamStop == nil 时直接丢弃。结果：首次 Stream() 之前的事件不入流、不分配 seq；新订阅方靠 StreamSnapshot 补齐状态）。
- **事件生产方**（两个，都合法）：run goroutine 与 API goroutine（steer）。

**全序规则**（B2 定案 + R3 边界）：
- **在 `s.mu` 内分配流序号**。每条 SessionEvent 在锁内获得一个
  递增的 `seq`，fan-out 按 seq 顺序投递。状态变更与流序号在
  同一个锁段分配——**"状态变更顺序 = 流内事件顺序"是结构保证**。
- **fan-out 队列无上限**（R3）：生产者在锁内入队**永不阻塞**
  （append 到 slice，无容量上限）。dispatcher 的投递全部非阻塞
  （select+default / 摘除），队列以 CPU 速度被清空——生产速率
  （LLM 流式输出）远低于消费速率（内存操作）。
- **dispatcher 是唯一的发送方**（R3）：生产者入队（锁内、
  永不阻塞），dispatcher 投递（锁外、非阻塞）——单发送方 +
  永不 close 的订阅 channel ⟹ 无 send-on-closed 可能。
- **Lost 只关一次**（R3）：**只有 dispatcher 关闭 Lost**。订阅方
  主动 `Stop()` 时只是把自己从 fan-out 列表移除（锁内），不碰
  Lost——Lost 的 close 是 dispatcher 的摘除动作，且摘除后该
  订阅者已不在列表上，不会再被摘第二次。单关闭方 + 单次
  列表移除 ⟹ 无 double-close 可能。不需要 sync.Once。
- **dispatcher 生命周期**（R3）：Session.Open 时启动；Close 时
  通过一个 `stopDispatcher` channel 通知退出，Session.Close
  等 `<-dispatcherDone` 后才关 storage。Close 期间 run 产生的
  事件： dispatcher 收到 stop 后清空队列、关全部 Lost、退出
  ——订阅方收到 Lost 重连时发现 session 已关（Stream() 返回
  错误或空快照 + 立即 Lost），自然终止。
- **Session.Close 的完整顺序**（R3，C3 修订）：1) 取消当前 run 的
  ctx（`Stop()`）→ 2) 等 run 排空（< -done）→ 3) 通知 dispatcher 摘除
  全部订阅者（由 dispatcher 关闭各自的 Lost）→ 4) 停 dispatcher
  （关 stopDispatcher，等 dispatcherDone）→ 5) 关 storage。
  dispatcher 在首次 Stream() 时懒启动（理由见上）。
- 这同时解掉"Status{Running:true} 在锁内发 vs 锁外发送纪律"的矛盾：
  **全部入队都在锁内**（与状态变更同锁段），**全部投递都在锁外**
  （dispatcher goroutine）。发送纪律的"锁外"指的是投递，不是入队。

**顺序保证**：
- **run 内**：同一 item 的 started/delta 在 RecordCommitted 之前
  （同 goroutine、seq 递增）。
- **completed 与 RecordCommitted**：commit 先、completed 后。消费方
  幂等。
- **跨 run**：N 的末次入队 < N 的 close(done) < 锁内置 currentRun=nil
  < N+1 的 Start 拿锁 < N+1 的首次入队。
- **steer 消费 vs steer_queued**（B2 的具体场景）：Steer 持锁入队
  seq(k) 的 steer_queued；drainSteered 持锁 commit user.input 并
  入队 seq(k+1) 的 RecordCommitted。seq(k) < seq(k+1) ⟹ 客户端
  先收到 steer_queued 再收到消费记录——**不会出现已消费的排队消息
  复活**。
- **Status 帧**：running=true 在 run 首事件之前、running=false 在
  最后一条 RecordCommitted 之后（拆卸序列 + seq 递增）。

**背压与摘除**（B3 修订）：
- channel 缓冲 `streamBuffer = 1024`。
- **水位线**：delta（Transient）在缓冲超过 **3/4** 时开始丢弃，
  给 RecordCommitted 和 Status 预留 1/4 空间——降低摘除频率。
  **所有 Status（含 run 开始的）都不可丢弃**：消费者靠 run-start
  建立生命周期机制（hub 的遗留通道排空以此触发），静默丢弃会让
  run 永久卡死且无恢复路径；满则摘除订阅者，重订阅后由快照
  兑底（step 3 修订：早期实现把 run-start 标为可丢弃，与本文冲突）。
- **RecordCommitted / Status**：缓冲满即**摘除该订阅者**——
  close(Lost) + 从 fan-out 列表移除。订阅方收到 Lost 后重新
  `Stream()`（从 Initial 重建，hub 侧触发 window reissue 给它的
  SSE 客户端）。
- **流式状态变更与 seq 同锁段**（step 3 修订）：Streaming 内容
  的累积（delta 追加、tool call 追加）与对应 Transient 的入队
  必须在**同一锁段**（applyStreamingEmit）。分两个锁段时，快照
  可能落在中间——内容已折进 Streaming 但事件 seq 还未分配，
  客户端会把同一段文本收两次（一次在快照里，一次重放）。
- **终态 Status 的保证路径**：终态入队后，即使所有订阅者都满了，
  dispatcher 也不阻塞——它投递到空列表（全部摘除后），然后 run
  goroutine 正常拆卸。权威收敛（running/compacting 是 Session 的
  状态，Initial()/重连拿到正确值）兜底。
- **读者契约**：收到 `Lost()`（或自己 `Stop()`）后**不得再读
  `Events()`**——必须重新 `Stream()`。半途已投递但未消费的事件按
  "重连重放"处理（Initial + window reissue），不作为真值。
- **无死锁**：队列无上限 ⟹ 入队永不阻塞 ⟹ 终态入队不会因订阅者
  慢而卡住拆卸 ⟹ Close 的"等 run 排空"与终态入队不会互等——
  结构上不存在死锁路径。
- **无订阅方**：dispatcher 对空列表无操作。
- **摘除恢复测试计划**：慢订阅者被摘除 → 收到 Lost → 重新 Stream()
  → Initial() 与最终状态一致（step 5 的验收项）。

**通知与 Notice 的分层**（N4）：
- agent 层的 SessionEvent.Status 只带 Running/Compacting——
  **不含 Notice**。Notice 是 hub 的展示层缓存（生命周期规则、
  ClearNotices 语义都在 hub），由 hub 从 Transient 事件
  （CompactionFailedEvent、ProviderRetryEvent 等）自行推导。
- agent 不发 Status{Notice} 帧。

### 14.3 steer 的归属

**决策**（用户确认）：steer 队列**不归属 Run**，跨 run 存活，由
Session（状态层）持有。

- 队列在 `State.Steered`。Snapshot 能看到。事件走会话流。
- **准入谓词**：`CurrentRun() != nil && CurrentRun().kind == RunTurn`。
  压缩 run 不满足——返回 ErrNoRun。
- **消费时机**：run 的迭代边界——首迭代前 + 每迭代末
  （`iteration < maxIterations` 时）。预算耗尽的那次终态不 drain。
- **消费 vs 事件的顺序**：drainSteered 与 steer_queued 的 seq 在
  同一把 `s.mu` 下分配（§14.2 全序规则）——入队先于消费记录，
  消费者不会看到已消费条目的排队事件"复活"。
- Run 结束时剩余条目留给下一个 run（用户确认，语义不变）。

### 14.4 内存（原 P1-7）：只保留活跃段

**（step 5 已实现）**：Session.records 在 context.compacted commit 时
截断为 `[session.created, context.compacted]`（与 jsonl 段切换、
memory store 的同语义截断对齐——内存中与 fresh Open 重放逐条一致）；
hub 镜像同源截断 + 段 genesis 重置 + snapshot 重发（§14.5）。CLI 的
摘除恢复按 ItemID（非下标）续渲染。

**设计**：

- **Session.records 截断到活跃段**：commit 检测到 context.compacted
  时截断为 `[session.created 副本, context.compacted]`（与 jsonl 段
  切换对齐）。内存中的 records 与 fresh Open 重放一致。
- **软冷却计数**：改为状态层的两个计数器：
  - `inputsSinceCompacted int`：距上次成功压缩的 user.input 数
    （compacted commit 时清零；user.input commit 时递增）。
  - `inputsSinceSoftAttempt int`：距上次软尝试的 user.input 数。
  - 冷却通过 = 两者都满足 ≥2。
  - **off-by-one 断言**：软尝试之后第 1 个新输入不重试、第 2 个
    重试恰好一次（与现有计数断言同口径）。
  - **重建**：`inputsSinceCompacted` 在 Open 重放时从活跃段重建；
    `inputsSinceSoftAttempt` 纯内存。
- **hub 镜像**：records/chain 保留活跃段（同源截断）。

**digest 链跨段的设计**（B4 定案：**逐段独立，零持久化**）：
- **段 0（活跃段无 context.compacted）**：genesis =
  `H(chainVersion + sessionID)`——与现有定义**完全相同**。
  现有会话、两个客户端零改动。
- **压缩后的段**：genesis = `H(chainVersion + sessionID + "/" +
  context.compacted 的 ItemID)`。该 ItemID 在截断时和 fresh Open
  时都能拿到（就是活跃段的 compacted 记录），**不依赖任何额外存储**、
  不要求 storage 层知道 digest 链、不修改段头（§13 的“逐字副本”
  不变）。**段形态与判据**（截断与 fresh Open 调用同一个函数
  `segmentGenesis`，重启一致性对每种形态成立）：
  - `[session.created, context.compacted, …]`（有 header 的压缩后段）
    → 种子取 records[1] 的 ItemID；
  - `[context.compacted, …]`（**无 header 的压缩后段**——旧段首行
    无效时 jsonl 段切换不拷贝 header，这是真实磁盘状态）→ 种子取
    records[0] 的 ItemID；
  - 注意：无 header 的**段形态**在链推导上仍成立（列表也容忍它们、
    显示"无模型"），但整份日志折叠后**无任何 session.created** 的
    会话在打开层被拒（§15.2：空 meta → ErrModelUnavailable）——
    "列表可见、打开被拒"，两者不矛盾：列表展示磁盘事实，打开需要
    可解析的模型身份；
  - 其它（段 0，含无 header 的段 0——手工截断/手改首行）→ 段 0
    genesis。
  判据是纯数据推导，**不 panic**：畸形段退化为确定性的链（客户端
  走 miss/窗口重放重同步），绝不能楔死 serve 进程（P1 教训：
  panic 发生在 NewHub 持 h.mu 时，且 Manager.open 的 opening 占位
  清理若无 defer，一个手改日志文件就能让该会话的所有请求永久
  挂死——占位清理因此是 defer，防一切 panic，不只这一处）。
- **重启一致性**：截断时 hub 的 genesis 基于活跃段的 compacted
  ItemID；重启后 fresh Open 读同一段、拿到同一 ItemID、算出同一
  genesis，fold 同一记录集 ⟹ **同一条链**（结构保证）。
- **将来跨段回滚**：每个归档段用自己的 genesis 独立校验
  （genesis = H(version + sessionID + "/" + 该段首条 compacted 的
  ItemID)）；服务端按段顺序返回，客户端逐段校验——不需要
  首尾哈希衔接。
- **测试**（step 5 验收）：压缩 → 截断 → 记录链头 → 重启
  （Close + Open）→ fresh Open 的链头 == 截断时的链头。再加
  **线级断言**：重锚 snapshot 后，hub 广播的 head 与客户端
  按 `frame.anchor + records` 折出的 head 相等。
- **公式实现约定**：Go 侧一个参数化 helper
  `chainGenesis(sessionID, compactionItemID)`（writer/hub/
  校验方共用，hub 不另写公式）；JS 侧 chain.mjs 的
  chainGenesis 同参数化（本里程碑不受影响——服务器总给
  anchor——但 fixture 加一个"压缩后段"用例提前钉住 Go↔JS
  一致性）。

**截断通知**：commit context.compacted 时，hub 对已连接客户端重发
snapshot 帧（`resumed:false` + 新锚 + 窗口记录）。客户端现有逻辑
天然处理。**不引入新字段**。

**/history stale_anchor**（N5，定案）：保留 409 stale_anchor，
客户端收到后重置 prefixAnchor 为当前链头并重试一次；仍失败则显示
"已到活跃段开头"。"游标不认识就从头返回"**不采纳**——它会破坏
`fold(anchor, records) == prefixAnchor` 的校验契约（客户端会把
结果当 drift 丢掉）。

### 14.5 对外线协议

**原则不变**：SSE 帧（snapshot/record/event/status）、digest 链
规则、`/api/s/{id}/...` 路由、稳定码表不动。chat 和 /log 客户端
不需要协议变更——截断通知走现有 snapshot 帧重发，无新字段。

hub 内部改消费 SessionEvent 流，帧映射：
RecordCommitted → record 帧、Transient → event 帧、Status →
status 帧。快照帧从 Initial 拼装；截断时的 snapshot 重发复用
同一帧映射（§14.4）。

### 14.6 已知限制（milestone A 边界）

- **回滚以活跃段为界**：翻历史翻不过压缩点。将来扩展：读归档段，
  段边界衔接见 §14.4 的链跨段设计（逐段独立 genesis，
  各归档段独立校验，不需首尾衔接）。不堵死。
- CLI stderr 提示的 e2e 未写（低优先）。

### 14.7 迁移顺序与回归保护

| 步骤 | 内容 | 回归保护 | doc-sync |
|---|---|---|---|
| 1 | Run 对象；Send/Compact 走 Start/StartCompact；删 Session 上的 events/cancel/done（事件流暂留 Subscribe+Events 原样） | 现有全部测试不改断言；steer 准入 kind==RunTurn 行为等价；TestSteerSettleRaceCanary 不改过 | multi-session.md §3 锁域段补 currentRun 指针 |
| 2 | SessionEvent/Stream（seq 全序 + dispatcher + Lost 信号）；旧 Subscribe/Events 变纯转译薄适配 | 双接口同场景顺序测试；steer 消费 vs 事件顺序测试；慢订阅者摘除→Lost→重连恢复测试 | design §7/§11 更新 |
| 3 | hub 迁到 Stream；删 Wake/drainEvents/drainThenStatus；删 handleCompact 轮询 | TestEventRecordCausalOrder 断言变强；全部 web fixture 不改预期 | webui.md 更新 |
| 4 | CLI 迁到 Stream + 同步 Run()；删 parked/reconcileParked/liveShown/waitForRecords | CLI 渲染测试过；输出字节级：提示符在末条 delta 之后 | development.md 若提及轮询 |
| 5 | 内存截断；冷却双计数器；hub 截断 snapshot 重发；/history stale 保留 409 | 冷却四测计数断言保留；内存测试 records ≤ 活跃段+2；重启一致性：截断时链头 == 重启后 fresh Open 链头；摘除恢复测试 | multi-session.md + design §13 |

每步单独送审 → commit → 通知 opus 验收。

### 14.8 与里程碑 B 的兼容

- SessionEvent 定义在 agent（依赖 storage.Record 和
  model.Event）——B2 步骤 5 已随包公开（"一起搬"即同一时机）。
- Run 对象不泄漏 storage 细节——B 的 sessionhost 可以持有 *Run。
- Stream 的快照语义与 B 的 Namespace 化正交。
- seq 全序机制与 B 的 Hooks 正交（seq 分配在 Session 内部）。

## 15. 里程碑 B1：Namespace、按会话配置模型、sessionhost

三个互相咬合的重构：存储层去项目化（Namespace）、模型配置下沉到会话、
会话池从 web.Manager 抽出为不依赖 HTTP 的 sessionhost。它们是里程碑 C
（项目与会话管理）的直接前置。

### 15.1 Namespace（项目离开核心层）

**原则**：核心层（storage、agent）不知道"项目"。它只见一个**不透明的
ns 字符串**；"项目 = 从 cwd 推导 ns"与"注册校验 = 只见已注册 ns 的
会话"都是宿主语义，住在 projects 与 sessionhost。

**Store 接口改造**（storage.Store）：

```go
type Store interface {
    Create(ns string) (Session, error)      // ns：不透明分组键
    Open(ns, id string) (Session, error)
}
// 可选能力：宿主用类型断言探测，核心接口不膨胀。
type Lister  interface{ Store; ListSessions(ns string) ([]SessionInfo, error) }
type Deleter interface{ Store; Delete(ns, id string) error }
type Prober  interface{ Store; HasSession(ns, id string) bool }
```

- 目录布局不变：jsonl 的 `<root>/<ns>/<session>/log.jsonl`——ns 就是
  当前的 project 目录名；`ValidProjectID` 更名 `ValidNamespace`（仍是
  jsonl 的存储层契约：ns 必须是安全目录名），`ProjectID(dir)` 移到
  projects 包（宿主的 cwd→ns 推导）。
- **删除** `agent.Options.ProjectID` 与 `agent.OpenSessionIn`。Agent 的
  `NewSession(ns, meta)` / `OpenSession(ns, id)` 把 ns 原样透传给
  store——对 agent 而言 ns 无语义。
- **CLI `-session <id>` / `/resume <id>`（产品决策 Q1=方案 B，用户可
  见行为变更）**：只在当前目录对应项目的 ns 内查找；跨项目恢复返回
  "找不到"。**不引入 `Locator` 能力**，storage 的跨项目扫描 `Open(id)`
  一并删除（唯一调用方就是 CLI 恢复路径——`cmd/sai/main.go:539` 经
  `agent.OpenSession`；web 早已按项目查找，grep 复核无其他调用方）。
  memory store 的 `Open(ns, id)` 需校验 `projectOf` 归属（现 map 按
  id 全局存，ns 不设防）。CLI 的错误提示写明**"会话只能在它所属的项
  目目录下恢复"**而非裸 not found；README 同步。测试：项目 X 目录下
  `-session` 项目 Y 的会话 → 找不到，且**不得触碰 Y 的会话锁**（不开
  锁文件）。
- **jsonl 的 stderr 直写**（log.go 两处 mid-file corruption 提示）改为
  注入 `*slog.Logger`（`OpenStore` 增加 option；缺省 `slog.Discard`，
  CLI/serve 注入自己的 logger）。

### 15.2 按会话配置模型

**记录**：`session.created` 新增 `reasoning_level`（字符串；空 = 该
模型的默认推理强度。**不能叫 `reasoning`**——已被 assistant 推理内容
占用）。现有 `provider`/`model` 字段沿用。**digest 链已升 `sai/v2/`**
（2b 落地：Go wire.go 与 JS chain.mjs 同步、reasoning_level 追加进
元组**末尾**、chain_check fixture 含 v2 用例）：新可渲染字段按链版本
规则进元组（wire.go 既有注释即此语义）；升级后老客户端的 head 一律
miss → 走已有的窗口重放安全回退（一次性全量 resync，机制现成，
store_check s14 用真实 v1 摘要钉住该路径）。不采用"排除在元组外"——
那会留下永久性的"该字段不在防篡改覆盖内"特例。旧会话的
`reasoning_level` 空值在 wire 帧上被 omitempty 省略（字节不变），
在 v2 元组中以空串折叠。

**升级注意事项**：chat 的 dist 是手工构建的，升 v2
后不重建会持续显示 drift——README 的"dist 手工构建"段须提醒"链版本
升级后必须重建"，release 流程若不自动构建也要说明；/log 页面由二进
制内嵌，自动更新，无此问题。


```go
// storage 包：SessionMeta 就是 session.created 的载荷。
type SessionMeta struct {
    Provider, Model, ReasoningLevel string
}
```

放 storage（记录载荷），agent 与 config 都不互依。旧会话（无
`reasoning_level`）按空值打开 → 解析时取该模型默认（现有
`ApplyReasoningLevel(…, "")` 行为，零迁移）。

**组装分工**：

- **Agent 只管项目**：tools、tool executor、system prompt 模板（未渲
  染）、cwd、MaxIterations、AutoCompact。`Options` 删除
  Provider/ProviderName/ModelID/Parameters/ContextWindow/MaxTokens/
  DeveloperRole——全部来自会话的模型解析。
- **ModelResolver**（注入 Agent；internal/config 提供 sai.yaml 实现）：

```go
type ModelResolver interface {
    Resolve(meta SessionMeta) (Resolved, error) // ErrModelUnavailable
    List() []ModelDescriptor                    // C 预留（见 15.5），B1 只留接口位
}
type Resolved struct {
    Provider      model.Provider // 实例按 provider 名缓存，全会话共用
    Parameters    map[string]any
    ContextWindow int
    OutputLimit   int
    DeveloperRole model.MessageRole
}
```

  provider 实例缓存于 resolver 内部（`map[providerName]model.Provider`，
  互斥锁保护）——同一个 provider 只构造一次 HTTP client。**缓存键 =
  provider 名**：安全前提是配置进程内不可变（sai.yaml 启动读一次、
  运行期不重载——现状即如此）；若将来支持热重载，缓存键必须升为
  "名 + baseURL/key 指纹"，否则不同配置互相污染。
- **打开会话**：replay 后从 `session.created` 读 meta → Resolve →
  `ErrModelUnavailable`（类型化；web 映射稳定错误码
  `model_unavailable`，错误码表新增一行）→ **绝不悄悄换默认模型**。
  **空值同样拒绝**（Resolve 对空 provider/model 返回
  ErrModelUnavailable——ResolveModel 的默认值替换是创建时语义，
  解析期不做；两种退化情形：无头会话 = 无 created 记录 → 空 meta →
  拒绝；有记录但 provider/model 为空 = 手改/外来日志 → 拒绝。**两类
  会话在列表里仍然可见**（readSessionHead 容忍缺 header，显示"无
  模型"）——列表可见、打开被拒，见 §14.4 的说明。生产
  写入路径不可能写出空值：CLI/serve 的 meta 一律来自已解析的
  ResolveModel 输出，空值在到达 agent 前就被配置校验拦下）。
- **预算**：`resolveContextBudget` 改为按会话调用（open 时用该会话的
  ContextWindow/OutputLimit 算出，挂在 Session 上）；压缩预算随之按
  会话。
- **system prompt 的 `{{time.now}}`**：Agent 持有未渲染模板；渲染点
  收敛到**唯一的请求组装 helper**（`buildRequest` 的调用侧统一先渲
  染）——`Start`/`StartCompact` 与 **run 中途的 `maybeAutoCompact →
  compactContext`（它自己也组请求）**都走它，否则压缩请求会拿到未渲
  染模板、逐 run 语义不一致。cwd/config_dir 项目内稳定，time.now
  逐次组装刷新。

### 15.3 sessionhost

从 web.Manager 抽出会话生命周期，**不含 hub、不含 HTTP**：

```go
package sessionhost

type Pool struct{ … }
func New(opts Options) (*Pool, error)

// Options: Store、Locate func(id) (ns, error)（注册门：由
// projects+宿主实现，返回归属 ns 或 ErrNotFound/ErrConflict）、
// AgentFor func(ns) (*agent.Agent, error)、EvictGuard
// func(sessionID) bool（驱逐前的否决钩子：web 用它表达"还有 SSE
// 订阅者"）、IdleWindow。

func (p *Pool) Acquire(id string) (*Lease, error) // 懒打开 + 并发去重
type Lease struct{ … }
func (l *Lease) Release()                          // 引用归零 → 计入空闲时钟
func (l *Lease) HostData() any                     // OnOpened 的返回值（web 放 hub）
// Delete 的两个拒绝条件（与现状一致）：
//   会话运行中 → ErrRunActive；引用 > 0（在途请求/Lease）或有订阅者
//   → ErrInUse——订阅者在切分后不持 refs，"被观看"由 Options.
//   InUseGuard 回调表达（web 读自己的订阅计数）。
// 拒绝之外：置 closing、等在途打开排空、unlink、删存储。
func (p *Pool) Delete(id string) error
func (p *Pool) Close() error                       // 现有顺序：停清扫、排空、逐个关
```

**保留的全部语义**（逐条迁移，断言不变弱）：懒打开、同一 ID 并发打开
去重（opening 占位 + `<-inFlight` 重入）、closing 状态（delete 期间拒
绝打开、open 发布前复查）、空闲驱逐（清扫者 + EvictGuard）、
DeleteSession 等在途打开排空、Close 顺序（停清扫 → 等 opening 清空 →
逐个关闭）、**open 的 defer 占位清理**（panic 不楔死，P1 教训）。

**hub 的通知钩子**（hub 留在 web，但 Pool 的三个拆除点必须通知宿主）：

```go
type Options struct {
    …
    // OnEvicted/OnClosed 在拆除路径上回调宿主（先从 Pool 的表里
    // unlink，再回调 teardown——回调里的任何错误不影响拆除本身）。
    // 驱逐：清扫者命中空闲 + EvictGuard 放行 → unlink → 回调 → 关
    // Session。删除：排空在途打开 → unlink → 回调 → 删存储。
    // Close：全部 unlink → 逐个回调 → 关。
    // OnOpened 在 openSlow 里、会话发布之前同步调用（仍在 defer 占位
    // 保护内）：宿主在这里建好自己的伴随对象（web 建 hub——hub 的镜像
    // 播种必须先于任何客户端可见，阶段 A 的空白聊天视图 bug 正是播种
    // 晚了）。返回值挂在 Lease.HostData() 上，OnEvicted/OnClosed 回调
    // 时原样传回。返回错误：关闭会话、清占位、Acquire 返回该错误——
    // 不留下半开的条目。并发去重由 Pool 保证：同一 ID 的并发 Acquire
    // 只触发一次 OnOpened（占位机制天然覆盖——输家等赢家的结果而非
    // 重开）。
    OnOpened  func(sessionID string, sess *agent.Session) (hostData any, err error)
    OnEvicted func(sessionID string, sess *agent.Session, hostData any)
    OnClosed  func(sessionID string, sess *agent.Session, hostData any)
    // InUseGuard 与 EvictGuard 同构的宿主判断：删除路径上问"该会话
    // 是否被宿主持有"——web 用它表达"还有 SSE 订阅者"（切分后订阅者
    // 不再持 Lease refs，这条删除拒绝条件由回调承载）。
    InUseGuard func(sessionID string) bool
}
```

web 在 OnOpened 里 `NewHub(sess)`（HostData=hub），在 OnEvicted/
OnClosed 里 `hub.Stop()`（今天的四个触点 manager.go:191/541/729/
delete 路径一一对应）。**顺序：先 unlink 再 teardown**——回调期间该
id 已不在表内，新 Acquire 重新打开，不会与 teardown 竞争；closing
窗口语义（`TestSweeperEvictsIdleSession` 考的：驱逐进入 closing 后
拒绝新打开）保持。`Subscribers()` 的读取不需要进 Pool——web 自己就是
订阅宿主，EvictGuard 闭包里直接读自己的 hub 表。

**root lock（serve.lock）归 Pool**：`jsonl.AcquirePathLock` 的获取/
释放随构造/Close 进出 Pool——"同一 root 单进程"的保证跟着会话池走
（serve 的进程唯一性本质是会话池的唯一性）。CLI 不建 Pool，也就不占
root lock（现状一致）。

**错误类型化与稳定码逐条映射**（web 只做翻译；码表**新增三行**：
session_conflict、model_unavailable，以及 unknown_session 从纯文本
升为码——码值用下划线风格，与既有码一致）：

| 类型化错误 | HTTP | 稳定码 |
|---|---|---|
| ErrNotFound | 404 | `unknown_session`（现状纯文本，升为码；message 保留原文 "unknown session"） |
| ErrLocked（他进程持锁） | 409 | session_locked（现状即此） |
| ErrClosing | 503 | session_closing（现状即此） |
| ErrConflict（注册表多命中） | 409 | `session_conflict`（现状 409+纯文本，升为码；message 保留原文） |
| ErrModelUnavailable | 409 | model_unavailable（新） |
| ErrRunActive（删除时运行中） | 409 | run_active（现状即此，码值不变） |
| ErrInUse（删除时引用 > 0） | 409 | session_open（现状即此，码值不变） |

**响应体格式变更（N3）**："unknown session"与"session_conflict"从纯
文本升为码，会改变相应 404/409 响应体——`error` 字段由空变为有值、
`message` 保留原文。chat 的 api.ts 与 /log 的 app.js 均以 `error`
字段（非正文文本）判别错误（迁移时 grep 复核确认），不受影响；web 的
fixture 在步 3 证明响应体兼容。

**CLI 使用**：B1 不接（CLI 单会话直开 storage）；里程碑 C 的
`/sessions`、`/resume` 列表/恢复会经 Pool（列表数据 = Lister + 宿主
注册表过滤）。

### 15.4 迁移步骤（每步独立送审 + commit）

**能力 × 实现表**（web 测试大量用 memory store，能力面必须在迁移步 1
对齐，否则相应路径测不到）：

| 能力 | jsonl | memory |
|---|---|---|
| Lister（ListSessions） | ✓ | ✓ |
| Deleter（Delete） | ✓ | ✓ |
| Prober（HasSession） | ✓ | ✓ |

**CLI 与注册表**：CLI 直开 storage、**不经注册门**（现状语义，保留——
注册是 serve 的宿主概念）；web 的注册门由 `Locate` 回调（宿主用
projects 注册表 + Prober 实现）承担，未注册 ns 的会话对 web 404。
"多 ns 命中 → 409"的来源是 **web 的注册表探针**（对每个已注册项目
`HasSession(ns, id)`，多处命中即冲突），经 Pool 的 `Options.Locate`
回调浮出为类型化 `ErrConflict`——**jsonl 不含 `Locate`、不产生歧义
错误**（Q1=B 后该能力已整体移除）；步 1 只保留 web 这条 409 回归。

**SessionInfo 扩展**（C 的 /sessions 列表要显示模型，现在定字段避免
改接口）：`SessionInfo` 增 `Meta SessionMeta`（describeSession 已读
created 记录，加字段即得）。

| 步 | 内容 | 回归保护 | 文档同步 |
|---|---|---|---|
| 1 | Namespace + slog：Store 接口改造、能力接口、agent 去 ProjectID/OpenSessionIn、**删跨项目扫描 Open(id)**、jsonl logger、CLI -session//resume 只查当前 ns（Q1=B） | 全部现有测试改调用点后不改断言；storage 的扫描/歧义测试删除并由"X 目录 -session Y 会话 → 找不到且不触碰 Y 锁"替代；web 的多 ns 命中 409 测试保留（注册表探针路径不变） | multi-session.md（ns 术语）、README（恢复行为变更说明） |
| 2 | 按会话模型：session.created 增字段、链升 sai/v2/、SessionMeta/ModelResolver、agent.Options 瘦身、预算按会话、prompt 单点渲染、ErrModelUnavailable | **内联验收清单 a–g**（见下）；现有 provider 相关测试迁移 | design §5/§13（预算）、configuration.md、chain fixture、**webui.md（h₀ = SHA256("sai/v2/" + …) 链版本行同步）** |
| 3 | sessionhost 抽取：Pool/Lease/类型化错误/通知钩子/root lock、Manager 瘦身 | **点名承重测试逐个迁移且断言不变弱**：`TestSweeperEvictsIdleSession`（closing 窗口）、`TestDuplicateSessionAcrossProjectsIs409`（→ErrConflict，409 语义不变）、`TestOpenDuringCloseDiscards`、`TestManagerCloseIdempotent`、`TestSlowOpenDoesNotBlockOtherSessions`、`TestConcurrentOpenOpensOnce`、`TestFailedOpenClearsPlaceholder`、`TestDeleteDuringInFlightOpen`、**删除拒绝两条件**（运行中 → run_active、引用 > 0 → session_open；含切分后唯一会静默变语义的一条：**已连接 SSE 订阅者 + 无 Lease refs → 仍 409 session_open**，经 InUseGuard 承载）、**D2 两条新测试**（并发 Acquire 同一 ID 只触发一次 OnOpened；OnOpened 返回错误不留下占位与锁）；web 全部 fixture 不改预期 | multi-session.md §3（架构图）、webui.md |
| 4 | CLI /new 参数覆盖（`/new [-provider p] [-model m] [-reasoning lvl]`，默认继承启动参数；Q2=照办）**（已落地）** | /new 无参数 = 现行为；参数覆盖落到 session.created；**非法参数（未知 provider/model、不支持的推理强度、对无推理配置的模型给 -reasoning、未知 flag/多余参数/缺值）报错且不新建会话、保留当前会话**；省略项继承启动默认（非上一次 /new 的值）；校验经共享 ValidateSessionMeta 完成（错误带类型化前缀 the model is not available in the configuration + resolver 的参数/可用列表文案——C2 起 CLI 与 web 共用同一段文案；B1 的 not-available 措辞规则由 C2 的共享文案取代） | README、design §15.2 |

**步 2 的验收清单（a–g）**：

- a) 两个会话使用不同模型，请求打到各自的 provider；
- b) provider 实例被复用（同一个 provider 只构造一次）；
- c) 记录里的模型从配置中删除后，打开返回 ErrModelUnavailable，
  web 返回 model_unavailable；
- d) 预算按会话的模型计算；
- e) 跨越两次 run，system prompt 里的时间会刷新；
- f) 旧会话（没有 reasoning_level）能按默认值打开；
- g) chain_check 的 v2 用例：含 reasoning_level 的 session.created
  元组，Go/JS 跨实现折叠一致。

### 15.5 里程碑 C 的接口预留（只留位置，不实现）

- **新建会话传 SessionMeta**：`Pool`（或其 Options 的工厂）在 C 里加
  `Create(meta SessionMeta)`——sessionhost.Pool 的构造参数与 Lease 形
  状按"未来有 Create"设计（AgentFor 已含全部项目装配，Create 只差
  meta 的透传，不破坏现有接口）。
- **列出模型**：`ModelResolver.List()` 的接口位（15.2 已列）；数据源
  = sai.yaml 的 provider/model 表（含 reasoning 档位）。C 的
  `GET /api/models` 直接消费它。
- **归档**：storage 可选能力接口 `Archiver{ Archive(ns, id) error }`
  （B1 只留接口位，**存储方式不预设**——移动目录到 archive/ 子树会改
  变 `<root>/<ns>/<id>` 的路径不变量，ListSessions/Delete/HasSession
  都要跟着动，留到 C 的设计阶段定）；宿主经类型断言探测并决定 UI 暴
  露与否。**前置条件（写进接口注释）**：调用方必须持有 root lock 且
  该会话无在途 Lease——归档与追加互斥。

  **C 阶段 0 修订（§17.3）**：前置条件改为**会话自己的 OS 锁**
  （CLI 不建 Pool 不持 root lock；serve 打开同一会话时也持这把
  锁）——互斥机制见 §17.3。已按此落地，接口注释为 §17.3 版本。

### 15.6 产品决策（已定，opus chk2ajpl1j）

- **Q1 = 方案 B**：CLI 的 `-session`/`/resume` 只在当前目录对应项目
  的 ns 内查找，跨项目返回"找不到"（提示写明会话只能在其所属项目
  目录下恢复）。行为变更，README 与错误提示同步；Locator 能力与跨项目
  扫描 Open(id) 不引入/删除；"cwd 不一致"子问题随之消失。
- **Q2 = 照办**：`/new [-provider p] [-model m] [-reasoning lvl]` 三
  参数皆可覆盖，未给沿用启动参数；非法参数报错、不新建会话。

## 16. 里程碑 B2：Tool 接口、审批 Hooks、公共包

**消歧**：§14.2 里出现的"B2 定案"是里程碑 A 评审问题的编号标签，与本章
无关；本章全文用"里程碑 B2"指代本里程碑。B2 = B1 之后的"B 剩余"：
统一 Tool 接口、工具审批 Hooks、公共包移出 `internal/`。排序依据（用户
2026-10-14 决定）：C 的新代码直接落在最终包路径，免得二次搬运。

### 16.1 现状盘点

（本表是设计时刻的快照——**表内路径与拼写均为设计时刻**：步骤 1 后
`model.Tool` 均为 `model.ToolDefinition`，步骤 2 后 `internal/model`
已迁至顶层 `model/`；接口形状未变。）

**接口三件套（分散三处，语义互不认识）**：

| 位置 | 形状 | 角色 |
|---|---|---|
| `internal/model/model.go:88` | `model.Tool{Name, Description, InputSchema}` | provider 面向模型的**定义**（纯数据） |
| `internal/tools/shell.go:39` | `tools.Executor{Execute(ctx, args) (ToolResult, error)}` | **执行**（不含名称/描述） |
| `internal/agent/agent.go:22` | `agent.ToolExecutor{Execute(ctx, name, args)}` | agent 循环的**按名分发**口 |

装配链（`cmd/sai/main.go`）：`tools.Shell(cwd)` 返回
`(model.Tool, Executor)` 二元组 → CLI 用 `namedExecutor`（main.go:282，
单工具适配器）把 Executor 包成 agent.ToolExecutor → `Options{Tools:
[]model.Tool, ToolExecutor}` 两字段分别喂"定义"与"执行"。**定义与执行
的配对靠调用方自觉**：Tools 里列了名、ToolExecutor 按名找不到 →
运行期错误；注册第二个工具需要重写分发器。

**工具来源**：生产只有 `tools.Shell`（唯一内建）；原 page-provider
调试通道已删除（见 16.5）。

**tool call 生命周期**（§8 已述）：delta 累积在 Streaming 的
ToolCalls → assistant.message 落盘（wire ID）→ ToolStarted（mint result
ItemID、占 Running 槽）→ 执行 → ToolResult 落盘（同 ItemID upsert）。
审批 Hooks 挂在"Running 槽/result ItemID 建立之后、执行之前"与"执行之后"（16.3）。

**输出截断（两层，归属不同）**：工具层——shell 的 `limitedOutput`
（shell.go:164，执行期字节帽，防 OOM）；agent 层——
`agent/tool_output.go` 的 `limitToolResultOutput`（落盘前 2000 行 /
50KiB 预算，保尾）。两层都要留：前者是**工具自保**，后者是**记录预算**
（宿主策略）。

### 16.2 Tool 接口

```go
// internal/agent（迁移后 agent/）——接口定义在消费者侧（Go 惯例）：
// 编排核心声明契约，内建包实现它。
type Tool interface {
    // Definition is the provider-facing shape: name, description,
    // JSON schema. Returned by value; callers must not mutate maps.
    // (model.ToolDefinition per the step-1 rename — landed; 16.1's
    // inventory keeps model.Tool as the pre-rename spelling.)
    Definition() model.ToolDefinition
    // Execute runs one call. Contract:
    //  - Arguments are self-validated: an invalid argument returns a
    //    normal error (the agent loop turns it into a tool.result
    //    is_error record — same as today's shell argument errors).
    //  - ctx is the run's context: cancellation (user /stop, session
    //    close) must unblock Execute promptly.
    //  - Execute may block arbitrarily (network, subprocess); the
    //    agent loop calls it synchronously in the iteration.
    Execute(ctx context.Context, arguments map[string]any) (model.ToolResult, error)
}
```

**接口归属与依赖方向**：接口定义在 **agent**（消费者侧，Go 惯例）；
`tools` 是内建实现与适配器的**扩展包**，依赖方向 `tools → agent →
(model, storage)`——agent 不 import tools（编排核心不依赖"内建工具
集"这个叶子；第三方自定义工具只 import agent 即可实现接口，不必为
一个接口拖进内建包）。

**注册**：`agent.Options.Tools []agent.Tool` 取代
`Tools []model.Tool + ToolExecutor` 两字段。agent 循环按
`Definition().Name` 分发（等价于今天的 namedExecutor，内化为循环的一
步；今天"定义与执行不配对"的失败形态本就是 tool.result + is_error
（executeToolCall 的 toolErrorResult），与新接口的未知工具路径同形）。
**迁移适配**：`tools.Named(def model.ToolDefinition, exec Executor)
agent.Tool` 适配旧二元组（第三方按名拼接的工具一步迁移）；`Shell(cwd)`
改为直接返回 `agent.Tool`（签名破坏性变更，v0 无稳定性承诺，16.4）。

**参数校验归属**：**工具自校验**（Execute 内），不引入 schema 校验层。
理由：内建零外部依赖纪律（JSON-schema 校验器是新依赖）；现状即自校验
（shell.go 的 requiredStringArgument/optionalPositiveIntArgument）；
schema 是给**模型**看的说明，不是运行期断言。契约写进接口注释。

**并发与取消**：每次 tool call 同步执行（iteration 内顺序，现状）；
ctx 语义如上；不引入工具级并发（多 tool_calls 并行执行留给将来，接口
形状已兼容——Execute 独立无共享状态）。

**截断归属**：agent 层 `limitToolResultOutput` **留在 agent**（记录预
算是宿主/agent 策略，工具不该知道记录预算）；工具层的自我保护（如
shell 的字节帽）是工具实现细节。接口不暴露截断参数。

### 16.3 审批 Hooks

**机制总览**：策略钩子**同步、不阻塞**（Allow / Deny / Ask 三态）；
`Ask` 时**待审批状态由 agent 拥有**——在 `s.mu` 下与
`ToolApprovalPending` 事件同一临界区置快照字段，宿主经会话方法
`ResolveApproval` 给出决定，等待受 run ctx 约束。宿主不建审批状态机
（没有 map+channel 要自造），web 只需一个 HTTP 端点。

```go
// internal/agent（迁移后 agent/）
type Hooks struct {
    // BeforeToolCall is a synchronous POLICY hook: it decides, never
    // blocks. Returning Ask hands the decision to the session's
    // approval flow (the run goroutine waits, bounded by run ctx).
    // nil hook = Allow (the default: no behavior change).
    // A panic is fail-closed: treated as Deny with the reason noted.
    BeforeToolCall func(ctx context.Context, call ToolCallInfo) ToolDecision
    // AfterToolCall observes the finished call (telemetry, audit).
    // Boundaries: runs for executed calls AND for denials (the
    // observer sees the same is_error result the model sees); does
    // NOT run for cancelled calls (no result object exists); never
    // runs at process exit. A panic inside the hook is recovered and
    // silently discarded (the agent holds no logger); hosts that need
    // telemetry handle their own errors inside the hook.
    AfterToolCall func(ctx context.Context, call ToolCallInfo, result model.ToolResult)
}

type ToolCallInfo struct {
    SessionID string // 审批卡按会话路由（宿主一 agent 多会话）
    RunID     string
    ItemID    string // the tool RESULT item id (approval identity)
    Name      string
    Arguments string // raw JSON (policies parse as needed)
}

type ToolDecision struct {
    Action ToolAction // Allow / Deny / Ask
    Reason string     // Deny: surfaces as the tool.result error content
}
```

**挂点**：仅工具级（Running 槽/result ItemID 建立之后、执行之前的策略 + 执行后的观察；策略 Deny 不发 ToolStarted——未执行的调用不发执行开始事件）。**run 级挂点不提供**
（B2）：run 生命周期已在会话流上（Status 事件），宿主经 `Stream()`
已可观察；若将来 C 的宿主需要 run 级**干预**再议。

**Rewrite 不进 B2**：参数改写被明确排除——落盘的 assistant.message
记录携带的是**原始** tool_call 参数，改写后"实际执行了什么"在磁盘上
无法还原，审计留痕需要新的记录/事件形状（事件带 original/effective
或 tool.result 带实际参数摘要）。等有真实用例再连同留痕设计一起加，
避免留一个实现者各自发明的未定义动作值。

**deny 语义**（策略 deny 与人工 deny 同形）：**作为 tool.result 回给
模型**（is_error + Reason 作 content），**不终止 run**。理由：模型
看到"被拒绝及原因"可调整策略（换命令/放弃），这正是审批的产品语
义；记录面零新形状（tool.result 记录照常、seq/RecordCommitted 流不
变）；终止 run 是 /stop 与硬错误的职责，混入审批会把"用户拒绝一次
操作"放大成"报废整轮"。策略钩子 panic → **fail-closed**：按 deny
处理，Reason 注明 "approval hook panicked"（审批钩子失效时放行比拒
绝危险）。

**Ask：agent 拥有的待审批状态**：
- **置位**：策略返回 Ask → run goroutine 在 `s.mu` 内发
  `ToolApprovalPending{ItemID, Name, Arguments}` 并置快照工具槽的
  `AwaitingApproval`（同一临界区——事件与快照原子一致），然后在内部
  决定 channel 上等待。
- **决定**：`Session.ResolveApproval(itemID string, allow bool,
  reason string) error`——web 一个端点 `POST /api/s/{id}/approval`
  打到它（与其它路由同样过 guardAPI：POST 的 CSRF/Origin/Host 守卫；
  经既有会话解析取会话——会话已关/被驱逐/未知时先给出既有的
  `session_closing`/`unknown_session` 码，客户端按"先会话级码、再审批
  级码"判别）。类型化错误与码表归属（multi-session.md §4 错误码全集
  +2 行）：`ErrApprovalUnknown` → 404 `approval_unknown`（无此待审批
  项——可能已落地或从未存在）、`ErrApprovalResolved` → 409
  `approval_resolved`（已被回答）。Deny 的 reason 走 tool.result（同
  策略 deny）。
- **解决事件**：`ToolApprovalResolved{ItemID, Outcome}`（approved /
  denied / cancelled），在决定生效的同一临界区发出。
- **ctx**：等待受 run ctx 约束——`/stop`、session Close 取消等待，
  Outcome=cancelled；等待期间 run 占用单 run 槽（Running）。策略
  Allow/Deny **不产生任何事件**（秒批不闪审批卡：只有 Ask 走事件与
  快照位）。
- **run 超时**：现状无 run 级超时；B2 不引入——等待审批的 run 可无
 限期挂起，由用户/宿主经 ctx 终结（审批等待本质是人的节奏）。
- **快照恢复**（§14.2 快照字段规则）：重连客户端从快照工具槽的
  `AwaitingApproval`（连同 ToolCall 字段）重建审批卡，后续 Resolved
  事件收尾。web 的 `ToolWire` 增加 `awaiting_approval` 字段——**链版
  本不升**（digest 链只折叠**记录**，帧不参与哈希，sai/v2/ 不动）；
  chat 的 store_check 加一例；/log 的 reducer 不渲染工具槽，不涉及。

**取消时的记录形态（与中断规则的关系）**：审批等待被取消 = 一次中
断，走**既有**中断语义（session.go 的工具循环在 ctx 取消时直接返
回，语义为"中断不留收尾记录"）——不落 tool.result；落盘的是 assistant.message 里的 tool_calls
（ToolStarted 是瞬态事件不落盘，占位是内存 Running 槽）。**未配对的
tool_call 由 `sanitizeContext` 在下一次请求组装时修补**（agent.go:
"every assistant tool call is followed by its tool result …
synthesized as interrupted errors in call order"，有
TestSanitizeContextPatchesDanglingToolCalls /
TestSanitizeContextKeepsCompleteExchanges 双测试钉住）——今天
/stop 打断执行中工具已是这条路，审批把窗口从毫秒拉长到分钟，机制不
变。占位文案是否区分"审批等待被取消"与"执行被中断"（现在是统一的
`[tool execution interrupted]`）留作实现期小决定，不改变契约。

**默认行为与判据**：`Hooks{}` 零值（或 Options 不设）= 全部放行 =
现状不变。**可执行判据**（步骤 7a 的回归保护）：现有 agent/web/CLI
测试全部不改断言通过，**另加一条无钩子场景的事件序列 golden 测试**
（同一脚本化场景，记录序列与事件序列的类型 + 关键字段逐条对比现
状捕获的基线）——"不变"由测试证明，不靠声明。

### 16.4 公共包移出 internal/

**逐包裁定**：

| 包 | 处置 | 理由 |
|---|---|---|
| `model`（+ 各 provider 子包） | **公开** | wire 类型与 provider 抽象是 SDK 的第一表面 |
| `storage`（+ jsonl、memory） | **公开** | 会话存储格式（log.jsonl/段切换/链）是可复用契约 |
| `tools` | **公开** | Tool 接口与内建 shell 是扩展点 |
| `agent`（含 SessionEvent/Stream） | **公开** | Agent/Session/Run/Stream/Hooks 是编排核心。SessionEvent 留在 agent 随包公开（§14.8"一起搬"=同一时机）：sessionhost import agent，事件只有留在 agent 才不成环 |
| `sessionhost` | **公开** | 宿主（CLI/serve/第三方）构建会话池的接口层 |
| `config` | 留 internal | sai.yaml 是**应用**配置不是 SDK；宿主自带 resolver 即可（agent.ModelResolver 是接口） |
| `projects` | 留 internal | serve 的注册表概念（宿主细节） |
| `web` | 留 internal | serve 应用本体；C 会重塑（新建会话 UI），过早公开会冻结错误形状 |
| `codexauth` | 留 internal | 特定 provider 的认证细节 |

**目标路径**：**顶层** `github.com/rexzhao/simple-agent/<pkg>`（不用
`pkg/` 目录）。理由：Go 社区主流（标准库、多数库优先）；`pkg/` 无编译
语义、徒增一层；本仓库 `cmd/sai` 已是顶层惯例。

**公开前必须收口**（阻塞项，随迁移各步完成）：
- `agent.SessionMetaForTest`：删除，测试改用字面量（待办在案）。
- `agent.NewStaticResolver`：**保留**——单模型宿主是合法用例，补文档
  注释（"生产配置解析请自建 ModelResolver"）；当前零宿主使用，注明
  "等第一个真实宿主出现再定形"。
- `sessionhost.SweeperInterval`/`SessionIdleEvict` 包级可变变量：
  收为 `Options.SweepInterval`/`IdleWindow`（已存在 IdleWindow），
  变量删除——公开包的可变全局是数据竞争表面。测试改走 Options。
  **（已落地）**：变量删除为 `defaultIdleWindow`/`defaultSweepInterval`
  包内常量，零值 Options 回落默认；web 经 ManagerOptions 透传
  （IdleWindow/SweepInterval），驱逐测试经 Options 注入。
- `web.Manager.agentFactory`（测试专用导出）：随 web 留 internal，待办
  不变。
- 命名收口：`agent.Tool`（接口）与 `model.Tool`（定义结构）跨包同名
  在 Go 里合法，但编排代码里两者高频同文件出现，读混是真实的维护隐患
  ——**`model.Tool` 改名 `model.ToolDefinition`**，让定义结构的角色
  自描述（wire 不可见——它只进请求组装，不落盘不上面；改动是纯 Go
  API 破坏，v0 可接受；opus 已裁定：做，公开前做代价最低）。

**兼容承诺**：v0——**无任何稳定性保证**，破坏性变更不预告；README 与
各公开包 doc.go 写明"v1 前随时变"。不设 `stable` 标记、不发 v1（用户
明确后再议）。

**迁移方式**：`git mv` 保历史；**一包一 commit**（依赖序：
`model → storage(+jsonl,memory) → tools → agent → sessionhost`，每步
全仓导入路径重写后 build+test 全绿才进下一步）。导入重写用**机械文本
替换**（sed 或等价脚本——`gofmt -r` 作用于表达式模式，不是可靠的
import 重写工具；具体手段在步骤 2 实测后固定）；验证 = build/vet/
test 全绿 + 全仓 grep 旧路径为零。

**公开包对 internal 的依赖**：Go 的 internal 规则允许**同模块内**的
公开包 import internal 包（外部使用者无感——只是该依赖对他们不可替
换）。现状盘点：五个将公开包之间只有相互依赖（model/storage/tools/
agent/sessionhost），**无**指向 codexauth/config/projects/web 的边（**非测试**代码，已
grep 核实；测试文件中的此类边合法且不影响发布）；若将来出现此类边（如某 provider 需要 codexauth），
review 必须显式标注——它是对外隐藏的实现依赖。`clients/chat` 对 `internal/web/static/chain.mjs` 的相对导入不受
影响（web 留 internal，同模块内合法）；`go:embed` 的
`internal/web/static` 路径不变。

### 16.5 页面工具提供者 / broker

**已删除（B2 步骤 8）**。page-provider（"页面当 provider"的调试通道）
整体移除：clients/chat 的 pageProvider.ts、App.tsx 的接线与横幅、
page_provider_check fixture 与对应 Go 测试、README 的调试章节。理由：
它是一条**未鉴权的 LAN 通路**且可经 page_send 触达 agent 的 shell 工具
（安全面）；Tool 接口与审批 Hooks（§16.2/§16.3）取代了它的调试价值。
如需恢复，revert 该提交即可。

### 16.6 待用户拍板的问题

- **(a) 审批交互 UI 的范围**：
  - 选项 1：B2 交付策略钩子 + agent 侧审批状态机 + ResolveApproval
    （web 侧只需一个 HTTP 端点即可接通，审批卡 UI 留 C 增量做）；CLI
    确认提示同样留后续；
  - 选项 2：Hooks + CLI 的 Y/n 提示（runLoop 中途读 stdin，渲染循环需
    小改）；web 卡留 C；
  - 选项 3：Hooks + CLI + web 审批卡全做。
  - **推荐 1**：接口与事件语义是难且易错的部分（快照恢复、取消交
    互、fail-closed），先把契约钉死；UI 是增量，且 web 审批卡与 C 的
    页面工作天然同批。选项 2 的 CLI 提示可在 B2 内加做（工程量小、
    独立送审）。
- **(b) 公开范围与路径布局**：推荐 16.4 表（五个包公开：model/
  storage/tools/agent/sessionhost；config/projects/web/codexauth 留
  internal）+ 顶层布局。可收紧（只公开 agent+model）或放宽（加
  config）——收紧的代价是 C 的宿主代码仍走 internal 路径。
- **(c) broker/page-provider 处置**：推荐 B2 末尾删除（16.5）；若仍在
  用则原样保留、C 前再议。

### 16.7 迁移步骤（每步独立送审 + commit）

| 步骤 | 内容 | 回归保护 | doc-sync |
|---|---|---|---|
| 1 | `model.Tool → model.ToolDefinition` 改名（纯 Go API，wire 零变化）**（已落地）** | 全仓 build/test；grep 旧名清零 | development.md 若提及 |
| 2 | 包迁移·model（含 provider 子包）：git mv + 导入重写 + 收口注释**（已落地）** | 每步后全绿；旧路径 grep 为零；`go list ./...` 无 internal/model | README 包布局段 |
| 3 | 包迁移·storage（jsonl/memory 同批）+ **doc.go 写明公开契约**：log.jsonl 记录格式、段切换（.next/归档/恢复规则）、sai/v2/ 摘要链——公开即契约，内部注释不够**（已落地）** | 同上 | storage 包文档、development.md 格式附录（若引用） |
| 4 | 包迁移·tools + Tool 接口落地（接口在 `agent.Tool`（internal/agent，随步骤 5 公开）；`tools.Named` 适配、`Shell` 返回 `agent.Tool`、`agent.Options.Tools []agent.Tool`、namedExecutor 删除）**（已落地）** | 既有 agent/web/CLI 测试不改断言；接口 kill（按名分发丢失→运行期错误测试红） | README；design §8 已核无需改动 |
| 5 | 包迁移·agent（SessionEvent/Stream 随包公开；SessionMetaForTest 删除；NewStaticResolver 文档化）**（已落地）** | 测试改字面量后全绿；公开 API 清单核对 | 同上 |
| 6 | 包迁移·sessionhost（SweeperInterval/SessionIdleEvict 收为 Options，变量删除，测试改 Options）**（已落地）** | 驱逐/清扫测试改注入后断言不变弱；可变变量 grep 清零 | multi-session.md §3 |
| 7a | 审批状态机（**已落地**）：`Ask` + agent 拥有的待审批态（`s.mu` 内事件+快照原子置位）+ `Session.ResolveApproval` + `ErrApprovalUnknown/Resolved` + `ToolApprovalPending/Resolved` 事件 + 快照 `AwaitingApproval` + web `ToolWire.awaiting_approval`（一个端点）+ fixture 两例 | kill：快照丢待审批态（重连看不到卡）、ResolveApproval 双答/未知不报错、事件与快照不同锁段（乱序）；取消走中断路径（sanitizeContext 修补，既有双测试） | webui.md 事件表 + 快照字段、multi-session.md §4 码表 +2 行（approval_unknown/approval_resolved） |
| 7b | 策略钩子：`agent.Hooks`（同步三态）+ deny-as-result + fail-closed + panic 防护 + AfterToolCall 边界（deny 跑、cancel 不跑、退出不跑） | **默认基线判据**：现有测试不改断言 + 无钩子事件序列 golden 测试（§16.3）；kill：deny 不落记录、钩子 panic 楔死、默认行为变 | 同上（码表不动） |
| 8 | broker 处置（按 16.6c：删除 pageProvider + fixture + 文档段，或原样保留）**（已落地：删除）** | 若删：chat tsc + 剩余 fixture 全绿；grep page_provider 清零 | webui.md、chat README |

步骤 2-6 每步一包是 review 粒度考量；若 review 负担可承受，2+3、
4 紧邻步骤可合并送审（commit 仍分开）。

### 16.8 与里程碑 C 的关系

- **C 依赖 B2**：新代码落最终包路径（公开五包）；web 新建会话弹窗若
  需要审批卡，消费步骤 7 的事件与快照字段；CLI `/sessions` 走公开的
  sessionhost/agent API。
- **C 不依赖 B2**：Archiver 能力（storage 可选接口）、项目注册管理
  （internal/projects 的宿主概念）、归档——纯 B1 基础即可做。
- B2 不动 web 的会话面（新建/删除/列表 API 属 C），只动 Tool/Hooks
  与包路径；web 留 internal 正是给 C 留重塑空间。

## 17. 里程碑 C：项目与会话管理

**消歧**：本章是路线图的第三里程碑（A=会话流重构、B1=Namespace/
按会话模型/sessionhost、B2=Tool 接口/审批/公共包）。C 围绕**项目与
会话的管理面**：新建、列表、归档、注册、删除的补全。

### 17.1 现状盘点

| 能力 | 现状 | 缺口 |
|---|---|---|
| 新建会话 | CLI `/new`（B1 步 4）；web 无 | web 端点 + 对话框 |
| 模型列表 | `ModelResolver.List()` 接口位 + `ModelDescriptor{Provider, Model, ReasoningLevels}`（agent/resolver.go:46）；config resolver 的 List 已实现（`reasoningLevelNames`） | 无消费方；无测试；无默认标记 |
| 会话列表 | `Store.ListSessions(ns)`（jsonl 按 mtime 排序；`readSessionHead` 从**最早段**取 title/created/meta）| web /api/projects 已消费；CLI 无 |
| 归档 | 无（§15.5 只留 `Archiver` 接口位 + 前置条件） | 存储方式未定；CLI/web 全缺 |
| 删除 | web DELETE /api/s/{id}（B1）；chat onDelete 未接 | chat 二次确认 |
| 项目注册 | CLI `sai project add`；web 只读 projects.json（stat-gated 缓存，Manager.registryEntries） | web 注册/注销端点；注册表并发写 |
| 标题 | `readSessionHead`：最早段 records[0]=session.created 且 records[1]≠compacted 时扫首条 user.input | 用户报告"压缩后标题变空"——**规则冲突，非单纯 bug**（见下方实测基线：A 形态该分支不可达） |
| /resume | `strings.HasPrefix(text, "/resume")`（B2 待办：/resumefoo 误匹配） | 精确匹配收紧 |

**标题问题（设计点 6，实测基线 + 规则冲突）**：
**C1 结论（用户裁定，2026-10-15）：维持现有规则，不追查；压缩后
标题变空的 B 形态是既有规则的预期行为。**
用户报告"压缩后标题
变空"。这**不是单纯的 bug，而是用户报告与既有书面规则的冲突**——
multi-session.md（§ Title 规则）明文规定"若最早段前两行是
created+compacted → Title 留空；**绝不取中途输入当标题——标题撒谎
比留空更糟**"。

**实测基线**（review 用真实 Append 路径的 overlay 探针，三种状态）：
- **A. 正常压缩**（created→input→compact→input）：归档段
  `[created, input]`，活跃段 `[created, compacted, input]`——
  **标题非空**（"FIRST"）✓；`records[1]==compacted` 的提前返回
  **不可达**（最早段是归档，不含尾随 compacted）。
- **B. 全部压缩前归档不在磁盘**（带外删除/清理）：活跃段
  `[created, compacted, input]`——标题空（**既有规则的预期行为**）；
  此时放宽扫描得到的"later"**不是会话最早输入**——正是规则说的
  "撒谎"。
- **C. 最早归档第二行 torn**：`[created, <坏行>]`——标题静默空
  （jsonl 从首坏行截断的既有语义）；与 `records[1]==compacted`
  无关，**放宽扫描修不了**。

仓库内**无任何清理归档的代码**（grep prune/Remove 零命中）⟹ B/C
只能来自人工/外部工具/旧版本遗留。**chat 的标题必然来自
SessionInfo.Title**（两处 `s.title || "untitled"`，源是服务端快照）
——客户端路径假设已否决。**agent 层三条真实压缩路径均保留标题**
（agent/title_compaction_test.go：手动 Compact、自动 pre-turn 压缩、
压缩后重开追加——全部绿；K2 变异段切换不归档使手动路径红）。

**C1 的纪律**：第一项是**经真实压缩路径的复现测试**（A 形态）——
若绿（实测预期绿），则需拿到用户那条会话的真实磁盘形态（目录里的
log-*.jsonl 清单）：归档齐全 → 报告另有来源（旧版本段布局，未验
证）；归档缺失 → 命中 B 或 C。**B 是既有规则的行为**：要不要改规则
（"撒谎比留空更糟"→"最早可读输入"）是产品决策，交用户；C 是截断
语义的固有行为，修复方向另议（或接受）。**不按推测改扫描语义**。

### 17.2 新建会话

**服务端**：`sessionhost.Pool.Create(ns, projectPath string, resolver agent.ModelResolver, meta storage.SessionMeta) (*Lease, error)`（§15.5 预留落地）：
- 校验 meta 经**共享校验函数**（`agent.ValidateSessionMeta(resolver, meta)`——B1 待办"level 校验下沉"在此落地）：provider/model 必须解析、reasoning_level 必须是该模型支持的档位（空=默认）。CLI `/new` 的 `newMetaValidator` 与 web 都调它，非法输入得到同一份错误。
- 校验通过 → `agent.NewSession(ns, meta)`（惰性 header：磁盘无目录，首条 append 才落盘——现状不变）。
- **发布进 Pool 表**：Create 走与 **Acquire 慢路径**同款的发布序列——openSlow（OnOpened 钩子：建 hub，宿主伴随对象先于任何客户端可见；失败 → 关会话 + 返回错误）→ `s.mu` 内置表（refs=1）→ defer 占位清理（不留半开条目）。之后 `GET /api/s/{id}/stream` 走表内**快路径**（不经 Locate——未落盘会话没有目录，`HasSession` 探针找不到它，Locate 必然 404；表内命中是它唯一的服务端入口）。
- **ID 冲突**：NewSession mint 的新 ID 与表内/磁盘冲突概率为零（26 字符随机）；仍加一行守卫（表内已有该 id → 返回错误）——零概率一旦发生就是覆盖活条目，一行守卫比事后排查便宜。
- **Pool 关闭**：`p.closed` 时 Create 返回 ErrClosing（与 Acquire 同款）。
- **未落盘会话的生命周期**：表内条目受空闲驱逐管辖——refs 归零且无订阅者（EvictGuard）后，清扫者照常拆除：**拆除即永久丢失**（磁盘无目录，重启后同样不可恢复；`#/s/<id>` 返回 unknown_session）。**刷新在空闲窗口内可回到**（表内快路径），**超时或重启后回不去**——这是惰性 header + 驱逐的真实代价，不是"只能靠 #/s/<id>"那么温和。
- **列表可见性**：惰性 header 意味着 `ListSessions` 看不见未落盘会话（jsonl 的 `listSession` 要求 hasSegment）。这是现状语义（CLI /new 的会话在首条消息前也不在 /api/projects 里），**保持**：对话框不含首条消息（用户拍板），新会话在侧栏显示靠**客户端本地状态**（创建返回的 ID 直接导航），服务端列表要等首条消息落盘。

**端点**：`POST /api/projects/{pid}/sessions`
- body（严格：未知字段 → 400 `unexpected_field`）：`{"provider": "p", "model": "m", "reasoning_level": ""}`；provider/model 必填（客户端默认填充后提交），reasoning_level 可选。
- 201 + `{"id": "<sid>", "ns": "<pid>"}`；客户端用返回的 id 导航（`#/s/<id>`）——**无服务端切换逻辑**（用户拍板）。
- 错误码：`model_unavailable`（409，校验失败——理由：客户端只从 GET /api/models 选择，提交失配意味着**配置在选取与提交之间变了**；reasoning_level 不在该模型档位时同样返回此码）；`unknown_project`（404，**新码**——项目不在注册表）；`invalid_input`（400，字段缺失/类型错/未知字段）。

**GET /api/models**：`[{"provider": "p", "model": "m", "reasoning_levels": ["low","high"], "default": bool}]`——`ModelDescriptor` 增 `Default bool`（config resolver 的 List 填充：default_provider+default_model 组合为 true）。`List()` 需测试（B1 待办）：pin 顺序稳定（按 provider 名、model 名排序）。

### 17.3 归档

**存储方案对比**：

| 维度 | A：标记文件（`<id>/archived`） | B：目录移动（`<ns>/archive/<id>`） |
|---|---|---|
| 路径不变量 | **不变**——`<root>/<ns>/<id>` 寻址全保留 | 破坏：ListSessions/Delete/HasSession/Acquire 全部要识别两棵树 |
| ListSessions | 每目录多一次 stat（标记存在性）——在已有的 ReadDir 循环里是同一次 IO 的附带查询 | 同一次 ReadDir（扫两个父目录） |
| 崩溃一致性 | 两态（标记在/不在），`os.Create`+close 即可，无需原子重命名 | 两个目录的 rename 链，中断恢复复杂（复用段切换的模式但作用在目录级） |
| 锁 | 会话锁路径不变（锁文件仍在 `<id>/` 内） | 锁文件跟着移动——移动期间锁语义中断 |
| 外部工具（grep/备份） | 会话目录结构不变，直觉一致 | 归档会话"消失"到另一棵子树 |
| 与段切换的交互 | 无（标记文件不参与段命名） | 归档目录里的段切换路径要重推 |

**推荐 A（标记文件）**——与 opus 倾向一致，理由如上表；代价（列表多
一次 stat）在 ReadDir 循环内可忽略。

**接口**：`storage.Archiver`（§15.5 位置）：
```go
// Archiver is the optional archive capability. Archive/Unarchive
// acquire the SESSION's own OS lock (the same one Open takes); no
// root lock is required (the CLI never builds a Pool and cannot hold
// it). The web host goes through Pool.Archive, which additionally
// guarantees no in-flight Lease/subscribers before calling in; the
// CLI calls the Store's Archiver directly.
type Archiver interface {
    Archive(ns, id string) error   // session lock held; creates the marker
    Unarchive(ns, id string) error // session lock held; removes the marker
    IsArchived(ns, id string) (bool, error)
}
```
jsonl 与 memory 都实现。`SessionInfo` 增 `Archived bool`（ListSessions
填充；默认过滤归档项，`ListSessionsOpts{IncludeArchived bool}` 或
`ListSessionsAll`——接口形状实现期定）。

**互斥**（设计点 4，修订：跨进程互斥靠**会话自己的 OS 锁**，不靠
root lock——CLI 不建 Pool 不持 root lock，而 serve 可能正打开着同
一会话）：
- **Archiver 的实现**：Archive/Unarchive **获取该会话的 OS 锁**（与
  Open 同一把锁：被其它进程持有 → LockHeldError → web 409
  `session_locked`）。标记的写/删在锁内完成。
- **打开与归档的 TOCTOU 消除**：标记检查放在**会话锁内**——
  jsonl 的 `openDir` 在拿到锁后检查 `archived` 标记（存在 →
  `storage.ErrArchived`，**新类型化错误**，agent 透传）。归档方
  （持锁写标记）与打开方（持锁查标记）串行化在同一把锁上，窗口不
  存在。memory 实现同语义（锁退化为进程内互斥）。
- **Pool.Archive（web 宿主路径）与 Delete 同构**：运行中 →
  ErrRunActive；refs>0 或有订阅者（InUseGuard）→ ErrInUse——**不
  强制驱逐用户正在看的会话**；空闲 → 置 closing 占位（defer 清理，
  与 Delete 同款）→ 池内 teardown → 获取会话锁 → 写标记 → 清占位。
  占位期间 Acquire 得 ErrClosing。Unarchive 经 Pool 时靠**会话锁**
  拒绝：被任何进程（含本池——池内条目存活期间持有该会话的 OS 锁）
  打开着的会话 → ErrLocked / 409 `session_locked`；归档会话不在池
  里，空闲时直接锁内删标记（落地即此口径，不加 InUseGuard）。
- **CLI 路径**：`/archive` 直接经 Store 的 Archiver（获取会话锁 →
  写标记）——CLI 与 serve 各自持锁串行，跨进程安全；serve 若已打
  开该会话（持锁中）→ CLI 得 session_locked。
- **归档会话的打开/resume**：**拒绝打开**（ErrArchived → web 409
  `session_archived`，CLI 提示"先 /unarchive"）。追加同拒（追加
  必经打开）。删除允许（归档≠保护，删除路径先删标记再删目录——
  或直接删目录带标记）。驱逐不会遇到归档会话（打不开就不在池
  里）。

**CLI**：`/archive <序号|前缀>`、`/unarchive <同>`、`/sessions --all`。
**web**：`POST /api/s/{id}/archive`、`DELETE /api/s/{id}/archive`、
侧栏归档区（默认折叠，展开列出归档项 + 恢复按钮）。

### 17.4 项目注册/注销

**端点**：
- `POST /api/projects` body `{"path": "..."}`——路径必须存在且为目录
  （400 `invalid_path`）；**路径规范化用既有导出函数
  `projects.Canonical(path)`**（Abs + Clean + windows/darwin 平台的
  ToLower——与 ProjectID 的哈希前规范化完全一致；CLI 的 project add
  与注册表判重已在用它，自造 Abs+Clean 会产生漂移）；已注册（同规范
  路径）→ 幂等 200；注册表写失败 → 500。
- `DELETE /api/projects/{pid}`——pid 必须在注册表（404
  `unknown_project`）；**有已打开会话 → 409 `project_in_use`**。**与
  打开的竞态**：顺序为①注册表锁下写入（移除该条目）→ ②之后的
  Acquire 经 Locate（读注册表）自然拒绝新打开（unknown_session）；
  ③已在途的打开：openSlow 的 Locate 在锁外读注册表——若读到旧表
  （注销前快照），打开完成并进入池；**接受这个窗口**（已在途的打开
  是合法的旧世界请求；该会话照常工作，驱逐后不可再开）——文档写明
  而非消灭。判定来源：写注册表**前**问 Pool "该 ns 是否有打开会话"
  （此刻快照 + Locate 门拒绝后来者 = 足够窄）。无会话但有归档/磁盘
  目录 → **允许注销，数据不删**（用户拍板"不删除数据"）。

**注册表文件锁**：**复用 jsonl.AcquirePathLock**（锁
`<root>/projects.json.lock`）——同原语、同语义（进程级独占），写路径
`读 → 修改 → 写临时文件 → rename → 释放锁`。CLI 的 `sai project add`
同样走锁（现状是 CLI 独占运行所以无并发，加了锁两端对称）。
**锁竞争**：AcquirePathLock 是非阻塞的（被持有即 LockHeldError）。
两个并发 POST /api/projects：**短重试**（100ms/2ms 有界，与 readLockHolder 的重试同形——非复
用那个函数）后仍被持 → 409 `registry_locked`（**新码**：
注册表正被另一写操作持有，瞬时态、可重试）。CLI 同策略。

**缓存失效**：Manager 的 stat-gated 缓存（mtime+size）在注册表被
rename 后自动失效（新文件 mtime 不同）——**无需额外失效逻辑**。极限
情形（同尺寸写入落在同一 mtime tick）沿用 multi-session.md §3 的既有
前提："下一次写即自愈"，满足"最迟下一次请求"。

### 17.5 CLI /sessions 与 /resume

**/sessions 输出**（当前项目范围，Q1=B）：
```
 1  s0000389abc…  修复登录超时            fake/fake-reasoner  10-14 12:03
 2  s0000389def…  （无标题）              fake/fake-model     10-13 18:22
 A  s0000389ghi…  旧会话                  fake/fake-model     09-30 09:00
```
- 序号 = **列表位置的 1-based 下标**（ListSessions 的 mtime 降序）；
  **不稳定**（两次命令之间新增/归档会漂移）——**文档写明**，/resume
  的序号匹配以"当次 /sessions 输出"为准；ID 前缀是稳定寻址。
- 归档项：默认隐藏；`--all` 显示（标记 `A`）。
- 模型列：SessionInfo.Meta（provider/model）。

**/resume 匹配**：
- 参数形态：纯数字 → 序号（取当次 ListSessions 的第 N 项，范围外报
  错列出可用范围）；其它 → ID 前缀（唯一命中 → 打开；零命中 → "找
  不到"；多命中 → 列出全部候选，要求补全长前缀）。
- **命令匹配收紧**（B2 待办）：`text == "/resume"` 与
  `strings.HasPrefix(text, "/resume ")`——`/resumefoo` 落 unknown
  command（与 /new 同款 + stderr 断言测试）。

**C7 落地的两个补白**（实现期定）：
- **序号基准**：`/resume <n>` 与 `/archive <n>` 的序号索引**未归档**
  行（两种视图下同基准，无歧义）；`/unarchive <n>` 的序号索引
  **归档**行（`--all` 中标 A 的行按 mtime 降序）——live 序号对恢复
  操作无意义。ID 前缀对三个命令都匹配**全集**（含归档行，故
  `/resume` 对归档命中能提示"先 /unarchive"而非"找不到"）。
- **归档当前会话**：**拒绝**（"this is the current session — pick
  another, or /quit first"），不自动切换。理由：当前会话的 OS 锁
  由本进程持有，标记写不进去（锁决定了必须先关闭）；且显式拒绝
  优于把用户静默丢进一个他没要的新会话。

### 17.6 chat 客户端

- **新建对话框**：侧栏"+"按钮与输入框 `/new` 均打开；项目下拉（默认
  当前）、模型下拉（GET /api/models，默认标记项）、推理强度（所选模
  型 ReasoningLevels 非空才显示）；提交 → POST → 用返回 id 导航。
- **归档区**：侧栏底部折叠区；归档/恢复按钮在会话项的上下文菜单。
- **删除二次确认**：onDelete 已有事件，接浏览器 confirm。
- **审批卡**（C6c 已落地）：消费 `tool_approval_pending/resolved` 事件
  与快照 `awaiting_approval`——渲染批准/拒绝按钮，回答走 POST
  /api/s/{id}/approval。**wire 事件帧在 C6c 补齐**（B2 7a 只落了
  `ToolWire.awaiting_approval` 快照标志；事件从未过
  encodeEventFrame——其 default 分支静默丢弃，C6c 用
  TestApprovalEventFrames 钉住精确 JSON 后补上）。

### 17.7 公开 API 收敛（B2 待办）

**推荐：C 的最后一步做收敛**（C 的宿主代码——Create/Archive/List 的
消费——写完后，近义项的实际用法可见，此时收敛有证据；另起小里程碑
会把"用法定型"与"收敛"拆到两处，中间态更久）。范围：
- `Send`/`Start`：看 C 的宿主代码用哪个（预计 Start 为主——web 需要
  Run 控制；Send 可能内化为语法糖或删除）。
- `SnapshotStream`/`SnapshotStreamState`：同上。
- `DefaultContextWindowTokens`：若 C 无人用则内化。
- `sessionhost.Coded`/`Options.Locate` 的宿主语义：看 Create/Archive
  路径是否需要变更签名。

**C9a 收敛结论（2026-10-15，逐项附调用点证据）**：
- **`Send`/`Start`：都保留。** `Start` 的生产调用点：CLI 的 send
  （cmd/sai/main.go，需要 Run 做 /stop）；`Send` 的生产调用点：web
  的 POST /send（internal/web/server.go——fire-and-forget，不需要
  Run 对象），另有 114 处测试调用。`Send` 是三行便利封装，方法注释
  已写明"for callers that don't need the Run object"。
- **`SnapshotStream`/`SnapshotStreamState`：都保留。** 前者的生产
  调用点：CLI bind 的历史渲染（订阅后快照）；后者：hub 的每请求
  快照帧（不订阅）。二者语义不同（订阅 vs 不订阅），注释已区分。
- **`DefaultContextWindowTokens`：已内化**（agent/budget.go 改小写
  `defaultContextWindowTokens`）——仓库内唯一使用点是 budget 的
  回退逻辑；无任何宿主读取它，关心窗口的宿主应按模型配置覆盖。
- **`sessionhost.Coded`/`Options.Locate` 的 projectPath：保留。**
  `Coded` 是 web 错误码映射的载体（writePoolError 消费），签名无
  变更需求；`Locate` 返回的 projectPath 由唯一的实现（web Manager）
  传给 `AgentFor(projectCwd)`——池的 Options 面本就是宿主装配向的
  （§15.3），把"项目路径"改名为更中性的"宿主上下文"只损失信息
  （现在传递的确实是一个项目工作目录），不启用任何新形态。等出现
  第二种宿主形状再议。

### 17.8 待用户拍板的问题（已全部裁定）

**裁定（用户 2026-10-15，opus 转达）：以下五题全部按推荐方案。
另：C1 标题问题另行裁定维持现状（见 §17.1）。**

- **(a) 归档存储方式**：推荐 A（标记文件）——路径不变量不动、崩溃
  一致性两态、锁语义连续（17.3 对比表）；B（目录移动）的收益只有
  "列表天然分区"，代价是全部寻址路径重构。
- **(b) 审批卡是否进 C**：推荐进（chat 页面本就在改；wire 就绪）。
- **(c) /sessions 的序号语义**：推荐"当次列表位置，文档写明不稳
  定"（备选：坚持 ID 前缀寻址、去掉序号——更稳定但可用性差）。
- **(d) 注销项目时有磁盘会话但未打开**：推荐允许注销（数据保留，
  重新注册即恢复可见）——用户拍板"不删除数据"的自然延伸；备选：
  拒绝并要求先归档/迁移。
- **(e) 惰性 header 的新会话在服务端列表的可见性**：推荐保持现状
  （首条消息前不可见，客户端本地显示）。**真实代价**（D1 修订）：
  刷新在空闲窗口内可回到（表内快路径），但**驱逐超时或进程重启后未
  发首条消息的新会话永久消失**（磁盘无目录）——空会话本来就没有任
  何内容，丢失的只是"导航占位"。备选：Create 后立即
  MaterializeHeader（列表即时可见、刷新/重启后可恢复，但产生"空标
  题会话"噪音且改变"空会话不落盘"的既有不变量）。两案的取舍交给
  用户。

### 17.9 迁移步骤（每步独立送审 + commit）

| 步骤 | 内容 | 回归保护 | 硬依赖（弱序见下行说明） | doc-sync |
|---|---|---|---|---|
| C1 | 标题问题：**先复现**（A 形态：真实压缩路径断言标题非空——实测预期绿）；绿则取用户磁盘形态（log-*.jsonl 清单）分流 B/C；**B 是规则冲突**——改规则需用户裁定（"撒谎比留空更糟"→"最早可读输入"）；C 另议或接受（**已落地：仅复现测试；规则不变**——用户裁定维持现有规则，不做阶段二，2026-10-15；commit `4a39846`） | A/B/C 三形态各有独立断言的测试；现有 list 测试全过 | 无 | **若改规则**：multi-session.md 的 Title 规则（"绝不取中途输入"段）+ storage/jsonl/store.go 的 readSessionHead 注释 + storage/storage.go 的 SessionInfo.Title 文档契约（"empty when the session start is no longer on disk"句）三处同步 |
| C2 | 共享校验函数 `ValidateSessionMeta`（B1 下沉）+ CLI /new 切换 + resolver level 校验测试 | CLI /new 既有测试不改断言；校验 kill（level 不校验→红） | 无 | design §17.2 |（已落地，commit `ca1eb33`） |
| C3 | `ModelDescriptor.Default` + `List()` 测试 + `GET /api/models` | List 顺序/默认标记 pin；端点过 guardAPI | （建议）C2 | multi-session 码表+路由 |（已落地，commit `4f17303`） |
| C4 | `sessionhost.Pool.Create` + `POST /api/projects/{pid}/sessions` | Create 校验 kill（非法 meta→400 码）；端点测试（unknown_project/model_unavailable/严格字段） | C2、C3 | 同上 |（已落地，commit `11508a0`） |
| C5 | `Archiver`（标记文件）+ SessionInfo.Archived + ListSessions 过滤 + `Pool.Archive` 互斥 | 归档/取消/IsArchived round-trip；打开归档→ErrArchived；运行中归档→ErrRunActive；列表默认过滤+--all | （建议）C1 | storage doc.go 契约；multi-session.md §4 码表（session_archived 409）+ 归档路由；**§15.5 前置条件补指针**（"C 阶段 0 修订：互斥机制见 §17.3"） |（已落地，commit `6e8eb88`，含 C5w） |
| C5w | web 归档端点（POST/DELETE /api/s/{id}/archive）+ 409 session_archived 映射 | 端点测试：归档/取消 round-trip、打开归档→409 码、锁竞争→session_locked | C5 | 同 C5 |（已落地，与 C5 同 commit `6e8eb88`） |
| C6a | chat 新建对话框 + 删除二次确认 | 对话框提交→POST→导航；tsc | C3、C4 | webui.md（无需改动：聊天 UI 的文档在 clients/chat/README.md，webui.md 只覆盖 log 视图并已指向该 README） |（已落地，commit `b686438`）
| C6b | chat 侧栏归档区（归档/恢复按钮 + 折叠区） | UI 消费 SessionInfo.Archived；tsc | C5、C5w | webui.md（无需改动：聊天 UI 的文档在 clients/chat/README.md，webui.md 只覆盖 log 视图并已指向该 README） |（已落地，commit `b227ee6`）
| C6c | chat **审批卡**（若 (b) 通过） | UI 消费 tool_approval_pending/resolved + 快照 awaiting_approval；回答走 /approval；tsc | 无（wire 事件编码在本步补齐——B2 只落了 ToolWire.awaiting_approval 标志） | webui.md（无需改动：聊天 UI 的文档在 clients/chat/README.md） |（已落地，commit `a836fa5`）
| C7 | CLI：/sessions、/archive、/unarchive、/resume 序号+前缀+精确匹配收紧 | /resumefoo stderr 断言（B2 待办）；序号/前缀/歧义用例 | C5 | README |（已落地，commit `f1e0e43`） |
| C8 | web 项目注册/注销端点 + 注册表文件锁 + CLI project add 走锁 | 并发注册/注销测试；注销有打开会话→409；缓存自动失效验证 | C4 | multi-session |（已落地，commit eb296c1；C8b 判定修正 17078cf）
| C9 | **公开 API 收敛**（17.7 清单，按 C 代码的实际用法） | 全部 C 测试不改断言通过 + 收敛项的调用方迁移 | C1–C8 | README API 面 |（已落地：C9a 结构与 API 收敛 commit `3c984d9`；C9b 事件 wire 守卫 + 文档收尾本步） |

硬依赖只有：C4→C2、C3；C5w→C5；C6a→C3、C4；C6b→C5、C5w；C7→C5；
C8→C4；C9→C1–C8。其余顺序（如 C1 在 C5 前）只是建议（避开同文件
编辑冲突）。后端（C1–C5、C5w、C8）先于前端（C6a/b/c）；CLI（C7）
与 web（C6*）在 C5 后可并行。C9 收尾。

### 17.9.1 遗留与未做（裁定记录，供后续参考）

里程碑 C 结束时**经裁定不做**的项及原因：

- **`/archive` 命中已归档会话的 "already archived" 措辞**——不做（opus，jgwfqzd5y1）：存储层幂等，结果正确；多一句措辞无收益。
- **provider/model 单侧为空时 "/m" 形状的显示**——不做（opus，jgwfqzd5y1）：合法会话两者必有，空值只出现在异常记录上。
- **删除对话框空标题 "untitled" 与 CLI（无标题）措辞统一**——不做（opus，jgwfqzd5y1）：两个界面各自一致即可。
- **`sai <id>` / `-session` 接受序号**——不做（opus，ykheyugd15）：脚本与 -session 的稳定入口，不引入不稳定序号；完整 ID only（归档命中的提示已在 C9a 补上）。
- **C1 标题规则变更（阶段二）**——不做（用户，p6ixoucavm）：维持现有规则（最早段缺失/损坏时留空，不取中途输入）。
- **注销项目时"在途打开"窗口的消灭**——不做（设计 §17.4）：接受窗口（在途打开是合法旧世界请求，会话照常工作，驱逐后不可重开），文档写明而非消灭。
- **`fixtureAgentForRegistry` 改结构体字段形态**——保持包级 var（review 认可，lu745aqfa0）：已是最简形态，改字段收益有限。
- **chat 深链到 locked/archived 会话的进一步处理**——已做基础（C6b：提示 + 停留/回列表）；无进一步计划。
- **TestSidebarUIFixture 的首跑偶发（C6a-fix2 记录）**：完整套件后紧接首跑曾见
  1/8 无诊断失败（opus 复现；随后 6 连绿）。负载形态量化：并行重载下
  紧预算等待 2/3 红（review 复现）→ 预算 20s 后 0/4 与套件并行 5/5；
  失败诊断已加（步骤名 + API 尾日志 + 失败截图路径，C6a-fix2）——
  下一次出现可定位。首跑含 dist 重建/浏览器冷启动，属环境态。

### 17.10 与既有设计的关系

- §15.5 的三个接口预留（Create/List/Archiver）在本章落地，在预留
  形状上**扩展**：Create 增 `ns`（宿主按项目 ns 建会话——预留的
  `Create(meta)` 漏了宿主需要的寻址参数）；Archiver 增
  Unarchive/IsArchived（取消归档与查询是 UI 必需）。**§15.5 的前置
  条件（"持有 root lock 且零 Lease"）被 §17.3 修订**：跨进程互斥靠
  会话自己的 OS 锁（CLI 不持 root lock）；接口注释按 §17.3 的机制
  写。
- §15.6 Q1=B 的项目范围语义贯穿（/sessions、/resume 均当前 ns）。
- §16（B2）的审批 wire（事件+快照+端点）在 C6 消费，无新 wire 字段。
- Create 的签名在 §15.5 预留的 `Create(meta)` 上扩展为 `Create(ns, projectPath, resolver, meta)`（宿主解析 ns→path；sessionhost 不引入注册表概念）。
- 需改的既有段落：§15.5 的 Archiver 前置条件原文（"调用方必须持
  有 root lock 且该会话无在途 Lease"）**被 §17.3 修订**——落地时在
  §15.5 补一句"（C 阶段 0 修订：互斥机制见 §17.3）"；其余既有段落
  不改。
