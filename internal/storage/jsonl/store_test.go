package jsonl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/storage"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return store
}

func TestLazyCreateTouchesNoDisk(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(ProjectID(t.TempDir()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if session.ID() == "" {
		t.Fatal("session id is empty before materialization")
	}
	if _, err := os.Stat(session.(*Session).Dir()); !os.IsNotExist(err) {
		t.Fatalf("lazy session left a directory behind: %v", err)
	}
	records, err := session.Records()
	if err != nil || len(records) != 0 {
		t.Fatalf("Records() = %v, %v; want empty", records, err)
	}
}

func TestFirstAppendOpensLogInOrder(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(ProjectID(t.TempDir()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The state layer commits session.created as the first record; the
	// store just stores what it is given, in order.
	if _, err := session.Append(storage.Record{ItemID: "h1", Type: storage.TypeSessionCreated, Provider: "p", CWD: "/w"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := session.Append(storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(filepath.Join(session.(*Session).Dir(), logFileName)); err != nil {
		t.Fatalf("first append did not open the log: %v", err)
	}
	records, err := session.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].Type != storage.TypeSessionCreated || records[0].Provider != "p" || records[0].CWD != "/w" {
		t.Fatalf("session.created = %+v", records[0])
	}
	if records[1].Type != storage.TypeUserInput || records[1].ItemID != "u1" {
		t.Fatalf("user.input = %+v", records[1])
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOpenRebuildsRecordsAcrossProjects(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(ProjectID(t.TempDir()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := session.Append(storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "persisted"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	records, err := reopened.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) != 1 || records[0].Content != "persisted" {
		t.Fatalf("reopened records = %+v", records)
	}
	// The reopened session knows its directory (the store field is set).
	if dir := reopened.(*Session).Dir(); !strings.Contains(dir, id) {
		t.Fatalf("Dir() = %q, want it to contain the session id", dir)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOpenUnknownSessionFails(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Open("s-does-not-exist"); err == nil {
		t.Fatal("Open of unknown session succeeded")
	}
}

func TestTornTailRecovery(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(ProjectID(t.TempDir()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := session.Append(storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "one"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a crash mid-append: a final line without its newline.
	path := filepath.Join(session.(*Session).Dir(), logFileName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	if _, err := file.WriteString(`{"item":"torn`); err != nil {
		t.Fatalf("write torn tail: %v", err)
	}
	file.Close()

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("Open after torn tail: %v", err)
	}
	records, err := reopened.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want torn tail discarded", len(records))
	}
	// The next append extends the recovered log.
	if _, err := reopened.Append(storage.Record{ItemID: "u2", Type: storage.TypeUserInput, Content: "two"}); err != nil {
		t.Fatalf("Append after recovery: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestProjectIDDeterministicAndPathSafe(t *testing.T) {
	// t.TempDir returns a fresh directory per call, so sample once.
	workspace := t.TempDir()
	first := ProjectID(workspace)
	second := ProjectID(workspace)
	if first != second {
		t.Fatalf("ProjectID not deterministic: %s vs %s", first, second)
	}
	if !ValidProjectID(first) {
		t.Fatalf("ProjectID %q rejected", first)
	}
	if ValidProjectID("nope") || ValidProjectID("") {
		t.Fatal("malformed project ids accepted")
	}
	if strings.ContainsAny(first, `/\`) {
		t.Fatalf("ProjectID %q is not path-safe", first)
	}
}

func TestReadRecordsNamesInvalidLineNumber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	content := "{\"item\":\"u1\",\"t\":\"user.input\",\"ts\":\"2026-01-02T03:04:05Z\"}\n" +
		"\n" +
		"{not json"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if _, err := readRecords(path); err == nil || !strings.Contains(err.Error(), "invalid line at line 3") {
		t.Fatalf("readRecords error = %v, want invalid line at line 3 (blank lines counted)", err)
	}
}

func TestRecoverDiscardWarningNamesLastValidItem(t *testing.T) {
	original := maxTornTailBytes
	maxTornTailBytes = 16
	t.Cleanup(func() { maxTornTailBytes = original })

	store := newTestStore(t)
	session, err := store.Create(ProjectID(t.TempDir()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := session.Append(storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "kept"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A garbage tail far over the (shrunken) torn-tail bound: recovering it
	// must report mid-file corruption and locate the boundary by the last
	// valid item.
	path := filepath.Join(session.(*Session).Dir(), logFileName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	if _, err := file.WriteString("{" + strings.Repeat("x", 200) + "}\n"); err != nil {
		t.Fatalf("write garbage tail: %v", err)
	}
	file.Close()

	var reopened storage.Session
	stderr := captureStderr(t, func() {
		reopened, err = store.Open(id)
	})
	if err != nil {
		t.Fatalf("Open after garbage tail: %v", err)
	}
	if !strings.Contains(stderr, "after item u1 (type user.input)") {
		t.Fatalf("discard warning = %q, want it to name the last valid item", stderr)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// captureStderr runs fn with os.Stderr redirected to a temp file and
// returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("create stderr capture: %v", err)
	}
	original := os.Stderr
	os.Stderr = tmp
	defer func() { os.Stderr = original }()
	fn()
	tmp.Close()
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(data)
}
