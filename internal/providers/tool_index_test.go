package providers

import "testing"

// 网关的 tool_calls 都不带 index（或都从 0 开始）：多个工具的参数不能拼到同一个
// 缓冲里（否则参数变成非法 JSON、整个回合失败）。
func TestOpenAIParserSeparatesToolCallsWithoutIndex(t *testing.T) {
	parser := newOpenAIParser(true)
	first := parser.Parse(SSEEvent{Data: `{"choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"name":"bash","arguments":"{\"command\":"}}]}}]}`})
	second := parser.Parse(SSEEvent{Data: `{"choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"name":"bash","arguments":"{\"command\":"}}]}}]}`})
	firstArgs := parser.Parse(SSEEvent{Data: `{"choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"arguments":"\"pwd\"}"}}]}}]}`})
	secondArgs := parser.Parse(SSEEvent{Data: `{"choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"arguments":"\"ls\"}"}}]}}]}`})

	indexOf := func(events []Event, kind EventKind) int {
		for _, event := range events {
			if event.Kind == kind {
				return event.Index
			}
		}
		return -1
	}
	aStart := indexOf(first, EventToolStart)
	bStart := indexOf(second, EventToolStart)
	if aStart < 0 || bStart < 0 || aStart == bStart {
		t.Fatalf("两个工具分了同一个下标：%d / %d", aStart, bStart)
	}
	if indexOf(firstArgs, EventToolArgs) != aStart || indexOf(secondArgs, EventToolArgs) != bStart {
		t.Fatal("参数增量没有回到各自的工具")
	}
}
