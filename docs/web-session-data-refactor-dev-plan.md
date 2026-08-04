> **Stage 6 completed (HEAD review target).** The settlement state machine now
> uses `run.settled.committed_revision` (with a legacy `last_seq` fallback) as
> a precision-safe completeness watermark. A covered settlement tears down
> only transient run state; it does not issue a snapshot. The old
> `recentStepsByTurn` post-settlement process bridge has been removed because
> durable assistant/tool items are already projected by the backend. When
> `show_reasoning` is enabled, persisted assistant reasoning is also projected
> into the item DTO, so terminal reasoning is not a client-side bridge. The
> remaining live process row is intentionally limited to non-durable reasoning
> and tool progress. The older run-state sketches below are historical
> planning notes where they mention unconditional settled refreshes or saving
> steps into `recentStepsByTurn`; § 6 records the superseding implementation.
>

---

## 0. 现状与问题（原计划缺失，此处补齐）

### 当前链路

| 层 | 现状 | 代码位置 |
|---|---|---|
| 后端 detail | `GET /api/sessions/{id}` → `Service.GetSession` → `sessionStore.Load` → `sessionDetailFromStore` | `server.go:handleGetSession`、`service.go:675` |
| 后端 items | `GET /api/sessions/{id}/items` → `Service.GetSessionChatItemsPage` → 独立 `sessionStore.Load` | `server.go:handleSessionItems`、`session_events.go:239` |
| 前端 detail+page | `useSessionHistory` 内 `Promise.all([api.session(id), api.items(id)])` 两个独立请求 | `useSessionHistory.ts:75` |
| 前端权威状态 | `useSessionHistory` 持有 `sessionDetail` / `itemsPage` / `conversationCacheRef` 三套独立 state+ref | `useSessionHistory.ts:15-35` |
| 前端列表状态 | `App.tsx` 持有 `sessionsByProject` / `archivedSessionsByProject`，无 generation 保护 | `App.tsx` |
| run 归并 | `useRunRegistry` + `App.handleRunEvent`；settled 后 `await refreshSession` 再 `update(() => null)` 移除 run；refresh 失败仍移除 | `App.tsx` `run.settled` 分支、`useRunRegistry.ts:updateActiveRun` |
| 身份传递 | `Conversation` 通过 `props.detail?.id` 推断 sessionID，scroll memory 也从 `props.detail?.id` 取 | `Conversation.tsx:59-64` |
| Conversation memo | `Conversation` 使用**自定义** memo comparator（浅比较部分 props），未包含 `sessionID` | `Conversation.tsx:375-386` |
| 版本标识 | session 有 `last_seq`（单调 int64，record log seq，含 transaction records）、`UpdatedAt`（时间戳）；**无** revision 概念 | `v2.go:107,140` |

### 核心问题

1. **混合快照**：detail 和 items 是两次独立 `sessionStore.Load`，中间若发生 compaction/run settled，两次 load 看到的是不同状态。
2. **多权威源**：`useSessionHistory` 的 `sessionDetail`/`itemsPage`/`conversationCacheRef` 与 `App` 的列表 state 各自独立，无统一归并点。
3. **settled overlay 移除不可靠**：`handleRunEvent` 的 `run.settled` 分支虽然 `await refreshSession` 后才 `update(() => null)`，但 refresh **失败时仍移除** overlay，用户看到空白或旧数据。
4. **stream error 清理不可靠**：`startNewRun` 的 catch 分支 `updateActiveRun(sessionID, runID, () => null)` 中 `runID` 取自 `activeRunsRef.current[sessionID]?.id`，若此时已有新 run，会误清新 run。
5. **列表无 generation 保护**：`loadSessions` 没有按 project generation 丢弃旧响应，乱序返回会覆盖新列表。首屏 bootstrap 和 1.5s coordinator poll 也无保护。
6. **身份从 detail 推断**：`Conversation` 内 `sessionIDRef.current = props.detail?.id ?? ''`，detail 延迟到达时 sessionID 为空，scroll memory 和 resend 绑定到错误身份。且自定义 memo comparator 未包含 `sessionID`，身份切换时可能跳过 re-render。
7. **command target 绑全局 selection**：`resendMessage`/`retryRun`/`cancelRun`/`sendMessage` 等闭包 `selectedSessionID`，异步完成时若 selection 已切换，操作打到错误 session。

---

## 1. 版本标识定义

### 1.1 两层标识，各司其职

| 标识 | 类型 | 来源 | 用途 | 生命周期 |
|---|---|---|---|---|
| `revision` | `string`（DTO 层，JS 安全） | = `strconv.FormatInt(session.LastSeq, 10)` | **session 级**状态比较：判断 snapshot 是否比本地新。 | 随每条 record 追加单调递增 |
| `last_seq` | `int64` | `session.LastSeq` | **session 级** durable settlement 水位：判断 durable store 是否已追上 run 的最终状态。settled overlay 只能在 `snapshot.session.last_seq >= run.settled.last_seq` 时移除。**注意**：`last_seq` 是 record log seq（含 `transaction.begin`/`commit`/`active_history.replaced`），**不等于** chat page 的 `newest_seq`（仅可见 chat items 的最大 seq）。 | 随每条 record 追加单调递增 |

**不再使用 `window_epoch`**（经代码验证取消）：
- compaction 追加 hidden summary item + 替换 `ActiveHistory`，但**不删除/改写**已有 visible chat items（`sessionItemVisibleInChat` 只过滤 `session.Items` 的 visibility/kind/audience，不看 `ActiveHistory`）。
- visible items 的 seq 在 compaction 后仍有效、连续可分页。
- 因此不存在"compaction 后旧窗口 seq 失效"的场景，`window_epoch` 无实际用途。
- history 窗口归并完全依赖 `revision`（`LastSeq`）+ 现有 overlap merge 逻辑（`mergeRefreshedPage`）：重叠则 merge prefix，不重叠则整体替换。

### 1.2 为什么用 `LastSeq` 作 revision

**`UpdatedAt` 不可靠**（经代码验证）：
- `UpdatedAt = now` 只出现在 `SaveMetadata`（`v2.go:353`）、`SaveTurn`（`v2.go:814`）、`SaveCompactedTurn`（`v2.go:870`）。
- `AppendItemsAndReplaceActiveHistoryFromState`（`v2.go:923-984`）追加 items + records 但**不碰 `UpdatedAt`**。

