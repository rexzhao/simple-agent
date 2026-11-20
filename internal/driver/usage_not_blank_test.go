package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// 新一轮请求刚开始时，状态条不能从 99% 闪成 0%。
//
// 回归（用户："显示 100% 然后又没了"）：Usage() 原先写成
// `if d.round != nil { return d.round.usage }` —— 新一轮的 round 已经建好、
// 但 usage 事件还没到，那份 usage 全 0，于是界面瞬间变成"缓存 0%"或整行消失。
// 网关侧日志显示 cached=685952（99.8%），UI 侧却是 input=0 read=0。
func TestUsageNotBlankedByNewRound(t *testing.T) {
	conv := conversation.New("ctx-1")
	d := driver.New(conv, driver.Options{
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(false),
	})

	// 第一轮：拿到真实 usage
	effects, err := d.Send("第一轮")
	if err != nil {
		t.Fatal(err)
	}
	first := messageIDOf(effects)
	_, _ = d.ApplyEvent(first, providers.Event{
		Kind:            providers.EventUsage,
		InputTokens:     1000,
		OutputTokens:    50,
		CacheReadTokens: 900,
		CacheReported:   true,
	})
	if got := d.Usage().CacheReadTokens; got != 900 {
		t.Fatalf("第一轮应为 900，实际 %d", got)
	}
	d.FinishStream(first, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	// 第二轮开始：round 是新建的、usage 全 0，但此时的 Usage() 不该变成 0
	effects, err = d.Send("第二轮")
	if err != nil {
		t.Fatal(err)
	}
	second := messageIDOf(effects)
	if second == "" {
		t.Fatalf("应有第二轮 StartStream：%#v", effects)
	}
	if got := d.Usage(); got.CacheReadTokens != 900 || got.InputTokens != 1000 {
		t.Fatalf("新一轮刚开始时不该清空上一轮的数据（会把界面闪成 0），实际 %+v", got)
	}

	// 新数据到达后要立刻切过去（不能一直显示旧值）
	_, _ = d.ApplyEvent(second, providers.Event{
		Kind:            providers.EventUsage,
		InputTokens:     2000,
		OutputTokens:    80,
		CacheReadTokens: 1900,
		CacheReported:   true,
	})
	if got := d.Usage(); got.CacheReadTokens != 1900 {
		t.Fatalf("新 usage 到达后应更新为 1900，实际 %d", got.CacheReadTokens)
	}
}

func messageIDOf(effects []driver.Effect) string {
	for _, e := range effects {
		if s, ok := e.(driver.StartStream); ok {
			return s.MessageID
		}
	}
	return ""
}
