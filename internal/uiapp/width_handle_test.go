package uiapp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/locale"
	"simple-agent/internal/providers"
	"simple-agent/internal/uiapp"
	"simple-agent/internal/uistate"
)

// newWidthApp 起一个带 ui-state 路径的界面（偏好写到临时目录）。
func newWidthApp(t *testing.T, width, height int, prefWidth int) (*uiapp.App, *poster, *ui.Tester, string) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "ui-state.json")
	if prefWidth > 0 {
		if err := uistate.Save(statePath, uistate.State{ChatContentWidth: prefWidth}); err != nil {
			t.Fatal(err)
		}
	}
	p := &poster{}
	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:             "你是助手",
		Workspace:                t.TempDir(),
		Title:                    "测试会话",
		Language:                 locale.ZH,
		Post:                     p.Post,
		ContextWindow:            200000,
		SidebarAutoCollapseWidth: -1,
		UIStatePath:              statePath,
	})
	tester := ui.NewTester(app.View, width, height)
	pump(tester, p)
	return app, p, tester, statePath
}

// 默认宽度是 dsh 的 748（没设置偏好时）。
func TestContentWidthDefault(t *testing.T) {
	app, _, _, _ := newWidthApp(t, 1400, 800, 0)
	if got := app.ContentWidthForTest(); got != 748 {
		t.Fatalf("默认正文宽度应是 748，实际 %d", got)
	}
}

// 拖动把手可以把正文列拖宽，并在**松手时**落盘。
func TestContentWidthDragAndPersist(t *testing.T) {
	app, p, tester, statePath := newWidthApp(t, 1400, 800, 0)
	before := app.ContentWidthForTest()

	rect, ok := tester.Find("调整会话宽度（右）")
	if !ok {
		t.Fatalf("找不到宽度把手：%v", tester.Texts())
	}
	x, y := rect.X+rect.W/2, rect.Y+rect.H/2
	tester.Press(x, y)
	tester.Move(x+40, y)
	tester.Move(x+80, y)
	tester.Release(x+80, y)
	pump(tester, p)

	after := app.ContentWidthForTest()
	if after <= before {
		t.Fatalf("向右拖动后正文应变宽：%d → %d", before, after)
	}

	// 松手后偏好写进了文件
	state, err := uistate.Load(statePath)
	if err != nil {
		t.Fatalf("读取偏好失败：%v", err)
	}
	if state.ChatContentWidth != after {
		t.Fatalf("落盘宽度应与界面一致：文件 %d，界面 %d", state.ChatContentWidth, after)
	}
}

// 拖动不能越过 dsh 的上下限（640 / 920）。
func TestContentWidthDragClamped(t *testing.T) {
	app, p, tester, _ := newWidthApp(t, 1400, 800, 0)
	rect, ok := tester.Find("调整会话宽度（右）")
	if !ok {
		t.Fatalf("找不到把手")
	}
	x, y := rect.X+rect.W/2, rect.Y+rect.H/2

	// 疯狂往左拖：应收敛到最小值
	tester.Press(x, y)
	for step := 1; step <= 20; step++ {
		tester.Move(x-float32(step)*50, y)
	}
	tester.Release(x-1000, y)
	pump(tester, p)
	if got := app.ContentWidthForTest(); got < 640 {
		t.Fatalf("不应小于 640，实际 %d", got)
	}

	// 疯狂往右拖：不超过 920
	rect, _ = tester.Find("调整会话宽度（右）")
	x, y = rect.X+rect.W/2, rect.Y+rect.H/2
	tester.Press(x, y)
	for step := 1; step <= 40; step++ {
		tester.Move(x+float32(step)*50, y)
	}
	tester.Release(x+2000, y)
	pump(tester, p)
	if got := app.ContentWidthForTest(); got > 920 {
		t.Fatalf("不应大于 920，实际 %d", got)
	}
}

// 窄窗口下没有把手可用（dsh 用 min(10px, …) 让热区归零）。
func TestContentWidthHandleHiddenWhenNarrow(t *testing.T) {
	// 窗口刚够放正文列 + 边距：留给把手的空间不够
	_, _, _, _ = newWidthApp(t, 760, 700, 0)
	app, _, _, _ := newWidthApp(t, 760, 700, 0)
	if got := app.ContentWidthHandleWidthForTest(); got != 0 {
		t.Fatalf("窄窗口下把手应不可用，实际宽 %d", got)
	}

	// 宽窗口下可用
	wide, _, _, _ := newWidthApp(t, 1600, 900, 0)
	if got := wide.ContentWidthHandleWidthForTest(); got <= 0 {
		t.Fatalf("宽窗口下把手应可用，实际 %d", got)
	}
}

// 保存过的偏好会在启动时恢复。
func TestContentWidthRestoredFromDisk(t *testing.T) {
	app, _, _, _ := newWidthApp(t, 1400, 800, 860)
	if got := app.ContentWidthForTest(); got != 860 {
		t.Fatalf("应从文件恢复 860，实际 %d", got)
	}
}

// 偏好文件损坏时按默认值启动，且不挡住界面。
func TestContentWidthCorruptFileFallsBack(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "ui-state.json")
	if err := os.WriteFile(statePath, []byte("坏掉的内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &poster{}
	app := uiapp.NewApp(&fakeRuntime{}, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		Workspace:                t.TempDir(),
		Title:                    "测试会话",
		Language:                 locale.ZH,
		Post:                     p.Post,
		ContextWindow:            200000,
		SidebarAutoCollapseWidth: -1,
		UIStatePath:              statePath,
	})
	tester := ui.NewTester(app.View, 1400, 800)
	pump(tester, p)

	if got := app.ContentWidthForTest(); got != 748 {
		t.Fatalf("损坏时应回默认 748，实际 %d", got)
	}
}
