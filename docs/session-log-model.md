# 会话日志模型：Round / Turn 与分页

> 状态：**设计（未实现）**
>
> 上游：`docs/persistence-design.md`（事件信封与存储层）
>
> 本文定义会话日志的**内容模型**，以及基于它做的分页协议。
> 目标：让"日志一旦写入就不可变"这个性质真正成立，从而支持展示端缓存与增量拉取。

---

## 1. 两个层次：Round 与 Turn

| 概念 | 定义 | 用途 |
| --- | --- | --- |
| **Round**（大回合） | 用户一次提问 → 全部工具执行完毕 | 界面上的「已完成，用时 22s」 |
| **Turn**（小回合） | **一次 LLM 请求**（含其后的工具执行） | 分页单位、内容定位、缓存 |

一次 Round 包含多个 Turn；Turn 的驱动逻辑是「上一轮有 tool_use 就自动开下一轮」。

```
Round 0
  ├─ Turn 0
  │    ├─ message.input   ← 用户提问
  │    ├─ message.output  ← LLM 返回（thinking / text / tool_use）
  │    └─ tool.result     ← 该轮工具执行的结果
  ├─ Turn 1
  │    ├─ message.input   ← 用户插话（可选，可能多条）
  │    ├─ message.output
  │    └─ tool.result
  └─ Turn 2
       └─ message.output  ← 无 tool_use → Round 结束
```

### 1.1 命名对照（与现有代码）

| 新概念 | 现有代码 | 改动 |
| --- | --- | --- |
| `Round` | `driver.Turn` 结构体 | **改名**为 `Round` |
| `roundId` | 无（靠 user 消息推导） | **新增**，落盘 |
| `Turn` | `d.steps`（只计数） | **提升**为落盘概念 |
| `turnId` | `d.steps` 的值 | 落盘 |
| `timeline.KindTurnFooter` | 回合结束行 | 改为 `KindRoundFooter`（大回合） |

### 1.2 计数语义的冲突（待处理）

现有 `Counts`：

```go
// Turns = 用户输入条数（每次 Send 一条；一次投递的多条插话算多条）
// Steps = 发给模型的请求次数（每次 startTurn 一条）
type Counts struct { Turns, Steps int }
```

按新命名，`Steps` 才是 Turn。建议：

| 现状 | 建议 |
| --- | --- |
| `Counts.Turns`（用户输入数） | `Counts.Inputs` |
| `Counts.Steps`（请求次数） | `Counts.Turns` |

这会影响 `compact.snapshot` 里的 `Turns`/`Steps` 字段，需要兼容旧日志。

---

## 2. 事件清单

### 2.1 内容事件（不可变，可分页）

| 事件 | 载荷 | 说明 |
| --- | --- | --- |
| `message.input` | `{round, turn, message}` | 用户输入。**一个 Turn 可有多条**（插话） |
| `message.output` | `{round, turn, message}` | LLM 输出。含该 Turn 的全部 parts（thinking / text / tool_use） |
| `tool.result` | `{round, turn, call_id, name, output, is_error}` | 单条工具结果，**靠 `callId` 与 tool_use 关联** |

**粒度原则：一条记录 = 一条完整的东西。**

| 内容 | 记录粒度 |
| --- | --- |
| 一条用户消息 | 一条 `message.input` |
| 一条 LLM 响应（含多个 parts） | 一条 `message.output` |
| 一次工具调用的结果 | 一条 `tool.result` |

**不拆到 part 级**。一次 LLM 响应在协议上就是一个 assistant message，拆细会让重放组装多一层无谓复杂度。

**长内容的处理**：如果某个 output 或工具结果特别大（长输出、大文件内容），
考虑改成**资源引用**（复用现有的 blob 机制，按 hash 存、按需拉取），
而不是把全文塞进事件。这样事件本身保持小而稳定，分页与缓存都不受影响。

> `callId` 关联与 LLM 协议一致（Anthropic `tool_use_id` / OpenAI `tool_call_id`），
> 并行工具完成顺序不影响配对。

### 2.2 边界事件（显式标记，便于崩溃恢复）

| 事件 | 载荷 | 说明 |
| --- | --- | --- |
| `round.started` | `{round}` | Round 开始 |
| `round.finished` | `{round, duration_ms}` | Round 正常结束 |
| `turn.started` | `{round, turn}` | Turn 开始（请求即将发出） |
| `turn.finished` | `{round, turn, duration_ms}` | Turn 正常结束 |

**为什么显式记录**：崩溃恢复需要判定"哪些 Turn 没走完"。没有 `turn.finished` 的一律视为崩溃。
用户要求"先都记录一下，就算以后不要也容易删除"——这几个事件的载荷很小，价值大于成本。

