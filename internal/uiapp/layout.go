package uiapp

import (
	"os"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// 布局与视觉常量（单一来源）。
//
// 尺寸、圆角、间距全部来自 dsh（MIT，见 THIRD_PARTY_NOTICES.md）：
//   - packages/client/ui-layout/src/client/columns.ts（侧栏与正文的最小/最大宽度）
//   - packages/client/ui-theme/src/styles/design-platform.css（圆角）
//   - 其余为截图量取，标注在注释里
//
// 规则（方案 §0.5）：每一期只能使用这里的常量与 theme_dsh.go 里的令牌，
// 不要再散落魔法数字。

const (
	// chatColumnWidth 是正文列的**默认**最大宽度（dsh 的聊天内容宽度 748：
	// clamp(680, 列宽×0.64, 920) 在常见窗口下的取值）。
	//
	// 运行时的实际宽度见 App.contentWidth()：用户拖动把手后会被覆盖，
	// 并记在 ui-state.json 里。
	chatColumnWidth = 748
	// chatContentMin 是拖动允许的最小宽度（dsh 的 CONTENT_MIN = 640）。
	chatContentMin = 640
	// chatContentMax 是拖动允许的最大宽度（dsh 的 CSS 上限 920）。
	chatContentMax = 920
	// chatContentEdgeBudget 是两侧把手与安全边距占的横向空间
	// （dsh 的 CONTENT_EDGE_BUDGET = 176）：窗口再窄就放不下更宽的正文。
	chatContentEdgeBudget = 176
	// widthHandleWidth 是贴边把手的最宽值（dsh `.widthHandle { width: min(10px, …) }`）。
	widthHandleWidth = 10
	// widthHandleInset 是把手与正文列之间留的间距（dsh 的 +24px）。
	widthHandleInset = 24

	// chatSidePadding 是正文列两侧的留白。
	chatSidePadding = 16
	// chatBubbleMaxWidth 是用户气泡的最大宽度。
	chatBubbleMaxWidth = 520

	// sidebarWidthDefault 是侧栏展开时的默认宽度（dsh SIDEBAR_DEFAULT）。
	sidebarWidthDefault = 280
	// sidebarWidthMin/Max 是拖动范围（dsh SIDEBAR_MIN/SIDEBAR_MAX）。
	sidebarWidthMin = 264
	sidebarWidthMax = 420
	// sidebarWidthCollapsed 是收起后的图标栏宽度（dsh SIDEBAR_COLLAPSED）：
	// 24px 图标列 + 两侧 16px 留白。
	sidebarWidthCollapsed = 56
	// sidebarAutoCollapseWidth 是自动收起的窗口宽度阈值（dsh SIDEBAR_AUTO_COLLAPSE）。
	sidebarAutoCollapseWidth = 1024
	// centerMinWidth 是正文区最小宽度（dsh CENTER_MIN）。
	centerMinWidth = 400

	// headerHeight 是右栏头部高度（截图量取）。
	headerHeight = 48
	// composerRadius 是输入卡圆角（截图量取）。
	composerRadius = 16
	// bubbleRadius 是用户气泡圆角。
	bubbleRadius = 16
)

// clampInt 把 v 限制在 [low, high]（侧栏拖动要用；P0.1 起使用）。
func clampInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}

// contentWidth 返回本帧正文列应该用的宽度。
//
// 规则照 dsh 的 `resolveContentWidth`：
//   - 有用户偏好 → 夹在 [640, min(920, 列宽 − 176)] 之间；
//   - 没有偏好   → clamp(列宽 × 0.64, 680, 920)，窄窗口下再让位给把手。
//
// 上限随窗口收缩是"窄窗口下把手自然消失"的根据：宽度被压到放不下把手时，
// 把手的可用宽度会算成 0，用户抓不到它（与 dsh 的 `min(10px, …)` 同一效果）。
func (a *App) contentWidth() float32 {
	column := a.viewportW
	// 窗口宽度还没测出来（首帧）：用默认值，避免按 0 算出一个极窄的列
	if column <= 0 {
		if a.contentWidthPreference > 0 {
			return float32(clampInt(int(a.contentWidthPreference), chatContentMin, chatContentMax))
		}
		return chatColumnWidth
	}
	maxWidth := int(column) - chatContentEdgeBudget
	if maxWidth < chatContentMin {
		maxWidth = chatContentMin
	}
	if maxWidth > chatContentMax {
		maxWidth = chatContentMax
	}
	if a.contentWidthPreference > 0 {
		return float32(clampInt(int(a.contentWidthPreference), chatContentMin, maxWidth))
	}
	return chatColumnWidth
}

