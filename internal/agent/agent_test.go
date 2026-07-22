package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
)

func TestStreamEmitsToolRequestedStartedFinishedInOrder(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "final"},
			},
		},
	}
	executor := &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "tool output"}}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	var lifecycle []string
	var iterations []int
	for _, event := range gotEvents {
		switch event := event.(type) {
		case model.AgentIterationStartedEvent:
			iterations = append(iterations, event.Iteration)
		case model.ToolCallDoneEvent:
			lifecycle = append(lifecycle, "requested")
		case model.ToolStartedEvent:
			lifecycle = append(lifecycle, "started")
		case model.ToolResultEvent:
			lifecycle = append(lifecycle, "finished")
		}
	}
	if want := []string{"requested", "started", "finished"}; !reflect.DeepEqual(lifecycle, want) {
		t.Fatalf("tool lifecycle = %#v, want %#v", lifecycle, want)
	}
	if want := []int{1, 2}; !reflect.DeepEqual(iterations, want) {
		t.Fatalf("agent iterations = %#v, want %#v", iterations, want)
	}
}

func TestStreamOmitsToolStartedForValidationFailures(t *testing.T) {
	tests := []struct {
		name    string
		tools   []model.Tool
		args    string
		wantMsg string
	}{
		{
			name:    "invalid arguments",
			tools:   []model.Tool{{Name: "echo"}},
			args:    `{"text":`,
			wantMsg: "invalid tool arguments",
		},
		{
			name:    "disabled tool",
			tools:   nil,
			args:    `{}`,
			wantMsg: "is not enabled",
		},
		{
			name:    "missing executor",
			tools:   []model.Tool{{Name: "echo"}},
			args:    `{}`,
			wantMsg: "tool executor is not configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &fakeProvider{
				turns: [][]model.Event{
					{
						model.ToolCallDoneEvent{
							ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: tt.args},
						},
					},
					{
						model.TextDeltaEvent{Text: "recovered"},
					},
				},
			}
			var executor ToolExecutor
			if tt.name != "missing executor" {
				executor = &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "should not run"}}
			}

			events, err := Stream(context.Background(), model.Request{
				Model:    "model-test",
				Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
				Tools:    tt.tools,
			}, Options{
				Provider:     provider,
				ToolExecutor: executor,
				MaxTurns:     4,
			})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}

			gotEvents := collectAgentEvents(t, events)
			if hasToolStartedEvent(gotEvents) {
				t.Fatalf("events = %#v, want no ToolStartedEvent for validation failure", gotEvents)
			}
			result := firstToolResult(t, gotEvents)
			if !result.IsError || !strings.Contains(result.Content, tt.wantMsg) {
				t.Fatalf("tool result = %#v, want error containing %q", result, tt.wantMsg)
			}
			if fake, ok := executor.(*fakeToolExecutor); ok && fake.called {
				t.Fatal("executor was called for validation failure")
			}
		})
	}
}

func TestStreamExecutesToolResultAndContinuesUntilFinalText(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"text":"hello"}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "final"},
			},
		},
	}
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "tool output"},
	}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "final" {
		t.Fatalf("text events = %q, want final", gotText)
	}
	result := firstToolResult(t, gotEvents)
	if result.ToolCallID != "call_1" || result.Name != "echo" || result.Content != "tool output" || result.IsError {
		t.Fatalf("tool result = %#v, want successful echo result", result)
	}
	if executor.name != "echo" {
		t.Fatalf("executor name = %q, want echo", executor.name)
	}
	if !reflect.DeepEqual(executor.arguments, map[string]any{"text": "hello"}) {
		t.Fatalf("executor arguments = %#v, want text argument", executor.arguments)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("len(requests) = %d, want 2", len(provider.requests))
	}

	secondMessages := provider.requests[1].Messages
	if len(secondMessages) != 3 {
		t.Fatalf("len(second request messages) = %d, want 3: %#v", len(secondMessages), secondMessages)
	}
	assertAgentMessage(t, secondMessages[1], model.MessageRoleAssistant, "", "call_1")
	assertAgentMessage(t, secondMessages[2], model.MessageRoleTool, "tool output", "call_1")
}

