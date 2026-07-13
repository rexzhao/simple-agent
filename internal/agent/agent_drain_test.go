package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
)

// drainSequence returns a drain callback that returns the given batches in
// order, one per call, then nil thereafter.
func drainSequence(batches ...[]model.Message) (ActivePromptDrain, *int) {
	calls := 0
	drain := ActivePromptDrain(func() []model.Message {
		calls++
		if calls <= len(batches) {
			return batches[calls-1]
		}
		return nil
	})
	return drain, &calls
}

func publisherKinds(publisher *fakePublisher) []string {
	kinds := make([]string, 0, len(publisher.events))
	for _, event := range publisher.events {
		kinds = append(kinds, event.Kind())
	}
	return kinds
}

func publisherTurnInputReady(publisher *fakePublisher) []model.Message {
	var msgs []model.Message
	for _, event := range publisher.events {
		if input, ok := event.(eventbus.TurnInputReady); ok {
			msgs = append(msgs, input.Message)
		}
	}
	return msgs
}

func TestStreamDrainsActivePromptsBeforeFirstProviderRequest(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "final"}},
		},
	}
	publisher := &fakePublisher{}
	// First drain call (checkpoint 1) returns queued input; later calls nil.
	drain, calls := drainSequence([]model.Message{
		{Role: model.MessageRoleUser, Content: "pre-queued"},
	})

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "original"}},
	}, Options{
		Provider:          provider,
		TurnID:            "turn-1",
		Publisher:         publisher,
		ActivePromptDrain: drain,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "final" {
		t.Fatalf("text events = %q, want final", gotText)
	}
	collectTurnResult(t, results)

	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.requests))
	}
	first := provider.requests[0].Messages
	if len(first) != 2 || first[0].Content != "original" || first[1].Content != "pre-queued" {
		t.Fatalf("first request messages = %#v, want original + pre-queued as separate user items", first)
	}
	if first[0].Role != model.MessageRoleUser || first[1].Role != model.MessageRoleUser {
		t.Fatalf("first request roles = %q/%q, want both user", first[0].Role, first[1].Role)
	}
	// Drain is polled again at checkpoint 3 (no-tool terminal), so 2 calls total.
	if *calls != 2 {
		t.Fatalf("drain calls = %d, want 2", *calls)
	}
	if got := publisherKinds(publisher); !equalStrings(got, []string{eventbus.KindTurnInputReady, eventbus.KindAssistantReady}) {
		t.Fatalf("publisher kinds = %#v, want TurnInputReady then AssistantReady", got)
	}
	inputs := publisherTurnInputReady(publisher)
	if len(inputs) != 1 || inputs[0].Content != "pre-queued" {
		t.Fatalf("TurnInputReady messages = %#v, want pre-queued", inputs)
	}
}

