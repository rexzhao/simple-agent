package uiapp_test

import (
	"image/png"
	"os"
	"testing"
)

// 侧栏项目/会话两级的实际样子，人眼确认用。
//
// 设 SIMPLE_AGENT_SHOT=<path> 生成 png；平时只是普通断言。
func TestSidebarTreeScreenshot(t *testing.T) {
	app, _, p, tester := newLayoutApp(t, -1, 1200, 700)

	first := app.Current()
	first.SetWorkspaceForTest("/work/alpha")
	first.Title = "整理配置说明"
	second := app.NewSession()
	second.SetWorkspaceForTest("/work/alpha")
	second.Title = "修复登录跳转"
	third := app.NewSession()
	third.SetWorkspaceForTest("/work/beta")
	third.Title = "压测与调优"
	pump(tester, p)
	pump(tester, p)

	if path := os.Getenv("SIMPLE_AGENT_SHOT"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := png.Encode(file, tester.Image()); err != nil {
			t.Fatal(err)
		}
	}

	// 会话与项目的文字左对齐（几何断言，截图只是辅助）
	groupRect, ok := findRectContaining(tester, "alpha")
	if !ok {
		t.Fatalf("找不到项目行：%v", tester.Texts())
	}
	sessionRect, ok := findRectContaining(tester, "整理配置说明")
	if !ok {
		t.Fatalf("找不到会话行：%v", tester.Texts())
	}
	if diff := sessionRect.X - groupRect.X; diff > 1 || diff < -1 {
		t.Fatalf("会话文字应与项目文字左对齐：项目 x=%.1f，会话 x=%.1f",
			groupRect.X, sessionRect.X)
	}
}
