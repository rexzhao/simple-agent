# 界面优化方案（按 deepseek harness 复刻）

状态：**P0.0 – P4 全部已实现**；状态条的轮数/步数/上下文占比已在 P4 之后落地，tok/s 与 cache hit 仍按 §6 留档
参照：用户要求"参照 dsh 界面设计，**尽量复刻**，必要时参考源码"。dsh 源码在 `F:/work/deepseek-harness`（**MIT**）。
现状基线：HEAD `7cbe7c0`，`internal/uiapp/app.go` 2733 行 + `markdown.go` / `images.go` / `attachments.go` / `copy.go` / `composer.go` / `process_activity.go`

**这次修订的要点**（来自 `v7z3at1tz8`）：

- 新增用户决定：**浅色色板复刻 dsh**（推翻决策 #15）、**侧栏尺寸/收起按 dsh**、**不打包字体**、
  **界面始终浅色**（不跟随系统深色）；记为 #37/#38/#39/#40。
- 新增「设计令牌」一步（P0 的第 0 步）：色值、圆角、间距、字号、字体栈全部从 dsh 的 CSS 抄出来，落到 `theme_dsh.go` + `layout.go`，**后面每一期不得再自定数值**。
- 文案改用 dsh 的 zh locale（例：回合结束行按其 `message.turnProcess.took` 用「已完成，用时 」）。
- 图标直接用 dsh 的 SVG，并在 `THIRD_PARTY_NOTICES.md` 注明 MIT 版权。
- 更正三处事实错误：侧栏数值、`ui.Split` 没有最大值（范围要自己 clamp）、选文件是 `mygo.Dialog.Open` 且**会阻塞**（必须在 goroutine 里调）。

## 已确认的七个方向（本方案按此执行）

| # | 决定 | 来源 |
|---|---|---|
| 1 | **用户消息保持右侧浅底圆角气泡**（截图确认），气泡下方右对齐「时间 + 复制图标」。**不做**「Execution requested」那种整宽输入卡片——那是 dsh 插件产物 | 用户 |
| 2 | **侧栏不做日期分组**，做成「项目可折叠」的两级树（项目 → 会话），会话行右侧显示相对时间 | 用户 |
| 3 | 底部状态条：轮数/步数/上下文占比**已实现**（§6.1）；tok/s、cache hit、环形仍不做 | 用户 |
| 4 | **Chat / Trajectory 页签完全不做**（连占位也不留） | 用户 |
| 5 | **项目来源走路径 A**：`session.created` 加可选 `workspace` 字段，重放/catalog 带出，旧日志归「默认」组，不写迁移脚本 | 用户（三选一选 A） |
| 6 | **模型选择改用原生 `ui.Menu`**（替掉自绘的两级面板） | 用户 |
| 7 | **侧栏删除改右键菜单 + 二次确认** | 用户 |
| 8 | **配色复刻 dsh 的浅色色板**（品牌蓝 `rgb(65,118,230)`、中性偏蓝灰阶、border-l1..l4、交互 hover/active、menu、turn-trigger、代码块等）。**推翻决策 #15** | 用户 |
| 9 | **侧栏尺寸与收起按 dsh**：默认 280，可拖 264–420，**收起后留 56px 图标栏**（不是完全隐藏）；窗口 < 1024 自动收起成图标栏；正文区最小 400 | 用户 |
| 10 | **不打包 Montserrat 字体**：品牌名用系统字体；正文与代码字体按 dsh 的字体栈 | 用户 |

### 已完成的可行性探针（结论：原生菜单与对话框都可用）

原本 P1 / P3 的不确定性是「原生菜单与 `AlertDialog` 在 `ui.Tester` 里能否被驱动」。已写临时探针实测，**四项全部通过**（探针已删除，结论如下）：

| 探针 | 结果 |
|---|---|
| `Element.Menu`（按钮弹菜单）→ `tester.Click` + `tester.ChooseMenuItem("glm-5.3")` | 通过，`Chosen()` 正常回调 |
| `Element.ContextMenu`（右键菜单）→ `tester.RightClick` + `ChooseMenuItem("改名")` | 通过 |
| `ui.MenuButton`（按钮自带菜单）→ `Click` + `ChooseMenuItem("压缩上下文")` | 通过 |
| `ui.AlertDialog`（二次确认）→ `Click("取消")` 返回 1 / `Click("删除")` 返回 0 | 通过 |

`ui.Tester` 提供 `Menu() []string`（列出菜单项标签）、`ChooseMenuItem(path ...string)`（支持子菜单路径）、`CloseMenu()`。
`MenuItem` 另有 `Disabled(bool)` / `Checked(bool)` / `Shortcut(mods,key)` —— **`Checked` 可用于在模型菜单里标出当前模型**。

**`AlertDialog` 的一个坑（探针实测）**：可见文本里**标题会出现两次**（`panel.Label(title)` 给无障碍，`Text(c, title)` 给可见），实测 `Texts()` 为
`[打开 背景文字 删除会话 删除会话 不可恢复 删除 取消]`。因此测试**不能**用 `HasText("删除会话")` 断言唯一性；要点按钮就直接 `Click("删除")` / `Click("取消")`（按钮文案唯一）。

因此 P3 / P1 都不需要「探针失败就退回自绘面板」的备用分支，方案按原生菜单与 `AlertDialog` 直接执行。

---

## 0.5 设计令牌（P0 的第 0 步，先做）

> 规则：**后续每一期只能用这里的令牌，不许再自己定数值**。色值来自
> `packages/client/ui-theme/src/styles/design-platform.css`（浅色段，第 167–280 行），
> 字体来自 `packages/client/ui-theme/src/styles/base.css`，尺寸来自
> `packages/client/ui-layout/src/client/columns.ts`。

### 0.5.1 色板（浅色，`uiapp/theme_dsh.go`）

dsh 是 `静态色阶 → 语义别名 → 组件变量` 三层。我们只抄到**语义别名**这一层，
在 Go 里就是一个 `ui.Theme` 结构体的字段（命名沿用别名，便于对照 dsh）。

