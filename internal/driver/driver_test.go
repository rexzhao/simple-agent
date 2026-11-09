package driver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/tools"
)

// fakeProvider 是测试替身：只用来满足 providers.Provider（driver 不直接调用它，
// 请求由调用方按 Effect 发起）。
type fakeProvider struct{}

func (fakeProvider) Config() providers.ProviderConfig {
	return providers.ProviderConfig{ID: "fake", Kind: providers.KindOpenAI}
}

func (fakeProvider) Stream(context.Context, providers.ChatRequest) (<-chan providers.Event, error) {
	ch := make(chan providers.Event)
	close(ch)
	return ch, nil
}

var _ providers.Provider = fakeProvider{}

// newDriver 构造一个空闲 driver（工作区用临时目录）。
func newDriver(t *testing.T, allowShell bool) (*driver.Driver, *conversation.Conversation) {
	t.Helper()
	conv := conversation.New("ctx-1")
	tc := tools.NewToolContext(t.TempDir(), allowShell)
	opts := driver.Options{
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(allowShell),
	}
	d := driver.New(conv, opts)
	_ = tc
	return d, conv
}

// startStream 取出 StartStream effect。
func startStream(t *testing.T, effects []driver.Effect) driver.StartStream {
	t.Helper()
	for _, e := range effects {
		if s, ok := e.(driver.StartStream); ok {
			return s
		}
	}
	t.Fatalf("没有 StartStream effect：%#v", effects)
	return driver.StartStream{}
}

// runTools 取出 RunTools effect。
func runTools(t *testing.T, effects []driver.Effect) driver.RunTools {
	t.Helper()
	for _, e := range effects {
		if r, ok := e.(driver.RunTools); ok {
			return r
		}
	}
	t.Fatalf("没有 RunTools effect：%#v", effects)
	return driver.RunTools{}
}

func TestSendStartsStream(t *testing.T) {
	d, conv := newDriver(t, false)

	effects, err := d.Send("你好")
	if err != nil {
		t.Fatalf("发送应成功：%v", err)
	}
	start := startStream(t, effects)
	if d.Busy() != driver.BusyStreaming {
		t.Fatalf("状态应为 streaming，实际 %v", d.Busy())
	}
	if !d.OwnsStream(start.MessageID) {
		t.Fatal("StartStream 的 messageID 应属于当前流")
	}
	if start.Request.Model != "m" || start.Request.System != "你是助手" {
		t.Fatalf("请求构造不一致：%#v", start.Request)
	}
	if len(start.Request.Messages) != 1 || start.Request.Messages[0].Role != message.RoleUser {
		t.Fatalf("首次请求应只含用户消息：%#v", start.Request.Messages)
	}
	if len(start.Request.Tools) == 0 {
		t.Fatal("应带上工具声明")
	}
	if len(conv.Entries()) != 2 {
		t.Fatalf("应有用户消息 + agent 消息，实际 %d", len(conv.Entries()))
	}
}

func TestSendRejectedWhileBusy(t *testing.T) {
	d, conv := newDriver(t, false)
	if _, err := d.Send("第一条"); err != nil {
		t.Fatal(err)
	}
	before := len(conv.Entries())

	// 忙时：既不追加消息，也不开流（原子性）
	if _, err := d.Send("第二条"); err == nil {
		t.Fatal("忙时发送应被拒绝")
	}
	if after := len(conv.Entries()); after != before {
		t.Fatalf("忙时不应追加消息：%d → %d", before, after)
	}
}

func TestTextDeltasMergeIntoOnePart(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("hi")
	start := startStream(t, effects)

	for _, delta := range []string{"你", "好", "，世界"} {
		if _, err := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: delta}); err != nil {
			t.Fatal(err)
		}
	}
	entry, ok := conv.Message(start.MessageID)
	if !ok {
		t.Fatal("找不到 agent 消息")
	}
	if entry.PartCount() != 1 {
		t.Fatalf("增量应合并成一个 part，实际 %d", entry.PartCount())
	}
	if entry.PreviewText() != "你好，世界" {
		t.Fatalf("文本不一致：%q", entry.PreviewText())
	}
	if entry.Status() != conversation.StatusStreaming {
		t.Fatalf("收到增量后应为 streaming，实际 %v", entry.Status())
	}
}

func TestDoneCompletesAndGoesIdle(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("hi")
	start := startStream(t, effects)

	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "答案"})
	if _, err := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"}); err != nil {
		t.Fatal(err)
	}

	if d.Busy() != driver.BusyIdle {
		t.Fatalf("结束后应为 idle，实际 %v", d.Busy())
	}
	entry, _ := conv.Message(start.MessageID)
	if entry.Status() != conversation.StatusComplete {
		t.Fatalf("应为 complete，实际 %v", entry.Status())
	}
	// 只有 complete 的消息进历史
	history := conv.History()
	if len(history) != 2 {
		t.Fatalf("历史应有 2 条，实际 %d", len(history))
	}
}

