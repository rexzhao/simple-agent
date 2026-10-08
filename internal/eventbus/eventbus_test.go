package eventbus

import (
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestEventKinds(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{"assistant ready", AssistantReady{TurnID: "turn", Message: model.Message{Role: model.MessageRoleAssistant, Content: "hello"}}, KindAssistantReady},
		{"tool result ready", ToolResultReady{TurnID: "turn", Result: model.ToolResult{ToolCallID: "call", Content: "ok"}}, KindToolResultReady},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.event.Kind(); got != tt.want {
				t.Fatalf("Kind() = %q, want %q", got, tt.want)
			}
		})
	}
}
