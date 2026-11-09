package driver_test

import (
	"errors"
	"strings"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// autoRound 跑一轮正常回合，并返回回合结束时的 effects（自动压缩就在其中）。
func autoRound(t *testing.T, d *driver.Driver, rec *recorder, text string) []driver.Effect {
	t.Helper()
	effects, err := d.Send(text)
	if err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	effects = rec.apply(effects)
	start, ok := effectOfType[driver.StartStream](effects)
	if !ok {
		t.Fatal("没有开始流")
	}
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: strings.Repeat("回复内容。", 10)})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	return rec.apply(effects)
}

// newAutoDriver 构造开着自动压缩的 driver。
func newAutoDriver(t *testing.T, window int) (*driver.Driver, *recorder) {
	t.Helper()
	conv := conversation.New("ctx-auto")
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:         tools.Builtin(false),
		ContextWindow: window,
	})
	rec := &recorder{}
	rec.apply(d.Created(""))
	return d, rec
}

// P0：自动压缩的决策在 driver 里（回合正常结束 → 直接返回摘要请求）。
func TestAutoCompactDecidedByDriver(t *testing.T) {
	d, rec := newAutoDriver(t, 300)
	started := false
	for round := 0; round < 4 && !started; round++ {
		effects := autoRound(t, d, rec, strings.Repeat("用户消息填充上下文。", 5))
		if _, ok := effectOfType[driver.StartSummary](effects); ok {
			started = true
			if d.Busy() != driver.BusyCompacting {
				t.Fatalf("应进入压缩状态，实际 %v", d.Busy())
			}
		}
	}
	if !started {
		t.Fatal("超过阈值时应由 driver 直接发起自动压缩")
	}
}

// P0：压缩失败之后，同一个回合内不会再自动重试（否则断网会无限烧 token）；
// 下一个用户回合仍可再试一次（这是设计允许的粒度）。
func TestAutoCompactDoesNotRetryAfterFailure(t *testing.T) {
	d, rec := newAutoDriver(t, 300)

	triggered := false
	for round := 0; round < 4 && !triggered; round++ {
		effects := autoRound(t, d, rec, strings.Repeat("用户消息填充上下文。", 5))
		if _, ok := effectOfType[driver.StartSummary](effects); ok {
			triggered = true
			rec.apply(d.FailCompact("摘要请求失败：连接被拒绝"))
			if d.Busy() != driver.BusyIdle {
				t.Fatalf("失败后应回到 idle，实际 %v", d.Busy())
			}
		}
	}
	if !triggered {
		t.Fatal("应先触发一次自动压缩")
	}

	// 失败后紧接着不应该再冒出第二个摘要请求（不能形成循环）
	if _, ok := effectOfType[driver.StartSummary](rec.apply(d.FailCompact("再次失败"))); ok {
		t.Fatal("失败后不应立刻重试")
	}

	// 新一轮用户回合允许再尝试一次，但每回合最多一次
	for round := 0; round < 3; round++ {
		effects := autoRound(t, d, rec, strings.Repeat("继续填充。", 5))
		starts := 0
		for _, effect := range effects {
			if _, ok := effect.(driver.StartSummary); ok {
				starts++
			}
		}
		if starts > 1 {
			t.Fatalf("每回合最多自动压缩一次，实际 %d", starts)
		}
		if starts == 1 {
			rec.apply(d.FailCompact("失败"))
		}
	}
}

// P0：压缩成功但用量仍超阈值 → 给提示；同一回合内不再循环压缩。
func TestAutoCompactStopsWhenStillOverLimit(t *testing.T) {
	d, rec := newAutoDriver(t, 300)

	// 一轮就超过窗口的大回复
	effects, err := d.Send("开始")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)
	d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventText, Text: strings.Repeat("非常长的回复。", 300)})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec.apply(effects)

	if d.Busy() == driver.BusyCompacting {
		if _, err := d.ApplyCompact("摘要"); err != nil {
			t.Fatal(err)
		}
	}

	// 同一回合内不会反复压缩：紧接着再跑一帧也不会出现摘要请求
	if _, ok := effectOfType[driver.StartSummary](rec.apply(d.FailCompact("x"))); ok {
		t.Fatal("不应立即重复压缩")
	}

	// 之后每回合最多一次（允许重试，但不会在一个回合里循环）
	for round := 0; round < 3; round++ {
		effects := autoRound(t, d, rec, "再来一轮")
		starts := 0
		for _, effect := range effects {
			if _, ok := effect.(driver.StartSummary); ok {
				starts++
			}
		}
		if starts > 1 {
			t.Fatalf("每回合最多压缩一次，实际 %d", starts)
		}
		if starts == 1 {
			rec.apply(d.FailCompact("失败"))
		}
	}
}

