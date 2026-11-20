package uiapp

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/tools"
)

// diff 渲染：按行号对齐的两列布局，删除行标 -、新增行标 +。
//
// 形态对齐常见 diff 工具：
//
//	  24       function Foo()
//	  25           self:SetBarExpanded(false)
//	 -26
//	 -27           self.UI.btnExit.click = ...
//	 +26           self.UI.btnExpand.click = ...
//	  28       end
//
// 对齐靠两点：
//  1. **等宽字体**（dshCodeFontStack）—— 行号列与代码列才不会错位
//  2. **固定宽度的行号列**（按最大行号的位数算）—— 2 位数和 3 位数不会把代码推歪

// diffGutterWidth 是行号列的字符宽度上限。
//
// 取 4：四位数行号（9999）覆盖绝大多数文件，再多就太占地方。
const diffGutterWidth = 4

// diffLineHeight 是每一行的高度（紧凑排布，避免 diff 撑得太高）。
const diffLineHeight = 16

// renderDiff 渲染一个文件的差异。
func (a *App) renderDiff(c *ui.Context, diff *tools.FileDiff, theme *ui.Theme) {
	if diff == nil || len(diff.Hunks) == 0 {
		return
	}
	ui.Column(c).FillWidth().Gap(0).Children(func() {
		// 文件头：文件名 + 增删统计
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Text(c, diff.Path).Font(dshCodeFontStack).FontSize(12).
				TextColor(theme.Text)
			ui.Text(c, fmt.Sprintf("+%d", diff.Added)).Font(dshCodeFontStack).
				FontSize(11).TextColor(diffAddText(theme))
			ui.Text(c, fmt.Sprintf("-%d", diff.Removed)).Font(dshCodeFontStack).
				FontSize(11).TextColor(diffDelText(theme))
		})
		for _, hunk := range diff.Hunks {
			a.renderDiffHunk(c, hunk, theme)
		}
		if diff.Truncated {
			ui.Text(c, "… 差异过大，只显示前面部分").
				FontSize(11).TextColor(theme.TextMuted).Padding(2, 0)
		}
	})
}

// renderDiffHunk 渲染一个差异块。
func (a *App) renderDiffHunk(c *ui.Context, hunk tools.DiffHunk, theme *ui.Theme) {
	ui.Column(c).FillWidth().Children(func() {
		for _, line := range hunk.Lines {
			a.renderDiffLine(c, line, theme)
		}
	})
}

// renderDiffLine 渲染一行。
//
// **整行拼成一个字符串**用一个等宽 Text 渲染，而不是拆成多个 Text 元素，
// 原因：Row 里的多个 Text 各自按内容宽度排版，补的空格会被压缩，
// 行号和代码就挤在一起（实测「6 6 self.UI...」「9end」）。
// 拼成一个字符串交给等宽字体，列对齐是天然保证的。
func (a *App) renderDiffLine(c *ui.Context, line tools.DiffLine, theme *ui.Theme) {
	mark := " "
	var background ui.Color
	textColor := theme.TextMuted
	switch line.Kind {
	case "del":
		mark = "-"
		background = diffDelBg(theme)
		textColor = diffDelText(theme)
	case "add":
		mark = "+"
		background = diffAddBg(theme)
		textColor = diffAddText(theme)
	case "ctx":
		textColor = theme.TextMuted
	}
	// 列：旧行号(4) 空格 新行号(4) 空格 标记(1) 空格 代码
	text := formatDiffLineNo(line.OldLine) + " " +
		formatDiffLineNo(line.NewLine) + " " + mark + " " + line.Text
	// 背景铺满整行：外层 Row FillWidth + 底色，Text 只负责文字。
	// 直接给 Text 上底色只会覆盖文字本身那么宽（实测色块到句尾就断了）。
	row := ui.Row(c).FillWidth().AlignItems(ui.Center)
	if background != (ui.Color{}) {
		row.Background(background)
	}
	row.Children(func() {
		ui.Text(c, text).Font(dshCodeFontStack).FontSize(12).
			TextColor(textColor).MaxLines(1).Padding(1, 0)
	})
}

// formatDiffLineNo 把行号格式化成**固定宽度**的字符串（右对齐补空格）。
//
// 这是对齐的核心：2 行号和 3 行号占同样的字符数，代码列才不会参差。
func formatDiffLineNo(n int) string {
	if n <= 0 {
		return strings.Repeat(" ", diffGutterWidth)
	}
	text := fmt.Sprintf("%d", n)
	if len(text) >= diffGutterWidth {
		return text
	}
	return strings.Repeat(" ", diffGutterWidth-len(text)) + text
}

// diff 配色：浅色底 + 更深的文字，保证可读又不刺眼。
func diffAddBg(theme *ui.Theme) ui.Color {
	return ui.Hex("#E6F4EA")
}

func diffDelBg(theme *ui.Theme) ui.Color {
	return ui.Hex("#FCE8E6")
}

func diffAddText(theme *ui.Theme) ui.Color {
	return ui.Hex("#1E7B34")
}

func diffDelText(theme *ui.Theme) ui.Color {
	return ui.Hex("#B3261E")
}