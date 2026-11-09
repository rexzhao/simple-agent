package uiapp_test

import (
	"context"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/locale"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
	"simple-agent/internal/uiapp"
)

// todoRuntime 在工具轮里执行真实的 todo_write 工具。
//
// 用真工具而不是假的 RunTools：清单同步发生在**工具执行之后**
// （host 读 ToolContext.Todos()），假实现会绕开这条链路，测不到真正会坏的地方。
type todoRuntime struct {
	*fakeRuntime
	todos []map[string]any
}

func (r *todoRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	out := make([]tools.ToolOutcome, 0, len(calls))
	for _, call := range calls {
		if call.Name != "todo_write" {
			out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "ok"})
			continue
		}
		tool, ok := tools.Find(tools.Builtin(false), "todo_write")
		if !ok {
			out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "缺工具", IsError: true})
			continue
		}
		// 参数要包成 []any：工具层的 Args.Items 只认这个类型（JSON 解出来的形态）
		raw := make([]any, 0, len(r.todos))
		for _, todo := range r.todos {
			raw = append(raw, todo)
		}
		result, err := tool.Run(ctx, tools.Args{"todos": raw}, tc)
		outcome := tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: result.Output}
		if err != nil {
			outcome.Output, outcome.IsError = err.Error(), true
		}
		out = append(out, outcome)
	}
	return out
}

// newTodoApp 起一个界面 + 自定义 runtime（清单由真工具写入）。
func newTodoApp(t *testing.T, todos []map[string]any) (*uiapp.App, *todoRuntime, *poster) {
	t.Helper()
	rt := &todoRuntime{fakeRuntime: &fakeRuntime{}, todos: todos}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:             "你是助手",
		Workspace:                t.TempDir(),
		Title:                    "测试会话",
		MaxToolRounds:            3,
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
		Post:                     p.Post,
		ContextWindow:            testContextWindow,
	})
	return app, rt, p
}

// sendText 走真实界面路径发送（点输入框 → 输入 → 点发送按钮）。
func sendText(t *testing.T, tester *ui.Tester, text string) {
	t.Helper()
	clickComposer(t, tester)
	tester.Type(text)
	tester.Frame()
	if err := tester.Click(locSend); err != nil {
		t.Fatalf("点不到发送按钮：%v", err)
	}
}

