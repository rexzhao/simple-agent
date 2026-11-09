package driver_test

import (
	"fmt"
	"strings"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// lastUserText 返回请求里最后一条 user 消息的文本。
func lastUserText(messages []message.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == message.RoleUser {
			return messages[i].PreviewText()
		}
	}
	return ""
}

// 这组测试固定「插话（steer）」的语义：
//
//   - 忙时消息只进队列，**不进 Conversation、不落盘、不开流**；
//   - 投递点是三个安全边界：工具轮之间、回合正常结束、压缩完成之后；
//   - 同一时刻排队的多条作为多个 user 消息**一次性投递**，只开一轮流；
//   - 投递后 depth 归零（插话是新的用户输入）；
//   - 停止与删除会话都清空队列。

// allUserTexts 返回**全部**（含已归档）user 消息文本，按顺序。
//
// History() 只给"当前请求历史"（压缩后更早的会被排除），断言投递顺序要用
// 全量入口。
func allUserTexts(conv *conversation.Conversation) []string {
	var out []string
	for _, entry := range conv.Entries() {
		if entry.Message == nil || entry.Message.Message.Role != message.RoleUser {
			continue
		}
		out = append(out, entry.Message.Message.PreviewText())
	}
	return out
}

// userTexts 返回历史里 user 消息的文本（按顺序）。
func userTexts(conv *conversation.Conversation) []string {
	var out []string
	for _, msg := range conv.History() {
		if msg.Role != message.RoleUser {
			continue
		}
		var text string
		for _, part := range msg.Parts {
			if value, ok := part.AsText(); ok {
				text += value
			}
		}
		out = append(out, text)
	}
	return out
}

// 流式中插话：只排队，不动会话。
func TestEnqueueWhileStreamingDoesNotTouchConversation(t *testing.T) {
	d, conv := newDriver(t, false)
	if _, err := d.Send("第一句"); err != nil {
		t.Fatal(err)
	}
	before := len(conv.Entries())

	if !d.CanSteer() {
		t.Fatalf("流式中应可插话，实际状态 %v", d.Busy())
	}
	if _, err := d.Enqueue("等一下，先看 README"); err != nil {
		t.Fatal(err)
	}
	if after := len(conv.Entries()); after != before {
		t.Fatalf("插话不应立刻追加消息：%d → %d", before, after)
	}
	if got := d.Queued(); len(got) != 1 || got[0] != "等一下，先看 README" {
		t.Fatalf("队列内容不一致：%#v", got)
	}
	if d.Busy() != driver.BusyStreaming {
		t.Fatalf("插话不应改变状态，实际 %v", d.Busy())
	}
	// 队列不落盘：这里只能断言"没有产生 Record 事件"
	for _, effect := range d.TakeRecords() {
		if _, ok := effect.(driver.Record); ok {
			t.Fatal("插话不应产生落盘事件")
		}
	}
}

// 工具轮之间投递：下一轮流之前的请求里必须能看到插话。
func TestSteerDeliveredBetweenToolRounds(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("读文件")
	start := startStream(t, effects)

	// 模型要求调用工具
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	run := runTools(t, effects)

	// 工具执行中插话
	if d.Busy() != driver.BusyRunningTools {
		t.Fatalf("应为 runningTools，实际 %v", d.Busy())
	}
	if _, err := d.Enqueue("顺便看一下 b.txt"); err != nil {
		t.Fatal(err)
	}

	effects, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{
		{ID: run.Calls[0].ID, Name: "read", Output: "内容"},
	})
	if err != nil {
		t.Fatal(err)
	}
	next := startStream(t, effects)

	// 插话已经变成真正的 user 消息，且排在工具结果之后
	texts := userTexts(conv)
	if len(texts) != 2 || texts[0] != "读文件" || texts[1] != "顺便看一下 b.txt" {
		t.Fatalf("历史里的 user 消息不一致：%#v", texts)
	}
	openaiMsgs, err := providers.ToOpenAIMessages(next.Request.Messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := openaiMsgs[len(openaiMsgs)-1]
	if last["role"] != "user" || last["content"] != "顺便看一下 b.txt" {
		t.Fatalf("下一轮请求的最后一条应是插话：%#v", last)
	}
	// 工具配对仍完整：tool 结果必须出现，且在任何插话之前
	sawToolResult := false
	for _, m := range openaiMsgs {
		if m["role"] == "tool" && m["tool_call_id"] == "c1" {
			sawToolResult = true
			break
		}
	}
	if !sawToolResult {
		t.Fatalf("请求里丢了工具结果：%#v", openaiMsgs)
	}
	if d.HasQueued() {
		t.Fatalf("投递后队列应为空：%#v", d.Queued())
	}
}

