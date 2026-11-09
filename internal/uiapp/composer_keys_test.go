package uiapp_test

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/uiapp"
)

// 这组测试固定 composer 的键位语义（M4 探针的结论，G2）。
//
// 结论来源：mygo 的 multiline 编辑器对 Enter 的条件是 `m == 0`（Shift 已被剥掉），
// 带修饰键的 Enter 根本进不了编辑器；外层 Shortcut 抢不过编辑器。唯一可行的
// 截获点是焦点元素的 HandleInput，它在编辑器之前拿到 KeyDown。

// newComposerTester 构造一个只有输入框的视图。
func newComposerTester(t *testing.T, draft *string, onSend func()) *ui.Tester {
	t.Helper()
	pending := -1
	view := func(c *ui.Context) {
		composer := &uiapp.Composer{
			Draft: draft, Send: onSend,
			RequestCaret: func(pos int) { pending = pos },
		}
		ui.Column(c).Children(func() {
			area := ui.TextArea(c, draft).Lines(1, 6)
			composer.Area = area
			// 上一帧请求的光标位置：此刻编辑器已同步完文本
			if pending >= 0 {
				area.SetTextSelection(pending, pending)
				pending = -1
			}
			area.HandleInput(composer.HandleInput)
		})
	}
	tester := ui.NewTester(view, 600, 320)
	tester.ClickAt(12, 12) // 聚焦输入框
	tester.Frame()
	return tester
}

// Enter 发送：按键被认领，编辑器不插入换行。
func TestComposerEnterSends(t *testing.T) {
	draft := ""
	sent := 0
	tester := newComposerTester(t, &draft, func() { sent++ })

	tester.Type("你好")
	tester.Frame()
	tester.Key(0, ui.KeyEnter)
	tester.Frame()

	if sent != 1 {
		t.Fatalf("Enter 应触发发送，实际 %d 次", sent)
	}
	if strings.Contains(draft, "\n") {
		t.Fatalf("发送用的 Enter 不应进文本：%q", draft)
	}
}

// Shift+Enter 换行（编辑器原生行为，我们不认领）。
func TestComposerShiftEnterNewlines(t *testing.T) {
	draft := ""
	sent := 0
	tester := newComposerTester(t, &draft, func() { sent++ })

	tester.Type("第一行")
	tester.Frame()
	tester.Key(ui.Shift, ui.KeyEnter)
	tester.Frame()
	tester.Type("第二行")
	tester.Frame()

	if sent != 0 {
		t.Fatalf("Shift+Enter 不应发送，实际 %d 次", sent)
	}
	if draft != "第一行\n第二行" {
		t.Fatalf("Shift+Enter 应插入换行：%q", draft)
	}
}

// Alt+Enter 在光标处插入换行（这是用户要求的语义）。
func TestComposerAltEnterInsertsAtCaret(t *testing.T) {
	draft := "hello world"
	sent := 0
	tester := newComposerTester(t, &draft, func() { sent++ })

	// 光标移到 "hello " 之后（末尾左移 5）
	tester.Key(0, ui.KeyEnd)
	tester.Frame()
	for i := 0; i < 5; i++ {
		tester.Key(0, ui.KeyLeft)
		tester.Frame()
	}
	tester.Key(ui.Alt, ui.KeyEnter)
	tester.Frame()

	if sent != 0 {
		t.Fatalf("Alt+Enter 不应发送，实际 %d 次", sent)
	}
	if draft != "hello \nworld" {
		t.Fatalf("换行应插在光标处：%q", draft)
	}

	// 光标应落在换行之后：继续输入接在换行后
	tester.Type("X")
	tester.Frame()
	if draft != "hello \nXworld" {
		t.Fatalf("换行后光标位置不对：%q", draft)
	}
}

// Alt+Enter 之后**接着打字**必须落在换行之后（M4 遗留缺陷：光标被旧长度截断）。
func TestComposerAltEnterCaretAfterNewline(t *testing.T) {
	draft := "第一行"
	sent := 0
	tester := newComposerTester(t, &draft, func() { sent++ })
	tester.Key(0, ui.KeyEnd) // 光标放到末尾（草稿里已有一行文字）
	tester.Frame()

	tester.Key(ui.Alt, ui.KeyEnter)
	tester.Frame()
	if draft != "第一行\n" {
		t.Fatalf("应先插入换行：%q", draft)
	}
	tester.Type("第二行")
	tester.Frame()
	if draft != "第一行\n第二行" {
		t.Fatalf("换行后打字应接在新行，实际 %q", draft)
	}
	if sent != 0 {
		t.Fatalf("不应发送，实际 %d 次", sent)
	}
}