// 侧栏本体：展开时是可拖动宽度的面板（ui.Split），收起时是 56px 图标栏。
//
// 尺寸规则来自 dsh（columns.ts）：默认 280、拖动 264–420、收起 56、窗口 <1024
// 自动收起；用户手动展开后即使窗口窄也保持展开（dsh 的 narrowExpanded）。
func (a *App) sidebarPane(c *ui.Context) {
	theme := c.Theme()
	if collapsed, railWidth := a.sidebarCollapsed(c); collapsed {
		a.sidebarRail(c, railWidth, theme)
		ui.Box(c).Width(1).FillHeight().Background(dsh.BorderL1)
		a.mainPane(c)
		return
	}

	// Split 自己会改 *size（拖动/方向键），它只保证不小于最小值；
	// 264–420 的范围在这里先夹住（上一帧拖出去，这一帧收回边界）。
	if a.sidebarWidth == 0 {
		a.sidebarWidth = sidebarWidthDefault
	} else {
		a.sidebarWidth = float32(clampInt(int(a.sidebarWidth), sidebarWidthMin, sidebarWidthMax))
	}
	ui.Split(c, &a.sidebarWidth, func() {
		a.sidebarBody(c)
	}, func() {
		// 右栏由 Split 的第二个 pane 渲染（见 View）
		a.mainPane(c)
	}).Fill()
}

// sidebarCollapsed 报告本帧是否收起成图标栏，以及图标栏宽度。
func (a *App) sidebarCollapsed(c *ui.Context) (bool, int) {
	width, _ := c.Size()
	return a.sidebarCollapsedAt(width)
}

// sidebarCollapsedForWidth 用上一帧记录的窗口宽度判断（测试用）。
func (a *App) sidebarCollapsedForWidth() bool {
	collapsed, _ := a.sidebarCollapsedAt(a.viewportW)
	return collapsed
}

// sidebarCollapsedAt 按给定窗口宽度判断是否收起。
func (a *App) sidebarCollapsedAt(width float32) (bool, int) {
	if a.sidebarUserCollapsed {
		return true, sidebarWidthCollapsed
	}
	threshold := a.opts.SidebarAutoCollapseWidth
	if threshold == 0 {
		threshold = sidebarAutoCollapseWidth
	}
	if threshold < 0 {
		return false, 0 // 负数表示禁用自动收起（测试用）
	}
	if int(width) < threshold && !a.sidebarExpandNarrow {
		return true, sidebarWidthCollapsed
	}
	return false, 0
}

// sidebarRail 渲染收起后的 56px 图标栏：只放新建与展开两个图标按钮。
//
// 图标用 dsh 的 SVG（icons.go）；24px 图标列 + 两侧 16px 留白 = 56px。
func (a *App) sidebarRail(c *ui.Context, width int, theme *ui.Theme) {
	_ = theme
	ui.Column(c).Width(float32(width)).FillHeight().Background(dsh.SidebarFill).
		Padding(8).Gap(10).Children(func() {
		if iconButton(c, iconNewChatSVG, a.tr("session.new.label"), 16, 4) {
			a.NewSession()
		}
		if iconButton(c, iconPanelLeftSVG, a.tr("toggle.open"), 16, 4) {
			a.sidebarUserCollapsed = false
			a.sidebarExpandNarrow = true
		}
	})
}

// sidebarNode 是侧栏树的一项：项目（workspace）或会话。
type sidebarNode struct {
	// 项目节点：isGroup=true，key 是 workspace 路径（空串 = 「默认」）
	// 会话节点：isGroup=false，key 是会话 id
	isGroup bool
	key     string
}

