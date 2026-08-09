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

func TestBuildRequestBodyMapsConfiguredDeveloperRole(t *testing.T) {
	body, err := BuildRequestBody(model.Request{
		Model: "kimi-k3",
		Messages: []model.Message{
			{Role: model.MessageRoleSystem, Content: "You are helpful."},
			{Role: model.MessageRoleDeveloper, Content: "Follow project rules."},
			{Role: model.MessageRoleUser, Content: "Hello"},
		},
		DeveloperRole: model.MessageRoleSystem,
	}, true)
	if err != nil {
		t.Fatalf("BuildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "kimi-k3",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "system", "content": "Follow project rules."},
			{"role": "user", "content": "Hello"}
		],
		"stream": true
	}`)
}

func TestBuildRequestBodyRejectsUnsupportedDeveloperRoleMapping(t *testing.T) {
	_, err := BuildRequestBody(model.Request{
		Model:         "model-default",
		Messages:      []model.Message{{Role: model.MessageRoleDeveloper, Content: "rules"}},
		DeveloperRole: model.MessageRoleUser,
	}, true)
	if err == nil {
		t.Fatal("BuildRequestBody() error = nil, want unsupported developer role mapping error")
	}
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

func TestOpenAIChatToolNameMapperRoundTripsCanonicalWebEval(t *testing.T) {
	request := model.Request{
		Model: "chat-model",
		Messages: []model.Message{{
			Role:      model.MessageRoleAssistant,
			ToolCalls: []model.ToolCall{{ID: "call-1", Name: "web.eval", Arguments: `{"code":"1"}`}},
		}},
		Tools: []model.Tool{{Name: "web.eval", Description: "debug", InputSchema: map[string]any{"type": "object"}}},
	}
	body, err := BuildRequestBody(request, true)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	tools := decoded["tools"].([]any)
	toolName := tools[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	if toolName == "web.eval" || toolName == "" || !isValidOpenAIChatToolName(toolName) {
		t.Fatalf("provider tool name = %q, want legal non-canonical alias", toolName)
	}

	mapper := newToolNameMapper(request.Tools)
	if got := mapper.internalName(mapper.providerName("web.eval")); got != "web.eval" {
		t.Fatalf("tool name round trip = %q, want web.eval", got)
	}
	messages := decoded["messages"].([]any)
	assistant := messages[0].(map[string]any)
	callName := assistant["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"]
	if callName != toolName {
		t.Fatalf("historical tool call name = %v, want request alias %q", callName, toolName)
	}
	compatibility, err := resolveCompatibility("")
	if err != nil {
		t.Fatal(err)
	}
	decoder := newStreamEventDecoderWithCompatibility(compatibility, mapper)
	chunk := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"` + toolName + `","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	events, err := decoder.eventsFromChunk(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("mapped stream events = %#v", events)
	}
	call, ok := events[len(events)-1].(model.ToolCallDoneEvent)
	if !ok || call.ToolCall.Name != "web.eval" {
		t.Fatalf("mapped stream tool call = %#v, want canonical web.eval", events[len(events)-1])
	}
}

func TestOpenAIChatToolNameMapperPreservesLegalNamesAndAvoidsAliasCollisions(t *testing.T) {
	legal := newToolNameMapper([]model.Tool{{Name: "read_file"}})
	if got := legal.providerName("read_file"); got != "read_file" || legal.internalName(got) != "read_file" {
		t.Fatalf("legal tool round trip = %q -> %q, want read_file", got, legal.internalName(got))
	}

	mapper := newToolNameMapper([]model.Tool{{Name: "web.eval"}, {Name: "tool_0"}})
	alias := mapper.providerName("web.eval")
	if alias == "web.eval" || alias == "tool_0" || !isValidOpenAIChatToolName(alias) {
		t.Fatalf("web.eval alias = %q, want deterministic legal non-conflicting alias", alias)
	}
	if mapper.providerName("web.eval") != alias || mapper.internalName(alias) != "web.eval" || mapper.internalName(mapper.providerName("tool_0")) != "tool_0" {
		t.Fatalf("alias mapping did not round trip: alias=%q map=%#v", alias, mapper.toInternal)
	}
	other := newToolNameMapper([]model.Tool{{Name: "web.eval"}, {Name: "tool_0"}})
	if other.providerName("web.eval") != alias {
		t.Fatalf("alias was not deterministic: first=%q second=%q", alias, other.providerName("web.eval"))
	}
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

func TestBuildRequestBodyAppliesKimiCompatibility(t *testing.T) {
	compatibility, err := resolveCompatibility(CompatibilityKimi)
	if err != nil {
		t.Fatalf("resolveCompatibility() error = %v", err)
	}
	body, err := buildRequestBody(model.Request{
		Model:     "kimi-k3",
		SessionID: "session-123",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Inspect the repository"},
			{
				Role:             model.MessageRoleAssistant,
				ReasoningContent: "I should inspect the files first.",
				ToolCalls: []model.ToolCall{
					{ID: "call_1", Name: "list_files", Arguments: `{}`},
				},
			},
			{Role: model.MessageRoleTool, Content: "README.md", ToolCallID: "call_1"},
		},
		Parameters: map[string]any{
			"stream_options": map[string]any{"custom": true},
		},
	}, true, compatibility)
	if err != nil {
		t.Fatalf("buildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "kimi-k3",
		"messages": [
			{"role": "user", "content": "Inspect the repository"},
			{
				"role": "assistant",
				"content": "",
				"reasoning_content": "I should inspect the files first.",
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "list_files", "arguments": "{}"}
				}]
			},
			{"role": "tool", "content": "README.md", "tool_call_id": "call_1"}
		],
		"stream": true,
		"prompt_cache_key": "session-123",
		"stream_options": {"custom": true, "include_usage": true}
	}`)
}

func TestBuildRequestBodyPreservesExplicitKimiCacheAndUsageSettings(t *testing.T) {
	compatibility, err := resolveCompatibility(CompatibilityKimi)
	if err != nil {
		t.Fatalf("resolveCompatibility() error = %v", err)
	}
	body, err := buildRequestBody(model.Request{
		Model:     "kimi-k3",
		SessionID: "session-123",
		Messages:  []model.Message{{Role: model.MessageRoleUser, Content: "Hello"}},
		Parameters: map[string]any{
			"prompt_cache_key": "configured-key",
			"stream_options":   map[string]any{"include_usage": false},
		},
	}, true, compatibility)
	if err != nil {
		t.Fatalf("buildRequestBody() error = %v", err)
	}

	assertJSONEqual(t, body, `{
		"model": "kimi-k3",
		"messages": [{"role": "user", "content": "Hello"}],
		"stream": true,
		"prompt_cache_key": "configured-key",
		"stream_options": {"include_usage": false}
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
