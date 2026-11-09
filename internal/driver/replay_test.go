package driver_test

import (
	"strings"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// recorder 收集 driver 产生的 Record effect（可同时写进内存后端）。
type recorder struct {
	events  []store.Event
	applyTo *store.MemoryStore
	// seq 模拟 store 的序号分配（真实后端由 store 分配）
	seq uint64
}

// apply 执行 effects 里的记录，并把其余 effect 交给调用方处理。
func (r *recorder) apply(effects []driver.Effect) []driver.Effect {
	var rest []driver.Effect
	for _, effect := range effects {
		if rec, ok := effect.(driver.Record); ok {
			event := rec.Event
			event.Version = store.Version
			event.Seq = r.seq
			r.seq++
			r.events = append(r.events, event)
			if r.applyTo != nil {
				_ = r.applyTo.Append(event)
			}
			continue
		}
		rest = append(rest, effect)
	}
	return rest
}

// effectOfType 取第一个指定类型。
func effectOfType[T any](effects []driver.Effect) (T, bool) {
	var zero T
	for _, e := range effects {
		if v, ok := e.(T); ok {
			return v, true
		}
	}
	return zero, false
}

// 完整会话：发送 → 流式 → 工具 → 作答 → 续流 → 重命名 → 切模型。
// 记录所有事件后重放，逐字段比对在线状态。
func TestReplayMatchesLiveState(t *testing.T) {
	conv := conversation.New("ctx-live")
	d := driver.New(conv, driver.Options{
		Selection:    providers.Selection{ProviderID: "deepseek", Model: "deepseek-chat"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created("我的会话"))

	// 1. 发送
	effects, err := d.Send("读一下 a.txt")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)
	if start.MessageID == "" {
		t.Fatal("应开始流")
	}
	userMessageID := conv.Entries()[0].Message.Message.MessageID

	// 2. 流式正文 + 工具调用
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "我来读"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "ask_user_question"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0,
		Delta: `{"questions":[{"id":"q1","question":"用哪个?","options":[{"label":"甲"}]}]}`})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	rec.apply(effects)

	// 3. 工具结果：提问挂起
	askEffects, err := d.ToolResults(start.MessageID, []tools.ToolOutcome{{
		ID: "c1", Name: "ask_user_question",
		Pending: []tools.QuestionSpec{{ID: "q1", Question: "用哪个?", Options: []tools.QuestionOption{{Label: "甲"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rec.apply(askEffects)
	if d.Busy() != driver.BusyAwaitingAnswer {
		t.Fatalf("应等待回答，实际 %v", d.Busy())
	}

	// 4. 作答 → 续流
	groups := d.AskGroups()
	groups[0].Toggle(0, 0)
	effects, err = d.SubmitAsks("")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	second, _ := effectOfType[driver.StartStream](effects)
	if second.MessageID == "" {
		t.Fatal("作答后应继续开流")
	}
	d.ApplyEvent(second.MessageID, providers.Event{Kind: providers.EventText, Text: "好的"})
	effects, _ = d.ApplyEvent(second.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec.apply(effects)

	// 5. 重命名 + 切模型
	rec.apply(d.Rename("我的会话"))
	if err := d.SetSelection(providers.Selection{ProviderID: "openai", Model: "gpt-4o-mini"}); err != nil {
		t.Fatal(err)
	}
	rec.apply(d.TakeRecords())

	if d.Busy() != driver.BusyIdle {
		t.Fatalf("结束时应为 idle，实际 %v", d.Busy())
	}

	// 6. 重放
	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Warnings) > 0 {
		t.Fatalf("重放不应有警告：%v", replayed.Warnings)
	}

	// 逐字段比对
	if replayed.ContextID != "ctx-live" {
		t.Fatalf("context id 不一致：%q", replayed.ContextID)
	}
	if replayed.Title != "我的会话" {
		t.Fatalf("标题不一致：%q", replayed.Title)
	}
	if replayed.Selection.ProviderID != "openai" || replayed.Selection.Model != "gpt-4o-mini" {
		t.Fatalf("模型选择不一致：%+v", replayed.Selection)
	}
	if len(replayed.PendingAsks) != 0 {
		t.Fatalf("已作答的提问不应恢复：%d", len(replayed.PendingAsks))
	}

	compareConversations(t, conv, replayed.Conversation)

	// 重放出来的状态可以继续发消息
	restored := driver.Restore(replayed, driver.Options{
		Selection: providers.Selection{ProviderID: "openai", Model: "gpt-4o-mini"},
		Tools:     tools.Builtin(false),
	})
	if restored.Busy() != driver.BusyIdle {
		t.Fatalf("恢复后应空闲，实际 %v", restored.Busy())
	}
	if restored.Title() != "我的会话" {
		t.Fatalf("恢复的标题不一致：%q", restored.Title())
	}
	if _, err := restored.Send("继续"); err != nil {
		t.Fatalf("恢复后应能继续发送：%v", err)
	}
	if got := restored.Selection(); got.ProviderID != "openai" {
		t.Fatalf("恢复的选择应来自 selection.changed：%+v", got)
	}
	_ = userMessageID
}

// compareConversations 逐条比对两条会话（ID、顺序、状态、parts）。
func compareConversations(t *testing.T, live, replayed *conversation.Conversation) {
	t.Helper()
	left := live.Entries()
	right := replayed.Entries()
	if len(left) != len(right) {
		t.Fatalf("条目数不一致：%d vs %d", len(left), len(right))
	}
	for i := range left {
		if (left[i].Notice == nil) == (right[i].Notice == nil) {
			// 通知的 id 是本地生成的、不属于协议内容，不参与比对
			if left[i].Notice == nil && left[i].ID() != right[i].ID() {
				t.Fatalf("第 %d 条 ID 不一致：%s vs %s", i, left[i].ID(), right[i].ID())
			}
		}
		if (left[i].Notice == nil) != (right[i].Notice == nil) {
			t.Fatalf("第 %d 条类型不一致", i)
		}
		if left[i].Notice != nil {
			if left[i].Notice.Text != right[i].Notice.Text {
				t.Fatalf("第 %d 条通知内容不一致：%q vs %q", i, left[i].Notice.Text, right[i].Notice.Text)
			}
			continue
		}
		am, bm := left[i].Message, right[i].Message
		if am.Status() != bm.Status() {
			t.Fatalf("第 %d 条状态不一致：%v vs %v", i, am.Status(), bm.Status())
		}
		if am.FailureReason() != bm.FailureReason() {
			t.Fatalf("第 %d 条失败原因不一致", i)
		}
		if len(am.Message.Parts) != len(bm.Message.Parts) {
			t.Fatalf("第 %d 条 part 数不一致：%d vs %d", i, len(am.Message.Parts), len(bm.Message.Parts))
		}
		for j := range am.Message.Parts {
			a, b := am.Message.Parts[j], bm.Message.Parts[j]
			if a.MediaType != b.MediaType {
				t.Fatalf("第 %d 条第 %d 个 part 类型不一致：%q vs %q", i, j, a.MediaType, b.MediaType)
			}
			if at, ok := a.AsText(); ok {
				bt, _ := b.AsText()
				if at != bt {
					t.Fatalf("第 %d 条第 %d 个 part 文本不一致：%q vs %q", i, j, at, bt)
				}
			}
		}
	}
}

// 流式中间态（只有 streaming，没有结束事件）→ 重放为 Cancelled。
func TestReplayStreamingBecomesCancelled(t *testing.T) {
	events := []store.Event{
		store.MustEvent(store.TypeSessionCreated, driver.SessionCreatedData{ContextID: "ctx-1"}),
		store.MustEvent(store.TypeMessageAppended, driver.MessageAppendedData{
			Message: message.Message{MessageID: "m1", Role: message.RoleUser,
				Parts: []message.Part{message.NewTextPart("hi")}},
			Status: conversation.StatusComplete,
		}),
		store.MustEvent(store.TypeMessageAppended, driver.MessageAppendedData{
			Message: message.Message{MessageID: "m2", Role: message.RoleAgent,
				Parts: []message.Part{message.NewTextPart("半句")}},
			Status: conversation.StatusStreaming,
		}),
	}
	// 补 seq（store 会分配，这里手工构造）
	for i := range events {
		events[i].Seq = uint64(i)
		events[i].Version = store.Version
	}

	replayed, err := driver.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := replayed.Conversation.Message("m2")
	if !ok {
		t.Fatal("应保留消息")
	}
	if entry.Status() != conversation.StatusCancelled {
		t.Fatalf("流式中间态应重放为 cancelled，实际 %v", entry.Status())
	}
	if entry.PreviewText() != "半句" {
		t.Fatalf("已写入的内容应保留：%q", entry.PreviewText())
	}
	if len(replayed.Warnings) == 0 {
		t.Fatal("应记录一条「被中断」警告")
	}
	// Cancelled 不进历史
	for _, msg := range replayed.Conversation.History() {
		if msg.MessageID == "m2" {
			t.Fatal("被中断的消息不应进历史")
		}
	}
}

// 挂起的提问：重放后面板恢复、busy=AwaitingAnswer，作答后能续一轮。
func TestReplayRestoresPendingAsk(t *testing.T) {
	conv := conversation.New("ctx-ask")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	effects, _ := d.Send("问个问题")
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "ask_1", Name: "ask_user_question"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0,
		Delta: `{"questions":[{"id":"q1","question":"选哪个?","options":[{"label":"甲"}]}]}`})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	rec.apply(effects)
	effects, _ = d.ToolResults(start.MessageID, []tools.ToolOutcome{{
		ID: "ask_1", Name: "ask_user_question",
		Pending: []tools.QuestionSpec{{ID: "q1", Question: "选哪个?", Options: []tools.QuestionOption{{Label: "甲"}}}},
	}})
	rec.apply(effects)

	// 进程在这里被杀：重放
	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.PendingAsks) != 1 {
		t.Fatalf("应恢复 1 组提问，实际 %d", len(replayed.PendingAsks))
	}
	restored := driver.Restore(replayed, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	if restored.Busy() != driver.BusyAwaitingAnswer {
		t.Fatalf("恢复后应等待回答，实际 %v", restored.Busy())
	}
	asks := restored.AskGroups()
	if len(asks) != 1 || asks[0].CallID != "ask_1" {
		t.Fatalf("提问组不一致：%#v", asks)
	}
	if len(asks[0].Questions) != 1 || asks[0].Questions[0].Question != "选哪个?" {
		t.Fatalf("问题内容丢失：%#v", asks[0].Questions)
	}

	// 作答后能继续
	rec2 := &recorder{}
	asks[0].Toggle(0, 0)
	effects, err = restored.SubmitAsks("")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec2.apply(effects)
	if _, ok := effectOfType[driver.StartStream](effects); !ok {
		t.Fatal("作答后应继续开流")
	}
	if restored.Busy() != driver.BusyStreaming {
		t.Fatalf("应进入 streaming，实际 %v", restored.Busy())
	}
}

// 停止后重放：未配对的调用有合成结果，历史可用。
func TestReplayAfterStopKeepsPairing(t *testing.T) {
	conv := conversation.New("ctx-stop")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created(""))
	effects, _ := d.Send("跑工具")
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`})
	rec.apply(d.Stop())

	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := replayed.Conversation.Message(start.MessageID)
	if !ok {
		t.Fatal("消息应存在")
	}
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
		t.Fatalf("重放后仍要配对完整：uses=%d results=%d", uses, results)
	}
	if entry.Status() != conversation.StatusCancelled {
		t.Fatalf("应为 cancelled，实际 %v", entry.Status())
	}
	// 历史里能正常映射（说明配对完整）
	if _, err := providers.ToOpenAIMessages(replayed.Conversation.History(), nil); err != nil {
		t.Fatalf("历史应可用：%v", err)
	}
}

// 内存后端 + 重放：同一事件序列得到同一状态。
func TestMemoryStoreReplayEqualsLive(t *testing.T) {
	conv := conversation.New("ctx-mem")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	memory := store.NewMemory()
	rec := &recorder{applyTo: memory}
	rec.apply(d.Created(""))

	effects, _ := d.Send("你好")
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "回答"})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec.apply(effects)

	loaded, err := memory.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, replayed, err := driver.Load(loaded.Events, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Title != "" {
		t.Logf("标题：%q", replayed.Title)
	}
	compareConversations(t, conv, replayed.Conversation)
}

// 日志损坏（序号跳变）时重放给出警告并截断，而不是报错。
func TestReplayTruncatesOnSeqGap(t *testing.T) {
	events := []store.Event{
		store.MustEvent(store.TypeSessionCreated, driver.SessionCreatedData{ContextID: "c"}),
		store.MustEvent(store.TypeSessionRenamed, driver.SessionRenamedData{Title: "第一个"}),
		store.MustEvent(store.TypeSessionRenamed, driver.SessionRenamedData{Title: "第二个"}),
	}
	events[0].Seq = 0
	events[1].Seq = 1
	events[2].Seq = 9 // 跳变

	replayed, err := driver.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Title != "第一个" {
		t.Fatalf("应在跳变处截断（保留第一个标题），实际 %q", replayed.Title)
	}
	if len(replayed.Warnings) == 0 || !strings.Contains(replayed.Warnings[0], "序号") {
		t.Fatalf("应提示序号问题：%v", replayed.Warnings)
	}
}

// 回归（opus P0-3）：生成回复时崩溃 → 重放显示「已中断」占位。
//
// 这里用 driver **真实产生**的事件：启动流之后立刻取走到这一刻为止的 Record，
// 不写 finalized —— 等价于进程在生成过程中被杀。
func TestReplayCrashDuringStreamingShowsInterrupted(t *testing.T) {
	conv := conversation.New("ctx-crash")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	effects, err := d.Send("写点什么")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects) // 这里之后的事件都不再记录（模拟崩溃）

	start, _ := effectOfType[driver.StartStream](effects)
	if _, ok := conv.Message(start.MessageID); !ok {
		t.Fatal("消息应已建立")
	}
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "写到一半"})
	// 进程被杀：不记录 finalized

	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := replayed.Conversation.Message(start.MessageID)
	if !ok {
		t.Fatal("应有「已中断」的占位消息")
	}
	if entry.Status() != conversation.StatusCancelled {
		t.Fatalf("应标为 cancelled，实际 %v", entry.Status())
	}
	if len(replayed.Repairs) == 0 {
		t.Fatal("应产生一条 finalized(cancelled) 的修复事件")
	}

	// 修复事件写回日志后，再次重放不再产生新的修复（幂等）
	replayed2 := appendAndReplay(t, rec.events, replayed.Repairs)
	if len(replayed2.Repairs) != 0 {
		t.Fatalf("修复应幂等，第二次仍有 %d 条", len(replayed2.Repairs))
	}
	entry2, _ := replayed2.Conversation.Message(start.MessageID)
	if entry2.Status() != conversation.StatusCancelled {
		t.Fatalf("第二次重放状态应稳定，实际 %v", entry2.Status())
	}
}

// 回归（opus P0-2）：执行工具时崩溃 → 重放补「已中断」结果，历史可用且幂等。
func TestReplayCrashDuringToolsRepairsPairing(t *testing.T) {
	conv := conversation.New("ctx-tools")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	effects, _ := d.Send("跑工具")
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a.txt"}`})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	rec.apply(effects) // 只记录到「工具要开始跑」，没有 tool.results（模拟崩溃）

	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := replayed.Conversation.Message(start.MessageID)
	if !ok {
		t.Fatal("消息应存在")
	}
	var uses, results int
	for _, part := range entry.Message.Parts {
		switch part.MediaType {
		case render.MediaToolUse:
			uses++
		case render.MediaToolResult:
			results++
			data, _ := partDataOf(part)
			if data["is_error"] != true {
				t.Fatalf("补的结果应标记为错误：%#v", data)
			}
			if !strings.Contains(data["output"].(string), "已中断") {
				t.Fatalf("结果文本应说明已中断：%v", data["output"])
			}
		}
	}
	if uses != 1 || results != 1 {
		t.Fatalf("每个 tool_use 都应有结果：uses=%d results=%d", uses, results)
	}
	if len(replayed.Repairs) == 0 {
		t.Fatal("应产生 tool.results 修复事件")
	}

	// 历史可正常映射（配对完整）
	if _, err := providers.ToOpenAIMessages(replayed.Conversation.History(), nil); err != nil {
		t.Fatalf("OpenAI 映射失败：%v", err)
	}
	if _, err := providers.ToAnthropicMessages(replayed.Conversation.History(), nil); err != nil {
		t.Fatalf("Anthropic 映射失败：%v", err)
	}
	// 能继续发送
	restored := driver.Restore(replayed, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	if _, err := restored.Send("继续"); err != nil {
		t.Fatalf("修复后应能继续发送：%v", err)
	}

	// 幂等：写回 repairs 后再重放，不再产生新的修复，结果不变
	replayed2 := appendAndReplay(t, rec.events, replayed.Repairs)
	if len(replayed2.Repairs) != 0 {
		t.Fatalf("修复应幂等，第二次仍有 %d 条", len(replayed2.Repairs))
	}
	entry2, _ := replayed2.Conversation.Message(start.MessageID)
	uses2, results2 := 0, 0
	for _, part := range entry2.Message.Parts {
		switch part.MediaType {
		case render.MediaToolUse:
			uses2++
		case render.MediaToolResult:
			results2++
		}
	}
	if uses2 != 1 || results2 != 1 {
		t.Fatalf("第二次重放结果应稳定：uses=%d results=%d", uses2, results2)
	}
}

// appendAndReplay 把修复事件追加到原事件后面（模拟「加载时写回日志」）再重放。
func appendAndReplay(t *testing.T, events []store.Event, repairs []store.Event) driver.Replayed {
	t.Helper()
	merged := make([]store.Event, 0, len(events)+len(repairs))
	merged = append(merged, events...)
	seq := events[len(events)-1].Seq + 1
	for _, repair := range repairs {
		event := repair
		event.Version = store.Version
		event.Seq = seq
		seq++
		merged = append(merged, event)
	}
	replayed, err := driver.Replay(merged)
	if err != nil {
		t.Fatal(err)
	}
	return replayed
}