// sidebarBody 渲染展开状态的侧栏内容：品牌行 + 搜索 + 项目/会话树 + 用户区。
func (a *App) sidebarBody(c *ui.Context) {
	theme := c.Theme()
	ui.Column(c).FillHeight().Background(dsh.SidebarFill).
		Padding(10).Gap(6).Children(func() {
		// 品牌行：名称 + 新建（图标）+ 收起（图标）
		//
		// 品牌名用系统字体（决策 #39：不打包 Montserrat）；图标来自 dsh。
		ui.Row(c).Padding(2, 4).Gap(4).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, "simple-agent").FontSize(13).Bold().TextColor(theme.Text)
			ui.Spacer(c)
			if iconButton(c, iconNewChatSVG, a.tr("session.new.label"), 16, 4) {
				a.NewSession()
			}
			if iconButton(c, iconPanelLeftSVG, a.tr("toggle.collapse"), 16, 4) {
				a.sidebarUserCollapsed = true
			}
		})
		// 搜索：自己拼（`ui.SearchField` 内部把占位文字写死成英文 "Search"，
		// 外面覆盖不了；这里用图标 + 无装饰输入框，占位文案走 locale）
		a.searchField(c)

		ui.Box(c).Height(1).FillWidth().Background(dsh.BorderL1)

		if len(a.sessions) == 0 {
			ui.Text(c, a.tr("sidebar.empty")).FontSize(11).
				TextColor(theme.TextMuted).Padding(4, 6)
		}
		// 项目 → 会话 的两级树；会话多了它在 List 里滚
		a.sidebarRows(c)

		// 底部用户区
		ui.Box(c).Height(1).FillWidth().Background(dsh.BorderL1)
		ui.Row(c).Gap(8).Padding(4, 2).Children(func() {
			ui.Avatar(c, a.userName(), nil).Size(22, 22)
			ui.Text(c, a.userName()).FontSize(12).TextColor(theme.TextMuted)
		})
	})
}

// searchField 渲染侧栏搜索框（放大镜图标 + 圆角边框 + 走 locale 的占位文字）。
func (a *App) searchField(c *ui.Context) {
	theme := c.Theme()
	ui.Row(c).FillWidth().Gap(6).Padding(3, 8).Radius(8).
		Border(1, dsh.BorderL2).Background(dsh.BgBase).AlignItems(ui.Center).
		Children(func() {
			ui.Icon(c, iconSearchSVG).Size(13, 13).TextColor(theme.TextMuted)
			field := ui.TextInputBase(c, &a.sidebarQuery).Grow(1).
				Placeholder(a.tr("sidebar.search")).Label(a.tr("sidebar.search"))
			field.FontSize(12)
		})
}

// sidebarRows 渲染项目 → 会话的两级树。
//
// 为什么不用 `ui.Outline`：它给**每一行**都预留一个 Space(4)=16px 的三角槽，
// 并给子行再加 depth*Space(4) 缩进。叶子行（会话）不画三角，槽位却照样占着，
// 于是会话文字比项目文字多缩进 26px（实测），正是"会话缩进太深"的来源。
// Outline 的缩进是内部实现、不可配置，所以这里自己扁平化 + 画前缀：
//
//	项目行：[▾][名字]
//	会话行：[    ][标题]
//
// 两行文字从同一条 x 起（三角槽只在项目行占位，会话行不占），
// 三角紧贴项目名字，没有多余空隙。
func (a *App) sidebarRows(c *ui.Context) {
	query := strings.ToLower(strings.TrimSpace(a.sidebarQuery))
	type group struct {
		key      string
		sessions []*Session
	}
	var groups []group
	byKey := map[string]int{}
	// 组的出现顺序 = 会话的出现顺序（与 sidebarTree 一致）
	for _, session := range a.sessions {
		key := workspaceOr(session.Workspace, "")
		index, ok := byKey[key]
		if !ok {
			index = len(groups)
			byKey[key] = index
			groups = append(groups, group{key: key})
		}
		groups[index].sessions = append(groups[index].sessions, session)
	}

	// 扁平化：项目行 + （展开时的）会话行
	var rows []sidebarNode
	for _, g := range groups {
		var visible []*Session
		for i := len(g.sessions) - 1; i >= 0; i-- {
			session := g.sessions[i]
			if query != "" && !strings.Contains(strings.ToLower(session.Title), query) {
				continue
			}
			visible = append(visible, session)
		}
		if len(visible) == 0 {
			continue // 搜索把这一组过滤空了：整组隐藏
		}
		// 新出现的项目默认展开（用户收起过的保持收起）
		if !a.sidebarGroupSeen[g.key] {
			if a.sidebarGroupSeen == nil {
				a.sidebarGroupSeen = map[string]bool{}
			}
			if a.sidebarGroupOpen == nil {
				a.sidebarGroupOpen = map[string]bool{}
			}
			a.sidebarGroupSeen[g.key] = true
			a.sidebarGroupOpen[g.key] = true
		}
		rows = append(rows, sidebarNode{isGroup: true, key: g.key})
		if !a.sidebarGroupOpen[g.key] {
			continue
		}
		for _, session := range visible {
			rows = append(rows, sidebarNode{key: session.ID})
		}
	}

	ui.List(c, &a.sidebarList, len(rows), func(i int) {
		node := rows[i]
		if node.isGroup {
			a.sidebarGroupRow(c, node.key)
			return
		}
		a.sidebarSessionRow(c, node.key)
	}).Grow(1).FillWidth()

	// 「添加项目…」固定在列表**底部**（不随滚动）：项目再多也找得到，
	// 对齐 dsh 把 add 动作固定在菜单底部的做法。
	a.sidebarAddProjectRow(c)
}

