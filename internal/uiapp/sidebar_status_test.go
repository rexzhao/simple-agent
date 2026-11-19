package uiapp_test

import (
	"testing"

	"simple-agent/internal/driver"
	"simple-agent/internal/uiapp"
)

// 会话行的运行状态点：让用户一眼看出哪些会话还在跑。
//
// 空闲**不显示**：侧栏里大多数会话都是安静的，全都画一个点反而把真正
// 在跑的那几个淹没掉。
func TestSidebarHidesStatusDotWhenIdle(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().Title = "安静的会话"
	pump(tester, p)

	for _, dot := range []string{"●", "?", "◐"} {
		if tester.HasText(dot) {
			t.Fatalf("空闲会话不该显示状态点 %q：%v", dot, tester.Texts())
		}
	}
}

// 状态 → 符号的映射。
//
// 空符号 = 不显示。runningTools 与 streaming 都是 ●（都在干活），
// 靠颜色区分（工具是警告色）；awaitingAnswer 用 ? 而不是 ● ——
// 它需要用户**动手**，跟"正在跑"不是一回事。
func TestSessionStatusDotMapping(t *testing.T) {
	cases := []struct {
		busy driver.Busy
		want string
	}{
		{driver.BusyIdle, ""},
		{driver.BusyStreaming, "●"},
		{driver.BusyRunningTools, "●"},
		{driver.BusyAwaitingAnswer, "?"},
		{driver.BusyCompacting, "◐"},
	}
	for _, tc := range cases {
		if got := uiapp.StatusDotForTest(tc.busy); got != tc.want {
			t.Fatalf("Busy=%s 应为 %q，实际 %q", tc.busy, tc.want, got)
		}
	}
}

// 空闲时该留白而不是缺位：会话行的文字起点要与项目行对齐。
//
// 这个用截图对齐测试（TestSidebarSessionAlignsWithGroup）保证，
// 这里只确认状态点不占位时仍渲染了等宽的空盒子——即"没有 ● 但行还在"。
func TestSidebarRowStillRendersWhenIdle(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().Title = "标题应该可见"
	pump(tester, p)

	if !tester.HasText("标题应该可见") {
		t.Fatalf("空闲时标题仍应显示：%v", tester.Texts())
	}
}