package driver_test

import (
	"testing"

	"simple-agent/internal/driver"
	"simple-agent/internal/message"
)

// Retry **不追加用户消息**：只重开一轮请求。
//
// 这是它和 "Send(继续)" 的本质区别：
//   - 多一条用户消息 → 弄脏历史（模型看到一句莫名的话）
//   - 还会改变缓存前缀 → 整段缓存作废
func TestRetryDoesNotAppendUserMessage(t *testing.T) {
	d, _ := newDriver(t, true)

	if _, err := d.Send("帮我看看这段代码"); err != nil {
		t.Fatal(err)
	}
	before := countUserMessages(d)
	if before != 1 {
		t.Fatalf("前提：应有 1 条用户消息，实际 %d", before)
	}

	// 收尾当前这一轮（模拟网络失败后界面点了重试）
	closeCurrentStream(t, d)

	if _, err := d.Retry(); err != nil {
		t.Fatalf("重试失败：%v", err)
	}
	after := countUserMessages(d)
	if after != before {
		t.Fatalf("重试不该新增用户消息：之前 %d，之后 %d", before, after)
	}
}

// 没有用户消息时重试要报错（而不是发一个空请求）。
func TestRetryWithoutUserMessage(t *testing.T) {
	d, _ := newDriver(t, true)
	if _, err := d.Retry(); err == nil {
		t.Fatal("没有可重发的消息时应该报错")
	}
}

// 忙碌时不能重试（会让两个流同时跑）。
func TestRetryWhileBusy(t *testing.T) {
	d, _ := newDriver(t, true)
	if _, err := d.Send("你好"); err != nil {
		t.Fatal(err)
	}
	// 不收尾，保持 busy
	if _, err := d.Retry(); err == nil {
		t.Fatal("忙碌时应拒绝重试")
	}
}

func countUserMessages(d *driver.Driver) int {
	n := 0
	for _, entry := range d.Conversation().Entries() {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		if entry.Message.Message.Role == message.RoleUser {
			n++
		}
	}
	return n
}

// closeCurrentStream 结束当前正在流式的消息（让它回到 idle）。
func closeCurrentStream(t *testing.T, d *driver.Driver) {
	t.Helper()
	for _, entry := range d.Conversation().Entries() {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		id := entry.Message.Message.MessageID
		if d.OwnsStream(id) {
			_ = d.StreamClosed(id)
			return
		}
	}
	t.Fatal("没有正在流式的消息")
}