// sidebarAddProjectRow 渲染「＋ 添加项目…」。
//
// 对齐 dsh 的 `menu.addWorkspace`（"添加工作区…"）：点开系统目录选择器，
// 选定后在该目录新建会话——项目是从会话反推的，所以"加项目"必然要开一个会话。
func (a *App) sidebarAddProjectRow(c *ui.Context) {
	theme := c.Theme()
	label := a.tr("project.add")
	if a.pickingProject {
		// 选择器开着：文案换成"正在选择…"（dsh 的 directoryBusy）
		label = a.tr("project.picking")
	}
	// 与项目行同一套几何：Padding(3,6) + 宽 sidebarArrowSlot 的槽 + 文字，
	// 所以「＋ 添加项目」的**文字**和项目名、会话标题落在同一条 x 上。
	// 槽里放「＋」图标（项目行那里放三角）。
	outer := ui.Row(c).FillWidth().Gap(0).Padding(3, 6).Radius(6).
		Cursor(ui.CursorPointer)
	if outer.Hovered() {
		outer.Background(dsh.SurfaceHover)
	}
	outer.Children(func() {
		slot := ui.Row(c).Width(sidebarArrowSlot).Shrink(0).Center()
		slot.Children(func() {
			ui.Icon(c, iconPlusSVG).Size(11, 11).TextColor(theme.TextMuted)
		})
		ui.Text(c, label).FontSize(12).TextColor(theme.TextMuted)
	})
	if outer.Clicked() {
		a.addProject()
	}
}

// sidebarGroupRow 渲染一个项目行：三角 + 目录名 + 相对时间。
//
// 三角由我们自己画（不再依赖 Outline 的槽位），所以它可以紧贴文字；
// 会话行的缩进量也用同一个常量，两行文字因此左对齐。
func (a *App) sidebarGroupRow(c *ui.Context, key string) {
	theme := c.Theme()
	count := 0
	var newest time.Time
	for _, session := range a.sessions {
		if workspaceOr(session.Workspace, "") != key {
			continue
		}
		count++
		if updated := sessionUpdatedAt(session); updated.After(newest) {
			newest = updated
		}
	}
	open := a.sidebarGroupOpen[key]
	var clicked bool
	ui.Row(c).FillWidth().Gap(0).Padding(3, 6, 3, 6).Radius(6).AlignItems(ui.Center).Children(func() {
		// 三角槽：**固定宽度**，宽度与 sidebarArrowSlot 相同。
		//
		// 不用"字符宽度 + 补偿 Box"那种估算法：▸ 的实际宽度随字体变化，
		// 估错就会差几像素。固定槽宽让文字起点是确定的。
		//
		// 三角本身**不设 Label**（照 textButton 的写法）：设了 Label 会替换
		// 可见文本索引，点击命中路径不同，实测点不动。
		glyph := "▸"
		if open {
			glyph = "▾"
		}
		arrowSlot := ui.Row(c).Size(sidebarArrowSlot, sidebarArrowSlot).Center().
			Cursor(ui.CursorPointer)
		arrowSlot.Children(func() {
			ui.Text(c, glyph).FontSize(12).TextColor(theme.TextMuted)
		})
		if arrowSlot.Hovered() {
			arrowSlot.Background(dsh.SurfaceHover).Radius(4)
		}
		if arrowSlot.Clicked() {
			clicked = true
		}
		name := ui.Text(c, a.workspaceGroupName(key)).FontSize(12).Bold().
			TextColor(theme.Text).Cursor(ui.CursorPointer)
		if name.Clicked() {
			clicked = true
		}
		ui.Spacer(c)
		if count > 0 && !newest.IsZero() {
			ui.Text(c, a.relativeTime(newest)).FontSize(11).TextColor(theme.TextMuted)
		}
	})
	if clicked {
		a.setSidebarGroupOpen(key, !open)
	}
}