**为什么不新增 `ContentRevision` 字段**：
- `appendRecords`（`v2.go:1291`）签名是 `(sessionID string, records []v2Record) error`，只往 segment JSONL 追加行，**不读/写 `meta.json`**，没有 session 对象可修改。
- `sessionV2Metadata`（`v2.go:1476`）不含 `LastSeq`——`LastSeq` 是从 record log replay 中恢复的（`v2.go:507`），不存于 meta.json。新增 `ContentRevision` 需要同时改 meta.json 结构 + 所有写入路径 + replay 逻辑，改动面大且易遗漏。
- `LastSeq` 已经在**所有写入路径**中单调递增（每条 record 一个 seq），且 `Load` 后自然恢复，无需新字段或改 store 结构。
- metadata-only 变更（如 rename）不改变 `LastSeq`，但这些变更不影响 history 内容，snapshot 归并中 history 窗口不受影响（只更新 `sessionsByID` 中的 session 对象）。

**`UnixNano()` 精度问题**：
- `UnixNano()` ~1.7e18 > `Number.MAX_SAFE_INTEGER`（9e15）。前端 `number` 比较会丢精度。

**方案**：
- snapshot DTO 的 `revision` 字段类型为 Go `string`，值 = `strconv.FormatInt(session.LastSeq, 10)`。
- 前端比较用 `BigInt(revisionA) > BigInt(revisionB)`，**禁止字符串字典序比较**（否则 `"9" > "10"`）。
- 前端 `revision` 类型为 `string`。

### 1.3 `last_seq` 与 `newest_seq` 的区别（关键）

```
session.LastSeq = 最后一条 record 的 seq，包含：
  - transaction.begin
  - item.appended (可见 + 不可见)
  - active_history.replaced
  - transaction.commit

history.newest_seq = chat page 中可见 chat items 的最大 item.seq

典型关系：session.LastSeq > history.newest_seq
  （因为最后几号 record 常是 active_history.replaced + commit，不是可见 message）

因此：
  - revision（snapshot 新旧判定）= session.last_seq
  - settlement 判定 = snapshot.session.last_seq >= run.settled.last_seq
  - 两者都用 session.last_seq，不用 history.newest_seq
```

### 1.4 归并规则

```
snapshot 到达时：
  if snapshot.session_id !== expectedSessionID: reject（不变量 1）
  if localRevision exists && BigInt(snapshot.revision) <= BigInt(localRevision): discard（旧快照不覆盖新快照，不变量 3）
    （本地无 entry 时 localRevision 为 undefined，直接 accept）
  merge history window by item seq:
    保留 local 中 seq < snapshot.oldest_seq 的旧 items（用户已翻页的部分），
    用 snapshot items 替换 tail，按 seq 去重
    （与现有 mergeRefreshedPage 逻辑一致：重叠则 merge prefix，不重叠则整体替换）
  update localRevision = snapshot.revision
  update sessionsByID[snapshot.session_id] = snapshot.session

pageOlder 到达时：
  prepend items（compaction 不改写 visible items，无需 epoch 校验）
```

---

## 2. Run 状态机

### 2.1 状态定义

```
                    ┌─────────┐
  startRun ───────▶ │ active  │
                    └────┬────┘
                         │
         ┌───────────────┼───────────────┐
         │               │               │
   run.settled       turn.failed      stream error
   (committed)       (terminal)       (无 settledLastSeq)
         │               │               │
         ▼               ▼               ▼
  ┌────────────┐   ┌─────────┐   ┌──────────────────────┐
  │reconciling │   │ failed  │   │error_pending_refresh │
  └─────┬──────┘   │(refresh │   └──────────┬───────────┘
        │          │ →remove)│              │
  onSnapshotApplied └────┬───┘     refresh 成功?
  session.last_seq        │         ├─有 settledLastSeq:
  >= settled.last_seq     │         │  onSnapshotApplied 检查水位
        │                 │         │  ├─追上→ removed
        ▼                 │         │  └─未追上→ reconciling + backoff
   ┌─────────┐            │         └─无 settledLastSeq (stream error):
   │ removed │            │            refresh 成功即 removed
   └─────────┘            │         失败: 保持 error_pending_refresh
                          │           手动 refresh 按钮(§2.7)
                     run.settled
                     (failed) 到达
                          │
                     refresh→removed

  run.settled(cancelled) → refresh→removed（不经过 reconciling，不依赖 last_seq）
  新 run 覆盖旧 run → 先 saveRecentStepsAndRemove 旧 run（§2.3）
```

### 2.2 按 settled status 分支的移除策略

| `run.settled` status | 行为 | 原因 |
|---|---|---|
| `committed` | 进入 `reconciling`，等 `snapshot.session.last_seq >= settled.last_seq` 后移除 | durable items 是最终展示内容，必须确保已写入 |
| `cancelled` | 触发一次 `refresh`（更新 session 状态），refresh 完成后立即移除 | cancelled 无新 durable 内容，不需要追 last_seq |
| `failed` | `turn.failed` 已在 UI 展示 error；`run.settled(failed)` 到达后触发一次 `refresh`，然后移除 transient overlay | error 已展示，durable 状态已刷新即可 |

**`settled.last_seq` 缺失或为 0 的处理**：
- `committed` 且 `last_seq=0`：视为异常，进入 `reconciling` 但不依赖 `last_seq` 比较，仅等 refresh 成功后移除（refresh 成功即说明 durable 已写入）。
- `cancelled`/`failed` 且 `last_seq=0`：正常路径，refresh 后移除。

### 2.3 并发约束：新 run 覆盖旧 run

**关键约束：同 session 单 run 槽**（与现有 `useRunRegistry` 的 `activeRunsBySession: Record<string, ActiveRun>` 和后端 `run_coordinator.go` 一致）。

**覆盖前必须保存旧 run 的 steps**（避免丢 turn）：

```
addActiveRun(newRun) 时，若槽位中已有旧 run：
  1. 若旧 run.status === 'running'：
     → 不允许覆盖（后端 coordinator 单 session 单 run，不会出现此情况）
  2. 若旧 run.status ∈ {reconciling, error_pending_refresh, failed, cancelled}：
     → 调用 saveRecentStepsAndRemove(sessionID, oldRunID)
       （保存 steps 到 recentStepsByTurn，从 registry 移除）
     → 触发一次 refreshSession(sessionID)
       （刷新 durable，尽量让旧 turn 的 durable items 写入）
     → 然后 addActiveRun(newRun) 进入槽位
```

这保证了旧 turn 的 transient steps 不会随覆盖丢失，且 durable refresh 尽量补齐旧 turn 内容。

**其他场景**：

| 场景 | 规则 |
|---|---|
| stream error 后新 run | `error_pending_refresh` 的旧 run 的清理只匹配**绑定的 runID**（stream 启动时捕获），不读 `activeRunsRef.current[sessionID]?.id`。新 run 覆盖时走上述保存流程。 |
| reconciling 超时 | 60s 后标记 `error_pending_refresh`（不移除 run）。保留 overlay + 手动 refresh 按钮。`onSnapshotApplied` 同时处理 `reconciling` 和 `error_pending_refresh` 两种状态的 settlement 检查（§2.6）。 |
| 同 session 多个 settled | 按 runID 分别处理，互不干扰。但同一时刻只有一个 run 在 registry 中。 |

