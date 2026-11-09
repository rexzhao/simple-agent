package uiapp_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
	"simple-agent/internal/uiapp"
)

// 这组测试固定「插话（steer）」在界面上的行为：
//
//   - 回合进行中输入框仍然可用，回车 = 排队而不是发送；
//   - 排队内容显示在输入卡上方，可以单条撤回；
//   - 投递后它变成真正的用户消息出现在会话里；
//   - 「等待回答」时回车仍然作答（不能被插话抢走）。

// countUserMessages 统计会话里的用户消息条数。
func countUserMessages(app *uiapp.App) int {
	count := 0
	for _, entry := range app.Current().Conversation().Entries() {
		if entry.Message != nil && entry.Message.Message.Role == message.RoleUser {
			count++
		}
	}
	return count
}

// sendFirst 输入并发送第一条消息，等到流打开。
func sendFirst(t *testing.T, tester *ui.Tester, rt *fakeRuntime, p *poster, text string) {
	t.Helper()
	clickComposer(t, tester)
	tester.Type(text)
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatalf("点不到发送：%v", err)
	}
	waitFor(t, tester, p, "打开流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 1
	})
}

// 流式中回车：排队 + 清空草稿，状态不变。
func TestSteerQueuesWhileStreaming(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	sendFirst(t, tester, rt, p, "第一句")

	session := app.Current()
	if session.Busy() != driver.BusyStreaming {
		t.Fatalf("应为 streaming，实际 %v", session.Busy())
	}
	userBefore := countUserMessages(app)

	// 流式中继续输入
	clickComposer(t, tester)
	tester.Type("顺便说一句")
	tester.Frame()
	// 发送按钮的语义应变成「插话」（无障碍名）
	if !tester.HasText(locSteer) {
		t.Fatalf("忙时应显示插话语义：%v", tester.Texts())
	}
	tester.Key(0, ui.KeyEnter)
	waitFor(t, tester, p, "出现排队提示", func() bool {
		return tester.HasText("待插入（1 条，下一轮请求前发送）")
	})

	if got := session.QueuedForTest(); len(got) != 1 || got[0] != "顺便说一句" {
		t.Fatalf("队列内容不一致：%#v", got)
	}
	// 排队消息**还没进会话**
	if after := countUserMessages(app); after != userBefore {
		t.Fatalf("排队期间不应追加用户消息：%d → %d", userBefore, after)
	}
	if session.Busy() != driver.BusyStreaming {
		t.Fatalf("排队不应改变状态，实际 %v", session.Busy())
	}
	// 草稿已清空：文本只留在排队区
	if strings.Count(strings.Join(tester.Texts(), "|"), "顺便说一句") != 1 {
		t.Fatalf("草稿应被清空、只留在排队区：%v", tester.Texts())
	}
}

// 点排队行的 × 撤回。
func TestSteerDropQueuedFromUI(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	sendFirst(t, tester, rt, p, "开始")

	clickComposer(t, tester)
	tester.Type("要撤回的")
	tester.Frame()
	tester.Key(0, ui.KeyEnter)
	waitFor(t, tester, p, "出现排队提示", func() bool {
		return tester.HasText("待插入（1 条，下一轮请求前发送）")
	})

	if err := tester.Click("×"); err != nil {
		t.Fatalf("点不到撤回按钮：%v（界面：%v）", err, tester.Texts())
	}
	waitFor(t, tester, p, "排队区消失", func() bool {
		return !tester.HasText("待插入（1 条，下一轮请求前发送）")
	})
	if got := app.Current().QueuedForTest(); len(got) != 0 {
		t.Fatalf("队列应为空：%#v", got)
	}
}