func TestStreamWithResultAppendsFinalAssistantMessage(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.TextDeltaEvent{Text: "hello "},
				model.TextDeltaEvent{Text: "there"},
			},
		},
	}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Say hi"}},
	}, Options{
		Provider: provider,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "hello there" {
		t.Fatalf("text events = %q, want hello there", gotText)
	}
	result := collectTurnResult(t, results)
	if len(result.Messages) != 2 {
		t.Fatalf("len(result messages) = %d, want 2: %#v", len(result.Messages), result.Messages)
	}
	assertAgentMessage(t, result.Messages[0], model.MessageRoleUser, "Say hi", "")
	assertAgentMessage(t, result.Messages[1], model.MessageRoleAssistant, "hello there", "")
}

func TestStreamWithResultIncludesToolHistoryAndFinalAssistantText(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"text":"hello"}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "final"},
			},
		},
	}
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "tool output"},
	}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	if gotText := collectText(gotEvents); gotText != "final" {
		t.Fatalf("text events = %q, want final", gotText)
	}
	result := collectTurnResult(t, results)
	if len(result.Messages) != 4 {
		t.Fatalf("len(result messages) = %d, want 4: %#v", len(result.Messages), result.Messages)
	}
	assertAgentMessage(t, result.Messages[0], model.MessageRoleUser, "Use a tool", "")
	assertAgentMessage(t, result.Messages[1], model.MessageRoleAssistant, "", "call_1")
	assertAgentMessage(t, result.Messages[2], model.MessageRoleTool, "tool output", "call_1")
	assertAgentMessage(t, result.Messages[3], model.MessageRoleAssistant, "final", "")
}

func TestStreamWithResultFailsWhenFinalResponseHasNoVisibleOutput(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{
				model.ReasoningDeltaEvent{Text: "thinking only"},
			},
		},
	}
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "tool output"},
	}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if errorEvent.Err == nil || !strings.Contains(errorEvent.Err.Error(), "empty final response") {
		t.Fatalf("error event = %#v, want empty final response", errorEvent)
	}
	if _, ok := <-results; ok {
		t.Fatal("results produced for empty final response")
	}
	if len(provider.requests) != 2 {
		t.Fatalf("len(requests) = %d, want 2", len(provider.requests))
	}
}

func TestStreamWithPublisherPublishesDurableEventsInOrder(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"text":"hello"}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "final"},
			},
		},
	}
	publisher := &fakePublisher{}
	var executeCheckErr error
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "tool output"},
		onExecute: func() {
			if len(publisher.events) != 1 {
				executeCheckErr = fmt.Errorf("publisher events before tool execution = %d, want 1", len(publisher.events))
				return
			}
			if _, ok := publisher.events[0].(eventbus.AssistantReady); !ok {
				executeCheckErr = fmt.Errorf("first publisher event = %T, want AssistantReady", publisher.events[0])
			}
		},
	}

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
	if executeCheckErr != nil {
		t.Fatal(executeCheckErr)
	}
	if gotText := collectText(gotEvents); gotText != "final" {
		t.Fatalf("text events = %q, want final", gotText)
	}
	if result := firstToolResult(t, gotEvents); result.ToolCallID != "call_1" || result.Content != "tool output" {
		t.Fatalf("tool result event = %#v, want call_1/tool output", result)
	}
	turnResult := collectTurnResult(t, results)
	if len(turnResult.Messages) != 4 {
		t.Fatalf("len(result messages) = %d, want 4", len(turnResult.Messages))
	}

	if len(publisher.events) != 3 {
		t.Fatalf("publisher events = %#v, want 3 events", publisher.events)
	}
	firstAssistant, ok := publisher.events[0].(eventbus.AssistantReady)
	if !ok {
		t.Fatalf("publisher event[0] = %T, want AssistantReady", publisher.events[0])
	}
	if firstAssistant.TurnID != "turn-1" || firstAssistant.Message.Role != model.MessageRoleAssistant || len(firstAssistant.Message.ToolCalls) != 1 || firstAssistant.Message.ToolCalls[0].ID != "call_1" {
		t.Fatalf("first AssistantReady = %#v, want turn-1 assistant call_1", firstAssistant)
	}
	toolResult, ok := publisher.events[1].(eventbus.ToolResultReady)
	if !ok {
		t.Fatalf("publisher event[1] = %T, want ToolResultReady", publisher.events[1])
	}
	if toolResult.TurnID != "turn-1" || toolResult.Result.ToolCallID != "call_1" || toolResult.Result.Content != "tool output" {
		t.Fatalf("ToolResultReady = %#v, want call_1/tool output", toolResult)
	}
	finalAssistant, ok := publisher.events[2].(eventbus.AssistantReady)
	if !ok {
		t.Fatalf("publisher event[2] = %T, want AssistantReady", publisher.events[2])
	}
	if finalAssistant.TurnID != "turn-1" || finalAssistant.Message.Content != "final" || len(finalAssistant.Message.ToolCalls) != 0 {
		t.Fatalf("final AssistantReady = %#v, want final no-tool assistant", finalAssistant)
	}
}

