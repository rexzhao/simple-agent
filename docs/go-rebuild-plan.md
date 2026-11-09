# simple-agent · Go + mygo 重建方案

状态：**已实现**（M1–M7 全部完成；与原设计的差异见 §13）
前身：Rust + GPUI 版本（`simple-agent` 仓库，HEAD `c5ca872`，已停止开发）
远程执行端/展示端（relay）：`docs/remote-design.md`（设计待评审）
本方案提交审核通过后再开始写代码。

---

## 0. 为什么换、换掉什么

| | Rust 版 | Go + mygo 版 |
|---|---|---|
| 构建 | 依赖重（gpui 全量编译 5-10 分钟）、增量慢 | 纯 Go 无 cgo，单二进制，秒级增量 |
| UI | gpui 0.2.2（Entity/Task/订阅、手写 Element） | `ui` 包：状态即视图的纯函数，`ui.NewTester` 可无窗口测试 |
| 并发 | gpui 执行器 + Task drop 取消 | goroutine + `context.Context`（更直白） |
| 产出 | 多 crate workspace | 单 module + `internal/` 分包 |

**Go 版本（G1）**：`go 1.27.1`（mygo v0.2.18 的 go.mod 要求，且它直接 import `encoding/json/v2`）。`go.mod` 写 `go 1.27.1`，CI 与其他机器用 `GOTOOLCHAIN=auto` 或显式 toolchain 指令复现。

**JSON 方案（G1）**：用 `encoding/json/v2` 而不是 v1 + 手写 `UnmarshalJSON`：
- 严格解码：`json.Unmarshal(data, &v, json.RejectUnknownMembers(true))`
- 区分 `null` 与字段缺失：用 `jsontext.Value` / 指针字段 + 自定义 `UnmarshalJSONFrom`，只在 `Part` 这类需要 oneof 语义的地方手写
- 序列化保持 camelCase（struct tag），`raw` 用 base64（自定义 marshaler）
- **注意**：`encoding/json/v2` 在 1.27.1 里仍带 `//go:build goexperiment.jsonv2` 标记，但已是默认开启的基线（mygo 自身也依赖它）。构建时**不得设置 `GOEXPERIMENT=nojsonv2`**；API 仍属实验性，升级 Go 时需复核 `RejectUnknownMembers` 与 `UnmarshalJSONFrom`。

**不迁移的**：Rust 代码本身全部丢弃。**必须迁移的**：数据模型、协议适配、工具语义、编排状态机、持久化设计，以及 Rust 阶段 review 出来的**所有缺陷结论**（见 §9）。

---

## 1. mygo 能力对照（已核对 v0.2.18 源码与文档）

| 需求 | mygo 提供 | 说明 |
|---|---|---|
| 聊天列表（变高、贴底、虚拟化） | `ui.List` + `ui.ListState{Key, FollowEnd}` | `FollowEnd` 自动贴底、滚走即停止跟随；`Key` 让插入旧消息时行状态跟随 item |
| 按需加载历史 | `ListState.Visible()/ScrollTo/AtEnd()` | 滚到顶时触发加载，插入后 `Key` 保证视图不跳 |
| 输入框（中文 IME、多行自适应） | `ui.TextInput` / `ui.TextArea`（`Lines(min,max)`、`TextRanges`） | 单行用于重命名，多行用于 composer；IME 是内置能力 |
| markdown 内联样式 | `ui.RichText` + `ui.Span{Weight, Italic, Color, Underline}` | 粗体/斜体/行内代码/链接可直接映射 |
| 代码块 | `ui.Text` + `Font{Family, Size}` | 等宽字体 + 横向滚动容器；语法高亮自己算 span 颜色 |
| 图片 | `ui.Image` + `ui.DecodeBitmap` | 位图一次性解码，GPU 常驻；无需自建缓存 |
| 单实例锁 | `mygo.App.RequestSingleInstanceLock()` | 正好替代原设计里的"锁文件 + fs2" |
| 从 goroutine 改状态 | `win.Update(func(){…})` 投递到主线程并请求重绘 | 流式 token 回调走这里 |
| UI 测试 | `ui.NewTester`（渲染帧、按文本查找、点击/输入/滚动/按键） | 比 Rust 版只能测纯逻辑强很多 |

结论：**native UI（`ui` 包）够用**，不需要 webview 混排。

---

## 2. 目录结构

```
simple-agent-go/
  go.mod                          module github.com/<owner>/simple-agent
  cmd/simple-agent/main.go        入口：解析参数 → 单实例锁 → mygo.App.Run
  internal/
    message/                      A2A wire 模型（纯逻辑）
      message.go                  Message / Part / Role / Metadata
      part.go                     PartContent(Text|Raw|URL|Data) + 严格 oneof 校验
      wire.go                     自定义 JSON 编解码（camelCase、base64 raw、null 语义）
      message_test.go
    conversation/                 会话状态（纯逻辑）
      conversation.go             Conversation / Entry / ChatEntry / EntryStatus
      status.go                   Pending|Streaming|Complete|Cancelled|Failed
      conversation_test.go
    render/                       渲染分发（纯逻辑，不依赖 ui）
      view.go                     PartView(Text|Code|Image|File|ToolUse|ToolResult|Json|Summary)
      classify.go                 按 mediaType 分类（规范化 MIME、魔数嗅探）
      view_test.go
    providers/                    模型提供商
      provider.go                 Provider 接口 / ChatRequest / Event / Selection
      registry.go                 内置提供商 + 配置覆盖 + 选择
      openai.go | anthropic.go    两套协议（请求体 + 有状态流解析）
      sse.go                      SSE 读取（CRLF/多行 data/无尾空行）
      transport.go                net/http 流式 + context 取消
      mapping.go                  内部 Message ↔ 厂商消息（含 tool 配对）
      simulated.go                无凭据时的本地提示流
      provider_test.go            用 httptest.Server 模拟网关
    tools/                        工具链（对齐 deepseek-harness）
      registry.go                 ToolDef / ToolReturn(Output|AskUser) / ToolContext
      sandbox.go                  路径解析（词法 + lstat/canonicalize 防悬空链接）
      fs.go write.go edit.go glob.go grep.go shell.go todo.go ask.go cwd.go
      tools_test.go
    driver/                       会话编排状态机（不依赖 ui）
      driver.go                   SessionDriver / Busy / Turn / Effect
      replay.go                   Replay(events) → 重放状态（纯函数）
      compact.go                  上下文压缩编排（边界选择、摘要请求、失败回滚）
      driver_test.go replay_test.go compact_test.go
    store/                        事件日志与持久化
      event.go                    事件类型与信封（v/seq/at/type/data）
      store.go                    SessionStore 接口
      jsonl.go                    JSONL 实现（原子滚动、半行修复、分页）
      memory.go                   内存实现
      store_test.go
    markdown/                     markdown 子集 → 样式段落（纯逻辑）
      parse.go                    用 goldmark 解析成 AST（不手写解析器）
      render.go                   AST → 块/行内 span（含 LRU 缓存，见 G3）
      highlight.go                chroma 代码高亮（M7 接入，带缓存）
      markdown_test.go
    config/                       配置与启动参数
      config.go                   TOML/JSON 加载、provider 覆盖、allow_shell
      options.go                  CLI flag（--allow-shell/--no-shell/--help）
    uiapp/                        mygo 视图层（只有这里 import ui）
      app.go                      根视图与状态（sessions/active/输入/忙闲）
      sidebar.go                  会话列表（重命名、删除、确认态）
      chat.go                     ui.List + 行渲染（PartView → RichText/Image/卡片）
      composer.go                 TextArea + 发送/停止按钮
      ask.go                      提问面板
      theme.go                    颜色/字号/等宽字体
      app_test.go                 ui.NewTester 驱动
  docs/                          设计文档（含本文件、持久化设计）
  testdata/                      SSE 样本、JSONL 样本
```

依赖方向（单向，避免环）：

```
cmd → uiapp → driver → {conversation, providers, tools, store, render, markdown, message}
                    store → {conversation, message}
                    providers → message
                    tools → （独立，仅标准库）
```

`message` / `conversation` / `render` / `markdown` / `providers` / `tools` / `store` / `driver` 全部**不 import `ui`**，因此都能普通 `go test` 跑；只有 `uiapp` 需要 mygo。

---

## 2.1 界面方案对比与建议（用户要求：比较后给建议）

mygo 支持两种窗口，可以混用。我们只需要聊天界面。下方对比中的内存/体积数字**来自 mygo 文档 `docs/architecture.md`，且只有 macOS / Linux 数据，Windows 未测（我们的主平台）**。

### 1. 聊天列表虚拟化与贴底

| | 原生 | WebView |
|---|---|---|
| 变高虚拟化 | `ui.List` + `ListState`：按内容测高、只构建可见行，百万行级别 | 需自建或引 virtua/tanstack-virtual，能力更强但要自己接测高 |
| 贴底跟随 | `FollowEnd` 内置：新行自动贴底，用户滚走即停、滚回即跟随 | 需手写 scroll anchoring（`scrollTop`/`ResizeObserver`），经典 bug 源 |
| 插入历史不跳位 | `ListState.Key` 按消息 ID 保持位置 | 需手动维护 scroll offset 补偿 |

### 2. 输入法、多行输入、选择复制

| | 原生 | WebView |
|---|---|---|
| IME | 内置（`ui.TextInput`/`TextArea` 走系统输入法） | 浏览器原生，最成熟 |
| 多行自适应 | `TextArea().Lines(1,8)` | `textarea`/`contenteditable` 自动增高 |
| **Enter 语义** | **不能直接用**：`editor_navigation.go:269` 中 multiline 收到 `KeyEnter` 直接插 `\n`，不设 `submitted`；需验证外层 `Shortcut` 能否截获 | 完全可控：`keydown` 里拦截 Enter、Alt+Enter 自行插 `\n` |
| **文本选择与复制** | **开箱即用**：容器调 `Element.Selectable()`，内部所有 Text/RichText 共享一个选区，复制时按构建顺序用换行拼接（可跨段落复制） | 浏览器原生 |

### 3. markdown / 代码高亮 / 图片

| | 原生 | WebView |
|---|---|---|
| markdown | goldmark 解析 → `ui.RichText`+`ui.Span`；**表格用 `ui.Grid`（grid.go:49）**、**嵌套列表**用递归 Column + 缩进、**链接**用 `ui.Link` 作行内元素 | react-markdown/marked + CSS，生态最全 |
| 代码高亮 | chroma 产出 span 颜色，逐行拼 `ui.Text`（等宽 + 横向滚动） | highlight.js/shiki，能力最强（行号、diff 视图） |
| 图片 | `ui.Image` + `DecodeBitmap`，需自己管异步下载与解码 | `<img>` + 浏览器缓存，最省事 |

### 4. 性能

