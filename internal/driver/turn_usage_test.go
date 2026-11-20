package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// 没压缩过的会话，打开时也要能算出累计用量 —— 否则状态条不显示缓存命中率。
//
// 用户报的："刚打开会话时，不会显示缓存命中率，理论上应该可以计算出来了"。
// 根因：以前累计用量只存在**压缩快照**里，没压缩过的会话日志中一个用量
// 数字都没有，重放出来是 0，命中率判据（CacheRead > 0）不成立 → 不显示。
//
// 现在 turn.finished 带上本次请求的 usage，重放时逐条累加。
func TestReplaySumsUsageFromTurns(t *testing.T) {
	events := withSeq([]store.Event{
		store.MustEvent(store.TypeSessionCreated, map[string]any{
			"context_id": "ctx-u", "title": "会话", "created_at": "2026-01-01T00:00:00Z",
		}),
		// 三次请求，每次带不同用量
		store.MustEvent(store.TypeTurnFinished, map[string]any{
			"round": 1, "turn": 0, "duration_ms": 100,
			"usage": map[string]any{"input": 1000, "output": 50, "cache_read": 800},
		}),
		store.MustEvent(store.TypeTurnFinished, map[string]any{
			"round": 1, "turn": 1, "duration_ms": 100,
			"usage": map[string]any{"input": 2000, "output": 80, "cache_read": 1900},
		}),
		store.MustEvent(store.TypeTurnFinished, map[string]any{
			"round": 2, "turn": 2, "duration_ms": 100,
			"usage": map[string]any{"input": 500, "output": 20, "cache_read": 0, "cache_write": 100},
		}),
	})

	replayed, err := driver.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	got := replayed.TotalUsage
	if got.InputTokens != 3500 || got.OutputTokens != 150 ||
		got.CacheReadTokens != 2700 || got.CacheWriteTokens != 100 {
		t.Fatalf("应累加为 input=3500 output=150 read=2700 write=100，实际 %+v", got)
	}
}

// 旧日志（turn.finished 没有 usage 字段）要按 0 处理，不报错。
func TestReplayOldTurnsWithoutUsage(t *testing.T) {
	events := withSeq([]store.Event{
		store.MustEvent(store.TypeSessionCreated, map[string]any{
			"context_id": "ctx-old", "title": "旧会话", "created_at": "2026-01-01T00:00:00Z",
		}),
		store.MustEvent(store.TypeTurnFinished, map[string]any{
			"round": 1, "turn": 0, "duration_ms": 100,
		}),
	})
	replayed, err := driver.Replay(events)
	if err != nil {
		t.Fatalf("旧日志不该解析失败：%v", err)
	}
	if replayed.TotalUsage != (driver.Usage{}) {
		t.Fatalf("没有用量数据时应为零值，实际 %+v", replayed.TotalUsage)
	}
}

// turn.finished 必须真的把用量写进日志（不只是能读）。
func TestTurnFinishedRecordsUsage(t *testing.T) {
	dir := t.TempDir()
	file, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	d, messageID := newTurnUsageDriver(t)
	_, _ = d.ApplyEvent(messageID, providers.Event{
		Kind:            providers.EventUsage,
		InputTokens:     1234,
		OutputTokens:    56,
		CacheReadTokens: 1000,
		CacheReported:   true,
	})
	effects := d.FinishStream(messageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	// 把 effects 里的记录写进日志，再读回来重放
	found := false
	for _, effect := range effects {
		rec, ok := effect.(driver.Record)
		if !ok {
			continue
		}
		if err := file.Append(rec.Event); err != nil {
			t.Fatal(err)
		}
		if rec.Event.Type == store.TypeTurnFinished {
			var data struct {
				Usage *struct {
					Input     int `json:"input"`
					CacheRead int `json:"cache_read"`
				} `json:"usage"`
			}
			if err := rec.Event.Decode(&data); err != nil {
				t.Fatal(err)
			}
			if data.Usage == nil {
				t.Fatal("turn.finished 应带 usage")
			}
			if data.Usage.Input != 1234 || data.Usage.CacheRead != 1000 {
				t.Fatalf("usage 应是 input=1234 read=1000，实际 %+v", data.Usage)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("没有产出 turn.finished")
	}

	loaded, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := driver.Replay(loaded.Events)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.TotalUsage.CacheReadTokens != 1000 {
		t.Fatalf("重放后应能读回 cache_read=1000，实际 %d", replayed.TotalUsage.CacheReadTokens)
	}
}

// newTurnUsageDriver 造一个 driver 并起一轮流。
func newTurnUsageDriver(t *testing.T) (*driver.Driver, string) {
	t.Helper()
	conv := conversation.New("ctx-turn")
	d := driver.New(conv, driver.Options{
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(false),
	})
	effects, err := d.Send("你好")
	if err != nil {
		t.Fatal(err)
	}
	id := findStartStream(effects)
	if id == "" {
		t.Fatal("没有 StartStream")
	}
	return d, id
}