### 2.4 reconciling 期间输入策略

**策略：reconciling/error_pending_refresh 期间允许新 run。**

| run status | `sendMessage` 行为 | Composer UI |
|---|---|---|
| `running` | `appendRunMessage`（追加到 in-flight turn） | 正常，显示 queued |
| `reconciling` | `startNewRun`（新 run 覆盖旧 run，走 §2.3 保存流程） | 正常输入 |
| `error_pending_refresh` | `startNewRun`（同上） | 正常输入 |
| `failed` | `startNewRun` | 正常输入 |
| `cancelled` | `startNewRun` | 正常输入 |
| 无 run | `startNewRun` | 正常输入 |

**改造 `sendMessage`**：
```typescript
const activeRun = activeRunsRef.current[sessionID]
if (activeRun && activeRun.status === 'running') {
  await api.appendRunMessage(activeRun.id, content)
  return true
}
return startNewRun(sessionID, content, imageInputs)
```

**`cancelRun`**：仅对 `status === 'running'` 有效；对 reconciling/error_pending_refresh 为 no-op（run 已在后端结束）。

### 2.5 当前代码改造点

| 当前代码 | 问题 | 改造 |
|---|---|---|
| `App.tsx` `run.settled` 分支：`await refreshSession` 后**无条件** `update(() => null)` | refresh 失败时仍移除 overlay | 按 §2.2 分支处理：committed 进入 `reconciling`，refresh 失败进入 `error_pending_refresh` |
| `App.tsx` `startNewRun`/`resendMessage`/`retryRun` catch：`runID` 取自 ref | 可能误清新 run | stream 启动时捕获 `const boundRunID = started.run_id`，catch 中用 `boundRunID` |
| `useRunRegistry.ts` 无 reconciling 态 | — | `ActiveRun.status` 增加 `'reconciling'`、`'error_pending_refresh'` |
| `useRunRegistry.ts` `addActiveRun` 直接覆盖 | 覆盖时不保存旧 run steps | 覆盖前调用 `saveRecentStepsAndRemove` + `refreshSession`（§2.3） |
| `App.tsx` `sendMessage` 只检查 `activeRunsRef.current[sessionID]` 存在 | reconciling 期间会走 `appendRunMessage` → 后端 `ErrSessionRunSettled` | 按 §2.4 改为仅 `status === 'running'` 时 append |
| `recentStepsByTurn` 保存时机 | 当前 settled 后先保存 steps 再 refresh | reconciling 期间**不**保存 steps（仍渲染 overlay）；仅 `saveRecentStepsAndRemove` 成功移除 run 时才写入 `recentStepsByTurn` |
| `Conversation.tsx` `running={Boolean(props.activeRun)}` | reconciling 仍显示 Stop 按钮 | `running={activeRun?.status === 'running'}` |
| `Conversation.tsx` ActiveRunView "Generating" | reconciling 仍显示 Generating | 仅 `status === 'running'` 时显示 Generating/cancel |
| `useRunRegistry.ts` `runningSessionIDs` | reconciling 期间 session 仍在 running 集合 | `runningSessionIDs` 仅统计 `status === 'running'` 的 run |
| `Conversation.tsx` Compact 按钮 `disabled` | `Boolean(props.activeRun)` → reconciling 时仍禁用 | `disabled` 改为 `activeRun?.status === 'running'` |
| `App.tsx` Archive/Delete `disabled` | `Boolean(activeRunsRef.current[session.id])` → reconciling 时仍禁用 | 改为 `activeRunsRef.current[session.id]?.status === 'running'` |

### 2.6 reconciling 重试与 settlement 检查

committed 且首次 refresh 后 `durableLastSeq < settledLastSeq` 时：
- 保留 `reconciling` 状态。
- schedule 1 次 backoff refresh（2s 后），最多重试 2 次。
- 仍追不上则等 60s 超时 → `error_pending_refresh`。

**统一 settlement 检查点**：`onSnapshotApplied(sessionID, session)` hook（在 store dispatch `snapshot` action **成功 apply** 后调用，discarded snapshot 不触发）：
```typescript
function onSnapshotApplied(sessionID: string, session: Session) {
  const run = activeRunsRef.current[sessionID]
  if (!run) return
  if (run.status !== 'reconciling' && run.status !== 'error_pending_refresh') return

  // 无水位（stream error / 超时兜底）：refresh 成功即移除
  if (run.settledLastSeq == null || run.settledLastSeq === 0) {
    saveRecentStepsAndRemove(sessionID, run.id)
    return
  }
  // 有水位：检查 durable 是否追上
  if (session.last_seq >= run.settledLastSeq) {
    saveRecentStepsAndRemove(sessionID, run.id)
  } else {
    // 未追上：回 reconciling + backoff（仅 error_pending_refresh 需要回迁）
    if (run.status === 'error_pending_refresh') {
      updateActiveRun(sessionID, run.id, (r) => ({ ...r, status: 'reconciling' }))
    }
    scheduleReconcileRetry(sessionID, run.id, run.settledLastSeq)
  }
}
```

backoff 重试、手动 refresh、切回 session 的 `refreshSession` 共用此 hook。failed/cancelled 是例外：它们在 C.2 中 refresh 后直接 `saveRecentStepsAndRemove`，不进入 `reconciling` 状态，不经过 hook（因为它们的 status 仍为 `failed`/`cancelled`，`onSnapshotApplied` 的 status 检查会跳过）。

### 2.7 error_pending_refresh 手动 refresh

`error_pending_refresh` 时 UI 显示 "refresh to see latest" 按钮：
- 点击后调用 `retryRefreshSession(sessionID)`（C.5）。
- `retryRefreshSession` 调用 `refreshSession`，内部触发 `onSnapshotApplied`（§2.6）统一处理：
  - 有 `settledLastSeq`：检查水位，追上则 `saveRecentStepsAndRemove`；未追上则回 `reconciling` + backoff。
  - 无 `settledLastSeq`（stream error / 超时兜底）：refresh 成功即 `saveRecentStepsAndRemove`。
- refresh 失败：保持 `error_pending_refresh`，显示 error。
- 与 `turnErrors` / ErrorBanner 分工：`turnErrors` 展示 turn 级失败信息；`error_pending_refresh` 的按钮是 session 级 refresh 操作。

### 2.8 reconciling 期间 UI 行为

