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
		"store": false,
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
		"store": false,
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
		{name: "context management", parameter: "context_management", value: []any{map[string]any{"type": "compaction"}}},
		{name: "web search options", parameter: "web_search_options", value: map[string]any{"search_context_size": "low"}},
		{name: "tool resources", parameter: "tool_resources", value: map[string]any{}},
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
		"store": false,
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
		"store": false,
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
		"store": false,
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
		"store": false,
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

func TestBuildProviderRequestUsesSessionCacheKeyAndClampsMinimumOutputTokens(t *testing.T) {
	sessionID := strings.Repeat("会", 70)
	body, _, metadata, err := buildProviderRequest(model.Request{
		Model:     "gpt-5.5",
		SessionID: sessionID,
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Hello"},
		},
		Parameters: map[string]any{"max_output_tokens": 1},
	}, true, requestBodyOptions{origin: "https://api.openai.com/v1"})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}

	wantKey := strings.Repeat("会", 64)
	if metadata.CacheKey != wantKey || metadata.SessionAffinity != "auto" {
		t.Fatalf("metadata = %#v, want cache key %q and auto affinity", metadata, wantKey)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.5",
		"input": [{"role": "user", "content": "Hello"}],
		"stream": true,
		"store": false,
		"prompt_cache_key": "`+wantKey+`",
		"max_output_tokens": 16
	}`)
}

func TestBuildProviderRequestMapsLegacyAndModernCacheOptions(t *testing.T) {
	t.Run("legacy retention", func(t *testing.T) {
		body, _, _, err := buildProviderRequest(model.Request{
			Model:     "gpt-5.5",
			SessionID: "session-1",
			Parameters: map[string]any{
				"responses": map[string]any{
					"cache": map[string]any{"retention": "24h"},
				},
			},
		}, true, requestBodyOptions{})
		if err != nil {
			t.Fatalf("buildProviderRequest() error = %v", err)
		}
		assertJSONEqual(t, body, `{
			"model": "gpt-5.5",
			"input": [],
			"stream": true,
			"store": false,
			"prompt_cache_key": "session-1",
			"prompt_cache_retention": "24h"
		}`)
	})

	t.Run("modern explicit instructions", func(t *testing.T) {
		body, _, _, err := buildProviderRequest(model.Request{
			Model:     "gpt-5.6",
			SessionID: "session-2",
			Messages: []model.Message{
				{Role: model.MessageRoleSystem, Content: "Stable instructions"},
				{Role: model.MessageRoleUser, Content: "Hello"},
			},
			Parameters: map[string]any{
				"responses": map[string]any{
					"cache": map[string]any{
						"mode":       "explicit",
						"ttl":        "30m",
						"breakpoint": "instructions",
					},
				},
			},
		}, true, requestBodyOptions{})
		if err != nil {
			t.Fatalf("buildProviderRequest() error = %v", err)
		}
		assertJSONEqual(t, body, `{
			"model": "gpt-5.6",
			"input": [
				{"role": "system", "content": [{
					"type": "input_text",
					"text": "Stable instructions",
					"prompt_cache_breakpoint": {"mode": "explicit"}
				}]},
				{"role": "user", "content": "Hello"}
			],
			"stream": true,
			"store": false,
			"prompt_cache_key": "session-2",
			"prompt_cache_options": {"mode": "explicit", "ttl": "30m"}
		}`)
	})

	t.Run("modern implicit", func(t *testing.T) {
		body, _, _, err := buildProviderRequest(model.Request{
			Model:     "gpt-5.6",
			SessionID: "session-3",
			Parameters: map[string]any{
				"responses": map[string]any{
					"cache": map[string]any{"mode": "implicit", "ttl": "30m"},
				},
			},
		}, true, requestBodyOptions{})
		if err != nil {
			t.Fatalf("buildProviderRequest() error = %v", err)
		}
		assertJSONEqual(t, body, `{
			"model": "gpt-5.6",
			"input": [],
			"stream": true,
			"store": false,
			"prompt_cache_key": "session-3",
			"prompt_cache_options": {"mode": "implicit", "ttl": "30m"}
		}`)
	})
}

func TestBuildProviderRequestValidatesCacheCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		modelID    string
		cache      map[string]any
		messages   []model.Message
		wantErrSub string
	}{
		{
			name:       "explicit requires breakpoint",
			modelID:    "gpt-5.6",
			cache:      map[string]any{"mode": "explicit"},
			wantErrSub: "requires at least one prompt_cache_breakpoint",
		},
		{
			name:       "breakpoint requires explicit mode",
			modelID:    "gpt-5.6",
			cache:      map[string]any{"breakpoint": "instructions"},
			messages:   []model.Message{{Role: model.MessageRoleSystem, Content: "Stable"}},
			wantErrSub: "requires explicit cache mode",
		},
		{
			name:       "legacy rejects modern options",
			modelID:    "gpt-5.5",
			cache:      map[string]any{"mode": "implicit"},
			wantErrSub: "require a GPT-5.6-compatible model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := buildProviderRequest(model.Request{
				Model:    tt.modelID,
				Messages: tt.messages,
				Parameters: map[string]any{
					"responses": map[string]any{"cache": tt.cache},
				},
			}, true, requestBodyOptions{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("buildProviderRequest() error = %v, want containing %q", err, tt.wantErrSub)
			}
		})
	}
}

func TestBuildProviderRequestMapsInputContentBlocks(t *testing.T) {
	body, _, _, err := buildProviderRequest(model.Request{
		Model: "gpt-5.6",
		Messages: []model.Message{{
			Role: model.MessageRoleUser,
			ContentBlocks: []model.InputContentBlock{
				{Type: "input_text", Text: "Inspect these"},
				{Type: "input_image", ImageURL: "https://example.com/image.png", Detail: "high"},
				{Type: "input_file", FileID: "file_123", PromptCacheBreakpoint: true},
			},
		}},
		Parameters: map[string]any{
			"responses": map[string]any{
				"cache": map[string]any{"mode": "explicit"},
			},
		},
	}, true, requestBodyOptions{})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.6",
		"input": [{"role": "user", "content": [
			{"type": "input_text", "text": "Inspect these"},
			{"type": "input_image", "image_url": "https://example.com/image.png", "detail": "high"},
			{"type": "input_file", "file_id": "file_123", "prompt_cache_breakpoint": {"mode": "explicit"}}
		]}],
		"stream": true,
		"store": false,
		"prompt_cache_options": {"mode": "explicit"}
	}`)
}

