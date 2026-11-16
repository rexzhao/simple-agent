package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// 重放后每条消息都应带上日志 seq 区间（分页游标靠它）。
func TestReplayFillsSeqRange(t *testing.T) {
	conv := conversation.New("ctx")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	effects, _ := d.Send("你好")
	rec.apply(effects)
	start := startStream(t, effects)
	doneEffects, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec.apply(doneEffects)

	replayed, err := driver.Replay(rec.events)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range replayed.Conversation.Entries() {
		if entry.Message == nil {
			continue
		}
		first, last := entry.Message.SeqRange()
		t.Logf("消息 %s role=%s seq=[%d,%d]",
			entry.Message.Message.MessageID, entry.Message.Message.Role, first, last)
		if last == 0 {
			t.Fatalf("重放后消息 %s 没有 seq（分页游标会失效）",
				entry.Message.Message.MessageID)
		}
	}
	_ = store.Version
}