// startWithTodoWrite 发送一条消息，让模型发起一次 todo_write。
func startWithTodoWrite(t *testing.T, rt *todoRuntime, p *poster, tester *ui.Tester, text string) {
	t.Helper()
	sendText(t, tester, text)
	waitFor(t, tester, p, "开始第一轮流", func() bool { return rt.streamCount() >= 1 })
	// 工具调用分片 → 回合以 tool_calls 结束（push 遇 Done 会关通道，所以分两次）
	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "call-1", Name: "todo_write"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"todos":[]}`},
	)
	stream := rt.awaitStream(t)
	stream.events <- providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"}
	stream.close()
}

// finishTurn 等工具轮回填后推完第二条流。
func finishTurn(t *testing.T, rt *todoRuntime, p *poster, tester *ui.Tester) {
	t.Helper()
	waitFor(t, tester, p, "工具轮之后开第二轮", func() bool { return rt.streamCount() >= 2 })
	rt.push(t,
		providers.Event{Kind: providers.EventText, Text: "清单已更新"},
		providers.Event{Kind: providers.EventDone, StopReason: "stop"},
	)
}

// 任务清单面板：有清单时出现在输入卡上方，默认**折叠**（只显示一行头部）。
//
// 默认折叠对齐 dsh（`useState(true)`）：展开状态不落盘，每次启动都是收起的。
func TestTodoPanelCollapsedByDefault(t *testing.T) {
	app, rt, p := newTodoApp(t, []map[string]any{
		{"content": "读需求", "status": "completed"},
		{"content": "写代码", "status": "in_progress"},
		{"content": "跑测试", "status": "pending"},
	})
	tester := ui.NewTester(app.View, 1200, 800)
	pump(tester, p)

	startWithTodoWrite(t, rt, p, tester, "开始")
	waitFor(t, tester, p, "清单写入", func() bool { return len(app.Current().Todos()) == 3 })
	finishTurn(t, rt, p, tester)
	pump(tester, p)

	if !tester.HasText("任务") {
		t.Fatalf("应显示清单标题：%v", tester.Texts())
	}
	progress := findText(tester.Texts(), "1 已完成")
	if progress == "" {
		t.Fatalf("应显示完成数：%v", tester.Texts())
	}
	if !strings.Contains(progress, "1 进行中") || !strings.Contains(progress, "1 待处理") {
		t.Fatalf("统计应包含进行中与待处理：%q", progress)
	}
	if !strings.Contains(progress, "\u2002·\u2002") {
		t.Fatalf("统计分隔符应用 U+2002 宽空格（dsh 的做法）：%q", progress)
	}
	// 折叠时不显示具体条目
	for _, content := range []string{"读需求", "写代码", "跑测试"} {
		if tester.HasText(content) {
			t.Fatalf("默认折叠不应显示条目 %q：%v", content, tester.Texts())
		}
	}
}

// 点击头部展开后能看到每条内容。
func TestTodoPanelExpandsOnClick(t *testing.T) {
	app, rt, p := newTodoApp(t, []map[string]any{
		{"content": "第一个任务", "status": "in_progress"},
		{"content": "第二个任务", "status": "pending"},
	})
	tester := ui.NewTester(app.View, 1200, 800)
	pump(tester, p)

	startWithTodoWrite(t, rt, p, tester, "开始")
	waitFor(t, tester, p, "清单写入", func() bool { return len(app.Current().Todos()) == 2 })
	finishTurn(t, rt, p, tester)
	pump(tester, p)

	if err := tester.Click("展开或折叠任务清单"); err != nil {
		t.Fatalf("找不到清单头部：%v（文本：%v）", err, tester.Texts())
	}
	pump(tester, p)

	if !tester.HasText("第一个任务") || !tester.HasText("第二个任务") {
		t.Fatalf("展开后应显示全部条目：%v", tester.Texts())
	}
}

// 没有清单时面板完全不渲染（dsh 的 `todos.length === 0 → null`）。
func TestTodoPanelHiddenWithoutTodos(t *testing.T) {
	app, _, p := newApp(t)
	tester := ui.NewTester(app.View, 1200, 800)
	pump(tester, p)

	if tester.HasText("任务") {
		t.Fatalf("没有清单时不该出现面板：%v", tester.Texts())
	}
}

// 新一轮用户输入会清空清单（对齐 dsh 在 turn/start 清空）。
func TestTodoPanelClearsOnNextMessage(t *testing.T) {
	app, rt, p := newTodoApp(t, []map[string]any{
		{"content": "这一轮的任务", "status": "pending"},
	})
	tester := ui.NewTester(app.View, 1200, 800)
	pump(tester, p)

	startWithTodoWrite(t, rt, p, tester, "第一轮")
	waitFor(t, tester, p, "清单写入", func() bool { return len(app.Current().Todos()) == 1 })
	finishTurn(t, rt, p, tester)
	waitFor(t, tester, p, "回到空闲", func() bool { return app.Current().CanSend() })

	sendText(t, tester, "第二轮")
	waitFor(t, tester, p, "清单清空", func() bool { return len(app.Current().Todos()) == 0 })
	pump(tester, p)
	if tester.HasText("这一轮的任务") {
		t.Fatalf("新一轮不该显示上一轮的任务：%v", tester.Texts())
	}
}

// findText 找出包含子串的那条可见文本。
func findText(texts []string, sub string) string {
	for _, text := range texts {
		if strings.Contains(text, sub) {
			return text
		}
	}
	return ""
}
