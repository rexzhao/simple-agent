package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendRecord(t *testing.T, session *Session, record Record) Record {
	t.Helper()
	stored, err := session.Append(record)
	if err != nil {
		t.Fatalf("append %s: %v", record.Type, err)
	}
	return stored
}

func TestAppendAndReopenFold(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create("paperhub", "glm-5.2", `F:\work\demo`)
	if err != nil {
		t.Fatal(err)
	}

	appendRecord(t, session, Record{Type: TypeUserInput, RunID: "run-1", Content: "hello world this is a long first message"})
	appendRecord(t, session, Record{Type: TypeAssistantMessage, RunID: "run-1", TurnID: "turn-1", ItemID: "assistant-1", Content: "final answer", Reasoning: "thinking", ToolCalls: []WireToolCall{{ID: "call-1", Name: "read_file", Arguments: `{"path":"a.txt"}`}}})
	appendRecord(t, session, Record{Type: TypeToolResult, RunID: "run-1", TurnID: "turn-1", ToolCallID: "call-1", ToolName: "read_file", Content: "file body"})
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(session.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state := reopened.Snapshot()

	if state.Provider != "paperhub" || state.Model != "glm-5.2" {
		t.Fatalf("session meta not folded: %+v", state)
	}
	if state.Title != "hello world this is a long first message" {
		t.Fatalf("title = %q", state.Title)
	}
	kinds := make([]string, 0, len(state.Items))
	for _, item := range state.Items {
		kinds = append(kinds, item.Kind)
	}
	want := []string{ItemKindUser, ItemKindAssistant, ItemKindTool}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("item kinds = %v, want %v", kinds, want)
	}
	if state.Items[1].Content != "final answer" || state.Items[1].Reasoning != "thinking" {
		t.Fatalf("assistant item = %+v", state.Items[1])
	}
	if len(state.Items[1].ToolCalls) != 1 || state.Items[1].ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls not folded: %+v", state.Items[1].ToolCalls)
	}
	if state.Items[2].ToolCallID != "call-1" || state.Items[2].Status != ToolStatusCompleted {
		t.Fatalf("tool item not folded: %+v", state.Items[2])
	}

	// The model context must mirror the wire order.
	roles := make([]string, 0, len(state.Context))
	for _, message := range state.Context {
		roles = append(roles, string(message.Role))
	}
	if strings.Join(roles, ",") != "user,assistant,tool" {
		t.Fatalf("context roles = %v", roles)
	}
	if state.Context[1].Content != "final answer" {
		t.Fatalf("context assistant content = %q", state.Context[1].Content)
	}
}

func TestTornTailRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFileName)
	session, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	log := session
	for i := 0; i < 3; i++ {
		if _, err := log.Append(Record{Type: TypeUserInput, RunID: "r", Content: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash mid-append: a partial line without its newline.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"seq":4,"t":"user.input","ts":"2025`); err != nil {
		t.Fatal(err)
	}
	file.Close()

	recovered, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.LastSeq() != 3 {
		t.Fatalf("last seq = %d, want 3", recovered.LastSeq())
	}
	if _, err := recovered.Append(Record{Type: TypeUserInput, Content: "after recovery"}); err != nil {
		t.Fatal(err)
	}
	records, err := recovered.Records()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 || records[3].Seq != 4 {
		t.Fatalf("records after recovery = %+v", records)
	}
	recovered.Close()
}

func TestInvalidCompleteLineIsTornTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFileName)
	if err := os.WriteFile(path, []byte("{\"seq\":1,\"t\":\"turn.started\"}\nnot json at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if log.LastSeq() != 1 {
		t.Fatalf("last seq = %d, want 1", log.LastSeq())
	}
}