| | 原生 | WebView |
|---|---|---|
| 内存（idle） | macOS 小窗口约 44MB；Linux 半屏 4K（缩放 2 倍）约 65–67MB（其中 15MB 为窗口缓冲） | macOS WKWebView 及其子进程约 62MB |
| 启动 | 即出图，无 webview 初始化 | 需等 webview 进程与页面加载，首帧明显更慢 |
| 长会话 | 只渲染可见行；markdown 缓存（G3）后每帧成本可控 | DOM 节点增长，需虚拟化 + memo，长文档有 GC 压力 |

> 注：Windows 的内存数据 mygo 文档没有给出，而 WebView2（`msedgewebview2` 一组进程）的开销通常比 WKWebView 更大。**M4 需实测 Windows 空闲内存**（原生 vs WebView2），结果回填此处。

### 5. 测试手段

| | 原生 | WebView |
|---|---|---|
| 单测 | `ui.NewTester` 无窗口渲染、按文本点击/输入/滚动/按键，`go test` 直接跑，可 `-race` | 需 Playwright 类工具，另跑 node 进程，慢且脆 |
| 覆盖我们的流程 | 发送/停止/作答/删除确认/重命名都能用文本定位 | 同左但维护成本高 |

### 6. 包体积与依赖链

| | 原生 | WebView |
|---|---|---|
| 二进制 | 约 7MB（纯 Go，无 cgo） | 约 7MB + 前端产物 |
| 构建链 | 仅 Go（`go test`/`go build`） | Go + Bun + Vite + TypeScript |
| 运行时依赖 | **无** | macOS WKWebView / **Windows 需 WebView2 Runtime**（企业镜像/精简系统常缺失） / **Linux 需 WebKitGTK 4.1**（最小发行版常没有） |
| 语言栈 | 单一 Go | Go + TS 双栈，IPC 客户端由代码生成维护 |

### 7. 无障碍

原生 UI 支持屏幕阅读器（mygo README 声明）；浏览器生态更完善。两边都可接受，不构成选型分歧。

### 8. 推荐结论：**v1 用原生 UI**

理由按权重：