// 多条插话一次性投递：作为多个 user 消息，只开一轮流。
func TestSteerDeliversAllQueuedAsSeparateMessages(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("开始")
	start := startStream(t, effects)

	for _, text := range []string{"补充一", "补充二"} {
		if _, err := d.Enqueue(text); err != nil {
			t.Fatal(err)
		}
	}

	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "好的"})
	_ = effects
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	next := startStream(t, effects)

	texts := userTexts(conv)
	want := []string{"开始", "补充一", "补充二"}
	if len(texts) != len(want) {
		t.Fatalf("user 消息数量不一致：%#v", texts)
	}
	for i, wantText := range want {
		if texts[i] != wantText {
			t.Fatalf("第 %d 条不一致：%q，全部 %#v", i, texts[i], texts)
		}
	}
	// 只开了一轮流：请求里三条 user 消息都在
	var users []string
	for _, msg := range next.Request.Messages {
		if msg.Role == message.RoleUser {
			users = append(users, msg.PreviewText())
		}
	}
	if len(users) != 3 || users[2] != "补充二" {
		t.Fatalf("请求里的 user 消息不一致：%#v", users)
	}
}

// 回答结束后投递：回合正常结束且没有工具 → 直接开下一轮，不必再敲一次回车。
func TestSteerDeliveredAfterAnswer(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("第一问")
	start := startStream(t, effects)
	if _, err := d.Enqueue("再问一句"); err != nil {
		t.Fatal(err)
	}

	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "答"})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	next := startStream(t, effects)

	if d.Busy() != driver.BusyStreaming {
		t.Fatalf("投递后应直接进入下一轮流，实际 %v", d.Busy())
	}
	texts := userTexts(conv)
	if len(texts) != 2 || texts[1] != "再问一句" {
		t.Fatalf("插话应成为新的用户消息：%#v", texts)
	}
	if d.HasQueued() {
		t.Fatalf("投递后队列应为空")
	}
	// 新一轮的请求里要带上插话
	if !strings.Contains(lastUserText(next.Request.Messages), "再问一句") {
		t.Fatalf("下一轮请求里应包含插话：%#v", next.Request.Messages)
	}
}

// 空队列时不投递：回合结束仍是 Idle。
func TestNoDeliveryWithoutQueue(t *testing.T) {
	d, _ := newDriver(t, false)
	effects, _ := d.Send("hi")
	start := startStream(t, effects)
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	if _, ok := effectOf[driver.StartStream](effects); ok {
		t.Fatal("没有插话时不应自动开下一轮")
	}
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("应为 idle，实际 %v", d.Busy())
	}
}

// 停止时清空队列（用户已定：停下就不再自动动作）。
func TestStopClearsQueue(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("hi")
	startStream(t, effects)
	if _, err := d.Enqueue("别做了"); err != nil {
		t.Fatal(err)
	}

	effects = d.Stop()
	if _, ok := effectOf[driver.StartStream](effects); ok {
		t.Fatal("停止后不应投递队列")
	}
	if d.HasQueued() {
		t.Fatalf("停止应清空队列：%#v", d.Queued())
	}
	for _, msg := range conv.History() {
		if msg.Role == message.RoleUser && msg.PreviewText() == "别做了" {
			t.Fatal("被清空的插话不应进历史")
		}
	}
}

// 删除会话时清空队列。
func TestDeleteCurrentTurnClearsQueue(t *testing.T) {
	d, _ := newDriver(t, false)
	effects, _ := d.Send("hi")
	startStream(t, effects)
	if _, err := d.Enqueue("插话"); err != nil {
		t.Fatal(err)
	}
	d.DeleteCurrentTurn()
	if d.HasQueued() {
		t.Fatalf("删除会话应清空队列：%#v", d.Queued())
	}
}