- reconciling run 仍在 `activeRunsBySession` 中显示 transient process state；durable items 仍由 shared projection store 渲染，不再按文本/turn matching 隐藏。
- `ProcessTimeline` / `ActiveRunView` 显示 run steps，但**不显示** "Generating..." / cancel 按钮（`status !== 'running'`）。
- `Conversation` 的 `running` prop = `activeRun?.status === 'running'`（reconciling 时为 false，不显示 Stop 按钮）。
- `runningSessionIDs`（sidebar 指示器）仅统计 `status === 'running'` 的 run。
- `error_pending_refresh` 时显示 error banner + "refresh to see latest" 按钮（在 Conversation 内渲染，非全局 ErrorBanner）。
- 新 run 覆盖时走 §2.3 保存流程后直接切换到新 run 的 UI。

---

## 3. 工作包（对照代码细化）

### WP-A：后端聚合快照协议

#### A.1 新增 `GET /api/sessions/{sessionID}/snapshot`

**文件**：`internal/webapp/server.go`（路由）、`internal/execution/service.go`（service 方法）

```
Service.GetSessionSnapshot(id string) (SessionSnapshot, error)
  → session, err := sessionStore.Load(id)   // 单次 load
  → detail := sessionDetailFromStore(session)
  → page := buildItemsPage(session, defaultLimit, alignTurn=true)  // 复用提取的公共逻辑
  → revision := strconv.FormatInt(session.LastSeq, 10)             // string，JS 安全
  → return SessionSnapshot{SessionID: id, Revision: revision, Session: detail, History: page}
```

**SessionSnapshot DTO**（Go 端，`revision` 为 `string` 避免 JS 精度丢失）：
```go
type SessionSnapshot struct {
    SessionID string           `json:"session_id"`
    Revision  string           `json:"revision"`  // = FormatInt(session.LastSeq, 10)
    Session   SessionDetail    `json:"session"`   // 含 last_seq
    History   SessionItemsPage `json:"history"`
}
```

**响应示例**：
```json
{
  "session_id": "...",
  "revision": "142",
  "session": { "id": "...", "last_seq": 142, ... },
  "history": { "items": [...], "oldest_seq": 100, "newest_seq": 139, ... }
}
```

**注意**：`buildItemsPage` 从已加载的 `session` 派生 page，不再独立 Load。复用 `sessionItemDTO`（可能读 blob），失败语义与现 `GetSessionChatItemsPage` 对齐。

**不需要改 `SessionV2` 或 `sessionV2Metadata`**：revision 直接从 `session.LastSeq` 派生，`Load` 后已恢复。

**保留旧接口**：`GET /api/sessions/{id}` 和 `GET /api/sessions/{id}/items` 不删除（迁移期共存，见 §5）。

#### A.2 测试

- `internal/webapp/server_behavior_test.go` 新增 `TestServerSessionSnapshot`：验证单次 load、revision = `LastSeq`、history 与 detail 一致。
- `internal/execution/service_test.go` 新增 `TestServiceGetSessionSnapshot`：验证 revision 随 item append 递增。
- 验证 revision 为 string 类型、不丢精度。

---

### WP-B：前端规范化 Session Store

#### B.1 新增 `web/src/lib/sessionStore.ts`（reducer 驱动）

**State 结构**：
```typescript
interface SessionStoreState {
  sessionsByID: Record<string, Session>
  sessionIDsByProject: Record<string, { active: string[]; archived: string[] }>
  historyBySession: Record<string, { page: ItemsPage; revision: string }>
  metaBySession: Record<string, { loading: boolean; error: string; refreshGeneration: number }>
  listGenerationByProject: Record<string, number>
}
```

**LRU 淘汰**：`historyBySession` 保留最近 10 个 session 的 history（与现 `conversationCacheRef` 的 LRU cap=10 一致），超出时淘汰最旧。`clearSession` action 用于 archive/delete 时主动清理。

**Actions**：
```typescript
type Action =
  | { type: 'snapshot'; snapshot: SessionSnapshot; expectedSessionID: string }
  | { type: 'sessions'; projectID: string; sessions: Session[]; archived: boolean; generation: number }
  | { type: 'pageOlder'; sessionID: string; older: ItemsPage }
  | { type: 'setMeta'; sessionID: string; loading?: boolean; error?: string }
  | { type: 'clearSession'; sessionID: string }
```

**归并逻辑**（reducer 内实现 §1.4 规则）：
- `snapshot`：校验 `session_id`，比较 `revision`（`BigInt(snapshot.revision) <= BigInt(localRevision)` 则 discard），按 §1.4 merge history window（与现有 `mergeRefreshedPage` 逻辑一致）。
- `sessions`：比较 `generation`，旧 generation 丢弃。
- `pageOlder`：prepend items（compaction 不改写 visible items，无需 epoch 校验）。

#### B.2 改造 `useSessionHistory` → 改为 store 消费者

**当前**：`useSessionHistory` 持有 `sessionDetail`/`itemsPage`/`conversationCacheRef` 三套权威状态。

**改造后**：
- `useSessionHistory` 保留为 UI 层 hook，但**不再持有权威 state**。
- 它从 `useSessionStore`（基于 `sessionStore.ts` reducer）读取 detail/page。
- `refreshSession` 改为 fetch snapshot + dispatch `snapshot` action，**始终返回 fetched `{ session: Session, history: ItemsPage }`**（discard 时仍返回 fetched 数据，因为 fetched session 携带的 `last_seq` 对 settlement 检查有效）。**请求失败时 throw**（不返回 null），由调用方 catch 处理。仅在 store **成功 apply** snapshot 后调用 `onSnapshotApplied`（§2.6 settlement 检查）；discard 时不触发 hook，但返回值仍可用于 C.2 兜底水位检查。
- `refreshSession` 保留 `loadSessions(detail.project_id)` 调用（更新 sidebar 状态/last_used）。
- `loadOlder` 改为 fetch + dispatch `pageOlder`。
- `conversationCacheRef` 移除——store 的 `historyBySession` 已天然缓存（含 LRU 淘汰）。

**文件**：`web/src/hooks/useSessionHistory.ts`（改造）、`web/src/hooks/useSessionStore.ts`（新增）

#### B.3 App.tsx 列表 generation 保护（覆盖所有写入路径）

**当前**：`loadSessions` 无 generation。

**改造**：generation 保护覆盖**所有**写 `sessionsByProject` 的路径：
- `loadSessions`
- 首屏 bootstrap `useEffect`
- 1.5s coordinator poll `syncCoordinatorRuns`