1. **分发**：无 webview 运行时依赖（Windows WebView2 / Linux WebKitGTK 是真实分发风险），单二进制。
2. **可测试性**：`ui.NewTester` 让关键 UI 流程进 `go test`，直接支撑 G6。
3. **单语言 + 单构建链**：CI 只跑 Go，无 bun/TS 与代码生成。
4. **性能**：启动与内存更优，长会话更稳。
5. **能力足够**：列表/IME/内联样式/图片/等宽代码/**表格/链接/文本选择复制**都有现成机制；markdown 与高亮用 goldmark + chroma 自接（约 300–500 行）。

**因而不需要“原生外壳 + webview 消息区”的混排**——混排会把 WebView2 运行时依赖与 TS 前端重新带回来，反而丢掉原生的前两条优势。只有将来真要做 mermaid、diff 视图这类富内容时，才值得重新评估（届时只需替换 `uiapp`）。

**已知代价与必须在 M4 开头验证的项（不回避）**：

- **Enter 语义（G2）**：multiline 编辑器收到 Enter 直接插 `\n`，不设 submitted。M4 首项验证外层 `Shortcut` 能否截获；截获不到则**由用户决定键位方案**（候选：Ctrl+Enter 发送；或给 mygo 上游提 PR 增加“Enter 提交”选项；**不 fork**，1.0 前维护成本高）。**这一项不影响架构选型**。
- **CJK / emoji / 字体回退 / 高分屏**：原生在 Windows 走 DirectWrite（`internal/text/dwrite_windows.go`），理论上回退正常，但**需在 M4 与 Enter 一起实测**：中英混排、emoji、输入法候选窗位置、高 DPI 显示。
- markdown 表格/嵌套列表的排版不如 CSS 精致（可用 `ui.Grid` + 递归 Column 实现，效果可接受）。

**切换成本已隔离**：只有 `uiapp` import `ui`。将来若需富交互（mermaid/表格编辑/diff/富文本复制），只换 `uiapp` 或改混排，`driver`/`providers`/`tools`/`store` 全部不动。

---

## 3. 模块迁移映射

| Rust 版 | Go 版 | 迁移要点 |
|---|---|---|
| `core/message.rs` | `message/` | `PartContent` 用 interface + 严格校验函数（Go 无 enum）；`Part` 反序列化走自定义 `UnmarshalJSON` 实现"恰好一个字段"校验；`raw` 用 base64 std 编码（解码同时接受 URL-safe、无 padding）；`ROLE_USER/ROLE_AGENT` 枚举值 + 小写别名 |
| `core/conversation.rs` | `conversation/` | `EntryStatus` 加 `Cancelled`；ID 唯一性约束保留（重复返回错误）；`contextID` 只读（结构体字段私有 + 方法）；`streaming_part` 下标逻辑保留（增量只并入目标 part） |
| `core/render.rs` | `render/` | `PartView` 用带 tag 的 struct + `Kind()`，避免 Go 无 sum type 的痛点；分类规则（MIME 规范化、魔数嗅探、tool vnd 类型）照搬 |
| `providers/*` | `providers/` | 中立 `ChatRequest`/`Event`；两套服务端的有状态解析器（OpenAI 的 finish_reason 与 usage 分块、Anthropic 的 content_block_stop 只对 tool_use）；`stream_options` 可配；`http.Client` + `context` 取消 |
| `tools/*` | `tools/` | 9 个工具与参数名保持一致（read/write/edit/glob/grep/bash/pwsh/todo_write/working_directory/ask_user_question）；接口为 `Run(ctx, args, toolCtx)`，**ctx 取消即中止**（G5）；沙箱 = 词法校验 + `os.Lstat` + `EvalSymlinks`；shell 超时/取消要杀**整棵进程树**（Windows `cmd.Cancel` 调 `taskkill /T /F`，POSIX `Setpgid` + `kill(-pgid)`），并设 `cmd.WaitDelay` 或沿用临时文件重定向防 `Wait` 卡死 |
| `app/driver.rs` | `driver/` | `Busy` 状态机（Idle/Streaming/RunningTools/AwaitingAnswer/Compacting）+ 事件转移 + 非法转移 panic（debug）或回 Idle（release）；`Effect{StartStream, RunTools, Changed}`；`Turn{Selection, Depth, MessageID}` |
| `app/agent.rs`（事件 → 会话） | `driver/stream.go` | `TextDelta` 合并、`ReasoningDelta` 独立 part、工具参数**流式缓冲**到 `ToolCallEnd` 才解析 |
| `app/ui.rs` | `uiapp/` | 三栏布局；`ui.List` 用 `Key=entry.ID`、`FollowEnd=true`；流式只 `Invalidate()`（列表自动跟随） |
| `app/input.rs` | mygo `ui.TextArea`/`TextInput` | 不再自写 IME；composer 用 `TextArea().Lines(1,8)`，重命名用 `TextInput().Submitted()` |
| `app/markdown.rs` | `markdown/` | 用 **goldmark** 解析（CommonMark、能拿 AST），自己只写 AST → `[]ui.Span` 的映射；**解析/高亮结果按 `(messageID, part 序号, 内容哈希)` 缓存**（G3），流式中的那条只重算自己 |
| `app/options.rs` + 配置 | `config/` | `--allow-shell/--no-shell/--help`；`simple-agent.toml`（`active_provider`/`active_model`/`system_prompt`/`allow_shell`/`[providers.*]`） |
| 持久化设计（文档） | `store/` + `driver/replay.go` | 见 §6，已并入 opus 的全部修正 |

---

## 4. 并发模型（与 Rust 版的关键差异）

**硬约束（G4）**：

1. **会话状态与 driver 只在主线程访问**。goroutine 不得直接读写，只能通过 `win.Update(func(){…})` 投递；每个里程碑跑 `go test -race`。
2. **闭包捕获 `SessionID`，不捕获 `*Session`**：会话可能已被删除，回调里查不到就丢弃。
3. **停止的顺序**：在**同一个主线程回调**里先 `driver.CancelCurrent()`，再 `cancel()`。这样 goroutine 因 ctx 取消而投递的「流已关闭/出错」事件会被 `ownsStream` 校验挡掉，不会被当成失败处理。
4. 每 token 一次 `win.Update` 可以接受（closure 轻、`contentChanged` 只是 RequestFrame 并会合并到下一帧）；若实测卡顿，再在 goroutine 里攒 16–30ms 增量或加 atomic 保证单个在途。

```go
// 每轮一个 goroutine；取消 = cancel ctx
type runner struct {
    cancel context.CancelFunc
    done   chan struct{}
}

go func() {
    sid, mid := sessionID, messageID
    events := provider.Stream(ctx, req)   // ctx 取消 → HTTP 流关闭
defer close(done)
    for ev := range events {
        win.Update(func() {
            // 捕获的是 ID，不是结构体指针：会话删了就丢弃
            if s := app.Find(sid); s != nil && s.Driver.OwnsStream(mid) {
                app.Apply(s, mid, ev)
            }
        })
    }
}()
```

- **停止生成** = `driver.CancelCurrent()`（给未完成 tool_use 补结果）→ `cancel()`（中止 HTTP 流与工具 ctx）。
- **删除会话** = 从 map 移除 → `cancel()`；迟到事件因 `Find(sid) == nil` 丢弃。
- **工具执行也接 ctx（G5）**：见 §3 tools 行与 §9 第 12 条。
- **store 写入串行（回答第 2 问）**：所有写操作经同一条通道（主线程同步写，或单个写入 goroutine 从 channel 取）。压缩也走这条通道，因此「硬链接退化为复制」期间不可能有并发追加。

---

## 5. UI 结构（uiapp）

```
┌──────────┬───────────────────────────────┬─────────┐
│ sidebar  │ header: 标题 · provider/model │  (预留) │
│ 会话列表 │ ───────────────────────────── │         │
│ + 新建   │ ui.List（FollowEnd，Key=ID） │         │
│  （重命名/删除）│  行 = 用户气泡 / agent 卡片 / 工具卡片 /  │         │
│          │       提问面板 / 压缩分隔线     │         │
│          ├───────────────────────────────┤         │
│          │ composer: TextArea + 发送/停止 │         │
└──────────┴───────────────────────────────┴─────────┘
```

- 行渲染按 `render.PartView` 分发：`ui.RichText`（markdown/markdown 内的 code span）、`ui.Text`（等宽代码块，外层横向滚动）、`ui.Image`、工具卡片（`ui.Column` + 圆角 + 图标文字）。
- **立即模式下必须缓存渲染产物（G3）**：视图是状态的纯函数，**每帧对全部可见消息重跑一遍**。markdown 解析、代码高亮的输出按 `(messageID, partIndex, contentHash)` 缓存（LRU 上限，比如 256 条），正在流式的那条只重算它自己。否则流式时每帧把所有可见消息重新解析一遍。
- 状态全部挂在 `uiapp.App` 上（`sessions []*Session`、`active int`、`draft string`、`renaming *Rename`…），视图是 `func(c *ui.Context)` 的纯函数——与 Rust 版 `ChatApp` 的字段一一对应，但不再有 Entity/Subscription 生命周期问题（`ui.TextInput` 直接绑 `*string`）。
- **多行输入的 Enter 语义（G2，待验证）**：opus 核对 `ui/editor_navigation.go:269` —— multiline 编辑器收到 `KeyEnter` 会**直接插入 `\n`**，不设置 `submitted`；只有单行 `TextInput` 才有 `Submitted()`。所以“Enter 发送”不能原样实现。**M4 第一件事**用 `ui.NewTester` 验证外层 `Element.Shortcut(0, KeyEnter)` 能否抢在编辑器之前截获；截获不到的话需要**用户决定**备选方案（例如 Ctrl+Enter 发送），不自行更改（用户已明确要求 Alt+Enter 换行）。
- 输入框 Enter 发送：`ui.TextArea` 的提交语义需在实现时确认（单行 `TextInput.Submitted()`；多行用 `c.Modifiers()` 判 Alt+Enter 换行、Enter 发送，具体 API 见 §11 待确认项）。

---

## 6. 持久化与上下文压缩（沿用设计文档 + opus 全部修正）

沿用 `docs/persistence-design.md` 的骨架，并**并入 opus review 的全部修正**（详见 §7 的差异清单）。核心不变式：

1. 事件日志（信封 `{v,seq,at,type,data}`），消息本体嵌入 A2A wire JSON。
2. 两种后端 `JsonlStore` / `MemoryStore` 实现同一 `SessionStore`，共用 `driver.Replay`。
3. `compact` 时滚动：**不带后缀的 `session.jsonl` 永远是最新**，加载只读它。
4. 压缩 = **上下文压缩**（给模型的历史折叠成摘要 + 近期消息），与存储压缩是同一动作。
5. 加载时只重放最新文件；旧归档按需只读加载（**仅显示，不进模型历史**）。

### 6.1 目录

```
<data_dir>/sessions/<context_id>/
    session.jsonl            最新（不带后缀）
    archive-0001.jsonl       第一次压缩之前的完整历史
    archive-0002.jsonl
    meta.json                缓存：{title, remote_context_id, created_at, updated_at, message_count}
```

### 6.2 事件

| type | data |
|---|---|
| `session.created` | `{context_id, remote_context_id?, title?, created_at}` |
| `message.append` | `{message, status}` — 流式开始时写（内容为空，status=streaming） |
| `message.finalize` | `{message_id, message, status}` — 流正常结束/取消/失败时写最终内容与状态 |
| `tool.results` | `{message_id, results:[{id,name,output,is_error,pending?}]}` |
| `ask.raised` | `{message_id, call_id, questions}` |
| `ask.answered` | `{message_id, call_id, output}` |
| `selection.changed` | `{provider_id, model}` |
| `session.renamed` | `{title?}` |
| `compact.snapshot` | 仅新文件第一行，见下 |

快照 data：

```json
{"summary":"…","kept_entries":[{"message":{…},"status":"complete","created_at":"…"}],
 "selection":{"provider_id":"deepseek","model":"deepseek-chat"},"title":"…",
 "compacted_at":"…","kept_from_seq":118}
```

（`kept_entries` 保留本地元数据；压缩只在 Idle 发生，故不含 pending_asks。）

### 6.3 崩溃与原子性（并入 opus 修正）

- **半行修复**：打开 base 时定位最后一个 `\n`，`Truncate` 掉尾部残余再追加（否则新事件会接在半行后面，把损坏固化到文件中间）。
- **中间行损坏**：把原文件改名 `session.corrupt-<ts>.jsonl` 留档，用已解析的合法前缀重建 base（写一份快照）。
- **滚动顺序**：① `os.Link(base, archive-N)`（硬链接，O(1)，崩溃后继续追加的事件也出现在归档里）；不支持硬链接时退化为 `copy → tmp → rename`；目标已存在则**删除重建**。② 写 `session.jsonl.tmp`。③ `rename(tmp, base)`。POSIX 上再 `fsync` 目录。
- **Windows**：`compact` 前先关闭 base 的 `*os.File`，rename 后重新打开；rename 带有限次重试（杀毒/索引可能短暂占用）。
- **硬链接的隐蔽坑（回答第 2 问）**：base 与 archive 同一 inode，rename 后若仍拿旧句柄追加，**事件会写进归档而不是新 base**。所以“关句柄 → rename → 重开”是硬性要求，并配一条测试：compact 后追加一条事件，断言它出现在新 base 而非 archive。
- **退化路径的正确性**：所有 store 写操作串行（主线程同步写或单一写入 goroutine），压缩只在 Idle 且走同一通道 → 复制期间不可能有并发追写。`os.Link` 做成**可注入函数**，测试强制它失败以走复制路径。
- **单实例**：用 `mygo.App.RequestSingleInstanceLock()`（不是锁文件存在性判断）。

### 6.4 重放（`driver/replay.go`，纯函数）

```go
func Replay(events []store.Event) (SessionState, []store.Event)  // state, repairs
```

**分工**：坏行处理、`seq` 连续性检查都在 `store.Load` 阶段完成；交给 `Replay` 的**只有已解析的合法事件**。
- `store.Load` 负责：定位并截断半行、中间坏行截断/改名留档、`seq` 不连续检测，并返回 `[]Warning`；
- 加载流程（`driver.LoadSession`）把 warnings 转成一条 notice 插到会话开头（如"上次会话日志末尾有损坏，已修复（丢弃 1 行）"），同时把 corrupt 文件名告知用户；
- **这条 notice 只用于显示，不写进日志**：它不是会话状态；若写进去，以后每次重放都会再出现一次，也会和 repairs 的语义混在一起（它属于 UI 层的加载提示，不属于事件日志）。
- `Replay` 不再返回 error，只返回 `(state, repairs)`。

规则：

- `seq` 只在单文件内检查连续性。
- 有 `message.append{streaming}` 而无 `message.finalize` → **Cancelled**（显示"已中断"，不进模型历史）。
- `ask.raised` 无 `ask.answered` → 恢复为悬停：面板重现、`Busy=AwaitingAnswer`，作答后照常续一轮。事件里带 `depth`（否则恢复后从 0 计，轮数上限只是安全阀，从 0 开始也可接受）。
- **未配对的 tool_use（S3）**：重放时若某条 agent 消息里有 `tool_use` 但无正式结果（缺失或仍是 pending 占位），产出 repair 事件（补一条 `is_error=true` 的结果），**而不是直接修改 state**。
- **Replay 保持纯函数**：只返回 `(state, repairs)`，由**加载流程**在**接受任何新事件之前**把 repairs 追写进日志。这样后续新事件不会排在"看起来未配对"的 tool_use 后面，重放结果与在线状态一致；中间行损坏时的重建同样在加载时写。

### 6.5 上下文压缩（`driver/compact.go`）

- 触发：手动按钮；自动在**回合结束回到 Idle 时**检查用量 ≥ 80% 窗口（常量，后续可配置）。
- **用量取真实 usage**：`最近一次请求的 input+output tokens + 之后新增内容估算`（CJK 按 1 token/字符，其余按字节/4）；拿不到 usage 时才纯估算。窗口按**模型**配置（`ProviderConfig.Models[].ContextWindow`，缺省 32k 并提示用户配置）。
- 摘要请求用当前 selection、无工具；`Busy=Compacting` 阻止发送；**失败/取消则什么都不动**（不写快照、不归档、状态回滚），提示"压缩失败/已取消"。
- **摘要用 user 角色**（`"以下是此前对话的摘要：…"`）作为 history 第一条——Anthropic 要求首条为 user，OpenAI 也接受。
- **边界规则（简化）**：`kept` 的起点必须是 **user 消息**（本项目工具结果回填在同一条 agent 消息的 part 里，消息粒度切割天然不会拆开 tool 配对；起点落在 user 上同时满足"至少保留一条用户输入"和"摘要(user) 后紧跟 user"）。
- 回合中途超窗导致请求失败 → 该轮 Failed、回 Idle、自动压缩、提示"上下文超出窗口，已自动压缩，请重试"。

### 6.6 界面

- **早期消息按需加载**：滚到顶（`ListState.Visible()` 首行为 0）时读一份归档，**只追加到 `archived []Entry`（独立于 Conversation）**，渲染时拼接在列表前；`ListState.Key` 用 messageID，插入不跳位。归档条目**永不进入** `historyForRequest`。
- 压缩边界显示「以上内容已压缩」分隔行，点击展开摘要原文。

---

## 7. 相对 Rust 设计文档的差异（opus review 修正的落地）

| 编号 | 修正 | 本方案落点 |
|---|---|---|
| D1 | 半行必须截断，否则损坏扩散 | §6.3 打开时 truncate |
| D2 | 归档与 kept 重复 | 按 `kept_from_seq` 截断归档分页 |
| D3 | 归档条目不进 Conversation | `uiapp.archived` 独立切片 |
| D4 | 摘要不能用 agent 角色 | §6.5 摘要用 user |
| D5 | 边界规则简化 | §6.5 起点必须是 user |
| D6 | replay 不放 core | 放 `driver/`（可依赖 providers/tools） |
| D7 | Windows 不能 rename 打开中的文件 | §6.3 先关句柄 + 重试 |
| S1 | 归档用硬链接、目标覆盖不复用 | §6.3 |
| S2 | 用量用真实 usage | §6.5 |
| S3 | 崩溃恢复与停止共用补全函数 | §6.4 `SettleUnfinishedToolCalls` |
| S4 | streaming 何时写入 | §6.2 `message.append` + `message.finalize` |
| S5 | 快照保留本地元数据 | §6.2 `kept_entries` |
| S6 | 单实例锁不用文件存在性判断 | `RequestSingleInstanceLock` |
| S7 | 快照只在新文件第一行 | §6.2 |

---

## 8. 里程碑

| 阶段 | 内容 | 完成标准 |
|---|---|---|
| M1 | `message` + `conversation` + `render` | 表驱动单测覆盖 oneof 严格性、null 语义、ID 唯一性、分类规则；固定 `go 1.27.1` + json/v2 严格解码 |
| M2 | `providers`（含 simulated） | `httptest` 模拟两家 SSE：文本/工具/usage/早结束/取消；真实网关冒烟（保留 Rust 版的 smoke 思路） |
| M3 | `tools`（9 个）+ 沙箱 | 表驱动：读/写/编辑唯一命中、grep 正则、glob、shell 超时与进程树、路径逃逸与悬空链接 |
| M4 | `driver` + `uiapp` 骨架 | **第一件事：NewTester 验证 Enter/Alt+Enter 能否截获（G2），并实测 CJK/emoji/输入法候选窗/高 DPI 与 Windows 空闲内存（原生 vs WebView2，回填 §2.1）**；然后三栏界面 + 流式 + 工具轮 + ask 面板 + 停止；`ui.NewTester` 覆盖发送/停止/删除/重命名 |
| M5 | `store` + replay | JSONL/内存双后端；§6.3/§6.4 的全部崩溃与重放测试 |
| M6 | `compact` | 手动 + 自动触发；摘要失败回滚；边界测试；摘要后 `toOpenAI`/`toAnthropic` 首条为 user |
| M7 | 打磨 | 图片渲染、markdown 完整度、主题、错误提示、`go vet`/`staticcheck` 干净 |

每个阶段独立提交，M1-M3 可与 M4 并行（互不依赖）。

---

## 9. 从 Rust 阶段带入的缺陷结论（必须实现，不能再犯）

1. **停止/崩溃必须补全未配对的 `tool_use`**（opus P1，Rust 版未修完就换了语言）：`SettleUnfinishedToolCalls(messageID, "用户已停止…")`——给每个没有正式结果的 tool_use 写 `{output, is_error:true}`，pending 占位直接覆盖。否则之后每条请求都会被两家 provider 以"tool_use 无配对结果"拒绝，会话报废。
2. **`Cancelled` 不进模型历史**：`historyForRequest` 只取 `Complete`。
3. **流结束用 id 校验**：迟到的 `fail`/`closed` 不得影响下一轮（按当前流的 messageID 判断）。
4. **工具执行窗口禁止发送**：状态机 `RunningTools` 期间 send 被拒（否则历史出现 tool_use 无结果）。
5. **OpenAI 的 usage 在 finish_reason 之后单独成块**：解析器有状态，`Done` 在 `[DONE]`/EOF/收到 usage 后发。
6. **Anthropic 的 `content_block_stop` 对文本块也会发**：只对 tool_use 索引报结束。
7. **工具参数是分片 JSON**：累积到结束才 parse，非法则保留原文并标错误。
8. **沙箱要防悬空链接**：`os.Lstat` 判存在（`os.Stat` 会跟随链接），`EvalSymlinks` 失败即拒绝。
9. **shell 默认关闭**，超时要杀进程树（Windows `taskkill /T /F`；POSIX 用进程组）。
10. **`bash` 在 Windows 实际是 cmd**：描述里写明，避免模型写 bash 语法静默失败。
11. **测试必须走真实入口（G6，流程纪律）**：
    - 原则：**凡是 UI 必须调用才能保证正确性的方法都不应该存在**；
    - driver 的测试只能通过公开入口驱动（不手动模拟状态转移、不手调内部初始化）；
    - uiapp 的关键流程（发送、停止、作答、删除确认、重命名）用 `ui.NewTester` **按文本点击/输入**，不直接调内部函数。
12. **工具接 ctx，可以真正中止（G5）**：`Run(ctx, args, tc)`；停止生成/删除会话时 cancel，shell 命令被杀（`cmd.Cancel` → `taskkill /T /F`；POSIX `Setpgid` + `kill(-pgid)`；`WaitDelay` 防管道卡死）。界面文案从「不会被中止」改为「已中止」。**该产品语义待用户确认**。
13. **其余带入结论**：跨会话的挂起提问只处理当前会话；busy 转移带校验（release 非法转移回 Idle 并记日志）；`send_user_text` 是原子操作（忙时既不追加也不开流）；每个会话独立 ListState；归档与 kept 去重后 `ListState.Key` 必须唯一（重复会让列表错乱）。

---

## 10. 测试策略

- 纯逻辑包（message/conversation/render/markdown/providers/tools/store/driver）：表驱动 `go test`，无网络、无 UI。
- `providers`：`httptest.NewServer` 回放 SSE 样本（`testdata/*.sse`），覆盖分片、CRLF、无尾空行、错误体、早结束、取消。
- `store`：`t.TempDir()` + 人为截断/损坏文件，验证 §6.3 全部崩溃点。
- `uiapp`：`ui.NewTester` 无窗口渲染，按文本点击/输入，覆盖发送、停止、ask 作答、删除确认、重命名。
- 真实网关冒烟：`cmd/smoke`（保留 Rust 版做法），读环境变量，不进 CI。

---

## 11. 已定项与待确认项

**已定（用户拍板 / 采纳 review）**：

- **代码位置**：直接在当前项目 `F:\work\simple-agent` 开发。开工前清空旧仓库（含 `.git`）并重置，从空仓库开始；`.pi/`（mailbox 运行时目录，被占用）保留并在 `.gitignore` 忽略。旧 Rust 仓库完整备份在 `simple-agent-backup-rust-4bdac5f-20261027.zip`（330 文件，含 .git），**仅作参考，不进新仓库**。
- **首次提交**：两份定稿设计文档 `docs/go-rebuild-plan.md`、`docs/persistence-design.md` + `.gitignore`。
- **停止/删除即中止命令（G5，已定）**：取消时结束**整棵进程树**（Windows `taskkill /T /F`，POSIX 进程组），界面文案为「已中止」。
- **界面**：v1 用 **原生 UI**（§2.1 结论），不做 webview。
- 代码高亮用 **chroma**（M7 + G3 缓存）；markdown 解析用 **goldmark**（不手写）。
- 配置用 **TOML**（兼容现有 `simple-agent.toml`），库选 `BurntSushi/toml` 或 `pelletier/go-toml/v2`。
- `data_dir`：`$SIMPLE_AGENT_DATA` → Linux 用 `$XDG_DATA_HOME`（默认 `~/.local/share`）→ 其他平台 `os.UserConfigDir()/simple-agent`。
- 上下文窗口：**内置表 + 配置覆盖**；内置表注明来源与日期；未知模型默认 32k 并提示配置。
- mygo 在 `go.mod` 里**锁精确版本**；升级单独提交并跑一遍 NewTester；依赖只允许出现在 `uiapp`。
- **json/v2 解码**：`RejectUnknownMembers(true)` + `Part` 的 oneof/null 用 `jsontext.Value` 自定义 `UnmarshalJSONFrom`；不得设置 `GOEXPERIMENT=nojsonv2`。

---

## 12. 已定决策汇总（实现时逐条照做）

这一节把整条路上**已经定下来的事情**集中记下来。以后出现分歧时先查这里。
「来源」一列区分**用户决定**与**评审结论**——后者是 review 中确认的技术选择，
改动时按工程判断走；前者要改必须先问用户。

| # | 决定 | 来源 | 实现位置 |
|---|---|---|---|
| 1 | **Rust 版停止开发**，改用 Go + mygo 重写 | 用户 | 本仓库（前身停在 `c5ca872`） |
| 2 | **代码在当前仓库开发**：开工前清空旧仓库并重置 git，从空仓库开始 | 用户 | 提交 `32faf5b` 起的线性历史 |
| 3 | **已定需求全部沿用到 Go 版**（数据模型、协议适配、工具语义、状态机、持久化、Rust 阶段 review 出的缺陷结论） | 用户 | 对应各里程碑（见 §13） |
| 4 | **Enter 发送、Alt+Enter 换行**（Shift+Enter 也换行，输入法组合期间一律放行） | 用户 | `uiapp/composer.go`、`composer_keys_test.go` |
| 5 | **停止生成与删除会话都要中止命令**（整棵进程树，文案「已中止」） | 用户 | `tools/shell.go`、`proctree_*.go`、`uiapp/app.go` |
| 6 | **没有 Key 时不自动选 Ollama**，走模拟提示引导配置；要用 Ollama 需显式 `active_provider` | 用户 | `providers/registry.go`、`gateway_test.go` |
| 7 | **持久化**：JSONL 与内存两种后端；每次上下文压缩时滚动文件；加载只读最新的 `session.jsonl`；归档全部保留；压缩前的消息**按需加载、只用于显示**；「以上内容已压缩」分隔线可展开摘要 | 用户 | `store/`、`driver/replay.go`、`uiapp/app.go` |
| 8 | **压缩**：手动 + 自动；阈值 80%；摘要用当前模型生成（不带工具）；**工具轮中途不压缩**；超出窗口导致失败后压缩一次并提示重试 | 用户 | `driver/compact.go`、`auto_compact_test.go` |
| 9 | **崩溃后的表现**：生成中崩溃显示「已中断」占位；执行工具中崩溃把调用标记为「已中断」，**不重跑** | 用户 | `driver/replay.go`（repairs）、`replay_test.go` |
| 10 | **单实例**：第二个实例先给出提示再退出；第一个实例把窗口拉到前台并留一条显示型提示 | 用户 | `cmd/simple-agent/main.go` |
| 11 | **v1 用原生 UI**（不做 webview 混排） | 用户 | `internal/uiapp`（唯一 import `ui` 的包） |
| 12 | **新仓库的第一次提交只放设计文档**（+ `.gitignore`） | 用户 | 提交 `32faf5b` |
| 13 | **摘要消息用 user 角色**、放在请求历史第一条；kept 起点必须是一条 user 消息 | 评审结论 | `driver/compact.go`、`compact_test.go` |
| 14 | **停止/删除后不再自动压缩**（与用户「停下来」的意图一致） | 评审结论 | `driver/auto_compact_test.go` |
| 15 | **v1 不做自定义主题**，使用 mygo 内置的深色/浅色主题，等试用后再评估 | 用户 | 全部界面用 `c.Theme()`（无自定义色板） |
| 16 | **插话（steer）**：回合进行中（Streaming / RunningTools）可以继续输入，回车把消息**排队**，在回合的下一个安全边界投递成真正的 user 消息 | 用户 | `driver/driver.go`（`Enqueue`/`deliverQueued`）、`uiapp/app.go`（`composer`/`Send`） |
| 17 | **投递点**：工具轮之间 + 回答结束后 + 自动压缩完成之后（三处都必须"工具配对完整"） | 用户 | `continueAfterTools`、`finishStream`、`ApplyCompact` |
| 18 | **多条插话一次性投递，作为多个 user 消息**（不拼成一条）。**回合失败（不是停止）时队列保留**，不自动投递也不自动开流；用户下次按发送时，先按顺序把队列作为 user 消息追加，再追加这次输入，**只开一轮流**（顺序为「插话A → 插话B → 这次输入」）。压缩失败同理 | 用户（修法 B） | `deliverQueued`、`Driver.Send`、`TestSendAfterFailureDeliversQueueFirst`、`TestCompactFailureKeepsQueue` |
| 19 | **停止时清空插话队列**（与 #14 同源：停下就不再自动动作）。**压缩中点停止也算停止**（`CancelCompact`，清空队列）；自动压缩**失败**不算停止（保留队列，见 #18） | 用户（修法 B） | `Driver.Stop`、`Driver.CancelCompact`、`DeleteCurrentTurn`、`TestCancelCompactClearsQueue` |
| 20 | 插话队列**不落盘**（语义等同草稿；崩溃即丢，不改动事件清单与重放语义） | 开发决定 | `Driver.queued`（无 `Record` 事件） |
| 21 | **Anthropic 相邻同角色消息暂不合并**（属适配器职责，单独提交再评估；当前依赖服务端把相邻 user 合并成一个 turn） | 用户 | `providers/mapping.go`（未改动，代码内有注释标记） |
| 22 | **Anthropic 不写 `reasoning_effort`**（Messages API 无此字段，未知顶层字段会 400）：适配器不写，界面也不给 anthropic 类提供商显示档位选择；要开思考请用 `extra_body` 的 `thinking` | 评审结论（P2-1） | `providers/anthropic.go`、`cmd/simple-agent/main.go`、`TestAnthropicNeverSendsReasoningEffort` |
| 23 | **流式工具参数只落前 2KB**（`liveRawArgsLimit`）：界面摘要用的键都在开头，完整参数仍由 `collectToolCalls` 从缓冲区解析。避免"每个增量重写整段 part"的 O(n²) 复制 | 评审结论（P2-2） | `driver.clipRawArgs`、`TestLiveToolArgsAreClipped` |
| 24 | **请求交给异步开流前必须深拷贝**（`deepCopyMessages`）：`History()` 是浅拷贝，DataPart 的 map 与会话共享，主线程继续改 part 会真实竞争 | 评审结论（P2-3） | `driver.buildRequest`、`TestRequestIsDeepCopiedForAsyncMarshal`（`-race`） |
| 25 | **`PruneEmpty` 保守删目录**：目录里除 `session.jsonl` 之外只要有别的文件（归档、`session.corrupt-*.jsonl` 备份）就跳过 | 评审结论（P2-4） | `store.hasOtherFiles`、`TestPruneEmptyKeepsDirectoriesWithBackups` |
| 26 | **图片附件**：粘贴（Ctrl/Cmd+V，只有剪贴板真有图片才认领，否则放行文本粘贴）与拖入（整个聊天区可接收）；只收按内容嗅探出的 png/jpeg/gif/webp，其他格式给提示；读文件/写存储/解码缩略图都在后台 goroutine | 用户 | `uiapp/attachments.go`、`uiapp/composer.go`、`cmd/simple-agent/paste.go` |
| 27 | **blob 存储**（内容寻址、通用，不只给图片）：`<data_dir>/blobs/<sha256 前2位>/<hash>`，无扩展名；引用写在 part 里 `URLPart("blob:sha256:<hex>")` + `mediaType`/`filename`；JSONL 只存引用；**先 Put 成功再写引用事件**；`Open` 只接受 64 位小写 hex 并校验内容哈希 | 用户 | `store/blobs.go`、`render.BlobHash`、`TestBlobsRejectBadHash` |
| 28 | **发送时读图片**：引用在**异步开流的 goroutine**里读成字节（`providers.BlobReader`），主线程不读文件；读不到（缺失/损坏）替换成文字「[图片丢失]」，界面显示同名占位 | 评审结论（P2-1） | `providers.Extract`、`uiapp/images.go`（`renderBlobImage`） |
| 29 | **vision 默认不支持**：模型配置 `vision: true` 才允许发图片；不支持时禁止发送并提示；历史里的图片发送时替换成「[图片]」，同一个会话同一个模型只提示一次（显示型 `DisplayNotice`，不落盘） | 用户 | `config.ModelEntry.Vision`、`driver.replaceImagesForModel`、`driver.noteVisionImages` |
| 30 | **图片按固定值计入上下文估算**（每张 1000 token），多图会话的自动压缩才不会太晚触发 | 评审结论（P2-4） | `driver/compact.go`（`imageTokens`） |
| 31 | **blob 清理：v1 不清理**，删除会话也不删 blob。备选方案（以后需要时再做）：`--gc-blobs` 在会话与归档文件字节上用正则抓 `blob:sha256:<hex>` 得到 reachable 集合，sweep 不在集合且 mtime > 24h 的 blob（24h 保护"刚 Put、事件还没写"的窗口），不做引用计数 | 用户 | —（未实现，见本文档） |
| 32 | **markdown 里的 `blob:` 不生效**：只有消息 part 里的引用会被解析；markdown 图片仍只允许 http(s) | 评审结论（P2-4） | `uiapp/markdown.go`、`render.BlobHash` 仅用于 part 分类 |
| 33 | **只含图片的用户消息不进翻历史**（`historyTexts` 跳过空文本） | 开发决定 | `uiapp/app.go`（`historyTexts`） |
| 34 | **文字选中复制**：正文与代码块用 `Selectable()`（mygo 的只读编辑器），选中后 Ctrl/Cmd+C 复制。**选择范围限制在单个文本元素内**（不能跨段落/跨消息），v1 接受这个限制。注意代码块是**每行一个可选元素**，所以**不能在代码块里跨行拖选**——要整段复制请用代码块右上角的复制按钮（不是 bug，是 v1 的已知限制） | 用户 | `uiapp/markdown.go`、`TestMessageTextIsSelectable` |
| 35 | **每条消息一个「复制」按钮**：复制该消息里所有正文的 **markdown 原文**（多个 part 用空行分隔），**不含**推理、工具调用/结果、压缩摘要与图片；流式输出的消息不显示按钮 | 用户 + 评审默认 | `uiapp/copy.go`（`messageCopyText`） |
| 36 | **代码块右上角「复制」按钮**：只复制该段代码（不带 ``` 围栏与语言标记，末尾换行也去掉）；点击后按钮显示「已复制」约 1.5 秒（`time.Until` + `c.After` 到期重绘，不用 goroutine）。key = 消息 id + part 序号 + 代码内容的 FNV 哈希，跨帧稳定；已知小限制：同一 part 里两段**内容完全相同**的代码块 key 相同，点一个时两个都会短暂显示「已复制」 | 用户 + 评审默认 | `uiapp/markdown.go`（`renderCodeBlock`）、`uiapp/copy.go` |
| 37 | **界面按 deepseek-harness 尽量复刻**：配色改用 dsh 的浅色色板（品牌蓝 `rgb(65,118,230)`、偏蓝中性灰阶、border-l1..l4、交互态、menu、turn-trigger、代码块等）；**推翻本表第 15 条** | 用户 | `uiapp/theme_dsh.go`、`docs/ui-redesign-plan.md` §0.5 |
| 38 | **侧栏尺寸与收起按 dsh**（`ui-layout/client/columns.ts`）：默认 280、可拖 264–420、**收起后 56px 图标栏**、窗口 <1024 自动收起、正文区最小 400 | 用户 | `uiapp/layout.go`（宽度常量已落，收起/自动收起随 P0.1） |
| 39 | **不打包 Montserrat 字体**：品牌名与正文都用系统字体栈，代码用 dsh 的代码栈（`ui-theme/base.css`） | 用户 | `uiapp/theme_dsh.go`（`dshTextFontStack` / `dshCodeFontStack`） |
| 40 | **界面始终为 dsh 浅色**：不跟随系统深色模式（系统深色时也用浅色），`themeFor` 是唯一开关 | 用户 | `uiapp/theme_dsh.go`（`themeFor`）、`TestDarkModeAlsoUsesDSHLight` |
| 41 | **会话按 workspace 分组**：`session.created` 记 `workspace`（`driver.Options.Workspace`）；旧日志没有该字段 → 归「默认」；侧栏两级树（项目 → 会话），组内按创建倒序，组名取目录名 | 用户（路径 A） | `driver/record.go`、`uiapp/layout.go`（`sidebarTree`）、`TestSidebarGroupsByWorkspace` |
| 42 | **侧栏交互**：搜索框过滤标题；右键菜单（改名 / 删除）+「⋯」菜单；删除二次确认用 `AlertDialog`（标题 + 不可恢复说明）；会话行显示相对时间（刚刚 / Nm / Nh / Nd） | 用户 | `uiapp/layout.go`（`sidebarSessionRow`/`confirmDialogs`）、`TestSidebarDeleteNeedsConfirm` |
| 43 | **相邻相同 CJK 字的叠字只在 `ui.NewTester` 的离屏栅格化里出现，真实窗口正常**（用户实测 cjk-repro.exe）。**不报 mygo 上游、不做规避**（`separateRepeatedCJK` 已撤除，hair space 只会污染复制内容）。推论：**视觉 review 时 Tester 截图与真实窗口可能不一致，字形相关结论必须用真实窗口确认** | 用户 | `uiapp/layout.go`（`relativeTime`）、`/tmp/cjk-repro`、`/tmp/simple-agent-cjk-repeat-matrix.png` |
| 44 | **图标用 dsh 的 SVG**：新建会话 / 侧栏开关搬自 `ui-primitives/src/icons`（MIT），`uiapp/icons.go` 里转成静态 SVG 常量 + `MustParseSVG` 解析；图标按钮只有**无障碍名**（「新建会话」「收起侧边栏」「打开侧边栏」），测试按标签定位 | 用户 | `uiapp/icons.go`、`uiapp/layout.go`（`sidebarRail`/`sidebarBody`）、`THIRD_PARTY_NOTICES.md` |
| 45 | **界面中英双语**：默认跟随系统（Windows 用 `GetUserDefaultUILanguage` 的 LANGID，`&0x3FF==0x04` 为中文；其他平台看 `LANG`），配置 `language: auto|zh|en` 可指定；dsh 已有的文案用它 zh/en 两套，其余按 dsh 风格翻译。**不翻译**：模型回复、用户内容、系统提示词、工具返回给模型的结果正文、已落盘的旧 notice；**只有 `--help` 仍是中文** | 用户 | `internal/locale/`、`uiapp/locale` 接入、`TestEnglishUI`、`TestTablesAreComplete` |
| 46 | **locale 表的守护**：zh/en 的 key 集合完全一致、值非空、**格式占位符个数一致**；并扫描代码里 `tr("…")` 的字面量断言 key 都在表里（动态拼 key 不允许） | 评审结论 | `internal/locale/locale_test.go` |
| 47 | **回合结束行**（dsh turn footer）：**只在回合真正结束时显示**（后面已有新的 user 消息，或会话空闲且无流式消息；工具轮之间/等待回答时不显示，避免闪烁）。成功「已完成，用时 22秒」（<1s 只显示「已完成」）+ **本回合所有消息的 token 之和**；失败「▸ 处理失败 · 原因摘要」，点开展示完整原因与建议（**与 dsh 不同**：dsh 默认不显示原因）；中止「已中止」。耗时 = 回合前那条 user 消息的 `CreatedAt` → 最后一条消息的 `finishedAt`，**内存与重放共用**（重放用事件 at） | 用户 + 评审结论 | `uiapp/turnview.go`（`renderTurnFooter`/`turnBounds`）、`conversation.ChatEntry.FinishedAt`、`driver/replay.go` |
| 48 | **消息元信息**：用户气泡下方右对齐「10/29 10:51 + 复制」；行内代码改成 dsh 的 chip（正常字重 + `dsh.InlineCode` 浅底）；**已知限制**：mygo 的 Span 背景是直角、无内边距 | 用户 + 评审默认 | `uiapp/copy.go`（`formatMessageTime`）、`uiapp/markdown.go`（spans） |
| 49 | **过程组与通知的图标**：活动分类 → dsh 的 `PROCESS_ICONS`（think/browse/search/edit/api/code/globe/agent/question/sparkle），通知按级别用 warning/info 图标（MIT，见 THIRD_PARTY_NOTICES）；dsh 的 plan 图标（ListPen）暂用编辑图标代替 | 用户 | `uiapp/icons_activity.go` |
| 50 | **P3 头部与输入卡**：头部只留会话名 + 忙状态 + 右侧图标按钮（侧栏开关、更多菜单：压缩/删除）；模型选择**只在输入卡右下**（原生 `ui.Menu`：提供商 → 模型、再加「思考档位」子菜单；忙时整份菜单禁用并给一条说明）；`+` 按钮走 `mygo.Dialog.Open`（阻塞 → goroutine + post）；「权限」只读展示 `allow_shell`（dsh 的 `permission.fullAccess` / `permission.workspaceWrite`）+ Tooltip 指向配置；发送按钮是圆形近黑（`dsh.ButtonSolid`）+ dsh 的发送箭头 | 用户 | `uiapp/composer_ui.go`、`uiapp/app.go`（header）、`cmd/simple-agent/paste.go`（`pickImageFiles`）、`TestModelSwitcher` |
| 51 | **底部状态条**：输入卡下方单独一行，左侧「上下文 x / y（n%）」+ `ui.Meter` 条形（`Warning` = `driver.AutoCompactRatio`，`Critical` = 95%），右侧「N 轮」「N 步」。**计数口径**：Turns = 用户输入条数、Steps = 发给模型的请求次数（工具续轮/插话续轮各算一步）——两者都能从会话内容推导，所以「重放 == 在线」成立；压缩丢弃历史，故 `compact.snapshot` 带上累计值当基线（`turns`/`steps` 可选字段）。tok/s、cache hit、环形仍不做（见 §6.2） | 用户（口径由实现定，测试钉住） | `driver.Driver.turns/steps`、`Driver.Counts()`、`driver/compact.go`（快照基线）、`driver/replay.go`、`uiapp/app.go`（`statusBar`）、`driver/counts_test.go`、`uiapp/statusbar_test.go` |
| 52 | **placeholder 里提示 Alt+Enter**（评审建议）：`输入消息，Alt+Enter 换行` / `Message, Alt+Enter for a new line`——键盘提示不再占一行，但不该让人猜不到换行键 | 评审建议 | `locale.tables`（`composer.placeholder`） |
| 53 | **悬停「⋯」菜单必须每帧创建**：mygo 的原生菜单在"打开"与"选中之后"两帧都按**元素 id** 回找元素并重跑 build（`ui/menu.go`）。「⋯」若只在 `Hovered()` 时创建，指针移到弹出菜单上后元素消失 → `Chosen()` 永不触发（真实窗口里"点删除没反应"）。现在「⋯」每帧创建、不悬停时**透明**（不能 `Disabled`：open 路径会跳过 disabled 元素，菜单直接不弹），无障碍名带会话标题便于定位 | 评审结论（用户 bug） | `uiapp/layout.go`（`sidebarSessionRow`）、`TestSessionMenuWorksAfterPointerLeavesRow` |
| 54 | **删掉最后一个会话 → 允许列表为空并显示空态**（用户选 B）：主区域「还没有会话 / 新建一个会话开始对话 + 新建会话」按钮，**不渲染**头部与输入卡；侧栏树为空时显示一行淡色「还没有会话」，新建入口（含收起后的图标栏）照用；**启动时一个会话都没有也进入空态，不再自动新建空会话**；无会话时 Send/Stop/Compact/拖放/粘贴/翻历史都安全地什么都不做（拖入图片直接忽略） | 用户 | `uiapp/app.go`（`emptyStatePane`、`deleteSession`、`restoreSessions`）、`uiapp/layout.go`（侧栏空态）、`TestEmptyStateAfterDeletingLastSession`、`TestStartupWithNoSessionsShowsEmptyState` ；**空态按钮用 dsh 的近黑主按钮**（`ButtonSolid`，不是 PrimaryButton 的品牌蓝） | 用户 | `uiapp/app.go`（`emptyStatePane`、`deleteSession`、`restoreSessions`、`primaryButton`）、`TestEmptyStateAfterDeletingLastSession`、`TestStartupWithNoSessionsShowsEmptyState` |
| 55 | **启动/删除失败必须可见**：有会话时 `startupErr` 用**显示型通知**、无会话时显示在**空态**里（Danger 色 + 数据目录路径）；删除失败用 **Toast**；多个会话恢复失败要**累计**（「N 个会话恢复失败（第一个：…）」），不能只留最后一条 | 评审结论（P1） | `uiapp/app.go`（`restoreSessions`/`emptyStatePane`/`flashErr`）、`TestRestoreFailureIsVisible`、`TestDeleteFailureShowsToast` |
| 56 | **工具输出可能是非 UTF-8，写日志绝不能 panic**：中文 Windows 上读 GBK 文件/命令输出会带出非法字节，`json/v2` 拒绝编码 + `MustEvent` 直接 panic 掉界面线程（用户实测）。三层防护：工具层（`read`/`search` 用 `decodeConsole`、`OutputOf` 统一 `sanitizeUTF8`、`edit` 遇非 UTF-8 文件明确报错）、`store` 层（`NewEvent` 编码失败时反射修掉非法字节后重试）、测试层（store/driver/tools 各一条回归） | 用户 | `tools/utf8.go`、`tools/fs.go`、`store/sanitize.go`、`store/event.go`、`TestEventWithInvalidUTF8DoesNotPanic`、`TestToolResultWithInvalidUTF8DoesNotPanic`、`TestReadGBKFileIsValidUTF8`、`TestEditNonUTF8FileFails` ；`write` 工具描述明确"以 UTF-8 写入"，`read` 遇非 UTF-8 文件在输出开头提示"已按系统代码页解码、写回会变成 UTF-8"（v1 不拦截这种转换，只告知） | 评审补充 | `tools/fs.go`（`readNonUTF8Note`）、`TestReadGBKFileIsValidUTF8` |
| 57 | **状态条的窗口必须取会话自己的模型**：`contextNumbers` 以前读 `Options.ContextWindow`（默认模型的窗口），切换/恢复会话后「x / y（n%）」与 Meter 警告色都按错误的窗口算，和自动压缩阈值对不上。改为 `session.drv.EffectiveContextWindow()`（driver 新增导出）；删除会同时改 opts 与 drv、掩盖这个 bug 的 `SetContextWindowForTest`，测试改走真实路径（`WindowFor` + `SelectModelForTest`） | 评审结论（P2） | `driver/compact.go`（`EffectiveContextWindow`）、`uiapp/app.go`（`contextNumbers`）、`TestStatusBarUsesSessionWindow` |
| 58 | **designer（另一会话）改动的处理规则**：有问题由我修；功能完成即提交——修好后把相关改动（含 designer 留在工作区的部分）一起提交，不长期挂在工作区。流程：① 先告诉 opus 改了哪些文件、什么功能；② 有问题我修，没问题则**单独提交**并在提交信息注明「designer 的改动：xxx」；③ 始终 `git add <具体文件>`，不用 `-A` | 用户 | 本文件 §12 |
| 59 | **`read`/`edit` 拒绝二进制文件**（崩溃的真正入口：用户那次是 `read` 读了 PNG）。判定与 dsh `fs-local` 一致：数据**前 8192 字节**内出现 NUL 即判为二进制（`BINARY_SAMPLE_BYTES`）；`read` 返回「无法读取 x：二进制文件（不是文本）。如需查看图片，请在输入框粘贴或拖入图片作为附件」，`edit` 返回「无法编辑 x：二进制文件」；工具描述写明 text-only。`search` 本来就用 `isTextFile`（前 4KB 无 NUL）跳过二进制。dsh 另有独立的 `read_image` 工具（PNG/JPEG/WebP/GIF → 图片内容块，自动缩放），我们 v1 未实现（工具结果目前只支持文本） | 用户 | `tools/fs.go`（`isBinarySample`）、`TestReadBinaryFileRefused` ；**UTF-16 文本不算二进制**：按 BOM（`FF FE`/`FE FF`）解码成 UTF-8 后读取，并在输出开头提示「文件是 UTF-16，已转换；用 write 写回会变成 UTF-8」；UTF-8 的 BOM 顺手剥掉；`edit` 对 UTF-16 明确拒绝（不做隐式编码转换，`FF FE`/`FE FF` 判定见 `isUTF16`） | 评审补充 | `tools/fs.go`（`decodeBOMText`/`isUTF16`）、`TestReadUTF16File` |
| 60 | **新增 `read_image` 工具（对齐 dsh）**：读 PNG/JPEG/WebP/GIF 并把图片本身交给模型。规则取自 dsh `packages/fs/tool-fs/src/read-image.ts`：按**文件签名**判类型（不看扩展名）、限额 20MB / 单边 8192px / 6400 万像素、长边超 1568px 自动缩小（区域平均重采样，透明通道留 PNG、否则 JPEG）、输出用 dsh 的 `<path>/<type>/<content>` 信封并注明 `downscaled from WxH`；**模型无图片输入时直接拒绝**（对齐 dsh 的 route 门禁）。图片以 `blob:sha256:` 引用存进 `tool.results`（事件字段 `images`），重放保留；OpenAI 走 tool 消息的 content 数组 + `image_url`，Anthropic 走 `tool_result` 的 image 块；无 blob / 读不到降级成文字，模型切到非视觉时请求里剥掉图片并注明「图片已省略」。界面：展开「工具结果」显示缩略图 | 用户 | `tools/image.go`、`driver`（`ToolResult`/`replay`/`stripToolResultImages`）、`providers/mapping.go`、`uiapp`（`persistToolImages`/`toolOutput`）、`TestReadImageTool`、`TestToolResultImagesMapping`、`TestToolResultImagesPersistAndReplay`、`TestToolResultImagesStrippedForTextModel`、`TestReadImageEndToEnd` 。**OpenAI 的 tool 消息只放文本**（规范不允许 image_url），工具结果的图片收集后在这批 tool 消息之后追加一条 user 消息（`Attached image(s) from tool result:` + image_url，多条结果合并成一条；只有图片没有文本时用占位 `(see attached image)`），与 dsh 用的 pi-ai 适配器一致；`estimateTokens` 也把 tool 结果里的图片按张计入（`imageCount*imageTokens`），否则连续读图时自动压缩触发太晚 | 评审结论（P1/P2） | `providers/mapping.go`、`driver/compact.go`、`TestToolResultImagesMapping`、`TestToolResultImagesMergeIntoOneUserMessage` |
| 61 | **远程执行端/展示端（relay）方案与范围**：设计见 `docs/remote-design.md`。用户确认：执行端**默认只监听 `127.0.0.1`**（对外需显式 `--listen 0.0.0.0:7801`）；`--connect` 与本地模式互斥（P4 前展示端不跑本地会话）；多端"停止"不加额外提示；relay 用 `simple-agent --relay`，不另出程序；展示端不缓存会话日志、只缓存 blob（`viewer.blob_cache_mb` 默认 512，LRU）；单事件超包上限走 `log.event.large` 分块、包上限不调大。展示端可操作、多端按到达顺序、先答为准、界面状态本地不同步（§0.2） | 用户 | `docs/remote-design.md` §11.2 |
| 62 | **P1（远程重构）实施顺序**：先拆 `driver.ApplyStreamDelta`/`FinishStream`（`ApplyEvent` 为两者组合，执行端零变化）→ 再引入 `internal/host`（`Host` 接口 + `localHost`，搬 `applyEffects`/`startStream`/`runTools`/`startSummary`）→ 再改 uiapp 调用点。命令一律异步 `Submit`，结果经 `Change{Kind:"cmdResult"}` 回界面线程；改动可拆多个提交，每个提交都要编译且测试全绿 | 用户（评审） | 本文 §10 P1 。**第 1 步（已完成）**：`driver.ApplyEvent` 拆成 `ApplyStreamDelta`（纯增量：文字/推理/tool_use name 与 raw_args）+ `FinishStream`（回合推进），`ApplyEvent` 为两者组合；新增 `Mirror`（只应用增量 + `ApplyPersisted`，`Replay` 的 `apply` 抽成 `replayState` 供增量续传共用）与 `conversation.SetStreamingParts`（快照恢复流式消息）；测试 `TestApplyEventSplit`、`TestStreamDeltaMatchesExecutor` | 用户（评审） | `driver/driver.go`、`driver/replay.go`、`driver/mirror.go`、`conversation/conversation.go` 。**第 1 步补充（评审 P1）**：`tool_use` part 写入 provider 的 `stream_index`（Anthropic 是 content block 下标，不是工具序号），镜像接管时按它恢复 `toolParts`，并从 part 的 `id`/`name`/`raw_args` 预填缓冲区；`ReplaceInflight` 丢弃旧 Turn 重建；测试 `TestMirrorTakeoverMidToolArgs`（text=0 + tool_use=1，参数流到一半接管）、`TestMirrorTakeoverMidReasoning` | 评审结论（P1） | `driver/driver.go`、`driver/mirror.go`、`driver/mirror_test.go` 。**第 2 步（已完成）**：新增 `internal/host`（`Host` 接口 + `Local` 执行端 + 会话/效果执行：startStream、runTools、startSummary、record、applyEffects、LoadOlder、blob 落盘、启动恢复）。命令统一 `Submit(Command)`（异步语义：立即返回 CmdID，结果经 `Change{Kind:"cmdResult"}` 回界面线程；单机实现直接执行但调用方不得依赖"返回即生效"）。uiapp 的 `Session` 只保留界面状态并嵌入 `*host.Session`，读接口全部走 host；`Options` 里执行端相关字段仅作为构造 host 的输入。测试 `TestLocalHostMatchesDirectDriver`、`TestCommandIsAsync` | 用户（评审） | `internal/host/`、`uiapp/app.go` 。**第 2 步补充（评审 P1）**：发送/插话改为"乐观清空 + `pending` 记录"，`cmdResult` 失败时还原草稿与附件（用户已在输入新内容则只提示、不覆盖）；`errMsg` 只保留在 uiapp（host 不再存第二份，错误只经 `cmdResult`/`notice` 表达）；显示型提示只由执行端写 Conversation，uiapp 收到 `notice` 只重绘（否则同一提示出现两次）；`Local` 新增 `Deferred` 模式，单机默认同步但用 Deferred 跑异步语义测试 | 评审结论（P1/P2） | `uiapp/app.go`、`uiapp/async_test.go`、`host/local.go` |
| 63 | **P2（远程只读同步）已实现**：`internal/wire`（Packet/Conn/`PipePair`、Hello/Welcome 与凭据预留、`SendLarge` 分块 + `Reassembler` sha256 校验）；`internal/host/remote.go`（执行端 `Serve`：catalog/attach/log.begin/log.events/log.end/state.snapshot，事件与 stream 增量转发、notice、cmdResult；展示端 `Remote`：镜像会话 + 游标 `(generation, seq)` 断线续传、快照 `Inflight` 补齐已输出内容、所有镜像修改都投递回界面线程）。协议序号用连接级 `wireSeq`（日志 seq 压缩后会重置，不能当游标）；首次同步强制全量，续传命中 ring 时 `truncated=false` | 用户（评审） | `internal/wire/`、`host/remote.go`、`TestViewerSeesStreamingText`、`TestViewerSeesInflightText`、`TestViewerReconnectResumes`、`TestViewerSnapshotFallback`、`TestViewerSeesToolRun` |
| 64 | **P2 序号/观察者归执行端所有**（评审 P0）：持久事件与瞬时增量由 `Local` 在**产生时**打序号（`eventSeq` 进 ring、`streamSeq` 与快照 `Seq` 同一数列），与"是否有展示端在线"无关——断线期间的事件同样有序号与 ring 记录；观察者改成**集合**（`AddObserver`/`RemoveObserver`），支持多展示端并存、断开互不影响；bug 修复：此前序号由每条连接的 execServer 打、ring 只记录在线事件 ⇒ 断线期间静默丢消息、重连误判"无缺口"，且第二个展示端会顶掉第一个。压缩时 `generation++` 并清空 ring。验收测试重写：断线期间跑完整一轮后重连**两端深度相等**且 `truncated=false`、小 ring 覆盖不到时 `truncated=true` 且相等、双展示端同时在线/一个断开另一个继续、断线期间压缩后重连、显示型提示后连可见、中途 attach 文本正好 `ABC` | 评审结论（P0/P1） | `host/local.go`、`host/session.go`、`host/remote.go`、`host/remote_test.go` |
| 65 | **续传游标是 `(epoch, generation, seq)`，全量同步的游标用 `eventSeq`**（评审 P0）：全量事件只重建状态、不推进游标，`log.end` 带 `to_seq`；`attach` 带 epoch，执行端重启（epoch 变）一律全量。修复两处静默丢消息：全量后游标落在日志 seq 上（压缩后会重置）、重启后 eventSeq 从 1 开始被误判为已收到。内存后端（无 data_dir）给不出完整历史，只返回快照+增量（文档已写明远程模式建议用 JSONL）。测试 `TestViewerResumeAfterRestoredSession`、`TestViewerReconnectAfterExecutorRestart` | 评审结论（P0） | `host/remote.go`、`host/remote_test.go`、`docs/remote-design.md` |
| 66 | **内存后端要"让用户看得见"**（评审补充）：执行端首次服务连接时若没有 `data_dir`，打一条启动警告日志；展示端 attach 时执行端给该会话加一条显示型提示「执行端未启用持久化，更早的历史无法同步」（每会话一次，随快照同步到所有端），不显示成空会话 | 评审补充 | `host/remote.go`（`Persistent`/`notifyNoPersistence`）、`TestViewerSeesNoPersistenceNotice` |
| 67 | **显示型提示要按位置同步**（评审补充）：`Notice` 增加位置锚点 `After`（提示发生时最后一个条目的 id，空表示最前），`conversation.InsertNoticeAfter` 按锚点插入、找不到才落末尾；展示端合并快照提示时按锚点插回原位置。`digestOf` 改回**按顺序**比较（不再排序掩盖差异），第二展示端用例在不排序下必须通过 | 评审补充 | `conversation`（`InsertNoticeAfter`）、`host`（`Notice.After`）、`TestViewerSeesDisplayNotice` |
| 68 | **P3 第一步：remoteHost 接入 uiapp + TCP 直连 + `--listen`/`--connect`**：`Host` 接口加 `Active/SetActive/StartupError`，`uiapp.Options.Host` 可注入远端执行端；`wire.NewTCPConn/Dial`（4 字节长度前缀、8MB 帧上限）；`--listen addr`（默认建议 `127.0.0.1:7801`，执行端 UI + 监听）与 `--connect addr`（展示端不拿单实例锁/目录锁、注入 Remote）**互斥**；两个 flag 在 `config.Load` 之前解析并摘除。真实双进程验证见提交说明 | 用户（评审） | `cmd/simple-agent/main.go`、`cmd/simple-agent/remote.go`、`wire/tcp.go`、`uiapp/app.go`、`host/host.go` |
| 69 | **P3 第二步：发送队列与自动重连**（评审 P1）：每条展示端连接一个**有界发送队列 + 专属写 goroutine**，观察者回调（主线程）只编码入队、绝不碰 socket；队列满（`outQueueLimit`，默认 2000 包，测试可注入）**直接断开该端**（丢包会破坏有序前提），由断线续传追上来；`tcpConn` 写超时 30s；`Serve` 返回/队列溢出/写失败都关闭连接。展示端 `NewRemoteWithDial`：断线后 3s 起指数退避到 30s 自动重连，按 `(epoch, generation, seq)` 游标重新 attach，连接状态以 `Change{Kind:"connection"}` 通知界面（会话内提示）。`--listen` 关闭监听器与在服务的连接；`--help` 说明 `--listen` 仍独占数据目录 | 评审结论（P1/P2） | `host/remote.go`、`wire/tcp.go`、`cmd/simple-agent/remote.go`、`TestStalledViewerDoesNotBlockExecutor`、`TestViewerAutoReconnect` |
| 70 | **展示端传输层改为 actor（单 goroutine 连接管理器）**（评审 P1）：`connManager` 独占 conn/connID/connected/发送队列/pending 命令/重连退避；每条物理连接一个只转发的读 goroutine + 一个专属写 goroutine；`attach` 计划用"只保留最新一份"的通道合并（永不丢最后一次）；过期 connID 消息直接丢弃；`Submit` 只入队（非阻塞，断线/繁忙立即失败、不补发），结果走 `cmdResult`；`Close` 终止、`Reconnect` 走 `useConn`。测试：现有重连/回退/压缩后重连/双端 + `TestSubmitFailsWhileDisconnected` + `TestRapidReconnectCycles`（10 轮断线重连夹 Submit 与执行端事件），`-race -count=10` 全绿 | 评审结论（P1） | `host/connmanager.go`、`host/remote_conn.go`、`host/remote.go`、`host/remote_test.go` |
| 71 | **命令回执的三态语义**（评审 P1）：管理器给每条命令记 `cmdQueued`/`cmdSent`，收到 `cmd.result` 时从 pending 删除；写 goroutine **写之前**声明"已写出"（写失败无法确定对端是否收到）；断线时未写出的回「连接中断，命令未送达」（界面还原草稿），已写出的回 `CmdResult{Unknown:true}`「结果未知」（界面**不还原草稿**，只提示重连后查看）；断线处理前先 drain 已排到的"已写出"回执，避免竞态误判。`adopt` 的 hello/attach 改非阻塞入队，写满即断连重连（避免管理器 goroutine 卡死） | 评审结论（P1/P2） | `host/connmanager.go`、`uiapp/app.go`、`TestNoFailureForCompletedCommandsOnDisconnect`、`TestUnknownForSentCommandOnDisconnect`、`TestNotSentForQueuedCommandOnDisconnect` |
| 72 | **分块机制通用化 + 通知不丢**（评审）：`wire.Fragment` 增加 `Kind`（重组后交给谁处理），`Reassembler.Add` 返回 `(kind, payload, done)`，两端按 kind 分发——修掉"执行端把**所有**重组结果当 `cmd`"（blob 的 commit 因此永远到不了）；`SendLarge` 带 innerKind，`log.events` 分块声明为 `log.events`。`connManager` 的读/写 goroutine 通知 inbox 改为**阻塞发送** + `done` 脱身（丢 `written` 会被误判未送达→重复发送；丢 `err` 会让管理器永远以为在线） | 评审结论（P1） | `wire/wire.go`、`host/remote.go`、`host/remote_conn.go`、`host/connmanager.go`、`wire/wire_test.go` |
| 73 | **归档按需分页**：`CmdResult` 带 `Kind`，展示端按类型还原 `Data`（`loadOlder` → `OlderPage`、`newSession` → `NewSessionData`）；uiapp 把页**插到最前面**、`More=false` 后不再请求、只接受请求过的页 | P3 | `host/{host,local,remote}.go`、`uiapp/app.go` | `TestLoadOlderResultStaysTypedOverWire`、`TestArchivePageAppliedInOrder` |
| 74 | **§10 P3 五条验收**：`TestTwoViewersSteerInOrder`、`TestStopFromViewerStopsAll`、`TestAskAnsweredElsewhere`（无待答提问时返回 `Notice`「已被其他端回答」）、`TestViewerPasteImageReachesExecutor`、`TestViewerOlderArchive`；另加忙时带附件被拒 | P3 | `host/remote_test.go`、`host/session.go`、`locale` | 全部 `-race` 通过 |
| 75 | **真实双进程验证**：`SIMPLE_AGENT_DATA=<temp>` 起 `--listen 127.0.0.1:47813`，另一个进程 `--connect`；两端均保持运行，监听端日志「执行端已监听…展示端可用 --connect … 连接」，展示端无错误（无单实例锁冲突、不写会话日志） | P3 | — | 手工验证 |
| 76 | **归档真链路补齐**（评审发现原验收是空数据）：`Session.recordEffect` 的压缩快照改走 `journal.Compact`（此前只当普通事件追加，**从不产生 archive 文件**）；`OlderPage.Next` + `LoadOlderPayload.Cursor` 让游标由展示端携带（多端各自翻页互不干扰）；`loadOlder` 按当前会话消息 id 去重（kept_from_seq 只能算最后事件序号，单靠它会重复显示） | P3 | `host/{session,local,payloads,host}.go`、`uiapp/app.go` | `TestViewerOlderArchive`（压缩两次→两页→两端与本地逐条一致→无重复）、`TestOlderArchiveSurvivesDisconnect`（翻页中断线→失败→重连从同一 cursor 继续） |
| 77 | **插话投递后的顺序验收**：`TestTwoViewersSteerInOrder` 补到"第二轮跑完后"——`users == [第一条, 插一句]`，且 A/B 两端 digest 与执行端深度相等 | P3 | `host/remote_test.go` | 同上 |
| 78 | 工作区 `config.go` 反复变 M 的原因：`gofmt` 认为该文件（工作区是 CRLF）需要重排，任何目录级 `gofmt -w internal/…` 都会把它重写成 LF（内容不变、mtime 变）。以后只对具体改过的文件跑 gofmt，不再对目录跑 | 流程 | — | `git status` 干净 |
| 79 | **旧格式会话文件迁移**：`store.migrateEmbeddedSnapshots`——加载时若 `compact.snapshot` 不在第 0 行，先整份备份 `session.pre-migrate-<ts>.jsonl`，再按快照切段写 `archive-NNNN.jsonl`、base 改写成「最后一个快照 + 之后事件」并补 `archived_as`/`kept_from_seq`；全程 tmp+rename，失败回滚新建归档、保留原文件；幂等（首行是快照就不再迁移） | P1（评审） | `store/{migrate,jsonl}.go`、`host/{local,session}.go` | `TestMigrateEmbeddedSnapshotOnce/Twice`、`TestCompactWritesArchiveFile`、`TestEmbeddedSnapshotMigrationKeepsState` |
| 80 | **真实双进程操作验证**（临时 Go 客户端 `/tmp`，两条真实 TCP 连接，用完删除；临时数据目录 + 真实网关）：展示端粘贴纯红 PNG → 执行端模型回答「红色」；压缩后 `loadOlder` 翻出压缩前 3 条（含图片提问与回答）；A 发送 + B 插话 → 两端队列都是 `[插一句：顺便说说天气。]`；B 按停止 → A/B/执行端都回 idle。提问抢答（后到者「已被其他端回答」）由自动化用例覆盖（真实模型不会按需调用提问工具） | P3 收尾 | — | 手工验证，输出见提交说明 |
| 81 | **迁移安全加固**（评审 P1/P2）：先修复再迁移（中间损坏先留档 `session.corrupt-*` 并产生警告，有警告时**跳过迁移**、下次打开再做，绝不静默丢弃损坏点之后的事件）；迁移前删掉 `archived_as` 之后的残留归档（上次迁移中断）；分段改为「第 k 段以上一个快照开头」（与真正 Compact 一致）；备份按 SHA-256 去重；测试辅助移出生产代码（`FirstEvent/ReadEvents/WriteEvents/Renumber`，不再 import `testing`） | P1 | `store/{jsonl,migrate}.go` | `TestMigrateEmbeddedSnapshotWithCorruption` |
| 82 | **P4 relay**：`internal/relay`（注册/列表/按 hostID 转发，**不解析会话协议**）+ `wire.Mux`（一条底层连接复用多逻辑通道，执行端每个展示端一条虚拟 `wire.Conn`）+ 客户端（`DialViewer`/`ListExecutors`/`RunExecutor` 退避重连）。relay 侧只保留一个读循环（两个读循环会抢帧）。测试：`TestRelayRoutesPackets`、`TestRelayRestartRecovers`、`TestMuxSeparatesChannels` | P4 | `relay/*`、`wire/mux.go` | 同上 |
| 83 | **P4 headless**：`--headless` 不建窗口，直接构造 `Local` + `listenHeadless`（可选 `--relay` 注册）；收到 SIGINT/SIGTERM 先逐会话 `stop`、`Sync`、关监听后退出；启动失败返回 1；`serialPost` 代替界面主线程；`relay` 子命令只转发。测试：`TestHeadlessLifecycle`、`TestParseRemoteOptions` | P4 | `cmd/simple-agent/{headless,remote,main}.go` | 同上 |
| 84 | **relay 连接性质**（评审 P1/P2）：`wire.MuxCloseKind` 双向关闭通道（relay→执行端发 mux.close，执行端关通道→relay 关该展示端）；relay 每个展示端**独立有界队列 + 写 goroutine**，队列满只断自己；`Mux.readLoop` 通道满则 `dropChannel`、先解锁再 accept；`NewTCPConn` 内置心跳（15s ping / 30s 静默断开）；`MaxExecutors`/同名注册回 `relay.registered{error}`，执行端打日志并退避；`--name` 指定 relay 名字，**同名拒绝后来的** | P4 | `wire/{mux,tcp}.go`、`relay/*`、`cmd/simple-agent/*` | `TestRelayViewerDisconnectReleasesExecutor`、`TestRelayStalledViewerDoesNotBlockOthers`、`TestHeartbeatDetectsSilentPeer` |
| 85 | **三进程真机验证（第二部分）**：relay + headless（`--name exec-p5`）+ 临时客户端；客户端发消息→收到回答→断开→重连：历史补齐（2 条）→再发一条成功（4 条），exit=0；headless 日志两次「展示端已断开（观察者剩余 0）」证明通道关闭传到执行端、观察者无泄漏 | P4 | — | 手工验证 |
| 86 | **P5 跳过（用户决定）**：v1 不做认证/加密；`Hello.Credentials{Kind:"none"}`、`Welcome.Error`、`Authenticator` 位置保留；`--help`、`--listen` 说明与启动日志（监听非回环时）明确提示"任何能连上的人都可以操作该执行端（执行命令、修改文件）" | P5 | `docs/remote-design.md`、`internal/config/config.go`、`cmd/simple-agent` | 手工核对说明文本 |

## 13. 已实现状态

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M1 | `message` / `conversation` / `render` | 已实现 |
| M2 | `providers`（OpenAI 兼容 + Anthropic + SSE + 工具配对 + 模拟） | 已实现 |
| M3 | `tools`（9 个工具 + 沙箱 + 进程树中止） | 已实现 |
| M4 | `driver` 状态机、`uiapp` 三栏界面、`config` + 入口 | 已实现 |
| M5 | `store`（JSONL/内存 + 滚动归档 + 数据目录锁）、`Replay`、启动恢复 | 已实现 |
| M6 | 上下文压缩（边界选择、摘要、快照、按需加载归档） | 已实现 |
| M7 | markdown（goldmark + chroma + LRU 缓存）、表格、图片、链接白名单、错误提示、staticcheck | 已实现 |

**构建与运行**：

```powershell
pwsh scripts/build.ps1          # 生成 bin\simple-agent.exe（版本号取 git describe）
pwsh scripts/build.ps1 -Version 1.2.3 -NoStrip
pwsh scripts/check.ps1          # vet + staticcheck + go test -race
```

`scripts/build.sh` 是 Linux/macOS 的等价脚本（`VERSION=1.2.3 scripts/build.sh`）。
构建用 `-trimpath`（不把本机路径写进二进制）与 `-s -w`（体积约减半，排查崩溃时用
`-NoStrip`）；版本号通过 `-ldflags -X main.version=…` 注入，`simple-agent --version`
可以查。

**与原设计的差异（实现时确认的）**：

1. **Enter 语义（G2）已解决**：不用改键位，也不用 fork。`Element.HandleInput` 在编辑器之前拿到按键，因此 `Enter` 发送、`Alt+Enter` 在光标处插入换行都能原生实现（`composer.go`）。
2. **Windows 内存实测**：原生 UI 窗口（1180×760）空闲 WorkingSet ≈ 62–66MB，二进制 25MB；没有引入 WebView2 的需求。
3. **`win.Update` 不存在**：这个 mygo 版本的正确写法是 `Window.Update(fn)`（= `postMain` + `contentChanged`）。
4. **`filepath.EvalSymlinks` 不解析 Windows junction**：沙箱改用 `GetFinalPathNameByHandleW`（`tools/canonical_windows.go`）。
5. **`message.status` 只为兼容历史保留**：新写入的日志用 `message.appended`（流开始）+ `message.finalized`（完整内容）。
6. **`notice.appended` 是新增事件**：通知也显示在会话里，不写日志会让「重放 == 在线状态」不成立。
7. **自动压缩策略放在 driver 里**：UI 不再自己判断阈值（否则失败会无限重试、Stop 后会自己压起来）。
8. **`Fill()` 会把宽度设成父容器的 100%**：侧栏必须 `Width + FillHeight`，右栏用 `Grow(1)`，有 `TestLayoutSidebarAndMainColumn` 守着。
