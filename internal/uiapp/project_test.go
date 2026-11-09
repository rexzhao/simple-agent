package uiapp_test

import (
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/locale"
	"simple-agent/internal/providers"
	"simple-agent/internal/uiapp"
)

// 「添加项目」：选一个目录后，在该目录新建会话，侧栏出现新项目组。
//
// 对齐 dsh 的 WorkspacePickFlow：添加项目只有"选一个宿主目录"这一条路，
// 项目从会话的 workspace 反推（本仓没有独立的项目存储）。

// newProjectApp 起一个界面，PickDirectory 由测试注入。
func newProjectApp(t *testing.T, pick func() (string, error)) (*uiapp.App, *poster, *ui.Tester) {
	t.Helper()
	p := &poster{}
	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		Title:                    "测试会话",
		Language:                 locale.ZH,
		Post:                     p.Post,
		ContextWindow:            200000,
		SidebarAutoCollapseWidth: -1,
		PickDirectory:            pick,
	})
	tester := ui.NewTester(app.View, 1200, 700)
	pump(tester, p)
	return app, p, tester
}

// 点「添加项目…」→ 选目录 → 新会话落在该目录下。
func TestAddProjectCreatesSessionInDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-project")
	picked := false
	app, p, tester := newProjectApp(t, func() (string, error) {
		picked = true
		return dir, nil
	})

	if !tester.HasText("添加项目…") {
		t.Fatalf("侧栏应有「添加项目…」入口：%v", tester.Texts())
	}
	if err := tester.Click("添加项目…"); err != nil {
		t.Fatalf("点不到添加项目：%v", err)
	}
	// PickDirectory 在 goroutine 里调用，结果 post 回主线程
	waitFor(t, tester, p, "新建会话", func() bool {
		return picked && len(app.SessionsForTest()) == 2
	})

	// 新会话（当前）的工作区就是选中的目录
	if got := app.Current().WorkspaceForTest(); got != dir {
		t.Fatalf("新会话的 workspace 应是选中的目录：%q，期望 %q", got, dir)
	}
	// 侧栏出现以目录名命名的项目组
	if !tester.HasText("my-project") {
		t.Fatalf("侧栏应出现新项目组：%v", tester.Texts())
	}
}

// 取消选择（返回空串）不该建会话。
func TestAddProjectCancelledDoesNothing(t *testing.T) {
	app, p, tester := newProjectApp(t, func() (string, error) { return "", nil })
	before := len(app.SessionsForTest())

	if err := tester.Click("添加项目…"); err != nil {
		t.Fatalf("点不到添加项目：%v", err)
	}
	pump(tester, p)
	pump(tester, p)
	if got := len(app.SessionsForTest()); got != before {
		t.Fatalf("取消后不该新建会话：%d → %d", before, got)
	}
}

// 同名项目拒绝：不新建会话，给出提示（对齐 dsh 的 conflict.named）。
func TestAddProjectRejectsDuplicateName(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "dup")
	app, p, tester := newProjectApp(t, func() (string, error) { return existing, nil })

	// 先加一次，成功
	if err := tester.Click("添加项目…"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "第一次添加", func() bool { return len(app.SessionsForTest()) == 2 })
	// 等选择器状态复位：pickingProject=false 之前再点会被防重复逻辑拦掉
	// （那是**正确行为**——选择器还没关就重复点，不该弹第二个）
	waitFor(t, tester, p, "选择器复位", func() bool { return !app.PickingProjectForTest() })
	afterFirst := len(app.SessionsForTest())

	// 再选一个**不同路径但同名**的目录：应被拒绝。
	//
	// 这里直接驱动 adoptProject 而不是再点一次按钮：Toast 只显示一帧，
	// 而"等 pickingProject 复位"需要好几帧，等到了 Toast 早消失了。
	// 拒绝对话本身的逻辑是 adoptProject，按钮只负责取路径——分开测更稳。
	app.AdoptProjectForTest(filepath.Join(t.TempDir(), "x", "dup"))

	if got := len(app.SessionsForTest()); got != afterFirst {
		t.Fatalf("同名项目应被拒绝：%d → %d", afterFirst, got)
	}
	// 同一条提示也走界面：这一帧它应该已经被 Toast 消费掉了
	pump(tester, p)
	if !toastShown(tester, "已存在名为") {
		t.Fatalf("应给出重名提示：%v", tester.Texts())
	}
}

// 不同名的目录可以被接纳（确认上面的拒绝不是"什么都拒绝"）。
func TestAddProjectAllowsDistinctName(t *testing.T) {
	first := filepath.Join(t.TempDir(), "alpha")
	app, p, tester := newProjectApp(t, func() (string, error) { return first, nil })
	if err := tester.Click("添加项目…"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "第一次添加", func() bool { return len(app.SessionsForTest()) == 2 })
	waitFor(t, tester, p, "选择器复位", func() bool { return !app.PickingProjectForTest() })

	app.AdoptProjectForTest(filepath.Join(t.TempDir(), "beta"))
	waitFor(t, tester, p, "第二次添加", func() bool { return len(app.SessionsForTest()) == 3 })
	if !tester.HasText("beta") {
		t.Fatalf("侧栏应出现第二个项目：%v", tester.Texts())
	}
}

// toastShown 报告界面上是否出现过含该子串的 Toast。
//
// Toast 只显示一小段时间，pump 两帧内一般还在；用 Texts() 里找文本的方式
// 而不是读 App 字段——字段在渲染那一帧就被清空了。
func toastShown(tester *ui.Tester, sub string) bool {
	return findText(tester.Texts(), sub) != ""
}
