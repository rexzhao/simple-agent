package uiapp

import (
	"strings"

	"github.com/egoist/mygo/ui"
)

// slashHintsPanel 在输入框内容以 / 开头时，浮出一张候选命令列表。
//
// 交互：点一条即把命令补全进输入框（并带上空格，方便接着打参数）。
// 键盘选择（↑↓ + Tab）留到以后：现在候选最多几条，点击已经够用，
// 而键盘事件要和输入框的光标逻辑配合，改动面更大。
func (a *App) slashHintsPanel(c *ui.Context, theme *ui.Theme) {
	prefix, ok := a.slashPrefix()
	if !ok {
		return
	}
	matches := matchCommands(prefix)
	if len(matches) == 0 {
		// 输入了 / 但没有匹配：给一条明确提示，避免用户以为卡住了
		a.slashHintShell(c, theme, func() {
			ui.Text(c, "没有匹配的命令，输入 /help 查看全部").
				FontSize(12).TextColor(theme.TextMuted)
		})
		return
	}
	// 只有一个匹配且用户已经把命令名打全了：不用再挡着视野
	if len(matches) == 1 && strings.EqualFold(matches[0].Name, prefix) {
		return
	}

	a.slashHintShell(c, theme, func() {
		for _, command := range matches {
			command := command
			row := ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).
				Padding(3, 6).Radius(6).Cursor(ui.CursorPointer).Children(func() {
				ui.Text(c, "/"+command.Usage()).FontSize(12).TextColor(theme.Text)
				ui.Text(c, command.Help).FontSize(11).TextColor(theme.TextMuted).
					MaxLines(1).Grow(1)
			})
			if row.Hovered() {
				row.Background(theme.SurfaceHover)
			}
			if row.Clicked() {
				// 补全到命令名 + 一个空格（有帮助参数就直接带出来，省得记语法）
				a.draft = "/" + command.Name + " "
				a.refocusComposer = true
			}
		}
	})
}

// slashHintShell 是候选面板的外壳（与输入卡同宽居中、同色系）。
func (a *App) slashHintShell(c *ui.Context, theme *ui.Theme, body func()) {
	ui.Column(c).FillWidth().Padding(chatSidePadding, 2).Children(func() {
		ui.Column(c).MaxWidth(a.contentWidth()).AlignSelf(ui.Center).FillWidth().
			Background(theme.Surface).Border(1, theme.Border).Radius(8).
			Padding(4, 6).Gap(2).Children(body)
	})
}

// slashPrefix 判断当前草稿是不是在打斜杠命令，返回命令名前缀。
//
// 只在"还没打参数"时提示：一旦出现空格，说明用户已经在写参数，再弹候选
// 会盖住输入内容。
func (a *App) slashPrefix() (string, bool) {
	// 注意：这里不能用 TrimSpace 之后的文本判断空格 —— 用户刚打完 "/connect "
	// 时末尾就是个空格，TrimSpace 会把它吃掉，于是又弹出候选，很别扭。
	// 只在左侧裁掉空白（容错"误打前导空格"），右侧原样保留。
	text := strings.TrimLeft(a.draft, " \t")
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	rest := strings.TrimPrefix(text, "/")
	if strings.ContainsAny(rest, " \t\n") {
		return "", false
	}
	return rest, true
}
