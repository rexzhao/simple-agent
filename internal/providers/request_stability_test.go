package providers

import (
	"strings"
	"testing"

	"simple-agent/internal/message"
)

// buildStabilityMessages 造几条典型消息：纯文本、带工具调用的 agent 消息。
//
// 带工具调用是因为它的序列化最复杂（嵌套 map + 数组），最容易暴露
// 键顺序不稳定的问题。
func buildStabilityMessages() []message.Message {
	return []message.Message{
		{MessageID: "u1", Role: message.RoleUser,
			Parts: []message.Part{message.NewTextPart("你好")}},
		{MessageID: "a1", Role: message.RoleAgent, Parts: []message.Part{
			message.NewTextPart("我看看"),
			{MediaType: "tool_use", Content: message.DataPart{Data: map[string]any{
				"id": "c1", "name": "working_directory", "raw_args": `{"x":1}`,
			}}},
		}},
	}
}

// 请求体必须**逐字节稳定**：同样的输入连续序列化多次，结果完全一致。
//
// 为什么这条测试很关键：OpenAI / Anthropic 的 prompt caching 要求前缀
// 逐字节一致。请求体里的 messages 全是 map[string]any，而
// `encoding/json/v2` 默认按 map 迭代顺序输出键（随机）—— 同一个会话
// 连续两次请求，`{"content":..,"role":..}` 与 `{"role":..,"content":..}`
// 会交替出现，前缀在第一条消息处就分叉，**缓存永远不可能命中**。
//
// 这个 bug 极隐蔽：请求语义完全相同，provider 只回 cached_tokens=0，
// 看不出任何异常。真机 28 万 token 的会话 0% 命中才被发现。
func TestRequestBodyIsByteStable(t *testing.T) {
	req := ChatRequest{
		Model:  "m",
		System: "你是助手",
		Messages: buildStabilityMessages(),
		Tools: []ToolSpec{
			{Name: "read", Description: "读文件", Parameters: map[string]any{
				"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
			}},
		},
	}

	first, err := buildOpenAIBody(req, true, map[string]any{"reasoning_effort": "high"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 连跑 30 次：map 迭代顺序随机，次数够多才能暴露不稳定
	for i := 0; i < 30; i++ {
		again, err := buildOpenAIBody(req, true, map[string]any{"reasoning_effort": "high"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("第 %d 次序列化结果与第一次不同 —— 缓存会永远不命中\n第一次：%s\n这一次：%s",
				i+1, truncateForTest(first), truncateForTest(again))
		}
	}
}

// 键顺序要按字母序（v1 的行为）：这样跨进程、跨版本都一致，
// 不依赖 Go 实现细节。
func TestRequestBodyKeyOrder(t *testing.T) {
	body, err := buildOpenAIBody(ChatRequest{
		Model: "m", System: "s", Messages: buildStabilityMessages(),
	}, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	// messages 里每条消息都以 {"content":..,"role":..} 开头（c < r）
	if !strings.Contains(text, `{"content":"你好","role":"user"}`) {
		t.Fatalf("消息的键应按字母序：%s", truncateForTest(body))
	}
}

// Anthropic 的请求体同样要稳定。
func TestAnthropicBodyIsByteStable(t *testing.T) {
	req := ChatRequest{Model: "m", System: "你是助手", Messages: buildStabilityMessages()}
	first, err := buildAnthropicBody(req, nil, nil, CacheControlConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		again, err := buildAnthropicBody(req, nil, nil, CacheControlConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("第 %d 次与第一次不同：%s / %s",
				i+1, truncateForTest(first), truncateForTest(again))
		}
	}
}

func truncateForTest(data []byte) string {
	if len(data) <= 300 {
		return string(data)
	}
	return string(data[:300]) + "…"
}