```typescript
const loadSessions = async (projectID, preferredSessionID, preserveSelection) => {
  const generation = (listGenerationRef.current[projectID] ?? 0) + 1
  listGenerationRef.current[projectID] = generation
  const [payload, archivedPayload] = await Promise.all([...])
  if (listGenerationRef.current[projectID] !== generation) return // 丢弃旧响应
  dispatch({ type: 'sessions', projectID, sessions: payload.sessions, archived: false, generation })
  ...
}
```

**文件**：`web/src/App.tsx`

#### B.4 dual-fetch fallback

snapshot 接口 404 时回退到 `api.session` + `api.items`，合成：
- `revision = String(session.last_seq)`（从 `Session.last_seq` 取）
- 走正常 §1.4 归并规则（不特殊处理，`revision` 与 snapshot 路径一致）。
- fallback 路径的 snapshot 不应被 store 当作 revision=0 丢弃（`session.last_seq` 始终 > 0 除非空 session）。

#### B.5 测试

- `web/src/lib/sessionStore.test.ts`（新增）：
  - snapshot 乱序到达：旧 revision 不覆盖新。
  - revision 为 string 的 JS 安全比较（`BigInt("9") < BigInt("10")`，非字典序）。
  - sessionID mismatch 被拒绝。
  - list generation 旧响应被丢弃。
  - LRU 淘汰：超过 10 个 session 后最旧被淘汰。
  - fallback revision（`String(session.last_seq)`）不被丢弃。
  - snapshot merge：重叠时保留 prefix，不重叠时整体替换（与 `mergeRefreshedPage` 一致）。
- 改造 `web/src/hooks/useSessionHistory.test.ts`：适配 store 消费模式。

---

### WP-C：运行态归并

#### C.1 ActiveRun 增加 reconciling 态

**文件**：`web/src/types.ts`

```typescript
export interface ActiveRun {
  ...
  status: 'running' | 'failed' | 'cancelled' | 'reconciling' | 'error_pending_refresh'
  settledLastSeq?: number  // run.settled 携带的 last_seq，用于判断 durable 是否追上
}
```

#### C.2 改造 `handleRunEvent` 的 `run.settled` 分支

**当前**：`await refreshSession` 后无条件 `update(() => null)`。

**改造后**（按 §2.2 分支，**进入 reconciling 时只改 status，不保存 steps**）：
```typescript
case 'run.settled': {
  const settledLastSeq = Number(event.last_seq ?? 0)
  const settledStatus = String(event.status)

  if (settledStatus === 'failed') {
    // turn.failed 通常已展示 error；若 late-attach（turn.failed 未先到），此处兜底
    setTurnErrors((current) => current[sessionID]
      ? current
      : { ...current, [sessionID]: { turnID: String(event.turn_id ?? ''), message: String(event.message ?? 'Run failed') } })
    update(run => ({ ...run, status: 'failed', settledLastSeq }))  // 先标 failed，避免 refresh 窗口内 sendMessage 走 append
    try {
      await refreshSession(sessionID)  // refresh 但不进 reconciling，onSnapshotApplied 因 status 检查跳过
    } catch { /* ignore, error already shown */ }
    saveRecentStepsAndRemove(sessionID, runID)  // 移除时才保存 steps
    break
  }

  if (settledStatus === 'cancelled') {
    update(run => ({ ...run, status: 'cancelled', settledLastSeq }))  // 先标 cancelled，避免 refresh 窗口内 sendMessage 走 append
    try {
      await refreshSession(sessionID)
    } catch { /* ignore */ }
    saveRecentStepsAndRemove(sessionID, runID)
    break
  }

  // committed
  update(run => ({ ...run, status: 'reconciling', settledLastSeq }))  // 只改 status
  let result: { session: Session; history: ItemsPage } | null = null
  try {
    result = await refreshSession(sessionID)  // 始终返回 fetched { session, history }；失败时 throw（见下方"关键"说明）
    // onSnapshotApplied 在 refreshSession 内部已调用（§2.6），处理追上/未追上/backoff
    // 兜底：若 snapshot 被 store discard（revision <= local），onSnapshotApplied 不触发，
    // 但 refreshSession 仍返回 fetched session（含 last_seq），此处用返回值显式检查水位
    if (result?.session && activeRunsRef.current[sessionID]?.id === runID) {
      const durableLastSeq = result.session.last_seq ?? 0
      if (settledLastSeq === 0 || durableLastSeq >= settledLastSeq) {
        saveRecentStepsAndRemove(sessionID, runID)
      }
      // 未追上时 onSnapshotApplied（若触发过）已调度 backoff；
      // 若 onSnapshotApplied 未触发（discard），此处不重复调度，
      // 依赖 60s 超时或用户切回 session 时的 refreshSession 兜底。
    }
  } catch {
    update(run => ({ ...run, status: 'error_pending_refresh' }))
  }
  // completionNotice 逻辑保留：使用 result?.session 获取 session 名称
  if (String(event.status) === 'committed' && selectedSessionRef.current !== sessionID) {
    setCompletionNotice({
      sessionID,
      sessionName: result?.session ? sessionName(result.session) : `Session ${sessionID.slice(-6)}`,
    })
  }
}
```

**`saveRecentStepsAndRemove`**：保存 `recentStepsByTurn` 后 `update(() => null)`。仅在成功移除时调用。

**关键**：`refreshSession` 始终返回 **fetched** `{ session, history }`（含 `last_seq`），无论 store 是否 apply（discard 时仍返回 fetched 数据，因为 fetched session 携带的 `last_seq` 对 settlement 检查有效）。**请求失败时 throw**（不返回 null），由调用方 catch 处理。内部在 store **成功 apply** 后调用 `onSnapshotApplied` 检查是否可移除 run；discard 时不触发 hook，但返回值仍可用于 C.2 的兜底水位检查。不依赖从 React state 读陈旧快照。

#### C.3 stream error 清理修复

**当前**：catch 中 `runID` 取自 `activeRunsRef.current[sessionID]?.id`。

**改造后**：所有 stream 启动处捕获 `boundRunID`，catch 中用绑定值：
```typescript
const boundRunID = started.run_id
void streamRun(boundRunID, (event) => handleRunEvent(sessionID, boundRunID, event))
  .catch((reason) => {
    updateActiveRun(sessionID, boundRunID, (run) =>
      run ? { ...run, status: 'error_pending_refresh' } : null
    )
    setError(errorMessage(reason))
  })
```

修复位置：`startNewRun`、`resendMessage`、`retryRun`、recovered runs。

**stream error 的 `error_pending_refresh` 无 `settledLastSeq`**（§2.7）：手动 refresh 成功即 `saveRecentStepsAndRemove`，不检查水位。

#### C.4 reconciling 超时与重试