| 用途（dsh 别名） | 浅色值 | 对应 dsh 变量 |
|---|---|---|
| 背景（bg-base / layer-1..3） | `#FFFFFF` | `--dsw-alias-bg-base` = `static-neutral-bluish-00` |
| 侧栏底（sidebar-fill） | `#F9FAFB` | `--dsw-specific-sidebar-fill` = `bluish-50` |
| 平台底（module-platform） | `#F5F6F7` | `--dsw-alias-bg-module-platform` = `bluish-60` |
| 正文主色（label-primary） | `#0F1115` | `--dsw-alias-label-primary` = `bluish-1000` |
| 次要文字（label-secondary） | `#61666B` | `bluish-700` |
| 三级文字（label-tertiary） | `#81858C` | `bluish-600` |
| 说明文字（label-caption） | `#ADB2B8` | `bluish-400` |
| 品牌蓝 / 链接（brand / link） | `rgb(65,118,230)` | `--dsw-alias-brand-primary-new-color`、`--dsw-alias-link` = `deepseek-500` |
| 用户气泡底 | `#EDF3FE` | `--dsw-specific-bubble` = `deepseek-50` |
| 用户气泡高亮 | `#D3E2FF` | `--dsw-specific-bubble-highlight` = `deepseek-200` |
| 边框 l1 / l2 / l3 / l4 | `rgba(0,0,0,.04)` / `.10` / `.12` / `.16` | `--dsw-alias-border-l1..l4` |
| 交互 hover / active | `rgba(38,49,72,.06)` / `rgba(38,49,72,.10)` | `--dsw-alias-interactive-bg-hover/-active` |
| 交互 hover（实色） | `#F1F3F5` | `--dsw-alias-interactive-bg-hover-solid` = `bluish-75` |
| 侧栏条目 hover / active | `#F1F3F5` / `#EBEEF2`（active 强调 `#E4EDFD`） | `sidebar-nav-item-hover/-active/-active-accent` |
| 菜单面（menu） | `rgba(248,249,250,.58)` | `--dsw-specific-menu` |
| 菜单分组头 | `rgba(248,249,250,.94)` | `--dsw-alias-menu-group-header-fill` |
| 代码块底 / banner | `#F9FAFB` | `--dsw-alias-markdown-code-block(-banner)` = `bluish-50` |
| 行内代码底 | `#FAFAFA` | `--dsw-alias-markdown-inline-code` = `neutral-50` |
| 回合触发底（turn-trigger） | 同代码块底 | `--dsw-alias-turn-trigger-bg` |
| Tooltip 底 / 字 | `#2C2C2E` / `#FFFFFF` | `tooltip-bg` = `bluish-850` |
| Toast 底 / 字 | `#353638` / `#FFFFFF` | `toast-bg` = `bluish-800` |
| 错误 / 成功 / 警告 | `#EC1313` / `#22C55E` / `#F59E0B`（警告文字 `#DD8629`） | `state-error-primary` / `state-success-primary` / `state-warn-primary(-label)` |
| 滚动条 | `#E5E5E5`（hover `#D4D4D4`） | `scrollbar-bg-l1` / `scrollbar-hover-l1` |

注意：dsh 的浅色品牌色**有两套**——`--dsw-alias-brand-primary` 实际是近黑
（`bluish-1000`），蓝色那套叫 `brand-primary-new-color`（`rgb(65,118,230)`）。
用户要的是**蓝色**，因此 `Accent` 取 `rgb(65,118,230)`，`Button primary fill` 仍按 dsh
（近黑）—— 两者都记在 `theme_dsh.go` 里，用哪个由组件决定。

### 0.5.2 字体

| 用途 | 栈（按 dsh `base.css`） |
|---|---|
| 正文 | `-apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', 'Helvetica Neue', Helvetica, Arial, sans-serif` |
| 代码 | `'SF Mono', 'JetBrains Mono', 'Fira Code', Consolas, 'Liberation Mono', Menlo, Courier, 'PingFang SC', 'Microsoft YaHei'` |
| 品牌名 | dsh 用 `'Montserrat'` + 正文栈；**用户决定不打包字体** → 直接用正文栈（Windows 上就是 Segoe UI） |

mygo 的字体 API 是 `ui.FontFamily(names ...string)`（`Element.Font(...)` 也接受一族名字），
所以这些栈要落成 `layout.go` 里的两个 `[]string` 常量，而不是单个字符串。

### 0.5.3 尺寸 / 圆角 / 间距（`uiapp/layout.go`）

| 令牌 | 值 | 来源 |
|---|---|---|
| 侧栏默认 / 最小 / 最大 | 280 / 264 / 420 | `columns.ts` `SIDEBAR_DEFAULT/MIN/MAX` |
| 侧栏收起后的图标栏 | 56 | `SIDEBAR_COLLAPSED` |
| 侧栏自动收起阈值 | 1024 | `SIDEBAR_AUTO_COLLAPSE` |
| 正文区最小宽度 | 400 | `CENTER_MIN` |
| 正文列最大宽度 | 748（现状保留） | 现有 `chatColumnWidth` |
| 头部高 | 48 | 截图量取 |
| 输入卡圆角 | 16 | 截图量取（现状 14） |
| 消息圆角 / 用户气泡圆角 | 16（气泡 16，与 dsh `--dsw-radius-*` 取整） | `design-platform.css` 的圆角变量 |
| 间距刻度 | 4 / 6 / 8 / 10 / 12 / 16 / 20 | 与现有 `Padding(…)` 保持一致，写成常量 |

### 0.5.4 文案（直接抄 dsh 的 zh locale）

| 我们的位置 | 用 dsh 的 key / 值 | 来源文件 |
|---|---|---|
| 回合结束行 | `message.turnProcess.took` = **「已完成，用时 」** + 时长 | `ui-chat/src/client/locale.ts:70` |
| 回合失败 | `message.turnProcess.failed` = **「处理失败」** | 同上 |
| 回合完成（无耗时） | `message.turnProcess.worked` = **「已完成」** | 同上 |
| 新建会话 | `session.new.label` = **「新建会话」** | `ui-sidebar/src/client/locales.ts:6` |
| 收起/打开侧栏 | `toggle.collapse` = **「收起侧边栏」**、`toggle.open` = **「打开侧边栏」** | 同上 |
| 过程组标题 | `message.stepProcess.*` | `ui-chat/src/client/locale.ts:6–45`（已在 `process_activity.go` 里用过） |

### 0.5.5 图标与第三方声明

- 图标一律从 dsh 的 `packages/client/ui-primitives`（SVG 组件）搬到 `uiapp/icons.go`，
  用 `ui.MustParseSVG` 解析成 `ui.Icon`，颜色走 `TextColor`。
- 新建 `THIRD_PARTY_NOTICES.md`，写清：图标与部分文案来自 deepseek-harness，
  Copyright DeepSeek，**MIT License**，附许可证正文与仓库地址。
- 代码块/菜单等视觉不复制 CSS（我们不用 CSS），只抄**色值、圆角、间距**这些数值。

---

## 0. 结论摘要

