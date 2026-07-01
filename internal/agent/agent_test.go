package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

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
}

func (e *fakeToolExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	e.called = true
	e.name = name
	e.arguments = arguments
	return e.result, e.err
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
		if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != toolCallID {
			t.Fatalf("assistant tool calls = %#v, want id %q", message.ToolCalls, toolCallID)
		}
	case model.MessageRoleTool:
		if message.ToolCallID != toolCallID {
			t.Fatalf("tool_call_id = %q, want %q", message.ToolCallID, toolCallID)
		}
	}
}
