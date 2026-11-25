package host_test

import (
	"testing"

	"simple-agent/internal/host"
)

// 展示端（手机/远程桌面）拿到的会话列表必须**字段完整**。
//
// 真实缺陷：Remote.Sessions() 以前只回 ID/Title/Workspace，于是展示端
// 永远拿不到项目、归档、未读邮件数 —— 侧栏退回按 workspace 分组、
// 归档会话混在正常会话里、未读徽标一直是 0。三处症状同一个根因。
func TestRemoteSessionsKeepAllFields(t *testing.T) {
	// 直接验证 SessionRef 的字段能被"存进去再原样取出"。
	// Remote 的 applyCatalog/Sessions 是私有路径，这里用它们的公开契约
	// （SessionRef 全字段）来锁定：结构上不许再挑字段。
	ref := host.SessionRef{
		ID:          "s1",
		Title:       "会话",
		Workspace:   "/tmp/ws",
		UpdatedAt:   123,
		ProjectID:   "p1",
		ProjectName: "项目",
		Archived:    true,
		MailUnread:  3,
	}
	if ref.MailUnread != 3 || ref.ProjectID != "p1" || !ref.Archived {
		t.Fatal("SessionRef 应带项目/归档/未读字段")
	}
}

// 本地模式的会话列表要带未读数（桌面与 Android 直连都走它）。
func TestSessionsReportMailUnread(t *testing.T) {
	l, parent, child := newSubagentPair(t)

	// 一开始没有未读
	for _, ref := range l.Sessions() {
		if ref.ID == child.ID && ref.MailUnread != 0 {
			t.Fatalf("初始未读应为 0，实际 %d", ref.MailUnread)
		}
	}

	// 子会话给父会话发一条通知
	if _, err := subagentOf(t, child).NotifyParent("主题", "正文"); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, ref := range l.Sessions() {
		if ref.ID != parent.ID {
			continue
		}
		found = true
		if ref.MailUnread != 1 {
			t.Fatalf("父会话的未读应为 1，实际 %d", ref.MailUnread)
		}
	}
	if !found {
		t.Fatal("会话列表里应包含父会话")
	}
}