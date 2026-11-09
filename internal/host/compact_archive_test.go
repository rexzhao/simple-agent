package host_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
)

// 单机模式回归：压缩一次之后磁盘上必须真的生成 archive-0001.jsonl，
// 并且 session.jsonl 的第一行就是快照（不再把快照夹在文件中间）。
func TestCompactWritesArchiveFile(t *testing.T) {
	pair := newP2Pair(t, true)
	sess := pair.sessID

	streamIndex := 0
	round := func(question, answer string) {
		t.Helper()
		streamIndex++
		pair.send(t, question)
		live := streamAt(t, pair.rt, pair.post, streamIndex)
		live <- providers.Event{Kind: providers.EventText, Text: answer}
		live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
		pair.pumpUntil(t, func() bool {
			session := pair.local.Session(sess)
			return session != nil && session.Busy() == driver.BusyIdle
		})
	}
	round("第一问", "第一答")
	round("第二问", "第二答")

	streamIndex++
	if _, err := pair.local.Submit(host.Command{SessionID: sess, Kind: "compact"}); err != nil {
		t.Fatal(err)
	}
	summary := streamAt(t, pair.rt, pair.post, streamIndex)
	summary <- providers.Event{Kind: providers.EventText, Text: "摘要"}
	close(summary)
	time.Sleep(50 * time.Millisecond)
	t.Logf("summary 里还剩 %d 条（未被读取说明宿主没在 range）", len(summary))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pair.post.pump()
		session := pair.local.Session(sess)
		if session.Busy() == driver.BusyIdle && !session.Compacting() {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if s := pair.local.Session(sess); s.Busy() != driver.BusyIdle || s.Compacting() {
		t.Fatalf("压缩没结束：busy=%v compacting=%v streams=%d", s.Busy(), s.Compacting(), pair.rt.count())
	}

	catalog := pair.local.CatalogForTest()
	if catalog == nil {
		t.Fatal("测试需要 Catalog")
	}
	dir := filepath.Join(catalog.SessionsDir(), sess)
	if _, err := os.Stat(filepath.Join(dir, "archive-0001.jsonl")); err != nil {
		t.Fatalf("压缩后应生成 archive-0001.jsonl：%v", err)
	}
	first := mustFirstEvent(t, filepath.Join(dir, "session.jsonl"))
	if first.Type != store.TypeCompactSnapshot {
		t.Fatalf("base 第一行应是快照，实际 %s", first.Type)
	}
	page, err := pair.local.Session(sess).LoadOlderForTest("")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) == 0 {
		t.Fatal("归档里应有压缩前的消息")
	}
}

// 旧格式迁移：把「归档内容塞回 base 中间」还原成 M6 的文件形态，
// 重新加载后应当自动迁移：状态不变、旧消息能翻出来、留备份、不重复迁移。
func TestEmbeddedSnapshotMigrationKeepsState(t *testing.T) {
	pair := newP2Pair(t, true)
	sess := pair.sessID

	streamIndex := 0
	round := func(question, answer string) {
		t.Helper()
		streamIndex++
		pair.send(t, question)
		live := streamAt(t, pair.rt, pair.post, streamIndex)
		live <- providers.Event{Kind: providers.EventText, Text: answer}
		live <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
		pair.pumpUntil(t, func() bool {
			session := pair.local.Session(sess)
			return session != nil && session.Busy() == driver.BusyIdle
		})
	}
	round("第一问", "第一答")
	round("第二问", "第二答")

	streamIndex++
	if _, err := pair.local.Submit(host.Command{SessionID: sess, Kind: "compact"}); err != nil {
		t.Fatal(err)
	}
	summary := streamAt(t, pair.rt, pair.post, streamIndex)
	summary <- providers.Event{Kind: providers.EventText, Text: "摘要"}
	close(summary)
	time.Sleep(50 * time.Millisecond)
	t.Logf("summary 里还剩 %d 条（未被读取说明宿主没在 range）", len(summary))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pair.post.pump()
		session := pair.local.Session(sess)
		if session.Busy() == driver.BusyIdle && !session.Compacting() {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if s := pair.local.Session(sess); s.Busy() != driver.BusyIdle || s.Compacting() {
		t.Fatalf("压缩没结束：busy=%v compacting=%v streams=%d", s.Busy(), s.Compacting(), pair.rt.count())
	}

	catalog := pair.local.CatalogForTest()
	dir := filepath.Join(catalog.SessionsDir(), sess)
	want := digestOf(t, pair.local.Session(sess))
	_ = pair.local.Close()

	// 还原成旧格式：archives 的事件塞回 base 中间
	archivePath := filepath.Join(dir, "archive-0001.jsonl")
	basePath := filepath.Join(dir, "session.jsonl")
	// 还原成旧格式：归档事件塞回 base 中间，并按"一条条追加"重排 seq
	archiveEvents := mustReadEvents(t, archivePath)
	baseEvents := mustReadEvents(t, basePath)
	legacy := store.Renumber(append(archiveEvents, baseEvents...))
	if err := os.Remove(archivePath); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteEvents(basePath, legacy); err != nil {
		t.Fatal(err)
	}

	// 重新加载：迁移发生
	catalog2 := store.NewCatalog(filepath.Dir(catalog.SessionsDir()))
	local2 := host.NewLocal(host.Config{
		Catalog: catalog2, Runtime: newP2Runtime(),
		Selection:    providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: (&poster{}).Post,
	})
	t.Cleanup(func() { _ = local2.Close() })
	var restored *host.Session
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && restored == nil {
		for _, ref := range local2.Sessions() {
			if ref.ID == sess {
				restored = local2.Session(ref.ID)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if restored == nil {
		t.Fatal("迁移后会话没有恢复")
	}
	if got := digestOf(t, restored); got != want {
		t.Fatalf("迁移前后状态不一致：\n迁移前=%s\n迁移后=%s", want, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "archive-0001.jsonl")); err != nil {
		t.Fatalf("迁移后应重新生成归档：%v", err)
	}
	backups := 0
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if len(entry.Name()) > 20 && entry.Name()[:20] == "session.pre-migrate-" {
			backups++
		}
	}
	if backups != 1 {
		t.Fatalf("应留 1 份备份，实际 %d", backups)
	}
	page, err := restored.LoadOlderForTest("")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) == 0 {
		t.Fatal("迁移后归档里应有压缩前的消息")
	}
}

func mustFirstEvent(t *testing.T, path string) store.Event {
	t.Helper()
	event, err := store.FirstEvent(path)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func mustReadEvents(t *testing.T, path string) []store.Event {
	t.Helper()
	events, err := store.ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	return events
}
