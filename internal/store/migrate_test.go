package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple-agent/internal/store"
)

// writeOldFormat 手工构造「快照夹在文件中间」的旧日志（M6 到 8a5b8ce 之间的产物）。
func writeOldFormat(t *testing.T, dir string, snapshots int) []store.Event {
	t.Helper()
	var events []store.Event
	seq := 1
	add := func(kind string, data map[string]any) {
		event := mustEvent(t, kind, data)
		event.Seq = uint64(seq)
		seq++
		events = append(events, event)
	}
	for round := 0; round < snapshots; round++ {
		add(store.TypeMessageAppended, map[string]any{"round": round, "role": "user"})
		add(store.TypeMessageFinalize, map[string]any{"round": round, "role": "agent"})
		add(store.TypeCompactSnapshot, map[string]any{
			"summary": "摘要", "kept_from_seq": 0,
		})
	}
	add(store.TypeMessageAppended, map[string]any{"round": 99, "role": "user"})

	path := filepath.Join(dir, "session.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, event := range events {
		if err := store.AppendEvent(file, event); err != nil {
			t.Fatal(err)
		}
	}
	return events
}

// 旧格式（1 个中间快照）加载后：备份存在、文件被切开、旧消息能翻出来、再加载不重复迁移。
func TestMigrateEmbeddedSnapshotOnce(t *testing.T) {
	dir := t.TempDir()
	writeOldFormat(t, dir, 1)

	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var backups, archives int
	for _, entry := range entries {
		switch {
		case strings.HasPrefix(entry.Name(), "session.pre-migrate-"):
			backups++
		case strings.HasPrefix(entry.Name(), "archive-"):
			archives++
		}
	}
	if backups != 1 {
		t.Fatalf("迁移前应留一份备份，实际 %d", backups)
	}
	if archives != 1 {
		t.Fatalf("1 个中间快照应切出 1 份归档，实际 %d", archives)
	}

	// base 的第一行必须是快照
	first := mustFirstEvent(t, filepath.Join(dir, "session.jsonl"))
	if first.Type != store.TypeCompactSnapshot {
		t.Fatalf("base 第一行应是快照，实际 %s", first.Type)
	}

	// 旧消息能翻出来（1 快照前的 2 条）
	page, err := s.LoadOlder(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("归档应有 2 条，实际 %d", len(page.Events))
	}
	if page.Next != nil {
		t.Fatal("只有一份归档时不该还有下一页")
	}

	// 再打开一次：不再迁移（备份仍然只有 1 份）
	s.Close()
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	entries, _ = os.ReadDir(dir)
	backups, archives = 0, 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "session.pre-migrate-") {
			backups++
		}
		if strings.HasPrefix(entry.Name(), "archive-") {
			archives++
		}
	}
	if backups != 1 || archives != 1 {
		t.Fatalf("重复迁移了：backups=%d archives=%d", backups, archives)
	}
}

// 旧格式（2 个中间快照）：按时间切两份归档，翻页顺序正确（新的在前）。
func TestMigrateEmbeddedSnapshotsTwice(t *testing.T) {
	dir := t.TempDir()
	writeOldFormat(t, dir, 2)

	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	entries, _ := os.ReadDir(dir)
	archives := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "archive-") {
			archives++
		}
	}
	if archives != 2 {
		t.Fatalf("2 个中间快照应切出 2 份归档，实际 %d", archives)
	}

	first, err := s.LoadOlder(nil)
	if err != nil {
		t.Fatal(err)
	}
	// 第 2 份归档以第 1 个快照开头（与真正的 Compact 一致）
	if len(first.Events) != 3 || !(first.Next != nil) {
		t.Fatalf("第一页应有 3 条（含上个快照）且还有更早：%d more=%v", len(first.Events), (first.Next != nil))
	}
	second, err := s.LoadOlder(first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 2 || (second.Next != nil) {
		t.Fatalf("第二页应有 2 条且到底：%d more=%v", len(second.Events), (second.Next != nil))
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


// 中间夹着快照、并且快照之后有一行损坏：必须先修复（留档 + 警告），不能静默丢掉损坏点之后的事件。
func TestMigrateEmbeddedSnapshotWithCorruption(t *testing.T) {
	dir := t.TempDir()
	writeOldFormat(t, dir, 1)
	path := filepath.Join(dir, "session.jsonl")

	// 在文件末尾追加一行坏内容，并把 seq 打乱（模拟损坏）
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 中间坏行：后面**还有**合法行，才算"中间损坏"（末尾半行是另一种修复）
	data = append(data, []byte("{\"v\":1,\"seq\":99,\"type\":")...)
	data = append(data, []byte("\n{\"v\":1,\"seq\":100,\"type\":\"message.appended\",\"data\":{}}\n")...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 至少有一条警告（告诉用户文件被修过），并且没有静默丢弃可读事件
	loaded, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Warnings) == 0 {
		t.Fatal("损坏被静默处理了：应该有警告")
	}
	entries, _ := os.ReadDir(dir)
	corrupt := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "session.corrupt-") {
			corrupt = true
		}
	}
	if !corrupt {
		t.Fatal("损坏的文件应留档为 session.corrupt-*.jsonl")
	}

	// base + 归档里的事件总数不能少（修复 + 迁移会把它们分到两处）
	after, err := store.ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	total := len(after)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "archive-") {
			events, readErr := store.ReadEvents(filepath.Join(dir, entry.Name()))
			if readErr != nil {
				t.Fatal(readErr)
			}
			total += len(events)
		}
	}
	if total < len(before) {
		t.Fatalf("修复后事件变少了：%d → %d（base=%d）", len(before), total, len(after))
	}
}
