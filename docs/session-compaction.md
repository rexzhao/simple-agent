# 会话压缩设计

本文记录会话压缩功能的需求、设计决策和验收标准。它是独立设计文档，不修改现有
`docs/development.md`、`docs/milestones.md`、`docs/checklist.md` 或
`docs/configuration.md` 的内容。

目标是保证即使未来在新的会话中继续开发，也能从本文恢复足够上下文并完成实现。

## 目标

会话压缩只优化模型上下文，不删除用户可见会话历史。

压缩后：

- 模型使用压缩后的 active history 继续工作。
- GUI 仍能通过 server 展示压缩前后的完整会话内容。
- `--resume` 使用最新 active history 续跑，不从完整可见历史反推模型上下文。
- 完整 session 存储是 append-only，可分页读取，可承载未来 server + Web GUI。
  GUI + server 设计单独记录在 `docs/server-gui.md`。

## 非目标

MVP 不实现：

- mid-turn 自动压缩。
- 模型切换兼容性检测，例如 compaction compatibility hash。
- 远端 compaction provider 分派。
- session-history 查询工具。
- 旧 session 格式兼容或迁移。
- GUI 直接读取 session 文件。

session-history 查询工具可以作为低优先级后续项记录：未来可提供
`search_session_history` / `read_session_items` 之类工具，让 agent 按需查询当前 session
的旧 visible items。但它不进入 compaction MVP。

## 核心原则

1. `Items` 是完整事实账本。
2. GUI 是 `Items` 的 filtered / paginated view，具体接口和产品形态见
   `docs/server-gui.md`。
3. `ActiveHistory` 是发给模型的 ordered projection。
4. 压缩只替换 `ActiveHistory`，不删除旧 `Items`。
5. 只持久化成功 turn。
6. compact 是原子状态变更。
7. summary 不保存隐藏推理链，只保存事实、状态、决策、约束和下一步。

## Session 数据模型

不兼容旧 session 格式。实现时可以直接升级 session version，例如 `Version = 2`。

旧的 `Messages []model.Message` 不再同时承担完整历史和模型上下文两个职责。新的 session
应拆成：

- `Items`: append-only 会话事实账本。
- `ActiveHistory`: 发给模型的 item id 列表。
- `Compactions`: 压缩 checkpoint metadata。

建议结构：

```go
type Session struct {
    ID        string    `json:"id"`
    Version   int       `json:"version"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`

    Provider        string         `json:"provider"`
    ModelProfile    string         `json:"model_profile"`
    ModelID         string         `json:"model_id"`
    ModelParameters map[string]any `json:"model_parameters,omitempty"`
    CWD             string         `json:"cwd"`
    ConfigPath      string         `json:"config_path,omitempty"`

    EnabledTools  []string `json:"enabled_tools,omitempty"`
    EnabledMCP    []string `json:"enabled_mcp,omitempty"`
    EnabledSkills []string `json:"enabled_skills,omitempty"`
    ShowReasoning bool     `json:"show_reasoning"`

    Items         []SessionItem          `json:"items,omitempty"`          // replayed state or snapshot
    ActiveHistory []string               `json:"active_history,omitempty"` // item ids
    Compactions   []CompactionCheckpoint `json:"compactions,omitempty"`

    Context         contextwindow.Metadata `json:"context,omitempty"`
    SaveToolResults bool                   `json:"save_tool_results"`
}
```

`Items` 在持久化层建议通过 JSONL replay 得到，内存中可以缓存 replay 后的状态。

### SessionItem

所有会话相关内容都可以进 `SessionItem`，包括普通用户消息、assistant 消息、tool result、
system/developer/runtime context、compaction summary 等。GUI 自己根据类型和可见性过滤。

```go
type SessionItem struct {
    ID        string    `json:"id"`
    TurnID    string    `json:"turn_id,omitempty"`
    Seq       int64     `json:"seq,omitempty"`
    CreatedAt time.Time `json:"created_at"`

    Kind       string `json:"kind"`       // message, compaction, runtime_context
    Visibility string `json:"visibility"` // visible, hidden, debug
    Audience   string `json:"audience"`   // user, model, internal

    Message *model.Message `json:"message,omitempty"`
}
```