界面与截图的**信息架构本来就接近**（左栏会话 + 右栏消息流 + 底部输入卡）。按上面的确认，真正要做的只有四件事：

| 项 | 性质 | 是否动协议层 |
|---|---|---|
| **A. 侧栏做成项目可折叠的树 + 修「会话多了画到窗口外」** | 纯 UI（`ui.Outline` 现成）+ `session.created` 加一个可选 `workspace` 字段 | 只加可选字段，向后兼容 |
| **B. 消息流补回合元信息**（回合结束行「Completed in 22s」、气泡下方时间戳、过程组图标化） | 纯 UI + 回合耗时（1 个 driver 字段） | 否 |
| **C. 头部图标按钮组 + 输入卡（`+` 附件、模型原生菜单、圆形发送）** | 纯 UI，**删掉约 130 行**自绘面板代码 | 否 |
| **D. 底部状态条预留位** | 只放已有的「上下文 x / y」 | 否 |

**最大成本依然是测试**，不是画界面：`internal/uiapp/*_test.go` 有 **60+ 处按可见文本定位**（`tester.Click("发送")` 35 次、`"新建"` 7 次、`"压缩上下文"` 4 次…），`ui.Tester.Find` 是精确匹配。§8 给了迁移纪律与写法示例。

---

## 1. 截图逐块拆解 vs 现状

### 1.1 侧栏（280px）

| 截图元素 | 现状 | 差距 |
|---|---|---|
| 顶部：图标 + 名称 + 胶囊徽章 | `Text("simple-agent").Bold()` | 无图标、无徽章 |
| 「New Session」整宽描边按钮（⊕ 图标） | 同排右侧的小文本按钮 | 需要 `ui.Button` + 图标，独占一行 |
| 「Workspaces」小标题 + 右侧图标按钮（搜索/新建） | 无 | 需要小标题行 |
| **两级树：项目（可折叠）→ 会话** | **平铺列表，且没有滚动容器** | 会话超约 20 个会画到窗口外（现存缺陷）；需要 `Outline` + `Scroll` |
| 会话行右侧相对时间（`5d` / `4d` / `9d`） | 无 | `UpdatedAt` 已有，直接格式化 |
| 选中整行浅底 + 圆角 | `SurfaceHover` + `Radius(8)` | 已接近 |
| 底部用户区：头像 + 名字 + 「Update」 | 无 | `ui.Avatar` 现成 |
| Plugins / Automation tasks 导航项 | 无 | **不照搬**（无对应功能，§9） |

**唯一的结构性缺陷**：`View()` 里左栏直接 `for range a.sessions` 平铺，没有 `ui.Scroll`。`ui.List` 才虚拟化，侧栏没有。

### 1.2 右栏顶部

| 截图元素 | 现状 | 差距 |
|---|---|---|
| 会话名 + 「Standard mode」小标签 | 标题 + 模型 chip + 档位 chip + 忙状态 | 「Standard mode」**不做**（无对应语义） |
| 右上图标按钮组（视图/历史/更多/侧栏开关） | 无 | 「侧栏开关」值得做；「更多」放压缩上下文等 |
| **Chat / Trajectory 页签** | 无 | **完全不做**（用户确认 #4） |

### 1.3 消息流（以截图 #1 为准）

截图 #1 的实际结构：

```
[助手正文，整宽，无底色]              ← markdown，粗体段落 + 列表
                                      ┌──────────────────────────┐
                        [用户气泡] →  │ 就是说很多供应商的信息/… │  ← 右侧，浅紫底，圆角
                                      └──────────────────────────┘
                                          10/23 17:15  ⧉        ← 气泡下方右对齐：时间 + 复制
Completed in 22s  ⌄                   ← 回合结束行（可折叠）
──────────────────────────────────
[助手正文，整宽]
```

| 截图元素 | 现状 | 差距 |
|---|---|---|
| 用户右侧浅底气泡 | **已有**（`AlignSelf(End)` + `Selection` 底色 + `Radius(16)` + max-width 620） | 无 |
| 气泡下方右对齐复制图标 | **已有**（`messageCopyButton`，贴在气泡下方右对齐） | 无 |
| **气泡下方时间戳**（`10/23 17:15`） | 无 | 需要把 `ChatEntry.CreatedAt` 显示出来 |
| **`Completed in 22s ⌄` 回合结束行** | 无 | 需要回合耗时 + 一个可折叠状态行 |
| 助手正文整宽 | **已有** | 无 |
| 「Execution requested」整宽输入卡片 | 无 | **不做**（插件产物，用户确认 #1） |
| 「Automation task / Stopped / Analysis completed」卡片 | 无（Notice 是一行灰字） | **本期不做**（同为插件产物）；列为以后可选项 §9 |
| 回合顶部元信息行（赞/踩/分享/Usage/时间） | 无 | 赞踩分享**不做**（无后端）；只取「时间 + Usage」 |
| 「Called tools」小标题 + 分隔线 | 过程组标题「已读取文件」+ ▸ | 语义相同，补图标与分隔线即可 |
| markdown（粗体/列表） | 已有 `goldmark` + `chroma` | 基本对齐 |
| **行内代码 chip** | 加粗等宽、无底色 | **待改**（P2）：dsh 是正常字重 + 浅底 `#FAFAFA` + 圆角的小 chip |

### 1.4 输入卡与底部状态条

| 截图元素 | 现状 | 差距 |
|---|---|---|
| 大圆角面板 + 多行 placeholder（`/ commands, @ files`） | 圆角 14 面板 + placeholder | 接近，placeholder 可补命令提示 |
| 左下 `+` 附件按钮 | 无（只有粘贴/拖入） | 需要 `+` 按钮 → 文件对话框 |
| 「Full access」权限下拉 | 无 | 对应 `allow_shell`（启动期全局开关）；**本期只做展示** |
| 右下模型 chip（`glm-5.3-flash High ⌄`） | 在顶部 header | 搬下来即可 |
| 圆形蓝色发送按钮（↑） | `ui.PrimaryButton("发送")` | 需要圆形图标按钮 |
| 底部状态条（轮数/步数/tok-s/cache hit/环形） | 只有「上下文 12k / 64k」 | **已实现**轮数/步数/上下文占比（§6.1）；tok-s/cache hit/环形留档 §6.2 |

---

## 2. mygo 能力盘点（已核对 v0.2.18 源码）

### 2.1 可直接用

