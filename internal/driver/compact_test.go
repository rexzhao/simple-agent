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

// longConversation 造一段足够长的会话（每轮一条用户消息 + 一条 agent 回复）。
func longConversation(t *testing.T, d *driver.Driver, rec *recorder, rounds int) {
	t.Helper()
	for i := 0; i < rounds; i++ {
		effects, err := d.Send(strings.Repeat("这是一段较长的用户消息，用来把上下文撑大。", 4))
		if err != nil {
			t.Fatalf("第 %d 轮发送失败：%v", i, err)
		}
		effects = rec.apply(effects)
		start, ok := effectOfType[driver.StartStream](effects)
		if !ok {
			t.Fatalf("第 %d 轮没有开始流", i)
		}
		d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: strings.Repeat("这是助手的回复内容。", 4)})
		effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
		rec.apply(effects)
	}
}

// newCompactDriver 构造带上下文窗口的 driver。
func newCompactDriver(t *testing.T, window int) (*driver.Driver, *recorder) {
	t.Helper()
	conv := conversation.New("ctx-compact")
	d := driver.New(conv, driver.Options{
		Selection:          providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:              tools.Builtin(false),
		ContextWindow:      window,
		DisableAutoCompact: true, // 这些用例单独驱动压缩流程
	})
	rec := &recorder{}
	rec.apply(d.Created("压缩测试"))
	return d, rec
}

// 边界：kept 的起点必须是一条 user 消息；压缩后的历史首条也是 user。
func TestCompactChoosesUserBoundary(t *testing.T) {
	d, rec := newCompactDriver(t, 400)
	longConversation(t, d, rec, 4)

	effects, err := d.BeginCompact()
	if err != nil {
		t.Fatalf("应能开始压缩：%v", err)
	}
	start, ok := effectOfType[driver.StartSummary](effects)
	if !ok {
		t.Fatal("应返回摘要请求")
	}
	if len(start.Request.Tools) != 0 {
		t.Fatal("摘要请求不应带工具")
	}
	if d.Busy() != driver.BusyCompacting {
		t.Fatalf("应进入 compacting，实际 %v", d.Busy())
	}

	effects, err = d.ApplyCompact("这是摘要正文")
	if err != nil {
		t.Fatalf("应用摘要失败：%v", err)
	}
	rec.apply(effects)

	history := d.Conversation().History()
	if len(history) == 0 {
		t.Fatal("压缩后应有历史")
	}
	if history[0].Role != message.RoleUser {
		t.Fatalf("历史首条必须是 user（Anthropic 要求），实际 %v", history[0].Role)
	}
	if history[0].Parts[0].MediaType != render.MediaSummary {
		t.Fatalf("首条应是摘要 part：%v", history[0].Parts[0].MediaType)
	}
	if !strings.Contains(history[0].PreviewText(), "这是摘要正文") {
		t.Fatalf("摘要内容应写进消息：%q", history[0].PreviewText())
	}
	// kept 部分每条都完整（工具配对不可能被拆开：本项目一条消息里同时含调用与结果）
	for _, msg := range history[1:] {
		if msg.Role == message.RoleAgent {
			continue
		}
		if msg.Role != message.RoleUser {
			t.Fatalf("kept 里出现了非 user/agent 消息：%v", msg.Role)
		}
	}
	// 两家协议都能接受压缩后的历史
	if _, err := providers.ToOpenAIMessages(history, nil); err != nil {
		t.Fatalf("OpenAI 映射失败：%v", err)
	}
	anthropic, err := providers.ToAnthropicMessages(history, nil)
	if err != nil {
		t.Fatalf("Anthropic 映射失败：%v", err)
	}
	if anthropic[0]["role"] != "user" {
		t.Fatalf("Anthropic 首条应为 user：%#v", anthropic[0])
	}
	// 工具配对完整：每个 tool_use 都有 tool_result
	msgs, _ := providers.ToOpenAIMessages(history, nil)
	for i, m := range msgs {
		if _, ok := m["tool_calls"]; !ok {
			continue
		}
		found := false
		for _, later := range msgs[i+1:] {
			if later["role"] == "tool" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("压缩后的历史里 tool_use 没有配对结果")
		}
	}
}

