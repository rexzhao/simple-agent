package uiapp

import (
	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
)

// 任务清单面板（对齐 deepseek-harness 的 TodoPanel）。
//
// 位置与形态来自 dsh 的 `conversation.input.dock` 插槽：挂在**输入卡正上方**，
// 与输入卡同宽居中，默认**折叠**，只显示一行头部。
//
// 视觉全部照 dsh 的 TodoPanel.module.css：
//   - 底板：`--dsw-specific-menu`（rgba(248,249,250,.58)）+ 圆角 lg(12) + 细边 l1
//   - 内边距 6/12，行间距 8
//   - 标题 13px/500；进度 13px/400 三级灰，用 U+2002 宽空格分隔（HTML 会合并
//     普通空格，dsh 也是这么处理的）
//   - 列表最大高度 180，超出内部滚动
//   - 每项单行、超出省略号，16px 状态圆点位
const (
	// todoPanelRadius 是面板圆角（dsh `--dsw-radius-lg`）。
	todoPanelRadius = 12
	// todoListMaxHeight 是展开后列表的最大高度（dsh `.list { max-height: 180px }`）。
	todoListMaxHeight = 180
	// todoDotSlot 是状态圆点的布局槽宽（dsh `.glyph { width: 16px }`）。
	todoDotSlot = 16
	// todoDotSize 是实心圆点的直径（dsh `.dot` 10px 槽内画 6px 芯，inset 20%）。
	todoDotSize = 6
)

// todoPanel 渲染任务清单；没有清单时什么都不画（dsh 的 `todos.length === 0
// return null`）。
func (a *App) todoPanel(c *ui.Context, session *Session, theme *ui.Theme) {
	todos := session.Todos()
	if len(todos) == 0 {
		return
	}

	// 与输入卡同宽居中：外层留白、内层限宽（与 composer 的写法一致）
	ui.Column(c).FillWidth().Padding(chatSidePadding, 4).Children(func() {
		ui.Column(c).MaxWidth(a.contentWidth()).AlignSelf(ui.Center).FillWidth().
			Background(dsh.TodoMenuSurface).Border(1, dsh.BorderL1).Radius(todoPanelRadius).
			Padding(6, 12).Gap(8).Children(func() {
			a.todoHeader(c, session, todos, theme)
			if !session.todoCollapsed {
				a.todoList(c, todos, theme)
			}
		})
	})
}

// todoHeader 是折叠/展开的那一行（dsh 的 `<button className={css.header}>`）。
func (a *App) todoHeader(c *ui.Context, session *Session, todos []conversation.TodoItem, theme *ui.Theme) {
	// 折叠状态用箭头：折叠时朝下（可展开），展开时朝上（可收起）——
	// 与 dsh 一致（collapsed ? ChevronUp : ChevronDown）。
	chevron := "⌃"
	if !session.todoCollapsed {
		chevron = "⌄"
	}
	row := ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).
		Label(a.tr("todo.toggle")).Children(func() {
		// 复选框图标（dsh 的 IconChecklistOutlineRegular）：mygo 没有内建
		// 图标集，用等宽字符近似，自绘 SVG 不值当。
		ui.Text(c, "☑").FontSize(13).TextColor(dsh.LabelTertiary)
		ui.Text(c, a.tr("todo.title")).FontSize(13).TextColor(dsh.LabelPrimary)
		ui.Text(c, todoProgressLabel(a, todos)).FontSize(13).TextColor(dsh.LabelTertiary).
			MaxLines(1).Grow(1)
		ui.Text(c, chevron).FontSize(13).TextColor(dsh.LabelTertiary)
	})
	// 点击整行切换折叠（dsh 的 onClick 在 header 按钮上）
	if row.Clicked() {
		session.todoCollapsed = !session.todoCollapsed
	}
}

// todoList 渲染展开后的清单（dsh 的 `<ul className={css.list}>`）。
func (a *App) todoList(c *ui.Context, todos []conversation.TodoItem, theme *ui.Theme) {
	ui.Scroll(c).FillWidth().MaxHeight(todoListMaxHeight).Children(func() {
		ui.Column(c).FillWidth().Gap(8).Children(func() {
			for _, todo := range todos {
				ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).
					Label(todoStatusLabel(a, todo.Status)).Children(func() {
					// 16px 槽位里居中放一个圆点（dsh 的 `.glyph` + StateDot）
					ui.Row(c).Size(todoDotSlot, todoDotSlot).Center().Children(func() {
						ui.Box(c).Size(todoDotSize, todoDotSize).Radius(todoDotSize / 2).
							Background(todoDotColor(todo.Status))
					})
					ui.Text(c, todo.Content).FontSize(13).TextColor(dsh.LabelSecondary).
						MaxLines(1).Grow(1)
				})
			}
		})
	})
}

// todoProgressLabel 生成头部右侧的统计，例如「2 已完成　·　1 进行中」。
//
// 分隔符用 U+2002（en space）加中点，与 dsh 的 `\u2002·\u2002` 一致——
// 普通空格会被排版合并，看不出间距。
func todoProgressLabel(a *App, todos []conversation.TodoItem) string {
	done, active := 0, 0
	for _, todo := range todos {
		switch todo.Status {
		case conversation.TodoCompleted:
			done++
		case conversation.TodoInProgress:
			active++
		}
	}
	pending := len(todos) - done - active

	parts := make([]string, 0, 3)
	if done > 0 {
		parts = append(parts, a.tr("todo.progress.done", done))
	}
	if active > 0 {
		parts = append(parts, a.tr("todo.progress.active", active))
	}
	if pending > 0 {
		parts = append(parts, a.tr("todo.progress.pending", pending))
	}
	if len(parts) == 0 {
		return ""
	}
	joined := parts[0]
	for _, part := range parts[1:] {
		joined += "\u2002·\u2002" + part
	}
	return joined
}

// todoStatusLabel 是给无障碍读的状态文字（dsh 的 `statusLabel`）。
func todoStatusLabel(a *App, status string) string {
	switch status {
	case conversation.TodoCompleted:
		return a.tr("todo.status.completed")
	case conversation.TodoInProgress:
		return a.tr("todo.status.inProgress")
	default:
		return a.tr("todo.status.pending")
	}
}

// todoDotColor 按状态取圆点颜色（dsh 的 StateDot 色映射）。
//
// ongoing 在 dsh 里是旋转的环形 spinner；mygo 没有内建动画组件，这里用品牌蓝
// 实心点代替——形状差异记在文档里，比自绘旋转动画更划算。
func todoDotColor(status string) ui.Color {
	switch status {
	case conversation.TodoCompleted:
		return dsh.Success
	case conversation.TodoInProgress:
		return dsh.BrandBlue
	default:
		return dsh.StateIdle
	}
}
