package anthropicmessages

import (
	"bytes"
	"encoding/json"
	"reflect"
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

func TestBuildRequestBodyMapsToolsAssistantToolUseAndToolResults(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "claude-sonnet-5",
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
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "claude-sonnet-5",
		"messages": [
			{"role": "user", "content": "Search local files"},
			{
				"role": "assistant",
				"content": [
					{"type": "text", "text": "I'll search."},
					{"type": "tool_use", "id": "call_search", "name": "tool_0", "input": {"query": "needle"}},
					{"type": "tool_use", "id": "call_read", "name": "read_file", "input": {}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type": "tool_result", "tool_use_id": "call_search", "content": "found"},
					{"type": "tool_result", "tool_use_id": "call_read", "content": "missing file", "is_error": true}
				]
			}
		],
		"stream": true,
		"tools": [
			{
				"name": "read_file",
				"description": "Read a file.",
				"input_schema": {
					"type": "object",
					"properties": {
						"path": {"type": "string"}
					}
				}
			},
			{
				"name": "tool_0",
				"description": "Search local data.",
				"input_schema": {
					"type": "object",
					"properties": {}
				}
			}
		]
	}`)
}

func TestBuildRequestBodyAvoidsAnthropicToolNameAliasConflicts(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "claude-sonnet-5",
		Tools: []model.Tool{
			{Name: "tool_0", Description: "Already legal."},
			{Name: "mcp.local.search", Description: "Needs an alias."},
		},
	}, false)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "claude-sonnet-5",
		"messages": [],
		"stream": false,
		"tools": [
			{
				"name": "tool_0",
				"description": "Already legal.",
				"input_schema": {"type": "object", "properties": {}}
			},
			{
				"name": "tool_1",
				"description": "Needs an alias.",
				"input_schema": {"type": "object", "properties": {}}
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
