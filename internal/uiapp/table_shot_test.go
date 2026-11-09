package uiapp_test

import (
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
)

// 表格的实际样子（人眼确认用，设 SIMPLE_AGENT_SHOT 生成 png）。
//
// 复刻用户截图里的场景：两列表格 + 中文长单元格。
func TestTableScreenshot(t *testing.T) {
	app, _, p := newApp(t)
	text := "分层结构（约 1.5 万行）\n\n" +
		"| 层 | 模块 |\n" +
		"|---|---|\n" +
		"| 容器与寻址 | src/casc/* |\n" +
		"| 压缩 | casc/bite.rs |\n" +
		"| 数据表 | db2.rs |\n" +
		"| 索语义 | schema.rs, map.rs |\n" +
		"| 地图清单 | wdt.rs |\n" +
		"| 地形 | adt.rs |\n" +
		"| 流式加载 | stream.rs |\n"
	msg := message.Message{MessageID: "tbl-shot", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart(text),
	}}
	entry, err := app.Current().Conversation().AddMessage(msg, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	entry.SetFinishedAt(time.Now())

	tester := ui.NewTester(app.View, 968, 560)
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

	// 两列必须横向并排
	left, okLeft := tester.Find("容器与寻址")
	right, okRight := tester.Find("src/casc/*")
	if !okLeft || !okRight {
		t.Fatalf("找不到单元格：%v", tester.Texts())
	}
	if right.X <= left.X {
		t.Fatalf("两列没有并排：%.1f vs %.1f", right.X, left.X)
	}
	if diff := left.Y - right.Y; diff > 2 || diff < -2 {
		t.Fatalf("两个单元格不在同一行：%.1f vs %.1f", left.Y, right.Y)
	}
}