// §9 第 3 条：迟到的流事件不得影响下一轮。
func TestLateEventsFromPreviousStreamIgnored(t *testing.T) {
	d, conv := newDriver(t, false)
	first, _ := d.Send("第一轮")
	firstStart := startStream(t, first)
	d.ApplyEvent(firstStart.MessageID, providers.Event{Kind: providers.EventText, Text: "回答一"})
	d.ApplyEvent(firstStart.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	second, _ := d.Send("第二轮")
	secondStart := startStream(t, second)

	// 上一轮的迟到 fail / 增量
	d.ApplyEvent(firstStart.MessageID, providers.Event{Kind: providers.EventError, Err: fmt.Errorf("旧连接失败")})
	d.ApplyEvent(firstStart.MessageID, providers.Event{Kind: providers.EventText, Text: "幽灵增量"})

	if d.Busy() != driver.BusyStreaming {
		t.Fatalf("迟到事件不应改变状态，实际 %v", d.Busy())
	}
	entry, _ := conv.Message(secondStart.MessageID)
	if strings.Contains(entry.PreviewText(), "幽灵") {
		t.Fatalf("迟到增量不应写入新消息：%q", entry.PreviewText())
	}
}

func TestErrorMarksFailedAndIdle(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("hi")
	start := startStream(t, effects)

	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "半句"})
	if _, err := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventError, Err: fmt.Errorf("401 invalid key")}); err != nil {
		t.Fatal(err)
	}

	if d.Busy() != driver.BusyIdle {
		t.Fatalf("失败后应回到 idle，实际 %v", d.Busy())
	}
	entry, _ := conv.Message(start.MessageID)
	if entry.Status() != conversation.StatusFailed {
		t.Fatalf("应为 failed，实际 %v", entry.Status())
	}
	if !strings.Contains(entry.FailureReason(), "401") {
		t.Fatalf("失败原因应保留：%q", entry.FailureReason())
	}
	// 失败的消息不进历史
	for _, msg := range conv.History() {
		if msg.MessageID == start.MessageID {
			t.Fatal("失败消息不应进历史")
		}
	}
}

// 流在没有终止事件的情况下关闭（连接中断）→ 判失败，不能永远停在生成中。
func TestStreamClosedMarksFailed(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("hi")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "半句"})

	effects = d.StreamClosed(start.MessageID)
	if len(effects) == 0 {
		t.Fatal("应产生 Changed effect")
	}
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("应回到 idle，实际 %v", d.Busy())
	}
	entry, _ := conv.Message(start.MessageID)
	if entry.Status() != conversation.StatusFailed {
		t.Fatalf("应判为 failed，实际 %v", entry.Status())
	}
}

