package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// 限流类报错不能被当成"上下文超限"。
//
// 为什么重要：这类报错里常夹带 "too many tokens" 之类的字样
// （如 Bedrock 的 "ThrottlingException: Too many tokens, please wait"），
// 而溢出判定里有 too many tokens —— 不先排除就会白白压缩一次，
// 把好好的历史折叠掉，用户的上下文白丢。
func TestRateLimitIsNotOverflow(t *testing.T) {
	reasons := []string{
		`http (HTTP 429): rate limit exceeded`,
		`Rate limit reached for gpt-4.1`,
		`ThrottlingException: Too many tokens, please wait before trying again.`,
		`Too Many Requests`,
		`quota exceeded for this month`,
		`Service Unavailable`,
		`slow down`,
	}
	for _, reason := range reasons {
		if driver.IsContextOverflowForTest(reason) {
			t.Fatalf("限流/服务不可用不该判成上下文超限：%q", reason)
		}
	}
}

// 413 的识别只看状态码位置，不能误伤正文里出现的 "413"。
//
// 对齐 pi 的 /^(?:[\w -]+ API error \(413\):|413(?:\s|:|$))/：
// request id、token 数里都可能出现 413，误判会白压缩一次。
func TestStatus413PositionMatters(t *testing.T) {
	overflow := []string{
		`http (HTTP 413): {}`,
		`some-provider API error (413): failed to parse request`,
		`413 {"error":{"message":"failed to parse request"}}`,
		`413`,
	}
	for _, reason := range overflow {
		if !driver.IsContextOverflowForTest(reason) {
			t.Fatalf("应识别为 413：%q", reason)
		}
	}

	notOverflow := []string{
		// 正文里提到 413，但状态码是 400
		`github-copilot API error (400): {"message":"failed to parse request: invalid value 413"}`,
		// 4130 不是 413
		`4130 failed to parse request`,
		`custom-provider API error (4130): invalid input`,
	}
	for _, reason := range notOverflow {
		if driver.IsContextOverflowForTest(reason) {
			t.Fatalf("不该误判为 413：%q", reason)
		}
	}
}

// 静默溢出：网关不报错、正常返回，但真实用量已经吃满窗口。
//
// 对齐 pi 的 Case 2（z.ai 风格）：这类网关超限不报错，默默接受，
// 等到下一轮才突然失败、甚至开始静默截断上下文。
// 必须在"正常结束"时就发现，用服务端回的真实 usage 判断。
func TestSilentOverflowTriggersCompact(t *testing.T) {
	conv := conversation.New("ctx-silent")
	// 造够历史，压缩才有东西可折
	for i := 0; i < 20; i++ {
		if _, err := conv.AddMessage(message.Message{
			MessageID: "u" + itoa(i), Role: message.RoleUser,
			Parts: []message.Part{message.NewTextPart("问题")},
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
		if _, err := conv.AddMessage(message.Message{
			MessageID: "a" + itoa(i), Role: message.RoleAgent,
			Parts: []message.Part{message.NewTextPart("回答，长一点好让压缩有东西可折。")},
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Tools:         tools.Builtin(false),
		ContextWindow: 1000, // 小窗口：方便用真实 usage 撞破
	})

	effects, err := d.Send("再来一轮")
	if err != nil {
		t.Fatal(err)
	}
	var messageID string
	for _, e := range effects {
		if s, ok := e.(driver.StartStream); ok {
			messageID = s.MessageID
		}
	}

	// 网关**正常返回**（不是错误），但 usage 已经超出窗口
	_, _ = d.ApplyEvent(messageID, providers.Event{
		Kind:            providers.EventUsage,
		InputTokens:     5000, // > ContextWindow(1000)
		OutputTokens:    10,
		CacheReadTokens: 0,
		CacheReported:   true,
	})
	effects = d.FinishStream(messageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	// 应触发压缩（发起摘要请求）
	sawSummary := false
	for _, e := range effects {
		if _, ok := e.(driver.StartSummary); ok {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatalf("静默溢出应触发压缩，实际 effects：%d 个", len(effects))
	}
}

// 没超窗口时不该因为"正常结束"就压缩。
func TestNoSilentOverflowWhenWithinWindow(t *testing.T) {
	conv := conversation.New("ctx-ok")
	for i := 0; i < 20; i++ {
		conv.AddMessage(message.Message{
			MessageID: "u" + itoa(i), Role: message.RoleUser,
			Parts: []message.Part{message.NewTextPart("问题")},
		}, conversation.StatusComplete)
		conv.AddMessage(message.Message{
			MessageID: "a" + itoa(i), Role: message.RoleAgent,
			Parts: []message.Part{message.NewTextPart("回答。")},
		}, conversation.StatusComplete)
	}
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Tools:         tools.Builtin(false),
		ContextWindow: 200_000, // 大窗口
	})
	effects, _ := d.Send("问")
	var messageID string
	for _, e := range effects {
		if s, ok := e.(driver.StartStream); ok {
			messageID = s.MessageID
		}
	}
	_, _ = d.ApplyEvent(messageID, providers.Event{
		Kind: providers.EventUsage, InputTokens: 1000, OutputTokens: 10,
		CacheReported: true,
	})
	effects = d.FinishStream(messageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	for _, e := range effects {
		if _, ok := e.(driver.StartSummary); ok {
			t.Fatal("没超窗口不该压缩")
		}
	}
}