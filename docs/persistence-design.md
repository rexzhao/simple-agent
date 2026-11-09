# 会话持久化与上下文压缩 · 设计文档

> **实现状态：已实现**（M5/M6）。与本文档的差异都记在
> `docs/go-rebuild-plan.md` §12「已定决策汇总」与 §13「已实现状态」里，
> 主要有：`message.status` 仅为兼容历史保留、新增 `notice.appended`、
> 重放返回 repairs、自动压缩策略放在 driver 层。

范围：`internal/store`、`internal/driver`（重放与压缩）、`internal/uiapp`（接线与 UI）
不涉及：渲染细节、provider 协议

> 文中出现的 `crates/*`、`replay()` 的 Rust 签名属于**原始设计**的写法（这份文档
> 从 Rust 阶段延续而来）；Go 版的对应实现见每节末尾的说明。

---

## 1. 目标

1. 会话可持久化（JSONL）或不落盘（纯内存），两者实现同一接口、共用同一套重放逻辑。
2. 日志是**事件日志**：所有会改变会话状态的动作都记录（消息、流结束/失败/取消、工具结果、ask 挂起/作答、切换模型、重命名、压缩），从头重放得到最终状态。
3. 上下文压缩发生时滚动文件：**不带后缀的文件永远是最新的**，加载只读它。
4. 压缩前的历史按需加载（只为显示，不发给模型）。

---

## 2. 分层

```
crates/store     事件定义 + 存储 trait + JSONL / 内存实现 + 分页读取   （依赖 core，不依赖 gpui）
crates/core      replay(events) -> ReplayedSession 纯函数            （无 IO）
crates/app       driver 接线：状态变化 -> 事件；启动时 replay          （gpui 只出现在 ui.rs）
```

重放放在 `core`：它只依赖 `Conversation` / `Message` / `EntryStatus`，可以在任何后端之上复用。

---

## 3. 事件模型

### 3.1 信封

每行一个 JSON 对象（JSONL）：

```json
{"v":1,"seq":12,"at":"2026-10-27T11:30:00Z","type":"message.appended","data":{…}}
```

- `v`：日志格式版本，便于以后迁移。
- `seq`：单调递增，用于检测截断/重排（读到 `seq` 不连续即视为损坏点）。
- `at`：写入时刻（UTC，RFC3339）。
- `type` + `data`：见下表。

### 3.2 事件清单

| type | data | 说明 |
|---|---|---|
| `session.created` | `{context_id, remote_context_id?, title?, created_at}` | 必须是文件第一行（除快照外）。`remote_context_id` 只存这里（A2A 服务端分配的 id，与本地 `context_id` 分开） |
| `message.appended` | `{message: <A2A Message wire JSON>, status}` | **流开始时**写入的条目（`status=streaming`，内容为空壳）。用户消息则以 `complete` 直接写入 |
| `message.finalized` | `{message, status, reason?}` | **流结束/停止/失败时**写入最终内容（完整 parts）。只有 `appended` 没有 `finalized` 的消息，重放为 `cancelled`（「已中断」占位） |
| `message.status` | `{message_id, status, reason?}` | **仅为兼容历史日志保留**（早期实现用它做单点更新）。新代码不再产生；重放仍然认得 |
| `notice.appended` | `{level, text}` | 本地通知（截断提示、轮数上限、工具未返回结果等）。必须写日志，否则重放后界面上会少一条 |
| `tool.results` | `{message_id, results:[{id,name,output,is_error,pending?}]}` | 工具结果回填（与 `ToolResult` 同构） |
| `ask.raised` | `{message_id, call_id, questions:[…]}` | `ask_user_question` 的提问（复用 `QuestionSpec`） |
| `ask.answered` | `{message_id, call_id, output}` | 作答/跳过的最终结果文本（模型可见的那段 JSON 字符串） |
| `selection.changed` | `{provider_id, model}` | 切换模型（重放后仍是当前选择） |
| `session.renamed` | `{title?}` | `null` 表示恢复未命名 |
| `compact.snapshot` | 见 §6.2 | 压缩后的完整快照，**只出现在带后缀归档文件的最后一行 / 新文件的第一行** |

> `message.appended` 对**用户消息**和**完成的 agent 消息**都会写；流式过程中的增量不写。