// 工具轮：Done 之后进入 RunningTools，结果回填后开下一轮流。
func TestToolRoundTrip(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("读一下文件")
	start := startStream(t, effects)

	// 分片到达：start（重复带 id）→ args 两段 → end
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "call_1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "call_1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":`})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `"a.txt"}`})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolEnd, Index: 0})

	effects, err := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Busy() != driver.BusyRunningTools {
		t.Fatalf("有工具调用时应进入 runningTools，实际 %v", d.Busy())
	}

	run := runTools(t, effects)
	if len(run.Calls) != 1 {
		t.Fatalf("应有 1 个调用，实际 %d", len(run.Calls))
	}
	call := run.Calls[0]
	if call.ID != "call_1" || call.Name != "read" {
		t.Fatalf("调用不一致：%#v", call)
	}
	if call.Args["file_path"] != "a.txt" {
		t.Fatalf("分片参数应拼成合法 JSON：%#v", call.Args)
	}

	// 工具结果回填 → 开下一轮流
	effects, err = d.ToolResults(start.MessageID, []tools.ToolOutcome{
		{ID: "call_1", Name: "read", Output: "文件内容"},
	})
	if err != nil {
		t.Fatal(err)
	}
	next := startStream(t, effects)
	if next.MessageID == start.MessageID {
		t.Fatal("应开一条新的 agent 消息")
	}

	// 下一轮请求里必须带上 tool_use 与配对结果
	entry, _ := conv.Message(start.MessageID)
	if entry.Status() != conversation.StatusComplete {
		t.Fatalf("带工具的消息应为 complete，实际 %v", entry.Status())
	}
	openaiMsgs, err := providers.ToOpenAIMessages(next.Request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sawToolCall, sawToolResult bool
	for _, m := range openaiMsgs {
		if _, ok := m["tool_calls"]; ok {
			sawToolCall = true
		}
		if m["role"] == "tool" && m["tool_call_id"] == "call_1" {
			sawToolResult = true
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("请求应含配对的调用与结果：%#v", openaiMsgs)
	}
}

// §9 第 1 条：停止（流式中）必须给未配对的 tool_use 补错误结果。
func TestStopDuringStreamSettlesUnfinishedCalls(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("hi")
	start := startStream(t, effects)

	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`})
	// 没有 ToolEnd，也没有 Done：用户直接停止
	d.Stop()

	if d.Busy() != driver.BusyIdle {
		t.Fatalf("停止后应为 idle，实际 %v", d.Busy())
	}
	entry, _ := conv.Message(start.MessageID)
	if entry.Status() != conversation.StatusCancelled {
		t.Fatalf("流式中停止应为 cancelled，实际 %v", entry.Status())
	}
	// Cancelled 不进历史
	for _, msg := range conv.History() {
		if msg.MessageID == start.MessageID {
			t.Fatal("Cancelled 消息不应进历史")
		}
	}

	// 停止后还能继续用（历史里没有带病的 tool_use）
	if _, err := d.Send("继续"); err != nil {
		t.Fatalf("停止后应能继续发送：%v", err)
	}
}

// 工具执行中停止：消息已 Complete，只需补配对结果，历史可用。
func TestStopDuringToolRunSettlesCalls(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("跑个工具")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "bash"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	if d.Busy() != driver.BusyRunningTools {
		t.Fatalf("应处于 runningTools，实际 %v", d.Busy())
	}

	d.Stop()
	entry, _ := conv.Message(start.MessageID)
	// 每个 tool_use 都要有结果，否则之后每条请求都会被拒
	var uses, results int
	for _, part := range entry.Message.Parts {
		switch part.MediaType {
		case render.MediaToolUse:
			uses++
		case render.MediaToolResult:
			if data, ok := part.Content.(message.DataPart).Data.(map[string]any); ok {
				if data["is_error"] == true {
					results++
				}
			}
		}
	}
	if uses != 1 || results != 1 {
		t.Fatalf("应补 1 条错误结果：uses=%d results=%d", uses, results)
	}
	// 补了结果之后这条消息可以进历史
	if len(conv.History()) != 2 {
		t.Fatalf("历史应有 2 条，实际 %d", len(conv.History()))
	}
}

// 提问：进入 AwaitingAnswer，作答后继续；期间历史排除这轮。
func TestAskUserQuestionFlow(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("问个问题")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"})
	toolEffects, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	run := runTools(t, toolEffects)

	outcome := tools.ToolOutcome{
		ID: "ask_1", Name: "ask_user_question",
		Pending: []tools.QuestionSpec{{ID: "q1", Question: "选哪个？", Options: []tools.QuestionOption{{Label: "A"}}}},
	}
	if _, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{outcome}); err != nil {
		t.Fatal(err)
	}
	if d.Busy() != driver.BusyAwaitingAnswer {
		t.Fatalf("应进入 awaitingAnswer，实际 %v", d.Busy())
	}
	_ = run

	groups := d.AskGroups()
	if len(groups) != 1 || groups[0].CallID != "ask_1" {
		t.Fatalf("应返回待回答提问组：%#v", groups)
	}
	if len(groups[0].Questions) != 1 || groups[0].Questions[0].ID != "q1" {
		t.Fatalf("问题不一致：%#v", groups[0].Questions)
	}
	// 等待回答期间这轮不进请求历史
	if history := providers.HistoryForRequest(conv.History()); len(history) != 1 {
		t.Fatalf("含 pending 的消息应被排除，实际 %d 条", len(history))
	}

	groups[0].Toggle(0, 0)
	submitEffects, err := d.SubmitAsks("")
	if err != nil {
		t.Fatal(err)
	}
	next := startStream(t, submitEffects)
	if len(next.Request.Messages) == 0 {
		t.Fatal("继续后应带历史")
	}
	if d.Busy() != driver.BusyStreaming {
		t.Fatalf("作答后应继续流式，实际 %v", d.Busy())
	}
}

// 跳过提问：填错误结果并继续。
func TestSkipPendingContinues(t *testing.T) {
	d, _ := newDriver(t, false)
	effects, _ := d.Send("问")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})

	d.ToolResults(start.MessageID, []tools.ToolOutcome{{
		ID: "ask_1", Name: "ask_user_question",
		Pending: []tools.QuestionSpec{{ID: "q1", Question: "?"}},
	}})
	if d.Busy() != driver.BusyAwaitingAnswer {
		t.Fatalf("应等待回答，实际 %v", d.Busy())
	}

	effects, err := d.DismissAsks()
	if err != nil {
		t.Fatal(err)
	}
	startStream(t, effects)
	if d.Busy() != driver.BusyStreaming {
		t.Fatalf("跳过后应继续流式，实际 %v", d.Busy())
	}
	if d.AskGroups() != nil {
		t.Fatal("跳过后不应还有待回答")
	}
}

// 工具结果里的迟到回填不得影响新的一轮。
func TestStaleToolResultsIgnored(t *testing.T) {
	d, _ := newDriver(t, false)
	effects, _ := d.Send("hi")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	// 现在已经 idle，上一轮的结果迟到
	effects, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{{ID: "c1", Name: "read", Output: "x"}})
	if err != nil || effects != nil {
		t.Fatalf("迟到结果应被丢弃：effects=%#v err=%v", effects, err)
	}
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("状态不应改变，实际 %v", d.Busy())
	}
}

// 失败时未配对的调用也要补结果，之后能继续发请求。
func TestFailureSettlesUnfinishedCalls(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("hi")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventError, Err: fmt.Errorf("boom")})

	entry, _ := conv.Message(start.MessageID)
	var uses, results int
	for _, part := range entry.Message.Parts {
		switch part.MediaType {
		case render.MediaToolUse:
			uses++
		case render.MediaToolResult:
			results++
		}
	}
	if uses != 1 || results != 1 {
		t.Fatalf("失败时应补配对结果：uses=%d results=%d", uses, results)
	}
	if _, err := d.Send("继续"); err != nil {
		t.Fatalf("失败后应能继续：%v", err)
	}
}

// 推理内容作为独立 part 保存，且不进请求。
func TestReasoningKeptButNotSent(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("hi")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventReasoning, Text: "我在思考"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "答案"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	entry, _ := conv.Message(start.MessageID)
	var reasoning, text int
	for _, part := range entry.Message.Parts {
		switch part.MediaType {
		case render.MediaReasoning:
			reasoning++
		case "", render.MediaPlainText:
			text++
		}
	}
	if reasoning != 1 || text != 1 {
		t.Fatalf("应有 1 个推理 part 与 1 个文本 part：reasoning=%d text=%d", reasoning, text)
	}

	effects, _ = d.Send("继续")
	next := startStream(t, effects)
	// 请求体里不该出现推理文本（Extract 会跳过 MediaReasoning）
	openaiMsgs, err := providers.ToOpenAIMessages(next.Request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range openaiMsgs {
		if content, ok := m["content"].(string); ok && strings.Contains(content, "我在思考") {
			t.Fatalf("推理内容不应进请求：%q", content)
		}
	}
}

// 工具参数不是合法 JSON：不执行该调用，直接给一条写明真实原因的错误结果
// （拿 {} 去跑只会得到「缺少 xxx 参数」，把模型引向错误方向）。
func TestInvalidToolArgsNotExecuted(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("x")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":`})

	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	for _, e := range effects {
		if _, ok := e.(driver.RunTools); ok {
			t.Fatal("参数非法的调用不应被执行")
		}
	}

	entry, _ := conv.Message(start.MessageID)
	var sawUse, sawErrorResult bool
	for _, part := range entry.Message.Parts {
		data, _ := part.Content.(message.DataPart).Data.(map[string]any)
		switch part.MediaType {
		case render.MediaToolUse:
			sawUse = true
			if raw, _ := data["raw_args"].(string); !strings.Contains(raw, "file_path") {
				t.Fatalf("非法参数应保留原文：%#v", data)
			}
		case render.MediaToolResult:
			if out, _ := data["output"].(string); data["is_error"] == true && strings.Contains(out, "不是合法 JSON") {
				sawErrorResult = true
			}
		}
	}
	if !sawUse || !sawErrorResult {
		t.Fatalf("应留下调用与错误结果：use=%v result=%v", sawUse, sawErrorResult)
	}
}

func TestSelectionOnlyWhenIdle(t *testing.T) {
	d, _ := newDriver(t, false)
	if err := d.SetSelection(providers.Selection{ProviderID: "other", Model: "m2"}); err != nil {
		t.Fatalf("空闲时应允许切换：%v", err)
	}
	if _, err := d.Send("hi"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSelection(providers.Selection{ProviderID: "x", Model: "y"}); err == nil {
		t.Fatal("忙时不应允许切换模型")
	}
}