### 2.3 状态事件（用于重放，不做分页游标）

| 事件 | 说明 |
| --- | --- |
| `session.created` / `session.renamed` | 会话元信息 |
| `selection.changed` | 当前模型 |
| `ask.raised` / `ask.answered` | 待回答提问 |
| `notice.appended` | 本地通知 |
| `todo.write` | 任务清单（整体替换） |
| `compact.snapshot` | 压缩快照 |
| `message.status` | 仅为兼容历史日志保留，新代码不再产生 |

### 2.4 移除

| 事件 | 原因 |
| --- | --- |
| `message.appended`（streaming 空壳） | 违反"内容只写一次"；崩溃恢复改用 `turn.started/finished` 判定 |
| `tool.results`（批量） | 拆成单条 `tool.result` |

> **不做兼容**：旧日志（含 `message.appended` / `message.finalized` / 批量 `tool.results`）
> 不需要继续支持。迁移时直接换掉事件模型，旧会话数据不保留。
> 这省掉了双写与兼容层，也让新模型能干净地落地。

---

## 3. 崩溃恢复

### 3.1 判定规则

> **没有 `turn.finished` 的 Turn 一律视为被中断。**

| 已落盘的内容 | 判定 | 恢复动作 |
| --- | --- | --- |
| 全部 Turn 都有 `turn.finished` | 正常 | 无 |
| `turn.started` + `message.input`，无 output | 请求未发出 | **标记待重发** |
| `turn.started` + `message.output`（含 tool_use），无 tool.result | 工具执行中 | **补 `tool.result`（is_error）** |
| `turn.started` + `message.output`（无 tool_use），无 `turn.finished` | 输出已完成但边界没写 | 补 `turn.finished` |

### 3.2 为什么必须补 tool.result

工具调用（`tool_use`）已经落盘。若下次请求不带配对的 `tool_result`，LLM 协议会直接拒收（OpenAI / Anthropic 都要求成对）。

所以补全不是"给用户看失败提示"，而是**让下一次请求能正常发出**。

补齐的内容：`is_error = true`，`output = "已中断：进程在执行工具时退出。"`

### 3.3 待重发

- **不自动重发**：打开一个崩溃过的会话不应该立刻触发 API 调用
- 界面显示已落盘的内容 + 提示「上次会话异常结束」，提供「重发」按钮
- 用户点重发时：先确保 `tool.result` 都补齐（幂等），再调 `startTurn` 继续
- **不重复写 `message.input`**（它已在日志里）

### 3.4 边界情况：自动续轮的 Turn 没有 input

```
Turn 0: message.input + message.output(tool_use)   ← 完整
Turn 1: message.output(tool_use)                    ← 无 input，正常
Turn 2: turn.started（崩溃，什么都没有）             ← 待重发
```

重发应从 Turn 2 继续（带上 Turn 1 已补齐的 tool.result），而不是重发 Turn 1。
判定依据是「最后一个 `turn.finished` 之后的内容」，不是「最后一条 input」。

---

## 4. 分页协议

### 4.1 单位：Turn

**按 Turn 分页，不按 Round**。原因：一个长任务可能在同一个 Round 里跑很多 Turn，按 Round 切一页会过大且不均匀。

### 4.2 规则

```
一页 = 若干个完整的 Turn
合法边界：
  ✅ [Turn 0 完整][Turn 1 完整] | [Turn 2 完整][Turn 3 完整]
  ❌ [Turn 0 完整][Turn 1 一半] | [Turn 1 另一半]

"Turn 完整"的判定：
  - 有对应的 turn.finished
  - 且该 Turn 的所有 tool_use 都有 tool.result
```

### 4.3 游标

```go
// 向后（加载更早）
TimelinePageJSON(sessionID string, beforeSeq uint64, limit int) string

// beforeSeq = 0 表示"最后一页"
// 返回覆盖的 [seqFrom, seqTo]，客户端据此缓存
```

**为什么用 seq 而不是 item key**：item 是渲染产物，key 会随流式变化；seq 一旦写入永不变。

### 4.4 响应结构

```json
{
  "items": [...],
  "seq_from": 120,
  "seq_to": 187,
  "has_more": true,
  "state": { ... }        // 截至 seq_to 的状态快照
}
```

**状态随每页附带**，且只反映"截至该 seq"的值。客户端翻历史页时不应用旧状态覆盖当前状态。

### 4.5 客户端缓存

```
已有：[Turn 0 ... Turn 5]（seq 1..87）
    ↓ 滚到顶部
请求：beforeSeq=1, limit=N
    ↓
得到：[Turn -2 ... Turn -1]（seq 1..30 之前），合并到头部
```

