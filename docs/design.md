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
  /clear 保持现状（新开 session），上下文重置由 /compact 承担。

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

**预算推导**（budget.go，移植自旧栈）：`context_window − 输出预留
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
// SessionEvent 归属 internal/agent。
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

- SessionEvent 定义在 internal/agent（依赖 storage.Record 和
  model.Event）；B 的 sessionhost 移出 internal 时一起搬。
- Run 对象不泄漏 storage 细节——B 的 sessionhost 可以持有 *Run。
- Stream 的快照语义与 B 的 Namespace 化正交。
- seq 全序机制与 B 的 Hooks 正交（seq 分配在 Session 内部）。
