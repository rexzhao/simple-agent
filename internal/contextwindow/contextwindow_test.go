package contextwindow

import (
	"context"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestTrackingProviderPrefersProviderUsageEvent(t *testing.T) {
	tracker := NewTracker(Window{Tokens: 1000, Source: WindowSourceConfigured}, Metadata{})
	provider := TrackingProvider{
		Inner: fakeProvider{events: []model.Event{
			model.TextDeltaEvent{Text: "fallback text should not win"},
			model.UsageEvent{Usage: model.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
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
	if metadata.LastOutputTokens != 4 || metadata.LastTotalTokens != metadata.LastInputTokens+4 {
		t.Fatalf("metadata usage = input %d output %d total %d, want estimated output 4", metadata.LastInputTokens, metadata.LastOutputTokens, metadata.LastTotalTokens)
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
