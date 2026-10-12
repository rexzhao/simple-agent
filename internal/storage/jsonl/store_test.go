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
// returns what was written. The redirect (like the maxTornTailBytes
// variable) is a package-global seam: this package's tests must not run
// in parallel while it exists.
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

func appendRecord(t *testing.T, session storage.Session, record storage.Record) storage.Record {
	t.Helper()
	stored, err := session.Append(record)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return stored
}

func TestCompactionSwitchesSegments(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(ProjectID(t.TempDir()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The session holds a range-locked lock file: without Close, Windows
	// cannot even delete the temp dir.
	defer func() { _ = session.Close() }()
	appendRecord(t, session, storage.Record{ItemID: "h1", Type: storage.TypeSessionCreated, Provider: "p", CWD: "/w"})
	appendRecord(t, session, storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "old turn"})
	appendRecord(t, session, storage.Record{ItemID: "a1", Type: storage.TypeAssistantMessage, Content: "old answer"})
	compacted := appendRecord(t, session, storage.Record{ItemID: "x1", Type: storage.TypeContextCompacted, Content: "summary"})
	appendRecord(t, session, storage.Record{ItemID: "u2", Type: storage.TypeUserInput, Content: "post turn"})

	dir := session.(*Session).Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 4 || !contains(names, logFileName) || !contains(names, "log-0000.jsonl") ||
		!contains(names, "lock") || !contains(names, "holder") {
		t.Fatalf("segment files = %v, want log.jsonl + log-0000.jsonl (+ lock/holder)", names)
	}

	// The active segment is self-contained: header copy + compaction +
	// post-compaction records — never the archived history.
	records, err := session.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	wantTypes := []string{storage.TypeSessionCreated, storage.TypeContextCompacted, storage.TypeUserInput}
	if len(records) != len(wantTypes) {
		t.Fatalf("active records = %v, want %v", recordTypes(records), wantTypes)
	}
	for i, want := range wantTypes {
		if records[i].Type != want {
			t.Fatalf("active[%d] = %s, want %s", i, records[i].Type, want)
		}
	}
	// The header copy preserves the original metadata and identity.
	if records[0].ItemID != "h1" || records[0].Provider != "p" {
		t.Fatalf("segment header = %+v, want a verbatim copy of the original", records[0])
	}
	if records[1].ItemID != compacted.ItemID {
		t.Fatalf("compaction item id lost: %q vs %q", records[1].ItemID, compacted.ItemID)
	}

	// The archive holds the pre-compaction history verbatim.
	archive, err := readRecords(filepath.Join(dir, "log-0000.jsonl"))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(archive) != 3 || archive[0].ItemID != "h1" || archive[2].Content != "old answer" {
		t.Fatalf("archive = %v", recordTypes(archive))
	}

	// A second compaction archives again, numbered in order.
	appendRecord(t, session, storage.Record{ItemID: "a2", Type: storage.TypeAssistantMessage, Content: "post answer"})
	appendRecord(t, session, storage.Record{ItemID: "x2", Type: storage.TypeContextCompacted, Content: "summary two"})
	entries, _ = os.ReadDir(dir)
	if len(entries) != 5 { // two archives + active + lock + holder
		t.Fatalf("segment files after second switch = %d, want 5", len(entries))
	}
	records, err = session.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	// The second compaction absorbed the post-compaction turns: the new
	// active segment is again just header + compaction.
	if len(records) != 2 || records[1].Content != "summary two" {
		t.Fatalf("active after second switch = %v, want header + second summary", recordTypes(records))
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestResumeReadsActiveSegmentOnly(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(ProjectID(t.TempDir()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	appendRecord(t, session, storage.Record{ItemID: "h1", Type: storage.TypeSessionCreated, Provider: "p"})
	appendRecord(t, session, storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "old"})
	appendRecord(t, session, storage.Record{ItemID: "x1", Type: storage.TypeContextCompacted, Content: "summary"})
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
	if len(records) != 2 || records[0].Type != storage.TypeSessionCreated || records[1].Content != "summary" {
		t.Fatalf("resumed records = %v, want header + compaction only", recordTypes(records))
	}
	// Appending continues in the active segment.
	appendRecord(t, reopened, storage.Record{ItemID: "u2", Type: storage.TypeUserInput, Content: "next"})
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSegmentSwitchRecoveryWindows(t *testing.T) {
	writeFixture := func(t *testing.T) (store *Store, session storage.Session, dir string) {
		store = newTestStore(t)
		session, err := store.Create(ProjectID(t.TempDir()))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		appendRecord(t, session, storage.Record{ItemID: "h1", Type: storage.TypeSessionCreated})
		appendRecord(t, session, storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "old"})
		dir = session.(*Session).Dir()
		return store, session, dir
	}

	// Window 1: staging exists, active exists — the staged compaction is
	// discarded, the context stays pre-compaction.
	store, session, dir := writeFixture(t)
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, logSwitchStaging), []byte("{\"item\":\"x1\",\"t\":\"context.compacted\"}\n"), 0o644); err != nil {
		t.Fatalf("write staging: %v", err)
	}
	reopened, err := store.Open(session.ID())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, logSwitchStaging)); !os.IsNotExist(err) {
		t.Fatal("stale staging survived recovery")
	}
	records, _ := reopened.Records()
	if len(records) != 2 || records[1].Content != "old" {
		t.Fatalf("window-1 records = %v, want the untouched pre-compaction history", recordTypes(records))
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Window 2: staging exists, active missing — the switch completes.
	store, session, dir = writeFixture(t)
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	staged := "{\"item\":\"h1\",\"t\":\"session.created\"}\n{\"item\":\"x1\",\"t\":\"context.compacted\",\"content\":\"summary\"}\n"
	if err := os.WriteFile(filepath.Join(dir, logSwitchStaging), []byte(staged), 0o644); err != nil {
		t.Fatalf("write staging: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, logFileName), filepath.Join(dir, "log-0000.jsonl")); err != nil {
		t.Fatalf("archive: %v", err)
	}
	reopened, err = store.Open(session.ID())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	records, _ = reopened.Records()
	if len(records) != 2 || records[1].Content != "summary" {
		t.Fatalf("window-2 records = %v, want the adopted staged segment", recordTypes(records))
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Window 3: no active, no staging, archives exist — the newest
	// archive rolls back into place.
	store, session, dir = writeFixture(t)
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, logFileName), filepath.Join(dir, "log-0000.jsonl")); err != nil {
		t.Fatalf("archive: %v", err)
	}
	reopened, err = store.Open(session.ID())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	records, _ = reopened.Records()
	if len(records) != 2 || records[1].Content != "old" {
		t.Fatalf("window-3 records = %v, want the rolled-back archive", recordTypes(records))
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func recordTypes(records []storage.Record) []string {
	types := make([]string, len(records))
	for i, record := range records {
		types[i] = record.Type
	}
	return types
}

// TestAppendNormalizesInvalidUTF8 verifies the jsonl boundary: a record
// with invalid UTF-8 bytes (raw command output) is stored, returned, and
// read back in the normalized form — in-memory and on-disk never diverge.
func TestAppendNormalizesInvalidUTF8(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(ProjectID(t.TempDir()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = session.Close() }()

	stored, err := session.Append(storage.Record{
		ItemID: "r1", Type: storage.TypeToolResult, Content: "out\xff\xfeend",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if stored.Content != "out\ufffdend" { // one replacement per invalid run
		t.Fatalf("returned record not normalized: %q", stored.Content)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := store.Open(session.ID())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	records, err := reopened.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) != 1 || records[0].Content != stored.Content {
		t.Fatalf("read-back diverged: %+v", records)
	}
}

// TestValidSessionIDStrictShape pins the tightened validation: session
// ids are exactly what NewSessionID mints. The Windows hazards ("...",
// "CON", trailing dots) and arbitrary strings are all foreign now.
func TestValidSessionIDStrictShape(t *testing.T) {
	good := storage.NewSessionID()
	if !ValidSessionID(good) {
		t.Fatalf("minted id rejected: %q", good)
	}
	for _, bad := range []string{
		"", ".", "..", "...", "CON", "blobs",
		"s",                                              // too short
		"s" + strings.Repeat("0", 25),                    // one short
		"s" + strings.Repeat("0", 27),                    // one long
		"u" + strings.Repeat("0", 26),                    // wrong kind
		"s" + strings.Repeat("i", 26),                    // i not in the alphabet
		"s" + strings.Repeat("0", 25) + " ",              // whitespace
		strings.Repeat("0", 27),                          // no prefix
		"s0000389560h59pb15q9vcfzpdw\x00",                // control byte
	} {
		if ValidSessionID(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}
