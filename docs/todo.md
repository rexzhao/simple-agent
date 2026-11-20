# 待办任务

> 按时间倒序。每条写清**现状**、**根因**、**期望行为**与**改哪里**，
> 让接手的人不必重新调研一遍。

---

## 1. 桌面版输入框草稿没有绑定会话

**状态**：✅ 已完成（待真机确认）
**提出**：2026-11-20

### 做法（已完成）

- `Session.draft` 存该会话未发送的输入；`App.switchTo(index)` 是**唯一**
  切换入口（离开时存到旧会话、进入时取回）。
- 5 个入口全部收敛：侧栏点击、新建会话、删除后定位、`SelectSessionForTest`。
  `syncSessions` 的重新定位**不保存**（那不是用户切换），只在输入框为空时恢复。
- 发送成功后同时清 `a.draft` 与 `session.draft`，避免"已发出的内容复活"。
- 删除当前会话时清掉草稿。
- 附件**本来就是按会话存的**（`Session.attachments`），无需改动 —— 之前
  文档里说"附件也会串"是我的误判（把 `Session` 的字段当成 `App` 的了）。

### 测试

`internal/uiapp/draft_per_session_test.go`：5 条，覆盖切走切回、新建会话、
发送后清空、侧栏点击路径。已验证"把 switchTo 里的存取去掉"时测试立刻失败
（`切到 B 后不该看到 A 的草稿，实际 "这是 A 的草稿"`），确认断言不是空跑。

### 原始调研记录（留档）

**根因**：

`App.draft` 原本是**一个全局字符串**，切换会话时没人动它：

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
}
```

另外 `Session.restoreDraft` 是**另一件事**：发送失败且用户已切走时留下的草稿，
只在输入框为空时放回。语义与"正常切换会话带回内容"不同，没有合并。

**为什么会有这个 bug**：切换入口散落在 5 处，每处各做各的事，谁都没管草稿。
现在收敛成一个 `switchTo`，以后加"切换时的副作用"只需改一处。

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