| 需求 | mygo 现成件 | 说明 |
|---|---|---|
| **两级可折叠树（项目 → 会话）** | `ui.Outline[K]` + `OutlineState[K]` | 自带展开/收起（左/右键、Opt 全展开）、按需构建行、`List.Key` 保持位置 |
| 侧栏视觉（图标 + 徽章 + 选中态） | `ui.SidebarItem` / `SidebarSection` / `Badge` | 若用 `Sidebar` 会自带键盘导航；与 `Outline` 二选一 |
| 侧栏可拖拽宽度 | `ui.Split(c, &width, first, second)` | 拖拽 + 方向键 + 40 DIP 边距。**注意：`Split` 只有最小值（`minPane = Space(10)`），没有最大值**，264–420 必须在拖动回调里自己 clamp（见 §3 备注） |
| 搜索框 | `ui.SearchField(c, &query)` | 带放大镜 |
| 折叠行 | `ui.Collapsible` / `CollapsibleBase` | 带三角与动画（回合结束行可用） |
| 右键菜单 | `Element.ContextMenu` / `Element.Menu` / `MenuButton` | **原生菜单**，用于会话行的改名/复制标题/删除 |
| 气泡提示 | `Element.Tooltip(s)` | 图标按钮必备 |
| 轻提示 | `c.Toast` / `c.ToastAction` | 「已复制」「已删除」比自绘更好 |
| 头像 | `ui.Avatar(c, name, bitmap)` | 姓名首字 + 哈希配色 |
| 上下文用量条 | `ui.Meter(c, used, 0, window, &levels)` | 有 Warning/Critical 分级 |
| 转圈 | `ui.Spinner(c)` | 「正在生成」 |
| SVG 图标 | `ui.MustParseSVG` + `ui.Icon` + `.TextColor()` | 支持 path/rect/circle/ellipse/line/polyline/polygon + 全部 stroke 属性，够画 Lucide/Feather |
| 自定义标题栏 | `c.TitleBar()` + `.DragWindow()` + `WindowOptions.TitleBarStyle=hidden` | 扁平标题栏（可选，见 §5 P0 备注） |
| 应用菜单栏 | `mygo.App.SetMenu` + `MenuItem{Label,Accelerator,Submenu}` | 截图顶部 Application/Edit；也可不加 |

### 2.2 没有、需要自绘或绕开

| 需求 | 结论 |
|---|---|
| 聊天气泡 | 自绘（现状已如此，保持） |
| 环形进度 | `Meter` 是条形；环形要 `Draw` 自绘弧。**不做**（条形够用，见 §6.2） |
| 悬停才出现的操作条 | `Element.Attach` + `Hovered()`；需固定行高避免抖动 |
| 代码块跨行拖选 | 已知限制（决策 #34），本次不动 |

---

## 3. 目标布局

```
┌─ 主栏（Row）──────────────────────────────────────────────────────────────┐
│ ┌ 侧栏 280（Split 可拖 264–420）─┬─ 1px ─┬─ 主列（Grow 1，≥400）────────┐ │
│ │ [◉] simple-agent              │       │ header h=48                    │ │
│ │ ┌───────────────────────────┐ │       │  QA                    [▤][⋯]  │ │
│ │ │  ⊕  新建会话               │ │       │ ────────────────────────────── │ │
│ │ └───────────────────────────┘ │       │                                │ │
│ │                               │       │  ┌ List（FollowEnd）─────────┐ │ │
│ │  项目                [🔍]     │       │  │ [助手正文，整宽]           │ │ │
│ │  ▾ simple-agent          5d   │       │  │                            │ │ │
│ │     分析当前项目         9d   │       │  │        ┌────────────────┐  │ │ │
│ │     插话功能设计         3d   │       │  │        │ 用户气泡（浅底）│  │ │ │
│ │  ▸ layer-fs              4d   │       │  │        └────────────────┘  │ │ │
│ │  ▸ pi-mailbox            6d   │       │  │        10/23 17:15  ⧉      │ │ │
│ │                               │       │  │ 已完成 · 22s  ⌄            │ │ │
│ │  （Scroll）                    │       │  │ ────── 调用了工具 ──────   │ │ │
│ │                               │       │  │  ⚙ read  docs/plan.md      │ │ │
│ │ [rex] rex                     │       │  │                            │ │ │
│ │                               │       │  └────────────────────────────┘ │ │
│ │                               │       │  ┌ 输入卡 ───────────────────┐ │ │
│ │                               │       │  │ 消息，或 / 命令、@ 文件…   │ │ │
│ │                               │       │  │ ＋ 完全访问⌄  glm-5.3 High⌄ (↑)│ │
│ │                               │       │  └────────────────────────────┘ │ │
│ │                               │       │  上下文 12k / 64k  ← 预留状态条 │ │
│ └───────────────────────────────┴───────┴────────────────────────────────┘ │
└───────────────────────────────────────────────────────────────────────────┘
```

关键度量（与现状对比）：

| 项 | 现状 | 目标（dsh 数值，`columns.ts`） |
|---|---|---|
| 侧栏宽 | 240 固定 | **默认 280**，`Split` 可拖 **264–420**（回调里 clamp），**收起后 56px 图标栏** |
| 侧栏自动收起 | 无 | 窗口宽 **< 1024** 自动收起成 56px 图标栏（手动开关可再展开） |
| 正文区最小宽度 | 无约束 | **400**（`CENTER_MIN`） |
| 正文列最大宽 | 748 | **不变**（已对齐） |
| 消息左右留白 | 16 | **不变** |
| 用户气泡 | 右侧，`Selection` 底，R14，max 620 | **不变**，只加下方时间戳 |
| 头部高 | ~40（随内容） | 48 固定，图标按钮右对齐 |
| 输入卡圆角 | 14 | 16（令牌 `radiusComposer`） |
| 状态条 | 一行文字 | 一行文字 + 右侧预留空白（§6） |
| 颜色 / 字体 | mygo 内置主题 | **dsh 浅色令牌**（§0.5）+ dsh 字体栈 |

### 3.1 侧栏的收起形态（dsh 的三段式）

dsh 的侧栏是**三态**，不是"开/关"：

| 状态 | 宽度 | 触发 |
|---|---|---|
| 展开 | 用户拖出来的宽度（默认 280，范围 264–420） | 默认；或用户手动展开 |
| 收起（图标栏） | **56** | 用户点「收起侧边栏」；或窗口 < 1024 自动收起 |
| （无） | — | dsh 在 macOS 桌面还有"完全隐藏"，**我们不做**（截图里是图标栏形态） |

实现要点：

- 宽度状态用 `App.sidebarWidth int`（0 表示"收起成图标栏"），`ui.Split` 绑它；
- **clamp 在拖动回调里做**：`Split` 没有最大值参数，`&width` 变小时也只能拦在回调里
  （`width = clampInt(width, 264, 420)`）；
