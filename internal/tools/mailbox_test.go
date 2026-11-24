package tools

import (
	"strings"
	"testing"
)

// 收件人解析：先精确 ID，再标题关键词，多个命中要求模型重选。
//
// 这是整块功能里最容易出错的地方（发错人比发不出去更糟），
// 所以把规则逐条钉住。
func TestResolveRecipient(t *testing.T) {
	peers := []MailboxPeer{
		{ID: "aaa111", Title: "修复特效"},
		{ID: "bbb222", Title: "修复着色器"},
		{ID: "ccc333", Title: "重构网络层"},
	}

	t.Run("精确 ID", func(t *testing.T) {
		got, candidates, err := resolveRecipient(peers, "bbb222")
		if err != nil || got == nil || got.ID != "bbb222" {
			t.Fatalf("应按 ID 精确命中：%v %v %v", got, candidates, err)
		}
	})

	t.Run("ID 优先于标题", func(t *testing.T) {
		// 有个会话标题里含 "aaa111" 的子串也不该抢过精确 ID
		withTwin := append([]MailboxPeer{{ID: "zzz", Title: "提到 aaa111 的会话"}}, peers...)
		got, _, err := resolveRecipient(withTwin, "aaa111")
		if err != nil || got == nil || got.ID != "aaa111" {
			t.Fatalf("精确 ID 应优先：%v %v", got, err)
		}
	})

	t.Run("标题唯一命中", func(t *testing.T) {
		got, _, err := resolveRecipient(peers, "网络层")
		if err != nil || got == nil || got.ID != "ccc333" {
			t.Fatalf("应按标题命中：%v %v", got, err)
		}
	})

	t.Run("标题多个命中不发送", func(t *testing.T) {
		got, candidates, err := resolveRecipient(peers, "修复")
		if err != nil {
			t.Fatalf("多个命中不该是错误（由调用方给候选）：%v", err)
		}
		if got != nil {
			t.Fatalf("多个命中时不该给出目标：%v", got)
		}
		if len(candidates) != 2 {
			t.Fatalf("应给出 2 个候选，实际 %d", len(candidates))
		}
	})

	t.Run("大小写不敏感", func(t *testing.T) {
		got, _, err := resolveRecipient(peers, "AAA111")
		if err != nil || got == nil || got.ID != "aaa111" {
			t.Fatalf("ID 匹配应忽略大小写：%v %v", got, err)
		}
	})

	t.Run("没命中给出可发清单", func(t *testing.T) {
		_, _, err := resolveRecipient(peers, "不存在的会话")
		if err == nil {
			t.Fatal("没命中应报错")
		}
		if !strings.Contains(err.Error(), "ccc333") {
			t.Fatalf("错误里应列出可发会话：%v", err)
		}
	})

	t.Run("空收件人", func(t *testing.T) {
		if _, _, err := resolveRecipient(peers, "  "); err == nil {
			t.Fatal("空收件人应报错")
		}
	})

	t.Run("没有其它会话", func(t *testing.T) {
		_, _, err := resolveRecipient(nil, "任何人")
		if err == nil || !strings.Contains(err.Error(), "没有可收信的会话") {
			t.Fatalf("应说明没有可发的会话：%v", err)
		}
		// 提示要说清**为什么**（不能自投、归档会话不能收信），
		// 否则用户会以为功能坏了。
		if !strings.Contains(err.Error(), "未归档") {
			t.Fatalf("应说明归档会话不能收信：%v", err)
		}
	})
}

// 工具在没有接通讯能力时给出明确错误，而不是静默什么都不做。
func TestMailboxToolsWithoutHost(t *testing.T) {
	tc := NewToolContext(t.TempDir(), false)
	// Mailbox 为 nil（例如未接入的测试环境）

	if _, err := mailboxSendRun(t.Context(), Args{
		"to": "x", "subject": "s", "body": "b",
	}, tc); err == nil {
		t.Fatal("没接通讯能力时应报错")
	}
	if _, err := mailboxListRun(t.Context(), Args{}, tc); err == nil {
		t.Fatal("list 也应报错")
	}
	if _, err := mailboxReadRun(t.Context(), Args{"id": "x"}, tc); err == nil {
		t.Fatal("read 也应报错")
	}
}