func TestBuildProviderRequestReplaysResponsesOutputItemsWhenStoreFalse(t *testing.T) {
	body, _, metadata, err := buildProviderRequest(model.Request{
		Model: "gpt-5.6",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Do work"},
			{
				Role:    model.MessageRoleAssistant,
				Content: "Done",
				ToolCalls: []model.ToolCall{{
					ID: "call_1", ProviderID: "fc_1", Name: "lookup", Arguments: `{"id":1}`,
				}},
				ResponseState: &model.ResponseState{
					ID: "resp_1", Origin: "https://api.openai.com/v1", Model: "gpt-5.6",
					MessageID: "msg_1", MessagePhase: "final_answer",
					ReasoningItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"cipher"}`)},
				},
			},
			{Role: model.MessageRoleTool, ToolCallID: "call_1", Content: "result"},
		},
		Tools: []model.Tool{{Name: "lookup"}},
	}, true, requestBodyOptions{origin: "https://api.openai.com/v1"})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	if metadata.Store {
		t.Fatal("metadata.Store = true, want false")
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.6",
		"input": [
			{"role": "user", "content": "Do work"},
			{"type": "reasoning", "encrypted_content": "cipher"},
			{"type": "message", "role": "assistant", "status": "completed", "phase": "final_answer", "content": [
				{"type": "output_text", "text": "Done", "annotations": []}
			]},
			{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{\"id\":1}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "result"}
		],
		"stream": true,
		"store": false,
		"tools": [{"type": "function", "name": "lookup", "description": "", "parameters": {"type": "object", "properties": {}}}]
	}`)
}

func TestBuildProviderRequestKeepsItemIDsForStoredResponses(t *testing.T) {
	body, _, metadata, err := buildProviderRequest(model.Request{
		Model: "gpt-5.6",
		Messages: []model.Message{{
			Role: model.MessageRoleAssistant,
			ResponseState: &model.ResponseState{
				Stored: true,
				Origin: "https://api.openai.com/v1",
				Model:  "gpt-5.6",
				OutputItems: []json.RawMessage{
					json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"cipher"}`),
					json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}`),
				},
			},
		}},
		Parameters: map[string]any{
			"responses": map[string]any{"store": true},
		},
	}, true, requestBodyOptions{origin: "https://api.openai.com/v1"})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	if !metadata.Store {
		t.Fatal("metadata.Store = false, want true")
	}
	assertJSONEqual(t, body, `{
		"model":"gpt-5.6",
		"input":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"cipher"},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}
		],
		"stream":true,
		"store":true
	}`)
}