### 3.3 为什么消息直接嵌入 A2A wire JSON

`message.rs` 已有严格 oneof 校验、camelCase 命名、base64 处理；日志复用同一序列化，避免第二套格式漂移。代价是日志里字段名与 A2A 一致（`messageId`、`mediaType`…），可接受。

---

## 4. 存储接口

```rust
pub trait SessionStore: Send {
    fn append(&mut self, event: Event) -> io::Result<()>;
    /// 只读最新文件（或内存里的全部事件）。
    fn load(&self) -> io::Result<Vec<Event>>;
    /// 滚动：把当前内容归档，并用 `snapshot` 开一个新文件。
    fn compact(&mut self, snapshot: Event) -> io::Result<()>;
    /// 按需加载更早的归档（分页）；`cursor = None` 表示从最近的一份开始。
    fn load_older(&self, cursor: Option<ArchiveCursor>) -> io::Result<OlderPage>;
}

pub struct OlderPage {
    pub events: Vec<Event>,
    pub next: Option<ArchiveCursor>,   // None = 没有更早的了
}
```

两种实现（Go 版：`internal/store/jsonl.go`、`memory.go`）：

- `JsonlStore { dir, current File, archiveSeq uint32 }`
- `MemoryStore { events []Event }`（`compact` 只做快照折叠；`load_older` 恒为空）

Go 版另有两点实现细节：

- 打开文件时**先修文件**：末尾半行截掉（否则之后追加的事件会被反复当作「中间坏行」丢弃）、
  中间行损坏则留档为 `session.corrupt-<ts>.jsonl` 并用合法前缀重写；
- 快照里写入 `archived_as`（对应哪一号归档）与 `kept_from_seq`（按需加载时截断用），
  打开时据此清理「被中断的压缩」留下的孤儿归档。

两者把 `Vec<Event>` 交给同一个 `replay`，因此**重放逻辑只有一份**。

---

## 5. 目录结构与原子性

```
<data_dir>/sessions/<context_id>/
    session.jsonl            ← 最新（不带后缀）
    archive-0001.jsonl       ← 第一次 compact 之前的完整原始历史
    archive-0002.jsonl
    meta.json                ← {remote_context_id?, created_at}（轻量索引，可重建）
```

`data_dir`：`$SIMPLE_AGENT_DATA`，否则平台默认（Windows `%APPDATA%/simple-agent`）。

### 5.1 强制约束：任何时刻 `session.jsonl` 都存在且是完整的

`compact` 的顺序（每步写完 `sync_data`）：

1. 若 `archive-000N.jsonl` 不存在：把当前 `session.jsonl` **复制**为它（不是 rename）。
2. 写 `session.jsonl.tmp`：第一行 `seq=0` 的 `compact.snapshot`，后续无内容。
3. `rename(tmp, session.jsonl)`（同目录 rename 在 Windows/POSIX 都是原子的）。

崩溃分析：

| 崩在哪 | 状态 | 加载结果 |
|---|---|---|
| 1 之后 | 归档已存在，base 仍是旧内容 | 读到旧 base（合法），归档为多余副本 |
| 2 之后 | tmp 存在，base 仍是旧内容 | 读到旧 base；下次 compact 覆盖 tmp |
| 3 之后 | base = 快照 | 正常 |

启动时清理残留 `*.tmp`。归档序号取现有最大值 +1，若目标已存在则校验后复用（保证幂等）。

---

## 6. 重放语义

```rust
pub fn replay(events: &[Event]) -> ReplayedSession;

pub struct ReplayedSession {
    pub context_id: String,
    pub remote_context_id: Option<String>,
    pub title: Option<String>,
    pub selection: Option<Selection>,
    pub entries: Vec<Entry>,            // 含悬空 ask 的占位结果
    pub pending_asks: Vec<ReplayedAsk>, // 未作答的提问
    pub compacted: Option<CompactedInfo>, // 摘要文本 + 边界（若有）
}
```

### 6.1 规则

