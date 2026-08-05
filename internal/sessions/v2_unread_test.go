package sessions

import (
	"testing"
	"time"
)

func TestCancelledRunDoesNotCreateUnreadBadge(t *testing.T) {
	store := NewV2Store(t.TempDir())
	state, err := store.SaveMetadata(SessionV2{ID: "cancelled", ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRun(state.ID, "run-cancelled", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	state, err = store.SetRunStatus(state.ID, "run-cancelled", RunStatusCancelled, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if state.HasUnreadResult {
		t.Fatal("user-cancelled run incorrectly created unread result")
	}
}
