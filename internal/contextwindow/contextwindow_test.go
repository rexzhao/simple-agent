package contextwindow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestTrackingProviderPrefersProviderUsageEvent(t *testing.T) {
	tracker := NewTracker(Window{Tokens: 1000, Source: WindowSourceConfigured}, Metadata{})
	provider := TrackingProvider{
		Inner: fakeProvider{events: []model.Event{
			model.TextDeltaEvent{Text: "fallback text should not win"},
			model.UsageEvent{Usage: model.Usage{
				InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
				CachedTokens: 8, CacheWriteTokens: 2, ReasoningTokens: 3,
			}},
		}},
		Tracker: tracker,
	}

	events, err := provider.Stream(context.Background(), model.Request{
		Model:    "model",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	drainEvents(t, events)

	metadata := tracker.Metadata()
	if metadata.LastUsageSource != string(UsageSourceProvider) {
		t.Fatalf("LastUsageSource = %q, want provider", metadata.LastUsageSource)
	}
	if metadata.LastInputTokens != 10 || metadata.LastOutputTokens != 5 || metadata.LastTotalTokens != 15 {
		t.Fatalf("metadata usage = input %d output %d total %d, want 10/5/15", metadata.LastInputTokens, metadata.LastOutputTokens, metadata.LastTotalTokens)
	}
	if metadata.LastUsageCountTokens != 15 {
		t.Fatalf("LastUsageCountTokens = %d, want provider total 15", metadata.LastUsageCountTokens)
	}
	if metadata.LastCachedTokens != 8 || metadata.LastCacheWriteTokens != 2 || metadata.LastReasoningTokens != 3 {
		t.Fatalf("metadata details = cached %d write %d reasoning %d, want 8/2/3", metadata.LastCachedTokens, metadata.LastCacheWriteTokens, metadata.LastReasoningTokens)
	}
}

func TestTrackerUsageCountFallsBackToComponentsIncludingCache(t *testing.T) {
	tracker := NewTracker(Window{Tokens: 1000, Source: WindowSourceConfigured}, Metadata{})
	tracker.RecordProviderUsage(model.Usage{
		InputTokens:      10,
		OutputTokens:     5,
		CachedTokens:     8,
		CacheWriteTokens: 2,
	})

	metadata := tracker.Metadata()
	if metadata.LastUsageCountTokens != 25 {
		t.Fatalf("LastUsageCountTokens = %d, want 10+5+8+2", metadata.LastUsageCountTokens)
	}
	if metadata.LastTotalTokens != 15 {
		t.Fatalf("LastTotalTokens = %d, want normalized input+output total 15", metadata.LastTotalTokens)
	}
}

func TestTrackingProviderRecordsFallbackEstimateWhenUsageIsMissing(t *testing.T) {
	request := model.Request{
		Model:    "model",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "hello"}},
	}
	tracker := NewTracker(Window{Tokens: 1000, Source: WindowSourceConfigured}, Metadata{})
	provider := TrackingProvider{
		Inner: fakeProvider{events: []model.Event{
			model.TextDeltaEvent{Text: "abcd"},
		}},
		Tracker: tracker,
	}

	events, err := provider.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	drainEvents(t, events)

	metadata := tracker.Metadata()
	if metadata.LastUsageSource != string(UsageSourceEstimated) {
		t.Fatalf("LastUsageSource = %q, want estimated", metadata.LastUsageSource)
	}
	if want := EstimateRequestTokens(request); metadata.LastInputTokens != want {
		t.Fatalf("LastInputTokens = %d, want request estimate %d", metadata.LastInputTokens, want)
	}
	if metadata.LastOutputTokens != 1 || metadata.LastTotalTokens != metadata.LastInputTokens+1 {
		t.Fatalf("metadata usage = input %d output %d total %d, want estimated output 1", metadata.LastInputTokens, metadata.LastOutputTokens, metadata.LastTotalTokens)
	}
}

func TestTrackingProviderUsesProviderUsageAsNextRequestAnchor(t *testing.T) {
	tracker := NewTracker(Window{Tokens: 1000, Source: WindowSourceConfigured}, Metadata{})
	first := TrackingProvider{
		Inner: fakeProvider{events: []model.Event{
			model.UsageEvent{Usage: model.Usage{InputTokens: 90, OutputTokens: 10, TotalTokens: 100}},
		}},
		Tracker: tracker,
	}
	prefix := []model.Message{
		{Role: model.MessageRoleSystem, Content: strings.Repeat("a", 3000)},
	}
	events, err := first.Stream(context.Background(), model.Request{Model: "model", Messages: prefix})
	if err != nil {
		t.Fatalf("first Stream() error = %v", err)
	}
	drainEvents(t, events)

	called := false
	second := TrackingProvider{
		Inner:   fakeProvider{called: &called},
		Tracker: tracker,
	}
	trailing := model.Message{
		Role:       model.MessageRoleTool,
		ToolCallID: "call-1",
		Content:    strings.Repeat("b", 2400),
	}
	nextRequest := model.Request{
		Model: "model",
		Messages: append(append(append([]model.Message{}, prefix...),
			model.Message{Role: model.MessageRoleAssistant, Content: "answer"}),
			trailing),
	}
	if full := EstimateRequestTokens(nextRequest); full < 1000 {
		t.Fatalf("full request estimate = %d, want at least context window", full)
	}
	events, err = second.Stream(context.Background(), nextRequest)
	if err != nil {
		t.Fatalf("second Stream() error = %v", err)
	}
	drainEvents(t, events)
	if !called {
		t.Fatal("inner provider was not called for request covered by provider usage anchor")
	}

	want := 100 + EstimateMessageTokens(trailing)
	if got := tracker.Metadata().LastRequestTokens; got != want {
		t.Fatalf("LastRequestTokens = %d, want provider usage plus trailing estimate %d", got, want)
	}
}