// P0：停止之后不自动压缩（与用户意图相反）。
func TestNoAutoCompactAfterStop(t *testing.T) {
	d, rec := newAutoDriver(t, 300)

	effects, err := d.Send(strings.Repeat("很长的用户消息。", 30))
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: strings.Repeat("回复。", 30)})

	stopEffects := rec.apply(d.Stop())
	if _, ok := effectOfType[driver.StartSummary](stopEffects); ok {
		t.Fatal("停止不应触发压缩")
	}
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("停止后应 idle，实际 %v", d.Busy())
	}
}

// P0：回合因「上下文超出窗口」失败 → 自动压缩一次并提示重试。
func TestContextOverflowFailureTriggersCompact(t *testing.T) {
	d, rec := newAutoDriver(t, 300)

	// 先攒几轮（触发自动压缩就让它失败，模拟「压缩不生效」的场景）
	for i := 0; i < 3; i++ {
		effects := autoRound(t, d, rec, strings.Repeat("填充。", 5))
		if _, ok := effectOfType[driver.StartSummary](effects); ok {
			rec.apply(d.FailCompact("摘要失败"))
		}
	}

	effects, err := d.Send("继续")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventError,
		Err:  errors.New("400 Bad Request: this model's maximum context length is 8192 tokens"),
	})
	effects = rec.apply(effects)

	if _, ok := effectOfType[driver.StartSummary](effects); !ok {
		t.Fatalf("超窗失败应触发一次自动压缩，busy=%v", d.Busy())
	}
	found := false
	for _, entry := range d.Conversation().Entries() {
		if entry.Notice != nil && strings.Contains(entry.Notice.Text, "请重试") {
			found = true
		}
	}
	if !found {
		t.Fatal("应提示用户重试")
	}
}

// P1：用量基线用真实 usage（含 system 与工具的开销）。
func TestUsageBaselineUsesRealUsage(t *testing.T) {
	conv := conversation.New("ctx-usage")
	d := driver.New(conv, driver.Options{
		Selection:          providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:              tools.Builtin(false),
		ContextWindow:      100_000,
		DisableAutoCompact: true,
	})
	effects, _ := d.Send("你好")
	start, _ := effectOfType[driver.StartStream](effects)
	d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventUsage, InputTokens: 90_000, OutputTokens: 5_000})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	if got := d.TokensInUse(); got < 95_000 {
		t.Fatalf("应使用真实 usage 作为基线，实际 %d", got)
	}
	if !d.ShouldCompact() {
		t.Fatalf("95k/100k 应触发压缩：tokens=%d", d.TokensInUse())
	}
}

// P1：窗口未知时按 32k 处理，并提示一次。
func TestUnknownWindowUsesDefault(t *testing.T) {
	conv := conversation.New("ctx-default")
	d := driver.New(conv, driver.Options{
		Selection:          providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:              tools.Builtin(false),
		DisableAutoCompact: true,
	})
	effects, _ := d.Send(strings.Repeat("很长的消息。", 20))
	start, _ := effectOfType[driver.StartStream](effects)
	d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventText, Text: strings.Repeat("长回复。", 9000)})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})

	if !d.ShouldCompact() {
		t.Fatalf("窗口未知时应按 32k 判定，实际 tokens=%d", d.TokensInUse())
	}
	text, needed := d.NeedsDefaultWindowNotice()
	if !needed || !strings.Contains(text, "32k") {
		t.Fatalf("应提示按默认窗口处理：%q", text)
	}
	if _, again := d.NeedsDefaultWindowNotice(); again {
		t.Fatal("默认窗口提示只应出现一次")
	}
}

