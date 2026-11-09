package uiapp_test

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/driver"
	"simple-agent/internal/locale"
	"simple-agent/internal/providers"
	"simple-agent/internal/uiapp"
)

// 底部状态条（P4）：
//   - 左侧「上下文 x / y（n%）」+ 一条 Meter 条形；
//   - 右侧「N 轮」「N 步」，数字来自 driver.Counts。
//
// 这里只测「界面把 driver 的数字显示出来了」，计数口径本身由 driver 的
// counts_test.go 负责（含重放与压缩）。

// TestStatusBarShowsCounts 走一遍真实发送流程，断言状态条上的轮数/步数在变。
func TestStatusBarShowsCounts(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)

	// 空会话：0 轮 0 步
	pump(tester, p)
	if !tester.HasText("0 轮") || !tester.HasText("0 步") {
		t.Fatalf("空会话应显示 0 轮 0 步：%v", tester.Texts())
	}
	if !hasTextPrefix(tester, "上下文 ") {
		t.Fatalf("应有上下文用量：%v", tester.Texts())
	}

	// 发一条 → 1 轮 1 步
	clickComposer(t, tester)
	tester.Type("你好")
	tester.Frame()
	if err := tester.Click(locSend); err != nil {
		t.Fatalf("点不到发送按钮：%v", err)
	}
	waitFor(t, tester, p, "打开流", func() bool { return rt.streamCount() == 1 })
	rt.push(t, providers.Event{Kind: providers.EventText, Text: "你好！"})
	rt.push(t, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	waitFor(t, tester, p, "回合结束", func() bool { return app.Current().Busy() == driver.BusyIdle })

	pump(tester, p)
	if !tester.HasText("1 轮") {
		t.Fatalf("一轮之后应显示「1 轮」：%v", tester.Texts())
	}
	if !tester.HasText("1 步") {
		t.Fatalf("一轮之后应显示「1 步」：%v", tester.Texts())
	}
}

// TestStatusBarShowsContextPercent 断言上下文文案带百分比，且与窗口同源。
func TestStatusBarShowsContextPercent(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)
	pump(tester, p)

	if !hasTextPrefix(tester, "上下文 ") {
		t.Fatalf("上下文文案应带百分比：%v", tester.Texts())
	}
	// 空会话的用量很小，占比必须是 0%（不能出现 100% 这种离谱值）
	if tester.HasText("100%") {
		t.Fatalf("空会话不该显示 100%%：%v", tester.Texts())
	}
}

// TestStatusBarEnglish 英文界面下也要有状态条（不写死中文）。
func TestStatusBarEnglish(t *testing.T) {
	app, _, p := newApp(t)
	app.SetLanguageForTest(locale.EN)
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)
	pump(tester, p)

	if !hasTextPrefix(tester, "Context ") {
		t.Fatalf("英文界面应显示 Context：%v", tester.Texts())
	}
	if !tester.HasText("Turns: 0") || !tester.HasText("Steps: 0") {
		t.Fatalf("英文界面应显示 Turns/Steps：%v", tester.Texts())
	}
	if tester.HasText("轮") || tester.HasText("步") {
		t.Fatalf("英文界面不该出现中文状态条：%v", tester.Texts())
	}
}

// TestStatusBarHiddenInEmptyState 空态（没有会话）不渲染输入卡与状态条。
func TestStatusBarHiddenInEmptyState(t *testing.T) {
	app, _, p := newApp(t)
	// 删掉启动时自动建的会话 → 进入空态
	session := app.Current()
	if session == nil {
		t.Skip("启动即无会话，空态另有用例覆盖")
	}
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	app.DeleteSessionForTest(session.ID)
	pump(tester, p)

	if tester.HasText("0 轮") || hasTextPrefix(tester, "上下文 ") {
		t.Fatalf("空态不该显示状态条：%v", tester.Texts())
	}
}

// TestStatusBarMeterWarning 上下文接近窗口时条形用警告色（80% 起）。
//
// 直接构造一个窗口很小的 app：一条长消息就能把占比推过 80%。
// TestStatusBarUsesSessionWindow 状态条的窗口必须跟着**会话自己的模型**走。
//
// 默认模型 100k，切到一个 4k 的模型后：状态的「/ y」要变成 4k，百分比按 4k 算。
// 以前的实现读的是 Options.ContextWindow（默认模型的窗口），切换后仍是 100k。
func TestStatusBarUsesSessionWindow(t *testing.T) {
	app, _, p := newAppWithOptions(t, func(o *uiapp.Options) {
		o.ContextWindow = 100_000
		o.WindowFor = func(sel providers.Selection) int {
			if sel.Model == "small" {
				return 4_000
			}
			return 100_000
		}
	})
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)
	pump(tester, p)

	if !containsText(tester, "/ 100k（") {
		t.Fatalf("默认模型应显示 100k 窗口：%v", tester.Texts())
	}

	app.SelectModelForTest(providers.Selection{ProviderID: "fake", Model: "small"})
	pump(tester, p)

	if !containsText(tester, "/ 4k（") {
		t.Fatalf("切到 4k 模型后状态条应显示 4k 窗口：%v", tester.Texts())
	}
	if containsText(tester, "/ 100k（") {
		t.Fatalf("不该再显示默认模型的窗口：%v", tester.Texts())
	}
	// 百分比也要按 4k 算：system + 工具 schema 就有几千 token，用 100k 会显示 0%
	if got := app.ContextPercentForTest(); got == 0 {
		t.Fatalf("窗口 4k 时占比不该是 0%%：%v", tester.Texts())
	}
}

func TestStatusBarMeterWarning(t *testing.T) {
	// 很小的窗口（走构造函数这条真实路径：配置里就是这么给的）
	app, _, p := newAppWithOptions(t, func(o *uiapp.Options) { o.ContextWindow = 200 })
	tester := ui.NewTester(app.View, 900, 700)
	ensureSession(t, tester, p, app)

	clickComposer(t, tester)
	tester.Type(strings.Repeat("这是一段很长的用户消息，用来把上下文占比推到八成以上。", 4))
	tester.Frame()
	if err := tester.Click(locSend); err != nil {
		t.Fatalf("点不到发送按钮：%v", err)
	}
	pump(tester, p)
	pump(tester, p)

	// 断言百分比本身已经超过自动压缩阈值（Meter 的颜色由同一条百分比决定）
	if got := app.ContextPercentForTest(); got < int(driver.AutoCompactRatio*100) {
		t.Fatalf("这条消息应把占比推过 %d%%（实际 %d%%）：%v",
			int(driver.AutoCompactRatio*100), got, tester.Texts())
	}
	if !hasTextPrefix(tester, "上下文 ") {
		t.Fatalf("状态条应仍然显示上下文：%v", tester.Texts())
	}
}

// newAppWithOptions 建一个 app，允许在默认测试配置上改几项（真实构造函数路径）。
func newAppWithOptions(t *testing.T, mutate func(*uiapp.Options)) (*uiapp.App, *fakeRuntime, *poster) {
	t.Helper()
	rt := &fakeRuntime{}
	p := &poster{}
	opts := uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:             "你是助手",
		Workspace:                t.TempDir(),
		Title:                    "测试会话",
		MaxToolRounds:            3,
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
		Post:                     p.Post,
		ContextWindow:            testContextWindow,
	}
	if mutate != nil {
		mutate(&opts)
	}
	return uiapp.NewApp(rt, opts), rt, p
}

// containsText 判断是否有文本包含子串。
func containsText(tester *ui.Tester, sub string) bool {
	for _, text := range tester.Texts() {
		if strings.Contains(text, sub) {
			return true
		}
	}
	return false
}