- `App.tsx` 中 run 进入 `reconciling` 时启动 60s 定时器，超时后标记 `error_pending_refresh`（不移除 run）。
- `scheduleReconcileRetry`：2s 后再 `refreshSession`，最多重试 2 次。仍追不上则等 60s 超时。
  - 重试计数存储在 ref 中（`reconcileRetryCountRef`），按 `(sessionID, runID)` 键。
  - 每次 `onSnapshotApplied` 成功 apply 但未追上时调度一次 backoff；若已有 pending backoff 则不重复调度。
  - run 被 remove/supersede/切 session 时 `clearTimeout` 并重置计数。

#### C.5 error_pending_refresh 手动 refresh handler

新增 `retryRefreshSession(sessionID)`：
```typescript
async function retryRefreshSession(sessionID: string) {
  const run = activeRunsRef.current[sessionID]
  if (!run || run.status !== 'error_pending_refresh') return
  try {
    await refreshSession(sessionID)  // 内部调用 onSnapshotApplied（§2.6）
    // onSnapshotApplied 统一处理移除/回迁逻辑，此处无需额外判断
  } catch {
    // 保持 error_pending_refresh
  }
}
```

`onSnapshotApplied`（§2.6）统一处理：有 `settledLastSeq` 则检查水位（追上移除/未追上回 reconciling + backoff），无 `settledLastSeq` 则 refresh 成功即移除。C.5 不再自行判断移除逻辑，全部委托 hook。

#### C.6 改造 `sendMessage`（§2.4）

```typescript
const activeRun = activeRunsRef.current[sessionID]
if (activeRun && activeRun.status === 'running') {
  await api.appendRunMessage(activeRun.id, content)
  return true
}
return startNewRun(sessionID, content, imageInputs)
```

#### C.7 改造 `addActiveRun` 覆盖逻辑（§2.3）

`useRunRegistry.ts` 的 `addActiveRun` 覆盖前保存旧 run，**拒绝覆盖 running run**：
```typescript
const addActiveRun = useCallback((run: ActiveRun): boolean => {
  const existing = activeRunsRef.current[run.sessionID]
  if (existing && existing.id !== run.id) {
    if (existing.status === 'running') {
      // 不允许覆盖正在运行的 run（后端 coordinator 单 session 单 run，不应出现此情况）
      return false
    }
    // 旧 run 为 reconciling/error_pending_refresh/failed/cancelled：
    // 先保存 steps + 触发 refresh（由 App 层回调处理），再覆盖
    onSupersedeRun?.(run.sessionID, existing.id)
  }
  publish({ ...activeRunsRef.current, [run.sessionID]: run })
  // runningSessionIDs 由 C.8 的 useMemo 从 activeRunsBySession 派生，此处不手动管理
  return true
}, [publish, onSupersedeRun])
```

`onSupersedeRun` 在 App 层实现：调用 `saveRecentStepsAndRemove`（保存 steps 到 `recentStepsByTurn` + 从 registry 移除旧 run）+ `refreshSession`（best-effort 刷新 durable）。`publish` 随后覆盖槽位放入新 run。

**invariant 5 例外声明**：supersede 时旧 run 的 transient overlay 在 durable 未确认前即移除，这是单 run 槽约束下的显式例外。前提：（1）steps 已保存到 `recentStepsByTurn`，（2）refresh 是 best-effort（失败时 durable 数据仍在服务端，用户切换回 session 时 `refreshSession` 会补齐）。这是可接受的，因为 `recentStepsByTurn` 确保了旧 turn 的 UI 展示不丢失。

#### C.8 改造 `runningSessionIDs`（§2.8）

`useRunRegistry.ts` 的 `runningSessionIDs` 仅统计 `status === 'running'`：
```typescript
const runningSessionIDs = useMemo(() =>
  new Set(Object.entries(activeRunsBySession)
    .filter(([, run]) => run.status === 'running')
    .map(([sessionID]) => sessionID))
, [activeRunsBySession])
```

**注意**：改为 `useMemo` 后，每次 `activeRunsBySession` 变化（含 delta flush）都会新 `Set`。与现状的独立 `useState<Set>` 相比失去引用稳定性。可接受（下游用 `.has()` 检查），但需同步调整 `useRunRegistry.test.ts` 中"membership 稳定"的断言：改为检查 `.has()` 结果而非引用相等。

#### C.9 测试

- `web/src/hooks/useRunRegistry.test.ts` 新增：
  - settled(committed) 后 run 进入 reconciling 而非移除。
  - `session.last_seq >= settled.last_seq` 后 run 移除（用 `last_seq` 而非 `newest_seq`）。
  - `last_seq` 差距（`session.LastSeq > history.newest_seq`）不影响移除判定。
  - settled(failed) 后 refresh → 移除。
  - settled(cancelled) 后 refresh → 移除。
  - `last_seq=0` 的 committed：refresh 成功后移除。
  - stream error 进入 error_pending_refresh（无 settledLastSeq），手动 refresh 成功后移除。
  - stream error 只清理绑定的 runID，不影响新 run。
  - reconciling 超时 → error_pending_refresh（不移除 run）。
  - 新 run 覆盖 reconciling run：先保存旧 run steps + refresh，再覆盖。
  - reconciling 期间 `sendMessage` 走 `startNewRun` 而非 `appendRunMessage`。
  - error_pending_refresh 手动 refresh 成功后移除。
  - backoff 重试：首次未追上 → 2s 后重试 → 追上后移除。
  - `recentStepsByTurn` 仅在 run 移除时写入，reconciling 期间不写。
  - `runningSessionIDs` 仅包含 `status === 'running'` 的 session。

---

### WP-D：显式身份边界

#### D.1 Conversation 接收显式 sessionID

**当前**：`Conversation` 从 `props.detail?.id` 推断 sessionID。

**改造后**：
```typescript
export const Conversation = memo(function Conversation(props: {
  sessionID: string          // ← 新增，显式传入
  detail: Session | null
  ...
}) {
  sessionIDRef.current = props.sessionID
```

`App.tsx` 调用处传入 `sessionID={selectedSessionID}`。

#### D.2 detail/history 与 sessionID 不一致时不渲染

```typescript
const safeDetail = props.detail && props.detail.id === props.sessionID ? props.detail : null
// 使用 safeDetail 而非 props.detail
```

#### D.3 收紧自定义 memo comparator

**当前**（`Conversation.tsx:375-386`）：有自定义 comparator，但**未包含 `sessionID`**，且未比较所有 callback。

**改造**：
- comparator 增加 `sessionID` 比较。
- 审查所有 callback 是否稳定（`useCallback`），缺失的加入比较或确保稳定。
- 或改为默认 `memo`（移除 comparator），依赖 callback 稳定性。

#### D.4 scroll memory 用显式 sessionID

