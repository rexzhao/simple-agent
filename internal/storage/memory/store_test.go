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