func TestRunFailedCreatesErrorItem(t *testing.T) {
	root := t.TempDir()
	store, _ := OpenStore(root)
	session, _ := store.Create("p", "m", "")
	defer session.Close()
	appendRecord(t, session, Record{Type: TypeUserInput, RunID: "run-9", Content: "hi"})
	appendRecord(t, session, Record{Type: TypeRunFailed, RunID: "run-9", Error: "provider exploded"})
	state := session.Snapshot()
	last := state.Items[len(state.Items)-1]
	if last.Kind != ItemKindError || last.Content != "provider exploded" {
		t.Fatalf("error item = %+v", last)
	}
	// A failure is context-free: only the unanswered user message.
	if len(state.Context) != 1 {
		t.Fatalf("context = %d messages, want 1", len(state.Context))
	}
}

func TestSubscribeNotifiesOnAppend(t *testing.T) {
	root := t.TempDir()
	store, _ := OpenStore(root)
	session, _ := store.Create("p", "m", "")
	defer session.Close()
	ch, unsubscribe := session.Subscribe()
	defer unsubscribe()
	appendRecord(t, session, Record{Type: TypeUserInput, RunID: "r", Content: "x"})
	select {
	case <-ch:
	default:
		t.Fatal("no notification after append")
	}
	// A second append while the channel is full coalesces instead of blocking.
	appendRecord(t, session, Record{Type: TypeUserInput, RunID: "r2", Content: "again"})
	select {
	case <-ch:
	default:
		t.Fatal("notification lost")
	}
}

func TestUnknownRecordTypeIgnoredByFold(t *testing.T) {
	root := t.TempDir()
	store, _ := OpenStore(root)
	session, _ := store.Create("p", "m", "")
	defer session.Close()
	appendRecord(t, session, Record{Type: "future.event", Content: "irrelevant"})
	state := session.Snapshot()
	if len(state.Items) != 0 {
		t.Fatalf("unknown type changed the fold: %+v", state.Items)
	}
	if state.LastSeq != 2 {
		t.Fatalf("last seq = %d, want 2", state.LastSeq)
	}
}

func TestListSessionsNewestFirst(t *testing.T) {
	root := t.TempDir()
	store, _ := OpenStore(root)
	first, _ := store.Create("p", "m", "")
	appendRecord(t, first, Record{Type: TypeUserInput, RunID: "r", Content: "first session"})
	first.Close()
	second, _ := store.Create("p", "m", "")
	appendRecord(t, second, Record{Type: TypeUserInput, RunID: "r", Content: "second session"})
	second.Close()

	summaries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want 2", len(summaries))
	}
	if summaries[0].ID != second.ID() || summaries[0].Title != "second session" {
		t.Fatalf("newest summary = %+v", summaries[0])
	}
}

func TestLegacyAssistantReadyStillFolds(t *testing.T) {
	// Logs written before the assistant.ready → assistant.message rename
	// must still fold their assistant messages.
	root := t.TempDir()
	store, _ := OpenStore(root)
	session, _ := store.Create("p", "m", "")
	defer session.Close()
	appendRecord(t, session, Record{Type: TypeUserInput, RunID: "r", Content: "q"})
	appendRecord(t, session, Record{Type: TypeAssistantReady, RunID: "r", ItemID: "a-1", Content: "old spelling"})
	state := session.Snapshot()
	if len(state.Items) != 2 || state.Items[1].Content != "old spelling" {
		t.Fatalf("legacy assistant item = %+v", state.Items)
	}
	if len(state.Context) != 2 || state.Context[1].Content != "old spelling" {
		t.Fatalf("legacy context = %+v", state.Context)
	}
}

func TestValidSessionID(t *testing.T) {
	for id, valid := range map[string]bool{
		"s01a": true, ".": false, "..": false, "blobs": false, "a/b": false,
		"a\\b": false, "": false, " sp": false, strings.Repeat("x", 129): false,
	} {
		if got := ValidSessionID(id); got != valid {
			t.Errorf("ValidSessionID(%q) = %v, want %v", id, got, valid)
		}
	}
}

