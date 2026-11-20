package uiapp_test

import (
	"testing"
)

// 草稿必须跟着会话走：在 A 里打的字切到 B 不该跟过去。
//
// 回归（用户报的）：a.draft 原来是 App 上一个全局字符串，切会话时没人动它，
// 于是 A 会话打了一半的字会原封不动出现在 B 的输入框里。
func TestDraftIsPerSession(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)

	first := app.Current()
	first.Title = "会话 A"
	second := app.NewSession()
	second.Title = "会话 B"
	pump(tester, p)
	pump(tester, p)

	// 在 A 里写点东西
	app.SelectSessionForTest(first.ID)
	app.SetDraftForTest("这是 A 的草稿")

	// 切到 B：输入框应该是空的
	app.SelectSessionForTest(second.ID)
	if got := app.DraftForTest(); got != "" {
		t.Fatalf("切到 B 后不该看到 A 的草稿，实际 %q", got)
	}

	// 在 B 里写点东西
	app.SetDraftForTest("这是 B 的草稿")

	// 切回 A：应恢复 A 自己的草稿，而不是 B 的
	app.SelectSessionForTest(first.ID)
	if got := app.DraftForTest(); got != "这是 A 的草稿" {
		t.Fatalf("切回 A 应恢复它自己的草稿，实际 %q", got)
	}

	// 再切回 B
	app.SelectSessionForTest(second.ID)
	if got := app.DraftForTest(); got != "这是 B 的草稿" {
		t.Fatalf("切回 B 应恢复它自己的草稿，实际 %q", got)
	}
}

// 新建会话不该继承上一个会话的草稿。
func TestNewSessionHasEmptyDraft(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	pump(tester, p)

	app.SetDraftForTest("上一轮没发出去的内容")
	created := app.NewSession()
	if created == nil {
		t.Fatal("新建会话失败")
	}
	pump(tester, p)
	pump(tester, p)

	app.SelectSessionForTest(created.ID)
	if got := app.DraftForTest(); got != "" {
		t.Fatalf("新会话的输入框应为空，实际 %q", got)
	}
}

// 发送成功后要清掉会话自己存的那份草稿，否则切走再切回来，
// 已经发出去的内容会"复活"到输入框里。
func TestSendClearsSessionDraft(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	session := app.Current()
	session.Title = "发消息的会话"
	pump(tester, p)

	clickComposer(t, tester)
	tester.Type("发出去的内容")
	tester.Frame()
	if err := tester.Click(locSend); err != nil {
		t.Fatalf("点不到发送：%v", err)
	}
	pump(tester, p)

	// 造一个旁观会话，切走再切回来
	other := app.NewSession()
	other.Title = "旁观会话"
	pump(tester, p)
	pump(tester, p)

	app.SelectSessionForTest(other.ID)
	app.SelectSessionForTest(session.ID)

	if got := app.DraftForTest(); got != "" {
		t.Fatalf("发送成功后切回来不该看到已发出的内容，实际 %q", got)
	}
}

// 切会话时用户**当前正在写**的内容不能丢（这是最主要的回归点）。
func TestSwitchingSavesCurrentDraft(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	first := app.Current()
	first.Title = "一号"
	second := app.NewSession()
	second.Title = "二号"
	pump(tester, p)
	pump(tester, p)

	app.SelectSessionForTest(first.ID)
	app.SetDraftForTest("还没写完的一句话")

	// 切走
	app.SelectSessionForTest(second.ID)
	// 切回：内容必须还在
	app.SelectSessionForTest(first.ID)
	if got := app.DraftForTest(); got != "还没写完的一句话" {
		t.Fatalf("切走再切回，草稿不该丢，实际 %q", got)
	}
}

// 通过**侧栏点击**切换也要生效（这是用户实际操作的路径，
// 与 SelectSessionForTest 走的是同一个 switchTo，但要确认侧栏那条路接上了）。
func TestSidebarClickKeepsDraftsSeparate(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	first := app.Current()
	first.Title = "侧栏甲"
	second := app.NewSession()
	second.Title = "侧栏乙"
	pump(tester, p)
	pump(tester, p)

	app.SelectSessionForTest(first.ID)
	app.SetDraftForTest("甲的草稿")
	pump(tester, p)

	// 点侧栏里的"侧栏乙"
	if err := tester.Click("侧栏乙"); err != nil {
		t.Fatalf("点不到会话：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)
	if got := app.DraftForTest(); got != "" {
		t.Fatalf("点侧栏切到乙后不该看到甲的草稿，实际 %q", got)
	}

	// 点回甲
	if err := tester.Click("侧栏甲"); err != nil {
		t.Fatalf("点不到会话：%v", err)
	}
	pump(tester, p)
	if got := app.DraftForTest(); got != "甲的草稿" {
		t.Fatalf("点回甲应恢复它的草稿，实际 %q", got)
	}
}