package contextwindow

import (
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestTrackerAccumulatesUsageForSessionCost(t *testing.T) {
	tracker := NewTracker(Window{Tokens: 1000, Source: WindowSourceConfigured}, Metadata{})
	tracker.RecordEstimatedUsage(100, 20)
	tracker.RecordProviderUsage(model.Usage{
		InputTokens: 10, OutputTokens: 5, TotalTokens: 25,
		CachedTokens: 8, CacheWriteTokens: 2, ReasoningTokens: 3,
	})
	tracker.RecordProviderUsage(model.Usage{
		InputTokens: 4, OutputTokens: 6, TotalTokens: 10,
		CachedTokens: 0, CacheWriteTokens: 1, ReasoningTokens: 2,
	})

	got := tracker.Metadata()
	if got.TotalInputTokens != 14 || got.TotalOutputTokens != 11 || got.TotalTokens != 35 {
		t.Fatalf("total usage = input %d output %d total %d, want 14/11/35", got.TotalInputTokens, got.TotalOutputTokens, got.TotalTokens)
	}
	if got.TotalCachedTokens != 8 || got.TotalCacheWriteTokens != 3 || got.TotalReasoningTokens != 5 {
		t.Fatalf("total cache/reasoning = %d/%d/%d, want 8/3/5", got.TotalCachedTokens, got.TotalCacheWriteTokens, got.TotalReasoningTokens)
	}
	if got.LastInputTokens != 4 || got.LastOutputTokens != 6 {
		t.Fatalf("last usage = %d/%d, want 4/6", got.LastInputTokens, got.LastOutputTokens)
	}
}

func TestTrackerSplitsProviderUsageByContextLength(t *testing.T) {
	tracker := NewTracker(Window{Tokens: 1000, Source: WindowSourceConfigured}, Metadata{})
	tracker.SetLongContextTokenThreshold(100)
	tracker.RecordProviderUsage(model.Usage{InputTokens: 90, OutputTokens: 5, TotalTokens: 95})
	tracker.RecordProviderUsage(model.Usage{InputTokens: 101, OutputTokens: 6, TotalTokens: 107})

	got := tracker.Metadata()
	if got.TotalShortInputTokens != 90 || got.TotalShortOutputTokens != 5 {
		t.Fatalf("short usage = input %d output %d, want 90/5", got.TotalShortInputTokens, got.TotalShortOutputTokens)
	}
	if got.TotalLongInputTokens != 101 || got.TotalLongOutputTokens != 6 {
		t.Fatalf("long usage = input %d output %d, want 101/6", got.TotalLongInputTokens, got.TotalLongOutputTokens)
	}
	if got.TotalRequests != 2 {
		t.Fatalf("total requests = %d, want 2", got.TotalRequests)
	}
}