- 自动收起：`w, _ := c.Size()`（mygo 的窗口尺寸）**< 1024** 时按 56 渲染，但**不覆盖**
  用户手动设的宽度，窗口变宽后自动恢复；
- 56px 图标栏只放「新建会话」与「展开侧栏」两个图标按钮（+ 会话头像），不渲染会话列表。

---

## 4. 侧栏：项目从哪来（本期唯一需要动数据的地方）

截图是 `Workspaces → 项目（可折叠）→ 会话`。本项目**没有工作区概念**：`Options.Workspace` 是启动参数，`ToolContext` 每会话新建但都用同一个目录，`session.created` 事件里不记 workspace。

### 路径 A（**已确认采用**）：`session.created` 加可选 `workspace` 字段

```go
// driver/record.go
type SessionCreatedData struct {
    ContextID string    `json:"context_id"`
    Title     string    `json:"title,omitempty"`
    CreatedAt time.Time `json:"created_at"`
    Workspace string    `json:"workspace,omitempty"`   // 新增，可选
}
```

- 新建会话时写入 `toolCtx.Workspace()`（`tools.ToolContext` 已有 `Workspace()`），显示名取目录名（`filepath.Base`）。
- 重放的 `Replayed` 增加 `Workspace string`；`Session` 保存它。
- **向后兼容**：旧日志没有该字段 → 空 → 归到「默认」项目节点（**不是"未分组"这种额外概念，就叫「默认」**）。不需要迁移脚本。
- 侧栏按 workspace 分组，组名取目录名（`simple-agent`、`layer-fs`…），`Outline` 提供展开/收起。
- 同一 workspace 的会话归一组；**组内按 `UpdatedAt` 倒序**（现在是平铺倒序，规则不变，只是多了层分组）。
- 附带好处：将来做「新建会话时选目录」只需在 `NewSession` 加参数，事件格式已经就绪。

**代价**：`SessionCreatedData` 是重放与 catalog 都读的类型，加字段要同步 `store.SessionCreatedData`（`store/catalog.go` 里有同构副本）与 `firstSessionCreated`；`Catalog.Summary` 也要带出来供侧栏分组。

### 路径 B（已排除）：只有「当前项目」一个节点

所有会话同属一个项目节点，折叠功能形同虚设；做出来的东西和截图不一样，而成本几乎一样（树的代码都要写）。

**采用 A**（用户已确认）。这一项放在 P1 的第一个任务，先改数据、再改侧栏。

---

## 5. 分期方案

### P0 · 设计令牌 + 修缺陷 + 骨架

**P0.0 设计令牌（先做，视觉一次到位）**

1. 新建 `internal/uiapp/theme_dsh.go`：按 §0.5.1 构造一个 `ui.Theme`（字段名对齐 dsh 别名），
   在 `App.View` 里应用。**深色模式已确认：始终用 dsh 浅色，不跟随系统**（决策 #40），
   所以 `themeFor` 无条件返回浅色令牌；以后要支持深色，只改这一处。
2. 新建 `internal/uiapp/layout.go`：把 §0.5.3 的尺寸/圆角/间距写成常量，
   顺便把现有的 `chatColumnWidth` / `chatSidePadding` 也搬进去（单一来源）。
3. 字体栈常量（§0.5.2）：正文 `FontFamily(textFontStack...)`，代码块与行内代码用 `codeFontStack`。
4. ~~新建 `internal/uiapp/icons.go`~~ → **挪到 P0.1**：图标要"落地即被使用"才不会被
   `staticcheck` 报 U1000，而 P0.0 还没有用到图标的位置（侧栏图标栏是 P0.1 的事）。
5. 新建 `THIRD_PARTY_NOTICES.md`（dsh MIT，含版权与许可证正文）。
6. 文案：把 §0.5.4 里这几条改成 dsh 的 zh 文案（先只改**新增**的：回合结束行；已有文案
   等各自那期再改，避免 P0 就动到测试）。

**P0.1 修缺陷 + 骨架**

1. 侧栏套 `ui.Scroll`（修「会话多了画到窗口外」）。
2. 左栏改 `ui.Split`（默认 **280**，可拖 **264–420**，回调里 clamp）+ 收起成 **56px 图标栏**，
   窗口 **< 1024** 自动收起（§3.1）。
3. 抽 `internal/uiapp/layout.go`：度量常量集中（见 §0.5.3）。
4. 抽 `internal/uiapp/turnview.go`：`buildChatItems` / `turnItems` / `splitTurn` 移出 `app.go`（现有 2733 行已难导航）。
5. **测试定位器抽离**（本方案最重要的一步，见 §8）。

**验收**：`go test -race ./...` 全绿；新增 `TestSidebarScrollsWithManySessions`（造 60 会话，
断言最后一行在窗口内）、`TestSplitResizesSidebar`、`TestSplitClampsWidth`（拖到 100 → 264；
拖到 800 → 420）、`TestSidebarAutoCollapses`（窗口 900 → 图标栏位于 56 宽）、
`TestThemeTokensAreDSH`（抽查背景 `#FFFFFF`、品牌蓝、border-l2 三个值）。

**风险**：`Split` 分隔条占宽度，`TestLayoutSidebarAndMainColumn`（断言 220）要同步改常量。

---

### P1 · 侧栏重做

> dsh 对照：`ui-sidebar/src/client/SidebarRoot.tsx`（结构）、`SidebarRoot.module.css`
> （间距/配色，值见 §0.5）、`ui-sidebar/src/client/locales.ts`（文案）。
> 我们的侧栏**不复刻** dsh 的插件槽位（`sidebar.workspaces` 等 registrant）。

1. **数据**：`SessionCreatedData` 加 `workspace`（§4 路径 A）；`Replayed.Workspace` + `Session.workspace`；`Catalog.Summary.Workspace`。
2. 顶部品牌行（`MustParseSVG` 内联小 logo + 名称）。
3. 「新建会话」改整宽描边按钮（`ui.Button` + ⊕）。
4. 「项目」小标题 + `SearchField` 折叠按钮（点开才展开，避免常驻占高度）。
5. 会话树用 `ui.Outline[string]`：根是 workspace，叶子是会话；叶子渲染「标题 + 右对齐相对时间」。
6. 会话行的操作改成**右键菜单**（`Element.ContextMenu`）：改名 / 复制标题 / 删除；同时悬停浮出 `⋯` 按钮（`Element.Menu`）供不习惯右键的用户。
   - **删除必须二次确认**：选「删除」后弹 `ui.AlertDialog(c, &open, "删除会话", "「标题」及其历史将不可恢复。", "删除", "取消")`，返回 0 = 删除、1 = 取消。
   - 二次确认**不能**沿用现在的 `session.confirmDelete` 行内两按钮（那是为常驻按钮设计的；改成菜单后行内不该再挤两个按钮）。