// setSidebarGroupOpen 记住项目的展开/收起状态（纯界面态，不落盘）。
func (a *App) setSidebarGroupOpen(key string, open bool) {
	if a.sidebarGroupOpen == nil {
		a.sidebarGroupOpen = map[string]bool{}
	}
	a.sidebarGroupOpen[key] = open
}

// sidebarArrowSlot 是三角的槽位宽度（也是会话行的前导留白宽度）。
//
// 两者用同一个常量，是"会话文字与项目文字左对齐"的实现方式：
// 项目行 = [三角槽][名字]，会话行 = [同宽留白][标题]，文字自然落在同一条线上。
// 这正是用户要的效果——会话文字与项目文字左对齐，且项目那个三角不参与对齐。
const sidebarArrowSlot = 14

// sidebarSessionRow 渲染一条会话：标题 + 相对时间，右键菜单做改名/复制/删除。
func (a *App) sidebarSessionRow(c *ui.Context, id string) {
	theme := c.Theme()
	var session *Session
	index := -1
	for i, candidate := range a.sessions {
		if candidate.ID == id {
			session = candidate
			index = i
			break
		}
	}
	if session == nil {
		return
	}
	if session.renaming {
		ui.Column(c).Padding(0, 4).Gap(4).FillWidth().Children(func() {
			field := ui.TextInput(c, &session.renameDraft).
				Placeholder(a.tr("sidebar.rename.placeholder")).
				Label(a.tr("sidebar.rename.label"))
			field.AutoFocus().FillWidth()
			focused := field.Focused()
			if field.Submitted() || (session.renameWasFocused && !focused) {
				a.commitRename(session)
				return
			}
			session.renameWasFocused = focused
			if c.Shortcut(0, ui.KeyEscape) {
				session.renaming = false
			}
			ui.Row(c).Gap(2).Children(func() {
				if textButton(c, a.tr("common.ok"), 12, theme.Accent) {
					a.commitRename(session)
				}
				if textButton(c, a.tr("common.cancel"), 12, theme.TextMuted) {
					session.renaming = false
				}
			})
		})
		return
	}

	active := index == a.active
	// 两行的左右内边距相同（3,6）；对齐靠会话行开头补一个 sidebarArrowSlot
	// 宽的空白（项目行那里放的是三角），这样两种行的文字起点一致，
	// 选中背景的左右边距也一致。
	row := ui.Row(c).FillWidth().Gap(0).Padding(3, 6).Radius(6)
	switch {
	case active:
		row.Background(dsh.SurfaceActive)
	case row.Hovered():
		row.Background(dsh.SurfaceHover)
	}
	row.ContextMenu(func(m *ui.Menu) {
		a.sidebarSessionMenu(m, session)
	})
	row.Children(func() {
		// 与项目行的三角槽等宽的空白：让两种行的文字起点对齐
		ui.Box(c).Width(sidebarArrowSlot).Shrink(0)
		title := ui.Text(c, session.Title).FontSize(12).TextColor(theme.Text).
			MaxLines(1).Grow(1).Cursor(ui.CursorPointer)
		if title.Clicked() {
			a.active = index
		}
		// 空 Box 代替 Gap：Row 的 gap 设成 0（否则会叠在三角槽上导致错位），
		// 标题与时间之间的小间距在这里显式给。
		ui.Box(c).Width(6).Shrink(0)
		ui.Text(c, a.relativeTime(sessionUpdatedAt(session))).FontSize(10).
			TextColor(theme.TextMuted)
		// 悬停才**可见**的「⋯」菜单：不习惯右键的用户也能操作。
		//
		// 注意它必须**每帧都创建**：mygo 的原生菜单在"选中之后的那一帧"再跑一次
		// build 来报告 Chosen，靠元素 id 匹配。原生的弹出菜单一出现，指针就离开
		// 了会话行，如果这时元素不存在，Chosen 永远不会触发（真实窗口里就是
		// "点删除没有任何反应"）。不可见时用透明色 + Disabled，既不画出来也点不到，
		// 但 id 与菜单构建保持稳定。
		//
		// 不能改成 Disabled：原生菜单"打开"的那一帧同样按元素 id 找它，disabled
		// 的元素会被跳过（menu.go 的 buildMenu），菜单就直接不弹。所以这里是
		// **透明但可交互**：不悬停时画成透明，指针移上去后立刻变成正常颜色。
		more := ui.Text(c, "⋯").FontSize(12).Padding(0, 2).
			Label(a.tr("sidebar.more") + " " + session.Title).
			Cursor(ui.CursorPointer)
		if row.Hovered() || more.Hovered() {
			more.TextColor(theme.TextMuted)
		} else {
			more.TextColor(theme.TextMuted.Alpha(0))
		}
		more.Menu(func(m *ui.Menu) {
			a.sidebarSessionMenu(m, session)
		})
	})
}

