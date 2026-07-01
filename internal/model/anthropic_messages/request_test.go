package anthropicmessages

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestBuildRequestBodyMapsSystemMessagesAndTextMessages(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "claude-sonnet-5",
		Messages: []model.Message{
			{Role: model.MessageRoleSystem, Content: "Be concise."},
			{Role: model.MessageRoleDeveloper, Content: "Use the project style."},
			{Role: model.MessageRoleDeveloper, Content: "  "},
			{Role: model.MessageRoleUser, Content: "Hello"},
			{Role: model.MessageRoleAssistant, Content: "Hi there"},
		},
		Parameters: map[string]any{
			"temperature": 0.2,
			"max_tokens":  1024,
		},
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "claude-sonnet-5",
		"system": "Be concise.\n\nUse the project style.",
		"messages": [
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi there"}
		],
		"stream": true,
		"temperature": 0.2,
		"max_tokens": 1024
	}`)
}

func TestBuildRequestBodyRejectsToolUseUntilImplemented(t *testing.T) {
	tests := []struct {
		name    string
		request model.Request
		want    string
	}{
		{
			name: "request tools",
			request: model.Request{
				Tools: []model.Tool{{Name: "read_file"}},
			},
			want: "request tools are not supported",
		},
		{
			name: "tool result message",
			request: model.Request{
				Messages: []model.Message{{Role: model.MessageRoleTool, Content: "result", ToolCallID: "call_1"}},
			},
			want: "tool result messages are not supported",
		},
		{
			name: "assistant tool calls",
			request: model.Request{
				Messages: []model.Message{
					{
						Role:      model.MessageRoleAssistant,
						ToolCalls: []model.ToolCall{{ID: "call_1", Name: "read_file"}},
					},
				},
			},
			want: "assistant tool calls are not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := BuildRequestBody(tt.request, true)
			if err == nil {
				t.Fatalf("BuildRequestBody() error = nil, want unsupported tool use error; body = %s", body)
			}
			message := err.Error()
			for _, want := range []string{toolUseNotImplementedMessage, tt.want} {
				if !strings.Contains(message, want) {
					t.Fatalf("error = %q, want contain %q", message, want)
				}
			}
		})
	}
}

func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()

	gotValue := decodeJSON(t, got)
	wantValue := decodeJSON(t, []byte(want))
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON %q: %v", data, err)
	}
	return value
}
