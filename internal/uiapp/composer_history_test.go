package uiapp_test

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/message"
)

// 这组测试固定「输入框上下箭头翻当前会话历史」的语义：
//
//   - ↑/↓ 只从光标所在边界翻（第一行 ↑、最后一行 ↓），多行中间交给编辑器；
//   - 翻历史前暂存草稿，↓ 翻过最新一条后恢复；
//   - 输入法组合期间不认领；
//   - 历史按会话独立，切换会话后翻到的是新会话自己的内容；
//   - 翻出来的内容可以直接按 Enter 发送。

// userMsg 构造一条 user 消息（直接塞进会话，模拟"以前输入过的话"）。
func userMsg(id, text string) message.Message {
	return message.Message{MessageID: id, Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart(text)}}
}

// lastUserText 返回请求里最后一条 user 消息的文本。
func lastUserText(messages []message.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == message.RoleUser {
			return messages[index].PreviewText()
		}
	}
	return ""
}

// ↑ 依次翻出上一条、上上一条；↓ 往回翻，最后恢复原来的草稿。
func TestComposerHistoryArrows(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	_ = rt

	if _, err := app.Current().Conversation().AddMessage(userMsg("u1", "第一句"), "complete"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Current().Conversation().AddMessage(userMsg("u2", "第二句"), "complete"); err != nil {
		t.Fatal(err)
	}

	clickComposer(t, tester)
	tester.Type("写了一半")
	pump(tester, p)
	// 光标不在开头时 ↑ 交给编辑器（长单行可能自动折行，先保守）；Home 后才会翻
	tester.Key(0, ui.KeyHome)
	pump(tester, p)

	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if got := app.DraftForTest(); got != "第二句" {
		t.Fatalf("第一次 ↑ 应翻出最近一条，实际 %q", got)
	}
	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if got := app.DraftForTest(); got != "第一句" {
		t.Fatalf("第二次 ↑ 应翻出更早一条，实际 %q", got)
	}
	// 已到最早一条，再 ↑ 保持不动
	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if got := app.DraftForTest(); got != "第一句" {
		t.Fatalf("到最早一条应停住，实际 %q", got)
	}

	tester.Key(0, ui.KeyDown)
	pump(tester, p)
	if got := app.DraftForTest(); got != "第二句" {
		t.Fatalf("↓ 应回到较新一条，实际 %q", got)
	}
	tester.Key(0, ui.KeyDown)
	pump(tester, p)
	if got := app.DraftForTest(); got != "写了一半" {
		t.Fatalf("翻过最新一条后应恢复原草稿，实际 %q", got)
	}
	// 已经回到草稿，再 ↓ 不该有变化（交还编辑器）
	tester.Key(0, ui.KeyDown)
	pump(tester, p)
	if got := app.DraftForTest(); got != "写了一半" {
		t.Fatalf("回到草稿后 ↓ 不应改变内容，实际 %q", got)
	}
}

// 多行草稿：只有光标在第一行（↑）/最后一行（↓）才翻历史，中间行交给编辑器。
func TestComposerHistoryOnlyAtDraftEdges(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	if _, err := app.Current().Conversation().AddMessage(userMsg("u1", "历史一"), "complete"); err != nil {
		t.Fatal(err)
	}
	clickComposer(t, tester)
	// 用 Shift+Enter 插入换行（编辑器原生行为，光标会落在新行开头）
	tester.Type("第一行")
	tester.Key(ui.Shift, ui.KeyEnter)
	pump(tester, p)
	tester.Type("第二行")
	pump(tester, p)
	if app.DraftForTest() != "第一行\n第二行" {
		t.Fatalf("前置条件失败：草稿应是两行，实际 %q", app.DraftForTest())
	}

	// 光标在最后一行（末尾）：↑ 交给编辑器（往上移一行），不翻历史
	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if strings.Contains(app.DraftForTest(), "历史一") {
		t.Fatalf("光标不在第一行时不应翻历史，实际 %q", app.DraftForTest())
	}
	// 光标在第一行但不在开头（Home 到行首前先 End）：仍然交给编辑器
	tester.Key(0, ui.KeyHome)
	pump(tester, p)
	tester.Key(0, ui.KeyEnd)
	pump(tester, p)
	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if strings.Contains(app.DraftForTest(), "历史一") {
		t.Fatalf("第一行但光标不在开头时不应翻历史，实际 %q", app.DraftForTest())
	}
	// 光标回到 0：↑ 才翻历史
	tester.Key(0, ui.KeyHome)
	pump(tester, p)
	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if app.DraftForTest() != "历史一" {
		t.Fatalf("光标在第一行开头时 ↑ 应翻历史，实际 %q", app.DraftForTest())
	}
}

// 输入法组合期间 ↑/↓ 归输入法（选候选），不翻历史。
func TestComposerHistoryIgnoredWhileComposing(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	if _, err := app.Current().Conversation().AddMessage(userMsg("u1", "历史一"), "complete"); err != nil {
		t.Fatal(err)
	}
	clickComposer(t, tester)
	tester.Type("ni")
	tester.Compose("hào", 2)
	pump(tester, p)

	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if got := app.DraftForTest(); strings.Contains(got, "历史一") {
		t.Fatalf("组合期间不应翻历史，实际 %q", got)
	}
}

// 切换会话后翻到的是新会话自己的历史（旧会话的内容不串过来）。
func TestComposerHistoryIsPerSession(t *testing.T) {
	app, _, p := newApp(t)
	app.Current().Title = "旧会话"
	if _, err := app.Current().Conversation().AddMessage(userMsg("u1", "旧会话的话"), "complete"); err != nil {
		t.Fatal(err)
	}

	// 第二个会话：历史是另一句
	app.NewSession()
	app.Current().Title = "新会话"
	if _, err := app.Current().Conversation().AddMessage(userMsg("u2", "新会话的话"), "complete"); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	// 新会话里 ↑ → 只能翻到它自己的
	clickComposer(t, tester)
	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if got := app.DraftForTest(); got != "新会话的话" {
		t.Fatalf("应翻出新会话自己的历史，实际 %q", got)
	}

	// 切回旧会话再 ↑ → 位置与暂存草稿都重置，翻到旧会话的历史
	app.SetDraftForTest("")
	if err := tester.Click("旧会话"); err != nil {
		t.Fatalf("点不到旧会话：%v", err)
	}
	pump(tester, p)
	clickComposer(t, tester)
	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if got := app.DraftForTest(); got != "旧会话的话" {
		t.Fatalf("切换会话后应翻到该会话自己的历史，实际 %q", got)
	}
}

// 翻出某一条后按 Enter：发送的就是这条文本。
func TestComposerHistoryEntryCanBeSent(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	if _, err := app.Current().Conversation().AddMessage(userMsg("u1", "要重发的句子"), "complete"); err != nil {
		t.Fatal(err)
	}
	clickComposer(t, tester)
	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if got := app.DraftForTest(); got != "要重发的句子" {
		t.Fatalf("应翻出历史，实际 %q", got)
	}
	tester.Key(0, ui.KeyEnter)
	pump(tester, p)

	waitFor(t, tester, p, "发送翻出来的内容", func() bool {
		return rt.streamCount() == 1
	})
	last := lastUserText(rt.requests[0].Messages)
	if last != "要重发的句子" {
		t.Fatalf("请求里最后一条应是翻出来的文本，实际 %q", last)
	}
}

// 翻出历史后光标在**末尾**：继续输入应接在后面，而不是插到开头。
func TestComposerHistoryCaretAtEnd(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	if _, err := app.Current().Conversation().AddMessage(userMsg("u1", "历史句子"), "complete"); err != nil {
		t.Fatal(err)
	}
	clickComposer(t, tester)
	pump(tester, p)
	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if got := app.DraftForTest(); got != "历史句子" {
		t.Fatalf("应翻出历史，实际 %q", got)
	}
	tester.Type("……")
	pump(tester, p)
	if got := app.DraftForTest(); got != "历史句子……" {
		t.Fatalf("光标应在末尾，实际 %q", got)
	}
}

// 翻出来的条目被改过之后，再按 ↑/↓ 不能把改动丢掉：改后的文本成为新草稿。
func TestComposerHistoryEditedEntryBecomesDraft(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	for _, entry := range [][2]string{{"u1", "第一句"}, {"u2", "第二句"}} {
		if _, err := app.Current().Conversation().AddMessage(userMsg(entry[0], entry[1]), "complete"); err != nil {
			t.Fatal(err)
		}
	}

	clickComposer(t, tester)
	tester.Key(0, ui.KeyUp) // 草稿为空：直接翻到最近一条
	pump(tester, p)
	if got := app.DraftForTest(); got != "第二句" {
		t.Fatalf("应翻出最近一条，实际 %q", got)
	}

	// 改两个字
	tester.Type("（改过）")
	pump(tester, p)
	if got := app.DraftForTest(); got != "第二句（改过）" {
		t.Fatalf("前置条件失败：%q", got)
	}

	// 再按 ↓：不该跳走，也不该恢复成更早的暂存草稿
	tester.Key(0, ui.KeyDown)
	pump(tester, p)
	if got := app.DraftForTest(); got != "第二句（改过）" {
		t.Fatalf("改过的内容不能被丢掉，实际 %q", got)
	}

	// 再按 ↑（先把光标挪到行首）：改后的文本成为暂存草稿，翻出最近一条
	tester.Key(0, ui.KeyHome)
	pump(tester, p)
	tester.Key(0, ui.KeyUp)
	pump(tester, p)
	if got := app.DraftForTest(); got != "第二句" {
		t.Fatalf("↑ 应翻出最近一条，实际 %q", got)
	}
	tester.Key(0, ui.KeyDown)
	pump(tester, p)
	if got := app.DraftForTest(); got != "第二句（改过）" {
		t.Fatalf("↓ 应回到改后的草稿，实际 %q", got)
	}
}