func TestStreamDrainsActivePromptsAfterToolBatchAsSeparateUserItems(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_2", Name: "echo", Arguments: `{}`},
				},
			},
			{model.TextDeltaEvent{Text: "final"}},
		},
	}
	executor := &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "tool output"}}
	publisher := &fakePublisher{}
	// Checkpoint 1 (turn 1) nil; checkpoint 2 (after tool batch) returns two
	// queued user messages; checkpoint 3 (turn 2 terminal) nil.
	drain, _ := drainSequence(
		nil,
		[]model.Message{
			{Role: model.MessageRoleUser, Content: "more1"},
			{Role: model.MessageRoleUser, Content: "more2"},
		},
	)

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "original"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:          provider,
		ToolExecutor:      executor,
		MaxTurns:          4,
		TurnID:            "turn-1",
		Publisher:         publisher,
		ActivePromptDrain: drain,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "final" {
		t.Fatalf("text events = %q, want final", gotText)
	}
	collectTurnResult(t, results)

	// One assistant message carrying two tool calls produces two tool result
	// messages. Both tool results (and their durable ToolResultReady events)
	// must precede any drained TurnInputReady/user messages, proving no user
	// insertion inside a multi-tool batch.
	second := provider.requests[1].Messages
	if len(second) != 6 {
		t.Fatalf("second request messages = %d, want 6: %#v", len(second), second)
	}
	assertAgentMessage(t, second[0], model.MessageRoleUser, "original", "")
	if second[1].Role != model.MessageRoleAssistant || len(second[1].ToolCalls) != 2 ||
		second[1].ToolCalls[0].ID != "call_1" || second[1].ToolCalls[1].ID != "call_2" {
		t.Fatalf("assistant message = %#v, want two tool calls call_1,call_2", second[1])
	}
	assertAgentMessage(t, second[2], model.MessageRoleTool, "tool output", "call_1")
	assertAgentMessage(t, second[3], model.MessageRoleTool, "tool output", "call_2")
	assertAgentMessage(t, second[4], model.MessageRoleUser, "more1", "")
	assertAgentMessage(t, second[5], model.MessageRoleUser, "more2", "")

	wantKinds := []string{
		eventbus.KindAssistantReady,
		eventbus.KindToolResultReady,
		eventbus.KindToolResultReady,
		eventbus.KindTurnInputReady,
		eventbus.KindTurnInputReady,
		eventbus.KindAssistantReady,
	}
	if got := publisherKinds(publisher); !equalStrings(got, wantKinds) {
		t.Fatalf("publisher kinds = %#v, want %v", got, wantKinds)
	}
	inputs := publisherTurnInputReady(publisher)
	if len(inputs) != 2 || inputs[0].Content != "more1" || inputs[1].Content != "more2" {
		t.Fatalf("TurnInputReady messages = %#v, want more1 then more2 in order", inputs)
	}
}

func TestStreamDrainsActivePromptsAfterNoToolResponseExtendsTurn(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "first"}},
			{model.TextDeltaEvent{Text: "second"}},
		},
	}
	publisher := &fakePublisher{}
	// Checkpoint 1 nil; checkpoint 3 after "first" returns a followup; checkpoint
	// 3 after "second" nil (terminal).
	drain, _ := drainSequence(
		nil,
		[]model.Message{{Role: model.MessageRoleUser, Content: "followup"}},
	)

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "original"}},
	}, Options{
		Provider:          provider,
		MaxTurns:          4,
		TurnID:            "turn-1",
		Publisher:         publisher,
		ActivePromptDrain: drain,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "firstsecond" {
		t.Fatalf("text events = %q, want first+second", gotText)
	}
	turnResult := collectTurnResult(t, results)

	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	second := provider.requests[1].Messages
	if len(second) != 3 {
		t.Fatalf("second request messages = %d, want 3: %#v", len(second), second)
	}
	assertAgentMessage(t, second[0], model.MessageRoleUser, "original", "")
	assertAgentMessage(t, second[1], model.MessageRoleAssistant, "first", "")
	assertAgentMessage(t, second[2], model.MessageRoleUser, "followup", "")

	if len(turnResult.Messages) != 4 {
		t.Fatalf("result messages = %d, want 4: %#v", len(turnResult.Messages), turnResult.Messages)
	}
	assertAgentMessage(t, turnResult.Messages[3], model.MessageRoleAssistant, "second", "")

	wantKinds := []string{
		eventbus.KindAssistantReady,
		eventbus.KindTurnInputReady,
		eventbus.KindAssistantReady,
	}
	if got := publisherKinds(publisher); !equalStrings(got, wantKinds) {
		t.Fatalf("publisher kinds = %#v, want %v", got, wantKinds)
	}
}

func TestStreamDrainRejectsNonUserRoleAndStops(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "should not reach"}},
		},
	}
	publisher := &fakePublisher{}
	drain, _ := drainSequence([]model.Message{
		{Role: model.MessageRoleAssistant, Content: "bad role"},
	})

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "original"}},
	}, Options{
		Provider:          provider,
		TurnID:            "turn-1",
		Publisher:         publisher,
		ActivePromptDrain: drain,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if errorEvent.Err == nil || !strings.Contains(errorEvent.Err.Error(), "role user") {
		t.Fatalf("error event = %#v, want non-user role error", errorEvent)
	}
	assertNoTurnResult(t, results)
	if len(provider.requests) != 0 {
		t.Fatalf("provider requests = %d, want 0 (drain before first request)", len(provider.requests))
	}
	if len(publisher.events) != 0 {
		t.Fatalf("publisher events = %#v, want none before role validation", publisher.events)
	}
}