func TestProviderUsageAnchorSurvivesMetadataReload(t *testing.T) {
	tracker := NewTracker(Window{Tokens: 1000, Source: WindowSourceConfigured}, Metadata{})
	prefix := []model.Message{{Role: model.MessageRoleSystem, Content: strings.Repeat("a", 3000)}}
	provider := TrackingProvider{
		Inner: fakeProvider{events: []model.Event{
			model.UsageEvent{Usage: model.Usage{InputTokens: 90, OutputTokens: 10, TotalTokens: 100}},
		}},
		Tracker: tracker,
	}
	events, err := provider.Stream(context.Background(), model.Request{Model: "model", Messages: prefix})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	drainEvents(t, events)

	reloaded := NewTracker(Window{Tokens: 1000, Source: WindowSourceConfigured}, tracker.Metadata())
	trailing := model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: strings.Repeat("b", 2400)}
	estimate, _, err := reloaded.CheckRequest(model.Request{
		Model: "model",
		Messages: append(append(append([]model.Message{}, prefix...),
			model.Message{Role: model.MessageRoleAssistant, Content: "answer"}),
			trailing),
	})
	if err != nil {
		t.Fatalf("CheckRequest() error = %v", err)
	}
	if want := 100 + EstimateMessageTokens(trailing); estimate.InputTokens != want {
		t.Fatalf("InputTokens = %d, want provider usage plus trailing estimate %d", estimate.InputTokens, want)
	}
}

func TestTrackingProviderDiscardsUsageAnchorWhenPrefixChanges(t *testing.T) {
	tracker := NewTracker(Window{Tokens: 1000, Source: WindowSourceConfigured}, Metadata{})
	provider := TrackingProvider{
		Inner: fakeProvider{events: []model.Event{
			model.UsageEvent{Usage: model.Usage{InputTokens: 90, OutputTokens: 10, TotalTokens: 100}},
		}},
		Tracker: tracker,
	}
	events, err := provider.Stream(context.Background(), model.Request{
		Model:    "model",
		Messages: []model.Message{{Role: model.MessageRoleSystem, Content: strings.Repeat("a", 3000)}},
	})
	if err != nil {
		t.Fatalf("first Stream() error = %v", err)
	}
	drainEvents(t, events)

	called := false
	provider = TrackingProvider{Inner: fakeProvider{called: &called}, Tracker: tracker}
	_, err = provider.Stream(context.Background(), model.Request{
		Model: "model",
		Messages: []model.Message{
			{Role: model.MessageRoleSystem, Content: strings.Repeat("changed", 500)},
			{Role: model.MessageRoleAssistant, Content: "answer"},
			{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: strings.Repeat("b", 2400)},
		},
	})
	var budgetErr *BudgetExceededError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("Stream() error = %v, want BudgetExceededError", err)
	}
	if called {
		t.Fatal("inner provider was called after provider usage anchor prefix changed")
	}
}

func TestTrackingProviderRejectsOverBudgetBeforeProviderRequest(t *testing.T) {
	called := false
	tracker := NewTracker(Window{Tokens: 10, Source: WindowSourceConfigured}, Metadata{})
	provider := TrackingProvider{
		Inner:   fakeProvider{called: &called},
		Tracker: tracker,
	}

	_, err := provider.Stream(context.Background(), model.Request{
		Model:    "model",
		Messages: []model.Message{{Role: model.MessageRoleSystem, Content: "system secret"}, {Role: model.MessageRoleUser, Content: "user secret"}},
	})
	if err == nil {
		t.Fatal("Stream() error = nil, want over-budget error")
	}
	if called {
		t.Fatal("inner provider was called for an over-budget request")
	}
}

func TestEstimateTextTokensUsesMixedCharacterHeuristic(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{name: "empty", text: "", want: 0},
		{name: "four ascii characters", text: "abcd", want: 1},
		{name: "five ascii characters", text: "abcde", want: 2},
		{name: "non ascii characters", text: "你好", want: 2},
		{name: "mixed text", text: "abcd你好", want: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := EstimateTextTokens(test.text); got != test.want {
				t.Fatalf("EstimateTextTokens(%q) = %d, want %d", test.text, got, test.want)
			}
		})
	}
}

type fakeProvider struct {
	events []model.Event
	called *bool
}

func (p fakeProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if p.called != nil {
		*p.called = true
	}
	events := make(chan model.Event, len(p.events))
	for _, event := range p.events {
		events <- event
	}
	close(events)
	return events, nil
}

func drainEvents(t *testing.T, events <-chan model.Event) {
	t.Helper()
	for range events {
	}
}