建议可见性语义：

- `visible`: 默认 GUI chat view 可见。
- `hidden`: 普通 GUI 不展示，但可能进入模型上下文。
- `debug`: 普通 GUI 不展示，debug view 可见。

建议 audience 语义：

- `user`: 用户可见或面向用户展示。
- `model`: 可进入模型上下文。
- `internal`: 仅运行时或 debug 使用。

不要只用 `hidden bool`，后续不够表达 GUI、模型和内部调试的差异。

## Append-only JSONL 存储

该存储需要支持未来 server + Web GUI。server 和 GUI 的具体职责、接口、WebSocket 事件、
分页 view 等单独记录在 `docs/server-gui.md`。

建议存储布局：

```text
sessions/
  blobs/
    sha256/
      ab/
        <hash>.data
  <session-id>/
    meta.json
    segments/
      000001.jsonl
      000002.jsonl
```

### Segment

session event log 使用 segmented append-only JSONL。

要求：

- 每条 record 有全局递增 `seq`。
- segment 只限制最大行数，满了滚动到下一个 segment。
- 分页 cursor 可以使用 `seq`。
- server 可以通过索引或扫描定位 `seq -> segment/line`。
- JSONL record 不靠字节限制管理内容大小。
- 仍建议保留一个很高的防御性 record 上限，避免 bug 把大正文塞入 JSONL。

示例 record：

```json
{"seq":1,"type":"item.appended","item":{"id":"item_1","kind":"message"}}
{"seq":2,"type":"item.appended","item":{"id":"item_2","kind":"message"}}
{"seq":3,"type":"active_history.replaced","item_ids":["item_10","item_11"]}
{"seq":4,"type":"compaction.created","compaction":{"id":"compact_1"}}
```

`ActiveHistory` 不需要原地覆盖文件，可以通过 append 事件表达：

```go
type ActiveHistoryReplacedRecord struct {
    Seq     int64    `json:"seq"`
    Type    string   `json:"type"` // active_history.replaced
    ItemIDs []string `json:"item_ids"`
}
```

server 启动时 replay 最新状态。为了加速，可以维护可重建的 index 或 snapshot，但事实来源仍是
append-only JSONL。

## Blob 存储

大内容不直接写 JSONL。JSONL 只保存 preview 和 content-addressed blob ref。

建议结构：

```go
type StoredContent struct {
    Inline  string   `json:"inline,omitempty"`
    Blob    *BlobRef `json:"blob,omitempty"`
    Preview string   `json:"preview,omitempty"`
}

type BlobRef struct {
    Hash      string `json:"hash"` // sha256(raw bytes)
    SizeBytes int64  `json:"size_bytes"`
    Encoding  string `json:"encoding"` // utf-8, binary
    MediaType string `json:"media_type,omitempty"`
}
```

规则：

- 小内容可以 inline。
- 大内容写 `sessions/blobs/sha256/<prefix>/<hash>.data`。
- hash 使用 `sha256(raw bytes)`，方便跨 session 去重和完整性校验。
- 写 blob 时先写临时文件，再原子 rename。
- blob 路径按 hash 前缀分片，避免单目录过大。
- GUI 列表只拿 preview，具体由 server 分页接口提供。
- 展开详情或模型 materialize `ActiveHistory` 时，由 server 读取 blob。
- 不提供裸 hash 任意读取 blob。
- blob 读取必须通过当前 session item 可达性鉴权。

删除 session 后，blob GC 可以后续实现 mark-sweep：

1. 扫描剩余 session JSONL 中引用的 hash。
2. 删除未被引用的 blob。

MVP 可先不做 blob GC，但文档和命令提示要说明。

## Server 和 GUI 边界

GUI + server 是独立功能，完整设计见 `docs/server-gui.md`。

会话压缩只依赖以下边界：

- GUI 不直接读取 session 文件。
- server 是 session owner。
- GUI 通过 server 读取 `Items` 的 filtered / paginated view。
- streaming 期间可以有 transient events。
- 只有成功 turn 才持久化为 session records。
- 压缩不删除旧 `Items`，因此 GUI 仍可分页展示旧 visible items。