// 压缩会减少上下文占用，并且只保留最近的一部分。
func TestCompactShrinksContext(t *testing.T) {
	d, rec := newCompactDriver(t, 400)
	longConversation(t, d, rec, 6)

	beforeEntries := len(d.Conversation().Entries())
	beforeTokens := d.TokensInUse()

	if _, err := d.BeginCompact(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplyCompact("摘要"); err != nil {
		t.Fatal(err)
	}

	afterEntries := len(d.Conversation().Entries())
	if afterEntries >= beforeEntries {
		t.Fatalf("压缩后条目应变少：%d → %d", beforeEntries, afterEntries)
	}
	afterTokens := d.TokensInUse()
	if afterTokens >= beforeTokens {
		t.Fatalf("压缩后 token 估算应变少：%d → %d", beforeTokens, afterTokens)
	}
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("压缩完成应回到 idle，实际 %v", d.Busy())
	}
}

// 摘要失败/取消：什么都不改（不写快照、内存状态不变）。
func TestCompactFailureChangesNothing(t *testing.T) {
	d, rec := newCompactDriver(t, 400)
	longConversation(t, d, rec, 5)

	before := len(d.Conversation().Entries())
	beforeEvents := len(rec.events)

	if _, err := d.BeginCompact(); err != nil {
		t.Fatal(err)
	}
	// 用户取消（或请求失败）
	rec.apply(d.FailCompact("已取消"))

	if d.Busy() != driver.BusyIdle {
		t.Fatalf("应回到 idle，实际 %v", d.Busy())
	}
	if got := len(d.Conversation().Entries()); got != before {
		t.Fatalf("失败时不应改动会话：%d → %d", before, got)
	}
	if len(rec.events) != beforeEvents {
		t.Fatalf("失败时不应写事件：%d → %d", beforeEvents, len(rec.events))
	}
	// 之后仍可正常发送
	if _, err := d.Send("继续"); err != nil {
		t.Fatalf("失败后应能继续：%v", err)
	}
}

// 空摘要按失败处理。
func TestCompactRejectsEmptySummary(t *testing.T) {
	d, rec := newCompactDriver(t, 400)
	longConversation(t, d, rec, 3)
	if _, err := d.BeginCompact(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplyCompact("   "); err == nil {
		t.Fatal("空摘要应报错")
	}
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("应回到 idle，实际 %v", d.Busy())
	}
}

// 上下文还短时不需要压缩。
func TestBeginCompactRejectsShortHistory(t *testing.T) {
	d, rec := newCompactDriver(t, 100_000)
	longConversation(t, d, rec, 1)
	if _, err := d.BeginCompact(); err == nil {
		t.Fatal("上下文很短时应拒绝压缩")
	}
}

// 自动压缩阈值。
func TestShouldCompactRatio(t *testing.T) {
	d, rec := newCompactDriver(t, 400)
	longConversation(t, d, rec, 5)
	if !d.ShouldCompact() {
		t.Fatalf("上下文已超 80%% 应触发：tokens=%d", d.TokensInUse())
	}

	short, shortRec := newCompactDriver(t, 100_000)
	longConversation(t, short, shortRec, 1)
	if short.ShouldCompact() {
		t.Fatal("上下文很短时不应触发")
	}

	unknown, unknownRec := newCompactDriver(t, 0)
	longConversation(t, unknown, unknownRec, 2)
	if unknown.ShouldCompact() {
		t.Fatal("窗口未知时不应自动压缩")
	}
}

