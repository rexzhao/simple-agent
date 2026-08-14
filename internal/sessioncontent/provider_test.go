package sessioncontent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

type testBlobWriter struct {
	calls    int
	contents [][]byte
}

func (w *testBlobWriter) Put(_ context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error) {
	w.calls++
	w.contents = append(w.contents, append([]byte(nil), content...))
	return protocol.BlobDescriptor{ID: fmt.Sprintf("session-content-test-%d", w.calls), URL: fmt.Sprintf("/api/blobs/session-content-test-%d", w.calls), ContentType: contentType, Size: uint64(len(content)), SHA256: strings.Repeat("a", 64), ETag: "\"test\"", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}, nil
}

func newContentTestStore(t *testing.T, id string) (*sessions.V2Store, sessions.SessionV2) {
	t.Helper()
	store := sessions.NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(sessions.SessionV2{ID: id, ProjectID: "project-a", DisplayName: "Test session", Provider: "codex", ModelID: "codex/gpt-5.6-luna", CreatedCWD: "/tmp"})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	return store, session
}

func openContent(t *testing.T, p *Provider, sessionID string, resume *protocol.ResumeToken) syncengine.OpenedResource {
	t.Helper()
	opened, err := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: sessionID}, resume)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return opened
}

func decodeSnapshot(t *testing.T, opened syncengine.OpenedResource) Snapshot {
	t.Helper()
	if _, ok := opened.Snapshot.Content.BlobDescriptor(); ok {
		t.Fatal("expected inline test snapshot")
	}
	var snapshot Snapshot
	if err := json.Unmarshal(opened.Snapshot.Content.InlineBytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("snapshot.Validate() error = %v", err)
	}
	return snapshot
}

func TestSharedSnapshotFixtureMatchesSchema(t *testing.T) {
	raw, err := os.ReadFile("../protocol/testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Valid []struct {
			Name    string          `json:"name"`
			Message json.RawMessage `json:"message"`
		} `json:"valid"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures.Valid {
		if fixture.Name != "session_content_snapshot_inline" {
			continue
		}
		var envelope struct {
			Payload struct {
				Content struct {
					Inline Snapshot `json:"inline"`
				} `json:"content"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(fixture.Message, &envelope); err != nil {
			t.Fatal(err)
		}
		if err := envelope.Payload.Content.Inline.Validate(); err != nil {
			t.Fatalf("shared session-content fixture does not match schema: %v", err)
		}
		return
	}
	t.Fatal("session_content_snapshot_inline fixture not found")
}

func nextChange(t *testing.T, opened syncengine.OpenedResource) syncengine.JournalEntry {
	t.Helper()
	select {
	case entry := <-opened.Changes:
		return entry
	case terminal := <-opened.Terminal:
		t.Fatalf("subscription terminated: %#v", terminal)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for durable change")
	}
	return syncengine.JournalEntry{}
}