func TestStreamDrainPublishFailureStopsRun(t *testing.T) {
	publishErr := errors.New("persist turn input failed")
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{model.TextDeltaEvent{Text: "should not reach"}},
		},
	}
	executor := &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "tool output"}}
	publisher := &fakePublisher{errKind: eventbus.KindTurnInputReady, err: publishErr}
	drain, _ := drainSequence(
		nil,
		[]model.Message{{Role: model.MessageRoleUser, Content: "queued"}},
	)

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "original"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:          provider,
		ToolExecutor:      executor,
		MaxTurns:          4,
		TurnID:            "turn-1",
		Publisher:         publisher,
		ActivePromptDrain: drain,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if !errors.Is(errorEvent.Err, publishErr) || errorEvent.Message != "persist turn input" {
		t.Fatalf("error event = %#v, want persist turn input failure", errorEvent)
	}
	assertNoTurnResult(t, results)
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1 (no second request after publish failure)", len(provider.requests))
	}
	// AssistantReady and ToolResultReady were published before the failed
	// TurnInputReady; the failed event is also recorded by the fake publisher.
	if got := publisherKinds(publisher); !equalStrings(got, []string{eventbus.KindAssistantReady, eventbus.KindToolResultReady, eventbus.KindTurnInputReady}) {
		t.Fatalf("publisher kinds = %#v, want AssistantReady, ToolResultReady, TurnInputReady", got)
	}
}

func TestStreamDrainNilPreservesExistingBehavior(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{model.TextDeltaEvent{Text: "final"}},
		},
	}
	executor := &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "tool output"}}
	publisher := &fakePublisher{}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
		TurnID:       "turn-1",
		Publisher:    publisher,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "final" {
		t.Fatalf("text events = %q, want final", gotText)
	}
	turnResult := collectTurnResult(t, results)
	if len(turnResult.Messages) != 4 {
		t.Fatalf("result messages = %d, want 4: %#v", len(turnResult.Messages), turnResult.Messages)
	}
	// No drain configured: no TurnInputReady events, original durable order.
	wantKinds := []string{
		eventbus.KindAssistantReady,
		eventbus.KindToolResultReady,
		eventbus.KindAssistantReady,
	}
	if got := publisherKinds(publisher); !equalStrings(got, wantKinds) {
		t.Fatalf("publisher kinds = %#v, want %v (no TurnInputReady)", got, wantKinds)
	}
}

func TestStreamDrainAtMaxTurnsWithQueuedInputStopsExplicitly(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "final"}},
		},
	}
	publisher := &fakePublisher{}
	// Checkpoint 1 nil; checkpoint 3 after the final response returns queued
	// input, but the run is already at the turn limit.
	drain, _ := drainSequence(
		nil,
		[]model.Message{{Role: model.MessageRoleUser, Content: "late"}},
	)

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "original"}},
	}, Options{
		Provider:          provider,
		MaxTurns:          1,
		TurnID:            "turn-1",
		Publisher:         publisher,
		ActivePromptDrain: drain,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}
	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if errorEvent.Err == nil || !strings.Contains(errorEvent.Err.Error(), "max_turns 1") || !strings.Contains(errorEvent.Err.Error(), "queued input") {
		t.Fatalf("error event = %#v, want max_turns with queued input", errorEvent)
	}
	assertNoTurnResult(t, results)
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1 (no continuation past limit)", len(provider.requests))
	}
	// The queued input was still published (not silently dropped) before the
	// max_turns error.
	wantKinds := []string{eventbus.KindAssistantReady, eventbus.KindTurnInputReady}
	if got := publisherKinds(publisher); !equalStrings(got, wantKinds) {
		t.Fatalf("publisher kinds = %#v, want AssistantReady then TurnInputReady", got)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
