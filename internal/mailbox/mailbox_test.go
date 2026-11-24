package mailbox_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"simple-agent/internal/mailbox"
)

func newBox(t *testing.T) *mailbox.Box {
	t.Helper()
	return mailbox.Open(t.TempDir(), "target-session")
}

func TestAddAndList(t *testing.T) {
	box := newBox(t)
	mail, err := box.Add(mailbox.Mail{
		From: "sender", FromTitle: "修复特效",
		Subject: "关于 shader", Body: "正文内容",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mail.ID == "" {
		t.Fatal("应生成 ID")
	}

	list := box.List(false, 0)
	if len(list) != 1 {
		t.Fatalf("应有 1 封，实际 %d", len(list))
	}
	if list[0].Subject != "关于 shader" || list[0].To != "target-session" {
		t.Fatalf("内容不对：%+v", list[0])
	}
	if list[0].Read {
		t.Fatal("新邮件不该是已读")
	}
}

// 已读也是追加事件：重开邮箱后状态要保持（不能只存在内存里）。
func TestMarkReadPersists(t *testing.T) {
	dir := t.TempDir()
	box := mailbox.Open(dir, "target")
	mail, err := box.Add(mailbox.Mail{From: "a", Subject: "s", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.MarkRead(mail.ID); err != nil {
		t.Fatal(err)
	}

	// 重新打开：模拟重启
	reopened := mailbox.Open(dir, "target")
	if unread, _ := reopened.Unread(); unread != 0 {
		t.Fatalf("重开后应是 0 未读，实际 %d", unread)
	}
	got, err := reopened.Get(mail.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Read {
		t.Fatal("重开后应保持已读")
	}
}

// 重复标记已读不该重复追加事件（文件会无限长）。
func TestMarkReadIdempotent(t *testing.T) {
	dir := t.TempDir()
	box := mailbox.Open(dir, "target")
	mail, _ := box.Add(mailbox.Mail{From: "a", Subject: "s"})

	if err := box.MarkRead(mail.ID); err != nil {
		t.Fatal(err)
	}
	before := fileSize(t, filepath.Join(dir, "target.jsonl"))
	if err := box.MarkRead(mail.ID); err != nil {
		t.Fatal(err)
	}
	after := fileSize(t, filepath.Join(dir, "target.jsonl"))
	if after != before {
		t.Fatalf("重复标记不该写文件：%d → %d", before, after)
	}
}

// 未读数与发件人标题：投递层靠它组装通知。
func TestUnreadSenders(t *testing.T) {
	box := newBox(t)
	box.Add(mailbox.Mail{From: "s1", FromTitle: "修复特效", Subject: "一"})
	box.Add(mailbox.Mail{From: "s1", FromTitle: "修复特效", Subject: "二"})
	box.Add(mailbox.Mail{From: "s2", FromTitle: "重构", Subject: "三"})

	unread, senders := box.Unread()
	if unread != 3 {
		t.Fatalf("应 3 封未读，实际 %d", unread)
	}
	// 去重但保持顺序
	if len(senders) != 2 || senders[0] != "修复特效" || senders[1] != "重构" {
		t.Fatalf("发件人列表不对：%v", senders)
	}
}

// 不存在的邮件要报 ErrNotFound（工具层据此给友好提示）。
func TestGetNotFound(t *testing.T) {
	box := newBox(t)
	if _, err := box.Get("nope"); !errors.Is(err, mailbox.ErrNotFound) {
		t.Fatalf("应返回 ErrNotFound，实际 %v", err)
	}
}

// 校验：主题必填、长度上限。
func TestValidate(t *testing.T) {
	box := newBox(t)
	if _, err := box.Add(mailbox.Mail{From: "a"}); err == nil {
		t.Fatal("空主题应被拒")
	}
	long := make([]rune, mailbox.MaxSubject+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := box.Add(mailbox.Mail{From: "a", Subject: string(long)}); err == nil {
		t.Fatal("超长主题应被拒")
	}
}

// 邮箱文件不存在时是空邮箱，不是错误。
func TestEmptyBoxIsNotError(t *testing.T) {
	box := mailbox.Open(t.TempDir(), "never-received")
	if unread, senders := box.Unread(); unread != 0 || len(senders) != 0 {
		t.Fatalf("应是空：%d %v", unread, senders)
	}
	if box.Total() != 0 {
		t.Fatal("应 0 封")
	}
}

// ID 在本邮箱内不能撞。
func TestIDsUnique(t *testing.T) {
	box := newBox(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		mail, err := box.Add(mailbox.Mail{From: "a", Subject: "s", At: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if seen[mail.ID] {
			t.Fatalf("ID 重复：%s", mail.ID)
		}
		seen[mail.ID] = true
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