// 撤回单条插话。
func TestDropQueued(t *testing.T) {
	d, _ := newDriver(t, false)
	effects, _ := d.Send("hi")
	startStream(t, effects)
	for _, text := range []string{"一", "二", "三"} {
		if _, err := d.Enqueue(text); err != nil {
			t.Fatal(err)
		}
	}
	d.DropQueued(1)
	got := d.Queued()
	if len(got) != 2 || got[0] != "一" || got[1] != "三" {
		t.Fatalf("撤回结果不一致：%#v", got)
	}
	if effects := d.DropQueued(9); effects != nil {
		t.Fatal("越界撤回应无操作")
	}
}

// 不忙时不能插话（空闲请走 Send）；等待回答、压缩中也拒绝。
func TestEnqueueRejectedWhenNotSteerable(t *testing.T) {
	d, _ := newDriver(t, false)
	if d.CanSteer() {
		t.Fatal("空闲时不应可插话")
	}
	if _, err := d.Enqueue("x"); err == nil {
		t.Fatal("空闲时插话应被拒绝")
	}

	// 等待回答：输入框里是自定义答案，不能当插话
	effects, _ := d.Send("问")
	start := startStream(t, effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask1", Name: "ask_user_question"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0,
		Delta: `{"questions":[{"question":"选哪个","options":[{"label":"A"}]}]}`})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	if _, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{
		{ID: "ask1", Name: "ask_user_question", Pending: []tools.QuestionSpec{{Question: "选哪个"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if d.Busy() != driver.BusyAwaitingAnswer {
		t.Fatalf("应进入等待回答，实际 %v", d.Busy())
	}
	if d.CanSteer() {
		t.Fatal("等待回答时不应可插话")
	}
	if _, err := d.Enqueue("x"); err == nil {
		t.Fatal("等待回答时插话应被拒绝")
	}
}

// 投递后 depth 归零：插话是新的用户输入，不该被上一轮的轮数上限误杀。
func TestSteerResetsToolRoundDepth(t *testing.T) {
	conv := conversation.New("ctx-depth")
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:         tools.Builtin(false),
		MaxToolRounds: 2,
	})

	effects, _ := d.Send("开始")
	start := startStream(t, effects)

	// 第一轮工具
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	run := runTools(t, effects)

	// 工具执行中插话
	if _, err := d.Enqueue("换个方向"); err != nil {
		t.Fatal(err)
	}

	effects, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{
		{ID: run.Calls[0].ID, Name: "read", Output: "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	next := startStream(t, effects)

	// 插话之后还能再跑一轮工具（depth 已归零，不会被上限打断）
	d.ApplyEvent(next.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c2", Name: "read"})
	d.ApplyEvent(next.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"b.txt"}`})
	effects, _ = d.ApplyEvent(next.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	if _, ok := effectOf[driver.RunTools](effects); !ok {
		t.Fatalf("插话之后应还能执行工具（depth 应已归零）：%#v", effects)
	}
}

// 插话优先于轮数上限：用满上限那一轮结束时的插话不能被丢掉。
func TestSteerBeatsToolRoundLimit(t *testing.T) {
	conv := conversation.New("ctx-limit-steer")
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:         tools.Builtin(false),
		MaxToolRounds: 1,
	})

	effects, _ := d.Send("开始")
	start := startStream(t, effects)
	if _, err := d.Enqueue("还有一件事"); err != nil {
		t.Fatal(err)
	}
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	run := runTools(t, effects)

	effects, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{
		{ID: run.Calls[0].ID, Name: "read", Output: "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := effectOf[driver.StartStream](effects); !ok {
		t.Fatalf("插话应优先于轮数上限：%#v", effects)
	}
	if d.HasQueued() {
		t.Fatal("队列应已投递")
	}
}

// 回合**失败**（不是停止）：队列保留、回到 Idle、**不投递也不开流**。
func TestFailureKeepsQueueWithoutDelivery(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("第一句")
	start := startStream(t, effects)
	if _, err := d.Enqueue("插话A"); err != nil {
		t.Fatal(err)
	}

	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventError, Err: fmt.Errorf("boom"),
	})
	if _, ok := effectOf[driver.StartStream](effects); ok {
		t.Fatal("失败后不应自动开新流")
	}
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("失败后应回到 Idle，实际 %v", d.Busy())
	}
	if got := d.Queued(); len(got) != 1 || got[0] != "插话A" {
		t.Fatalf("失败后队列应保留：%#v", got)
	}
	// 队列里的内容不能偷偷进历史
	for _, msg := range conv.History() {
		if msg.Role == message.RoleUser && msg.PreviewText() == "插话A" {
			t.Fatal("失败后插话不应进历史")
		}
	}
}

// 失败后下一次 Send：先发队列，再发新消息，只开一轮流。
func TestSendAfterFailureDeliversQueueFirst(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("第一句")
	start := startStream(t, effects)
	for _, text := range []string{"插话A", "插话B"} {
		if _, err := d.Enqueue(text); err != nil {
			t.Fatal(err)
		}
	}
	d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventError, Err: fmt.Errorf("boom"),
	})

	effects, err := d.Send("第二句")
	if err != nil {
		t.Fatal(err)
	}
	next := startStream(t, effects)
	if streams := countStreams(effects); streams != 1 {
		t.Fatalf("应只开一轮流，实际 %d", streams)
	}
	texts := allUserTexts(conv)
	want := []string{"第一句", "插话A", "插话B", "第二句"}
	if len(texts) != len(want) {
		t.Fatalf("user 消息顺序不一致：%#v", texts)
	}
	for i, wantText := range want {
		if texts[i] != wantText {
			t.Fatalf("第 %d 条不一致：%q，全部 %#v", i, texts[i], texts)
		}
	}
	if d.HasQueued() {
		t.Fatalf("发送后队列应为空：%#v", d.Queued())
	}
	if !strings.Contains(lastUserText(next.Request.Messages), "第二句") {
		t.Fatalf("新一轮请求的最后一条应是这次输入：%#v", next.Request.Messages)
	}
}

// 队列非空时空输入也能发送（把队列发出去）。
func TestSendWithOnlyQueued(t *testing.T) {
	d, conv := newDriver(t, false)
	effects, _ := d.Send("第一句")
	start := startStream(t, effects)
	if _, err := d.Enqueue("插话A"); err != nil {
		t.Fatal(err)
	}
	d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventError, Err: fmt.Errorf("boom"),
	})

	effects, err := d.Send("")
	if err != nil {
		t.Fatalf("队列非空时应当允许空输入发送：%v", err)
	}
	startStream(t, effects)
	if texts := allUserTexts(conv); len(texts) != 2 || texts[1] != "插话A" {
		t.Fatalf("应把队列发出去：%#v", texts)
	}
}

// 投递时也重置 autoCompactUsed（与 Send 的口径一致）。
func TestSteerResetsAutoCompactBudget(t *testing.T) {
	conv := conversation.New("ctx-steer-compact-budget")
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:         tools.Builtin(false),
		ContextWindow: 300,
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	// 撑到阈值以上并完成一次自动压缩
	started := false
	for round := 0; round < 4 && !started; round++ {
		effects := autoRound(t, d, rec, strings.Repeat("用户消息填充上下文。", 5))
		if _, ok := effectOfType[driver.StartSummary](effects); ok {
			started = true
		}
	}
	if !started {
		t.Fatal("前置条件失败：没有触发自动压缩")
	}
	if _, err := d.ApplyCompact("摘要"); err != nil {
		t.Fatal(err)
	}

	// 起一轮并插话；结束后触发压缩 → 压缩完成时投递插话，
	// 投递之后应当又允许自动压缩了（autoCompactUsed 被重置）
	effects, _ := d.Send("再来一轮")
	effects = rec.apply(effects)
	start := startStream(t, effects)
	if _, err := d.Enqueue("插话"); err != nil {
		t.Fatal(err)
	}
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: strings.Repeat("回复内容。", 40)})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec.apply(effects)
	if _, ok := effectOfType[driver.StartSummary](effects); !ok {
		t.Fatalf("这一轮结束应自动压缩：%#v", effects)
	}
	effects, err := d.ApplyCompact("第二份摘要")
	if err != nil {
		t.Fatal(err)
	}
	rec.apply(effects)
	next := startStream(t, effects)

	// 再让这一轮结束：若 autoCompactUsed 没重置，就不会再压缩
	d.ApplyEvent(next.MessageID, providers.Event{Kind: providers.EventText, Text: strings.Repeat("回复内容。", 40)})
	effects, _ = d.ApplyEvent(next.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	if _, ok := effectOfType[driver.StartSummary](effects); !ok {
		t.Fatalf("投递插话后应恢复自动压缩额度：%#v", effects)
	}
}

// 自动压缩**失败**：队列保留，下次 Send 时一起投递。
func TestCompactFailureKeepsQueue(t *testing.T) {
	conv := conversation.New("ctx-compact-fail-steer")
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:         tools.Builtin(false),
		ContextWindow: 300,
	})
	rec := &recorder{}
	rec.apply(d.Created(""))
	fillContext(t, d, rec)

	// 起一轮 → 生成中插话 → 这一轮结束触发自动压缩（队列被推迟到压缩完成）
	effects, _ := d.Send("再来一轮")
	effects = rec.apply(effects)
	start := startStream(t, effects)
	if _, err := d.Enqueue("压缩期间插的话"); err != nil {
		t.Fatalf("插话应被接受：%v", err)
	}
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: strings.Repeat("回复内容。", 40)})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec.apply(effects)
	if _, ok := effectOfType[driver.StartSummary](effects); !ok {
		t.Fatalf("这一轮结束应自动压缩：%#v", effects)
	}
	if got := d.Queued(); len(got) != 1 {
		t.Fatalf("压缩进行中队列应还在：%#v", got)
	}
	_ = conv

	// 摘要请求失败：回 Idle，队列**保留**（失败不是停止）
	d.FailCompact("摘要请求失败")
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("压缩失败后应回到 Idle，实际 %v", d.Busy())
	}
	if got := d.Queued(); len(got) != 1 || got[0] != "压缩期间插的话" {
		t.Fatalf("压缩失败后队列应保留：%#v", got)
	}
	for _, text := range allUserTexts(d.Conversation()) {
		if text == "压缩期间插的话" {
			t.Fatal("压缩失败后插话不应自己进历史")
		}
	}

	// 下次发送：先发队列，再发这次输入
	effects, err := d.Send("接着来")
	if err != nil {
		t.Fatal(err)
	}
	startStream(t, effects)
	texts := allUserTexts(d.Conversation())
	last := texts[len(texts)-2:]
	if len(last) != 2 || last[0] != "压缩期间插的话" || last[1] != "接着来" {
		t.Fatalf("下次发送应先把队列发出去：%#v", texts)
	}
}

// 压缩中点停止 = 停止：队列清空（与 FailCompact 区分）。
func TestCancelCompactClearsQueue(t *testing.T) {
	conv := conversation.New("ctx-cancel-compact")
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:         tools.Builtin(false),
		ContextWindow: 300,
	})
	rec := &recorder{}
	rec.apply(d.Created(""))
	fillContext(t, d, rec)

	effects, _ := d.Send("再来一轮")
	effects = rec.apply(effects)
	start := startStream(t, effects)
	if _, err := d.Enqueue("压缩时插的话"); err != nil {
		t.Fatal(err)
	}
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: strings.Repeat("回复内容。", 40)})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec.apply(effects)
	if _, ok := effectOfType[driver.StartSummary](effects); !ok {
		t.Fatalf("这一轮结束应自动压缩：%#v", effects)
	}

	effects = d.CancelCompact("已取消")
	if _, ok := effectOf[driver.StartStream](effects); ok {
		t.Fatal("取消压缩后不应投递队列")
	}
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("取消压缩后应回到 Idle，实际 %v", d.Busy())
	}
	if d.HasQueued() {
		t.Fatalf("压缩中停止应清空队列：%#v", d.Queued())
	}
}

// fillContext 先把上下文撑到自动压缩阈值之上，并完成一次压缩。
func fillContext(t *testing.T, d *driver.Driver, rec *recorder) {
	t.Helper()
	for round := 0; round < 4; round++ {
		effects := autoRound(t, d, rec, strings.Repeat("用户消息填充上下文。", 5))
		if _, ok := effectOfType[driver.StartSummary](effects); !ok {
			continue
		}
		if _, err := d.ApplyCompact("前置摘要"); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("前置条件失败：没有触发自动压缩")
}

// countStreams 数 effects 里有几个 StartStream。
func countStreams(effects []driver.Effect) int {
	count := 0
	for _, effect := range effects {
		if _, ok := effect.(driver.StartStream); ok {
			count++
		}
	}
	return count
}
