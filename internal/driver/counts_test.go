package driver_test

import (
	"strings"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// 状态条的计数口径（driver.Counts）：
//   - Turns = 用户输入条数；
//   - Steps = 发给模型的请求次数。
//
// 下面这组测试把「在线」与「重放」两条路径钉在一起——这是状态条唯一容易出错
// 的地方（压缩会丢历史，计数必须靠快照基线接续）。
func TestCountsTurnsAndSteps(t *testing.T) {
	d, rec := newCompactDriver(t, 100_000)

	// 第一轮：用户一条 + agent 一条
	sendAndFinish(t, d, rec, "你好")
	if got, want := d.Counts(), (driver.Counts{Turns: 1, Steps: 1}); got != want {
		t.Fatalf("一轮之后计数应为 %+v，实际 %+v", want, got)
	}

	// 第二轮
	sendAndFinish(t, d, rec, "再来")
	if got, want := d.Counts(), (driver.Counts{Turns: 2, Steps: 2}); got != want {
		t.Fatalf("两轮之后计数应为 %+v，实际 %+v", want, got)
	}

	// 重放必须给出同一份数字
	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := (driver.Counts{Turns: replayed.Turns, Steps: replayed.Steps}), d.Counts(); got != want {
		t.Fatalf("重放的计数应与在线一致：重放 %+v、在线 %+v", got, want)
	}
}

// 工具续轮算步：一次用户输入 → 工具 → 再请求，Steps 应为 2、Turns 仍为 1。
func TestCountsToolRoundCountsAsStep(t *testing.T) {
	d, rec := newCompactDriver(t, 100_000)

	effects, err := d.Send("读一下文件")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	start, ok := effectOfType[driver.StartStream](effects)
	if !ok {
		t.Fatal("应开始流")
	}
	// 模型调用一个工具后正常结束（stop_reason=tool_calls）
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "list_files"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"path":"."}`})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"})
	rec.apply(effects)

	effects, err = d.ToolResults(start.MessageID, []tools.ToolOutcome{{
		ID: "c1", Name: "list_files", Output: "a.txt",
	}})
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	next, ok := effectOfType[driver.StartStream](effects)
	if !ok {
		t.Fatalf("工具跑完应继续下一轮请求（当前状态 %v）", d.Busy())
	}
	d.ApplyEvent(next.MessageID, providers.Event{Kind: providers.EventText, Text: "读到了"})
	effects, _ = d.ApplyEvent(next.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec.apply(effects)

	if got, want := d.Counts(), (driver.Counts{Turns: 1, Steps: 2}); got != want {
		t.Fatalf("一次输入 + 工具续轮应为 %+v，实际 %+v", want, got)
	}

	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Turns != 1 || replayed.Steps != 2 {
		t.Fatalf("重放应得到 turns=1 steps=2，实际 turns=%d steps=%d", replayed.Turns, replayed.Steps)
	}
}

// 插话投递会追加 user 消息：Turns 按消息条数计（与重放口径一致）。
func TestCountsSteerCountsEachMessage(t *testing.T) {
	d, rec := newCompactDriver(t, 100_000)

	effects, err := d.Send("第一条")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)

	// 生成中插话两条
	if _, err := d.Enqueue("插话一"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Enqueue("插话二"); err != nil {
		t.Fatal(err)
	}

	// 本轮结束 → 投递插话并开下一轮
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "好的"})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	effects = rec.apply(effects)
	next, ok := effectOfType[driver.StartStream](effects)
	if !ok {
		t.Fatal("插话应投递并开下一轮")
	}
	d.ApplyEvent(next.MessageID, providers.Event{Kind: providers.EventText, Text: "收到"})
	effects, _ = d.ApplyEvent(next.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec.apply(effects)

	// 3 条 user（原文 + 两条插话）、2 次请求
	if got, want := d.Counts(), (driver.Counts{Turns: 3, Steps: 2}); got != want {
		t.Fatalf("应为 %+v，实际 %+v", want, got)
	}

	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Turns != 3 || replayed.Steps != 2 {
		t.Fatalf("重放应得到 turns=3 steps=2，实际 turns=%d steps=%d", replayed.Turns, replayed.Steps)
	}
}

// 压缩会丢弃历史：计数必须靠快照里的基线接续，不能掉回「只剩 kept 的几条」。
func TestCountsSurviveCompaction(t *testing.T) {
	d, rec := newCompactDriver(t, 400)
	longConversation(t, d, rec, 4)

	before := d.Counts()
	if before.Turns != 4 || before.Steps != 4 {
		t.Fatalf("压缩前应为 4/4，实际 %+v", before)
	}

	if _, err := d.BeginCompact(); err != nil {
		t.Fatal(err)
	}
	effects, err := d.ApplyCompact("摘要")
	if err != nil {
		t.Fatal(err)
	}
	rec.apply(effects)

	if got := d.Counts(); got != before {
		t.Fatalf("压缩不该让计数变少：压缩前 %+v、之后 %+v", before, got)
	}

	// 新日志文件 = 快照开头：重放要能还原同一份计数
	newFile := effectRecords(t, effects)
	for i := range newFile {
		newFile[i].Version = store.Version
		newFile[i].Seq = uint64(i)
	}
	fromSnapshot, err := driver.Replay(newFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := (driver.Counts{Turns: fromSnapshot.Turns, Steps: fromSnapshot.Steps}); got != before {
		t.Fatalf("从快照重放的计数应为 %+v，实际 %+v", before, got)
	}

	// 恢复后继续对话，计数从基线往上加
	restored := driver.Restore(fromSnapshot, driver.Options{
		Selection:          providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:              tools.Builtin(false),
		ContextWindow:      400,
		DisableAutoCompact: true,
	})
	if got := restored.Counts(); got != before {
		t.Fatalf("Restore 后应还原计数 %+v，实际 %+v", before, got)
	}
	if _, err := restored.Send("压缩后继续"); err != nil {
		t.Fatal(err)
	}
	if got, want := restored.Counts(), (driver.Counts{Turns: before.Turns + 1, Steps: before.Steps + 1}); got != want {
		t.Fatalf("继续一轮后应为 %+v，实际 %+v", want, got)
	}
}

// 旧快照没有 turns/steps 字段：读出来是 0，此时按「快照之后的消息」计数，
// 不能因此把整个会话算成负数或崩掉。
func TestCountsTolerateLegacySnapshot(t *testing.T) {
	events := []store.Event{
		store.MustEvent(store.TypeCompactSnapshot, driver.CompactSnapshotData{
			ContextID:   "ctx-old",
			Summary:     "旧摘要",
			SummaryID:   "s1",
			CompactedAt: "2026-01-01T00:00:00Z",
		}),
	}
	events = withSeq(events)
	replayed, err := driver.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Turns != 0 || replayed.Steps != 0 {
		t.Fatalf("旧快照没有计数基线，应为 0/0，实际 %d/%d", replayed.Turns, replayed.Steps)
	}
}

// Stop 之后计数不回退（停止只是结束当前回合，历史还在）。
func TestCountsStayAfterStop(t *testing.T) {
	d, rec := newCompactDriver(t, 100_000)
	effects, err := d.Send("开始了")
	if err != nil {
		t.Fatal(err)
	}
	rec.apply(effects)
	if got, want := d.Counts(), (driver.Counts{Turns: 1, Steps: 1}); got != want {
		t.Fatalf("发送后应为 %+v，实际 %+v", want, got)
	}
	rec.apply(d.Stop())
	if got, want := d.Counts(), (driver.Counts{Turns: 1, Steps: 1}); got != want {
		t.Fatalf("停止后计数不该变：应为 %+v，实际 %+v", want, got)
	}
}

// sendAndFinish 发一条消息并让它正常结束。
func sendAndFinish(t *testing.T, d *driver.Driver, rec *recorder, text string) {
	t.Helper()
	effects, err := d.Send(text)
	if err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	effects = rec.apply(effects)
	start, ok := effectOfType[driver.StartStream](effects)
	if !ok {
		t.Fatal("应开始流")
	}
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "回复：" + text})
	if effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"}); len(effects) > 0 {
		rec.apply(effects)
	}
}

// withSeq 给事件补上连续的序号（Replay 要求序号连续，否则会截断）。
func withSeq(events []store.Event) []store.Event {
	out := make([]store.Event, len(events))
	for i, event := range events {
		event.Version = store.Version
		event.Seq = uint64(i)
		out[i] = event
	}
	return out
}

// 计数相关的会话必须仍然可重放（回归守卫：新增的计数逻辑不该改变会话内容）。
func TestCountsDoNotAffectHistory(t *testing.T) {
	d, rec := newCompactDriver(t, 100_000)
	sendAndFinish(t, d, rec, "一句话")
	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}
	compareConversations(t, d.Conversation(), replayed.Conversation)
	history := replayed.Conversation.History()
	if len(history) != 2 {
		t.Fatalf("历史应是 1 条用户 + 1 条 agent，实际 %d 条：%v", len(history), len(history))
	}
	for _, msg := range history {
		if strings.TrimSpace(msg.MessageID) == "" {
			t.Fatal("历史消息 id 不该为空")
		}
	}
	_ = conversation.StatusComplete
}
