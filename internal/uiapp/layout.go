package uiapp

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/driver"
	"simple-agent/internal/host"
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
//
// 这里的「新建」**保留**而展开状态下顶部那个去掉了：折叠时看不到项目行，
// 没有别的地方能新建；它用默认工作区（NewSession 的行为）。
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
	// 项目节点：isGroup=true，key 是项目 ID（没有档案时是 workspace 路径）
	// 会话节点：isGroup=false，key 是会话 id
	isGroup bool
	key     string
	// name 是项目节点的显示名（来自项目档案；会话节点不用）。
	name string
	// isArchive 表示这是项目下的「已归档」小节（默认折叠）。
	//
	// 它和项目行一样是可折叠的分组，但不算一个项目：不显示「新建会话」，
	// 也不参与项目排序。
	isArchive bool
}

// archivedKey 返回某个项目下「已归档」小节的分组 key。
//
// 加前缀是为了不与项目 key 冲突（项目 key 是 UUID，前缀不会被撞上）。
func archivedKey(projectKey string) string { return "archive:" + projectKey }

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
			// 新建会话按钮已经移到各个项目行的右侧（在那里才知道新会话
			// 该归到哪个项目）；顶部只留收起侧栏。
			if iconButton(c, iconPanelLeftSVG, a.tr("toggle.collapse"), 16, 4) {
				a.sidebarUserCollapsed = true
			}
		})
		// 搜索：自己拼（`ui.SearchField` 内部把占位文字写死成英文 "Search"，
		// 外面覆盖不了；这里用图标 + 无装饰输入框，占位文案走 locale）
		a.searchField(c)

		ui.Box(c).Height(1).FillWidth().Background(dsh.BorderL1)

		if len(a.sessions) == 0 {
			// 空态：没有任何项目行，也就没有行内的新建按钮 ——
			// 这里必须给一个入口，否则用户卡在"还没有会话"出不来。
			// 它开在默认工作区（与折叠成图标栏时那个按钮同一语义）。
			ui.Text(c, a.tr("sidebar.empty")).FontSize(11).
				TextColor(theme.TextMuted).Padding(4, 6)
			if iconButton(c, iconNewChatSVG, a.tr("session.new.label"), 13, 3) {
				a.NewSession()
			}
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
		key      string // 项目 ID（没有档案时退回 workspace 路径）
		name     string // 显示名
		sessions []*Session
	}
	var groups []group
	byKey := map[string]int{}
	// 分组按**项目档案**：key 用项目 ID，显示名用档案里的 Name。
	//
	// 以前是拿会话的 workspace 反推：名字对了，但**排不出项目顺序**
	// （反推只有名字，没有先后），而且"移除的项目"也表达不了。
	// 现在后端给出 ProjectID/ProjectName，顺序跟 projects.json 一致。
	for _, session := range a.sessions {
		key := session.ProjectIDForTest()
		name := session.ProjectNameForTest()
		if key == "" {
			// 没有项目档案（内存后端 / 还没登记）：退回按 workspace 分组
			key = workspaceOr(session.Workspace, "")
			name = a.workspaceGroupName(key)
		}
		index, ok := byKey[key]
		if !ok {
			index = len(groups)
			byKey[key] = index
			groups = append(groups, group{key: key, name: name})
		}
		groups[index].sessions = append(groups[index].sessions, session)
	}
	// 项目顺序跟 projects.json 一致（后端已按索引顺序返回，这里再排一次
	// 是为了容忍"同一项目内会话分散在列表各处"的情况）。
	if order := a.projectOrder(); order != nil {
		sort.SliceStable(groups, func(i, j int) bool {
			ri, iok := order[groups[i].key]
			rj, jok := order[groups[j].key]
			if !iok || !jok {
				// 没有档案的组排在后面（内存后端）
				return iok && !jok
			}
			return ri < rj
		})
	}

	// 扁平化：项目行 + （展开时的）会话行 + 已归档小节
	var rows []sidebarNode
	for _, g := range groups {
		var active, archived []*Session
		// **保持档案里的顺序**，不反转也不按时间重排：
		// 用户能调整会话顺序，界面就得照它显示。
		for _, session := range g.sessions {
			if query != "" && !strings.Contains(strings.ToLower(session.Title), query) {
				continue
			}
			if session.archivedSession {
				archived = append(archived, session)
				continue
			}
			active = append(active, session)
		}
		if len(active) == 0 && len(archived) == 0 {
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
		rows = append(rows, sidebarNode{isGroup: true, key: g.key, name: g.name})
		if !a.sidebarGroupOpen[g.key] {
			continue
		}
		for _, session := range active {
			rows = append(rows, sidebarNode{key: session.ID})
		}
		// 归档的会话**默认完全不显示**，由项目行菜单的「显示已归档」打开。
		//
		// 以前是"默认折叠的小节"，用户嫌那个三角丑、也太占注意力。
		// 对齐 dsh：归档视角属于**项目**，用菜单切换。打开后它们以灰色
		// 排在正常会话后面（不另起小节），关掉就彻底看不见。
		if a.showArchived != nil && a.showArchived[g.key] {
			for _, session := range archived {
				rows = append(rows, sidebarNode{key: session.ID})
			}
		}
	}

	ui.List(c, &a.sidebarList, len(rows), func(i int) {
		node := rows[i]
		if node.isGroup {
			a.sidebarGroupRow(c, node.key, node.name)
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

// sidebarGroupRow 渲染一个项目行：三角 + 项目名 + 「在此项目新建会话」。
//
// 三角由我们自己画（不再依赖 Outline 的槽位），所以它可以紧贴文字；
// 会话行的缩进量也用同一个常量，两行文字因此左对齐。
//
// name 由调用方给出（来自项目档案）；没有档案时是 workspace 反推的名字。
func (a *App) sidebarGroupRow(c *ui.Context, key, name string) {
	theme := c.Theme()
	open := a.sidebarGroupOpen[key]
	var clicked bool
	var newHere bool
	// 项目行**比会话行重**：更大的字、更大的内边距、加一层底色。
	//
	// 用户的原话："会话加上前面的运行状态，比项目还要靠前，把项目高度/字体
	// 都稍微变大点并且加个底"。原来是两者同字号同内边距，只有名字粗体，
	// 层级全靠缩进撑着 —— 而缩进当时还是对齐的，看起来会话比项目还靠前。
	//
	// 底色只用最淡的一档（SurfaceHover，bluish-75）：既能把项目行连成
	// 一块，又不会让侧栏变成一堆色块。
	row := ui.Row(c).FillWidth().Gap(0).Padding(5, 6, 5, 6).
		Radius(6).AlignItems(ui.Center).Background(dsh.SurfaceHover)
	// 右键菜单：项目的视角设置（目前只有「显示已归档」）。
	//
	// 对齐 dsh 的做法：归档属于**项目自己的视角**，不是全局开关，
	// 也不该在侧栏里占一个常驻控件（用户嫌之前那个三角丑又抢眼）。
	a.projectMenu(row, key)
	row.Children(func() {
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
			// 项目行本身有底色，悬停反馈用更深一档才看得出来
			arrowSlot.Background(theme.SurfacePressed).Radius(4)
		}
		if arrowSlot.Clicked() {
			clicked = true
		}
		nameText := ui.Text(c, name).FontSize(13).
			Bold().TextColor(theme.Text).Cursor(ui.CursorPointer)
		if nameText.Clicked() {
			clicked = true
		}
		ui.Spacer(c)
		// 项目行**不显示时间**：它在树上只是分组，用户关心的是"这个项目
		// 有几个会话"，时间要看会话行或悬停的提示。显示时间还会跟这里的
		// 新建按钮抢位置（两者挤在一起，项目名被压窄）。
		if iconButton(c, iconNewChatSVG, a.tr("session.newHere"), 13, 3) {
			newHere = true
		}
	})
	if clicked {
		a.setSidebarGroupOpen(key, !open)
	}
	// 新建放到行外执行：iconButton 的点击发生在渲染中，
	// 立刻 Submit 会让本帧后续的会话列表与索引不同步。
	if newHere {
		a.newSessionIn(a.workspaceForGroup(key))
	}
}

// projectMenu 挂上项目行的右键菜单。
//
// 目前只有「显示已归档」：打开后这个项目下的归档会话以灰色列出来，
// 关掉就完全不显示（默认）。状态是**纯内存的**，重启回到默认（关）。
func (a *App) projectMenu(row *ui.Element, projectKey string) {
	row.ContextMenu(func(m *ui.Menu) {
		showing := a.showArchived != nil && a.showArchived[projectKey]
		label := a.tr("sidebar.showArchived")
		if showing {
			label = "✓ " + label
		}
		if m.Item(label).Chosen() {
			if a.showArchived == nil {
				a.showArchived = map[string]bool{}
			}
			a.showArchived[projectKey] = !showing
		}
	})
}

// workspaceForGroup 把侧栏分组的 key 还原成 workspace 路径。
//
// key 平时是项目 ID，而新建会话要的是目录路径 —— 从该组下的任一
// 会话身上取（同组会话的 workspace 相同）。取不到就退回 key 本身
// （没有项目档案时 key 就是 workspace）。
func (a *App) workspaceForGroup(key string) string {
	for _, session := range a.sessions {
		if session.projectID == key {
			return session.Workspace
		}
	}
	return key
}

// sessionIDsText 拼出"项目 id + 会话 id"两行文本（复制用）。
//
// 为什么两个一起复制：报问题时要让对方知道"哪个项目、哪个会话"，
// 只给会话 id 对方还得再问项目。两行文本粘过去能直接看懂，
// 也不会混淆哪个是哪个。
func sessionIDsText(session *Session) string {
	var b strings.Builder
	if session.projectID != "" {
		fmt.Fprintf(&b, "project: %s\n", session.projectID)
	} else {
		b.WriteString("project: (无)\n")
	}
	fmt.Fprintf(&b, "session: %s", session.ID)
	return b.String()
}

// setSidebarGroupOpen 记住项目的展开/收起状态（纯界面态，不落盘）。
func (a *App) setSidebarGroupOpen(key string, open bool) {
	if a.sidebarGroupOpen == nil {
		a.sidebarGroupOpen = map[string]bool{}
	}
	a.sidebarGroupOpen[key] = open
}

// sidebarArrowSlot 是三角槽位的宽度。
//
// 项目行 = [三角槽][名字]。会话行在此基础上**再缩进一档**
// （sidebarSessionIndent），让内容明显归属在项目之下 ——
// 原先两者同宽、都从 Padding 起算，结果会话的状态点和项目的三角
// 落在同一条竖线上，看上去会话比项目还靠前，层级是反的。
const sidebarArrowSlot = 14

// sidebarSessionIndent 是会话行相对项目行的额外缩进。
//
// 拿到状态点、标题都和项目名错开，视觉上"缩在里面"。
const sidebarSessionIndent = 12

// sidebarSessionRow 渲染一条会话：状态点 + 标题 + 相对时间，右键菜单做改名/复制/删除。
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
	// 会话行在项目行基础上**再缩进一档**（左侧多 sidebarSessionIndent），
	// 让层级一眼可见：项目在上、会话缩在里面。右侧内边距保持不变，
	// 这样选中背景的右边缘仍与项目行对齐。
	row := ui.Row(c).FillWidth().Gap(0).
		Padding(3, 6+sidebarSessionIndent, 3, 6).Radius(6)
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
		// 运行状态点：代替原来纯留白的位置，让用户一眼看出哪些会话还在跑。
		//
		// 为什么用字符而不是 SVG 图标：mygo 没有内建动画组件；而状态点最需要
		// 表达的恰恰是"还在动"。用实心点 + 颜色（对齐 todo_panel 里
		// StateDot 的做法）已经能把"忙/空闲/等你回答"分开，比画一个静态
		// 小图标信息量大，也不必自绘动画。
		//
		// 空闲时**不占位**：侧栏里大多数会话都是安静的，全都画一个灰点反而
		// 让"And others"淹没重点。
		//
		// **不能给这个 Text 设 Label**：mygo 里元素一旦有 Label，a11y 树就用
		// Label 顶替可见文本，符号会被"正在生成"这类文字替换掉（实测就是这样：
		// 侧栏里显示的是文字而不是圆点）。状态含义靠颜色和位置表达，
		// 读屏用户从"上下文 1 轮 1 步"那些信息也能判断，不必为此牺牲视觉。
		if dot, color := a.sessionStatusDot(session); dot != "" {
			ui.Text(c, dot).FontSize(10).TextColor(color).
				Width(sidebarArrowSlot).Shrink(0)
		} else {
			// 与项目行的三角槽等宽的空白：让两种行的文字起点对齐
			ui.Box(c).Width(sidebarArrowSlot).Shrink(0)
		}
		// 归档的会话用弱化色：一眼看出它不能跑（点进去也只能看历史）
		titleColor := theme.Text
		if session.archivedSession {
			titleColor = theme.TextMuted
		}
		title := ui.Text(c, session.Title).FontSize(12).TextColor(titleColor).
			MaxLines(1).Grow(1).Cursor(ui.CursorPointer)
		if title.Clicked() {
			// 走统一入口：草稿的存/取都在里面
			a.switchTo(index)
		}
		// 复制按钮：复制 project 和 session 的 id。
		//
		// 用途是"沟通"—— 报问题时对方要知道是哪个会话，而 id 太长没法手打。
		// 复制的是两行文本（`project: <id>` / `session: <id>`），
		// 粘到聊天窗口里能直接看懂，也不必让对方猜哪个是哪个。
		//
		// **结构每帧固定**：和下面的「⋯」同一道理 —— mygo 的原生菜单靠
		// 元素 id 匹配，某个兄弟元素"悬停时才有"会让指针一移开元素树就变，
		// 后面的「⋯」id 位移，菜单再也选不中（实测过）。
		// 所以按钮常驻，只在悬停时显示文字颜色。
		a.copyButtonIdle(c, "session-ids:"+session.ID, sessionIDsText(session),
			a.tr("sidebar.copyIds"), row.Hovered())
		// 未读邮件徽标：放在时间左边。
		//
		// 为什么必须显示：邮件是**别的会话主动发来的**，用户的当前会话可能
		// 完全没察觉到；没有徽标就永远不知道侧栏里某个会话在等它处理事情。
		//
		// 只在有未读时出现（不占位）：大多数会话没有邮件，常驻一个灰色 0
		// 只会让侧栏变吵。
		if unread := session.MailboxUnread(); unread > 0 {
			label := "✉"
			if unread > 1 {
				label = fmt.Sprintf("✉%d", unread)
			}
			ui.Text(c, label).FontSize(10).TextColor(theme.Accent).Shrink(0).
				Label(a.tr("sidebar.mailUnread", unread))
			ui.Box(c).Width(6).Shrink(0)
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

// sessionStatusDot 返回会话的运行状态符号与颜色（空符号表示空闲、不显示）。
func (a *App) sessionStatusDot(session *Session) (string, ui.Color) {
	return statusDotFor(session.Busy())
}

// statusDotFor 是 Busy → (符号, 颜色) 的映射，抽成纯函数便于测试。
//
// 为什么空闲不显示：侧栏里大多数会话都是安静的，全都画一个灰点反而
// 让真正在跑的那几个淹没在噪声里。
//
// 符号选择：
//
//	●  正在干活（流式输出 / 执行工具）—— 实心点，最醒目
//	?  等你回答 —— 必须让用户注意到，否则回合一直卡着
//	◐  后台压缩 —— 用户不需要操作，弱化显示
func statusDotFor(busy driver.Busy) (string, ui.Color) {
	switch busy {
	case driver.BusyStreaming:
		return "●", dsh.BrandBlue
	case driver.BusyRunningTools:
		// 执行工具用警告色：这段时间通常比流式长，且可能要用户授权
		return "●", dsh.Warning
	case driver.BusyAwaitingAnswer:
		return "?", dsh.BrandBlue
	case driver.BusyCompacting:
		return "◐", dsh.LabelSecondary
	default:
		return "", dsh.StateIdle
	}
}

// sidebarSessionMenu 填会话行的菜单项（右键菜单与「⋯」共用）。
func (a *App) sidebarSessionMenu(m *ui.Menu, session *Session) {
	// 归档的会话只能恢复，别的一律不给（改名、删除都拦着）——
	// 菜单里灰着也容易点错，直接不提供更省事。
	if session.archivedSession {
		if m.Item(a.tr("sidebar.restore")).Chosen() {
			a.setSessionArchived(session, false)
		}
		return
	}
	if m.Item(a.tr("sidebar.rename")).Chosen() {
		session.renaming = true
		session.renameDraft = session.Title
		session.renameWasFocused = false
	}
	if m.Item(a.tr("sidebar.archive")).Chosen() {
		a.setSessionArchived(session, true)
	}
	m.Separator()
	if m.Item(a.tr("common.delete")).Chosen() {
		a.deleteTarget = session.ID
		a.deleteConfirm = true
	}
}

// setSessionArchived 归档/恢复一个会话。
//
// 归档 = 封存：历史还能看，但不能发消息、压缩、改名，必须恢复才能继续。
// 不动任何文件，只改项目档案里的状态位。
func (a *App) setSessionArchived(session *Session, archived bool) {
	if session == nil {
		return
	}
	kind := "restoreSession"
	if archived {
		kind = "archiveSession"
	}
	_, _ = a.host.Submit(host.Command{SessionID: session.ID, Kind: kind})
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