// P0：摘要请求的输入有上限（否则压缩请求自己就会超窗）。
func TestSummaryInputIsBounded(t *testing.T) {
	conv := conversation.New("ctx-bounded")
	d := driver.New(conv, driver.Options{
		Selection:          providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:              tools.Builtin(false),
		ContextWindow:      2_000,
		DisableAutoCompact: true,
	})
	rec := &recorder{}
	rec.apply(d.Created(""))
	// 每轮都写很长，保证 older 部分超过摘要预算
	for i := 0; i < 6; i++ {
		effects, err := d.Send(strings.Repeat("这一段用户消息很长。", 30))
		if err != nil {
			t.Fatalf("发送失败：%v", err)
		}
		effects = rec.apply(effects)
		start, _ := effectOfType[driver.StartStream](effects)
		d.ApplyEvent(start.MessageID, providers.Event{
			Kind: providers.EventText, Text: strings.Repeat("这一段回复也很长。", 30)})
		effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
		rec.apply(effects)
	}

	effects, err := d.BeginCompact()
	if err != nil {
		t.Fatalf("应能开始压缩：%v", err)
	}
	start, _ := effectOfType[driver.StartSummary](effects)
	if len(start.Request.Messages) != 1 {
		t.Fatalf("摘要请求应只有一条消息，实际 %d", len(start.Request.Messages))
	}
	prompt := start.Request.Messages[0].PreviewText()
	if tokens := estimateTokensForTest(prompt); tokens > 1_300 {
		t.Fatalf("摘要输入应压到窗口 60%% 以内（≤1200），实际 %d", tokens)
	}
	if !strings.Contains(prompt, "省略") {
		t.Fatal("截断后应有省略标记")
	}
}

// estimateTokensForTest 复刻 driver 的估算口径（CJK 1/字，其余字节/4）。
func estimateTokensForTest(text string) int {
	cjk, other := 0, 0
	for _, r := range text {
		if r >= 0x2E80 && r <= 0x9FFF || r >= 0xFF00 && r <= 0xFFEF {
			cjk++
			continue
		}
		other += len(string(r))
	}
	return cjk + other/4
}

// 回归（opus）：估算低于阈值但服务端报超窗 → 仍要强制压缩一次。
//
// 超窗本身就说明本地估算偏低；按估算值决定是否压缩，用户重试多少次都会失败。
func TestContextOverflowForcesCompactDespiteEstimate(t *testing.T) {
	conv := conversation.New("ctx-force")
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:         tools.Builtin(false),
		ContextWindow: 100_000, // 估算远低于阈值
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	effects, err := d.Send("短消息")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)

	// 先攒一点内容，让 BeginCompact 有东西可压（至少两轮）
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: "回复"})
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	effects, _ = d.Send("第二轮")
	effects = rec.apply(effects)
	start, _ = effectOfType[driver.StartStream](effects)

	if d.ShouldCompact() {
		t.Fatal("前置条件：估算不应触发阈值")
	}

	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{
		Kind: providers.EventError,
		Err:  errors.New("context_length_exceeded: your message is too long"),
	})
	effects = rec.apply(effects)

	if _, ok := effectOfType[driver.StartSummary](effects); !ok {
		t.Fatalf("超窗失败应强制压缩一次，busy=%v", d.Busy())
	}
	// 提示文案应说明「正在压缩」而不是「已压缩」
	found := false
	for _, entry := range d.Conversation().Entries() {
		if entry.Notice != nil && strings.Contains(entry.Notice.Text, "正在自动压缩") {
			found = true
		}
	}
	if !found {
		t.Fatal("应提示「正在自动压缩，完成后请重试」")
	}
}

// 开流阶段就失败（HTTP 400/401 等）时，失败原因要如实记录，不能统一成
// 「连接意外中断」——那会把真正的原因藏起来（用户报过这个问题）。
func TestFailStreamWithKeepsReason(t *testing.T) {
	conv := conversation.New("ctx-http")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created(""))
	effects, err := d.Send("hi")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	start, _ := effectOfType[driver.StartStream](effects)

	reason := `http (HTTP 400): {"error":{"message":"max_tokens参数非法：限制数值范围[1,131072]"}}`
	rec.apply(d.FailStreamWith(start.MessageID, reason))

	entry, ok := conv.Message(start.MessageID)
	if !ok {
		t.Fatal("消息应存在")
	}
	if entry.Status() != conversation.StatusFailed {
		t.Fatalf("应判为失败，实际 %v", entry.Status())
	}
	if !strings.Contains(entry.FailureReason(), "max_tokens") {
		t.Fatalf("失败原因应保留服务端信息：%q", entry.FailureReason())
	}
	if strings.Contains(entry.FailureReason(), "连接意外中断") {
		t.Fatal("不应被替换成连接中断")
	}
	if d.Busy() != driver.BusyIdle {
		t.Fatalf("应回到 idle，实际 %v", d.Busy())
	}
}
