package sessions

import (
	"testing"
	"time"
)

func TestMarkReadIsDurableAndGuardsRunID(t *testing.T) {
	store := NewV2Store(t.TempDir())
	state, err := store.SaveMetadata(SessionV2{ID: "mark-read", ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRun(state.ID, "run-one", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	state, err = store.SetRunStatus(state.ID, "run-one", RunStatusCommitted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasUnreadResult {
		t.Fatal("settled run did not persist unread result")
	}
	state, marked, err := store.MarkRead(state.ID, "run-one")
	if err != nil || !marked || state.HasUnreadResult {
		t.Fatalf("matching mark read = state=%#v marked=%v err=%v", state, marked, err)
	}
	if _, marked, err := store.MarkRead(state.ID, "run-one"); err != nil || marked {
		t.Fatalf("repeated mark read = marked=%v err=%v", marked, err)
	}
	if _, err := store.CreateRun(state.ID, "run-two", "run-one", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	state, err = store.SetRunStatus(state.ID, "run-two", RunStatusCommitted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasUnreadResult {
		t.Fatal("new settled run did not restore unread result")
	}
	state, marked, err = store.MarkRead(state.ID, "run-one")
	if err != nil || marked || !state.HasUnreadResult {
		t.Fatalf("old run cleared current unread state: state=%#v marked=%v err=%v", state, marked, err)
	}
	persisted, err := store.LoadState(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.HasUnreadResult || persisted.LatestRunID != "run-two" {
		t.Fatalf("durable guarded state = %#v", persisted)
	}
}
