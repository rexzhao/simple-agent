package agent

import (
	"context"
	"testing"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
)

func TestStreamWithResultUsesDistinctDurableTurnsPerModelIteration(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "echo", Arguments: `{}`}}},
		{model.TextDeltaEvent{Text: "final"}},
	}}
	published := make([]eventbus.Event, 0)
	publisher := eventbus.NewBus(func(event eventbus.Event) error {
		published = append(published, event)
		return nil
	})
	defer publisher.Close()
	executor := &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "tool output"}}
	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model: "model-test", Messages: []model.Message{{Role: model.MessageRoleUser, Content: "use tool"}},
		Tools: []model.Tool{{Name: "echo"}},
	}, Options{
		Provider: provider, ToolExecutor: executor, MaxTurns: 4, TurnID: "turn-1", Publisher: publisher,
		NextTurnID: func(int) string { return "turn-2" },
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	for range events {
	}
	if _, ok := <-results; !ok {
		t.Fatal("agent result channel closed without result")
	}
	completedIndex := -1
	startedIndex := -1
	for index, event := range published {
		if completed, ok := event.(eventbus.TurnCompleted); ok && completed.TurnID == "turn-1" {
			completedIndex = index
		}
		if started, ok := event.(eventbus.TurnStarted); ok && started.TurnID == "turn-2" {
			startedIndex = index
		}
	}
	if completedIndex < 0 || startedIndex != completedIndex+1 {
		t.Fatalf("published durable events = %#v, want adjacent turn-1 completion/turn-2 start", published)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
}
