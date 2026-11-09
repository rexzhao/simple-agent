package uiapp

import (
	"github.com/egoist/mygo/ui"

	"simple-agent/internal/markdown"
)

// 表格渲染。
//
// 用 ui.Grid 等宽列 + 表头加粗，放在横向滚动容器里（宽表格不撑宽窗口）。

// renderTable 渲染表格：**等分列宽**、表头加粗。
//
// 关键：Grid 必须撑满正文列宽（FillWidth），它的 `Columns(n)` 才是"n 等分"。
//
// 之前外面套了 `ui.ScrollHorizontal`（本质是 Row）：Row 在横向给子元素的是
// **无约束宽度**，`Fr(1)` 没有可分的空间，于是每列塌缩到内容宽度——两列紧贴在一起，
// 视觉上像"每个单元格各占一行"、列也完全对不齐（用户截图里的现象）。
//
// 代价：超宽表格不再横向滚动，而是在正文列宽内做**等分 + 单元格换行**。
// 对本场景（项目文档里的结构表）这样更可读；真要横向滚动得给 Grid 一个确定的
// 最小宽度（`MinWidth`），那是另一套取舍，等真的遇到超宽表再说。
func (a *App) renderTable(c *ui.Context, block markdown.Block) {
	theme := c.Theme()
	columns := len(block.Header)
	if columns == 0 {
		return
	}
	aligns := block.Aligns
	cellAlign := func(index int) ui.Align {
		if index < len(aligns) {
			switch aligns[index] {
			case "center":
				return ui.Center
			case "right":
				return ui.End
			}
		}
		return ui.Start
	}

	ui.Grid(c).Columns(columns).Gap(6).FillWidth().Children(func() {
		for index, cell := range block.Header {
			ui.Column(c).MinWidth(0).JustifySelf(cellAlign(index)).Children(func() {
				ui.RichText(c, a.spans(cell, theme)...).Bold().FontSize(13).
					TextColor(theme.Text)
			})
		}
		// 表头分隔线（占满所有列）
		ui.Box(c).Height(1).FillWidth().Background(theme.Border).ColumnSpan(-1)
		for _, row := range block.Rows {
			for index := 0; index < columns; index++ {
				var cell []markdown.Span
				if index < len(row) {
					cell = row[index]
				}
				ui.Column(c).MinWidth(0).JustifySelf(cellAlign(index)).Children(func() {
					ui.RichText(c, a.spans(cell, theme)...).FontSize(13).
						TextColor(theme.Text)
				})
			}
		}
	})
}
