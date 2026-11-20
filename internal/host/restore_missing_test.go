package host_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple-agent/internal/store"
)

// 档案里有"目录不存在"的记录时，启动**不该报恢复失败**。
//
// 真实踩到的：移动端建了 4 个空会话（登记了但没落盘），重启桌面端
// 弹出 "4 session(s) failed to restore (first: ... The log is empty)"。
// 那些会话本来就没内容，用户看到这个提示只会困惑。
//
// 两道防线：
//   1. PruneMissing 启动时清掉这些记录
//   2. 真遇上了（竞态/手工删目录）静默跳过，不计入失败数
func TestRestoreSkipsMissingSessionDirs(t *testing.T) {
	catalog := store.NewCatalog(t.TempDir())

	// 造一条"只登记、没目录"的记录
	ghostDir, err := catalog.SessionDirFor("ghost-session")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.AddSession("ghost-session", "空会话", `F:\work\alpha`, ghostDir); err != nil {
		t.Fatal(err)
	}

	l := newHostWithCatalog(t, catalog)
	// 触发一次恢复：新加的 Local 构造时就会跑 restoreSessions
	// —— 若真报了错，startupErr 里会有"failed to restore"
	if msg := l.StartupError(); msg != "" {
		t.Fatalf("不该有启动错误，实际：%s", msg)
	}

	// 记录也该被清掉了
	if _, found := catalog.SessionArchived("ghost-session"); found {
		t.Fatal("目录不存在的记录应已被 PruneMissing 清掉")
	}
	// 侧栏里也没有它
	for _, ref := range l.Sessions() {
		if strings.Contains(ref.Title, "空会话") {
			t.Fatalf("不该出现空会话：%+v", ref)
		}
	}
}

// 日志为**空文件**的会话（目录在、session.jsonl 是 0 字节）：
// 启动时静默清理，不报"failed to restore"。
//
// 真实踩到的：手机端建了几个会话没发消息，桌面重启弹出
// "4 session(s) failed to restore (first: ... The log is empty)"。
//
// 那些目录是 openSession 自己建的（Open 里 MkdirAll + openCurrent），
// 所以光靠"目录不存在就跳过"拦不住 —— 必须识别出空日志并清掉。
func TestRestoreSilentlyDropsEmptyLogs(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)

	// 造一个"目录在、日志 0 字节"的会话，并登记进档案
	emptyDir := filepath.Join(dataDir, "sessions", "empty-1")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(emptyDir, "session.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := catalog.AddSession("empty-1", "空会话", `F:\work\alpha`, emptyDir); err != nil {
		t.Fatal(err)
	}

	l := newHostWithCatalog(t, catalog)
	if msg := l.StartupError(); msg != "" {
		t.Fatalf("空日志的会话不该计入恢复失败，实际提示：%s", msg)
	}

	// 记录应被移除，目录也应清掉
	if _, found := catalog.SessionArchived("empty-1"); found {
		t.Fatal("空日志会话的记录应已移除")
	}
	if _, statErr := os.Stat(emptyDir); statErr == nil {
		t.Fatal("空日志会话的目录应已删除")
	}
}