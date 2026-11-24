package host_test

import (
	"strings"
	"testing"
	"time"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
)

// newWakePair 造两个同项目会话，并返回 poster 以便驱动投递队列。
func newWakePair(t *testing.T) (*host.Local, *host.Session, *host.Session, *poster) {
	t.Helper()
	catalog := store.NewCatalog(t.TempDir())
	p := &poster{}
	l := host.NewLocal(host.Config{
		Runtime:       &fakeRuntime{},
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     t.TempDir(),
		MaxToolRounds: 3,
		ContextWindow: 200_000,
		Catalog:       catalog,
		Post:          p.Post,
	})
	t.Cleanup(func() { _ = l.Close() })
	return l, l.NewSession(""), l.NewSession(""), p
}

// 唤醒：空闲会话收到邮件后**真的跑起来**（发一条通知消息）。
//
// 对应需求："如果投递的会话没有处于运行状态，要像原插件一样启动会话并注入提示"。
func TestWakeIdleSession(t *testing.T) {
	_, first, second, p := newWakePair(t)

	before := second.UserMessageCountForTest()
	sendFrom(t, first, second.ID, "关于 shader", "正文内容")

	// 唤醒有去抖延迟（wakeDelay = 3s），且投递走 post 队列
	waitFor(t, 8*time.Second, "收件会话被唤醒", func() bool {
		p.pump()
		return second.UserMessageCountForTest() > before
	})

	last := second.LastUserTextForTest()
	if !strings.Contains(last, "未读") {
		t.Fatalf("唤醒消息应说明未读：%q", last)
	}
	// **不含正文**：正文要它自己用 mailbox_read 取
	if strings.Contains(last, "正文内容") {
		t.Fatalf("唤醒消息不该带正文：%q", last)
	}
	// 防循环：明确说不必回信
	if !strings.Contains(last, "不必回信") {
		t.Fatalf("唤醒消息应提醒不必回信：%q", last)
	}
}

// 连续来信**合并成一次唤醒**，且通知里说明总数。
//
// 用户的定调："唤醒可以稍微加个延迟，防止连续邮件"。
// 一个 agent 常连着发好几封（"发现 A"、"还有 B"），每封都唤醒会让
// 收件人跑好几轮，既慢又费钱。
func TestWakeDebouncesBurst(t *testing.T) {
	_, first, second, p := newWakePair(t)

	// 一口气发三封（模拟连续来信）
	sendFrom(t, first, second.ID, "主题一", "正文一")
	sendFrom(t, first, second.ID, "主题二", "正文二")
	sendFrom(t, first, second.ID, "主题三", "正文三")

	waitFor(t, 8*time.Second, "被唤醒", func() bool {
		p.pump()
		return second.UserMessageCountForTest() > 0
	})
	// 延迟再 pump 几次，确认不会又来第二条唤醒
	time.Sleep(500 * time.Millisecond)
	p.pump()
	p.pump()

	// 只应有一条用户消息（那条唤醒通知）
	if got := second.UserMessageCountForTest(); got != 1 {
		t.Fatalf("三封连续来信应合并成一次唤醒（1 条消息），实际 %d 条", got)
	}
	// 通知里要说清一共 3 封
	if last := second.LastUserTextForTest(); !strings.Contains(last, "3 封未读") {
		t.Fatalf("通知应说明合并后的总数：%q", last)
	}
}

// 同一批邮件只唤醒一次：冷却期内再发信不重复唤醒。
//
// 这是防循环的关键 —— 两个 agent 互相回信时，每次唤醒都触发新的发信，
// 没有这层限制就会变成极快的开销循环。
func TestWakeCooldown(t *testing.T) {
	_, first, second, p := newWakePair(t)

	sendFrom(t, first, second.ID, "第一封", "正文")
	waitFor(t, 8*time.Second, "第一次唤醒", func() bool {
		p.pump()
		return second.UserMessageCountForTest() > 0
	})
	afterFirst := second.UserMessageCountForTest()

	// 冷却期内（30s）再发一封：不唤醒
	sendFrom(t, first, second.ID, "第二封", "正文")
	time.Sleep(wakeDelay + 500*time.Millisecond)
	p.pump()
	if got := second.UserMessageCountForTest(); got != afterFirst {
		t.Fatalf("冷却期内不该再次唤醒：%d → %d", afterFirst, got)
	}
}

// 归档会话不能收信，也就不可能被唤醒。
func TestWakeSkipsArchived(t *testing.T) {
	_, first, second, p := newWakePair(t)
	second.SetArchivedForTest(true)

	before := second.UserMessageCountForTest()
	if _, err := mailboxOf(t, first).MailboxDeliver(second.ID, "主题", "正文", nil); err == nil {
		t.Fatal("归档会话不该能收信")
	}
	time.Sleep(wakeDelay + 500*time.Millisecond)
	p.pump()
	if second.UserMessageCountForTest() != before {
		t.Fatal("归档会话不该被唤醒")
	}
}

// wakeDelay 由 host 包内的常量决定，测试里只知道它不该太长。
func wakeDelayDuration() time.Duration { return 3 * time.Second }

// waitFor 轮询直到条件成立（超时即失败）。
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("超时：%s", what)
}

// wakeDelay 在测试里用固定值（host 包内的常量未导出）。
const wakeDelay = 3 * time.Second

var _ = wakeDelayDuration