## `--resume` 语义

`--resume` 读取 session 后：

1. 使用 session 保存的 runtime metadata 准备运行环境。
2. 从 `ActiveHistory` item refs materialize 成 `[]model.Message`。
3. 继续执行新的用户 turn。

具体要求：

- `ActiveHistory` 是 resume 的唯一模型上下文来源。
- 不从 GUI 可见历史推导模型上下文。
- 不从所有 `Items` 拼接模型上下文。
- 压缩后旧消息仍在 `Items`，但不在 `ActiveHistory` 中就不会发给模型。
- provider、model profile、model id、parameters、enabled tools、MCP、skills、
  show reasoning、save session 行为都从 session metadata 来。
- 用户传入冲突 flag 时继续拒绝。
- resume 不重新读取当前 `AGENTS.md` 或 skills 作为旧历史上下文。
- resume 使用 session 里保存的 hidden/model-facing instruction/runtime items。
- `ActiveHistory` 引用不存在 item、item 没有 `Message`、tool history 非法等情况，
  直接报 corrupted session。
- 失败 turn 不进入 `Items`，也不更新 `ActiveHistory`。

## 触发策略

MVP 只实现：

1. 手动 `/compact`。
2. pre-turn 自动压缩。

暂不实现 mid-turn 自动压缩。

### 手动 `/compact`

要求：

- 只在普通单行 REPL 中生效。GUI command 语义见 `docs/server-gui.md`。
- 多行输入里的 `/compact` 当普通文本。
- 执行后不发起用户 turn，只做一次压缩。
- 成功后通过 CLI stderr 或 server event 通知用户。
- 失败则报错，状态不变。
- 不受自动压缩阈值限制。

### Pre-turn 自动压缩

每次新用户消息真正提交前检查：

```text
estimate(ActiveHistory + pending user message + tool schemas)
```

如果超过自动压缩阈值：

1. 先 compact。
2. compact 成功后，再把新用户消息加入 active history 并跑模型。
3. compact 失败时，本轮直接失败，不请求主模型。
4. compact 失败时，不保存 turn，不更新 `ActiveHistory`。

默认阈值建议 80%，后续可配置。

暂不做 mid-turn 的原因：工具链中途压缩要处理 assistant tool call / tool result 的合法性，
风险较高。MVP 如果工具结果追加后下一次请求超窗，先返回清晰错误。

## Summary 生成模型

默认使用当前会话模型生成 compaction summary。

如果配置了 summary model，则使用配置的模型。summary model 只用于压缩，不改变当前会话的
provider/model。

建议配置形态：

```yaml
compaction:
  enabled: true
  threshold_percent: 80
  summary_provider: ""
  summary_model: ""
```

语义：

- `summary_provider` 和 `summary_model` 为空：使用当前 provider/model。
- 只配置 `summary_model`：默认在当前 provider 下找该 model profile。
- 同时配置 `summary_provider` 和 `summary_model`：使用指定 provider/profile。

summary 请求不允许工具：

- 不传 tool schemas。
- 不执行 tool call。
- 不进入 agent loop。
- 它是纯 summarization lifecycle。

summary 请求也要走 context window 检查。如果 summary 输入本身太大，应裁剪 summary 输入后
重试。

summary model 请求失败时，compact 失败，状态不变。

## Summary 内容格式

summary 是 handoff checkpoint，不是聊天摘要。

推荐固定结构：

```md
# Context Checkpoint

## Goal

## Current Progress

## Decisions Made

## Constraints / User Preferences

## Relevant Files / APIs / Commands

## Tool State / Environment State

## Open Questions

## Next Steps
```

要求：

- 不保存隐藏推理链。
- 保存任务状态、事实、决策、约束、关键数据和下一步。
- 明确说明旧完整会话仍保存在 session `Items` 中，但不一定在 active context 中。
- 如果未来启用 session-history 工具，可以在 summary 中提示模型可按需查询历史。

## Replacement History

