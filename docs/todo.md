# 待办任务

> 按时间倒序。每条写清**现状**、**根因**、**期望行为**与**改哪里**，
> 让接手的人不必重新调研一遍。

---

## 1. 桌面版输入框草稿没有绑定会话

**状态**：待做
**提出**：2026-11-20

### 现状

在会话 A 的输入框里打了字（未发送），切到会话 B，**那半句话跟着过去了**；
切回 A 时还是同一段内容。用户期望每个会话各存各的草稿。

### 根因

`App.draft` 是**一个全局字符串**，切换会话时没人动它：

```go
// internal/uiapp/layout.go，会话行的点击
if title.Clicked() {
    a.active = index      // ← 只换了 active，draft 原封不动
}
```

已经有一个按会话存的实现，但**只在远程模式生效**：

```go
// internal/uiapp/executors.go
func (a *App) saveDraftForHost() {
    if a.executors == nil {     // ← 本机直连时直接返回，什么也不做
        return
    }
    ...
    a.draftsByHost[a.executors.Current()+"\x00"+session.ID] = text
}
```

也就是说：`draftsByHost` 的 key 设计**已经包含 session.ID**，
只是本地模式没有接入这套存取。

### 期望行为

- 每个会话各自保存未发送的草稿（含附件？见下）
- 切换会话：离开前存、进入后取
- 新建会话：草稿为空
- 删除会话：连同草稿一起清掉（否则 map 会一直涨）
- 发送成功后清空该会话的草稿（`pending` 里已有乐观清空逻辑，确认它同步清了草稿）

### 改哪里

1. `internal/uiapp/app.go`：`draftsByHost` 的语义从"执行端 + 会话"扩展为
   "执行端 + 会话"，本地模式用固定 host key（如 `""` 或 `"local"`）。
   或者新开一个 `draftsBySession map[string]string`，避免把两件事混在一个 map 里
   —— **推荐后者**，语义更清楚。
2. `internal/uiapp/layout.go` 的会话行点击：切换前 `saveDraft(old)`，
   切换后 `restoreDraft(new)`。
3. 其它切换入口也要覆盖：侧栏点击、新建会话、删除会话、执行端切换、
   `SetActiveForTest` 之类。
   —— 建议收敛成**一个** `switchTo(index)` 函数，所有入口都走它，
   避免漏改（这次就是因为入口散落才漏的）。
4. 删除会话时清草稿：`deleteSession` 路径里顺手删。

### 附带问题

**附件也会串（已确认）**：`App.attachments` 同样是单个切片：

```go
// internal/uiapp/app.go
// attachments 是待发送的图片（粘贴/拖入）。
attachments []*attachment
```

所以切会话时已粘贴的图片也会跟着过去。修草稿时应**一起处理**——
它们的生命周期完全相同（都属于"未提交的输入"）。

建议：把 `draft string` 与 `attachments []*attachment` 合成一个
`composerDraft` 结构，按会话存，一次改到位。

**落盘**：草稿目前只在内存里，重启就没了。要不要持久化到 `ui-state.json`
是另一个决定（dsh 也没有），先不做，但记在这里。

### 测试建议

假 Runtime 起两个会话 → 在 A 里输入 → 点 B → 断言输入框为空 →
点回 A → 断言内容还在。这条能覆盖"两个入口都要接上"的核心路径。

---

## 2. 启动/打开会话的日志读取范围（已确认，记录备查）

**结论**：打开会话**只读 `session.jsonl`，不碰 `archive-*.jsonl`**。

真机数据（`%APPDATA%\simple-agent\sessions`）：

| 会话 | session.jsonl | 归档 |
| --- | --- | --- |
| 2514babb | 1.6 MB | 无 |
| 543a320f | **1.3 MB** | 2 份，共 **7.4 MB** |

即：打开 `543a320f` 只读 1.3 MB，7.4 MB 的归档完全不碰。

### 为什么不需要回翻

三样"跨压缩状态"都从最新那份里的 `compact.snapshot` 接续：

| 状态 | 快照字段 |
| --- | --- |
| 轮数 / 步数 | `turns` / `steps` |
| 累计用量（算缓存命中率） | `total_usage`（2026-11-20 加） |
| 任务清单 | `todos` |

归档只在用户**往上翻历史**时按需加载（`LoadOlder`）。

### 仍然会读日志的三处（都很轻）

1. **侧栏列表**（`Catalog.List`）：每个会话只读第一行（标题）与最后一行
   （更新时间），不重放全文。
2. **打开会话**（`openSession` → `logStore.Load`）：读 `session.jsonl` 全文
   并重放。这是**必须**的 —— 要重建消息、工具结果、状态。
3. **清理空会话**（`PruneEmpty`）：同样只读 `session.jsonl`。

### 守这条的测试

`internal/store/open_cost_test.go`：

- `TestLoadDoesNotTouchArchives`：把归档写坏，`Load` 仍正常返回且无警告
  （证明真没读它），而 `LoadOlder` 才去碰它。
- `TestSnapshotCarriesCountersSoOpenIsCheap`：快照必须带上 turns/steps/total_usage。

---

## 3. 历史遗留：relay 无自动启动

**状态**：未定

执行端经 relay 的部署仍没有 systemd 之类的东西负责拉起。
是否实现、用哪种方式，待定。