// sidebarSessionMenu 填会话行的菜单项（右键菜单与「⋯」共用）。
func (a *App) sidebarSessionMenu(m *ui.Menu, session *Session) {
	if m.Item(a.tr("sidebar.rename")).Chosen() {
		session.renaming = true
		session.renameDraft = session.Title
		session.renameWasFocused = false
	}
	m.Separator()
	if m.Item(a.tr("common.delete")).Chosen() {
		a.deleteTarget = session.ID
		a.deleteConfirm = true
	}
}

// userName 返回底部用户区显示的名字。
func (a *App) userName() string {
	for _, key := range []string{"USERNAME", "USER", "LOGNAME"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return a.tr("sidebar.user.local")
}

// sessionUpdatedAt 取会话的最近活动时间（没有消息时退回到"现在"）。
func sessionUpdatedAt(session *Session) time.Time {
	entries := session.Conversation().Entries()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Message != nil {
			return entries[i].CreatedAt()
		}
	}
	return time.Now()
}

// relativeTime 把时间格式化成 dsh 那样的短相对时间（5d / 4h / 12m / 刚刚）。
func (a *App) relativeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	delta := time.Since(t)
	switch {
	case delta < time.Minute:
		return a.tr("time.now")
	case delta < time.Hour:
		return a.tr("time.minutes", int(delta.Minutes()))
	case delta < 24*time.Hour:
		return a.tr("time.hours", int(delta.Hours()))
	default:
		return a.tr("time.days", int(delta.Hours()/24))
	}
}

// workspaceOr 取工作区：日志里没记（旧日志）时用当前配置的值。
func workspaceOr(saved, current string) string {
	if saved != "" {
		return saved
	}
	return current
}

// workspaceGroupName 是侧栏里的项目名：取目录名；空串归到「默认」。
func (a *App) workspaceGroupName(workspace string) string {
	if workspace == "" {
		return a.tr("sidebar.workspace.default")
	}
	cleaned := strings.TrimRight(strings.ReplaceAll(workspace, "\\", "/"), "/")
	if idx := strings.LastIndex(cleaned, "/"); idx >= 0 && idx+1 < len(cleaned) {
		return cleaned[idx+1:]
	}
	if cleaned == "" {
		return a.tr("sidebar.workspace.default")
	}
	return cleaned
}

// confirmDialogs 渲染二次确认对话框（删除会话）。
func (a *App) confirmDialogs(c *ui.Context) {
	// 菜单回调发生在帧内靠后的位置，目标 id 可能这一帧还没填上：
	// 等它填好再渲染对话框，避免出现标题为空的确认框（点确认会删错/删不掉）。
	if !a.deleteConfirm || a.deleteTarget == "" {
		return
	}
	title := ""
	if session := a.find(a.deleteTarget); session != nil {
		title = session.Title
	}
	// AlertDialog 返回 0 = 第一个按钮（删除），1 = 取消
	switch ui.AlertDialog(c, &a.deleteConfirm, a.tr("sidebar.delete.title"),
		a.tr("sidebar.delete.message", title), a.tr("common.delete"), a.tr("common.cancel")) {
	case 0:
		if index := a.sessionIndex(a.deleteTarget); index >= 0 {
			a.deleteSession(index)
		}
		a.deleteTarget = ""
	case 1:
		// 取消：什么都不做
		a.deleteTarget = ""
	}
}
