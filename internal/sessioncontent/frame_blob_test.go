package sessioncontent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/blobstore"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

func TestChangeMessageBoundPreflightsCompleteEnvelopeBeforeAppend(t *testing.T) {
	store, session := newContentTestStore(t, "session-frame-bound")
	latest, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest.ShowReasoning = true
	if _, err := store.SaveMetadata(latest); err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(store, ProviderOptions{MaxChangeMessageBytes: protocol.DefaultMaxMessageBytes})
	if err != nil {
		t.Fatal(err)
	}
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	defer p.Close()
	opened := openContent(t, p, session.ID, nil)
	defer opened.Close()

	items := make([]sessions.SessionItem, 0, 5)
	history := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("frame-item-%d", i)
		item := sessions.SessionItemFromMessage(id, model.Message{
			Role:             model.MessageRoleAssistant,
			ReasoningContent: strings.Repeat("reasoning-界", 6000),
			Content:          "small",
		})
		items = append(items, item)
		history = append(history, id)
	}
	if _, err := store.AppendItemsAndReplaceActiveHistory(session.ID, items, history); err != nil {
		t.Fatal(err)
	}
	select {
	case terminal := <-opened.Terminal:
		if terminal.Reason != syncengine.LiveTerminalSequence {
			t.Fatalf("oversized terminal = %#v, want a provider resync boundary", terminal)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("complete ChangeMessage preflight did not desync")
	}
	o := p.owners[session.ID]
	o.mu.Lock()
	stats := o.journal.Stats()
	o.mu.Unlock()
	if stats.LastSequence != 0 || stats.Bytes != 0 {
		t.Fatalf("oversized transaction appended before preflight: %+v", stats)
	}
	state, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastSeq <= 0 {
		t.Fatalf("durable transaction did not commit: LastSeq=%d", state.LastSeq)
	}
}