func TestBuildProviderRequestReplaysExactResponsesOutputItems(t *testing.T) {
	body, _, _, err := buildProviderRequest(model.Request{
		Model: "gpt-5.6",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Do work"},
			{
				Role:    model.MessageRoleAssistant,
				Content: "reconstructed text must not be used",
				ResponseState: &model.ResponseState{
					Origin: "https://api.openai.com/v1",
					Model:  "gpt-5.6",
					OutputItems: []json.RawMessage{
						json.RawMessage(`{"type":"reasoning","id":"rs_exact","encrypted_content":"cipher"}`),
						json.RawMessage(`{"type":"message","id":"msg_exact","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Exact"}]}`),
					},
				},
			},
		},
	}, true, requestBodyOptions{origin: "https://api.openai.com/v1"})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.6",
		"input": [
			{"role": "user", "content": "Do work"},
			{"type": "reasoning", "encrypted_content": "cipher"},
			{"type": "message", "role": "assistant", "status": "completed", "content": [
				{"type": "output_text", "text": "Exact"}
			]}
		],
		"stream": true,
		"store": false
	}`)
}

func TestBuildProviderRequestReplaysOpaqueProviderItemsInOrder(t *testing.T) {
	body, _, _, err := buildProviderRequest(model.Request{
		Model: "gpt-5.6",
		Messages: []model.Message{
			{
				Role: model.MessageRoleProvider,
				ProviderItems: []model.ProviderItem{
					{Origin: "https://api.openai.com/v1", Model: "gpt-5.6", Data: json.RawMessage(`{"type":"message","role":"developer","content":"retained"}`)},
					{Origin: "https://api.openai.com/v1", Model: "gpt-5.6", Data: json.RawMessage(`{"type":"compaction_summary","id":"cmp_1","encrypted_content":"sealed","future_counter":9007199254740993}`)},
				},
			},
			{Role: model.MessageRoleUser, Content: "Continue"},
		},
	}, true, requestBodyOptions{origin: "https://api.openai.com/v1"})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.6",
		"input": [
			{"type":"message","role":"developer","content":"retained"},
			{"type":"compaction_summary","encrypted_content":"sealed","future_counter":9007199254740993},
			{"role":"user","content":"Continue"}
		],
		"stream": true,
		"store": false
	}`)
	if !bytes.Contains(body, []byte(`"future_counter":9007199254740993`)) {
		t.Fatalf("provider item was not replayed without numeric coercion: %s", body)
	}

	_, _, _, err = buildProviderRequest(model.Request{
		Model: "gpt-5.6",
		Messages: []model.Message{{
			Role: model.MessageRoleProvider,
			ProviderItems: []model.ProviderItem{{
				Origin: "https://other.example/v1",
				Model:  "gpt-5.6",
				Data:   json.RawMessage(`{"type":"compaction","encrypted_content":"sealed"}`),
			}},
		}},
	}, true, requestBodyOptions{origin: "https://api.openai.com/v1"})
	if err == nil || !strings.Contains(err.Error(), "belongs to origin") {
		t.Fatalf("buildProviderRequest(mismatched origin) error = %v, want scoped provider item error", err)
	}
}

func TestUsesStandaloneCompactionRequiresExplicitSupportedMode(t *testing.T) {
	enabled, err := UsesStandaloneCompaction(map[string]any{
		"responses": map[string]any{"compaction": map[string]any{"mode": "responses-compact"}},
	})
	if err != nil || !enabled {
		t.Fatalf("UsesStandaloneCompaction() = %v, %v, want true, nil", enabled, err)
	}

	enabled, err = UsesStandaloneCompaction(nil)
	if err != nil || enabled {
		t.Fatalf("UsesStandaloneCompaction(nil) = %v, %v, want false, nil", enabled, err)
	}

	_, err = UsesStandaloneCompaction(map[string]any{
		"responses": map[string]any{"compaction": map[string]any{"mode": "auto"}},
	})
	if err == nil || !strings.Contains(err.Error(), "responses.compaction.mode") {
		t.Fatalf("UsesStandaloneCompaction(invalid) error = %v, want mode validation error", err)
	}
}

