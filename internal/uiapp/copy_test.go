package uiapp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/uiapp"
)

// 这组测试固定第 3 项需求：文字可选中复制、每条消息的复制按钮、代码块的复制按钮。
//
// 「复制整条消息」复制的是**markdown 原文**（不是渲染后的文字），且不含推理、
// 工具调用/结果与图片；代码块按钮只复制那一段代码（不带围栏与语言标记）。

// appWithParts 构造一个带给定 agent 消息的界面。
func appWithParts(t *testing.T, parts ...message.Part) (*uiapp.App, *poster) {
	t.Helper()
	app, _, p := newApp(t)
	if _, err := app.Current().Conversation().AddMessage(message.Message{
		MessageID: "a1", Role: message.RoleAgent, Parts: parts,
	}, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	return app, p
}

// 消息复制按钮：复制所有正文的 markdown 原文，不含推理与工具内容。
func TestMessageCopyButtonCopiesMarkdownSource(t *testing.T) {
	reasoning := message.Part{Content: message.TextPart{Text: "内部推理不该被复制"}, MediaType: render.MediaReasoning}
	copyFeedbackBak := uiapp.CopyFeedbackForTest()
	uiapp.SetCopyFeedbackForTest(10 * time.Millisecond)
	defer uiapp.SetCopyFeedbackForTest(copyFeedbackBak)

	app, p := appWithParts(t,
		reasoning,
		message.NewTextPart("第一段 **markdown**"),
		message.NewTextPart("- 列表项"),
		message.Part{Content: message.DataPart{Data: map[string]any{
			"id": "c1", "name": "read", "args": map[string]any{"file_path": "a.txt"},
		}}, MediaType: render.MediaToolUse},
		message.Part{Content: message.DataPart{Data: map[string]any{
			"id": "c1", "name": "read", "output": "工具输出不该被复制", "is_error": false,
		}}, MediaType: render.MediaToolResult},
	)
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)

	if err := tester.Click(locCopyAnswer); err != nil {
		t.Fatalf("点不到复制按钮：%v", err)
	}
	pump(tester, p)
	got := tester.Clipboard()
	want := "第一段 **markdown**\n\n- 列表项"
	if got != want {
		t.Fatalf("剪贴板内容不对：\n got=%q\nwant=%q", got, want)
	}
	if strings.Contains(got, "推理") || strings.Contains(got, "工具输出") {
		t.Fatalf("不该复制推理或工具内容：%q", got)
	}
}

// 代码块复制：只复制代码本身（不带 ``` 与语言标记），多个代码块各复制各的。
func TestCodeBlockCopyButtonCopiesCodeOnly(t *testing.T) {
	app, p := appWithParts(t, message.NewTextPart(
		"先看第一段：\n\n```go\nfmt.Println(\"one\")\n```\n\n再看第二段：\n\n```python\nprint('two')\n```\n"))
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)

	// 每个代码块一个按钮（无障碍标签里带语言，便于区分）
	if err := tester.Click("复制代码 go"); err != nil {
		t.Fatalf("点不到第一段代码的复制按钮：%v", err)
	}
	pump(tester, p)
	if got := tester.Clipboard(); got != "fmt.Println(\"one\")" {
		t.Fatalf("第一段代码复制不对：%q", got)
	}

	if err := tester.Click("复制代码 python"); err != nil {
		t.Fatalf("点不到第二段代码的复制按钮：%v", err)
	}
	pump(tester, p)
	if got := tester.Clipboard(); got != "print('two')" {
		t.Fatalf("第二段代码复制不对：%q", got)
	}
}

// 正文可以选中、Ctrl/Cmd+C 能复制（mygo 的只读编辑器）。
func TestMessageTextIsSelectable(t *testing.T) {
	app, p := appWithParts(t, message.NewTextPart("选我这段文字"))
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)

	rect, ok := tester.Find("选我这段文字")
	if !ok {
		t.Fatalf("没找到正文：%v", tester.Texts())
	}
	// 双击选词 + 复制：选择只在**单个文本元素内**生效（v1 限制）
	tester.ClickAt(rect.X+rect.W/2, rect.Y+rect.H/2)
	tester.ClickAt(rect.X+rect.W/2, rect.Y+rect.H/2)
	tester.Key(ui.Cmd, ui.KeyC)
	pump(tester, p)
	if got := tester.Clipboard(); got == "" {
		t.Fatalf("双击选中后应能复制：%v", tester.Texts())
	}
	tester.Key(ui.Cmd, ui.KeyA)
	tester.Key(ui.Cmd, ui.KeyC)
	pump(tester, p)
	if got := tester.Clipboard(); !strings.Contains(got, "选我这段文字") {
		t.Fatalf("全选复制内容不对：%q", got)
	}
}

// 复制按钮点击后短暂显示「已复制」，随后恢复。
// 复制按钮点击后，图标短暂变成对勾（无障碍名也从「复制回答」变成「已复制回答」），
// 随后恢复。
func TestCopyButtonShowsFeedback(t *testing.T) {
	bak := uiapp.CopyFeedbackForTest()
	uiapp.SetCopyFeedbackForTest(300 * time.Millisecond)
	defer uiapp.SetCopyFeedbackForTest(bak)

	app, p := appWithParts(t, message.NewTextPart("hello"))
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	if err := tester.Click(locCopyAnswer); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	// 反馈是**图标**换成对勾：可见文字里没有"已复制"，
	// 但无障碍名会变（读屏靠它播报），这里按无障碍名断言。
	if _, ok := tester.Find(locCopiedAnswer); !ok {
		t.Fatalf("点击后按钮应进入已复制状态：clipboard=%q texts=%v", tester.Clipboard(), tester.Texts())
	}
	// 到期后恢复（c.After 会请求一帧）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pump(tester, p)
		if _, ok := tester.Find(locCopiedAnswer); !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("「已复制」状态应在到期后恢复：%v", tester.Texts())
}
