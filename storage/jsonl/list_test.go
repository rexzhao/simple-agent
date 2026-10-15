package jsonl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/storage"
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
	project := testNS(t)
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
	other := testNS(t)
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
//
// This is also the C1 "A-form reproduction" (§17.1): the compaction
// goes through the REAL segment-switch path (session.Append of
// context.compacted triggers the jsonl archive+switch), so the
// earliest segment is the archive holding [created, input] — and the
// title survives. The records[1]==compacted early return is
// UNREACHABLE in this shape (the active segment opens with a header
// copy). Expected green; a red here means the segment switch or the
// head scan regressed.
func TestListSessionsAfterCompaction(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
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
	project := testNS(t)
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
	project := testNS(t)
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
	project := testNS(t)
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

func TestOpenScopesToNamespace(t *testing.T) {
	store, _ := testStore(t)
	projectA := testNS(t)
	projectB := testNS(t)
	session, err := store.Create(projectA)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session, headerRecord("2026-10-10T01:00:00Z"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	// Same namespace opens; a different one cannot see it.
	opened, err := store.Open(projectA, session.ID())
	if err != nil {
		t.Fatalf("open in owning namespace: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(projectB, session.ID()); err == nil {
		t.Fatal("open in foreign namespace succeeded")
	}
	if _, err := store.Open("not-a-project", session.ID()); err == nil {
		t.Fatal("invalid namespace accepted")
	}
}

// Q1=B regression (§15.1): a wrong-namespace open must be a clean miss
// that never touches the session's lock — the lock and holder files
// belong to the owning namespace's directory alone.
func TestWrongNamespaceOpenDoesNotTouchLock(t *testing.T) {
	store, _ := testStore(t)
	projectA := testNS(t)
	projectB := testNS(t)
	session, err := store.Create(projectA)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session, headerRecord("2026-10-10T01:00:00Z"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	dir := session.(*Session).Dir()
	lockPath := filepath.Join(dir, "lock")
	holderPath := filepath.Join(dir, "holder")
	// The closed session left the lock file behind (Release removes the
	// holder; the lock file itself persists). Stat it, then DELETE it:
	// merely re-opening an existing lock never changes its mtime, so a
	// "took the lock and released it" implementation would slip past
	// an mtime check. An absent lock file is the reliable probe —
	// lockSession opens with O_CREATE, so any acquisition (even one
	// released immediately) recreates the file.
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file: %v", err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove lock for the probe: %v", err)
	}

	_, err = store.Open(projectB, session.ID())
	if err == nil || !errors.Is(err, storage.ErrSessionNotFound) {
		t.Fatalf("wrong-ns open: %v, want ErrSessionNotFound", err)
	}

	// The lock file must NOT have been recreated: a wrong-ns open that
	// acquired the foreign lock (even transiently, releasing it before
	// returning not-found) leaves the O_CREATE artifact behind.
	if _, err := os.Stat(lockPath); err == nil {
		t.Fatal("the lock file was recreated — the wrong-ns open acquired (and released) the foreign session lock")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat lock: %v", err)
	}
	// No holder file either.
	if _, err := os.Stat(holderPath); err == nil {
		t.Fatal("a holder file appeared in the owning directory — the wrong-ns open took the session lock")
	}
	// And nothing leaked into the wrong namespace either.
	if _, err := os.Stat(filepath.Join(store.Root(), projectB, session.ID())); err == nil {
		t.Fatal("the wrong-ns open created a directory in the foreign namespace")
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

// The title is the FIRST user input, not the last.
func TestListSessionsTitleIsFirstInput(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session,
		headerRecord("2026-10-10T01:00:00Z"),
		userInput("u1", "2026-10-10T01:00:01Z", "FIRST INPUT"),
		storage.Record{ItemID: "a1", Type: storage.TypeAssistantMessage, Time: "2026-10-10T01:00:05Z", Content: "ok"},
		userInput("u2", "2026-10-10T01:01:00Z", "SECOND INPUT"),
		userInput("u3", "2026-10-10T01:02:00Z", "THIRD INPUT"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	infos, err := store.ListSessions(project)
	if err != nil || len(infos) != 1 {
		t.Fatalf("list: %v %d", err, len(infos))
	}
	if infos[0].Title != "FIRST INPUT" {
		t.Fatalf("title = %q, want the first input", infos[0].Title)
	}
}

// A headerless start ([context.compacted, user.input], archives gone) has
// no title — a mid-conversation input must not pose as the session start.
func TestListSessionsHeaderlessStart(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	dir := filepath.Join(store.Root(), project, "s000038940123456789abcdefgh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"item":"x1","t":"context.compacted","ts":"2026-10-10T02:00:00Z","content":"SUMMARY"}
{"item":"u2","t":"user.input","ts":"2026-10-10T02:00:10Z","content":"MID CONVERSATION INPUT"}
`
	if err := os.WriteFile(filepath.Join(dir, logFileName), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	infos, err := store.ListSessions(project)
	if err != nil || len(infos) != 1 {
		t.Fatalf("list: %v %d", err, len(infos))
	}
	if infos[0].Title != "" {
		t.Fatalf("title = %q, want empty (headerless start)", infos[0].Title)
	}
}

// A segment switch interrupted between the two renames (archive exists,
// staging ready, active log not yet activated) must not make the session
// vanish from the listing: the archive is a readable segment.
func TestListSessionsMidSwitchCrashStillListed(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session,
		headerRecord("2026-10-10T01:00:00Z"),
		userInput("u1", "2026-10-10T01:00:01Z", "before compaction"),
		storage.Record{ItemID: "x1", Type: storage.TypeContextCompacted, Time: "2026-10-10T02:00:00Z", Content: "SUMMARY"})
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	dir := session.(*Session).Dir()
	// Simulate the crash: the active log is gone, the staged next segment
	// sits beside the archive.
	active := filepath.Join(dir, logFileName)
	next := filepath.Join(dir, logSwitchStaging)
	if err := os.Rename(active, next); err != nil {
		t.Fatal(err)
	}
	infos, err := store.ListSessions(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("sessions = %d, want 1 (archive is a segment)", len(infos))
	}
	if infos[0].Title != "before compaction" {
		t.Fatalf("title = %q, want the archived original", infos[0].Title)
	}
}

// Merely opening a session (lock/holder bookkeeping) must not change its
// listed Modified — that field is content activity.
func TestListSessionsOpenDoesNotBumpModified(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session,
		headerRecord("2026-10-10T01:00:00Z"),
		userInput("u1", "2026-10-10T01:00:01Z", "hello"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListSessions(project)
	if err != nil || len(before) != 1 {
		t.Fatalf("list: %v %d", err, len(before))
	}
	// Open (rewrites holder) and close; the mtime of the content must
	// not move. Force a visible gap so an equal-filesystem-timestamp
	// flake cannot mask a regression.
	time.Sleep(1100 * time.Millisecond)
	opened, err := store.Open(project, session.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := store.ListSessions(project)
	if err != nil || len(after) != 1 {
		t.Fatalf("list: %v %d", err, len(after))
	}
	if !after[0].Modified.Equal(before[0].Modified) {
		t.Fatalf("Modified moved on open: %v -> %v", before[0].Modified, after[0].Modified)
	}
}

// One unreadable session directory must not blank the whole project
// listing (a listing is a view; opening is where corruption errors).
func TestListSessionsSkipsUnreadableSession(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session,
		headerRecord("2026-10-10T01:00:00Z"),
		userInput("u1", "2026-10-10T01:00:01Z", "good session"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	// A session dir whose "segment" is a directory: unreadable as a log.
	bad := filepath.Join(store.Root(), project, "s00003894unreadable01")
	if err := os.MkdirAll(filepath.Join(bad, logFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	infos, err := store.ListSessions(project)
	if err != nil {
		t.Fatalf("listing failed on one bad dir: %v", err)
	}
	if len(infos) != 1 || infos[0].Title != "good session" {
		t.Fatalf("infos = %+v, want only the good session", infos)
	}
}

// Delete removes the session directory; a held lock refuses it; a
// missing session is not-found.
func TestStoreDelete(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session,
		headerRecord("2026-10-10T01:00:00Z"),
		userInput("u1", "2026-10-10T01:00:01Z", "doomed"))
	id := session.ID()
	dir := session.(*Session).Dir()

	// Held by this handle: deletion refuses (the lock is the guard).
	if err := store.Delete(project, id); err == nil {
		t.Fatal("delete under a live session handle succeeded")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	// Released: deletion removes the directory.
	if err := store.Delete(project, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir still exists: %v", err)
	}
	if store.HasSession(project, id) {
		t.Fatal("HasSession after delete")
	}

	// Gone: not-found, not an error about directories.
	if err := store.Delete(project, id); !errors.Is(err, storage.ErrSessionNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// Foreign project: not-found (the scoped gate).
	if err := store.Delete(testNS(t), id); !errors.Is(err, storage.ErrSessionNotFound) {
		t.Fatalf("foreign delete: %v", err)
	}
}

// testNS derives a valid namespace the way the host does (§15.1): the
// projects package owns path→ns derivation. (The C8 detour — a local
// base32 copy because projects imported jsonl for the registry lock —
// is gone: the lock primitive lives in storage/lockfile now, and
// projects no longer imports jsonl, so the import is cycle-free
// again.)
func testNS(t *testing.T) string {
	t.Helper()
	return projects.ProjectID(t.TempDir())
}