// 投递：工具轮之间把插话送进会话，并出现在下一轮请求里。
func TestSteerDeliveredBetweenToolRoundsInUI(t *testing.T) {
	app, rt, p := newApp(t)
	// 用 gate 把「执行工具中」这段状态撑住：否则工具瞬间返回，测试还没来得及
	// 打字就已经进入下一轮流了（时序不稳）。
	gate := make(chan struct{})
	rt.runTools = func(ctx context.Context, _ *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
		<-gate
		outcomes := make([]tools.ToolOutcome, 0, len(calls))
		for _, call := range calls {
			outcomes = append(outcomes, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "工具结果"})
		}
		return outcomes
	}
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()

	tester := ui.NewTester(app.View, 900, 620)
	sendFirst(t, tester, rt, p, "跑个工具")

	// 模型要调工具
	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	waitFor(t, tester, p, "进入执行工具", func() bool {
		return app.Current().Busy() == driver.BusyRunningTools
	})

	// 工具执行中插话
	clickComposer(t, tester)
	tester.Type("改成看 b.txt")
	tester.Frame()
	tester.Key(0, ui.KeyEnter)
	waitFor(t, tester, p, "排队", func() bool {
		return len(app.Current().QueuedForTest()) == 1
	})
	if got := app.Current().QueuedForTest()[0]; got != "改成看 b.txt" {
		t.Fatalf("队列内容不一致：%q", got)
	}
	// 还没投递：会话里不该出现这条消息
	if tester.HasText("改成看 b.txt") && app.Current().Busy() != driver.BusyRunningTools {
		t.Fatalf("工具还没跑完就投递了：%v", tester.Texts())
	}

	// 放行工具 → 本批结果齐了 → 插话投递，开下一轮流
	close(gate)
	waitFor(t, tester, p, "开下一轮流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 2
	})
	rt.mu.Lock()
	second := rt.requests[len(rt.requests)-1]
	rt.mu.Unlock()
	found := false
	for _, msg := range second.Messages {
		if strings.Contains(msg.PreviewText(), "改成看 b.txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("下一轮请求里应包含插话：%#v", second.Messages)
	}
	if len(app.Current().QueuedForTest()) != 0 {
		t.Fatalf("投递后队列应为空")
	}
	if !tester.HasText("改成看 b.txt") {
		t.Fatalf("插话应作为用户消息出现在会话里：%v", tester.Texts())
	}
}

