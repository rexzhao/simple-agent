package openaichat

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestBuildRequestBodyMapsMessagesStreamAndParameters(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "glm-5.2",
		Messages: []model.Message{
			{Role: model.MessageRoleSystem, Content: "Be concise."},
			{Role: model.MessageRoleUser, Content: "Hello"},
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
		"model": "glm-5.2",
		"messages": [
			{"role": "system", "content": "Be concise."},
			{"role": "user", "content": "Hello"}
		],
		"stream": true,
		"temperature": 0.6,
		"max_tokens": 4096
	}`)
	assertJSONOmitsKey(t, body, "tools")
}

func TestBuildRequestBodyMapsToolsToOpenAIFunctionShape(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "glm-5.2",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Read a file"},
		},
		Tools: []model.Tool{
			{
				Name:        "read_file",
				Description: "Read a file from the workspace.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []any{"path"},
				},
			},
		},
	}, false)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "glm-5.2",
		"messages": [
			{"role": "user", "content": "Read a file"}
		],
		"stream": false,
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "read_file",
					"description": "Read a file from the workspace.",
					"parameters": {
						"type": "object",
						"properties": {
							"path": {"type": "string"}
						},
						"required": ["path"]
					}
				}
			}
		]
	}`)
}

func TestBuildRequestBodyMapsNilToolSchemaAndPreservesToolOrder(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "glm-5.2",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Use tools"},
		},
		Tools: []model.Tool{
			{
				Name:        "list_files",
				Description: "List files in the workspace.",
			},
			{
				Name:        "read_file",
				Description: "Read a file from the workspace.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
				},
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "glm-5.2",
		"messages": [
			{"role": "user", "content": "Use tools"}
		],
		"stream": true,
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "list_files",
					"description": "List files in the workspace.",
					"parameters": {}
				}
			},
			{
				"type": "function",
				"function": {
					"name": "read_file",
					"description": "Read a file from the workspace.",
					"parameters": {
						"type": "object",
						"properties": {
							"path": {"type": "string"}
						}
					}
				}
			}
		]
	}`)
}

func TestBuildRequestBodyMapsAssistantToolCallsAndToolMessages(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "glm-5.2",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Read a file"},
			{
				Role:    model.MessageRoleAssistant,
				Content: "",
				ToolCalls: []model.ToolCall{
					{ID: "call_1", Name: "read_file", Arguments: `{"path":"README.md"}`},
				},
			},
			{Role: model.MessageRoleTool, Content: "file body", ToolCallID: "call_1"},
		},
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "glm-5.2",
		"messages": [
			{"role": "user", "content": "Read a file"},
			{
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "read_file",
							"arguments": "{\"path\":\"README.md\"}"
						}
					}
				]
			},
			{"role": "tool", "content": "file body", "tool_call_id": "call_1"}
		],
		"stream": true
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

func TestBuildRequestBodyMapsUserImageContentBlocks(t *testing.T) {
	imageURL := model.ImageDataURL("image/png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	body, err := BuildRequestBody(model.Request{
		Model: "gpt-5.4",
		Messages: []model.Message{{
			Role: model.MessageRoleUser,
			ContentBlocks: []model.InputContentBlock{
				{Type: "input_text", Text: "What is shown?"},
				{Type: "input_image", ImageURL: imageURL, Detail: "high"},
			},
		}},
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.4",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "What is shown?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgo=", "detail": "high"}}
			]
		}],
		"stream": true
	}`)
}

func TestBuildRequestBodyRejectsNonUserContentBlocks(t *testing.T) {
	_, err := BuildRequestBody(model.Request{
		Model: "gpt-5.4",
		Messages: []model.Message{{
			Role:          model.MessageRoleAssistant,
			ContentBlocks: []model.InputContentBlock{{Type: "input_text", Text: "not allowed"}},
		}},
	}, true)
	if err == nil {
		t.Fatal("BuildRequestBody() error = nil, want non-user content block error")
	}
}
