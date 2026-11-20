package driver_test

import (
	"testing"

	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// 旧日志（快照里没有 total_usage）要按 0/0 兼容，不能报错、不能瞎编。
//
// 用户明确要求："要兼容之前的日志的话，如果日志开头没有记录就按照 0/0 来计算"。
// 0/0 的含义是"这个会话还没有准确的累计数字"——界面据此不显示命中率
// （判据是 CacheRead > 0），等这个会话跑一轮真实请求自然就出现了。
func TestOldSnapshotWithoutTotalUsage(t *testing.T) {
	// 模拟旧版本写下的快照：只有 summary 与 kept，没有 total_usage
	oldSnapshot := store.MustEvent(store.TypeCompactSnapshot, map[string]any{
		"context_id":         "ctx-old",
		"summary":            "之前的摘要",
		"summary_message_id": "summary-1",
		"kept_entries":       []any{},
		"compacted_at":       "2026-01-01T00:00:00Z",
		// 注意：**故意不写 turns/steps/total_usage**，这是老日志的样子
	})

	replayed, err := driver.Replay([]store.Event{oldSnapshot})
	if err != nil {
		t.Fatalf("旧日志不该解析失败：%v", err)
	}
	got := replayed.TotalUsage
	if got.InputTokens != 0 || got.CacheReadTokens != 0 || got.CacheWriteTokens != 0 {
		t.Fatalf("没有累计基线时应当是 0/0，实际 %+v", got)
	}

	// 恢复出的 driver 也要是零值（而不是崩掉或带上别的会话的数字）
	restored, _, err := driver.Load([]store.Event{oldSnapshot}, driver.Options{
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(false),
	})
	if err != nil {
		t.Fatalf("旧日志不该加载失败：%v", err)
	}
	if got := restored.TotalUsage(); got != (driver.Usage{}) {
		t.Fatalf("旧快照恢复后累计用量应为零值，实际 %+v", got)
	}
}

// 新快照带 total_usage 时要能读回来（字段名是长期契约，别写错）。
func TestNewSnapshotCarriesTotalUsage(t *testing.T) {
	snapshot := store.MustEvent(store.TypeCompactSnapshot, map[string]any{
		"context_id":         "ctx-new",
		"summary":            "摘要",
		"summary_message_id": "summary-1",
		"kept_entries":       []any{},
		"compacted_at":       "2026-01-01T00:00:00Z",
		"total_usage": map[string]any{
			"input":       10000,
			"output":      500,
			"cache_read":  9500,
			"cache_write": 200,
		},
	})

	replayed, err := driver.Replay([]store.Event{snapshot})
	if err != nil {
		t.Fatal(err)
	}
	got := replayed.TotalUsage
	if got.InputTokens != 10000 || got.OutputTokens != 500 ||
		got.CacheReadTokens != 9500 || got.CacheWriteTokens != 200 {
		t.Fatalf("应从快照读出 input=10000 output=500 read=9500 write=200，实际 %+v", got)
	}
}