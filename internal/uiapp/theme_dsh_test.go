package uiapp_test

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/providers"
	"simple-agent/internal/uiapp"
)

// 浅色模式下应用 dsh 的设计令牌；深色模式保持 mygo 自带主题。
func TestThemeTokensFollowDSH(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)

	theme := app.ThemeForTest()
	if theme.Dark {
		t.Fatal("默认应是浅色主题")
	}
	// 抽三个关键令牌：背景白、正文近黑、边框 l2 = rgba(0,0,0,.10)
	if theme.Background != ui.Hex("#FFFFFF") {
		t.Fatalf("背景应是 dsh 的 bg-base：%#v", theme.Background)
	}
	if theme.Text != ui.Hex("#0F1115") {
		t.Fatalf("正文应是 dsh 的 label-primary：%#v", theme.Text)
	}
	if theme.Accent != ui.RGB(65, 118, 230) {
		t.Fatalf("强调色应是 dsh 品牌蓝：%#v", theme.Accent)
	}
	if theme.Selection != ui.Hex("#EDF3FE") {
		t.Fatalf("用户气泡底色应是 dsh 的 bubble：%#v", theme.Selection)
	}
	if theme.Border != ui.RGBA(0, 0, 0, 0.10) {
		t.Fatalf("边框应是 border-l2：%#v", theme.Border)
	}
	// 字体栈是 dsh 的正文栈（Windows 上落到 Segoe UI）
	if !strings.HasPrefix(theme.Font, "-apple-system") || !strings.Contains(theme.Font, "Segoe UI") {
		t.Fatalf("应使用 dsh 的字体栈：%q", theme.Font)
	}
}

// 深色模式下**也是** dsh 浅色（用户决定：界面始终浅色，不跟随系统）。
func TestDarkModeAlsoUsesDSHLight(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	tester.SetDark(true)
	pump(tester, p)

	theme := app.ThemeForTest()
	if theme == nil {
		t.Fatal("应拿到主题")
	}
	if theme.Dark {
		t.Fatal("系统深色时界面仍应是浅色主题")
	}
	if theme.Background != ui.Hex("#FFFFFF") || theme.Text != ui.Hex("#0F1115") {
		t.Fatalf("系统深色时也应套用 dsh 浅色令牌：bg=%#v text=%#v", theme.Background, theme.Text)
	}
}

// 侧栏尺寸常量来自 dsh columns.ts（这里通过 App 的行为间接守一下默认值）。
// sidebarWidthDefaultForTest 取包内默认侧栏宽度（测试用）。
func sidebarWidthDefaultForTest() int { return uiapp.SidebarDefaultWidthForTest() }

func TestSidebarWidthDefaultsMatchDSH(t *testing.T) {
	app, _, p := newApp(t)
	_ = providers.Selection{}
	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	// Split 布局后 sidebarWidth 会被写成实际宽度（=280），未布局前是 0
	if got := app.SidebarWidthForTest(); got != 0 && (got < 264 || got > 420) {
		t.Fatalf("侧栏宽度应落在 dsh 的 264–420，实际 %d", got)
	}
	if sidebarWidthDefaultForTest() != 280 {
		t.Fatalf("侧栏默认宽度应是 dsh 的 280，实际 %d", sidebarWidthDefaultForTest())
	}
	_ = uiapp.SidebarDefaultWidthForTest()
}
