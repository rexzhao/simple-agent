package driver_test

import (
	"strings"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
)

// 自动压缩是回合结束的另一个边界：排队的插话必须在压缩完成后投递，
// 否则回合停在 Idle，队列会一直挂着。
func TestSteerDeliveredAfterAutoCompact(t *testing.T) {
	conv := conversation.New("ctx-steer-compact")
	d := driver.New(conv, driver.Options{
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:         tools.Builtin(false),
		ContextWindow: 300, // 小窗口：几轮就超阈值
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	// 先用正常回合把上下文撑到阈值以上（此时还没有插话）
	started := false
	for round := 0; round < 4 && !started; round++ {
		effects := autoRound(t, d, rec, strings.Repeat("用户消息填充上下文。", 5))
		if _, ok := effectOfType[driver.StartSummary](effects); ok {
			started = true
		}
	}
	if !started {
		t.Fatal("前置条件失败：没有触发自动压缩")
	}
	// 上一轮已经进入压缩态；先让它成功，回到干净状态
	if _, err := d.ApplyCompact("上一轮的摘要"); err != nil {
		t.Fatal(err)
	}

	// 现在起一轮，生成中插话，并让这一轮结束时再次触发自动压缩
	// （autoCompactUsed 已在每轮 Send 时重置）
	effects, err := d.Send("再来一轮")
	if err != nil {
		t.Fatal(err)
	}
	effects = rec.apply(effects)
	start, ok := effectOfType[driver.StartStream](effects)
	if !ok {
		t.Fatal("没有开始流")
	}
	if _, err := d.Enqueue("压缩完记得看这个"); err != nil {
		t.Fatal(err)
	}
	d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText,
		Text: strings.Repeat("回复内容。", 40)})
	effects, _ = d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
	rec.apply(effects)
	if _, ok := effectOfType[driver.StartSummary](effects); !ok {
		t.Fatalf("这一轮结束应再次自动压缩：%#v", effects)
	}
	if !d.HasQueued() {
		t.Fatal("压缩期间队列应保留")
	}

	// 压缩完成 → 立刻投递插话
	effects, err = d.ApplyCompact("新摘要")
	if err != nil {
		t.Fatal(err)
	}
	rec.apply(effects)
	if _, ok := effectOfType[driver.StartStream](effects); !ok {
		t.Fatalf("压缩完成后应投递插话并开新一轮：%#v", effects)
	}
	if d.HasQueued() {
		t.Fatal("投递后队列应为空")
	}
	history := d.Conversation().History()
	if len(history) == 0 {
		t.Fatal("历史不应为空")
	}
	last := history[len(history)-1]
	if last.PreviewText() != "压缩完记得看这个" {
		t.Fatalf("最后一条应是插话：%q", last.PreviewText())
	}
}