func TestStreamWithPublisherAssistantReadyFailureAborts(t *testing.T) {
	publishErr := errors.New("persist assistant failed")

	t.Run("final response", func(t *testing.T) {
		provider := &fakeProvider{
			turns: [][]model.Event{
				{model.TextDeltaEvent{Text: "final"}},
			},
		}
		publisher := &fakePublisher{errKind: eventbus.KindAssistantReady, err: publishErr}

		events, results, err := StreamWithResult(context.Background(), model.Request{
			Model:    "model-test",
			Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Say hi"}},
		}, Options{
			Provider:  provider,
			TurnID:    "turn-1",
			Publisher: publisher,
		})
		if err != nil {
			t.Fatalf("StreamWithResult() error = %v", err)
		}

		gotEvents := collectAgentEvents(t, events)
		errorEvent := firstErrorEvent(t, gotEvents)
		if !errors.Is(errorEvent.Err, publishErr) || errorEvent.Message != "persist assistant" {
			t.Fatalf("error event = %#v, want persist assistant error", errorEvent)
		}
		assertNoTurnResult(t, results)
		if len(provider.requests) != 1 {
			t.Fatalf("len(provider.requests) = %d, want 1", len(provider.requests))
		}
	})

	t.Run("tool round", func(t *testing.T) {
		provider := &fakeProvider{
			turns: [][]model.Event{
				{
					model.ToolCallDoneEvent{
						ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
					},
				},
			},
		}
		executor := &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "tool output"}}
		publisher := &fakePublisher{errKind: eventbus.KindAssistantReady, err: publishErr}

		events, results, err := StreamWithResult(context.Background(), model.Request{
			Model:    "model-test",
			Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
			Tools:    []model.Tool{{Name: "echo"}},
		}, Options{
			Provider:     provider,
			ToolExecutor: executor,
			TurnID:       "turn-1",
			Publisher:    publisher,
		})
		if err != nil {
			t.Fatalf("StreamWithResult() error = %v", err)
		}

		gotEvents := collectAgentEvents(t, events)
		errorEvent := firstErrorEvent(t, gotEvents)
		if !errors.Is(errorEvent.Err, publishErr) || errorEvent.Message != "persist assistant" {
			t.Fatalf("error event = %#v, want persist assistant error", errorEvent)
		}
		if executor.called {
			t.Fatal("executor ran after AssistantReady publish failed")
		}
		if hasToolResultEvent(gotEvents) {
			t.Fatalf("events = %#v, want no ToolResultEvent", gotEvents)
		}
		assertNoTurnResult(t, results)
		if len(provider.requests) != 1 {
			t.Fatalf("len(provider.requests) = %d, want 1", len(provider.requests))
		}
	})
}

