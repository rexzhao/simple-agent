package sessionindex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

func testSession(t *testing.T, store *sessions.V2Store, id, project, name string) sessions.SessionV2 {
	t.Helper()
	state, err := store.SaveMetadata(sessions.SessionV2{ID: id, ProjectID: project, DisplayName: name})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func openIndex(t *testing.T, provider *Provider, project string, resume *protocol.ResumeToken) syncengine.OpenedResource {
	t.Helper()
	opened, err := provider.Open(context.Background(), protocol.ResourceKey{Type: protocol.ResourceTypeSessionIndex, ID: project}, resume)
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func TestSummaryWireUsesNullAndRejectsPartialOrInvalidValues(t *testing.T) {
	summary := SessionSummary{
		SessionID: "s1", ProjectID: "p1", Status: StatusIdle,
		ResourceRevision: "0", UpdatedAt: time.Date(2024, 1, 2, 3, 4, 5, 6, time.UTC),
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || !containsJSON(data, "\"parent_session_id\":null") || !containsJSON(data, "\"run_id\":null") {
		t.Fatalf("empty identifiers did not use null wire semantics: %s", data)
	}
	var decoded SessionSummary
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ParentSessionID != "" || decoded.RunID != "" {
		t.Fatalf("null identifiers decoded as non-empty: %#v", decoded)
	}
	for _, invalid := range []string{
		`{"session_id":"s1","project_id":"p1","parent_session_id":null,"display_name":"","archived":false,"status":"idle","run_id":null,"resource_revision":"0","updated_at":"bad","has_unread_result":false}`,
		`{"session_id":"s1","project_id":"p1","parent_session_id":"","display_name":"","archived":false,"status":"idle","run_id":null,"resource_revision":"0","updated_at":"2024-01-02T03:04:05Z","has_unread_result":false}`,
		`{"session_id":"s1","project_id":"p1","parent_session_id":null,"display_name":"","archived":false,"status":"running","run_id":null,"resource_revision":"0","updated_at":"2024-01-02T03:04:05Z","has_unread_result":false}`,
	} {
		if err := json.Unmarshal([]byte(invalid), &decoded); err == nil {
			t.Fatalf("invalid summary accepted: %s", invalid)
		}
	}
}

func containsJSON(data []byte, value string) bool {
	for i := 0; i+len(value) <= len(data); i++ {
		if string(data[i:i+len(value)]) == value {
			return true
		}
	}
	return false
}

func TestProviderSnapshotFullUpsertReplayAndResync(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	a := testSession(t, store, "a", "p", "A")
	b := testSession(t, store, "b", "p", "B")
	provider, err := NewProvider(store, ProviderOptions{StreamEpoch: "test", JournalEntries: 2, JournalBytes: 1 << 20, LiveCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened := openIndex(t, provider, "p", nil)
	defer opened.Close()
	var snapshot SessionIndexSnapshot
	if err := json.Unmarshal(opened.Snapshot.Content.InlineBytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sessions) != 2 || snapshot.Sessions[0].SessionID != "a" || snapshot.Sessions[1].SessionID != "b" {
		t.Fatalf("deterministic snapshot = %#v", snapshot.Sessions)
	}
	// A durable run settles before the provider is told about it. The resulting
	// operation contains the complete B summary and the current run guard.
	if _, err := store.CreateRun(b.ID, "run-b", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	state, err := store.SetRunStatus(b.ID, "run-b", sessions.RunStatusCommitted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	summary := SummaryFromSession(state, false)
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedRunStarted, ProjectID: "p", SessionID: b.ID, RunID: "run-b", Summary: func() *SessionSummary {
		running := summary
		running.Status = StatusRunning
		running.RunID = "run-b"
		return &running
	}()}); err != nil {
		t.Fatal(err)
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedRunSettled, ProjectID: "p", SessionID: b.ID, RunID: "run-b", Summary: &summary}); err != nil {
		t.Fatal(err)
	}
	startedEntry := <-opened.Changes
	if startedEntry.Sequence != 1 {
		t.Fatalf("started sequence = %d", startedEntry.Sequence)
	}
	entry := <-opened.Changes
	if entry.Change.ResourceRevision != protocol.ResourceRevision("2") {
		t.Fatalf("project resource revision = %s, want 2", entry.Change.ResourceRevision)
	}
	var operation struct {
		Op    string          `json:"op"`
		Key   string          `json:"key"`
		Value *SessionSummary `json:"value"`
	}
	if err := json.Unmarshal(entry.Change.Operations[0].Raw, &operation); err != nil {
		t.Fatal(err)
	}
	if operation.Op != OperationUpsert || operation.Value == nil || operation.Value.SessionID != "b" || operation.Value.Status != StatusCompleted || !operation.Value.HasUnreadResult {
		t.Fatalf("full settled operation = %#v", operation)
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedSessionUpsert, ProjectID: "p", SessionID: "a", Summary: &SessionSummary{
		SessionID: "a", ProjectID: "p", DisplayName: "A renamed", Status: StatusIdle, ResourceRevision: "0", UpdatedAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedSessionRemove, ProjectID: "p", SessionID: "a"}); err != nil {
		t.Fatal(err)
	}
	if opened.Decision.Action != "snapshot" {
		t.Fatalf("unexpected initial decision: %#v", opened.Decision)
	}
	// A fresh open with the retained token replays both changes after the
	// current barrier only when the token is from an older open.
	resume := &protocol.ResumeToken{StreamEpoch: opened.StreamEpoch, Sequence: protocol.Sequence("2")}
	replayed := openIndex(t, provider, "p", resume)
	defer replayed.Close()
	if replayed.Snapshot.ResourceRevision != protocol.ResourceRevision("4") {
		t.Fatalf("snapshot project resource revision = %s, want 4", replayed.Snapshot.ResourceRevision)
	}
	if len(replayed.Decision.Entries) != 2 || replayed.Decision.Entries[0].Change.ResourceRevision != "3" || replayed.Decision.Entries[1].Change.ResourceRevision != "4" {
		t.Fatalf("project revisions did not advance per mutation: %#v", replayed.Decision.Entries)
	}
	old := &protocol.ResumeToken{StreamEpoch: opened.StreamEpoch, Sequence: protocol.Sequence("0")}
	tooOld := openIndex(t, provider, "p", old)
	if !tooOld.Decision.IsResync() || tooOld.Decision.Classification != syncengine.ResumeTooOld {
		t.Fatalf("too-old decision = %#v", tooOld.Decision)
	}
	tooOld.Close()
	_ = a
}

func TestProviderIgnoresLateOldRunAndOverflowsSlowSubscriber(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	state := testSession(t, store, "s", "p", "S")
	provider, err := NewProvider(store, ProviderOptions{StreamEpoch: "test", JournalEntries: 16, JournalBytes: 1 << 20, LiveCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened := openIndex(t, provider, "p", nil)
	defer opened.Close()
	newSummary := SummaryFromSession(state, false)
	newSummary.RunID = "new"
	newSummary.Status = StatusRunning
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedRunStarted, ProjectID: "p", SessionID: "s", RunID: "new", Summary: &newSummary}); err != nil {
		t.Fatal(err)
	}
	oldSummary := newSummary
	oldSummary.RunID = "old"
	oldSummary.Status = StatusCompleted
	oldSummary.HasUnreadResult = true
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedRunSettled, ProjectID: "p", SessionID: "s", RunID: "old", Summary: &oldSummary}); err != nil {
		t.Fatal(err)
	}
	// The stale event neither advances sequence nor fills the bounded queue.
	select {
	case <-opened.Terminal:
		t.Fatal("stale lifecycle event desynced the stream")
	default:
	}
	second := newSummary
	second.ResourceRevision = "1"
	second.DisplayName = "S2"
	if err := provider.PublishCommitted(CommittedChange{Kind: CommittedSessionUpsert, ProjectID: "p", SessionID: "s", Summary: &second}); err != nil {
		t.Fatal(err)
	}
	terminal := <-opened.Terminal
	if terminal.Reason != syncengine.LiveTerminalOverflow {
		t.Fatalf("slow subscriber terminal = %#v", terminal)
	}
	if first := <-opened.Changes; first.Sequence != 1 {
		t.Fatalf("first sequence = %d", first.Sequence)
	}
	if !errors.Is(terminal.Err, syncengine.ErrLiveDeliveryOverflow) {
		t.Fatalf("overflow error = %v", terminal.Err)
	}
}