7. 底部用户区：`ui.Avatar` + 名字（`os.Getenv("USERNAME")`，取不到用 "本机"）。
8. ~~图标栏改用 dsh 的 SVG 图标~~（**已完成**，见 §12 第 44 条）：新建会话与侧栏开关
   都是 dsh 的 16px SVG（`uiapp/icons.go`），悬停有 6px 圆角底色；图标按钮只设无障碍名，
   文字文案仍待语言方案。
9. 文案先用常量占位：语言方案（全英文/双语）待用户确认，届时统一替换（定位器已就绪）。
10. **搜索框占位文字是 mygo 的英文默认值 "Search"**（`SearchField` 内部设的，外面的
    `Placeholder()` 覆盖不到内层输入框）：等语言方案定了与其它文案一起处理，别漏。
11. **相对时间「刚刚」的叠字**：mygo 在 Windows 上把相邻相同 CJK 字叠画（§12 第 43 条），
    我们自己的标签用 hair space 规避；用户正文里的重复 CJK 属上游缺陷。

**验收**：`TestSidebarGroupsByWorkspace`、`TestSidebarCollapseProject`、`TestSidebarRelativeTime`、`TestSidebarSearchFilters`、`TestSidebarRenameViaContextMenu`、`TestSidebarDeleteNeedsConfirm`（右键→删除→断言弹确认→点「取消」会话仍在；再来一次点「删除」会话消失）。

**风险**：
- `Outline` 的 `List.Selected` 与现有 `a.active int` 需要桥接；`Outline` 叶子行内要放右对齐时间，用 `Children` 追加 `Grow(1)` 占位即可。
- 若 `Outline` 的键盘导航与输入框抢焦点，退回手写 `Row` + 自己的展开状态（`Outline` 的价值是虚拟化，会话量不大时可放弃）。
- **`ui.AlertDialog` 已验证可被 `Tester` 驱动**（弹窗 → `Click("取消")` 返回 1、`Click("删除")` 返回 0）。注意标题可见文本会出现两次，测试只按按钮文案定位。
- 原有 6 处测试依赖「改名/删除」文本按钮（§8.3），改菜单后要重写为 `RightClick` + `ChooseMenuItem`。

---

### P2 · 消息流

1. **回合结束行**（截图 #1 的 `Completed in 22s ⌄`）：`ui.Collapsible`。文案**用 dsh 的**：
   `已完成，用时 22s`（`message.turnProcess.took`）、失败时 `处理失败`（`message.turnProcess.failed`）、
   中止时沿用现有「已中止」。展开显示失败原因与 `failureHint`（现有 `itemMarker` 的升级）。
   对照源文件：`ui-chat/src/client/chat/TurnFooter*`（或同目录的 turn 组件）。
   - 需要 `driver` 记录回合耗时：`Turn.startedAt`（`startTurn` 时打点）→ `finishStream`/`failStream` 时算差值，存到 `ChatEntry` 或会话级 map。
2. **用户气泡下方时间戳**：`ChatEntry.CreatedAt` 格式化为 `01/02 15:04`，与复制图标同一行右对齐。
3. **回合元信息行**（轻量）：每条 user 消息上方不额外加行；只在**助手回合结束后**显示「时间 · Usage N tok」。赞/踩/分享不做。
4. **行内代码 chip**（评审补充）：现在的行内代码是**加粗等宽**、没有底色；dsh 是
   正常字重的小 chip（浅底 + 圆角 + 细内边距）。令牌用 `dsh.InlineCode`（`#FAFAFA`），
   具体 CSS 见 `ui-renderer` / `ui-chat` 里 markdown 的 inline code 样式。
5. **过程组图标化**：标题前加活动图标（`process_activity.go` 已有 13 类 `processActivity`，与 SVG 一一对应），标题与内容之间加一条分隔线（对齐「Called tools」的视觉）。
6. **Notice**：保持一行灰字，只加左侧图标（信息/警告/错误三色）。**卡化不做**（插件产物）。

**验收**：`TestTurnFooterShowsDuration`、`TestUserBubbleShowsTimestamp`、`TestProcessGroupHasIcon`；`TestAppSendFlow` / `TestAppStopFlow` 按 §8 迁移后仍绿。

**风险**：`turnItems` 的条目种类从 4 种扩到 5 种（多一个「回合结束行」），`chatItem.key` 必须同步扩展——`ListState.Key` 冲突会让列表错乱（决策 #13 踩过）。

---

### P3 · 头部 + 输入卡

1. 头部固定 48 高：会话名 + 右侧图标按钮（`Tooltip` 必备）：侧栏开关 / 更多（`MenuButton`：压缩上下文 / 导出 / 删除会话）。
2. 「压缩上下文」搬进「更多」菜单。
3. 输入卡：
   - 左下 `+` 按钮 → **`mygo.Dialog.Open(OpenDialogOptions{…})`** 选图，等价于现有粘贴/拖入。
     注意：它**会阻塞到对话框关闭**（文档允许在任意 goroutine 调用），因此必须
     `go func(){ paths, err := mygo.Dialog.Open(...); a.post(...) }()`，否则界面卡死；
     结果回主线程后走现有的 `attachFiles()`（与拖入同一条路径）；
   - 权限下拉「完全访问」：**只读展示** `allowShell`，点击 `c.Toast("需在配置文件里开启 allow_shell")`；
   - 模型 chip 下移到卡内右下，用 **`Element.Menu` 原生菜单**弹出模型列表（`Submenu` 放档位），**替换现有自绘 `providerMenu` / `effortMenu`（省约 130 行）**：
     - 当前模型用 `MenuItem.Checked(true)` 标勾；
     - 思考档位做成子菜单（`m.Submenu("思考", …)`），当前档位 `Checked`；
     - 忙时（非 Idle）用 `MenuItem.Disabled(true)` 代替现在的「点了没反应」；
   - 发送按钮改圆形图标（↑），禁用灰 / 可用 `Accent`。
4. **不做** Chat/Trajectory 页签（用户确认 #4）。

**验收**：`TestComposerPlusButton`、`TestModelMenuInComposer`（`Click("模型")` → `ChooseMenuItem("GLM-5.3", "高")` → 断言 `Selection()`）、`TestModelMenuItemChecked`、`TestModelMenuDisabledWhileBusy`、`TestSendButtonRound`；`composer_keys_test.go` 全部保持绿（键位语义不能动）。