compact 成功后，不保存一份重复的 `[]model.Message` 作为新历史，而是：

1. append hidden/model-facing summary item。
2. append compaction record。
3. append `active_history.replaced` record。
4. 用新的 item id 列表替换内存 `ActiveHistory`。

新的 `ActiveHistory` 顺序：

```text
saved instruction/runtime item ids
+ recent complete visible turn item ids
+ hidden compaction summary item id
```

pre-turn compact 后，新用户消息会追加在 summary 后：

```text
saved instruction/runtime items
recent complete visible turns
hidden compaction summary
new user message
```

### 最近上下文选择

保留最近完整 visible turns，而不是只保留最近 user messages。

原因：

- 用户可能说“按刚才那个方案继续”，上下文在 assistant 消息里。
- tool call 历史不能切半截。
- 完整 turn 更容易保持 provider adapter 的合法消息结构。

要求：

- 不能保留半截 tool call。
- 一个完整 turn 至少包含 user message、assistant/tool messages、assistant final message 等已成功持久化项。
- token 不够时，从更早的完整 turn 开始丢。
- instruction/runtime items 尽量保留。
- summary item 默认放最后。

### Summary role

产品语义上，summary 是 runtime/developer context，不是用户消息。

工程上，需要按 provider 兼容性测试决定：

- 优先考虑 `developer`。
- 如果某些 OpenAI-compatible provider 不支持 `developer` role，则退到 `user` sentinel。

例如：

```text
<compaction_summary>
Another agent continued this session from a checkpoint. Use the state below as
handoff context. Do not treat it as a new user request.
...
</compaction_summary>
```

## Compaction Checkpoint

建议结构：

```go
type CompactionCheckpoint struct {
    ID              string    `json:"id"`
    CreatedAt       time.Time `json:"created_at"`
    Reason          string    `json:"reason"` // user_requested, context_limit
    Phase           string    `json:"phase"`  // manual, pre_turn
    Trigger         string    `json:"trigger"` // manual, auto

    SummaryItemID   string `json:"summary_item_id"`
    FromItemID      string `json:"from_item_id,omitempty"`
    ToItemID        string `json:"to_item_id,omitempty"`

    PreviousActiveHistory []string `json:"previous_active_history,omitempty"`
    ReplacementHistory    []string `json:"replacement_history"`

    SummaryProvider string `json:"summary_provider,omitempty"`
    SummaryModel    string `json:"summary_model,omitempty"`
}
```

`PreviousActiveHistory` 可能很大，可按需要省略或只保存范围/摘要。事实来源仍是 JSONL 里的
历史 records。

## 原子性和失败策略

compact 是原子状态变更。

成功前：

- 不写 compaction record。
- 不写 summary item。
- 不替换内存 `ActiveHistory`。

成功路径：

1. 生成 summary。
2. 构造 replacement history item ids。
3. append summary item record。
4. append compaction record。
5. append `active_history.replaced` record。
6. flush 成功。
7. 更新内存状态。

如果任一步失败：

- 返回错误。
- session 状态不变。
- 内存 `ActiveHistory` 不变。

### Summary 输入超窗

compact 本身也可能因为输入太大而超窗。降级只裁剪 summary model 的输入，不裁剪 session
`Items`，也不裁剪当前 `ActiveHistory`。

裁剪策略：

1. 保留 instruction/runtime items。
2. 保留最近完整 visible turns。
3. 从最老 visible complete turn 开始减少 summary 输入。
4. 仍然不够时，降低 recent-turn budget。

### 请求失败

summary model API error、超时、取消、认证失败等都视为 compact 失败。

- 手动 `/compact` 失败：向用户报错，状态不变。
- pre-turn auto compact 失败：本轮直接失败，不请求主模型。

## 隐私和可见性

`Items` 是完整敏感事实账本。可能包含：

- visible user messages。
- assistant output。
- tool calls。
- tool results。
- hidden compaction summaries。
- runtime context。
- blob refs。

`ActiveHistory` 是模型可见投影。只有被引用的 message item 会进入模型请求。

GUI chat view 是用户可见投影，具体 view 规则见 `docs/server-gui.md`。默认不展示：