`sessionIDRef.current = props.sessionID`（不再从 `props.detail?.id` 推断）。

#### D.5 command target 绑定显式 sessionID

**当前**：`resendMessage`/`retryRun`/`cancelRun`/`sendMessage` 等闭包 `selectedSessionID`。

**改造**：
- `sendMessage`/`cancelRun`：接受 `sessionID` 参数而非读全局 selection。
- `resendMessage`：从 `item` 所属 session 发起（参数传入 `sessionID`），不读全局 selection。
- `retryRun`：接受 `sessionID` 参数。
- 所有异步操作完成时的回调检查绑定的 `sessionID` 而非 `selectedSessionRef.current`。

#### D.6 reconciling/error UI 改造点（§2.8）

**文件**：`web/src/components/Conversation.tsx`

- `running` prop = `activeRun?.status === 'running'`（reconciling 时为 false）。
- `ActiveRunView` 的 "Generating" 指示和 cancel 按钮仅 `status === 'running'` 时显示。
- `error_pending_refresh` 时渲染 "refresh to see latest" 按钮（在 Conversation 内，调用 `onRetryRefresh` callback）。
- 新增 `onRetryRefresh: () => void` prop（绑定 `retryRefreshSession`）。

#### D.7 测试

- `web/src/components/Conversation.test.tsx`（新增）：
  - sessionID 与 detail.id 不匹配时不渲染旧数据。
  - sessionID 切换时 scroll memory 按正确 ID 保存/恢复。
  - memo comparator 包含 sessionID 时身份切换触发 re-render。
  - reconciling 时不显示 Stop 按钮 / Generating。
  - error_pending_refresh 时显示 refresh 按钮。
- `web/src/App.tsx` 相关测试：command target 绑定 sessionID 后异步完成不误打到其他 session。

---

### WP-E：验证

#### E.1 自动化检查

```bash
cd web && npm run check        # TypeScript
cd web && npm run test         # Vitest（含新增竞态测试）
cd web && npm run build        # Vite build
go test ./internal/...         # Go test（含新增 snapshot 测试）
```

#### E.2 竞态测试（确定性构造方式）

| 测试 | 构造方式 | 验证 |
|---|---|---|
| session 快速切换 | mock `api.snapshot` 返回 deferred promise，rerender 切换 sessionID，resolve 旧 promise | 旧 snapshot 不覆盖新 session 的 state |
| project list 乱序 | mock `api.sessions` 返回两个 deferred，先 resolve 新 generation 再 resolve 旧 | 旧 list 不覆盖新 list |
| snapshot 乱序 | mock `api.snapshot` 返回旧 revision 先 resolve、新 revision 后 resolve | 最终 state 为新 revision |
| revision JS 安全比较 | snapshot revision 为 string `"142"`，local 为 `"141"`；另测 `"9"` vs `"10"` | `BigInt` 比较正确，非字典序 |
| snapshot merge | local 有 seq 1-5，snapshot 有 seq 3-8（重叠） | 保留 seq 1-2 + snapshot 3-8 |
| snapshot replace | local 有 seq 1-5，snapshot 有 seq 10-15（不重叠） | 整体替换为 10-15 |
| committed settlement 水位 | mock `session.last_seq=100`（含 records），`history.newest_seq=97`（仅 items），`settled.last_seq=100` | `100 >= 100` → run 移除 |
| settled refresh 失败保留 overlay | mock `refreshSession` reject，验证 run 仍为 `error_pending_refresh` 而非移除 | overlay 保留 |
| 旧 stream 失败不删新 run | 捕获 `boundRunID`，启动新 run 后 reject 旧 stream，验证新 run 仍在 | 新 run 不受影响 |
| snapshot identity mismatch | dispatch snapshot 带 `session_id: 'a'` 但 `expectedSessionID: 'b'` | state 不变 |
| failed settlement | mock `run.settled(failed)`，验证 refresh 后 run 移除、error 保留 | 正确移除 |
| cancelled settlement | mock `run.settled(cancelled)`，验证 refresh 后 run 移除 | 正确移除 |
| last_seq=0 committed | mock `run.settled(committed, last_seq=0)`，refresh 成功 | run 移除 |
| stream error 无 settledLastSeq | mock stream reject，验证 `error_pending_refresh`；手动 refresh 成功 | run 移除 |
| 新 run 覆盖 reconciling | run 在 reconciling，启动新 run | 旧 run steps 保存到 recentStepsByTurn + refresh 触发 + 新 run 进入 |
| 覆盖 running run 被拒绝 | run 在 running，尝试 addActiveRun 新 run | 返回 false，新 run 不进入槽位 |
| reconciling 期间 sendMessage | run 在 reconciling，调用 sendMessage | 走 `startNewRun` 而非 `appendRunMessage` |
| error_pending_refresh 手动 refresh | run 在 error_pending_refresh，点击 refresh | 成功后移除 |
| 手动 refresh 未追上水位 | run 在 error_pending_refresh（有 settledLastSeq），refresh 成功但 `session.last_seq < settledLastSeq` | 回 `reconciling` + backoff |
| backoff 重试 | committed 后首次 refresh 未追上 | 2s 后重试，追上后移除 |
| steps 保存时机 | reconciling 期间检查 `recentStepsByTurn` | 未写入；移除后写入 |
| fallback revision | snapshot 404，回退 `api.session`+`api.items`，`revision=String(session.last_seq)` | 不被 store 丢弃 |
| runningSessionIDs | run 在 reconciling | session 不在 `runningSessionIDs` 中 |
| reconciling 超时 | run 在 reconciling 60s | 进入 `error_pending_refresh`（不移除） |

#### E.3 E2E（Playwright）

现有 `web/e2e/` 4 个 spec 全部保持通过。新增：
- `snapshot-flow.spec.ts`：验证 snapshot 加载 → 发消息 → settled → reconciling → durable 追上 → overlay 消失的完整链路。使用 API mock/harness 固定 `last_seq` 时序，不依赖 full stack 时序。

---

## 4. 受影响文件清单

### 后端（Go）

| 文件 | 改动 |
|---|---|
| `internal/execution/service.go` | 新增 `GetSessionSnapshot`、`SessionSnapshot` 类型（`Revision string`） |
| `internal/execution/session_events.go` | 提取 `buildItemsPage` 供 snapshot 复用 |
| `internal/webapp/server.go` | 新增 `GET /api/sessions/{id}/snapshot` 路由 + handler |
| `internal/webapp/server_behavior_test.go` | 新增 snapshot 测试 |
| `internal/execution/service_test.go` | 新增 snapshot 单元测试 |

### 前端（TypeScript/React）