func TestStreamWithPublisherToolResultReadyFailureAborts(t *testing.T) {
	publishErr := errors.New("persist tool result failed")
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "should not request"},
			},
		},
	}
	executor := &fakeToolExecutor{result: model.ToolResult{Name: "echo", Content: "tool output"}}
	publisher := &fakePublisher{errKind: eventbus.KindToolResultReady, err: publishErr}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		TurnID:       "turn-1",
		Publisher:    publisher,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if !errors.Is(errorEvent.Err, publishErr) || errorEvent.Message != "persist tool result" {
		t.Fatalf("error event = %#v, want persist tool result error", errorEvent)
	}
	if !executor.called {
		t.Fatal("executor was not called before ToolResultReady publish")
	}
	if hasToolResultEvent(gotEvents) {
		t.Fatalf("events = %#v, want no ToolResultEvent", gotEvents)
	}
	assertNoTurnResult(t, results)
	if len(provider.requests) != 1 {
		t.Fatalf("len(provider.requests) = %d, want no second model request", len(provider.requests))
	}
	if len(publisher.events) != 2 {
		t.Fatalf("publisher events = %#v, want AssistantReady and ToolResultReady", publisher.events)
	}
}

func TestStreamWithPublisherRequiresTurnID(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "unused"}},
		},
	}
	publisher := &fakePublisher{}

	events, results, err := StreamWithResult(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Say hi"}},
	}, Options{
		Provider:  provider,
		Publisher: publisher,
	})
	if err != nil {
		t.Fatalf("StreamWithResult() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if errorEvent.Message != "persist turn" || !strings.Contains(errorEvent.Err.Error(), "turn id is required") {
		t.Fatalf("error event = %#v, want missing turn id error", errorEvent)
	}
	assertNoTurnResult(t, results)
	if len(provider.requests) != 0 {
		t.Fatalf("len(provider.requests) = %d, want 0", len(provider.requests))
	}
	if len(publisher.events) != 0 {
		t.Fatalf("publisher events = %#v, want none", publisher.events)
	}
}

