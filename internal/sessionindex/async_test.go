package sessionindex

import (
	"context"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestCommittedCallbackDoesNotWaitForBlockedProjector(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	state := testSession(t, store, "s", "p", "s")
	started := make(chan struct{})
	release := make(chan struct{})
	provider, err := NewProvider(store, ProviderOptions{BeforeApply: func(CommittedChange) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened := openIndex(t, provider, "p", nil)
	defer opened.Close()
	summary := SummaryFromSession(state, false)
	begin := time.Now()
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedSessionUpsert, ProjectID: "p", SessionID: "s", Summary: &summary}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
		t.Fatalf("commit callback waited for projector: %s", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("projector did not start")
	}
	close(release)
	if err := provider.Flush(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
}