**风险**：原生菜单**已验证可驱动**（见开头探针结论），无备用分支。剩余风险只有两点：
- 菜单是**系统级弹出**，不走视图树的布局，因此**不受正文列 748 宽度约束**——这正好解决了现在自绘面板要手动 `MaxWidth(320)` + `AlignSelf(Start)` 的笨拙。
- `Checked` 只在 `it.p != nil`（菜单已构建）时生效，即必须在 build 回调里设置，不能在菜单外预先设。写法：在 `Element.Menu(func(m *ui.Menu){ … })` 内部按当前状态逐个 `m.Item(x).Checked(x == cur)`。

---

### P4 · 状态条

1. 输入卡下方单独一行：左侧「上下文 12k / 64k（n%）」+ 一条 `ui.Meter` 条形（`Warning` 取 `driver.AutoCompactRatio`，`Critical` 取 95%，都到了会自动压缩的位置）；右侧「N 轮」「N 步」。
2. 计数口径见 §6.1（已实现）。

**不做**：tok/s、cache hit、环形、轮数/步数的跨进程累计口径以外的统计（见 §6.2）。

---

## 6. 状态条数据采集

### 6.1 已实现（轮数 / 步数 / 上下文占比）

| 指标 | 实现 | 位置 |
|---|---|---|
| **轮数**（turns） | 会话级计数：每追加一条 user 消息 +1（插话投递也各算一条，与重放口径一致） | `driver.Driver.turns`、`Driver.Counts()` |
| **步数**（steps） | 每次 `startTurn` +1（即「发给模型的请求次数」：工具续轮、插话续轮都各算一步） | `driver.Driver.steps` |
| **上下文占比** | `TokensInUse() / 窗口`，向上取整；条形用 `ui.Meter` | `uiapp.app.contextPercent` / `statusBar` |

**口径为什么这样定**：两个数都能从会话内容推导（turns = user 消息条数、steps = agent 消息条数），
所以「重放 == 在线」天然成立，不需要新增事件类型。唯一的例外是压缩会丢弃历史，因此
`compact.snapshot` 带上当时的累计值当基线（`turns` / `steps` 两个可选字段，旧快照读出来是 0）；
重放时 = 快照基线 + 快照之后的消息条数。`Driver.Counts` 的注释与
`driver/counts_test.go`（含压缩、插话、工具续轮、旧快照四种情形）把这条不变式钉住了。

**其它 UI 同步**：`internal/locale` 加了 `composer.usagePercent` / `composer.turns` / `composer.steps`
中英两套文案；`uiapp/statusbar_test.go` 覆盖「0 轮 0 步 → 1 轮 1 步」「英文界面」「空态不渲染」
「接近上限时百分比过阈值」四种情形。

### 6.2 仍未做（留档）

| 指标 | 需要的改动 | 难点 |
|---|---|---|
| **tok/s** | 记录「首个文本事件」到 `EventDone` 的时间差 + 该轮 `OutputTokens` | 需在 `startStream` goroutine 里打点 |
| **累计 total tok** | 累加上轮 usage；`usageBase` 目前只存最近一次 | 压缩后基线重置的逻辑要一并处理 |
| **Cache hit %** | **provider 完全不解析**：`openai.go` 加 `usage.prompt_tokens_details.cached_tokens`；`anthropic.go` 加 `usage.cache_read_input_tokens` 与 `cache_creation_input_tokens` | 要改 `providers.Event` 结构 + 两家适配器 + 真实网关验证 |

建议顺序：前两项 + 上下文条已经做完了（§6.1）；剩下的 tok/s 与 cache hit 要动 `providers`，
需真实网关冒烟，单独一期做。

---

## 7. 明确不做的事

| 不做 | 理由 |
|---|---|
| 「Execution requested」整宽输入卡片 | 用户确认：插件产物 |
| 「Automation task / Analysis completed」事件卡片 | 同上；本期 Notice 只加图标 |
| Chat / Trajectory 页签 | 用户确认 #4 |
| 日期分组 | 用户确认 #2 |
| 底部状态条的真实数据 | **已实现轮数/步数/上下文占比**（§6.1）；tok/s、cache hit 仍留档 §6.2 |
| Plugins / Automation tasks 导航项 | 无对应功能，点开是空的 |
| 赞 / 踩 / 分享 | 无后端 |
| 「Standard mode」 | 无对应语义 |
| 会话级权限下拉真实生效 | 涉及 `allow_shell` 安全边界，是产品决策 |
| ~~自定义主题色板~~ | **已改为要做**：用户决定复刻 dsh 浅色色板（决策 #37，推翻 #15）；界面始终浅色（决策 #40），深色模式不做 |
| 环形进度 | `Meter` 条形足够 |

---

## 8. 测试影响与迁移策略（本方案最大成本项）

### 8.1 现状

`internal/uiapp/*_test.go` 有 **60+ 处**按可见文本定位：

```
35× tester.Click("发送")        7× tester.Click("新建")
 5× tester.HasText("待插入（1 条，下一轮请求前发送）")
 4× "压缩上下文"   4× "删除"   4× "停止"   4× "确认删除"
 3× "提交"   3× "DeepSeek · deepseek-chat"   3× "甲"
 2× "改名"   2× "确定"   2× "重命名输入"   2× "×"   2× "复制" …
```

`ui.Tester.Find` 是**精确匹配**（`n.text == s`），`HasText` 是包含匹配。`Element.Label(s)` 会**取代**可见文字进入文本索引（`copy.go` 已踩过并留了注释）。

### 8.2 P0 必做：定位器抽离

在 `internal/uiapp/testlocate_test.go` 定义：

```go
const (
    locSend        = "发送"
    locNewSession  = "新建会话"
    locCompact     = "压缩上下文"
    …
)
func clickSend(t *testing.T, tester *ui.Tester)
```

把 60+ 处字面量换成常量引用。**这一步不改任何界面，测试必须仍然全绿**——它把「改文案」从 60 处降到 1 处。

### 8.3 每期改动点

| 期 | 需要改的测试 | 预计 |
|---|---|---|
| P0 | `TestLayoutSidebarAndMainColumn`（220→新常量） | 2 |
| P1 | 侧栏 6 处（新建/改名/删除/确认删除）→ 改 `RightClick` + `ChooseMenuItem` + `AlertDialog` | 8 |
| P2 | 气泡/状态行相关约 5 处 | 6 |
| P3 | 模型选择 6 处 → 改 `ChooseMenuItem`；压缩上下文 4 处 → 改「更多」菜单 | 12 |
| P4 | 新增（状态条测试 `TestStatusBar*` 5 条 + driver 计数 7 条） | 12 |