// 「等待回答」时回车仍然是作答，不会被插话抢走。
func TestSteerDoesNotStealAwaitingAnswer(t *testing.T) {
	app, rt, p := newApp(t)
	rt.runTools = func(_ context.Context, _ *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
		outcomes := make([]tools.ToolOutcome, 0, len(calls))
		for _, call := range calls {
			outcomes = append(outcomes, tools.ToolOutcome{
				ID: call.ID, Name: call.Name,
				Pending: []tools.QuestionSpec{{Question: "选一个", Options: []tools.QuestionOption{{Label: "A"}}}},
			})
		}
		return outcomes
	}

	tester := ui.NewTester(app.View, 900, 620)
	sendFirst(t, tester, rt, p, "问我")
	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask1", Name: "ask_user_question"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0,
			Delta: `{"questions":[{"question":"选一个","options":[{"label":"A"}]}]}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	waitFor(t, tester, p, "进入等待回答", func() bool {
		return app.Current().Busy() == driver.BusyAwaitingAnswer
	})

	clickComposer(t, tester)
	tester.Type("我自己写的答案")
	tester.Frame()
	if app.Current().CanSteerForTest() {
		t.Fatal("等待回答时不应可插话")
	}
	tester.Key(0, ui.KeyEnter)
	// 回车应当作答（继续一轮），而不是进队列
	waitFor(t, tester, p, "作答后继续一轮", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 2
	})
	if got := app.Current().QueuedForTest(); len(got) != 0 {
		t.Fatalf("等待回答时的输入不应进插话队列：%#v", got)
	}
}

// 停止时清空队列：界面上排队区消失，也不再自动投递。
func TestSteerClearedOnStopInUI(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	sendFirst(t, tester, rt, p, "开始")

	clickComposer(t, tester)
	tester.Type("别做了")
	tester.Frame()
	tester.Key(0, ui.KeyEnter)
	waitFor(t, tester, p, "排队", func() bool {
		return tester.HasText("待插入（1 条，下一轮请求前发送）")
	})

	if err := tester.Click(locStop); err != nil {
		t.Fatalf("点不到停止：%v", err)
	}
	waitFor(t, tester, p, "回到 idle", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})
	if got := app.Current().QueuedForTest(); len(got) != 0 {
		t.Fatalf("停止应清空队列：%#v", got)
	}
	if tester.HasText("待插入（1 条，下一轮请求前发送）") {
		t.Fatalf("排队区应消失：%v", tester.Texts())
	}
	// 停止后不应自动再开流
	rt.mu.Lock()
	count := len(rt.streams)
	rt.mu.Unlock()
	if count != 1 {
		t.Fatalf("停止后不应自动投递：%d 条流", count)
	}
}

// 压缩中停止 = 停止：插话队列被清空（与自动压缩失败保留队列不同）。
func TestStopWhileCompactingClearsQueue(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)

	// 两个回合（手动压缩要求历史里有可折叠的内容）
	sendFirst(t, tester, rt, p, "第一句")
	rt.push(t,
		providers.Event{Kind: providers.EventText, Text: "好的"},
		providers.Event{Kind: providers.EventDone, StopReason: "stop"},
	)
	pump(tester, p)
	waitFor(t, tester, p, "第一轮结束", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})
	clickComposer(t, tester)
	tester.Type("第二句")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "第二轮流", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.streams) == 2
	})

	// 生成中插话，然后这一轮失败 → 回到 Idle，队列**保留**
	clickComposer(t, tester)
	tester.Type("顺便看下 README")
	tester.Frame()
	tester.Key(0, ui.KeyEnter)
	waitFor(t, tester, p, "排队", func() bool {
		return tester.HasText("待插入（1 条")
	})
	rt.push(t, providers.Event{Kind: providers.EventError, Err: errors.New("boom")})
	waitFor(t, tester, p, "失败后回到 idle", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})
	if got := app.Current().QueuedForTest(); len(got) != 1 {
		t.Fatalf("失败后队列应保留：%#v", got)
	}

	// 队列还在的情况下手动压缩 → 进入压缩态
	if err := tester.Click(locMore); err != nil {
		t.Fatalf("点不到更多：%v", err)
	}
	if err := tester.ChooseMenuItem(locCompact); err != nil {
		t.Fatalf("选不到压缩：%v（菜单：%v）", err, tester.Menu())
	}
	waitFor(t, tester, p, "进入压缩", func() bool {
		return app.Current().Busy() == driver.BusyCompacting
	})

	// 压缩中点停止
	if err := tester.Click(locStop); err != nil {
		t.Fatalf("点不到停止：%v", err)
	}
	waitFor(t, tester, p, "回到 idle", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})
	if got := app.Current().QueuedForTest(); len(got) != 0 {
		t.Fatalf("压缩中停止应清空队列：%#v", got)
	}
}

// 忙时输入框为空、队列非空：「插话」按钮也不该可点（点了没有反应）。
func TestSteerButtonDisabledWhenEmptyWhileBusy(t *testing.T) {
	app, rt, p := newApp(t)
	tester := ui.NewTester(app.View, 900, 620)
	sendFirst(t, tester, rt, p, "开始")

	// 先排一条插话（此时草稿已清空）
	clickComposer(t, tester)
	tester.Type("排队的插话")
	tester.Frame()
	tester.Key(0, ui.KeyEnter)
	waitFor(t, tester, p, "排队", func() bool {
		return tester.HasText("待插入（1 条")
	})
	pump(tester, p)

	// 草稿空 + 队列非空 + 忙：按钮应禁用（点了 Send 会直接 return，等于没反应）
	if !app.SendDisabledForTest() {
		t.Fatalf("空输入时「插话」按钮应禁用（界面：%v）", tester.Texts())
	}

	// 回到空闲后，同样空输入 + 队列非空 → 可以点（把队列发出去）
	rt.push(t, providers.Event{Kind: providers.EventError, Err: errors.New("boom")})
	waitFor(t, tester, p, "失败后回到 idle", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatalf("空闲时队列非空应可发送：%v", err)
	}
	waitFor(t, tester, p, "队列被发出去", func() bool {
		return len(app.Current().QueuedForTest()) == 0
	})
}
