package sessionindex

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestWarmBarrierDoesNotLoseMutationDuringDiscovery(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	state := testSession(t, store, "s", "p", "before")
	warmEntered := make(chan struct{})
	allowWarm := make(chan struct{})
	provider, err := NewProvider(store, ProviderOptions{BeforeWarm: func() {
		close(warmEntered)
		<-allowWarm
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	warmDone := make(chan error, 1)
	go func() { warmDone <- provider.Warm(context.Background()) }()
	<-warmEntered
	state.DisplayName = "during-warm"
	if _, err := store.SaveMetadata(state); err != nil {
		t.Fatal(err)
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedSessionUpsert, ProjectID: "p", SessionID: state.ID}); err != nil {
		t.Fatal(err)
	}
	close(allowWarm)
	if err := <-warmDone; err != nil {
		t.Fatal(err)
	}
	opened := openIndex(t, provider, "p", nil)
	defer opened.Close()
	var snapshot SessionIndexSnapshot
	if err := json.Unmarshal(opened.Snapshot.Content.InlineBytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].DisplayName != "during-warm" {
		t.Fatalf("warm lost durable mutation: %#v", snapshot.Sessions)
	}
}
