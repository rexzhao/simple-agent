package uiapp_test

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
)

// 列表的编号/圆点要和首行文字**顶端对齐**。
//
// 这是一个真实出现过的缺陷（截图里编号飘到了两行文字的中间）：`ui.Row` 的默认
// 交叉轴对齐是 `Center`，所以编号相对于整块（多行）正文垂直居中。修法是显式
// `AlignItems(ui.Start)`，这两个测试守着它。
func TestListMarkerAlignsWithFirstLine(t *testing.T) {
	app, _, p := newApp(t)
	// 故意用一句长到会折行的正文：单行的话 Center 和 Start 看起来一样
	body := "这是一条很长很长的列表正文，长到在当前窗口宽度下必然会折成两行以上，" +
		"用来验证编号是不是跟首行对齐。"
	msg := message.Message{MessageID: "list-1", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("1. " + body + "\n2. " + body + "\n3. " + body),
	}}
	if _, err := app.Current().Conversation().AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)

	for _, marker := range []string{"1.", "2.", "3."} {
		markerRect, ok := tester.Find(marker)
		if !ok {
			t.Fatalf("找不到编号 %q：%v", marker, tester.Texts())
		}
		// 编号必须是**单行**的：它自己的高度不该因为正文折行而被撑高。
		// 这正是 Center 对齐下的问题——编号被放大/居中到整块高度。
		if markerRect.H > lineHeightBound {
			t.Fatalf("编号 %q 的高度 %v 超过一行（说明它被拉伸/居中了，不只是首行对齐）：%v",
				marker, markerRect.H, markerRect)
		}
	}
}

// 编号的顶端要和同一项正文的顶端基本齐平（允许 2px 的字形基线误差）。
func TestListMarkerTopAlignedWithText(t *testing.T) {
	app, _, p := newApp(t)
	body := "这一项故意写得很长很长，长到在 900 宽的测试窗口里必然折成多行，" +
		"因为只有当正文不止一行时「编号是贴首行还是被垂直居中」才区分得出来。" +
		"再补一些字确保它真的折行，不然这个测试等于没测。"
	msg := message.Message{MessageID: "list-2", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("1. " + body),
	}}
	if _, err := app.Current().Conversation().AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)

	// 正文是一整段 RichText（折行发生在它内部，没有「首行」子元素），
	// 所以拿它的矩形顶端当基准：段落是多行，用 Center 对齐时编号会被压到中间。
	bodyRect, ok := tester.Find(body)
	if !ok {
		t.Fatalf("找不到正文：%v", tester.Texts())
	}
	markerRect, ok := tester.Find("1.")
	if !ok {
		t.Fatalf("找不到编号：%v", tester.Texts())
	}
	if bodyRect.H <= lineHeightBound {
		t.Fatalf("这段正文应该折行了才有意义（当前高 %v，窗口太宽？）", bodyRect.H)
	}
	if diff := markerRect.Y - bodyRect.Y; diff > topAlignTolerance || diff < -topAlignTolerance {
		t.Fatalf("编号未与首行顶端对齐：编号 Y=%v、正文 Y=%v（相差 %v）；"+
			"正文高 %v，说明编号被垂直居中了", markerRect.Y, bodyRect.Y, diff, bodyRect.H)
	}
}

// 两位数编号（9. / 10.）的正文左边界要对齐，不能随位数左右跳。
func TestListMarkerWidthIsUniform(t *testing.T) {
	app, _, p := newApp(t)
	msg := message.Message{MessageID: "list-3", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("9. 第九项\n10. 第十项\n11. 第十一项"),
	}}
	if _, err := app.Current().Conversation().AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)

	nine, ok := tester.Find("第九项")
	if !ok {
		t.Fatalf("找不到第九项：%v", tester.Texts())
	}
	ten, ok := tester.Find("第十项")
	if !ok {
		t.Fatalf("找不到第十项：%v", tester.Texts())
	}
	if diff := nine.X - ten.X; diff > 1 || diff < -1 {
		t.Fatalf("各项正文左边界应对齐：第九项 X=%v、第十项 X=%v", nine.X, ten.X)
	}
}

const (
	// lineHeightBound 是 13px 正文的「约一行高」上界（行高 1.6 → 约 21px，
	// 留一点余量给字体度量差异）。
	lineHeightBound = 26
	// topAlignTolerance 是编号与首行顶端允许的像素误差（字形基线差异）。
	topAlignTolerance = 2
)