- 按 `seq` 递增应用；`seq` 不连续或 JSON 解析失败时：**若是最后一行 → 丢弃该行并正常返回**；若在中间 → 记录警告并在此截断（之后的数据不可信）。
- `message.appended` 建条目；`message.status` / `tool.results` 修改已有条目。
- **流式中间态**：日志里只有 `message.appended{status:streaming}` 而没有对应 `message.finalized` 时（进程被杀），重放为 **`Cancelled`**（界面上显示「已中断」）。不臆造内容。
- **修复事件（repairs）**：重放会返回一组必须**补写进日志**的事件，加载流程要在接受任何新事件之前追加它们：
  1. 只有 `appended` 没有 `finalized` → 补一条 `finalized{cancelled}`；
  2. 有 `tool_use` 但没有结果（执行工具时崩溃）→ 补 `tool.results`，`is_error=true`，文本为「已中断」；
  3. 挂起提问的 `pending` 占位**不算**缺结果（那是合法未完成）。
  补写之后再重放不会产生新的 repairs（幂等），有测试守着。
- **挂起中的 ask**：`ask.raised` 无对应 `ask.answered` → 恢复为悬停状态：面板重新出现、`busy = AwaitingAnswer`；提交后按现有 `submit_answers` 流程续一轮（`Turn.selection` 从 `selection.changed` 恢复）。**未配提供商或 provider 不可用时**，面板仍显示但提交会失败并提示（不阻塞加载）。
- `selection.changed` 覆盖当前选择；没有该事件时用 `Registry::initial_selection()`。
- `session.renamed` 覆盖标题。
- `tool.results` 中 `pending: true` 的结果按 `ask.raised` 的状态判断是否还原成占位。

### 6.2 `compact.snapshot` 的 data

```json
{
  "summary": "压缩摘要正文（纯文本 / markdown）",
  "kept_messages": [ "<A2A Message>", "…" ],
  "selection": {"provider_id": "deepseek", "model": "deepseek-chat"},
  "title": "会话标题",
  "pending_asks": [{"message_id": "...", "call_id": "...", "questions": [...]}],
  "compacted_at": "2026-10-27T11:30:00Z",
  "kept_from_seq": 118,
  "turns": 12,
  "steps": 18
}
```

- **不含**历史消息全文（那是归档文件的职责）。
- `kept_messages` 是压缩后仍进入模型上下文的那部分（含被保留下来的原始消息）。
- 快照自身 `seq = 0`，之后的增量从 1 开始，保证"新文件可独立重放"。
- `turns` / `steps` 是**压缩之前**的累计计数基线（底部状态条用，口径见
  `driver.Counts`：turns = 用户输入条数、steps = 发给模型的请求次数）。
  加这两个字段是因为计数由会话内容推导，而压缩丢弃了历史——不带基线的话，
  压缩后计数会掉回「只剩 kept 里的几条」。旧快照没有这两个字段，读出来是 0。

---

## 7. 上下文压缩

### 7.1 触发

| 方式 | 说明 |
|---|---|
| 手动 | 界面按钮「压缩上下文」（`busy == Idle` 时可用） |
| 自动 | 估算上下文用量 ≥ **80%** 窗口（常量 `AUTO_COMPACT_RATIO = 0.80`，后续做成配置） |

用量估算：`sum(消息文本字节数)/4 + 工具结果字节数/4 + 摘要长度`（保守的中英混合估算）。窗口大小来自 `ProviderConfig`（新增可选字段 `context_window: Option<u32>`，缺省按 128k）。

### 7.2 流程

```
Idle → Compacting（新增 Busy 状态，禁止发送）
  1. 选边界（见 7.4）
  2. 用当前 selection 请求摘要（无工具、非流式展示）
  3a. 成功 → 组装 compact.snapshot → store.compact(snapshot) → 重建内存状态 → Idle
  3b. 失败 → 状态不变、不滚动文件、notice 提示「压缩失败：…」，Idle
```

- **摘要请求失败不破坏任何东西**：不写快照、不归档、内存状态不变。可重试。
- `Compacting` 期间发送按钮置灰，提示「正在压缩上下文…」。
- 摘要请求本身不计入会话消息（不写日志、不显示在聊天流里）。

### 7.3 摘要的表示

摘要作为一个合成的 `Message`：

```json
{"messageId":"<uuid>","role":"ROLE_AGENT",
 "parts":[{"text":"…摘要…","mediaType":"text/vnd.simple-agent.summary"}]}
```

