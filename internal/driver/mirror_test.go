package driver_test

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strconv"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
)

// 事件序列：推理 + 文字 + 两次工具参数增量 + 工具结束 + Done。
func deltaEvents() []providers.Event {
	return []providers.Event{
		{Kind: providers.EventReasoning, Text: "先想一想"},
		{Kind: providers.EventText, Text: "我看看"},
		{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read"},
		{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"a`},
		{Kind: providers.EventToolArgs, Index: 0, Delta: `b.txt"}`},
		{Kind: providers.EventToolEnd, Index: 0},
	}
}

// execEmptyShell 构造日志里那条 streaming 空壳消息（与执行端 startTurn 写的一致）。
func execEmptyShell(messageID string) message.Message {
	return message.Message{MessageID: messageID, Role: message.RoleAgent}
}

// P1：拆分没有改变执行端行为 —— ApplyEvent 的结果 == ApplyStreamDelta + FinishStream。
func TestApplyEventSplit(t *testing.T) {
	run := func(combined bool) (*conversation.Conversation, driver.Busy, int) {
		d, conv := newDriver(t, false)
		effects, _ := d.Send("看文件")
		start := startStream(t, effects)
		for _, ev := range deltaEvents() {
			if combined {
				d.ApplyEvent(start.MessageID, ev)
				continue
			}
			d.ApplyStreamDelta(start.MessageID, ev)
		}
		if combined {
			d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
		} else {
			d.FinishStream(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
		}
		return conv, d.Busy(), len(conv.Entries())
	}

	combinedConv, combinedBusy, combinedEntries := run(true)
	splitConv, splitBusy, splitEntries := run(false)

	if combinedBusy != splitBusy {
		t.Fatalf("Busy 不一致：%v vs %v", combinedBusy, splitBusy)
	}
	if combinedEntries != splitEntries {
		t.Fatalf("条目数不一致：%d vs %d", combinedEntries, splitEntries)
	}
	if got, want := entryDigest(t, combinedConv), entryDigest(t, splitConv); got != want {
		t.Fatalf("拆分前后会话不一致：\ncombined=%s\nsplit=%s", got, want)
	}
}

// P1：mirror 只应用增量 —— 每个事件之后与执行端的增量结果深相等，
// 且 Done 之后不会自己加通知、不会多出消息、不会推进 Busy。
func TestStreamDeltaMatchesExecutor(t *testing.T) {
	// 执行端与镜像各自持有会话，**同一个事件逐步应用、逐步比较**
	execDriver, _ := newDriver(t, false)
	effects, _ := execDriver.Send("看文件")
	start := startStream(t, effects)

	replayed, err := driver.Replay([]store.Event{
		store.MustEvent(store.TypeMessageAppended, driver.MessageAppendedData{
			Message: message.Message{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{
				message.NewTextPart("看文件")}}, Status: conversation.StatusComplete,
		}),
		store.MustEvent(store.TypeMessageAppended, driver.MessageAppendedData{
			Message: execEmptyShell(start.MessageID), Status: conversation.StatusStreaming,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	mirror := driver.NewMirror(&replayed, driver.Options{})
	// snapshot 里带了正在流式输出的消息：此时还没输出任何内容（parts 为空）
	if err := mirror.ReplaceInflight(start.MessageID, nil, conversation.StatusStreaming); err != nil {
		t.Fatal(err)
	}

	for _, ev := range deltaEvents() {
		if _, err := execDriver.ApplyStreamDelta(start.MessageID, ev); err != nil {
			t.Fatalf("执行端应用 %s 失败：%v", ev.Kind, err)
		}
		if err := mirror.ApplyStreamDelta(start.MessageID, ev); err != nil {
			t.Fatalf("镜像应用 %s 失败：%v", ev.Kind, err)
		}
		if got, want := entryDigest(t, mirror.Conversation()), entryDigest(t, execDriver.Conversation()); got != want {
			t.Fatalf("%s 之后增量状态不一致：\nmirror=%s\nexec=%s", ev.Kind, got, want)
		}
	}

	// 镜像收到 Done/Error 也不做事（正常实现里根本不转发，这里防御性验证）
	before := len(mirror.Conversation().Entries())
	if err := mirror.ApplyStreamDelta(start.MessageID, providers.Event{Kind: providers.EventDone}); err != nil {
		t.Fatal(err)
	}
	if got := len(mirror.Conversation().Entries()); got != before {
		t.Fatalf("镜像不该因 Done 增加条目：%d → %d", before, got)
	}
	if len(replayed.Repairs) != 0 {
		t.Fatalf("镜像不该产生 repairs：%v", replayed.Repairs)
	}
}

// entryDigest 把会话条目序列化成稳定字符串（忽略指针地址，便于比较两端状态）。
func entryDigest(t *testing.T, conv *conversation.Conversation) string {
	t.Helper()
	type row struct {
		ID     string
		Status conversation.EntryStatus
		Parts  []message.Part
		Notice string
	}
	rows := make([]row, 0, len(conv.Entries()))
	for index, entry := range conv.Entries() {
		// ID 每次运行都随机（NewID），比较时用序号代替
		id := strconv.Itoa(index)
		if entry.Notice != nil {
			rows = append(rows, row{ID: id, Notice: entry.Notice.Text})
			continue
		}
		rows = append(rows, row{
			ID:     id,
			Status: entry.Message.Status(),
			Parts:  entry.Message.Message.Parts,
		})
	}
	raw, err := json.Marshal(rows, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	_ = jsontext.Value(nil)
	return string(raw)
}

// anthropicSequence 模拟 Anthropic 的 content block 下标：先一个 text 块（Index=0），
// 再一个 tool_use 块（Index=1）。OpenAI 的 index 恰好从 0 连续编号，会掩盖映射错误。
func anthropicSequence() []providers.Event {
	return []providers.Event{
		{Kind: providers.EventText, Text: "我先读文件"},
		{Kind: providers.EventToolStart, Index: 1, ID: "toolu_1", Name: "read"},
		{Kind: providers.EventToolArgs, Index: 1, Delta: `{"file_path":"docs/`},
		{Kind: providers.EventToolArgs, Index: 1, Delta: `plan.md"}`},
	}
}

// takeoverMirror 用执行端**当时**的 Conversation 做一次"中途接管"，
// 返回镜像（模拟 state.snapshot 携带 Inflight 内容）。
func takeoverMirror(t *testing.T, execConv *conversation.Conversation, messageID, userText string) *driver.Mirror {
	t.Helper()
	replayed, err := driver.Replay([]store.Event{
		store.MustEvent(store.TypeMessageAppended, driver.MessageAppendedData{
			Message: message.Message{MessageID: "u1", Role: message.RoleUser,
				Parts: []message.Part{message.NewTextPart(userText)}},
			Status: conversation.StatusComplete,
		}),
		store.MustEvent(store.TypeMessageAppended, driver.MessageAppendedData{
			Message: execEmptyShell(messageID), Status: conversation.StatusStreaming,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	mirror := driver.NewMirror(&replayed, driver.Options{})
	entry, ok := execConv.Message(messageID)
	if !ok {
		t.Fatal("执行端没有这条消息")
	}
	// snapshot 里的 Inflight：执行端此刻的完整 parts（深拷贝）
	parts := append([]message.Part(nil), entry.Message.Parts...)
	if err := mirror.ReplaceInflight(messageID, parts, conversation.StatusStreaming); err != nil {
		t.Fatal(err)
	}
	return mirror
}

// P1 回归（评审）：Anthropic 的 Index 是 content block 下标，中途接管的镜像
// 必须把增量对回**已有的**工具 part，不能新建第二张卡片；缓冲区也要从 part 恢复。
func TestMirrorTakeoverMidToolArgs(t *testing.T) {
	events := anthropicSequence()
	execDriver, _ := newDriver(t, false)
	effects, _ := execDriver.Send("看文件")
	start := startStream(t, effects)

	// 应用到「参数流到一半」（前 3 个事件）
	for _, ev := range events[:3] {
		if _, err := execDriver.ApplyStreamDelta(start.MessageID, ev); err != nil {
			t.Fatal(err)
		}
	}
	execConv := execDriver.Conversation()
	mirror := takeoverMirror(t, execConv, start.MessageID, "看文件")

	if got, want := entryDigest(t, mirror.Conversation()), entryDigest(t, execConv); got != want {
		t.Fatalf("接管时状态就应一致：\nmirror=%s\nexec=%s", got, want)
	}

	// 继续应用剩下的增量：不能多出工具卡片，name/id/raw_args 都要对
	for _, ev := range events[3:] {
		if _, err := execDriver.ApplyStreamDelta(start.MessageID, ev); err != nil {
			t.Fatal(err)
		}
		if err := mirror.ApplyStreamDelta(start.MessageID, ev); err != nil {
			t.Fatal(err)
		}
		if got, want := entryDigest(t, mirror.Conversation()), entryDigest(t, execDriver.Conversation()); got != want {
			t.Fatalf("%s 之后不一致：\nmirror=%s\nexec=%s", ev.Kind, got, want)
		}
	}

	// 断言细节：只有一张工具卡片，id/name/参数前缀正确
	entry, _ := mirror.Conversation().Message(start.MessageID)
	var tools int
	var rawArgs, id, name string
	for _, part := range entry.Message.Parts {
		if part.MediaType != render.MediaToolUse {
			continue
		}
		tools++
		data, _ := part.AsData()
		fields := data.(map[string]any)
		id, _ = fields["id"].(string)
		name, _ = fields["name"].(string)
		rawArgs, _ = fields["raw_args"].(string)
	}
	if tools != 1 {
		t.Fatalf("工具卡片应只有 1 张，实际 %d", tools)
	}
	if id != "toolu_1" || name != "read" {
		t.Fatalf("id/name 丢了：id=%q name=%q", id, name)
	}
	if rawArgs != `{"file_path":"docs/plan.md"}` {
		t.Fatalf("参数前缀/拼接不对：%q", rawArgs)
	}
}

// 推理流到一半时接管：后续推理要继续写进**同一个** reasoning part。
func TestMirrorTakeoverMidReasoning(t *testing.T) {
	execDriver, _ := newDriver(t, false)
	effects, _ := execDriver.Send("想想")
	start := startStream(t, effects)

	if _, err := execDriver.ApplyStreamDelta(start.MessageID, providers.Event{
		Kind: providers.EventReasoning, Text: "第一步：",
	}); err != nil {
		t.Fatal(err)
	}
	execConv := execDriver.Conversation()
	mirror := takeoverMirror(t, execConv, start.MessageID, "想想")

	rest := []providers.Event{
		{Kind: providers.EventReasoning, Text: "先看目录"},
		{Kind: providers.EventText, Text: "好"},
	}
	for _, ev := range rest {
		if _, err := execDriver.ApplyStreamDelta(start.MessageID, ev); err != nil {
			t.Fatal(err)
		}
		if err := mirror.ApplyStreamDelta(start.MessageID, ev); err != nil {
			t.Fatal(err)
		}
		if got, want := entryDigest(t, mirror.Conversation()), entryDigest(t, execDriver.Conversation()); got != want {
			t.Fatalf("%s 之后不一致：\nmirror=%s\nexec=%s", ev.Kind, got, want)
		}
	}

	entry, _ := mirror.Conversation().Message(start.MessageID)
	var reasoning []string
	for _, part := range entry.Message.Parts {
		if part.MediaType == render.MediaReasoning {
			text, _ := part.AsText()
			reasoning = append(reasoning, text)
		}
	}
	if len(reasoning) != 1 {
		t.Fatalf("推理应只有一个 part，实际 %d：%v", len(reasoning), reasoning)
	}
	if reasoning[0] != "第一步：先看目录" {
		t.Fatalf("推理内容不对：%q", reasoning[0])
	}
}
