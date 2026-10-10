package jsonl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/storage"
)

// testStore returns a store over a fresh data root.
func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "data")
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return store, root
}

// commit appends records to a session through the store so each write
// takes the real paths (lock, log open, segment switch on compaction).
func commit(t *testing.T, session storage.Session, records ...storage.Record) {
	t.Helper()
	for _, record := range records {
		if _, err := session.Append(record); err != nil {
			t.Fatalf("append %s: %v", record.Type, err)
		}
	}
}

func headerRecord(ts string) storage.Record {
	return storage.Record{ItemID: "h1", Type: storage.TypeSessionCreated, Time: ts,
		Provider: "p", Model: "m", CWD: "F:/work/x"}
}

func userInput(id, ts, content string) storage.Record {
	return storage.Record{ItemID: id, Type: storage.TypeUserInput, Time: ts, Content: content}
}

func TestListSessionsBasics(t *testing.T) {
	store, root := testStore(t)
	project := ProjectID(t.TempDir()) // any valid project id
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session,
		headerRecord("2026-10-10T01:00:00Z"),
		userInput("u1", "2026-10-10T01:00:01Z", "fix the login bug"),
		storage.Record{ItemID: "a1", Type: storage.TypeAssistantMessage, Time: "2026-10-10T01:00:05Z", Content: "ok"})
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	infos, err := store.ListSessions(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("sessions = %d, want 1", len(infos))
	}
	info := infos[0]
	if info.ID != session.ID() {
		t.Errorf("id = %s, want %s", info.ID, session.ID())
	}
	if info.Title != "fix the login bug" {
		t.Errorf("title = %q", info.Title)
	}
	created := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	if !info.Created.Equal(created) {
		t.Errorf("created = %v, want %v", info.Created, created)
	}

	// A different project sees nothing; an invalid project id errors.
	other := ProjectID(t.TempDir())
	infos, err = store.ListSessions(other)
	if err != nil || len(infos) != 0 {
		t.Fatalf("other project: %v %d", err, len(infos))
	}
	if _, err := store.ListSessions("not-a-project"); err == nil {
		t.Fatal("invalid project id accepted")
	}
	_ = root
}

// The original first input stays in the immutable archive after a
// compaction; the listing must keep reporting it (and the ORIGINAL
// creation time, not the compaction time).
func TestListSessionsAfterCompaction(t *testing.T) {
	store, _ := testStore(t)
	project := ProjectID(t.TempDir())
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session,
		headerRecord("2026-10-10T01:00:00Z"),
		userInput("u1", "2026-10-10T01:00:01Z", "THE ORIGINAL FIRST INPUT"),
		storage.Record{ItemID: "a1", Type: storage.TypeAssistantMessage, Time: "2026-10-10T01:00:05Z", Content: "hi"},
		storage.Record{ItemID: "x1", Type: storage.TypeContextCompacted, Time: "2026-10-10T02:00:00Z", Content: "SUMMARY"},
		userInput("u2", "2026-10-10T02:00:10Z", "POST COMPACTION INPUT"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	infos, err := store.ListSessions(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("sessions = %d, want 1", len(infos))
	}
	if infos[0].Title != "THE ORIGINAL FIRST INPUT" {
		t.Errorf("title = %q, want the archived original", infos[0].Title)
	}
	created := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	if !infos[0].Created.Equal(created) {
		t.Errorf("created = %v, want original %v", infos[0].Created, created)
	}
}

// A session whose records start as session.created+context.compacted has
// its start off-disk (archives cleaned): the title stays empty rather
// than posing a mid-conversation input as the session start.
func TestListSessionsOffDiskStart(t *testing.T) {
	store, _ := testStore(t)
	project := ProjectID(t.TempDir())
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session,
		headerRecord("2026-10-10T01:00:00Z"),
		storage.Record{ItemID: "x1", Type: storage.TypeContextCompacted, Time: "2026-10-10T02:00:00Z", Content: "SUMMARY"},
		userInput("u2", "2026-10-10T02:00:10Z", "MID CONVERSATION INPUT"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	// Drop the archive so the earliest segment is the compaction product.
	dir := session.(*Session).Dir()
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "log-") {
			if err := os.Remove(filepath.Join(dir, file.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}

	infos, err := store.ListSessions(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("sessions = %d, want 1", len(infos))
	}
	if infos[0].Title != "" {
		t.Errorf("title = %q, want empty (start off-disk)", infos[0].Title)
	}
	created := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	if !infos[0].Created.Equal(created) {
		t.Errorf("created = %v, want %v (header copy keeps the original ts)", infos[0].Created, created)
	}
}

// A concurrent writer may be mid-append into the tail: the listing skips
// the unparseable line instead of failing.
func TestListSessionsToleratesTornTail(t *testing.T) {
	store, _ := testStore(t)
	project := ProjectID(t.TempDir())
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session,
		headerRecord("2026-10-10T01:00:00Z"),
		userInput("u1", "2026-10-10T01:00:01Z", "intact"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a torn append: a partial line without the newline.
	dir := session.(*Session).Dir()
	file, err := os.OpenFile(filepath.Join(dir, logFileName), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"item":"a1","t":"assistant.mess`); err != nil {
		t.Fatal(err)
	}
	file.Close()

	infos, err := store.ListSessions(project)
	if err != nil {
		t.Fatalf("listing failed on torn tail: %v", err)
	}
	if len(infos) != 1 || infos[0].Title != "intact" {
		t.Fatalf("infos = %+v", infos)
	}
}

// Unmaterialized sessions (no log) hold nothing and stay unlisted.
func TestListSessionsSkipsUnmaterialized(t *testing.T) {
	store, _ := testStore(t)
	project := ProjectID(t.TempDir())
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	infos, err := store.ListSessions(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Fatalf("sessions = %d, want 0 (never materialized)", len(infos))
	}
}

func TestOpenInScopesToProject(t *testing.T) {
	store, _ := testStore(t)
	projectA := ProjectID(t.TempDir())
	projectB := ProjectID(t.TempDir())
	session, err := store.Create(projectA)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session, headerRecord("2026-10-10T01:00:00Z"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	// Same project opens; a different project cannot see it.
	opened, err := store.OpenIn(projectA, session.ID())
	if err != nil {
		t.Fatalf("open in owning project: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenIn(projectB, session.ID()); err == nil {
		t.Fatal("open in foreign project succeeded")
	}
	if _, err := store.OpenIn("not-a-project", session.ID()); err == nil {
		t.Fatal("invalid project id accepted")
	}
}

// Copied session directories produce the same id under two projects:
// Open (the unscoped scan) refuses instead of picking silently.
func TestOpenRefusesDuplicateSessionDirs(t *testing.T) {
	store, _ := testStore(t)
	projectA := ProjectID(t.TempDir())
	projectB := ProjectID(t.TempDir())
	session, err := store.Create(projectA)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session, headerRecord("2026-10-10T01:00:00Z"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	dir := session.(*Session).Dir()
	if err := copyDir(dir, filepath.Join(store.Root(), projectB, session.ID())); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(session.ID()); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("open duplicated id: %v, want multiple-hits error", err)
	}
	// The scoped open is unaffected: one dir per project.
	scoped, err := store.OpenIn(projectA, session.ID())
	if err != nil {
		t.Fatalf("scoped open: %v", err)
	}
	if err := scoped.Close(); err != nil {
		t.Fatal(err)
	}
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
