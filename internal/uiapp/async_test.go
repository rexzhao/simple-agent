package uiapp_test

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/driver"
	"simple-agent/internal/locale"
	"simple-agent/internal/providers"
	"simple-agent/internal/uiapp"
)

// newDeferredApp 建一个"命令延后执行"的 app：Submit 返回时不代表已经生效，
// 失败只能经 cmdResult 回来——这就是 remoteHost 的语义（P2 前哨）。
func newDeferredApp(t *testing.T) (*uiapp.App, *fakeRuntime, *poster) {
	t.Helper()
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:             "你是助手",
		Workspace:                t.TempDir(),
		Title:                    "测试会话",
		MaxToolRounds:            3,
		ContextWindow:            testContextWindow,
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
		Post:                     p.Post,
		DeferredCommands:         true,
	})
	return app, rt, p
}

// 延后语义下发送成功：草稿先乐观清空，命令执行后消息才出现。
func TestDeferredSendSucceeds(t *testing.T) {
	app, rt, p := newDeferredApp(t)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)

	clickComposer(t, tester)
	tester.Type("延后发送")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	if got := app.DraftForTest(); got != "" {
		t.Fatalf("草稿应乐观清空，实际 %q", got)
	}
	waitFor(t, tester, p, "命令执行后打开流", func() bool { return rt.streamCount() == 1 })
	if got := countText(tester, "延后发送"); got == 0 {
		t.Fatalf("用户消息应出现：%v", tester.Texts())
	}
}

// 延后语义下发送**失败**：草稿与附件必须回到输入框，错误提示不能被清掉。
func TestDeferredSendFailureRestoresDraft(t *testing.T) {
	app, rt, p := newDeferredApp(t)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)

	// 先让会话忙起来：一轮进行中再发第二条 → 执行端返回 ErrBusy
	clickComposer(t, tester)
	tester.Type("第一条")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "打开流", func() bool { return rt.streamCount() == 1 })

	// 直接走"提交发送"这条路径（绕过界面上的发送按钮禁用），模拟命令失败
	app.SetDraftForTest("第二条")
	app.SubmitSendForTest()
	pump(tester, p)
	pump(tester, p)

	if got := app.DraftForTest(); got != "第二条" {
		t.Fatalf("失败后草稿应还原，实际 %q", got)
	}
	if msg := app.ErrMsgForTest(); msg == "" {
		t.Fatalf("失败原因不该被清掉")
	}
}

// 延后语义下插话：草稿乐观清空，命令执行后进队列。
func TestDeferredEnqueue(t *testing.T) {
	app, rt, p := newDeferredApp(t)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)

	clickComposer(t, tester)
	tester.Type("第一条")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "打开流", func() bool { return rt.streamCount() == 1 })

	clickComposer(t, tester)
	tester.Type("插一条")
	pump(tester, p)
	// 忙时按钮是「插话」
	if err := tester.Click(locSteer); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "插话进队列", func() bool {
		session := app.Current()
		return session != nil && len(session.QueuedForTest()) == 1
	})
	if got := app.DraftForTest(); got != "" {
		t.Fatalf("插话后草稿应清空，实际 %q", got)
	}
}

// 延后语义下停止：命令执行后回到 idle（所有端都看到）。
func TestDeferredStop(t *testing.T) {
	app, rt, p := newDeferredApp(t)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)

	clickComposer(t, tester)
	tester.Type("跑起来")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "打开流", func() bool { return rt.streamCount() == 1 })

	app.Stop()
	waitFor(t, tester, p, "停止生效", func() bool { return app.Current().Busy() == driver.BusyIdle })
}

// 显示型提示只能出现一次（host 写 Conversation + uiapp 不能再写一遍）。
func TestHostNoticeAppearsOnce(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)
	app.NotifyDisplay("只应出现一次")
	pump(tester, p)
	if got := countText(tester, "只应出现一次"); got != 1 {
		t.Fatalf("提示应只出现 1 次，实际 %d：%v", got, tester.Texts())
	}
}

// 远程/延后语义：新建会话不能靠"提交后查列表"——要用 cmdResult 带回的 id 切换。
func TestDeferredNewSessionSwitches(t *testing.T) {
	app, _, p := newDeferredApp(t)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)
	before := app.Current().ID

	if err := tester.Click(locNewSession); err != nil {
		t.Fatalf("点不到新建会话：%v", err)
	}
	waitFor(t, tester, p, "新建会话生效", func() bool {
		current := app.Current()
		return current != nil && current.ID != before
	})
	if len(app.Sessions()) != 2 {
		t.Fatalf("应有两个会话：%v", titlesOf(app))
	}
}

// 失败结果回来时用户已经切到别的会话：草稿不能还原到当前输入框。
func TestDeferredSendFailureKeepsOtherSessionDraft(t *testing.T) {
	app, rt, p := newDeferredApp(t)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)
	original := app.Current().ID

	// 让会话忙起来，再直接在**原会话**上提交一条必失败的发送
	clickComposer(t, tester)
	tester.Type("第一条")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "打开流", func() bool { return rt.streamCount() == 1 })

	app.SetDraftForTest("会失败的一条")
	app.SubmitSendForTest()
	// 切到新会话（命令延后，所以这里先建另一个会话）
	app.NewSession()
	waitFor(t, tester, p, "切到新会话", func() bool { return app.Current().ID != original })

	// 清空输入框，模拟新会话的空白状态
	app.SetDraftForTest("")
	pump(tester, p)
	pump(tester, p)

	if got := app.DraftForTest(); got != "" {
		t.Fatalf("别的会话的草稿不该出现在当前输入框，实际 %q", got)
	}
}

// 失败时不在原会话：草稿存进原会话的槽位，切回去（输入框为空）能看到。
func TestDeferredFailureDraftReturnsOnSwitchBack(t *testing.T) {
	app, rt, p := newDeferredApp(t)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)
	original := app.Current().ID

	// 原会话忙起来 → 直接提交一条必失败的发送
	clickComposer(t, tester)
	tester.Type("第一条")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "打开流", func() bool { return rt.streamCount() == 1 })

	// 先切到会话 B，再在**原会话**上提交一条必失败的发送（失败结果回来时已经在 B）
	app.NewSession()
	waitFor(t, tester, p, "切到新会话", func() bool { return app.Current().ID != original })
	app.SetDraftForTest("第二条")
	app.SubmitSendForSessionForTest(original)
	app.SetDraftForTest("")
	pump(tester, p)
	pump(tester, p)
	if got := app.DraftForTest(); got != "" {
		t.Fatalf("会话 B 的输入框应保持为空，实际 %q", got)
	}

	// 切回会话 A：草稿应回到输入框
	app.SelectSessionForTest(original)
	pump(tester, p)
	if got := app.DraftForTest(); got != "第二条" {
		t.Fatalf("切回原会话后草稿应恢复，实际 %q", got)
	}
}
