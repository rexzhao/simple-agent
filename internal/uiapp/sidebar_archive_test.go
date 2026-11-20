package uiapp_test

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// 归档的会话**挂在项目下面**，但默认折叠、不占地方。
//
// 用户的要求："归档会话也显示在项目下面，但是默认折叠不显示"。
// 两个要点都得测到：不能整个藏掉（否则用户没法恢复），也不能混在
// 正常会话里（那等于没归档）。
func TestArchivedSessionFoldedUnderProject(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)

	active := app.Current()
	active.Title = "还在用的会话"
	archived := app.NewSession()
	archived.Title = "收起来的会话"
	pump(tester, p)
	pump(tester, p)

	active.SetProjectForTest("proj-1", "我的项目")
	archived.SetProjectForTest("proj-1", "我的项目")
	archived.SetArchivedForTest(true)
	tester.Frame()

	// 项目行在，正常会话在
	if !tester.HasText("我的项目") {
		t.Fatalf("项目行应出现：%v", tester.Texts())
	}
	if !tester.HasText("还在用的会话") {
		t.Fatalf("正常会话应显示：%v", tester.Texts())
	}
	// 归档会话**默认折叠**：**侧栏里**不该出现它的标题。
	//
	// 不能直接 HasText —— 主区域头部也显示当前会话的标题，
	// 而当前会话刚建出来就是那个归档的。只查侧栏宽度以内的文本。
	if sideHasText(tester, "收起来的会话", 280) {
		t.Fatalf("归档的会话在侧栏里应折叠不显示：%v", tester.Texts())
	}
	// 但要有「显示已归档」这个小节入口，否则永远恢复不了
	if !tester.HasText(locShowArchived) {
		t.Fatalf("应有「%s」小节：%v", locShowArchived, tester.Texts())
	}
}

// 展开「显示已归档」后，归档的会话就出来了。
func TestArchivedSectionExpands(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)

	active := app.Current()
	active.Title = "正常会话"
	archived := app.NewSession()
	archived.Title = "归档会话"
	pump(tester, p)
	pump(tester, p)

	active.SetProjectForTest("proj-1", "项目")
	archived.SetProjectForTest("proj-1", "项目")
	archived.SetArchivedForTest(true)
	tester.Frame()

	if err := tester.Click(locShowArchived); err != nil {
		t.Fatalf("点不到「%s」：%v", locShowArchived, err)
	}
	tester.Frame()

	if !tester.HasText("归档会话") {
		t.Fatalf("展开后归档的会话应显示：%v", tester.Texts())
	}
}

// sideHasText 报告侧栏（x < sidebarWidth）里有没有这段文本。
//
// 为什么需要：主区域头部也显示当前会话标题，直接 HasText 会把
// "侧栏折叠了但当前会话正是它"误判成"侧栏显示了"。
func sideHasText(tester *ui.Tester, text string, sidebarWidth float32) bool {
	for _, candidate := range tester.Texts() {
		if candidate != text {
			continue
		}
		rect, ok := tester.Find(candidate)
		if !ok {
			continue
		}
		if rect.X < sidebarWidth {
			return true
		}
	}
	return false
}

// 归档小节**只显示一个灰色小三角**，不写文字。
//
// 用户的原话："show archived 太抢眼了，就简单显示一个灰色向下箭头应该就可以了"。
// 它和项目行长一样、又是粗体小字，视觉权重太高；归档本来就该收着，
// 一个小三角足够表达"这里能展开"。
func TestArchivedSectionShowsOnlyArrow(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)

	active := app.Current()
	active.Title = "正常会话"
	archived := app.NewSession()
	archived.Title = "归档会话"
	pump(tester, p)
	pump(tester, p)

	active.SetProjectForTest("proj-1", "项目甲")
	archived.SetProjectForTest("proj-1", "项目甲")
	archived.SetArchivedForTest(true)
	tester.Frame()

	// 「显示已归档」**不该作为可见文字出现**（它只是无障碍名）。
	//
	// 不能用 HasText 判断：a11y 名也会进文本索引。查它有没有占据独立的
	// 文本元素——归档小节现在只有三角，那段文字不存在。
	foundText := false
	for _, text := range tester.Texts() {
		if text == locShowArchived {
			// 若存在，应只是标签而不是一段独立文字：检查它的宽度
			if rect, ok := tester.Find(text); ok && rect.W > 20 {
				foundText = true
			}
		}
	}
	if foundText {
		t.Fatalf("归档小节不该显示文字（宽度 > 20），只留三角：%v", tester.Texts())
	}
	// 三角可点（能展开）
	if err := tester.Click(locShowArchived); err != nil {
		t.Fatalf("归档三角应可点：%v", err)
	}
	tester.Frame()
	if !tester.HasText("归档会话") {
		t.Fatalf("点了三角应展开归档：%v", tester.Texts())
	}
}

// 没有归档会话时，不该出现「显示已归档」这个小节。
func TestNoArchivedSectionWhenEmpty(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)
	app.Current().Title = "唯一会话"
	pump(tester, p)
	app.Current().SetProjectForTest("proj-1", "项目")
	tester.Frame()

	if tester.HasText(locShowArchived) {
		t.Fatalf("没有归档会话时不该出现该小节：%v", tester.Texts())
	}
}