package openairesponses

import (
	"bytes"
	"encoding/json"
	"reflect"
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

func TestBuildRequestBodyPassesThroughStoreAndNestedReasoningParameters(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "gpt-5.5",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Hello"},
		},
		Parameters: map[string]any{
			"store": false,
			"reasoning": map[string]any{
				"effort": "high",
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "gpt-5.5",
		"input": [
			{"role": "user", "content": "Hello"}
		],
		"stream": true,
		"store": false,
		"reasoning": {
			"effort": "high"
		}
	}`)
}

func TestBuildRequestBodyOptionsForceStoreFalseOverridesParameter(t *testing.T) {
	body, _, err := buildRequestBodyWithOptions(model.Request{
		Model: "gpt-5.5",
		Parameters: map[string]any{
			"store": true,
			"reasoning": map[string]any{
				"effort": "high",
			},
		},
	}, true, requestBodyOptions{forceStoreFalse: true})
	if err != nil {
		t.Fatalf("buildRequestBodyWithOptions() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "gpt-5.5",
		"input": [],
		"stream": true,
		"store": false,
		"reasoning": {
			"effort": "high"
		}
	}`)
}

func TestBuildRequestBodyRejectsUnsupportedToolParameters(t *testing.T) {
	tests := []struct {
		name      string
		parameter string
		value     any
	}{
		{name: "tools", parameter: "tools", value: []any{map[string]any{"type": "function"}}},
		{name: "web search options", parameter: "web_search_options", value: map[string]any{"search_context_size": "low"}},
		{name: "tool resources", parameter: "tool_resources", value: map[string]any{}},
		{name: "legacy functions", parameter: "functions", value: []any{map[string]any{"name": "read_file"}}},
		{name: "function call output", parameter: "function_call_output", value: map[string]any{"call_id": "call_1"}},
		{name: "previous response id", parameter: "previous_response_id", value: "resp_1"},
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

			want := `OpenAI Responses adapter does not support parameter "` + tt.parameter + `"`
			if err.Error() != want {
				t.Fatalf("BuildRequestBody() error = %q, want %q", err, want)
			}
		})
	}
}

func TestBuildRequestBodyMapsToolsFunctionCallsToolOutputsAndToolParameters(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "gpt-5.1",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Search local files"},
			{
				Role:    model.MessageRoleAssistant,
				Content: "I'll search.",
				ToolCalls: []model.ToolCall{
					{ID: "call_search", Name: "mcp.local.search", Arguments: `{"query":"needle"}`},
					{ID: "call_read", Name: "read_file"},
				},
			},
			{Role: model.MessageRoleTool, ToolCallID: "call_search", Content: "found"},
			{Role: model.MessageRoleTool, ToolCallID: "call_read", Content: "missing file", IsError: true},
		},
		Tools: []model.Tool{
			{
				Name:        "read_file",
				Description: "Read a file.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
				},
			},
			{
				Name:        "mcp.local.search",
				Description: "Search local data.",
			},
		},
		Parameters: map[string]any{
			"tool_choice":         "auto",
			"parallel_tool_calls": true,
		},
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "gpt-5.1",
		"input": [
			{"role": "user", "content": "Search local files"},
			{"role": "assistant", "content": "I'll search."},
			{"type": "function_call", "call_id": "call_search", "name": "tool_0", "arguments": "{\"query\":\"needle\"}"},
			{"type": "function_call", "call_id": "call_read", "name": "read_file", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_search", "output": "found"},
			{"type": "function_call_output", "call_id": "call_read", "output": "missing file"}
		],
		"stream": true,
		"tool_choice": "auto",
		"parallel_tool_calls": true,
		"tools": [
			{
				"type": "function",
				"name": "read_file",
				"description": "Read a file.",
				"parameters": {
					"type": "object",
					"properties": {
						"path": {"type": "string"}
					}
				}
			},
			{
				"type": "function",
				"name": "tool_0",
				"description": "Search local data.",
				"parameters": {
					"type": "object",
					"properties": {}
				}
			}
		]
	}`)
}

func TestBuildRequestBodyAvoidsResponsesToolNameAliasConflicts(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "gpt-5.1",
		Tools: []model.Tool{
			{Name: "tool_0", Description: "Already legal."},
			{Name: "mcp.local.search", Description: "Needs an alias."},
		},
	}, false)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "gpt-5.1",
		"input": [],
		"stream": false,
		"tools": [
			{
				"type": "function",
				"name": "tool_0",
				"description": "Already legal.",
				"parameters": {"type": "object", "properties": {}}
			},
			{
				"type": "function",
				"name": "tool_1",
				"description": "Needs an alias.",
				"parameters": {"type": "object", "properties": {}}
			}
		]
	}`)
}

func TestBuildRequestBodyMapsForcedToolChoiceAlias(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "gpt-5.1",
		Tools: []model.Tool{{Name: "mcp.local.search", Description: "Search local data."}},
		Parameters: map[string]any{
			"tool_choice": map[string]any{
				"type": "function",
				"name": "mcp.local.search",
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "gpt-5.1",
		"input": [],
		"stream": true,
		"tool_choice": {"type": "function", "name": "tool_0"},
		"tools": [
			{
				"type": "function",
				"name": "tool_0",
				"description": "Search local data.",
				"parameters": {"type": "object", "properties": {}}
			}
		]
	}`)
}

func TestBuildRequestBodyMapsAllowedToolsToolChoiceAliases(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "gpt-5.1",
		Tools: []model.Tool{{Name: "mcp.local.search", Description: "Search local data."}},
		Parameters: map[string]any{
			"tool_choice": map[string]any{
				"type": "allowed_tools",
				"mode": "auto",
				"tools": []any{
					map[string]any{
						"type": "function",
						"name": "mcp.local.search",
					},
				},
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "gpt-5.1",
		"input": [],
		"stream": true,
		"tool_choice": {
			"type": "allowed_tools",
			"mode": "auto",
			"tools": [
				{"type": "function", "name": "tool_0"}
			]
		},
		"tools": [
			{
				"type": "function",
				"name": "tool_0",
				"description": "Search local data.",
				"parameters": {"type": "object", "properties": {}}
			}
		]
	}`)
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