| 文件 | 改动 |
|---|---|
| `web/src/types.ts` | `ActiveRun.status` 增加 reconciling/error_pending_refresh/settledLastSeq；新增 `SessionSnapshot` 类型 |
| `web/src/api.ts` | 新增 `api.snapshot(sessionID)` |
| `web/src/lib/sessionStore.ts` | **新增**：reducer + state + 归并逻辑 + LRU 淘汰 |
| `web/src/lib/sessionStore.test.ts` | **新增**：竞态测试 |
| `web/src/hooks/useSessionStore.ts` | **新增**：React hook 封装 reducer |
| `web/src/hooks/useSessionHistory.ts` | 改造为 store 消费者，移除独立权威 state；`refreshSession` 始终返回 fetched `{ session, history }`，失败 throw，仅 apply 成功时调 `onSnapshotApplied` |
| `web/src/hooks/useSessionHistory.test.ts` | 适配改造 |
| `web/src/hooks/useRunRegistry.ts` | `addActiveRun` 覆盖前保存旧 run；`runningSessionIDs` 仅统计 `running` |
| `web/src/hooks/useRunRegistry.test.ts` | 新增 reconciling/error/覆盖/重试测试 |
| `web/src/App.tsx` | list generation 保护；handleRunEvent reconciling 改造；stream error boundRunID；sendMessage 仅 running append；addActiveRun onSupersedeRun；onSnapshotApplied；retryRefreshSession；reconciling 超时定时器；Conversation 传 sessionID；command target 绑定 sessionID |
| `web/src/components/Conversation.tsx` | 接收显式 sessionID；detail/sessionID 不一致时不渲染；收紧 memo comparator；`running` 按 status 判断；reconciling/error_pending_refresh UI；onRetryRefresh prop |
| `web/src/components/Conversation.test.tsx` | **新增**：身份边界 + reconciling UI 测试 |

---

## 5. 上线策略

### 5.1 共存 + 渐进切换

- **WP-A**：后端新增 `GET /api/sessions/{id}/snapshot`，保留旧接口。前端未使用，无影响。直接合并。不需要改 `SessionV2` 或 store 结构。
- **WP-B**：前端实现 store + snapshot 消费。为降低风险，先实现 store + snapshot **保留 dual-fetch fallback**（若 snapshot 接口 404 则回退到旧 `api.session` + `api.items`，合成 `revision=String(session.last_seq)`），验证稳定后移除 fallback。
- **WP-C/D**：run 归并 + 身份边界改动，可在 WP-B 稳定后独立切换。

### 5.2 可提前的 quick wins

以下改动不依赖 snapshot 接口，可与 WP-A 并行：
- stream error `boundRunID` 修复（WP-C.3）
- Conversation 显式 `sessionID`（WP-D.1/D.4）
- list generation 保护（WP-B.3，不依赖 store reducer，先在 App.tsx 内实现）
- `sendMessage` 仅 `running` 时 append（WP-C.6）

### 5.3 回滚点

| 阶段 | 回滚方式 |
|---|---|
| WP-A 完成后 | 后端新增接口，前端未使用，无影响。直接合并。 |
| WP-B 完成后 | 前端切换到 store。若出问题，revert 该 PR，前端回退到旧 `useSessionHistory`（旧接口仍可用）。 |
| WP-C 完成后 | run 归并逻辑改动。若出问题，revert 该 PR，run settled 恢复为立即移除（已有行为）。 |
| WP-D 完成后 | 身份边界改动。若出问题，revert，恢复 `props.detail?.id` 推断。 |

### 5.4 验收标准

- `go test ./internal/...` 全绿。
- `cd web && npm run check && npm run test && npm run build` 全绿。
- `cd web && npx playwright test` 全绿（含新增 spec）。
- 手动验证：快速切换 session 无闪烁；settled 后无空白；reconciling 期间 UI 正确。

---

## 6. 实施顺序

```
WP-A（后端 snapshot）
  ├─ Quick wins（并行，不依赖 A）：
  │    ├─ stream error boundRunID 修复（C.3）
  │    ├─ Conversation 显式 sessionID（D.1/D.4）
  │    ├─ list generation 保护（B.3，App.tsx 内实现）
  │    └─ sendMessage 仅 running 时 append（C.6）
  │
  └─ WP-B（前端 store，依赖 A 的接口）
       └─ WP-C（运行态归并，依赖 B 的 store 状态）
            └─ WP-D（身份边界，依赖 B/C 的 sessionID 可用性）
                 └─ WP-E（验证，覆盖全部）
```

每个 WP 完成后独立测试，确保不破坏现有行为。

## 7. Stage 6 已完成 — committed revision settlement

本阶段 supersede 上述旧的 settled 草图，实际规则如下：

1. `run.settled.committed_revision` 是首选水位；没有该字段时才读取
   `last_seq`。前端只接受非负十进制值，使用 `BigInt`/等价的
   precision-safe 比较，绝不先转 `Number`。缺失或非法值走保守 snapshot
   resync。
2. run SSE 的 replay 顺序保证 settled 之前的 item projection event 已经
   dispatch 到 store。`useSessionStore` 提供基于 `stateRef` 的
   `getSessionRevision`/`isRevisionCovered`，但只把已建立的 snapshot
   history entry（及其后应用的 events）视为完整 projection；未 snapshot
   的 pending queue 不能证明 coverage。handler 因而不读取过期的 React
   render state。local revision 覆盖 settled 水位时直接移除 transient run；
   只有缺失或落后才 snapshot，并在覆盖前保留 run。
3. retry/60 秒 timeout 只服务于确实落后的 reconciliation。failed/cancelled
   同样保留已提交 partial item；covered 时不 refresh。lifecycle 与 per-run
   settled 重复投递按 run id 幂等处理，sidebar 状态由 lifecycle 或本地
   settlement metadata 更新，background covered completion 不刷新当前选中
   session。
4. 删除了 `recentStepsByTurn`、`saveRecentStepsAndRemove` 及
   `processKey`。它们曾在 settlement 后复制 transient steps 以弥补“snapshot
   才有 durable item”的旧假设；Stage 3/5 的后端 item projection 已取代该
   bridge。必要的 transient reasoning 和尚未 durable 的 tool progress 仍在
   `ActiveRun`，已 durable 的 tool call/result 只显示一次；terminal
   assistant reasoning 则由后端在 `show_reasoning` 开启时投影到 durable
   item DTO，历史行直接从该字段重建。`item.created` alias 与 dual-fetch
   snapshot fallback 仍保留，作为迁移兼容项。

对应的最小回归覆盖在 `web/src/App.test.tsx`、`sessionStore.test.ts`、
`useSessionStore.test.ts` 与 `conversationRows.test.ts`；本阶段不包含
Stage 7 的 fault-injection/E2E 大规模收尾。