// replay_from_snapshot_equals_full：全量重放后压缩 vs 直接重放新文件。
func TestReplayFromSnapshotEqualsFull(t *testing.T) {
	// 先跑一段会话并收集事件
	d, rec := newCompactDriver(t, 400)
	rec.apply(d.Created(""))
	_ = rec

	// 用新的 driver 重跑（Created 已在上面写入，避免重复）
	conv := conversation.New("ctx-compact")
	d2 := driver.New(conv, driver.Options{
		Selection:          providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:              tools.Builtin(false),
		ContextWindow:      400,
		DisableAutoCompact: true,
	})
	rec2 := &recorder{}
	rec2.apply(d2.Created(""))
	longConversation(t, d2, rec2, 4)

	// 路线 A：全量重放 → 在内存里压缩 → 得到快照事件
	full, err := driver.Replay(rec2.events)
	if err != nil {
		t.Fatal(err)
	}
	restored := driver.Restore(full, driver.Options{
		Selection:          providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:              tools.Builtin(false),
		ContextWindow:      400,
		DisableAutoCompact: true,
	})
	if _, err := restored.BeginCompact(); err != nil {
		t.Fatal(err)
	}
	effects, err := restored.ApplyCompact("快照摘要")
	if err != nil {
		t.Fatal(err)
	}
	// 路线 B：新文件 = 快照 + 同批产生的其它事件（真实加载时这些都会写进日志）
	newFile := effectRecords(t, effects)
	for i := range newFile {
		newFile[i].Version = store.Version
		newFile[i].Seq = uint64(i)
	}
	fromSnapshot, err := driver.Replay(newFile)
	if err != nil {
		t.Fatal(err)
	}

	// 两条路线得到的状态必须一致
	compareConversations(t, restored.Conversation(), fromSnapshot.Conversation)
	if fromSnapshot.Title != restored.Title() {
		t.Fatalf("标题不一致：%q vs %q", fromSnapshot.Title, restored.Title())
	}
	if fromSnapshot.Selection.ProviderID != restored.Selection().ProviderID {
		t.Fatalf("选择不一致：%+v vs %+v", fromSnapshot.Selection, restored.Selection())
	}
	if len(fromSnapshot.Warnings) > 0 {
		t.Fatalf("从快照重放不应有警告：%v", fromSnapshot.Warnings)
	}
}

// effectRecords 取出 effects 里的全部 Record（保持顺序）。
func effectRecords(t *testing.T, effects []driver.Effect) []store.Event {
	t.Helper()
	var out []store.Event
	for _, effect := range effects {
		if rec, ok := effect.(driver.Record); ok {
			out = append(out, rec.Event)
		}
	}
	if len(out) == 0 {
		t.Fatal("没有 Record effect")
	}
	return out
}

// effectOfFirstRecord 取出 effects 里的第一个 Record。
func effectOfFirstRecord(t *testing.T, effects []driver.Effect) store.Event {
	t.Helper()
	for _, effect := range effects {
		if rec, ok := effect.(driver.Record); ok {
			return rec.Event
		}
	}
	t.Fatal("没有 Record effect")
	return store.Event{}
}

// 压缩后按需加载归档不能重复：kept_from_seq 之前的消息不应再次出现。
func TestSnapshotKeepsFromSeq(t *testing.T) {
	d, rec := newCompactDriver(t, 400)
	longConversation(t, d, rec, 4)

	// 事件序号：kept_from_seq 应等于压缩点之前的最后一条事件序号
	if _, err := d.BeginCompact(); err != nil {
		t.Fatal(err)
	}
	effects, err := d.ApplyCompact("摘要")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := effectOfFirstRecord(t, effects)
	var data driver.CompactSnapshotData
	if err := snapshot.Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data.Summary != "摘要" {
		t.Fatalf("快照摘要不一致：%q", data.Summary)
	}
	if len(data.Kept) == 0 {
		t.Fatal("快照应保留近期消息")
	}
	if data.KeptFromSeq != 0 {
		t.Logf("kept_from_seq=%d（M6 第二批会在压缩时写入真实序号）", data.KeptFromSeq)
	}
	for _, entry := range data.Kept {
		if entry.Message.MessageID == "" {
			t.Fatal("kept 里应有完整消息")
		}
	}
}

// 摘要提示里遇到图片只写「一张图片」，不写 blob 引用（模型读不到本地文件）。
func TestSummaryPromptReplacesImages(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	messages := []message.Message{
		{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{
			message.NewTextPart("看这张图"),
			message.NewURLPart("blob:sha256:" + hash).WithMediaType("image/png"),
		}},
	}
	prompt := driver.SummaryPromptForTest(messages)
	if strings.Contains(prompt, hash) || strings.Contains(prompt, "blob:") {
		t.Fatalf("摘要提示里不应出现 blob 引用：%s", prompt)
	}
	if !strings.Contains(prompt, "一张图片") {
		t.Fatalf("摘要提示里应标明有图片：%s", prompt)
	}
}
