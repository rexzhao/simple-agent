package host_test

import (
	"strings"
	"testing"

	"simple-agent/internal/host"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// 造一个带项目档案的 host，并建两个同项目的会话。
func newMailboxPair(t *testing.T) (*host.Local, *host.Session, *host.Session) {
	t.Helper()
	catalog := store.NewCatalog(t.TempDir())
	l := newHostWithCatalog(t, catalog)

	// 两个会话在同一个工作区 ⇒ 同一个项目
	first := l.NewSession("")
	second := l.NewSession("")
	if first.ProjectID == "" || second.ProjectID == "" {
		t.Fatalf("两个会话都该有项目 ID：%q %q", first.ProjectID, second.ProjectID)
	}
	if first.ProjectID != second.ProjectID {
		t.Fatalf("同一工作区的会话应属于同一项目：%q vs %q",
			first.ProjectID, second.ProjectID)
	}
	return l, first, second
}

// 同项目内可以发信；信落到收件人的邮箱里。
func TestMailboxDeliverWithinProject(t *testing.T) {
	_, first, second := newMailboxPair(t)

	sendFrom(t, first, second.ID, "关于 shader", "正文内容")

	list := mailboxOf(t, second).MailboxInbox(false, 10)
	if len(list) != 1 {
		t.Fatalf("收件人应有 1 封，实际 %d", len(list))
	}
	if list[0].Subject != "关于 shader" || list[0].Body != "正文内容" {
		t.Fatalf("内容不对：%+v", list[0])
	}
	if list[0].From != first.ID {
		t.Fatalf("发件人应是第一个会话：%+v", list[0])
	}
	// 发件人自己的邮箱里不该有这封信
	if got := mailboxOf(t, first).MailboxInbox(false, 10); len(got) != 0 {
		t.Fatalf("发件人邮箱应为空：%+v", got)
	}
}

// 归档会话不能收信（用户定的规则）。
func TestMailboxRejectsArchived(t *testing.T) {
	_, first, second := newMailboxPair(t)
	second.SetArchivedForTest(true)

	host := mailboxOf(t, first)
	_, err := host.MailboxDeliver(second.ID, "主题", "正文", nil)
	if err == nil {
		t.Fatal("归档会话不该能收信")
	}
	if !strings.Contains(err.Error(), "归档") {
		t.Fatalf("错误应说明归档：%v", err)
	}
}

// 归档会话不出现在可收信列表里。
func TestMailboxPeersExcludeArchived(t *testing.T) {
	_, first, second := newMailboxPair(t)
	second.SetArchivedForTest(true)

	peers := mailboxOf(t, first).MailboxPeers()
	for _, peer := range peers {
		if peer.ID == second.ID {
			t.Fatal("归档会话不该出现在可收信列表里")
		}
	}
}

// 不能给自己发信。
func TestMailboxRejectsSelf(t *testing.T) {
	_, first, _ := newMailboxPair(t)
	_, err := mailboxOf(t, first).MailboxDeliver(first.ID, "主题", "正文", nil)
	if err == nil {
		t.Fatal("不该能给自己发信")
	}
}

// 未读通知：只列清单不含正文，且说明"不需要行动就不必回信"。
func TestMailboxNoticeContent(t *testing.T) {
	_, first, second := newMailboxPair(t)

	// 给 second 发一封
	if _, err := mailboxOf(t, first).MailboxDeliver(second.ID, "关于 shader", "这里是正文", nil); err != nil {
		t.Fatal(err)
	}

	notice := mailboxNoticeOf(t, second)
	if notice == "" {
		t.Fatal("收到信后应有未读通知")
	}
	if !strings.Contains(notice, "1 封未读") {
		t.Fatalf("应说明未读数：%q", notice)
	}
	if !strings.Contains(notice, "关于 shader") {
		t.Fatalf("应列出主题：%q", notice)
	}
	// **不含正文**：正文让它自己用 mailbox_read 取，避免长期占用上下文
	if strings.Contains(notice, "这里是正文") {
		t.Fatalf("通知里不该有正文：%q", notice)
	}
	// 防循环：明确说不必回信
	if !strings.Contains(notice, "不必回信") {
		t.Fatalf("应提醒不必回信（防止互相唤醒）：%q", notice)
	}
}

// 读信后未读清零，通知也不再出现。
func TestMailboxReadClearsNotice(t *testing.T) {
	_, first, second := newMailboxPair(t)
	from := mailboxOf(t, first)
	id, err := from.MailboxDeliver(second.ID, "主题", "正文", nil)
	if err != nil {
		t.Fatal(err)
	}

	to := mailboxOf(t, second)
	if _, err := to.MailboxReadMail(id); err != nil {
		t.Fatal(err)
	}
	if notice := mailboxNoticeOf(t, second); notice != "" {
		t.Fatalf("读完之后不该再有通知：%q", notice)
	}
}

// 发件人标题从会话标题取（收件人靠它知道是谁在找它）。
func TestMailboxSenderTitle(t *testing.T) {
	_, first, second := newMailboxPair(t)
	first.SetTitleForTest("修复特效")
	sendFrom(t, first, second.ID, "主题", "正文")

	// 未读通知里应出现发件人标题
	notice := mailboxNoticeOf(t, second)
	if !strings.Contains(notice, "修复特效") {
		t.Fatalf("通知应说明来自谁：%q", notice)
	}
	// 邮件本身也存了标题（发件人后来改名或删除也还能看出是谁）
	list := mailboxOf(t, second).MailboxInbox(false, 10)
	if len(list) != 1 || list[0].FromTitle != "修复特效" {
		t.Fatalf("邮件应记下发件人标题：%+v", list)
	}
}

// ---- 辅助 ----

// mailboxOf 取出会话的通讯客户端（测试用）。
//
// 通过 ToolsContext 拿不到私有字段，所以走 host 暴露的入口。
func mailboxOf(t *testing.T, session *host.Session) tools.MailboxHost {
	t.Helper()
	client := session.MailboxForTest()
	if client == nil {
		t.Fatal("会话应已接上通讯能力")
	}
	return client
}

func mailboxNoticeOf(t *testing.T, session *host.Session) string {
	t.Helper()
	return session.MailboxNoticeForTest()
}

func sendFrom(t *testing.T, from *host.Session, to, subject, body string) string {
	t.Helper()
	id, err := mailboxOf(t, from).MailboxDeliver(to, subject, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
