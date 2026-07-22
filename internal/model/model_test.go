package model

import "testing"

func TestEventTypeNames(t *testing.T) {
	tests := []struct {
		eventType EventType
		want      string
	}{
		{EventTypeAgentIterationStarted, "agent_iteration_started"},
		{EventTypeTextDelta, "text_delta"},
		{EventTypeReasoningDelta, "reasoning_delta"},
		{EventTypeMessageDone, "message_done"},
		{EventTypeToolCallDelta, "tool_call_delta"},
		{EventTypeToolCallDone, "tool_call_done"},
		{EventTypeToolStarted, "tool_started"},
		{EventTypeToolResult, "tool_result"},
		{EventTypeUsage, "usage"},
		{EventTypeSubagentCompletion, "subagent_completion"},
		{EventTypeError, "error"},
	}

	for _, tt := range tests {
		if got := string(tt.eventType); got != tt.want {
			t.Fatalf("event type = %q, want %q", got, tt.want)
		}
	}
}

func TestEventsReportTheirTypes(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  EventType
	}{
		{"agent iteration started", AgentIterationStartedEvent{}, EventTypeAgentIterationStarted},
		{"text delta", TextDeltaEvent{}, EventTypeTextDelta},
		{"reasoning delta", ReasoningDeltaEvent{}, EventTypeReasoningDelta},
		{"message done", MessageDoneEvent{}, EventTypeMessageDone},
		{"tool call delta", ToolCallDeltaEvent{}, EventTypeToolCallDelta},
		{"tool call done", ToolCallDoneEvent{}, EventTypeToolCallDone},
		{"tool started", ToolStartedEvent{}, EventTypeToolStarted},
		{"tool result", ToolResultEvent{}, EventTypeToolResult},
		{"usage", UsageEvent{}, EventTypeUsage},
		{"subagent completion", SubagentCompletionEvent{}, EventTypeSubagentCompletion},
		{"error", ErrorEvent{}, EventTypeError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.event.Type(); got != tt.want {
				t.Fatalf("Type() = %q, want %q", got, tt.want)
			}
		})
	}
}