func TestNewSessionIDsUniqueAndSorted(t *testing.T) {
	ids := make(map[string]struct{}, 100)
	var last string
	for i := 0; i < 100; i++ {
		id := newSessionID()
		if !ValidSessionID(id) {
			t.Fatalf("generated id %q is invalid", id)
		}
		if _, exists := ids[id]; exists {
			t.Fatalf("duplicate id %q", id)
		}
		ids[id] = struct{}{}
		// Ordering holds at millisecond granularity: compare the base32
		// prefix that encodes the timestamp, not the random suffix.
		if last != "" && id[:9] < last[:9] {
			t.Fatalf("ids not time-ordered: %q after %q", id, last)
		}
		last = id[:9]
	}
}

func TestLazyCreateTouchesNoDisk(t *testing.T) {
	root := t.TempDir()
	store, _ := OpenStore(root)
	session, err := store.Create("paperhub", "glm-5.2", `F:\demo`)
	if err != nil {
		t.Fatal(err)
	}

	// Nothing on disk, nothing listed, and Close is a no-op.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("lazy create wrote %d entries, want 0", len(entries))
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	summaries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("lazy session listed: %+v", summaries)
	}

	// The in-memory snapshot still carries the creation metadata for the UI.
	state := session.Snapshot()
	if state.Provider != "paperhub" || state.Model != "glm-5.2" || state.CWD != `F:\demo` {
		t.Fatalf("lazy state meta = %+v", state)
	}
	if state.CreatedAt.IsZero() {
		t.Fatal("lazy state has no CreatedAt")
	}
}

func TestFirstAppendMaterializes(t *testing.T) {
	root := t.TempDir()
	store, _ := OpenStore(root)
	session, _ := store.Create("paperhub", "glm-5.2", "cwd")
	if _, err := session.Append(Record{Type: TypeUserInput, RunID: "r1", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	records, err := readRecords(filepath.Join(store.SessionDir(session.ID()), logFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2 (session.created + user.input)", len(records))
	}
	if records[0].Type != TypeSessionCreated || records[0].Seq != 1 || records[0].Provider != "paperhub" {
		t.Fatalf("materialized header = %+v", records[0])
	}
	if records[1].Type != TypeUserInput || records[1].Seq != 2 {
		t.Fatalf("appended record = %+v", records[1])
	}

	reopened, err := store.Open(session.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state := reopened.Snapshot()
	if state.Provider != "paperhub" || state.Title != "hello" {
		t.Fatalf("reopened state = %+v", state)
	}
}

func TestManyEmptySessionsLeaveNoDirectories(t *testing.T) {
	root := t.TempDir()
	store, _ := OpenStore(root)
	// Simulate /clear churn: sessions created and abandoned without input.
	for i := 0; i < 5; i++ {
		session, _ := store.Create("p", "m", "")
		session.Close()
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("empty sessions left %d directories, want 0", len(entries))
	}
}

func TestRecordsAfter(t *testing.T) {
	root := t.TempDir()
	store, _ := OpenStore(root)
	// Lazy session: no records before materialization.
	session, _ := store.Create("p", "m", "")
	defer session.Close()
	if records, err := session.RecordsAfter(0); err != nil || len(records) != 0 {
		t.Fatalf("lazy RecordsAfter = %v, %v", records, err)
	}
	appendRecord(t, session, Record{Type: TypeUserInput, RunID: "r", Content: "one"})
	appendRecord(t, session, Record{Type: TypeRunFailed, RunID: "r", Error: "boom"})

	// seq 1 is the materialized session.created.
	all, err := session.RecordsAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Type != TypeSessionCreated {
		t.Fatalf("RecordsAfter(0) = %d records, first %s", len(all), all[0].Type)
	}
	tail, err := session.RecordsAfter(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].Seq != 3 {
		t.Fatalf("RecordsAfter(2) = %+v", tail)
	}
	none, err := session.RecordsAfter(3)
	if err != nil || len(none) != 0 {
		t.Fatalf("RecordsAfter(4) = %v, %v", none, err)
	}
}
