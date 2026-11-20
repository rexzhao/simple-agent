package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// newCacheTestDriver 造一个空闲 driver 并起一个流，返回 driver 与 messageID。
func newCacheTestDriver(t *testing.T) (*driver.Driver, string) {
	t.Helper()
	conv := conversation.New("ctx-1")
	d := driver.New(conv, driver.Options{
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(false),
	})
	effects, err := d.Send("你好")
	if err != nil {
		t.Fatal(err)
	}
	messageID := ""
	for _, e := range effects {
		if s, ok := e.(driver.StartStream); ok {
			messageID = s.MessageID
		}
	}
	if messageID == "" {
		t.Fatalf("没有 StartStream：%#v", effects)
	}
	return d, messageID
}

// 缓存 token 要能从流式事件一路进到 Driver.Usage()。
//
// 链路是：provider 解析 SSE → providers.Event{CacheReadTokens}
//
//	→ driver.ApplyEvent(EventUsage) → round.usage → Driver.Usage()
//	→ host.Session.Usage() → 状态条
//
// 中间任何一环丢字段，状态条都显示不出来。
func TestUsageCarriesCacheTokens(t *testing.T) {
	d, messageID := newCacheTestDriver(t)

	_, _ = d.ApplyEvent(messageID, providers.Event{
		Kind:            providers.EventUsage,
		InputTokens:     1000,
		OutputTokens:    50,
		CacheReadTokens: 800,
		CacheReported:   true,
	})

	usage := d.Usage()
	if usage.InputTokens != 1000 {
		t.Fatalf("InputTokens 应为 1000，实际 %d", usage.InputTokens)
	}
	if usage.CacheReadTokens != 800 {
		t.Fatalf("CacheReadTokens 应为 800，实际 %d（这一环丢了，状态条就永远不显示）",
			usage.CacheReadTokens)
	}
}

// 缓存字段每次都要覆盖，包括 0。
//
// 与 input/output 不同：那两个在流里可能分多个事件发（Anthropic 的
// message_start 给 input、message_delta 给 output），0 表示"这次没带"。
// 缓存字段只在一个事件里出现，0 就是"这次没命中"——沿用上一轮的旧值
// 会把命中率显示成假的。
func TestUsageCacheTokensOverwriteWithZero(t *testing.T) {
	d, messageID := newCacheTestDriver(t)

	_, _ = d.ApplyEvent(messageID, providers.Event{
		Kind: providers.EventUsage, InputTokens: 1000, CacheReadTokens: 800,
		CacheReported: true,
	})
	if got := d.Usage().CacheReadTokens; got != 800 {
		t.Fatalf("第一次应记下 800，实际 %d", got)
	}

	// 同轮再来一个不带缓存字段的 usage 块：应覆盖成 0，而不是留着 800
	_, _ = d.ApplyEvent(messageID, providers.Event{
		Kind: providers.EventUsage, InputTokens: 1000,
		CacheReported: true,
	})
	if got := d.Usage().CacheReadTokens; got != 0 {
		t.Fatalf("没命中时应覆盖成 0，实际 %d（会显示成假的命中率）", got)
	}
}

// Anthropic 的写入缓存字段也要能传进去（它只有这一家有）。
func TestUsageCarriesCacheWriteTokens(t *testing.T) {
	d, messageID := newCacheTestDriver(t)

	_, _ = d.ApplyEvent(messageID, providers.Event{
		Kind:             providers.EventUsage,
		InputTokens:      120,
		CacheReadTokens:  900,
		CacheWriteTokens: 80,
		CacheReported:    true,
	})
	usage := d.Usage()
	if usage.CacheReadTokens != 900 || usage.CacheWriteTokens != 80 {
		t.Fatalf("缓存读写应为 900/80，实际 %d/%d",
			usage.CacheReadTokens, usage.CacheWriteTokens)
	}
}