缓存 key 用 `seq` 区间。

**压缩后 seq 不重置**（关键设计）：

```
压缩前：seq 1..1696
压缩后：快照（seq 1697，内容 = 压缩摘要 + 保留的条目）
        后续事件：1698, 1699, ...
```

这样处理的原因：
- 展示端缓存 key 可以直接用 `seq`，不必拼「第几个文件」
- 分页游标跨压缩仍然有效，不会出现「压缩后游标失效」
- 单调递增这个性质贯穿整个会话生命周期

**代价**：`store` 层现在压缩后会 `nextSeq = 1`（`seq=0` 固定留给快照）。
改成不重置需要：
- 快照不再硬编码 `Seq = 0`，而是分配一个正常的值
- `kept_from_seq` 的语义要跟着调整（它现在指「base 里最后一条事件的序号」）
- 加载时能从快照读出「历史覆盖到哪个 seq 为止」

**压缩后的第一条记录**就是「压缩后需要用到的那部分信息」（摘要 + 保留条目），
读取方据此知道「seq < 这个值的历史已经不在当前上下文里了」。

---

## 5. 重放组装

日志形态 ≠ 内存形态。重放时要组装回 `[]message.Message`（LLM 协议要求）。

### 5.1 组装规则

```
1. 按 (round, turn) 分组，组内按 seq 排序
2. message.input  → 独立的 user message
3. message.output → 独立的 assistant message（parts 按 seq 顺序）
4. tool.result    → 附加到对应 tool_use 所在 message 的 parts（靠 callId）
```

### 5.2 为什么 tool.result 归"产生它的 Turn"

```
Turn 0:
  message.output { tool_use c1 }
  tool.result    { c1 }          ← 归 Turn 0（它是 Turn 0 工具调用的结果）
Turn 1:
  message.output { ... }         ← 用 Turn 0 的结果发起新请求
```

这样分页时，一个 Turn 包含"请求 → 输出 → 它触发的工具结果"，是完整的一页。

### 5.3 连续 user 消息

历史里会出现连续的 user 消息：

```
user      ← message.input
user      ← message.input（插话，同一 Turn 内多条）
assistant ← message.output
user      ← tool.result（LLM 协议里 tool_result 是 user 角色）
```

OpenAI 接受连续 user；Anthropic 会把相邻同角色消息合并。这点现有代码已经在依赖，重放组装保持现状即可。

---

## 6. 迁移步骤

| 阶段 | 内容 | 风险 |
| --- | --- | --- |
| 1 | 重命名：`driver.Turn` → `Round`；`d.steps` → `turn` | 低（机械，但涉及 driver/timeline/uiapp/bridge 与测试） |
| 2 | 新增 `roundId` / `turnId` 并落盘；新增四个边界事件 | 中 |
| 3 | 事件拆分：`tool.results` → 单条 `tool.result`；去掉 streaming 空壳 | 中 |
| 4 | 崩溃恢复改为基于 `turn.finished` | 中（替换现有空壳检测） |
| 5 | **压缩后 seq 不重置**（快照不再用 0） | 中（涉及 store 层与 kept_from_seq） |
| 6 | 分页协议从 item key 改为 seq | 中 |
| 7 | 展示端按 seq 缓存 | 小 |
| 8 | （可选）`timeline.Build` 改增量 | 高 |

**阶段 1-6 完成后**：分页稳定、崩溃恢复明确、"内容只写一次"成立、压缩后游标不失效。
**阶段 8** 才是解决"计算量随会话长度线性增长"的终极方案，可等观察到瓶颈再做。

### 6.1 不做兼容迁移

旧日志不需要继续支持。迁移时直接换掉事件模型：

- 不再写入 `message.appended` / `message.finalized` / 批量 `tool.results`
- 不再读取旧格式（旧会话数据不保留）
- 相关测试数据需要按新模型重新生成

这消除了双写、版本分派、兼容层这些复杂度。

---

## 7. 未决问题

1. **`Counts` 的字段命名**：现有 `Counts.Turns` 指"用户输入条数"，与新的 `Turn`（请求次数）冲突。
   建议改为 `Inputs` / `Turns`。因为不做兼容迁移，可以直接改。

2. **`turn.started` 的必要性**：如果 `message.output` 之前崩溃，`turn.started` 是唯一证据。
   也可以靠"有 input 无 output"判定，但自动续轮的 Turn 没有 input，会误判。保留更明确。

3. **`message.output` 是否要 part 级拆细**：当前设计一条事件一条 message。
   若将来某个 part 特别大，优先考虑**资源引用**（blob hash）而不是拆事件。