func TestBuildCompactionRequestBodyIncludesCanonicalToolPolicyDefaults(t *testing.T) {
	body, _, err := buildCompactionRequestBody(model.Request{
		Model:    "gpt-test",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "work"}},
	}, requestBodyOptions{})
	if err != nil {
		t.Fatalf("buildCompactionRequestBody() error = %v", err)
	}
	assertJSONEqual(t, body, `{
		"model":"gpt-test",
		"input":[{"role":"user","content":"work"}],
		"tools":[],
		"parallel_tool_calls":false
	}`)
}

func TestBuildProviderRequestUsesPreviousResponseIDOnlyForMatchingStoredState(t *testing.T) {
	request := model.Request{
		Model: "gpt-5.6",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "First"},
			{Role: model.MessageRoleAssistant, Content: "Answer", ResponseState: &model.ResponseState{
				ID: "resp_1", Origin: "https://api.openai.com/v1", Model: "gpt-5.6", Stored: true,
			}},
			{Role: model.MessageRoleUser, Content: "Follow up"},
		},
		Parameters: map[string]any{
			"responses": map[string]any{"store": true, "state": "previous_response_id"},
		},
	}
	body, _, metadata, err := buildProviderRequest(request, true, requestBodyOptions{origin: "https://api.openai.com/v1"})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	if !metadata.Store || !metadata.UsedContinuation {
		t.Fatalf("metadata = %#v, want stored continuation", metadata)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.6",
		"input": [{"role": "user", "content": "Follow up"}],
		"stream": true,
		"store": true,
		"previous_response_id": "resp_1"
	}`)

	body, _, metadata, err = buildProviderRequest(request, true, requestBodyOptions{
		origin: "https://another.example/v1", disableContinuation: true,
	})
	if err != nil {
		t.Fatalf("buildProviderRequest(full fallback) error = %v", err)
	}
	if metadata.UsedContinuation {
		t.Fatalf("metadata.UsedContinuation = true, want false")
	}
	assertJSONOmitsKey(t, body, "previous_response_id")
}

func TestBuildRequestBodyOmitsMaxOutputTokensWhenDisabled(t *testing.T) {
	// The Codex backend enforces a strict parameter allowlist and answers
	// HTTP 400 to max_output_tokens, so Codex providers must opt out of the
	// output_limit injection.
	body, _, _, err := buildProviderRequest(model.Request{
		Model:     "gpt-5.1-codex",
		MaxTokens: 128000,
	}, true, requestBodyOptions{omitMaxOutputTokens: true, forceStoreFalse: true})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.1-codex",
		"input": [],
		"stream": true,
		"store": false
	}`)
}

func TestBuildRequestBodyInjectsMaxOutputTokensFromRequest(t *testing.T) {
	body, _, _, err := buildProviderRequest(model.Request{
		Model:     "gpt-5.5",
		MaxTokens: 4096,
	}, true, requestBodyOptions{})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.5",
		"input": [],
		"stream": true,
		"store": false,
		"max_output_tokens": 4096
	}`)
}

func TestBuildRequestBodyKeepsExplicitMaxOutputTokensOverInjection(t *testing.T) {
	body, _, _, err := buildProviderRequest(model.Request{
		Model:     "gpt-5.5",
		MaxTokens: 4096,
		Parameters: map[string]any{
			"max_output_tokens": 1024,
		},
	}, true, requestBodyOptions{})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.5",
		"input": [],
		"stream": true,
		"store": false,
		"max_output_tokens": 1024
	}`)
}

func TestBuildRequestBodyKeepsExplicitMaxTokensOverInjection(t *testing.T) {
	body, _, _, err := buildProviderRequest(model.Request{
		Model:     "gpt-5.5",
		MaxTokens: 4096,
		Parameters: map[string]any{
			"max_tokens": 2048,
		},
	}, true, requestBodyOptions{})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.5",
		"input": [],
		"stream": true,
		"store": false,
		"max_output_tokens": 2048
	}`)
}

func TestBuildRequestBodyReasoningEffortNested(t *testing.T) {
	body, _, _, err := buildProviderRequest(model.Request{
		Model: "gpt-5.5",
		Parameters: map[string]any{
			"reasoning.effort": "high",
		},
	}, true, requestBodyOptions{})
	if err != nil {
		t.Fatalf("buildProviderRequest() error = %v", err)
	}
	assertJSONEqual(t, body, `{
		"model": "gpt-5.5",
		"input": [],
		"stream": true,
		"store": false,
		"reasoning": {"effort": "high"}
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
