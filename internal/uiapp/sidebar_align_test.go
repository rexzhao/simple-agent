package uiapp_test

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// 侧栏树：会话文字应与项目文字**左对齐**（不被项目前面的三角推到右边）。
//
// 历史：早先用 `ui.Outline` 渲染，它给**每一行**都预留 Space(4)=16px 的三角槽，
// 再给子行加 depth*Space(4) 缩进，于是会话比项目多缩进 26px（实测）。
// 那个缩进是 Outline 的内部实现、不可配置，所以改成自己扁平化 + 画前缀：
//
//	项目行：[三角 14px][名字]
//	会话行：[留白 14px][标题]
//
// 两条文字因此落在同一条 x 上（本测试守的就是这个）。
func TestSidebarSessionAlignsWithGroup(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	first := app.Current()
	first.SetWorkspaceForTest("/work/alpha")
	first.Title = "alpha 会话"
	pump(tester, p)

	groupRect, ok := findRectContaining(tester, "alpha")
	if !ok {
		t.Fatalf("找不到项目行：%v", tester.Texts())
	}
	sessionRect, ok := findRectContaining(tester, "alpha 会话")
	if !ok {
		t.Fatalf("找不到会话行：%v", tester.Texts())
	}
	diff := sessionRect.X - groupRect.X
	t.Logf("项目文字 x=%.1f，会话文字 x=%.1f，差 %.1f", groupRect.X, sessionRect.X, diff)
	if diff > 1 || diff < -1 {
		t.Fatalf("会话文字应与项目文字左对齐：项目 x=%.1f，会话 x=%.1f（差 %.1f）",
			groupRect.X, sessionRect.X, diff)
	}
}

// 「＋ 添加项目…」的文字也要与项目名、会话标题对齐。
func TestSidebarAddProjectAlignsWithGroups(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().SetWorkspaceForTest("/work/alpha")
	app.Current().Title = "alpha 会话"
	pump(tester, p)

	groupRect, ok := findRectContaining(tester, "alpha")
	if !ok {
		t.Fatalf("找不到项目行：%v", tester.Texts())
	}
	addRect, ok := findRectContaining(tester, "添加项目")
	if !ok {
		t.Fatalf("找不到「添加项目…」入口：%v", tester.Texts())
	}
	if diff := addRect.X - groupRect.X; diff > 1 || diff < -1 {
		t.Fatalf("「添加项目」文字应与项目名对齐：项目 x=%.1f，添加 x=%.1f",
			groupRect.X, addRect.X)
	}
}

// 项目可以收起/展开（换成手写树之后最容易坏的交互）。
//
// 判定不能用 HasText("会话标题")：主区域头部也显示当前会话标题，收起后它还在。
// 所以按**侧栏内**才有的东西判断——项目的三角字符 ▾/▸ 与侧栏里的会话行。
func TestSidebarGroupCollapses(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	first := app.Current()
	first.SetWorkspaceForTest("/work/alpha")
	first.Title = "alpha 会话"
	pump(tester, p)

	if !tester.HasText("▾") {
		t.Fatalf("默认应展开（▾）：%v", tester.Texts())
	}
	// 点三角收起
	if err := tester.Click("▾"); err != nil {
		t.Fatalf("点不到项目三角：%v", err)
	}
	pump(tester, p)
	if !tester.HasText("▸") {
		t.Fatalf("收起后应显示 ▸：%v", tester.Texts())
	}
	// 再点展开
	if err := tester.Click("▸"); err != nil {
		t.Fatalf("点不到项目三角：%v", err)
	}
	pump(tester, p)
	if !tester.HasText("▾") {
		t.Fatalf("展开后应显示 ▾：%v", tester.Texts())
	}
}

var _ = ui.Rect{}
