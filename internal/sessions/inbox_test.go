package sessions

import (
	"testing"
	"time"
)

func TestSessionInboxIsDurableIdempotentAndRetryable(t *testing.T) {
	store := NewV2Store(t.TempDir())
	parent, err := store.SaveMetadata(SessionV2{ID: "parent-inbox"})
	if err != nil {
		t.Fatalf("SaveMetadata(parent): %v", err)
	}
	childID, childRunID := "child-inbox", "run-child-inbox"
	deliveryID := NewSessionCompletionDeliveryID(parent.ID, childID, childRunID)
	created := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	first, err := store.RegisterSessionCompletion(parent.ID, childID, childRunID, deliveryID, created)
	if err != nil {
		t.Fatalf("RegisterSessionCompletion: %v", err)
	}
	if first.Status != SessionInboxStatusPending || first.DeliveryID != deliveryID {
		t.Fatalf("initial delivery = %#v", first)
	}
	duplicate, err := store.RegisterSessionCompletion(parent.ID, childID, childRunID, deliveryID, created.Add(time.Hour))
	if err != nil {
		t.Fatalf("duplicate RegisterSessionCompletion: %v", err)
	}
	if duplicate.CreatedAt != created {
		t.Fatalf("duplicate changed created_at to %v, want %v", duplicate.CreatedAt, created)
	}
	if err := store.MarkSessionCompletionDelivered(parent.ID, childID, childRunID, RunStatusCommitted, created.Add(time.Minute)); err != nil {
		t.Fatalf("MarkSessionCompletionDelivered: %v", err)
	}
	claimed, err := store.ClaimSessionCompletionDelivery(parent.ID, deliveryID, "run-parent-continuation")
	if err != nil || !claimed {
		t.Fatalf("first claim = %v/%v, want true", claimed, err)
	}
	if err := store.ClearSessionCompletionClaim(parent.ID, deliveryID, "run-parent-continuation", "startup failed"); err != nil {
		t.Fatalf("ClearSessionCompletionClaim: %v", err)
	}
	claimed, err = store.ClaimSessionCompletionDelivery(parent.ID, deliveryID, "run-parent-continuation")
	if err != nil || !claimed {
		t.Fatalf("retry claim = %v/%v, want true", claimed, err)
	}
	if err := store.ConsumeSessionCompletionDelivery(parent.ID, deliveryID, "run-parent-continuation", created.Add(2*time.Minute)); err != nil {
		t.Fatalf("ConsumeSessionCompletionDelivery: %v", err)
	}
	rows, err := store.ListSessionInbox(parent.ID)
	if err != nil {
		t.Fatalf("ListSessionInbox: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != SessionInboxStatusConsumed || rows[0].Attempt != 2 || rows[0].ChildStatus != RunStatusCommitted {
		t.Fatalf("final inbox rows = %#v", rows)
	}
}
