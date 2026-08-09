package sessioncontent

import (
	"testing"

	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

func TestSubscriptionEventFromExecutionMapsLiveRunSemantics(t *testing.T) {
	tests := []struct {
		name       string
		source     execution.SessionStreamEvent
		wantType   protocol.SubscriptionEventType
		wantItemID string
	}{
		{"text", execution.NewSessionStreamEvent("text.delta", map[string]any{"turn_id": "turn-1", "agent_iteration": 1, "item_id": "item-1", "text": "hello"}), protocol.SubscriptionEventTextDelta, "item-1"},
		{"reasoning", execution.NewSessionStreamEvent("reasoning.delta", map[string]any{"turn_id": "turn-1", "agent_iteration": 1, "item_id": "item-1", "text": "think"}), protocol.SubscriptionEventReasoningDelta, "item-1"},
		{"requested", execution.NewSessionStreamEvent("tool.requested", map[string]any{"turn_id": "turn-1", "agent_iteration": 1, "tool_call_id": "call-1", "name": "shell", "arguments": "{}"}), protocol.SubscriptionEventToolRequested, ""},
		{"running", execution.NewSessionStreamEvent("tool.started", map[string]any{"turn_id": "turn-1", "agent_iteration": 1, "tool_call_id": "call-1", "name": "shell"}), protocol.SubscriptionEventToolRunning, ""},
		{"progress", execution.NewSessionStreamEvent("tool.progress", map[string]any{"turn_id": "turn-1", "agent_iteration": 1, "tool_call_id": "call-1", "name": "shell", "arguments_delta": "x"}), protocol.SubscriptionEventToolProgress, ""},
		{"finished", execution.NewSessionStreamEvent("tool.finished", map[string]any{"turn_id": "turn-1", "agent_iteration": 1, "tool_call_id": "call-1", "name": "shell", "is_error": false}), protocol.SubscriptionEventToolFinished, ""},
		{"queue", execution.NewSessionStreamEvent("run.prompt_queue", map[string]any{"prompts": []map[string]any{{"id": "p-1", "content": "next", "steer": true}}}), protocol.SubscriptionEventPromptQueue, ""},
		{"appended", execution.NewSessionStreamEvent("run.prompt_appended", map[string]any{"prompts": []string{"next"}}), protocol.SubscriptionEventPromptAppended, ""},
		{"failure", execution.NewSessionStreamEvent("turn.failed", map[string]any{"turn_id": "turn-1", "code": "model_http_error", "message": "429: slow down and try again"}), protocol.SubscriptionEventTurnFailed, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := subscriptionEventFromExecution(test.source, "session-1", "run-1")
			if err != nil {
				t.Fatal(err)
			}
			if !ok || got.Type != test.wantType || got.SessionID != "session-1" || got.RunID != "run-1" {
				t.Fatalf("mapped event = %#v/%t, want type %q with identity", got, ok, test.wantType)
			}
			if got.ItemID != test.wantItemID {
				t.Fatalf("item id = %q, want %q", got.ItemID, test.wantItemID)
			}
			got.RunCursor = "1"
			if err := got.Validate(); err != nil {
				t.Fatalf("mapped event validation: %v", err)
			}
		})
	}
}

func TestSubscriptionEventFromExecutionRejectsTextWithoutStableItem(t *testing.T) {
	_, ok, err := subscriptionEventFromExecution(execution.NewSessionStreamEvent("text.delta", map[string]any{
		"turn_id": "turn-1", "agent_iteration": 1, "text": "cannot fabricate an id",
	}), "session-1", "run-1")
	if ok || err == nil {
		t.Fatalf("text without item identity = ok %t, err %v; want conservative rejection", ok, err)
	}
}

func TestDurableExecutionNoticesDoNotCreateTransientGaps(t *testing.T) {
	for _, typeName := range []string{"item.created", "item.updated", "active_history.replaced", "usage.updated", "turn.committed"} {
		if isTransientExecutionEvent(execution.NewSessionStreamEvent(typeName, nil)) {
			t.Fatalf("execution notice %q was classified as transient", typeName)
		}
	}
}
