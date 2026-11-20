package host_test

import (
	"testing"
	"time"

	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/message"

	"simple-agent/internal/store"
)

// 新建会话后**立刻发消息**（手机端的典型操作）：应能正常落盘并开流。
//
// 这是 NewSession 立刻登记之后的回归测试：登记会让会话进入项目档案，
// 而 ensurePersisted 里 Catalog.Create → Open 要靠档案找路径。
// 两者配合不好时，表现为"点发送没反应"（命令失败但界面只显示空提示）。
func TestNewSessionThenSend(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())
	l := newHostWithCatalog(t, catalog)

	session := l.NewSession(`F:\work\alpha`)

	// 立刻发送（不经过任何"等待会话落盘"的步骤）
	send(t, l, session.ID, "第一条消息")
	// 假模型的流不会自己结束，等它进入 streaming 就够说明"命令被接受了"
	waitBusy(t, l, session.ID)

	// 应该真的产生了内容：一条 user + 一条 agent
	entries := l.Session(session.ID).Conversation().Entries()
	if len(entries) < 2 {
		t.Fatalf("发送后应有 user 与 agent 两条消息，实际 entries=%d", len(entries))
	}
	hasUser := false
	for _, entry := range entries {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		if entry.Message.Message.Role == message.RoleUser {
			hasUser = true
		}
	}
	if !hasUser {
		t.Fatal("应有一条用户消息")
	}

	// 会话应已落盘
	dir, err := catalog.SessionDirFor(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("会话目录 = %s", dir)

	// 档案里应记录着它
	projects, err := catalog.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range projects {
		total += len(p.Sessions)
	}
	if total != 1 {
		t.Fatalf("档案里应有 1 个会话，实际 %d（projects=%+v）", total, projects)
	}
}

// waitBusy 等到会话进入 streaming（说明 send 命令被接受、流已打开）。
func waitBusy(t *testing.T, l *host.Local, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := l.Session(sessionID); s != nil && s.Busy() == driver.BusyStreaming {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	busy := driver.BusyIdle
	if s := l.Session(sessionID); s != nil {
		busy = s.Busy()
	}
	t.Fatalf("会话没有进入 streaming（实际 %v）：send 命令可能被拒绝了", busy)
}