package uiapp_test

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/tools"
)

// 工具结果**折进调用行**：不再单独占一行「工具结果 · read（展开）」。
//
// 用户的要求："toolresult 折叠进 tool call 里面"。
// 折叠时只显示调用 + 成败；点那一行才展开详情。
func TestToolResultFoldsIntoCallRow(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.TextPart{Text: "想一下"}, MediaType: render.MediaReasoning},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "read", "args": map[string]any{"file_path": "a.txt"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "read", "is_error": false, "output": "文件内容",
			}}, MediaType: render.MediaToolResult},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	clickProcessGroup(t, tester)
	pump(tester, p)

	all := strings.Join(tester.Texts(), " | ")
	// 不该再有独立的"工具结果"行
	if strings.Contains(all, "工具结果") {
		t.Fatalf("结果不该单独占一行：%v", tester.Texts())
	}
	// 折叠时看不到输出正文
	if tester.HasText("文件内容") {
		t.Fatalf("折叠时不该显示输出：%v", tester.Texts())
	}
	// 但能看到调用本身与成败
	if !tester.HasText("read") {
		t.Fatalf("应显示工具名：%v", tester.Texts())
	}

	// 点开那一行 → 看到输出
	clickToolRow(t, tester, "read")
	pump(tester, p)
	if !tester.HasText("文件内容") {
		t.Fatalf("展开后应显示输出：%v", tester.Texts())
	}
}

// edit 展开显示 **diff**（行号 + +/- 标记），而不是原始输出文本。
//
// 用户的要求："在命令的详情里面应该用 diff 的方式显示修改的内容"。
func TestEditShowsDiffWhenExpanded(t *testing.T) {
	app, _, p := newApp(t)
	conv := app.Current().Conversation()

	diff := tools.BuildLineDiff("panel.lua",
		"function Foo()\n    Bar()\n    Baz()\nend\n",
		"function Foo()\n    Bar()\nend\n", tools.LineDiffOptions{})
	if diff.Added != 0 || diff.Removed != 1 {
		t.Fatalf("前提不成立：应 -1 +0，实际 +%d -%d", diff.Added, diff.Removed)
	}
	if _, err := conv.AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent,
		Parts: []message.Part{
			{Content: message.TextPart{Text: "改一下"}, MediaType: render.MediaReasoning},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "edit", "args": map[string]any{"file_path": "panel.lua"},
			}}, MediaType: render.MediaToolUse},
			{Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "edit", "is_error": false,
				"output": "已编辑 panel.lua（+0 -1）",
				"diff":   diff,
			}}, MediaType: render.MediaToolResult},
		},
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 980, 620)
	pump(tester, p)
	clickProcessGroup(t, tester)
	pump(tester, p)
	clickToolRow(t, tester, "edit")
	pump(tester, p)

	all := strings.Join(tester.Texts(), "\n")
	// diff 的上下文行与删除行都要出现
	for _, want := range []string{"function Foo", "Baz", "panel.lua"} {
		if !strings.Contains(all, want) {
			t.Fatalf("diff 应显示 %q：%v", want, tester.Texts())
		}
	}
	// 行号要对齐出现（同一行里既有旧行号也有新行号）
	foundNumbered := false
	for _, text := range tester.Texts() {
		if strings.Contains(text, "3") && strings.Contains(text, "-") &&
			strings.Contains(text, "Baz") {
			foundNumbered = true
		}
	}
	if !foundNumbered {
		t.Fatalf("删除行应带行号与 - 标记：%v", tester.Texts())
	}
}