package sessionindex

import (
	"context"
	"testing"

	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestProjectResourceRevisionResetsWithNewEpoch(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	state := testSession(t, store, "s", "p", "s")
	provider, err := NewProvider(store, ProviderOptions{StreamEpoch: "epoch", ProjectorQueueCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	first := openIndex(t, provider, "p", nil)
	first.Close()
	summary := SummaryFromSession(state, false)
	summary.DisplayName = "renamed"
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedSessionUpsert, ProjectID: "p", SessionID: "s", Summary: &summary}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Flush(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	second := openIndex(t, provider, "p", &protocol.ResumeToken{StreamEpoch: first.StreamEpoch, Sequence: "0"})
	second.Close()
	// Invalidate is a new stream epoch. Its durable snapshot is authoritative
	// and starts the project-level revision at zero, independent of LastSeq.
	if err := provider.InvalidateProject("p", "test"); err != nil {
		t.Fatal(err)
	}
	rebuilt := openIndex(t, provider, "p", nil)
	defer rebuilt.Close()
	if rebuilt.Snapshot.ResourceRevision != "0" {
		t.Fatalf("rebuilt resource revision = %s, want 0", rebuilt.Snapshot.ResourceRevision)
	}
	if rebuilt.StreamEpoch == first.StreamEpoch {
		t.Fatal("rebuild did not create a new epoch")
	}
}
