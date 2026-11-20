package driver_test

import (
	"fmt"
	"strings"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// newUsageDriver 造一个 driver 并起一个流。
func newUsageDriver(t *testing.T) (*driver.Driver, func(string) string) {
	t.Helper()
	conv := conversation.New("ctx-usage")
	d := driver.New(conv, driver.Options{
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(false),
	})
	// send 发一条消息，返回 messageID
	send := func(text string) string {
		effects, err := d.Send(text)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range effects {
			if s, ok := e.(driver.StartStream); ok {
				return s.MessageID
			}
		}
		t.Fatalf("没有 StartStream：%#v", effects)
		return ""
	}
	return d, send
}

// 累计用量要把每一次请求都加进去，而不是只留最后一次。
//
// 这是命中率改累计口径的基础：单次请求的数字抖得厉害（新建缓存那轮 0%、
// 紧接着全命中 99.9%），累计值才稳定。
func TestTotalUsageAccumulates(t *testing.T) {
	d, send := newUsageDriver(t)

	round := func(input, output, read, write int) {
		id := send("问题")
		_, _ = d.ApplyEvent(id, providers.Event{
			Kind:             providers.EventUsage,
			InputTokens:      input,
			OutputTokens:     output,
			CacheReadTokens:  read,
			CacheWriteTokens: write,
			CacheReported:    true,
		})
		d.FinishStream(id, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	}

	round(1000, 50, 800, 0)
	if got := d.TotalUsage(); got.InputTokens != 1000 || got.CacheReadTokens != 800 {
		t.Fatalf("第一轮后应为 input=1000 read=800，实际 %+v", got)
	}

	round(2000, 80, 1900, 100)
	got := d.TotalUsage()
	if got.InputTokens != 3000 || got.OutputTokens != 130 ||
		got.CacheReadTokens != 2700 || got.CacheWriteTokens != 100 {
		t.Fatalf("第二轮后应累加为 input=3000 output=130 read=2700 write=100，实际 %+v", got)
	}

	// Usage() 仍是"最近一次"，两者不能混
	if last := d.Usage(); last.InputTokens != 2000 {
		t.Fatalf("Usage() 应仍是最近一次（2000），实际 %d", last.InputTokens)
	}
}

// 一次请求只累加一次：Anthropic 的 input/output 分两个事件发，
// 在事件里累加会把同一个请求算两遍。
func TestTotalUsageCountsOncePerRequest(t *testing.T) {
	d, send := newUsageDriver(t)
	id := send("问")

	// 模拟 Anthropic：message_start 给 input，message_delta 给 output
	_, _ = d.ApplyEvent(id, providers.Event{
		Kind: providers.EventUsage, InputTokens: 500, CacheReadTokens: 400,
		// 这条带缓存字段
		CacheReported: true,
	})
	_, _ = d.ApplyEvent(id, providers.Event{
		// message_delta：只带 output，**不带**缓存字段。
		// 关键是 CacheReported 为 false —— 它不该把上面那 400 抹成 0。
		Kind: providers.EventUsage, OutputTokens: 60,
	})
	d.FinishStream(id, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	got := d.TotalUsage()
	if got.InputTokens != 500 || got.OutputTokens != 60 {
		t.Fatalf("一次请求应只累加一次，实际 %+v（input=500 output=60 才是对的）", got)
	}
	if got.CacheReadTokens != 400 {
		t.Fatalf("缓存字段应保留 400，实际 %d", got.CacheReadTokens)
	}
}

// 压缩会把累计值写进快照，重放后接续；命中率不能因为压缩掉回 0。
func TestTotalUsageSurvivesCompact(t *testing.T) {
	dir := t.TempDir()
	file, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// 长对话：压缩要求"有更早的内容可折叠"，只有一轮会被拒
	history := make([]message.Message, 0, 40)
	for i := 0; i < 20; i++ {
		history = append(history,
			message.Message{
				MessageID: fmt.Sprintf("u%d", i), Role: message.RoleUser,
				Parts: []message.Part{message.NewTextPart(fmt.Sprintf("问题 %d", i))},
			},
			message.Message{
				MessageID: fmt.Sprintf("a%d", i), Role: message.RoleAgent,
				Parts: []message.Part{message.NewTextPart(strings.Repeat("回答内容。", 50))},
			},
		)
	}

	conv := conversation.New("ctx-compact")
	for _, msg := range history {
		if _, err := conv.AddMessage(msg, conversation.StatusComplete); err != nil {
			t.Fatal(err)
		}
	}
	d := driver.New(conv, driver.Options{
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(false),
		// 窗口给小一点，保证"需要压缩"
		ContextWindow: 2000,
		// 关掉自动压缩：否则回合一结束就会自己压，之后 BeginCompact 会
		// 报"当前状态不接受该操作"（正处在 compacting）。这里要手动控时序。
		DisableAutoCompact: true,
	})

	// 跑一轮积累用量（这一轮走完整个过程，回合结束回到 Idle）
	effects, _ := d.Send("你好")
	var messageID string
	for _, e := range effects {
		if s, ok := e.(driver.StartStream); ok {
			messageID = s.MessageID
		}
	}
	_, _ = d.ApplyEvent(messageID, providers.Event{
		Kind: providers.EventUsage, InputTokens: 5000, OutputTokens: 200,
		CacheReadTokens: 4800,
		CacheReported:   true,
	})
	d.FinishStream(messageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	before := d.TotalUsage()
	if before.CacheReadTokens != 4800 {
		t.Fatalf("压缩前应累计 4800，实际 %d", before.CacheReadTokens)
	}

	// 压缩：BeginCompact 拿到摘要请求的 effect，再 ApplyCompact 落地
	if _, err := d.BeginCompact(); err != nil {
		t.Fatalf("开始压缩失败：%v", err)
	}
	effects2, err := d.ApplyCompact("摘要")
	if err != nil {
		t.Fatalf("压缩失败：%v", err)
	}
	// 从 effect 里取出要写盘的快照
	var snapshot store.Event
	found := false
	for _, e := range effects2 {
		if rec, ok := e.(driver.Record); ok && rec.Event.Type == store.TypeCompactSnapshot {
			snapshot = rec.Event
			found = true
		}
	}
	if !found {
		t.Fatalf("压缩没有产出快照事件：%#v", effects2)
	}
	if err := file.Compact(snapshot); err != nil {
		t.Fatal(err)
	}

	// 累计值在压缩后**不能清零**
	if after := d.TotalUsage(); after.CacheReadTokens != 4800 {
		t.Fatalf("压缩不该清掉累计用量（命中率会掉成 0），实际 %d", after.CacheReadTokens)
	}

	// 重放：从快照恢复
	loaded, err := file.Load()
	if err != nil {
		t.Fatal(err)
	}
	restored, replayed, err := driver.Load(loaded.Events, driver.Options{
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手",
		Tools:        tools.Builtin(false),
	})
	if err != nil {
		t.Fatalf("重放失败：%v", err)
	}
	_ = replayed
	if got := restored.TotalUsage(); got.CacheReadTokens != 4800 || got.InputTokens != 5000 {
		t.Fatalf("重放后应从快照接续（input=5000 read=4800），实际 %+v", got)
	}
}
