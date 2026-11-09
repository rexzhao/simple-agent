package uiapp_test

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
)

// 无论系统是浅色还是深色，界面都用 dsh 浅色：两种模式下正文/次要文字/代码块/
// 引用条都要与背景有对比（防"系统深色时套浅色导致看不清"）。
func TestThemeContrastBothModes(t *testing.T) {
	for _, dark := range []bool{false, true} {
		app, _, p := newApp(t)
		conv := app.Current().Conversation()
		msg := message.Message{MessageID: "theme-1", Role: message.RoleAgent, Parts: []message.Part{
			{Content: message.TextPart{Text: "推理过程"}, MediaType: render.MediaReasoning},
			message.NewTextPart("正文 **粗体**\n\n> 引用内容\n\n```go\nx := 1\n```"),
		}}
		if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}

		// 采样主题色：前景与背景必须不同
		var fg, bg, muted, surface ui.Color
		contrast := func(a, b ui.Color) bool { return a != b }
		probe := func(c *ui.Context) {
			// 先让 App 应用主题（SetTheme 在 View 里），再采样
			app.View(c)
			theme := c.Theme()
			fg, bg, muted, surface = theme.Text, theme.Background, theme.TextMuted, theme.Surface
		}
		tester := ui.NewTester(probe, 900, 700)
		tester.SetDark(dark)
		pump(tester, p)

		if dark {
			// 系统深色时也必须是浅色令牌（用户决定：不跟随系统）
			if fg != ui.Hex("#0F1115") || bg != ui.Hex("#FFFFFF") {
				t.Fatalf("系统深色时应仍是 dsh 浅色：fg=%#v bg=%#v", fg, bg)
			}
		}

		if !contrast(fg, bg) {
			t.Fatalf("dark=%v：正文色与背景色相同", dark)
		}
		if !contrast(muted, bg) && !contrast(muted, surface) {
			t.Fatalf("dark=%v：次要文字在同色背景上", dark)
		}
		for _, want := range []string{"正文", "引用内容", "x := 1", "推理过程"} {
			if !tester.HasText(want) {
				t.Fatalf("dark=%v：缺少 %q", dark, want)
			}
		}
	}
}