func TestItemBlobCachePreservesContentTypeAndRefreshesNearExpiry(t *testing.T) {
	store, session := newContentTestStore(t, "session-item-blob-refresh")
	large := strings.Repeat("内容-", 500)
	if _, err := store.AppendItem(session.ID, sessions.SessionItemFromMessage("large", model.Message{
		Role: model.MessageRoleUser, Content: large,
	})); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	blobStore, err := blobstore.New(blobstore.Options{
		TTL:             5 * time.Minute,
		Now:             func() time.Time { return now },
		JanitorInterval: -1,
		MaxEntries:      16,
		MaxBytes:        4 * 1024 * 1024,
		MaxBlobBytes:    2 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer blobStore.Close()
	p, err := NewProvider(store, ProviderOptions{
		MaxItemContentBytes: 16,
		BlobWriter:          blobStore,
		Now:                 func() time.Time { return now },
		BlobRefreshSkew:     time.Minute,
		MaxItemBlobs:        1,
	})
	if err != nil {
		t.Fatal(err)
	}
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	defer p.Close()
	opened := openContent(t, p, session.ID, nil)
	defer opened.Close()
	snapshot := decodeSnapshot(t, opened)
	first := snapshot.History.Items[0].Message.Content
	if first == nil || first.Blob == nil || first.ContentType != first.Blob.ContentType {
		t.Fatalf("first large content descriptor/type = %#v", first)
	}
	firstDescriptor := *first.Blob

	// A cache hit must retain the descriptor's ContentType and must not create
	// another item upsert when an unrelated durable item changes.
	if _, err := store.AppendItem(session.ID, sessions.SessionItemFromMessage("small-1", model.Message{Role: model.MessageRoleUser, Content: "small"})); err != nil {
		t.Fatal(err)
	}
	change := nextChange(t, opened)
	for _, operation := range change.Change.Operations {
		if operation.Op != OpItemUpsert {
			continue
		}
		var payload struct {
			Item Item `json:"item"`
		}
		if err := json.Unmarshal(operation.Raw, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Item.Key.ItemID == "large" {
			t.Fatal("unrelated mutation repeated the unchanged large item upsert")
		}
	}
	if stats := blobStore.Stats(); stats.Entries != 1 {
		t.Fatalf("cache hit created a duplicate blob: %+v", stats)
	}
	metadata, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	metadata.DisplayName = "renamed without changing the item"
	if _, err := store.SaveMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	metadataChange := nextChange(t, opened)
	for _, operation := range metadataChange.Change.Operations {
		if operation.Op != OpItemUpsert {
			continue
		}
		var payload struct {
			Item Item `json:"item"`
		}
		if err := json.Unmarshal(operation.Raw, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Item.Key.ItemID == "large" {
			t.Fatal("metadata mutation repeated the unchanged large item upsert")
		}
	}

	// Near-expiry cache entries are rejected. The built-in writer's fresh path
	// creates a new descriptor, so the live upsert remains downloadable rather
	// than carrying an already stale URL.
	now = now.Add(4*time.Minute + 30*time.Second)
	if _, err := store.AppendItem(session.ID, sessions.SessionItemFromMessage("small-2", model.Message{Role: model.MessageRoleUser, Content: "small-2"})); err != nil {
		t.Fatal(err)
	}
	refreshChange := nextChange(t, opened)
	var refreshed *Item
	for _, operation := range refreshChange.Change.Operations {
		if operation.Op != OpItemUpsert {
			continue
		}
		var payload struct {
			Item Item `json:"item"`
		}
		if err := json.Unmarshal(operation.Raw, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Item.Key.ItemID == "large" {
			refreshed = &payload.Item
		}
	}
	if refreshed == nil || refreshed.Message == nil || refreshed.Message.Content == nil || refreshed.Message.Content.Blob == nil {
		t.Fatalf("near-expiry refresh did not upsert the large item: %#v", refreshChange.Change.Operations)
	}
	if refreshed.Message.Content.ContentType != refreshed.Message.Content.Blob.ContentType || refreshed.Message.Content.Blob.ID == firstDescriptor.ID {
		t.Fatalf("refreshed descriptor/type = %#v, old=%#v", refreshed.Message.Content, firstDescriptor)
	}
	if expires, err := time.Parse(time.RFC3339Nano, refreshed.Message.Content.Blob.ExpiresAt); err != nil || !now.Add(time.Minute).Before(expires) {
		t.Fatalf("refreshed descriptor is not safely usable: %#v, err=%v", refreshed.Message.Content.Blob, err)
	}
}

func TestBlobCapacityFailureDesyncsOnlyAffectedResource(t *testing.T) {
	store, sessionA := newContentTestStore(t, "session-capacity-a")
	sessionB, err := store.SaveMetadata(sessions.SessionV2{ID: "session-capacity-b", ProjectID: "project-a"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.AppendItem(sessionA.ID, sessions.SessionItemFromMessage(fmt.Sprintf("large-%d", i), model.Message{
			Role: model.MessageRoleUser, Content: strings.Repeat(fmt.Sprintf("blob-%d-", i), 500),
		})); err != nil {
			t.Fatal(err)
		}
	}
	clock := time.Now().UTC()
	blobStore, err := blobstore.New(blobstore.Options{
		TTL:             time.Hour,
		Now:             func() time.Time { return clock },
		JanitorInterval: -1,
		MaxEntries:      5,
		MaxBytes:        1024 * 1024,
		MaxBlobBytes:    1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer blobStore.Close()
	p, err := NewProvider(store, ProviderOptions{MaxItemContentBytes: 16, InlineSnapshotBytes: 1, BlobWriter: blobStore})
	if err != nil {
		t.Fatal(err)
	}
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	defer p.Close()
	b, err := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: sessionB.ID}, nil)
	if err != nil {
		t.Fatalf("baseline session could not open before capacity pressure: %v", err)
	}
	defer b.Close()
	a, err := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: sessionA.ID}, nil)
	if err != nil {
		t.Fatalf("baseline item-plus-snapshot unexpectedly failed: %v", err)
	}
	defer a.Close()
	if got := blobStore.Stats().Entries; got != 5 {
		t.Fatalf("capacity pressure entries = %d, want bounded 5", got)
	}
	// A failed projection is a resource-local error. It must neither block a
	// durable write nor poison a different session owner.
	if _, err := store.AppendItem(sessionA.ID, sessions.SessionItemFromMessage("capacity-fourth", model.Message{
		Role: model.MessageRoleUser, Content: strings.Repeat("new-item-", 500),
	})); err != nil {
		t.Fatal(err)
	}
	select {
	case terminal := <-a.Terminal:
		if terminal.Reason != syncengine.LiveTerminalSequence {
			t.Fatalf("capacity terminal = %#v, want resource resync", terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capacity-exhausted live projection did not desync")
	}
	start := time.Now()
	if _, err := store.AppendItem(sessionA.ID, sessions.SessionItemFromMessage("after-failure", model.Message{Role: model.MessageRoleUser, Content: "durable"})); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("durable write waited for blob capacity failure: %s", elapsed)
	}
	if _, err := store.AppendItem(sessionB.ID, sessions.SessionItemFromMessage("small-b", model.Message{Role: model.MessageRoleUser, Content: "B"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.Changes:
	case terminal := <-b.Terminal:
		t.Fatalf("unrelated session was poisoned by blob capacity: %#v", terminal)
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated session did not receive its bounded live change")
	}
	if _, err := blobStore.Open("does-not-exist"); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("blob store unexpected error after capacity pressure: %v", err)
	}
}
