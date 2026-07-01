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
