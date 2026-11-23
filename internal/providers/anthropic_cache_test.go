package providers

import (
	"encoding/json"
	"strings"
	"testing"

	"simple-agent/internal/message"
)

// 三处缓存断点都要打上：system、最后一个 tool、最后一条消息。
//
// Anthropic 的 prompt caching 默认不开，必须在请求里显式标 cache_control，
// 否则服务端返回的 cache_read/cache_creation 恒为 0（实测网关就是这样）。
// 分开打点的价值：缓存按前缀生效，三段独立，历史变化不会让 system/tools 失效。
func TestAnthropicCacheControlBreakpoints(t *testing.T) {
	req := ChatRequest{
		Model:  "m",
		System: "你是助手",
		Messages: []message.Message{
			{Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("第一问")}},
			{Role: message.RoleAgent, Parts: []message.Part{message.NewTextPart("第一答")}},
			{Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("第二问")}},
		},
		Tools: []ToolSpec{
			{Name: "read", Description: "读文件", Parameters: map[string]any{"type": "object"}},
			{Name: "write", Description: "写文件", Parameters: map[string]any{"type": "object"}},
		},
	}
	body, err := buildAnthropicBody(req, nil, nil, CacheControlConfig{})
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}

	// 1) system 上有
	system := parsed["system"].([]any)
	if !hasCacheControl(system[0]) {
		t.Fatalf("system 应带 cache_control：%v", system[0])
	}

	// 2) **只有最后一个** tool 上有（缓存按前缀生效，标最后即缓存整段）
	tools := parsed["tools"].([]any)
	if hasCacheControl(tools[0]) {
		t.Fatalf("第一个 tool 不该打点：%v", tools[0])
	}
	if !hasCacheControl(tools[len(tools)-1]) {
		t.Fatalf("最后一个 tool 应带 cache_control：%v", tools[len(tools)-1])
	}

	// 3) 最后一条消息的最后一个 block 上有
	messages := parsed["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	blocks := last["content"].([]any)
	if !hasCacheControl(blocks[len(blocks)-1]) {
		t.Fatalf("最后一条消息应带 cache_control：%v", last)
	}
	// 前面的消息不该有
	first := messages[0].(map[string]any)
	if hasCacheControl(first["content"].([]any)[0]) {
		t.Fatalf("第一条消息不该打点：%v", first)
	}
}

// 关掉之后一处都不能有。
func TestAnthropicCacheControlDisabled(t *testing.T) {
	body, err := buildAnthropicBody(simpleCacheReq(), nil, nil,
		CacheControlConfig{Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if containsCacheControl(t, body) {
		t.Fatalf("Disabled 时不该出现 cache_control：%s", body)
	}
}

// 可以只关 tools（某些网关不认 tools 上的 cache_control，会 400）。
func TestAnthropicCacheControlNoTools(t *testing.T) {
	body, err := buildAnthropicBody(simpleCacheReq(), nil, nil,
		CacheControlConfig{NoTools: true})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, tool := range parsed["tools"].([]any) {
		if hasCacheControl(tool) {
			t.Fatalf("NoTools 时 tools 上不该有点：%v", tool)
		}
	}
	// 但 system 仍要打
	system := parsed["system"].([]any)
	if !hasCacheControl(system[0]) {
		t.Fatalf("system 仍应打点：%v", system[0])
	}
}

// 可以只关消息。
func TestAnthropicCacheControlNoMessages(t *testing.T) {
	body, err := buildAnthropicBody(simpleCacheReq(), nil, nil,
		CacheControlConfig{NoMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, message := range parsed["messages"].([]any) {
		for _, block := range message.(map[string]any)["content"].([]any) {
			if hasCacheControl(block) {
				t.Fatalf("NoMessages 时消息上不该有点：%v", block)
			}
		}
	}
	// tools 仍要打
	tools := parsed["tools"].([]any)
	if !hasCacheControl(tools[len(tools)-1]) {
		t.Fatalf("tools 仍应打点")
	}
}

// ttl: 1h 要写进请求；默认（5m）不写这个字段。
func TestAnthropicCacheControlTTL(t *testing.T) {
	body, err := buildAnthropicBody(simpleCacheReq(), nil, nil,
		CacheControlConfig{TTL: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(t, body, `"ttl":"1h"`) {
		t.Fatalf("ttl=1h 应写进请求：%s", body)
	}

	body, err = buildAnthropicBody(simpleCacheReq(), nil, nil, CacheControlConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(t, body, `"ttl"`) {
		t.Fatalf("默认不该写 ttl（5m 是默认值）：%s", body)
	}
}

func simpleCacheReq() ChatRequest {
	return ChatRequest{
		Model:  "m",
		System: "你是助手",
		Messages: []message.Message{
			{Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("问")}},
		},
		Tools: []ToolSpec{
			{Name: "read", Description: "读", Parameters: map[string]any{"type": "object"}},
		},
	}
}

func hasCacheControl(value any) bool {
	block, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, exists := block["cache_control"]
	return exists
}

func containsCacheControl(t *testing.T, body []byte) bool {
	t.Helper()
	return containsString(t, body, "cache_control")
}

func containsString(t *testing.T, body []byte, needle string) bool {
	t.Helper()
	return strings.Contains(string(body), needle)
}
