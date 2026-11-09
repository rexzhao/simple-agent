package uiapp_test

import (
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
)

// 过程组展开后的样子：标题行不缩进，竖线只从标题下方开始。
//
// 人眼确认用：设 SIMPLE_AGENT_SHOT=<path> 生成 png；平时是普通断言。
func TestProcessGroupExpandedScreenshot(t *testing.T) {
	app, _, p := newApp(t)
	if _, err := app.Current().Conversation().AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{message.NewTextPart("帮我看看这个项目")},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	agent := message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.TextPart{Text: "先列一下目录，再读关键文件确认结构。"}, MediaType: render.MediaReasoning},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "glob", "args": map[string]any{"pattern": "**/*.go"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "glob", "output": "main.go\nconfig.go", "is_error": false,
			}}, MediaType: render.MediaToolResult},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c2", "name": "read", "args": map[string]any{"file_path": "main.go"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c2", "name": "read", "output": "package main", "is_error": false,
			}}, MediaType: render.MediaToolResult},
			message.NewTextPart("这是一个 Go 项目，入口在 `main.go`。"),
		},
	}
	entry, err := app.Current().Conversation().AddMessage(agent, conversation.StatusComplete)
	if err != nil {
		t.Fatal(err)
	}
	entry.SetFinishedAt(time.Now())

	tester := ui.NewTester(app.View, 760, 560)
	pump(tester, p)
	pump(tester, p)

	// 展开过程组：点标题（"已…"开头的那一行）
	groupRect, ok := findRectContaining(tester, "已")
	if !ok {
		t.Fatalf("找不到过程组标题：%v", tester.Texts())
	}
	tester.ClickAt(groupRect.X+groupRect.W/2, groupRect.Y+groupRect.H/2)
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
}