func TestStreamMalformedToolArgumentsAppendsToolErrorAndContinues(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"text":`},
				},
			},
			{
				model.TextDeltaEvent{Text: "recovered"},
			},
		},
	}
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "should not run"},
	}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	result := firstToolResult(t, gotEvents)
	if !result.IsError {
		t.Fatalf("tool result IsError = false, want true: %#v", result)
	}
	if !strings.Contains(result.Content, "invalid tool arguments") {
		t.Fatalf("tool result content = %q, want invalid arguments message", result.Content)
	}
	if executor.called {
		t.Fatal("executor was called for malformed arguments")
	}
	if gotText := collectText(gotEvents); gotText != "recovered" {
		t.Fatalf("text events = %q, want recovered", gotText)
	}

	secondMessages := provider.requests[1].Messages
	assertAgentMessage(t, secondMessages[2], model.MessageRoleTool, result.Content, "call_1")
	if !secondMessages[2].IsError {
		t.Fatalf("tool result message IsError = false, want true")
	}
}

func TestStreamStopsWithClearErrorAtMaxTurns(t *testing.T) {
	provider := &fakeProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{
				model.TextDeltaEvent{Text: "unexpected"},
			},
		},
	}
	executor := &fakeToolExecutor{
		result: model.ToolResult{Name: "echo", Content: "tool output"},
	}

	events, err := Stream(context.Background(), model.Request{
		Model:    "model-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "Use a tool"}},
		Tools:    []model.Tool{{Name: "echo"}},
	}, Options{
		Provider:     provider,
		ToolExecutor: executor,
		MaxTurns:     1,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	gotEvents := collectAgentEvents(t, events)
	errorEvent := firstErrorEvent(t, gotEvents)
	if errorEvent.Err == nil || !strings.Contains(errorEvent.Err.Error(), "max_turns 1") {
		t.Fatalf("error event = %#v, want max_turns error", errorEvent)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(provider.requests))
	}
	if gotText := collectText(gotEvents); gotText != "" {
		t.Fatalf("text events = %q, want empty", gotText)
	}
}

type fakeProvider struct {
	turns    [][]model.Event
	requests []model.Request
}

func (p *fakeProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if len(p.requests) >= len(p.turns) {
		return nil, fmt.Errorf("unexpected model request %d", len(p.requests)+1)
	}

	turn := len(p.requests)
	p.requests = append(p.requests, copyAgentRequest(request))

	events := make(chan model.Event, len(p.turns[turn]))
	for _, event := range p.turns[turn] {
		events <- event
	}
	close(events)
	return events, nil
}

type fakeToolExecutor struct {
	called    bool
	name      string
	arguments map[string]any
	result    model.ToolResult
	err       error
	onExecute func()
}

func (e *fakeToolExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	e.called = true
	e.name = name
	e.arguments = arguments
	if e.onExecute != nil {
		e.onExecute()
	}
	return e.result, e.err
}

type fakePublisher struct {
	events  []eventbus.Event
	errKind string
	err     error
}

func (p *fakePublisher) Publish(event eventbus.Event) error {
	p.events = append(p.events, event)
	if p.errKind != "" && event.Kind() == p.errKind {
		return p.err
	}
	return nil
}

func copyAgentRequest(request model.Request) model.Request {
	copied := request
	copied.Messages = append([]model.Message(nil), request.Messages...)
	for i := range copied.Messages {
		copied.Messages[i].ToolCalls = append([]model.ToolCall(nil), request.Messages[i].ToolCalls...)
	}
	copied.Tools = append([]model.Tool(nil), request.Tools...)
	if request.Parameters != nil {
		copied.Parameters = make(map[string]any, len(request.Parameters))
		for key, value := range request.Parameters {
			copied.Parameters[key] = value
		}
	}
	return copied
}

func collectAgentEvents(t *testing.T, events <-chan model.Event) []model.Event {
	t.Helper()

	var got []model.Event
	timeout := time.After(time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return got
			}
			got = append(got, event)
		case <-timeout:
			t.Fatal("timed out waiting for agent events")
		}
	}
}

func collectText(events []model.Event) string {
	var text strings.Builder
	for _, event := range events {
		if event, ok := event.(model.TextDeltaEvent); ok {
			text.WriteString(event.Text)
		}
	}
	return text.String()
}

func firstToolResult(t *testing.T, events []model.Event) model.ToolResult {
	t.Helper()

	for _, event := range events {
		if event, ok := event.(model.ToolResultEvent); ok {
			return event.Result
		}
	}
	t.Fatal("missing ToolResultEvent")
	return model.ToolResult{}
}

func firstErrorEvent(t *testing.T, events []model.Event) model.ErrorEvent {
	t.Helper()

	for _, event := range events {
		if event, ok := event.(model.ErrorEvent); ok {
			return event
		}
	}
	t.Fatal("missing ErrorEvent")
	return model.ErrorEvent{}
}

func hasToolResultEvent(events []model.Event) bool {
	for _, event := range events {
		if _, ok := event.(model.ToolResultEvent); ok {
			return true
		}
	}
	return false
}

func hasToolStartedEvent(events []model.Event) bool {
	for _, event := range events {
		if _, ok := event.(model.ToolStartedEvent); ok {
			return true
		}
	}
	return false
}

func collectTurnResult(t *testing.T, results <-chan TurnResult) TurnResult {
	t.Helper()

	select {
	case result, ok := <-results:
		if !ok {
			t.Fatal("result channel closed without TurnResult")
		}
		select {
		case extra, ok := <-results:
			if ok {
				t.Fatalf("unexpected extra TurnResult: %#v", extra)
			}
		default:
		}
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for TurnResult")
	}
	return TurnResult{}
}

func assertNoTurnResult(t *testing.T, results <-chan TurnResult) {
	t.Helper()

	select {
	case result, ok := <-results:
		if ok {
			t.Fatalf("unexpected TurnResult: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for result channel to close")
	}
}

func assertAgentMessage(t *testing.T, message model.Message, role model.MessageRole, content string, toolCallID string) {
	t.Helper()

	if message.Role != role {
		t.Fatalf("message role = %q, want %q", message.Role, role)
	}
	if message.Content != content {
		t.Fatalf("message content = %q, want %q", message.Content, content)
	}
	switch role {
	case model.MessageRoleAssistant:
		if toolCallID == "" {
			if len(message.ToolCalls) != 0 {
				t.Fatalf("assistant tool calls = %#v, want none", message.ToolCalls)
			}
			return
		}
		if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != toolCallID {
			t.Fatalf("assistant tool calls = %#v, want id %q", message.ToolCalls, toolCallID)
		}
	case model.MessageRoleTool:
		if message.ToolCallID != toolCallID {
			t.Fatalf("tool_call_id = %q, want %q", message.ToolCallID, toolCallID)
		}
	}
}