// Alt+Enter 有选区时：换行替换选中的内容（与常见编辑器一致）。
func TestComposerAltEnterReplacesSelection(t *testing.T) {
	draft := "abcdef"
	tester := newComposerTester(t, &draft, func() {})

	tester.Key(0, ui.KeyHome)
	tester.Frame()
	tester.Key(0, ui.KeyRight)
	tester.Frame()

	// 用 Shift+方向键选中 bc（HoldModifiers 只对指针事件生效，键盘要每次带修饰键）
	tester.Key(ui.Shift, ui.KeyRight)
	tester.Frame()
	tester.Key(ui.Shift, ui.KeyRight)
	tester.Frame()

	tester.Key(ui.Alt, ui.KeyEnter)
	tester.Frame()

	if draft != "a\ndef" {
		t.Fatalf("选区应被换行替换：%q", draft)
	}
}

// 输入法组合期间 Alt+Enter 不认领，组合不被破坏。
func TestComposerAltEnterIgnoredWhileComposing(t *testing.T) {
	draft := ""
	sent := 0
	tester := newComposerTester(t, &draft, func() { sent++ })

	tester.Compose("zhong", 5)
	tester.Frame()
	tester.Key(ui.Alt, ui.KeyEnter)
	tester.Frame()
	if strings.Contains(draft, "\n") {
		t.Fatalf("组合期间不应插入换行：%q", draft)
	}

	tester.Type("中")
	tester.Frame()
	if draft != "中" || sent != 0 {
		t.Fatalf("组合提交应正常且不发送：draft=%q sent=%d", draft, sent)
	}
}

// emoji / 组合字形：换行不能被插进字形的中间。
func TestComposerAltEnterKeepsGraphemes(t *testing.T) {
	// 🙂 是代理对、🇨🇳 是 ZWJ 组合、é 是 e + 组合重音
	draft := "a🙂🇨🇳e\u0301z"
	tester := newComposerTester(t, &draft, func() {})

	tester.Key(0, ui.KeyHome)
	tester.Frame()
	// 方向键按字形移动：a、🙂、🇨🇳、é → 光标在 z 之前
	for i := 0; i < 4; i++ {
		tester.Key(0, ui.KeyRight)
		tester.Frame()
	}
	tester.Key(ui.Alt, ui.KeyEnter)
	tester.Frame()

	if draft != "a🙂🇨🇳e\u0301\nz" {
		t.Fatalf("换行应插在字形边界：%q", draft)
	}
}

// 撤销：换行不是独立撤销步骤（与编辑器把连续输入合并成一条的行为一致），
// 但撤销后不应丢失换行之前就已存在的内容。
func TestComposerAltEnterUndoKeepsEarlierText(t *testing.T) {
	draft := "abc"
	tester := newComposerTester(t, &draft, func() {})

	tester.Key(0, ui.KeyEnd)
	tester.Frame()
	tester.Type("d")
	tester.Frame()
	tester.Key(ui.Alt, ui.KeyEnter)
	tester.Frame()
	if draft != "abcd\n" {
		t.Fatalf("前置条件失败：%q", draft)
	}

	tester.Key(ui.Cmd, ui.KeyZ)
	tester.Frame()
	if !strings.HasPrefix(draft, "abc") {
		t.Fatalf("撤销不应丢失换行之前已有的文本：%q", draft)
	}

	// 重做能回到换行后的状态
	tester.Key(ui.Cmd, ui.KeyY)
	tester.Frame()
	if draft != "abcd\n" {
		t.Fatalf("重做应恢复换行：%q", draft)
	}
}

// 高 DPI 下按键语义不变（scale=2）。
func TestComposerKeysAtHighDPI(t *testing.T) {
	draft := ""
	sent := 0
	view := func(c *ui.Context) {
		composer := &uiapp.Composer{Draft: &draft, Send: func() { sent++ }}
		ui.Column(c).Children(func() {
			area := ui.TextArea(c, &draft).Lines(1, 6)
			composer.Area = area
			area.HandleInput(composer.HandleInput)
		})
	}
	tester := ui.NewTester(view, 600, 320)
	tester.SetScale(2)
	tester.Frame()
	tester.ClickAt(12, 12)
	tester.Frame()

	tester.Type("hi")
	tester.Frame()
	tester.Key(ui.Alt, ui.KeyEnter)
	tester.Frame()
	if draft != "hi\n" {
		t.Fatalf("scale=2 下 Alt+Enter 失效：%q", draft)
	}
	tester.Key(0, ui.KeyEnter)
	tester.Frame()
	if sent != 1 {
		t.Fatalf("scale=2 下 Enter 未发送：%d", sent)
	}
}