### 8.4 能保住就保住的文案

`发送` / `停止` / `提交` / `确定` / `取消` / `删除` / `新建` 这类动词没有改的必要。必须改的（如「压缩上下文」搬进菜单）先加 `Label()` 给稳定无障碍名，让 `Find` 仍能命中。

### 8.5 测试写法示例（原生菜单 / 对话框）

```go
// 模型菜单：点 chip → 菜单 → 选模型 → 再进子菜单选档位
tester.Click("模型")
if got := tester.Menu(); !slices.Contains(got, "GLM-5.3") { t.Fatal(...) }
tester.ChooseMenuItem("GLM-5.3")
tester.Frame()
if app.Current().Selection().Model != "glm" { t.Fatal(...) }

// 勾选状态用 Menu() 查不到时，靠选完之后的 Selection() 断言，而不是断言菜单里的勾

// 侧栏删除：右键 → 菜单 → 删除 → 弹确认 → 取消
tester.RightClick("会话标题")
tester.ChooseMenuItem("删除")
tester.Frame()
tester.Click("取消")          // AlertDialog 返回 1
tester.Frame()
if app.Sessions() 里找不到该会话 → 已删除，Fatal

// 注意：AlertDialog 的标题在可见文本里出现两次，只按按钮文案定位
```

---

## 8.6 每一项视觉对应的 dsh 源文件（review 对照用）

| 我们的项 | dsh 源文件 | 抄什么 |
|---|---|---|
| 设计令牌（色/圆角/间距） | `ui-theme/src/styles/design-platform.css`（浅色段） | 语义别名色值、`border-l1..l4`、交互 hover/active、menu、turn-trigger、代码块 |
| 字体栈 | `ui-theme/src/styles/base.css`（`--dsh-font-family-*`） | 正文/代码两套 family 列表 |
| 侧栏尺寸与收起 | `ui-layout/src/client/columns.ts` | `SIDEBAR_DEFAULT/MIN/MAX/COLLAPSED/AUTO_COLLAPSE`、`CENTER_MIN` |
| 侧栏结构 / 配色 / 文案 | `ui-sidebar/src/client/SidebarRoot.tsx`、`SidebarRoot.module.css`、`locales.ts` | 品牌行、新建按钮、导航项、底部用户区；`session.new.label`、`toggle.collapse` |
| 回合结束行 / 过程组 | `ui-chat/src/client/chat/` 下 turn 相关组件 + `locale.ts` | `message.turnProcess.*`、`message.stepProcess.*` 文案与图标位置 |
| 消息气泡 / 复制按钮 | `ui-chat/src/client/chat/` 下消息容器与 `MessageActions` | 气泡底色（`--dsw-specific-bubble`）、时间戳格式、复制图标位置 |
| 代码块外观 | `ui-chat` 的 markdown 代码块样式 + `design-platform.css` 的 `markdown-code-block*` | 底色、banner 色、圆角、复制按钮位置 |
| 输入卡 | `ui-chat/src/client/chat/` 的 composer 组件 | 圆角、`+` 按钮、模型 chip、圆形发送按钮 |
| 图标 | `ui-primitives`（SVG 组件） | 需要的图标 path（MIT，记入 THIRD_PARTY_NOTICES） |

> 说明：dsh 是 React + CSS Modules，我们是 mygo 立即模式 UI，**不复制代码结构**，
> 只抄数值、文案与图标的视觉结果。

---

## 9. 里程碑与工作量

| 期 | 内容 | 预估 | 依赖 |
|---|---|---|---|
| P0.0 | **设计令牌**（theme/字体/布局常量/图标/THIRD_PARTY_NOTICES） | 0.75 天 | 无 |
| P0.1 | Scroll 修缺陷 + Split（280/264–420/56 图标栏/自动收起）+ 拆文件 + 测试定位器 | 0.5 天 | P0.0 |
| P1 | `session.created` 加 workspace + 侧栏两级树 + 品牌/新建/搜索/右键菜单+确认/用户区 | 1.5 天 | P0.1 |
| P2 | 回合结束行 + 气泡时间戳 + 过程组图标 + Notice 图标 | 1 天 | P0.1 |
| P3 | 头部按钮组 + 输入卡（+ / 权限展示 / 模型原生菜单 / 圆形发送） | 1.5 天 | P1、P2 |
| P4 | 状态条（上下文占比 + Meter + 轮数/步数） | 0.25 天 | P3 |
| 合计 | | **约 5.5 天**（不含评审往返） | |

**节奏**：P0.0（设计令牌）与 P0.1（重构）**分开提交**——前者有可见变化（配色/字体），
后者**零视觉变化**，便于 review 时不看像素；P1/P2 各自提交（视觉变化，截图对比验收）；P3 单独提交；P4 独立提交。每期跑 `scripts/check.ps1`（vet + staticcheck + `go test -race` + `-cpu=1,4` 界面稳定性）。

---

## 10. 决策已全部确认

| 问题 | 结论 |
|---|---|
| 用户消息形态 | 保持右侧浅底圆角气泡（只加时间戳） |
| 侧栏分组 | 项目可折叠两级树（不做日期分组） |
| 项目来源 | **路径 A**：`session.created` 加可选 `workspace` |
| 底部状态条 | **已实现**轮数/步数/上下文占比（§6.1）；tok/s、cache hit、环形不做 |
| 页签 | 完全不做 |
| 模型选择 | **原生 `ui.Menu`**（探针已验证可驱动） |
| 侧栏删除 | **右键菜单 + `AlertDialog` 二次确认**（探针已验证可驱动） |
| 配色 | **复刻 dsh 浅色令牌**（决策 #37，推翻 #15）；**界面始终浅色**，不跟随系统深色（决策 #40） |
| 侧栏尺寸 | 280 默认 / 264–420 拖动 / **56px 图标栏** / <1024 自动收起 / 正文 ≥400（决策 #38） |
| 字体 | **不打包 Montserrat**，品牌名与正文都用系统字体栈，代码用 dsh 的代码栈（决策 #39） |
| 文案 | 新增文案直接抄 dsh 的 zh locale（§0.5.4） |
| 图标 | 搬 dsh 的 SVG，`THIRD_PARTY_NOTICES.md` 注明 MIT |

所有前置不确定性已用探针排除（原生菜单 / 右键菜单 / MenuButton / AlertDialog 四项全部通过），P0 → P4 已按此顺序全部完成。

**P0 的提交拆分已执行**（`refactor: 拆文件` 与 `test: 定位器抽离` 分开，见提交记录）。