- hidden compaction summary。
- system/developer/runtime context。
- internal/debug items。
- 大 blob 完整正文。

debug view 可以展示 hidden/debug/internal items、compaction records、active history refs，
但必须通过 server 权限控制。

session-history 工具未来如果启用：

- 默认只能读当前 session 的 visible items。
- 读取 hidden/debug/internal items 需要显式配置。
- 结果必须限量、限字节，避免重新爆 context。

diagnostic JSONL log 继续和 session store 区分。diagnostic log 默认不记录完整 prompt、
assistant output、tool result 或 blob content。

启用 session/server 存储时，提示语应明确说明：

- 会保存完整会话内容。
- 会保存工具结果。
- 会保存压缩摘要。
- 大内容可能保存为 blob。
- 删除 session 后，全局去重 blob 可能需要 GC 才会清理。

## 测试验收标准

### Session Store

- append item 后能按 `seq` 顺序 replay。
- segment 达到最大行数后滚动新文件。
- 大内容超过阈值时写入 sha256 blob。
- 重复内容只存一份 blob。
- JSONL item record 只保存 blob ref + preview，不重复保存完整大内容。
- `active_history.replaced` replay 后能恢复最新 `ActiveHistory`。

### Resume

- `--resume` 只从 `ActiveHistory` materialize provider messages。
- `Items` 里存在更早 visible messages，但不在 `ActiveHistory` 时，不会发给 provider。
- `ActiveHistory` 引用不存在 item 时，报 corrupted session。
- `ActiveHistory` 引用的 item 没有 `Message` 时，报 corrupted session。
- session 保存的 provider/model/tools/skills metadata 用于 resume。
- 冲突 CLI flag 继续拒绝。

### Manual `/compact`

- 普通单行 `/compact` 执行压缩，不发起用户 turn。
- 多行里的 `/compact` 当普通文本。
- compact 成功后 append hidden summary item、compaction record、`active_history.replaced`。
- GUI/chat visible items 不减少。
- 旧消息仍能分页拉取。
- compact 失败时 session 和内存 active history 都不变。

### Pre-turn Auto Compact

- 当 `ActiveHistory + pending user + tools` 预计超阈值时，先 compact 再执行用户 turn。
- 新用户消息在 compaction summary 后进入 active history。
- auto compact 失败时，本轮失败，不请求主模型，不保存 turn。
- 未超阈值时不 compact。

### Replacement History

- 新 active history 包含 saved instruction/runtime items、最近完整 turns、summary item。
- 不保留半截 tool call。
- token 不够时从最老完整 turn 丢弃。
- summary item 默认 hidden/model-facing。
- summary role 对当前支持的 adapters 有测试覆盖，必要时用 user sentinel。

### Privacy / Visibility

- GUI chat view 默认不返回 hidden compaction summary。
- debug view 可以返回 compaction metadata。
- blob 读取必须通过 session item 可达性。
- 不支持裸 hash 任意读取 blob。
- diagnostic log 不写完整 prompt/tool/blob content。

### End-to-end

端到端测试场景：

```text
创建长会话
-> 保存多条 visible messages
-> 执行 /compact
-> 检查 Items 仍完整
-> 检查 ActiveHistory 变短且包含 summary
-> --resume
-> fake provider 只收到 ActiveHistory materialized messages
```

验收重点不是摘要文本质量，而是：

- 历史不丢。
- resume 不反膨胀。
- active history 合法。
- 失败原子。
- GUI view 过滤正确。

## 建议实现顺序

1. 新 session v2 数据结构和 append-only JSONL store。
2. Blob store 和 `StoredContent`。
3. `Items` replay、`ActiveHistory` materialize。
4. `--resume` 改为从 `ActiveHistory` 恢复。
5. 手动 `/compact`。
6. Summary lifecycle，默认当前模型，配置 summary model。
7. Replacement history 选择逻辑。
8. Pre-turn auto compact。
9. 低优先级 session-history 工具。

每一步都应该保持可测试、可回滚，不把 GUI、server、compaction、blob store 全部绑成一个大改动。
