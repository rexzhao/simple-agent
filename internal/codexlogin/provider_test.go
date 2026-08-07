package codexlogin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

func TestProviderSnapshotChangeReplayAndResync(t *testing.T) {
	var mu sync.Mutex
	status := execution.CodexAuthStatus{Status: StatusSignedOut}
	provider, err := NewProvider(ProviderOptions{
		StreamEpoch: "test-epoch", OwnerContext: context.Background(), JournalEntries: 1,
		ValidateProvider: func(name string) error {
			if name != "codex" {
				return ErrProviderNotFound
			}
			return nil
		},
		Status: func(string) (execution.CodexAuthStatus, error) {
			mu.Lock()
			defer mu.Unlock()
			return status, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()

	key := protocol.ResourceKey{Type: protocol.ResourceTypeCodexLogin, ID: "codex"}
	if err := provider.Authorize(context.Background(), syncengine.Principal{ID: "capability"}, key); err != nil {
		t.Fatal(err)
	}
	opened, err := provider.Open(context.Background(), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assertSnapshotStatus(t, opened.Snapshot.Content.InlineBytes(), StatusSignedOut)

	mu.Lock()
	status = execution.CodexAuthStatus{Status: StatusPending, LoginID: "login-1", UserCode: "ABCD", VerifyURL: "https://example.test/device"}
	mu.Unlock()
	if err := provider.PublishCommitted(CommittedChange{Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	entry := receiveEntry(t, opened.Changes)
	if entry.Sequence != 1 || entry.PreviousSequence != 0 || entry.Change.ResourceRevision != "1" {
		t.Fatalf("entry=%#v", entry)
	}
	if strings.Contains(string(entry.Change.Operations[0].Raw), "token") {
		t.Fatalf("resource change contains credential material: %s", entry.Change.Operations[0].Raw)
	}

	// A same-state publication is a no-op and does not append a duplicate.
	if err := provider.PublishCommitted(CommittedChange{Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-opened.Changes:
		t.Fatalf("unexpected duplicate change: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}

	mu.Lock()
	status = execution.CodexAuthStatus{Status: StatusSignedIn, Refreshable: true}
	mu.Unlock()
	if err := provider.PublishCommitted(CommittedChange{Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	second := receiveEntry(t, opened.Changes)
	if second.Sequence != 2 || second.Change.ResourceRevision != "2" {
		t.Fatalf("second=%#v", second)
	}

	// The old cursor is outside the one-entry journal window and must resync.
	opened.Close()
	resumed, err := provider.Open(context.Background(), key, &protocol.ResumeToken{StreamEpoch: opened.StreamEpoch, Sequence: "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.Decision.Action != syncengine.SyncActionResync || resumed.Decision.Reason == "" {
		t.Fatalf("resume decision=%#v", resumed.Decision)
	}
}

func TestProviderRejectsUnknownProviderAndMapsUnsafeStatus(t *testing.T) {
	provider, err := NewProvider(ProviderOptions{
		ValidateProvider: func(string) error { return errors.New("secret/auth-file/path") },
		Status: func(string) (execution.CodexAuthStatus, error) {
			return execution.CodexAuthStatus{Status: "unknown", Message: "access-token-secret /raw/auth"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	key := protocol.ResourceKey{Type: protocol.ResourceTypeCodexLogin, ID: "codex"}
	if err := provider.Authorize(context.Background(), syncengine.Principal{ID: "capability"}, key); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("authorize error=%v", err)
	}

	provider, err = NewProvider(ProviderOptions{
		ValidateProvider: func(string) error { return nil },
		Status: func(string) (execution.CodexAuthStatus, error) {
			return execution.CodexAuthStatus{Status: "unknown", Message: "access-token-secret /raw/auth"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	opened, err := provider.Open(context.Background(), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	data := opened.Snapshot.Content.InlineBytes()
	if strings.Contains(string(data), "access-token-secret") || strings.Contains(string(data), "/raw/auth") {
		t.Fatalf("unsafe status leaked: %s", data)
	}
	assertSnapshotStatus(t, data, StatusError)
}

func receiveEntry(t *testing.T, entries <-chan syncengine.JournalEntry) syncengine.JournalEntry {
	t.Helper()
	select {
	case entry := <-entries:
		return entry
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Codex login resource change")
		return syncengine.JournalEntry{}
	}
}

func assertSnapshotStatus(t *testing.T, data []byte, expected string) {
	t.Helper()
	var value Snapshot
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if value.Status != expected {
		t.Fatalf("snapshot status=%q, want %q", value.Status, expected)
	}
}
