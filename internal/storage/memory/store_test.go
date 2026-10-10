package memory

import (
	"testing"

	"github.com/rexzhao/simple-agent/internal/storage"
)

func TestCreateAppendOpenRoundTrip(t *testing.T) {
	store := OpenStore()
	session, err := store.Create("")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if session.ID() == "" {
		t.Fatal("session id is empty")
	}
	if _, err := session.Append(storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	records, err := session.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) != 1 || records[0].Content != "hi" {
		t.Fatalf("records = %+v, want one stored user input", records)
	}

	reopened, err := store.Open(session.ID())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reopened.ID() != session.ID() {
		t.Fatalf("reopened id = %q, want %q", reopened.ID(), session.ID())
	}
}

func TestRecordsSurviveClosedHandle(t *testing.T) {
	store := OpenStore()
	session, _ := store.Create("")
	if _, err := session.Append(storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "kept"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A closed handle is dead, but the records live on for a later Open,
	// mirroring the jsonl file semantics.
	if _, err := session.Append(storage.Record{ItemID: "u2", Type: storage.TypeUserInput}); err == nil {
		t.Fatal("Append after Close accepted")
	}
	reopened, err := store.Open(session.ID())
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	records, err := reopened.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) != 1 || records[0].Content != "kept" {
		t.Fatalf("records after reopen = %+v", records)
	}
}

func TestOpenUnknownSessionFails(t *testing.T) {
	store := OpenStore()
	if _, err := store.Open("s-nope"); err == nil {
		t.Fatal("Open of unknown session succeeded")
	}
}

func TestListSessionsAndOpenIn(t *testing.T) {
	store := OpenStore()
	project := "ptest0000000000001"
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(storage.Record{ItemID: "h1", Type: storage.TypeSessionCreated, Time: "2026-10-10T01:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Time: "2026-10-10T01:00:01Z", Content: "hello"}); err != nil {
		t.Fatal(err)
	}

	infos, err := store.ListSessions(project)
	if err != nil || len(infos) != 1 {
		t.Fatalf("list: %v %d", err, len(infos))
	}
	if infos[0].Title != "hello" || infos[0].ID != session.ID() {
		t.Fatalf("info = %+v", infos[0])
	}

	// A never-materialized session holds nothing and stays unlisted.
	empty, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	_ = empty
	if infos, err = store.ListSessions(project); err != nil || len(infos) != 1 {
		t.Fatalf("list after empty create: %v %d", err, len(infos))
	}

	// OpenIn is scoped: a foreign project cannot see the session.
	if _, err := store.OpenIn("ptest0000000000002", session.ID()); err == nil {
		t.Fatal("foreign project opened the session")
	}
	opened, err := store.OpenIn(project, session.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

// A records slice starting as a compaction product has its start
// off-store: no mid-conversation input may pose as the title.
func TestListSessionsCompactedStart(t *testing.T) {
	store := OpenStore()
	project := "ptest0000000000001"
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []storage.Record{
		{ItemID: "h1", Type: storage.TypeSessionCreated, Time: "2026-10-10T01:00:00Z"},
		{ItemID: "x1", Type: storage.TypeContextCompacted, Time: "2026-10-10T02:00:00Z"},
		{ItemID: "u2", Type: storage.TypeUserInput, Time: "2026-10-10T02:00:10Z", Content: "mid input"},
	} {
		if _, err := session.Append(record); err != nil {
			t.Fatal(err)
		}
	}
	infos, err := store.ListSessions(project)
	if err != nil || len(infos) != 1 {
		t.Fatalf("list: %v %d", err, len(infos))
	}
	if infos[0].Title != "" {
		t.Fatalf("title = %q, want empty", infos[0].Title)
	}
}
