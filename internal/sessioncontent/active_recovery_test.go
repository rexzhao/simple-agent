package sessioncontent

import (
	"encoding/json"
	"testing"
	"time"
)

func TestActiveRunWithoutTransientBufferRequiresRecovery(t *testing.T) {
	store, session := newContentTestStore(t, "active-recovery")
	if _, err := store.CreateRun(session.ID, "run-restart", "", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	provider, err := NewProvider(store, ProviderOptions{StreamEpoch: "new-server-epoch"})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened := openContent(t, provider, session.ID, nil)
	defer opened.Close()
	if opened.TransientResync != "active_run_recovery_required" {
		t.Fatalf("TransientResync = %q, want active recovery", opened.TransientResync)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(opened.Snapshot.Content.InlineBytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveRun == nil || !snapshot.ActiveRun.RecoveryRequired || snapshot.ActiveRun.ReplayAvailable {
		t.Fatalf("active recovery snapshot = %#v", snapshot.ActiveRun)
	}
	if snapshot.ActiveRun.RunID != "run-restart" || snapshot.ActiveRun.SessionID != session.ID {
		t.Fatalf("active identity = %#v", snapshot.ActiveRun)
	}
}
