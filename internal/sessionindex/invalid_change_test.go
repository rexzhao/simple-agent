package sessionindex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

func TestMalformedCommittedChangeInvalidatesOriginalProject(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	p1 := testSession(t, store, "s1", "p1", "one")
	_ = testSession(t, store, "s2", "p2", "two")
	provider, err := NewProvider(store, ProviderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened := openIndex(t, provider, "p1", nil)
	defer opened.Close()
	bad := SummaryFromSession(p1, false)
	bad.ProjectID = "p2"
	if err := provider.PublishCommitted(CommittedChange{
		Kind: CommittedSessionUpsert, ProjectID: "p1", SessionID: p1.ID, Summary: &bad,
	}); err == nil {
		t.Fatal("project-mismatched summary was accepted")
	}
	select {
	case terminal := <-opened.Terminal:
		if terminal.Reason != syncengine.LiveTerminalSequence {
			t.Fatalf("invalid change terminal=%#v", terminal)
		}
	case <-time.After(time.Second):
		t.Fatal("invalid committed change did not invalidate subscription")
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: "unknown", ProjectID: "p1", SessionID: p1.ID}); err == nil {
		t.Fatal("unknown committed kind was accepted")
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedSessionRemove, ProjectID: "p1"}); err == nil {
		t.Fatal("empty remove was accepted")
	}
	rebuilt := openIndex(t, provider, "p1", nil)
	defer rebuilt.Close()
	var snapshot SessionIndexSnapshot
	if err := json.Unmarshal(rebuilt.Snapshot.Content.InlineBytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].SessionID != "s1" {
		t.Fatalf("invalid change altered rebuilt snapshot=%#v", snapshot)
	}
}
