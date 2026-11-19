package providers

import "testing"

// OpenAI 缓存命中数的三个来源都要认，官方字段优先。
//
// 不同兼容网关写法不一样：官方是 usage.prompt_tokens_details.cached_tokens，
// 有的网关照 DeepSeek 给顶层 prompt_cache_hit_tokens，有的给 cached_tokens。
// 只认一种的话，换一个网关缓存命中率就恒为 0。
func TestOpenAIParserCacheTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{
			name: "官方 prompt_tokens_details.cached_tokens",
			body: `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50,` +
				`"prompt_tokens_details":{"cached_tokens":800}}}`,
			want: 800,
		},
		{
			name: "DeepSeek 风格顶层 prompt_cache_hit_tokens",
			body: `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50,` +
				`"prompt_cache_hit_tokens":600}}`,
			want: 600,
		},
		{
			name: "自定义顶层 cached_tokens",
			body: `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50,` +
				`"cached_tokens":400}}`,
			want: 400,
		},
		{
			name: "都没有（旧网关）",
			body: `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50}}`,
			want: 0,
		},
		{
			name: "官方字段优先于其它",
			body: `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50,` +
				`"prompt_tokens_details":{"cached_tokens":700},"cached_tokens":100}}`,
			want: 700,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parser := newOpenAIParser(true)
			events := parser.Parse(SSEEvent{Data: tc.body})
			var usage *Event
			for i := range events {
				if events[i].Kind == EventUsage {
					usage = &events[i]
					break
				}
			}
			if usage == nil {
				t.Fatalf("应产生 usage 事件：%+v", events)
			}
			if usage.CacheReadTokens != tc.want {
				t.Fatalf("缓存命中应为 %d，实际 %d", tc.want, usage.CacheReadTokens)
			}
			// OpenAI 是自动缓存，没有"写入费"的概念
			if usage.CacheWriteTokens != 0 {
				t.Fatalf("OpenAI 不该有 cache write，实际 %d", usage.CacheWriteTokens)
			}
		})
	}
}

// Anthropic 把缓存读写单列，且 input_tokens **不含**它们。
//
// 整段 prompt 都命中缓存时 input_tokens 可能是 0 —— 所以不能只在
// InputTokens > 0 时才发 usage 事件，否则命中率永远显示不出来。
func TestAnthropicParserCacheTokens(t *testing.T) {
	parser := &anthropicParser{}
	events := parser.Parse(SSEEvent{
		Event: "message_start",
		Data: `{"type":"message_start","message":{"usage":{` +
			`"input_tokens":120,"cache_read_input_tokens":900,"cache_creation_input_tokens":80}}}`,
	})
	var usage *Event
	for i := range events {
		if events[i].Kind == EventUsage {
			usage = &events[i]
			break
		}
	}
	if usage == nil {
		t.Fatalf("应产生 usage 事件：%+v", events)
	}
	if usage.InputTokens != 120 || usage.CacheReadTokens != 900 || usage.CacheWriteTokens != 80 {
		t.Fatalf("解析结果不对：input=%d read=%d write=%d",
			usage.InputTokens, usage.CacheReadTokens, usage.CacheWriteTokens)
	}
}

// input_tokens 为 0、只有缓存字段时也要发 usage 事件。
func TestAnthropicParserUsageWhenOnlyCacheHits(t *testing.T) {
	parser := &anthropicParser{}
	events := parser.Parse(SSEEvent{
		Event: "message_start",
		Data: `{"type":"message_start","message":{"usage":{` +
			`"input_tokens":0,"cache_read_input_tokens":1500}}}`,
	})
	found := false
	for _, event := range events {
		if event.Kind == EventUsage && event.CacheReadTokens == 1500 {
			found = true
		}
	}
	if !found {
		t.Fatalf("只有缓存命中时也应发 usage 事件：%+v", events)
	}
}