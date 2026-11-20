package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// 网关因请求体过大返回的 413 必须被当成"上下文超限"处理。
//
// 真实踩到的：paperhub 网关对超大请求体返回
//
//	HTTP 413: {"error":{"message":"Entry too big","code":"EntryTooBig"}}
//
// 它限制的是**请求体字节数**而不是 token 数，但处理方式完全一样 ——
// 折叠历史让它变小。漏掉这条的后果很严重：上下文涨过上限后每次请求都失败，
// 而失败不触发压缩，用户就卡在一个永远发不出去的会话里，界面上只有一行
// "HTTP 413"，看不出该做什么。
func TestOverflowDetectsEntityTooLarge(t *testing.T) {
	reasons := []string{
		// 实际收到的那条（driver 拼成 "http (HTTP 413): {...}"）
		`http (HTTP 413): {"error":{"message":"Entry too big Request id: 02179514","type":"Request Entity Too Large","param":"","code":"EntryTooBig"}}`,
		`HTTP 413: Entry too big`,
		`{"code":"EntryTooBig"}`,
		`Request Entity Too Large`,
		`413 Payload Too Large`,
	}
	for _, reason := range reasons {
		if !driver.IsContextOverflowForTest(reason) {
			t.Fatalf("应识别为上下文超限：%q", reason)
		}
	}
}

// 原有的 token 超限文案不能被这次改动弄坏。
func TestOverflowStillDetectsTokenLimits(t *testing.T) {
	reasons := []string{
		`context_length_exceeded`,
		`This model's maximum context length is 128000 tokens`,
		`prompt is too long: 200000 tokens > 128000 maximum`,
		`too many tokens`,
	}
	for _, reason := range reasons {
		if !driver.IsContextOverflowForTest(reason) {
			t.Fatalf("应识别为上下文超限：%q", reason)
		}
	}
}

// 无关的失败不该被误判成超限（否则会白白压缩一次）。
func TestOverflowIgnoresUnrelatedErrors(t *testing.T) {
	reasons := []string{
		`http (HTTP 401): invalid api key`,
		`http (HTTP 429): rate limit exceeded`,
		`connection reset by peer`,
		`http (HTTP 500): internal error`,
	}
	for _, reason := range reasons {
		if driver.IsContextOverflowForTest(reason) {
			t.Fatalf("不该识别为上下文超限：%q", reason)
		}
	}
}

// 413 失败后要**自动压缩一次**，并给出提示。
func TestEntityTooLargeTriggersCompact(t *testing.T) {
	conv := conversation.New("ctx-413")
	// 造够多的历史，让压缩有东西可折叠
	for i := 0; i < 20; i++ {
		if _, err := conv.AddMessage(message.Message{
			MessageID: "u" + itoa(i), Role: message.RoleUser,
			Parts: []message.Part{message.NewTextPart("问题")},
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
		if _, err := conv.AddMessage(message.Message{
			MessageID: "a" + itoa(i), Role: message.RoleAgent,
			Parts: []message.Part{message.NewTextPart("回答内容，写长一点好让压缩有东西可折。")},
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Tools:         tools.Builtin(false),
		ContextWindow: 2000,
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
	if messageID == "" {
		t.Fatalf("没有 StartStream：%#v", effects)
	}

	// 模拟网关回 413
	effects = d.FailStreamWith(messageID,
		`http (HTTP 413): {"error":{"message":"Entry too big","code":"EntryTooBig"}}`)

	// 应产出压缩相关的 effect：一条提示 + 发起摘要请求（StartSummary）
	var sawNotice, sawSummary bool
	for _, e := range effects {
		switch e.(type) {
		case driver.StartSummary:
			sawSummary = true
		case driver.Record:
			// 提示事件的类型在 record 里，用"是否出现了 StartSummary"为主判据
		}
	}
	if !sawSummary {
		t.Fatalf("413 之后应触发压缩（发起摘要请求 StartSummary），实际 effects：%d 个", len(effects))
	}
	// 给用户一条说明，否则界面上只有一行 HTTP 413，看不出该做什么
	for _, e := range effects {
		rec, ok := e.(driver.Record)
		if !ok {
			continue
		}
		if rec.Event.Type == "notice.appended" {
			sawNotice = true
		}
	}
	if !sawNotice {
		t.Fatal("压缩时应有提示（否则用户不知道为什么卡住、该做什么）")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}