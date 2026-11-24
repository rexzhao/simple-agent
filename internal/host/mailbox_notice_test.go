package host_test

import (
	"strings"
	"testing"

	"simple-agent/internal/store"
)

// 会话通讯的端到端不变量（不做 UI，只验证数据层与投递语义）。
//
// 这条最重要：**同一封邮件不会被反复通知**。两个 agent 互相回信会无限唤醒，
// 每一轮都真花钱 —— 所以通知只在"未读"时出现，读过就消失。
func TestMailboxNoticeStableAcrossRequests(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	l := newHostWithCatalog(t, catalog)
	first := l.NewSession("")
	second := l.NewSession("")

	sendFrom(t, first, second.ID, "主题一", "正文")

	first1 := mailboxNoticeOf(t, second)
	if first1 == "" {
		t.Fatal("应有未读通知")
	}
	// 连续两次组装（模拟两个请求）**内容一致**：说明它是从"未读状态"算出来的，
	// 不会像队列那样越堆越多。
	second1 := mailboxNoticeOf(t, second)
	if first1 != second1 {
		t.Fatalf("两次组装应一致：\n%q\n%q", first1, second1)
	}

	// 读掉之后不再通知
	list := mailboxOf(t, second).MailboxInbox(true, 10)
	if len(list) != 1 {
		t.Fatalf("应有 1 封未读：%d", len(list))
	}
	if _, err := mailboxOf(t, second).MailboxReadMail(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := mailboxNoticeOf(t, second); got != "" {
		t.Fatalf("读完后不该再通知：%q", got)
	}
}

// 多封未读合并成**一条**通知（不是每封一条）。
func TestMailboxNoticeMergesMultiple(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	l := newHostWithCatalog(t, catalog)
	first := l.NewSession("")
	second := l.NewSession("")

	sendFrom(t, first, second.ID, "主题一", "正文一")
	sendFrom(t, first, second.ID, "主题二", "正文二")
	sendFrom(t, first, second.ID, "主题三", "正文三")

	notice := mailboxNoticeOf(t, second)
	if !strings.Contains(notice, "3 封未读") {
		t.Fatalf("应合并成一条通知并说明总数：%q", notice)
	}
	// 三封主题都要列出来（让收件人判断值不值得读）
	for _, subject := range []string{"主题一", "主题二", "主题三"} {
		if !strings.Contains(notice, subject) {
			t.Fatalf("应列出 %q：%q", subject, notice)
		}
	}
	// 但没有正文
	for _, body := range []string{"正文一", "正文二", "正文三"} {
		if strings.Contains(notice, body) {
			t.Fatalf("通知不该含正文 %q：%q", body, notice)
		}
	}
}
