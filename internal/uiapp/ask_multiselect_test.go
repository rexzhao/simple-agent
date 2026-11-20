package uiapp_test

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/uiapp"
)

// 端到端：界面上勾选多选的**多个**选项，点「提交」，模型要看到全部选项。
//
// 用户报的："ask question 多选依旧没有发给 llm"。
func TestAskMultiSelectReachesModel(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	askWithQuestions(t, app, rt, p, tester, `{"questions":[{"id":"q1","question":"选哪些？","multiSelect":true,"options":[{"label":"选项甲"},{"label":"选项乙"},{"label":"选项丙"}]}]}`)

	// 勾两个
	if err := tester.Click("选项甲"); err != nil {
		t.Fatalf("点不到选项甲：%v（界面：%v）", err, tester.Texts())
	}
	tester.Frame()
	pump(tester, p)
	if err := tester.Click("选项丙"); err != nil {
		t.Fatalf("点不到选项丙：%v（界面：%v）", err, tester.Texts())
	}
	tester.Frame()
	pump(tester, p)

	// 提交前先确认 driver 里记着两个选择
	groups := app.Current().AskGroups()
	if len(groups) != 1 {
		t.Fatalf("应有 1 个提问组，实际 %d", len(groups))
	}
	if !groups[0].Selected(0, 0) || !groups[0].Selected(0, 2) {
		t.Fatalf("界面上勾选后 driver 应记着选项 0 和 2（实际 0=%v 2=%v）",
			groups[0].Selected(0, 0), groups[0].Selected(0, 2))
	}
	// 这就是提交后模型会看到的工具结果 —— 提交前算好存着
	expected := groups[0].Output(driver.AskSubmit, "")

	if err := tester.Click(locSubmit); err != nil {
		t.Fatalf("点不到提交：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)

	output := askToolResult(t, app)
	for _, want := range []string{"选项甲", "选项丙"} {
		if !strings.Contains(output, want) {
			t.Fatalf("模型应收到 %q，实际：%s", want, output)
		}
	}
	if strings.Contains(output, "选项乙") {
		t.Fatalf("没勾的「选项乙」不该出现：%s", output)
	}
	_ = expected
}

// 多个问题时，必须**全部作答**才能提交。
//
// 用户的要求："如果问题有多个，所有选完 submit 按钮才亮"。
func TestAskSubmitNeedsAllQuestionsAnswered(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	askWithQuestions(t, app, rt, p, tester, `{"questions":[
		{"id":"q1","question":"第一个问题？","options":[{"label":"一级甲"},{"label":"一级乙"}]},
		{"id":"q2","question":"第二个问题？","options":[{"label":"二级甲"},{"label":"二级乙"}]}]}`)

	session := app.Current()
	// 只答第一题
	if err := tester.Click("一级甲"); err != nil {
		t.Fatalf("点不到一级甲：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)

	// 两题没答全时提交：不应提交（仍在等待回答）
	if err := tester.Click(locSubmit); err == nil {
		pump(tester, p)
		if session.Busy() != driver.BusyAwaitingAnswer {
			t.Fatalf("只答了第 1 题就提交，不该继续（状态 %v）", session.Busy())
		}
	}

	// 答第二题
	clickAskTab(t, tester, 1)
	if err := tester.Click("二级甲"); err != nil {
		t.Fatalf("点不到二级甲：%v（界面：%v）", err, tester.Texts())
	}
	pump(tester, p)

	// 现在提交应生效：退出等待回答
	if err := tester.Click(locSubmit); err != nil {
		t.Fatalf("点不到提交：%v", err)
	}
	pump(tester, p)
	if session.Busy() == driver.BusyAwaitingAnswer {
		t.Fatalf("两题都答了，提交应生效（界面：%v）", tester.Texts())
	}

	output := askToolResult(t, app)
	for _, want := range []string{"一级甲", "二级甲"} {
		if !strings.Contains(output, want) {
			t.Fatalf("模型应收到 %q，实际：%s", want, output)
		}
	}
}

// askWithQuestions 让模型发起一次提问（argsJSON 是 ask_user_question 的参数）。
func askWithQuestions(t *testing.T, app *uiapp.App, rt *fakeRuntime, p *poster,
	tester *ui.Tester, argsJSON string) {
	t.Helper()
	clickComposer(t, tester)
	tester.Type("问我")
	tester.Frame()
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "打开流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 1
	})
	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: argsJSON},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	waitFor(t, tester, p, "进入等待回答", func() bool {
		return app.Current().Busy() == driver.BusyAwaitingAnswer
	})
	// 事件是异步投递的：状态到位后 AskGroups 可能还差一帧
	waitFor(t, tester, p, "提问组就绪", func() bool {
		return len(app.Current().AskGroups()) > 0
	})
}

// askToolResult 从会话里读 ask_user_question 的工具结果（模型看到的那段）。
//
// 提交后提问组已被消费，所以这里翻消息里的 part —— 那才是真正发出去的内容。
func askToolResult(t *testing.T, app *uiapp.App) string {
	t.Helper()
	for _, entry := range app.Current().Conversation().Entries() {
		chat := entry.Message
		if chat == nil || chat.Message == nil {
			continue
		}
		for _, part := range chat.Message.Parts {
			data, ok := part.Content.(message.DataPart)
			if !ok {
				continue
			}
			m, ok := data.Data.(map[string]any)
			if !ok {
				continue
			}
			if name, _ := m["name"].(string); name != "ask_user_question" {
				continue
			}
			if output, _ := m["output"].(string); output != "" {
				return output
			}
		}
	}
	t.Fatal("没找到 ask_user_question 的工具结果")
	return ""
}

// clickAskTab 切到第 tab 个问题的页签（0 基）。
//
// 页签文字是 Header；没给 Header 时用「问题 N」默认名。
func clickAskTab(t *testing.T, tester *ui.Tester, tab int) {
	t.Helper()
	label := "问题 " + itoa(tab+1)
	if err := tester.Click(label); err != nil {
		t.Fatalf("点不到页签 %q：%v（界面：%v）", label, err, tester.Texts())
	}
}

// itoa 是小整数转字符串（只用于页签编号）。
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}