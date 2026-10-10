# sai 最小 CLI：重构设计基准

> 状态：破坏性重构前的设计定稿。实现与本文冲突时，先改本文再改代码。
> 旧日志格式不做兼容迁移。

## 1. 目标与非目标

**目标**：最小版本的本地 CLI agent——单二进制、单工具（shell）、会话持久化为
append-only JSONL。为将来 WebUI 预留统一的 item 流模型，但不为它写任何代码。

**非目标**：List/会话选择器（砍掉，将来需要再加回）、旧日志兼容、WebUI 本体、
并行工具执行、非输入触发的 run。

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

- **单一累积点**：`session.AppendDelta` 写 `Streaming.Message`；
  `streamModelTurn` 的局部 builder 删除。Completed 时 commit 从 Streaming
  取内容拼记录。
- **重试语义**：重试 = 重置 Streaming + reset 信号。UI 是 replace 语义而非
  append 语义（Streaming 存全量累积内容）。CLI 终端可以直接 append 打印，
  两个 UI 语义不同没关系，数据源同一。
- **失败语义（接受的易失性）**：失败的半截内容只在失败事件里带给在场的
  UI；Snapshot 不保留，刷新即消失。中断的 run 就是 run.start 后无人应答，
  不需要收尾记录。
- **重连**：Snapshot()（durable + transient）+ 继续事件流，无缝接上，零重拉。

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
| Store.List / Summary | 无（将来 WebUI 需要时加回，Summarize 纯函数保留） |
| storage 级 RecordsAfter / Subscribe | 通知上移到 agent.Session 的 commit 通知（携带记录本身） |
| State.LastSeq | 无，行序即文件序 |
| TurnID / turn 字段 / MaxTurns 命名 | RunID / MaxIterations |
| agent 的 config 依赖 | 参数全部构造期注入 |
| chatapp.Session.Close 的 5s 忙等 | 显式同步（cancel → 等 pump goroutine 退出） |

## 11. UI 更新模型

| 场景 | 通道 |
|---|---|
| 在场 | 事件流（ItemID 寻址，幂等 upsert；delta/started/completed/tool 事件） |
| 重连 | Snapshot 一次 + 继续事件流 |
| 落盘 | 对 UI 只是终态事件的等价副本 |

CLI 的在场渲染：delta 逐字写终端（reasoning 带 `·` 行标记），assistant
消息的身份在 started 事件时就已标记，落盘记录按 ItemID 过滤不重复打印
（记录流与事件流分属两个 channel，无法证明顺序时的克录记录先暂存，
事件排干后统一裁决）。spinner 只标记静默阶段（等首 token、工具执行中）。

WebUI 将来的桥只做一件事：把事件流按序转发（WebSocket/SSE），UI 端按
ItemID upsert、按 RunID 分组。CLI 现在直接消费同一事件流，零额外成本。

## 12. 已记录的未来需求（未设计）

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
告知用户超预算但无摘要可压、请求照发）；软压冷却——上次压缩后需 ≥2 条
user.input；软压回收检查——摘要后估算缩减不足 max(2K, 前/20) 则丢弃
不落盘；硬压压缩后仍超硬线 → run.failed（与旧版差异：记录先落盘再判
定，下次 Send 会再压缩——窗口容不下任何摘要时本就无解）。

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
