package openairesponses

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestBuildRequestBodyMapsMessagesStreamAndParameters(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "gpt-5.1",
		Messages: []model.Message{
			{Role: model.MessageRoleSystem, Content: "Be concise."},
			{Role: model.MessageRoleDeveloper, Content: "Follow project rules."},
			{Role: model.MessageRoleUser, Content: "Hello"},
			{Role: model.MessageRoleAssistant, Content: "Hi."},
		},
		Parameters: map[string]any{
			"temperature": 0.6,
			"max_tokens":  4096,
		},
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "gpt-5.1",
		"input": [
			{"role": "system", "content": "Be concise."},
			{"role": "developer", "content": "Follow project rules."},
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi."}
		],
		"stream": true,
		"temperature": 0.6,
		"max_output_tokens": 4096
	}`)
	assertJSONOmitsKey(t, body, "tools")
	assertJSONOmitsKey(t, body, "max_tokens")
}

func TestBuildRequestBodyPreservesExplicitMaxOutputTokens(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "gpt-5.1",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Hello"},
		},
		Parameters: map[string]any{
			"max_tokens":        4096,
			"max_output_tokens": 1024,
		},
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "gpt-5.1",
		"input": [
			{"role": "user", "content": "Hello"}
		],
		"stream": true,
		"max_output_tokens": 1024
	}`)
	assertJSONOmitsKey(t, body, "max_tokens")
}

func TestBuildRequestBodyRejectsUnsupportedToolParameters(t *testing.T) {
	tests := []struct {
		name      string
		parameter string
		value     any
	}{
		{name: "tools", parameter: "tools", value: []any{map[string]any{"type": "function"}}},
		{name: "tool choice", parameter: "tool_choice", value: "auto"},
		{name: "parallel tool calls", parameter: "parallel_tool_calls", value: true},
		{name: "max tool calls", parameter: "max_tool_calls", value: 1},
		{name: "legacy functions", parameter: "functions", value: []any{map[string]any{"name": "read_file"}}},
		{name: "function call output", parameter: "function_call_output", value: map[string]any{"call_id": "call_1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := BuildRequestBody(model.Request{
				Model: "gpt-5.1",
				Messages: []model.Message{
					{Role: model.MessageRoleUser, Content: "Hello"},
				},
				Parameters: map[string]any{
					tt.parameter:        tt.value,
					"max_output_tokens": 1024,
				},
			}, true)
			if err == nil {
				t.Fatalf("BuildRequestBody() error = nil, want error; body = %s", body)
			}

			want := `OpenAI Responses adapter does not support tools yet: parameter "` + tt.parameter + `" is not supported`
			if err.Error() != want {
				t.Fatalf("BuildRequestBody() error = %q, want %q", err, want)
			}
		})
	}
}

func TestBuildRequestBodyRejectsUnsupportedToolPayloads(t *testing.T) {
	tests := []struct {
		name    string
		request model.Request
		want    string
	}{
		{
			name: "request tools",
			request: model.Request{
				Model: "gpt-5.1",
				Tools: []model.Tool{{Name: "read_file"}},
			},
			want: "does not support tools yet",
		},
		{
			name: "assistant tool calls",
			request: model.Request{
				Model: "gpt-5.1",
				Messages: []model.Message{
					{
						Role:      model.MessageRoleAssistant,
						ToolCalls: []model.ToolCall{{ID: "call_1", Name: "read_file", Arguments: "{}"}},
					},
				},
			},
			want: "does not support assistant tool calls yet",
		},
		{
			name: "tool messages",
			request: model.Request{
				Model: "gpt-5.1",
				Messages: []model.Message{
					{Role: model.MessageRoleTool, ToolCallID: "call_1", Content: "tool output"},
				},
			},
			want: "does not support tool messages yet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := BuildRequestBody(tt.request, true)
			if err == nil {
				t.Fatalf("BuildRequestBody() error = nil, want error; body = %s", body)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("BuildRequestBody() error = %q, want contain %q", err, tt.want)
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

func assertJSONOmitsKey(t *testing.T, data []byte, key string) {
	t.Helper()

	value := decodeJSON(t, data)
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("decoded JSON is %T, want object", value)
	}
	if _, ok := object[key]; ok {
		t.Fatalf("JSON contains unexpected key %q: %s", key, data)
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
