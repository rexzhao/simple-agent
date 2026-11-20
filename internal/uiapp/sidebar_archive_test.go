package uiapp_test

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// ChooseMenuItemEndingWith 选菜单里"以 suffix 结尾"的项（容忍 ✓ 之类的前缀）。
func chooseMenuItemEndingWith(tester *ui.Tester, suffix string) error {
	for _, item := range tester.Menu() {
		if len(item) >= len(suffix) && item[len(item)-len(suffix):] == suffix {
			return tester.ChooseMenuItem(item)
		}
	}
	return errNoMenuItem
}

var errNoMenuItem = errorString("菜单里没有匹配的项")

type errorString string

func (e errorString) Error() string { return string(e) }

// sideHasText 报告侧栏（x < sidebarWidth）里有没有这段文本。
//
// 为什么需要：主区域头部也显示当前会话标题，直接 HasText 会把
// "侧栏藏着但当前会话正是它"误判成"侧栏显示了"。
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

// 归档的会话**默认完全不显示**，要靠项目行菜单「显示已归档」才列出来。
//
// 用户的要求："默认不显示归档会话，然后有个菜单，选择之后用灰色显示归档对话"。
// 之前是「默认折叠的小节」+ 一个三角开关，用户嫌那个三角又丑又抢眼。
func TestArchivedSessionsHiddenByDefault(t *testing.T) {
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

	// 项目行和正常会话都在
	if !tester.HasText("我的项目") {
		t.Fatalf("项目行应出现：%v", tester.Texts())
	}
	if !tester.HasText("还在用的会话") {
		t.Fatalf("正常会话应显示：%v", tester.Texts())
	}
	// 归档的会话**侧栏里完全看不到**（主区域标题不算，用宽度区分）
	if sideHasText(tester, "收起来的会话", 280) {
		t.Fatalf("归档的会话默认不该显示：%v", tester.Texts())
	}
	// 也不该有常驻的「显示已归档」控件（用户嫌它抢眼）
	if tester.HasText(locShowArchived) {
		t.Fatalf("不该有常驻的归档控件：%v", tester.Texts())
	}
}

// 项目行右键菜单里能切换「显示已归档」，打开后归档会话以灰色列出。
func TestProjectMenuTogglesArchived(t *testing.T) {
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

	// 右键项目行 → 选「显示已归档」
	if err := tester.RightClick("项目甲"); err != nil {
		t.Fatalf("右键点不到项目行：%v（界面：%v）", err, tester.Texts())
	}
	if err := tester.ChooseMenuItem(locShowArchived); err != nil {
		t.Fatalf("菜单里没有「%s」：%v（菜单：%v）", locShowArchived, err, tester.Menu())
	}
	tester.Frame()

	// 现在归档会话出现了
	if !tester.HasText("归档会话") {
		t.Fatalf("打开后应显示归档会话：%v", tester.Texts())
	}

	// 再切一次：又藏起来。打开状态时菜单项带 ✓ 前缀，
	// 所以这里按"以 locShowArchived 结尾"选，不写死完整文字。
	if err := tester.RightClick("项目甲"); err != nil {
		t.Fatalf("右键点不到项目行：%v", err)
	}
	if err := chooseMenuItemEndingWith(tester, locShowArchived); err != nil {
		t.Fatalf("菜单项应仍在：%v（菜单：%v）", err, tester.Menu())
	}
	tester.Frame()
	if sideHasText(tester, "归档会话", 280) {
		t.Fatalf("关掉后归档会话应再次隐藏：%v", tester.Texts())
	}
}

// 这个开关是**纯内存态**：重启回到默认（不显示）。
//
// 用户的要求："这个菜单属于项目，且只需要运行时保持，重启不用记状态"。
func TestArchivedToggleIsMemoryOnly(t *testing.T) {
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

	if app.ShowArchivedForTest("proj-1") {
		t.Fatal("默认不显示归档")
	}
	app.SetShowArchivedForTest("proj-1", true)
	tester.Frame()
	if !tester.HasText("归档会话") {
		t.Fatalf("打开后应显示：%v", tester.Texts())
	}

	// 状态应该只在这个 map 里，不写任何文件 —— 用"新 App 默认 false"来验证
	fresh, _, freshP, freshTester := newLayoutApp(t, -1, 1200, 700)
	fresh.Current().SetProjectForTest("proj-1", "项目")
	pump(freshTester, freshP)
	if fresh.ShowArchivedForTest("proj-1") {
		t.Fatal("新实例应是默认（不显示归档）—— 状态不该被持久化")
	}
}

// 归档会话被选中时，输入卡要说明原因并给恢复入口。
//
// 用户的要求："选中这个会话之后只能先恢复再开始对话"。
func TestArchivedSessionShowsRestoreBanner(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)

	session := app.Current()
	session.Title = "归档会话"
	pump(tester, p)
	session.SetProjectForTest("proj-1", "项目")
	session.SetArchivedForTest(true)
	tester.Frame()
	pump(tester, p)

	// 有说明文案
	if !tester.HasText("会话已归档，恢复后才能继续") {
		t.Fatalf("应说明为什么发不出去：%v", tester.Texts())
	}
	// 有恢复按钮，点了能恢复
	if err := tester.Click(locRestore); err != nil {
		t.Fatalf("点不到恢复按钮：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)
	if app.Current().ArchivedForTest() {
		// 恢复是异步命令；测试环境里 host 会同步处理，这里给一帧机会
		tester.Frame()
		pump(tester, p)
	}
}
