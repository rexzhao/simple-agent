package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// 回合结束后用量不能丢：状态条要能继续显示（含缓存命中率）。
//
// 回归：FinishStream 的顺序是 reset() → recordUsage()，reset 清空了
// d.round；而 Usage() 原先只读 round.usage，于是**每一轮对话一结束，
// 状态条上的缓存命中率就消失了**。现在留一份 lastUsage 副本。
func TestUsageSurvivesRoundEnd(t *testing.T) {
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
	var messageID string
	for _, e := range effects {
		if s, ok := e.(driver.StartStream); ok {
			messageID = s.MessageID
		}
	}
	if messageID == "" {
		t.Fatalf("没有 StartStream：%#v", effects)
	}

	// 流式期间：usage 记在 round 上
	_, _ = d.ApplyEvent(messageID, providers.Event{
		Kind:            providers.EventUsage,
		InputTokens:     1000,
		OutputTokens:    50,
		CacheReadTokens: 800,
		CacheReported:   true,
	})
	if got := d.Usage().CacheReadTokens; got != 800 {
		t.Fatalf("流式期间应为 800，实际 %d", got)
	}

	// 结束这一轮（没有工具调用 → Round 结束 → reset 清空 round）
	d.FinishStream(messageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	// **回合结束后仍要能读到**，否则状态条会闪一下就没
	if got := d.Usage().CacheReadTokens; got != 800 {
		t.Fatalf("回合结束后用量丢了（实际 CacheRead=%d），状态条上的命中率会消失", got)
	}
}
