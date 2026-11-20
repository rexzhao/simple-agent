package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// 超限触发压缩后，压缩完要**自动替用户重发一次**。
//
// 对齐 pi 的 overflow recovery：只压缩不重试的话，用户刚刚才发过消息，
// 却要自己再点一次发送 —— 很容易被当成"程序卡住了"。
//
// 只重试一次：压缩后仍超限说明问题不在历史长度（比如单条工具结果本身
// 就超过网关体积上限），再压也没用，得让用户看见。
func TestOverflowCompactionRetriesOnce(t *testing.T) {
	d, messageID := newOverflowDriver(t)

	effects := d.FailStreamWith(messageID,
		`http (HTTP 413): {"error":{"message":"Entry too big","code":"EntryTooBig"}}`)

	if !hasStartSummary(effects) {
		t.Fatalf("超限应触发压缩，实际 effect 数：%d", len(effects))
	}

	// 压缩完成 → 应自动发起新的一轮（StartStream）
	afterCompact, err := d.ApplyCompact("这是摘要")
	if err != nil {
		t.Fatalf("压缩失败：%v", err)
	}
	if !hasStartStream(afterCompact) {
		t.Fatalf("压缩后应自动重发一次（StartStream），实际 effect 数：%d", len(afterCompact))
	}
}

// 重试只做一次：第二次撞限就停手，不能让用户陷入"压缩→重发→还超限"的循环。
func TestOverflowRetryOnlyOnce(t *testing.T) {
	d, messageID := newOverflowDriver(t)

	effects := d.FailStreamWith(messageID,
		`http (HTTP 413): {"error":{"message":"Entry too big"}}`)
	if !hasStartSummary(effects) {
		t.Fatal("第一次超限应触发压缩")
	}
	afterCompact, err := d.ApplyCompact("摘要")
	if err != nil {
		t.Fatal(err)
	}
	retryID := findStartStream(afterCompact)
	if retryID == "" {
		t.Fatal("压缩后应自动重发一次")
	}

	// 重试仍然撞限
	effects = d.FailStreamWith(retryID,
		`http (HTTP 413): {"error":{"message":"Entry too big"}}`)
	if hasStartSummary(effects) {
		// 允许它再压一次（用户内容确实还太长），但**必须不能一直循环**
		t.Log("第二次超限又触发压缩（可接受，但下面的重试必须不再发生）")
		after, err := d.ApplyCompact("摘要2")
		if err != nil {
			// 压缩失败也正常：没东西可压了
			return
		}
		if hasStartStream(after) {
			t.Fatal("已经自动重试过一次，不该再自动重发（会陷入死循环）")
		}
	}
}

// 正常结束（非超限）时不该有任何自动重试。
func TestNormalFinishDoesNotRetry(t *testing.T) {
	conv := conversation.New("ctx-normal")
	d := driver.New(conv, driver.Options{
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(false),
	})
	effects, _ := d.Send("问")
	var messageID string
	for _, e := range effects {
		if s, ok := e.(driver.StartStream); ok {
			messageID = s.MessageID
		}
	}
	_, _ = d.ApplyEvent(messageID, providers.Event{
		Kind: providers.EventUsage, InputTokens: 10, OutputTokens: 5, CacheReported: true,
	})
	effects = d.FinishStream(messageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	if hasStartStream(effects) {
		t.Fatal("正常结束不该自动重发")
	}
}

// newOverflowDriver 造一个"有足够历史可压"的 driver，并起一轮流。
func newOverflowDriver(t *testing.T) (*driver.Driver, string) {
	t.Helper()
	conv := conversation.New("ctx-overflow")
	for i := 0; i < 20; i++ {
		if _, err := conv.AddMessage(message.Message{
			MessageID: "u" + itoa(i), Role: message.RoleUser,
			Parts: []message.Part{message.NewTextPart("问题")},
		}, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
		if _, err := conv.AddMessage(message.Message{
			MessageID: "a" + itoa(i), Role: message.RoleAgent,
			Parts: []message.Part{message.NewTextPart("回答，写长一些，好让压缩有东西可折。")},
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
	id := findStartStream(effects)
	if id == "" {
		t.Fatalf("没有 StartStream：%d 个 effect", len(effects))
	}
	return d, id
}

func hasStartSummary(effects []driver.Effect) bool {
	for _, e := range effects {
		if _, ok := e.(driver.StartSummary); ok {
			return true
		}
	}
	return false
}

func hasStartStream(effects []driver.Effect) bool {
	return findStartStream(effects) != ""
}

func findStartStream(effects []driver.Effect) string {
	for _, e := range effects {
		if s, ok := e.(driver.StartStream); ok {
			return s.MessageID
		}
	}
	return ""
}