- 作为 history 的**第一条**（`role=agent`，纯文本 part），两家 provider 都能接受（assistant 文本消息）。
- 不使用 tool_use/自定义块，避免触发配对约束。
- `mapping.rs` 无需改动；`PartView` 新增 `Summary` 视图（弱化显示，点击展开）。

### 7.4 边界选择（关键约束：不切断 tool 配对）

`kept_messages` 取最近若干条，起点向前/向后微调直到满足：

1. 起点不能是 `tool_result`（OpenAI 的 `role:"tool"` / Anthropic 的 `tool_result` 块）——否则它的 `tool_use` 已被压缩掉，请求会 400。
2. 终点若落在 assistant 的 `tool_use` 上，必须把配对的 `tool_result` 一并保留（否则同样 400）。
3. 至少保留最后 1 条用户消息（避免出现"没有用户输入"的上下文）。

实现为纯函数 `choose_boundary(messages, budget) -> Range<usize>`，配单测覆盖：工具调用跨边界、连续 tool_result、只剩工具消息等。

### 7.5 压缩后发给模型的历史

`summary_message + kept_messages`。旧文件的早期消息**永远不参与请求**。

---

## 8. 界面

1. **早期消息按需加载**：启动只重放最新文件。用户向上滚到顶时，用 `load_older` 逐份读归档，把其中的 `message.appended` 以**只读显示条目**插到列表头部（`Conversation::prepend_history`）。这些条目不进入 `history_for_request`。
2. **压缩分隔线**：压缩边界处插入一条 `Entry::Compacted { summary, at }`，显示「以上内容已压缩」；点击展开摘要原文（复用 markdown 渲染）。
3. 自动压缩发生时如果用户正在看早期消息，不做滚动跳转，只在分隔线处更新。

---

## 9. 测试清单

> 以下清单在 Go 版全部有对应用例（`store/store_test.go`、`driver/replay_test.go`、
> `driver/compact_test.go`、`uiapp/app_test.go`）；其中「事件由 driver 真实产生」这一点
> 是硬要求——手工构造事件的测试会掩盖真实路径里的缺陷。

| 测试 | 要点 |
|---|---|
| `replay_matches_live_state` | 构造一段在线操作（发送→流式→工具→ask→作答→续流→重命名→切模型），一边跑 driver 一边记录事件；`replay(events)` 的结构与在线状态逐字段一致 |
| `replay_from_snapshot_equals_full` | 同一段历史：①全量重放后执行 compact；②直接重放新文件（快照 + 后续事件）→ 两者状态一致 |
| `tolerates_truncated_last_line` | 人为截断最后一行 JSON → 加载成功，前面的状态完整 |
| `compact_keeps_tool_pairing` | 边界落点处 tool_use/tool_result 不被拆开；`to_openai_messages` / `to_anthropic_messages` 不 panic 且配对完整 |
| `archive_roll_is_atomic` | 模拟 3 个崩溃点（§5.1 表格），加载都能拿到合法的 base |
| `streaming_entry_becomes_cancelled` | 只有 streaming 事件时重放为 Cancelled，保留内容 |
| `pending_ask_restored` | `ask.raised` 无作答 → 面板恢复、busy=AwaitingAnswer；作答后能续一轮 |
| `memory_store_shares_replay` | 两种后端同一事件序列 → 同一状态 |

---

## 10. 未决问题（请 review 时给意见）

1. **归档分页粒度**：一次 `load_older` 返回**整份归档**（一个 compact 周期的全部事件）还是按固定条数分页？我倾向整份（一个周期的量通常可接受，且实现简单），特别大时再细分。
2. **`seq` 在新文件中重置为 0 是否够用**：跨文件不连续，我按"只在新文件内检查连续性"处理。
3. **自动压缩的用量估算**：`字节/4` 对中文偏差较大（UTF-8 中文 3 字节/字 ≈ 1 token/字，反而接近）；是否改为 `字符数` 加权？目前倾向保守估算（宁可早一点压缩）。
4. **`meta.json` 是否必要**：`remote_context_id` 也可以只存在 `session.created` 事件里；独立 `meta.json` 便于不重放就列出会话列表（侧栏需要）。倾向保留。
5. **压缩摘要的提示词**放在哪里：`crates/store`（与压缩绑定）还是 `crates/app`（UI 文案）？倾向 `core` 里的常量，便于测试。