func TestSnapshotSchemaIdentityAndOrder(t *testing.T) {
	store, session := newContentTestStore(t, "session-schema")
	for i, item := range []sessions.SessionItem{
		sessions.SessionItemFromMessage("item-1", model.Message{Role: model.MessageRoleUser, Content: "one"}),
		sessions.SessionItemFromMessage("item-2", model.Message{Role: model.MessageRoleAssistant, Content: "two"}),
	} {
		item.TurnID = fmt.Sprintf("turn-%d", i+1)
		item.AgentIteration = i + 1
		if _, err := store.AppendItem(session.ID, item); err != nil {
			t.Fatalf("AppendItem(%d) error = %v", i, err)
		}
	}
	p, err := NewProvider(store, ProviderOptions{HistoryLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	opened := openContent(t, p, session.ID, nil)
	defer opened.Close()
	snapshot := decodeSnapshot(t, opened)
	if got := len(snapshot.History.Items); got != 2 {
		t.Fatalf("history item count = %d, want 2", got)
	}
	if snapshot.History.Items[0].Key != (ItemKey{TurnID: "turn-1", AgentIteration: 1, ItemID: "item-1"}) {
		t.Fatalf("first identity = %#v", snapshot.History.Items[0].Key)
	}
	if snapshot.History.Items[0].Seq >= snapshot.History.Items[1].Seq {
		t.Fatal("snapshot history is not in durable order")
	}
	if got := string(opened.Snapshot.ResourceRevision); got != fmt.Sprint(session.LastSeq+2) {
		t.Fatalf("resource revision = %q, want latest durable LastSeq", got)
	}
}

func TestSnapshotReplacesInvalidUTF8FromDurableTextBlob(t *testing.T) {
	store, session := newContentTestStore(t, "session-invalid-text-blob")
	ref, err := store.WriteBlobForSession(session.ID, []byte{'a', 0xff, 'b'}, "utf-8", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	item := sessions.SessionItemFromMessage("tool-invalid-text", model.Message{Role: model.MessageRoleTool})
	item.Content = &sessions.StoredContent{Blob: &ref}
	if _, err := store.AppendItem(session.ID, item); err != nil {
		t.Fatal(err)
	}
	provider, err := NewProvider(store, ProviderOptions{HistoryLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()

	snapshot := decodeSnapshot(t, openContent(t, provider, session.ID, nil))
	if len(snapshot.History.Items) != 1 || snapshot.History.Items[0].Message == nil || snapshot.History.Items[0].Message.Content == nil {
		t.Fatalf("snapshot history = %#v", snapshot.History.Items)
	}
	if got := snapshot.History.Items[0].Message.Content.Inline; got != "a\uFFFDb" {
		t.Fatalf("projected content = %q, want replacement character", got)
	}
}

func TestSnapshotRejectsInvalidUTF8OutsideDurableToolTextBlob(t *testing.T) {
	store, session := newContentTestStore(t, "session-invalid-user-text-blob")
	ref, err := store.WriteBlobForSession(session.ID, []byte{'a', 0xff, 'b'}, "utf-8", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	item := sessions.SessionItemFromMessage("user-invalid-text", model.Message{Role: model.MessageRoleUser})
	item.Content = &sessions.StoredContent{Blob: &ref}
	if _, err := store.AppendItem(session.ID, item); err != nil {
		t.Fatal(err)
	}
	provider, err := NewProvider(store, ProviderOptions{HistoryLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()

	_, err = provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil)
	if err == nil || !strings.Contains(err.Error(), "content text is not valid UTF-8") {
		t.Fatalf("Open() error = %v, want invalid UTF-8", err)
	}
}

func TestDurableOperationsAndActiveRunBaseline(t *testing.T) {
	store, session := newContentTestStore(t, "session-ops")
	p, err := NewProvider(store, ProviderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	defer p.Close()
	opened := openContent(t, p, session.ID, nil)
	defer opened.Close()
	item := sessions.SessionItemFromMessage("item-1", model.Message{Role: model.MessageRoleUser, Content: "hello"})
	item.TurnID, item.AgentIteration = "turn-1", 1
	if _, err := store.AppendItem(session.ID, item); err != nil {
		t.Fatal(err)
	}
	change := nextChange(t, opened)
	if len(change.Change.Operations) == 0 {
		t.Fatal("item append produced no operation")
	}
	if change.Change.Operations[0].Op != OpItemUpsert {
		t.Fatalf("first op = %q, want %q", change.Change.Operations[0].Op, OpItemUpsert)
	}
	var upsert struct {
		Op   string `json:"op"`
		Item Item   `json:"item"`
	}
	if err := json.Unmarshal(change.Change.Operations[0].Raw, &upsert); err != nil {
		t.Fatal(err)
	}
	if upsert.Item.Key != (ItemKey{TurnID: "turn-1", AgentIteration: 1, ItemID: "item-1"}) {
		t.Fatalf("upsert key = %#v", upsert.Item.Key)
	}
	if _, err := store.CreateRun(session.ID, "run-1", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	runChange := nextChange(t, opened)
	foundActive := false
	for _, op := range runChange.Change.Operations {
		if op.Op == OpActiveRunReplace {
			foundActive = true
		}
	}
	if !foundActive {
		t.Fatalf("run change operations = %#v, want active_run.replace", runChange.Change.Operations)
	}
	if _, err := store.SetRunStatus(session.ID, "run-1", sessions.RunStatusCancelled, time.Now()); err != nil {
		t.Fatal(err)
	}
	clearChange := nextChange(t, opened)
	foundClear := false
	for _, op := range clearChange.Change.Operations {
		if op.Op == OpActiveRunClear {
			foundClear = true
		}
	}
	if !foundClear {
		t.Fatalf("settled operations = %#v, want active_run.clear", clearChange.Change.Operations)
	}
	if _, err := store.AppendCompaction(session.ID, sessions.CompactionCheckpoint{ID: "compact-1", Reason: "test", Phase: "completed", Trigger: "test", SummaryItemID: "item-1", ReplacementHistory: []string{"item-1"}}); err != nil {
		t.Fatal(err)
	}
	compactionChange := nextChange(t, opened)
	foundCompaction := false
	for _, op := range compactionChange.Change.Operations {
		foundCompaction = foundCompaction || op.Op == OpCompactionReplace
	}
	if !foundCompaction {
		t.Fatalf("compaction operations = %#v, want compaction.replace", compactionChange.Change.Operations)
	}
}

func TestMetadataReplaceAndWindowItemRemove(t *testing.T) {
	store, session := newContentTestStore(t, "session-diff")
	p, err := NewProvider(store, ProviderOptions{HistoryLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	defer p.Close()
	opened := openContent(t, p, session.ID, nil)
	defer opened.Close()
	for _, id := range []string{"first", "second"} {
		if _, err := store.AppendItem(session.ID, sessions.SessionItemFromMessage(id, model.Message{Role: model.MessageRoleUser, Content: id})); err != nil {
			t.Fatal(err)
		}
		change := nextChange(t, opened)
		if id == "second" {
			var removed, upserted bool
			for _, operation := range change.Change.Operations {
				removed = removed || operation.Op == OpItemRemove
				upserted = upserted || operation.Op == OpItemUpsert
			}
			if !removed || !upserted {
				t.Fatalf("window slide operations = %#v, want item.remove and item.upsert", change.Change.Operations)
			}
		}
	}
	latest, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest.DisplayName = "Renamed"
	if _, err := store.SaveMetadata(latest); err != nil {
		t.Fatal(err)
	}
	change := nextChange(t, opened)
	if len(change.Change.Operations) == 0 || change.Change.Operations[0].Op != OpMetadataReplace {
		t.Fatalf("metadata operations = %#v, want metadata.replace", change.Change.Operations)
	}
}

func TestNoConsumerResetAndSlowSubscriberAreBounded(t *testing.T) {
	store, session := newContentTestStore(t, "session-bounded")
	p, err := NewProvider(store, ProviderOptions{LiveCapacity: 1, ProjectorQueueCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	defer p.Close()
	opened := openContent(t, p, session.ID, nil)
	oldEpoch := opened.StreamEpoch
	opened.Close()
	if _, err := store.AppendItem(session.ID, sessions.SessionItemFromMessage("without-consumer", model.Message{Role: model.MessageRoleUser, Content: "not encoded as a change"})); err != nil {
		t.Fatal(err)
	}
	reset := openContent(t, p, session.ID, &protocol.ResumeToken{StreamEpoch: oldEpoch, Sequence: protocol.Sequence("0")})
	if reset.StreamEpoch == oldEpoch || reset.Decision.Action != syncengine.SyncActionResync {
		t.Fatalf("no-consumer reopen = epoch %q decision %#v, want a fresh resync boundary", reset.StreamEpoch, reset.Decision)
	}
	reset.Close()
	slow := openContent(t, p, session.ID, nil)
	for i := 0; i < 3; i++ {
		if _, err := store.AppendItem(session.ID, sessions.SessionItemFromMessage(fmt.Sprintf("slow-%d", i), model.Message{Role: model.MessageRoleUser, Content: "x"})); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case terminal := <-slow.Terminal:
		if terminal.Reason != syncengine.LiveTerminalOverflow {
			t.Fatalf("slow terminal = %#v, want overflow", terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow subscriber was not bounded")
	}
	slow.Close()
}

func TestSessionIsolationAndUnsubscribedSessionHasNoLiveOutput(t *testing.T) {
	store, sessionA := newContentTestStore(t, "session-live-a")
	sessionB, err := store.SaveMetadata(sessions.SessionV2{ID: "session-live-b", ProjectID: "project-a"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(store, ProviderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	defer p.Close()
	a := openContent(t, p, sessionA.ID, nil)
	defer a.Close()
	if _, err := store.AppendItem(sessionB.ID, sessions.SessionItemFromMessage("only-b", model.Message{Role: model.MessageRoleUser, Content: "B"})); err != nil {
		t.Fatal(err)
	}
	select {
	case entry := <-a.Changes:
		t.Fatalf("session A received session B change: %#v", entry)
	case terminal := <-a.Terminal:
		t.Fatalf("session A terminated on session B change: %#v", terminal)
	case <-time.After(100 * time.Millisecond):
	}
	b := openContent(t, p, sessionB.ID, nil)
	defer b.Close()
	if snapshot := decodeSnapshot(t, b); len(snapshot.History.Items) != 1 || snapshot.History.Items[0].Key.ItemID != "only-b" {
		t.Fatalf("B durable snapshot = %#v", snapshot.History.Items)
	}
	if _, err := store.AppendItem(sessionB.ID, sessions.SessionItemFromMessage("second-b", model.Message{Role: model.MessageRoleAssistant, Content: "B2"})); err != nil {
		t.Fatal(err)
	}
	_ = nextChange(t, b)
	select {
	case entry := <-a.Changes:
		t.Fatalf("session A received B live change: %#v", entry)
	case terminal := <-a.Terminal:
		t.Fatalf("session A terminated on B live change: %#v", terminal)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSnapshotBarrierDoesNotDropMutation(t *testing.T) {
	store, session := newContentTestStore(t, "session-barrier")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p, err := NewProvider(store, ProviderOptions{BeforeSnapshot: func(string) {
		once.Do(func() {
			close(entered)
			<-release
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	defer p.Close()
	openedCh := make(chan syncengine.OpenedResource, 1)
	errCh := make(chan error, 1)
	go func() {
		opened, openErr := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil)
		if openErr != nil {
			errCh <- openErr
			return
		}
		openedCh <- opened
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot hook was not reached")
	}
	if _, err := store.AppendItem(session.ID, sessions.SessionItemFromMessage("barrier-item", model.Message{Role: model.MessageRoleUser, Content: "durable"})); err != nil {
		t.Fatal(err)
	}
	close(release)
	var opened syncengine.OpenedResource
	select {
	case opened = <-openedCh:
	case err := <-errCh:
		t.Fatalf("Open() error = %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for barrier open")
	}
	defer opened.Close()
	snapshot := decodeSnapshot(t, opened)
	if len(snapshot.History.Items) != 1 || snapshot.History.Items[0].Key.ItemID != "barrier-item" {
		t.Fatalf("barrier snapshot = %#v", snapshot.History.Items)
	}
}

func TestReplayAndExpiredResume(t *testing.T) {
	store, session := newContentTestStore(t, "session-replay")
	p, err := NewProvider(store, ProviderOptions{JournalEntries: 2, JournalBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	defer p.Close()
	opened := openContent(t, p, session.ID, nil)
	for i := 0; i < 3; i++ {
		item := sessions.SessionItemFromMessage(fmt.Sprintf("item-%d", i), model.Message{Role: model.MessageRoleUser, Content: "x"})
		if _, err := store.AppendItem(session.ID, item); err != nil {
			t.Fatal(err)
		}
		_ = nextChange(t, opened)
	}
	epoch := opened.StreamEpoch
	opened.Close()
	replay := openContent(t, p, session.ID, &protocol.ResumeToken{StreamEpoch: epoch, Sequence: protocol.Sequence("1")})
	if replay.Decision.Action != syncengine.SyncActionReplay {
		t.Fatalf("resume action = %q, want replay", replay.Decision.Action)
	}
	if len(replay.Decision.Entries) != 2 {
		t.Fatalf("replay entries = %d, want 2", len(replay.Decision.Entries))
	}
	replay.Close()
	tooOld := openContent(t, p, session.ID, &protocol.ResumeToken{StreamEpoch: epoch, Sequence: protocol.Sequence("0")})
	if tooOld.Decision.Action != syncengine.SyncActionResync || tooOld.Decision.Classification != syncengine.ResumeTooOld {
		t.Fatalf("too-old decision = %#v", tooOld.Decision)
	}
	tooOld.Close()
}

func TestOversizedLiveChangeIsRejectedBeforeJournalAppend(t *testing.T) {
	store, session := newContentTestStore(t, "session-live-bound")
	p, err := NewProvider(store, ProviderOptions{JournalBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	registration := store.RegisterMutationSink(p)
	defer registration.Unregister()
	defer p.Close()
	opened := openContent(t, p, session.ID, nil)
	defer opened.Close()
	latest, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest.ModelParameters = map[string]any{"large": strings.Repeat("x", 4096)}
	if _, err := store.SaveMetadata(latest); err != nil {
		t.Fatal(err)
	}
	select {
	case terminal := <-opened.Terminal:
		if terminal.Reason != syncengine.LiveTerminalSequence {
			t.Fatalf("oversized change terminal = %#v, want resync sequence terminal", terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("oversized live change was not rejected")
	}
	o := p.owners[session.ID]
	o.mu.Lock()
	stats := o.journal.Stats()
	o.mu.Unlock()
	if stats.LastSequence != 0 || stats.Bytes != 0 {
		t.Fatalf("oversized change mutated bounded journal: %+v", stats)
	}
}

func TestLargeSnapshotUsesBlobAndIsolation(t *testing.T) {
	store, sessionA := newContentTestStore(t, "session-a")
	sessionB, err := store.SaveMetadata(sessions.SessionV2{ID: "session-b", ProjectID: "project-a", DisplayName: "B"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := store.AppendItem(sessionA.ID, sessions.SessionItemFromMessage(fmt.Sprintf("large-%d", i), model.Message{Role: model.MessageRoleUser, Content: strings.Repeat("content-", 200)})); err != nil {
			t.Fatal(err)
		}
	}
	writer := &testBlobWriter{}
	p, err := NewProvider(store, ProviderOptions{InlineSnapshotBytes: 32, BlobWriter: writer})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	a := openContent(t, p, sessionA.ID, nil)
	defer a.Close()
	if _, ok := a.Snapshot.Content.BlobDescriptor(); !ok || writer.calls != 1 {
		t.Fatalf("snapshot content = %#v, blob calls = %d", a.Snapshot.Content, writer.calls)
	}
	b := openContent(t, p, sessionB.ID, nil)
	defer b.Close()
	var bSnapshot Snapshot
	if err := json.Unmarshal(writer.contents[1], &bSnapshot); err != nil {
		t.Fatal(err)
	}
	if len(bSnapshot.History.Items) != 0 {
		t.Fatalf("session B received A content: %#v", bSnapshot.History.Items)
	}
}

func TestUnsubscribeCloseShutdownAndRestart(t *testing.T) {
	root := t.TempDir()
	store := sessions.NewV2Store(root)
	session, err := store.SaveMetadata(sessions.SessionV2{ID: "session-restart", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(store, ProviderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reg := store.RegisterMutationSink(p)
	opened := openContent(t, p, session.ID, nil)
	opened.Close()
	if _, err := store.AppendItem(session.ID, sessions.SessionItemFromMessage("after-close", model.Message{Role: model.MessageRoleUser, Content: "persisted"})); err != nil {
		t.Fatal(err)
	}
	p.Close()
	reg.Unregister()
	p2, err := NewProvider(store, ProviderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	reg2 := store.RegisterMutationSink(p2)
	defer reg2.Unregister()
	opened2 := openContent(t, p2, session.ID, nil)
	defer opened2.Close()
	snapshot := decodeSnapshot(t, opened2)
	if len(snapshot.History.Items) != 1 || snapshot.History.Items[0].Key.ItemID != "after-close" {
		t.Fatalf("restart snapshot = %#v", snapshot.History.Items)
	}
	if err := store.Delete(session.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case terminal := <-opened2.Terminal:
		if terminal.Reason != syncengine.LiveTerminalSequence {
			t.Fatalf("delete terminal = %#v", terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delete terminal")
	}
	if _, err := p2.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: session.ID}, nil); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("open deleted session error = %v, want not found", err)
	}
}

func TestCanonicalOpenNotFoundCapacityAndConcurrentOwnerClaims(t *testing.T) {
	store, sessionA := newContentTestStore(t, "owner-a")
	sessionB, err := store.SaveMetadata(sessions.SessionV2{ID: "owner-b", ProjectID: "project-a", Provider: "codex", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(store, ProviderOptions{MaxOwners: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, id := range []string{" owner-a", "owner-a ", "owner/a", "owner\n-a", "blobs", "blobs."} {
		if _, err := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: id}, nil); err == nil {
			t.Fatalf("Open(%q) succeeded for non-canonical resource key", id)
		}
		if got := len(p.owners); got != 0 {
			t.Fatalf("invalid Open(%q) left owner count %d", id, got)
		}
	}
	if _, err := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: "\xff"}, nil); err == nil {
		t.Fatal("Open(invalid UTF-8) succeeded")
	}
	if _, err := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: "missing-owner"}, nil); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Open(missing) error = %v, want ErrNotFound", err)
	}
	if got := len(p.owners); got != 0 {
		t.Fatalf("not-found Open left owner count %d", got)
	}

	openedA := openContent(t, p, sessionA.ID, nil)
	openedA.Close()
	openedB := openContent(t, p, sessionB.ID, nil)
	if got := len(p.owners); got != 1 || p.owners[sessionB.ID] == nil {
		t.Fatalf("owner eviction = %d owners %#v, want only B", got, p.owners)
	}
	if _, err := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: sessionA.ID}, nil); err == nil {
		t.Fatal("Open(A) evicted an active B owner despite MaxOwners=1")
	}
	openedB.Close()
	openedAAgain := openContent(t, p, sessionA.ID, nil)
	openedAAgain.Close()

	const parallelOpens = 16
	var wg sync.WaitGroup
	errs := make(chan error, parallelOpens)
	for i := 0; i < parallelOpens; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opened, openErr := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: sessionA.ID}, nil)
			if openErr != nil {
				errs <- openErr
				return
			}
			opened.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for openErr := range errs {
		t.Fatalf("concurrent Open() error = %v", openErr)
	}
}

func TestOwnerEvictionDoesNotHoldProviderLockWhileWorkerStops(t *testing.T) {
	store, sessionA := newContentTestStore(t, "owner-lock-a")
	sessionB, err := store.SaveMetadata(sessions.SessionV2{ID: "owner-lock-b", ProjectID: "project-a", Provider: "codex", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	sessionC, err := store.SaveMetadata(sessions.SessionV2{ID: "owner-lock-c", ProjectID: "project-a", Provider: "codex", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var armed atomic.Bool
	p, err := NewProvider(store, ProviderOptions{MaxOwners: 1, BeforeSnapshot: func(string) {
		if !armed.Load() {
			return
		}
		once.Do(func() {
			close(entered)
			<-release
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	openedA := openContent(t, p, sessionA.ID, nil)
	openedA.Close()
	armed.Store(true)
	o := p.owners[sessionA.ID]
	sub, err := syncengine.NewLiveSubscription(o.journal.Epoch(), o.journal.LastSequence(), 1)
	if err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	o.subs[sub] = struct{}{}
	o.mu.Unlock()
	if err := o.enqueue(ownerTask{mutation: &sessions.Mutation{SessionID: sessionA.ID}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("owner worker did not reach the eviction gate")
	}
	o.mu.Lock()
	delete(o.subs, sub)
	o.mu.Unlock()
	sub.Close()

	openB := make(chan syncengine.OpenedResource, 1)
	errB := make(chan error, 1)
	go func() {
		opened, openErr := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: sessionB.ID}, nil)
		if openErr != nil {
			errB <- openErr
			return
		}
		openB <- opened
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		evicting := p.evicting
		p.mu.Unlock()
		if evicting == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	p.mu.Lock()
	evicting := p.evicting
	p.mu.Unlock()
	if evicting != 1 {
		t.Fatal("owner eviction did not enter lock-free close section")
	}
	start := time.Now()
	if _, err := p.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: sessionC.ID}, nil); err == nil {
		t.Fatal("capacity Open(C) succeeded while the only owner was being evicted")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Open(C) waited on an evicted worker while provider lock was held: %s", elapsed)
	}
	close(release)
	select {
	case opened := <-openB:
		opened.Close()
	case openErr := <-errB:
		t.Fatal(openErr)
	case <-time.After(2 * time.Second):
		t.Fatal("evicting Open(B) did not finish after worker release")
	}
}
