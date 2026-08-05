package syncengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

func testChange(revision, operation string) ResourceChange {
	return ResourceChange{
		ResourceRevision: protocol.ResourceRevision(revision),
		Operations:       []protocol.ChangeOperation{{Op: operation}},
	}
}

func testJournal(t *testing.T, config JournalConfig) *Journal {
	t.Helper()
	journal, err := NewJournal(config)
	if err != nil {
		t.Fatalf("new journal: %v", err)
	}
	return journal
}

func appendChanges(t *testing.T, journal *Journal, count int) []JournalEntry {
	t.Helper()
	entries := make([]JournalEntry, 0, count)
	for i := 1; i <= count; i++ {
		entry, err := journal.Append(testChange(fmt.Sprint(i), "upsert"))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestProviderRegistryAuthorizeOpenAndClosedTypes(t *testing.T) {
	provider := newFakeProvider(protocol.ResourceTypeProjectIndex)
	registry := NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(provider); !errors.Is(err, ErrDuplicateProvider) {
		t.Fatalf("duplicate register error = %v, want ErrDuplicateProvider", err)
	}
	if err := registry.Register(newFakeProvider(protocol.ResourceType("not_registered"))); !errors.Is(err, ErrUnknownResourceType) {
		t.Fatalf("unknown register error = %v, want ErrUnknownResourceType", err)
	}
	if _, err := registry.Provider(protocol.ResourceTypeSessionIndex); !errors.Is(err, ErrProviderNotRegistered) {
		t.Fatalf("missing provider error = %v, want ErrProviderNotRegistered", err)
	}
	if err := ValidateResourceKey(protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: " "}); !errors.Is(err, ErrInvalidResourceKey) {
		t.Fatalf("whitespace resource key error = %v, want ErrInvalidResourceKey", err)
	}

	key := protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}
	opened, err := registry.Open(context.Background(), Principal{ID: "user-1"}, key, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if provider.authorizeCalls != 1 || provider.openCalls != 1 {
		t.Fatalf("authorize/open calls = %d/%d, want 1/1", provider.authorizeCalls, provider.openCalls)
	}
	if opened.Decision.Classification != ResumeNoToken || opened.Decision.Action != SyncActionSnapshot {
		t.Fatalf("no-token decision = %+v", opened.Decision)
	}
	opened.Close()
	opened.Close()
	if provider.closeCalls != 1 {
		t.Fatalf("close calls = %d, want one", provider.closeCalls)
	}

	provider.authorizeErr = errors.New("denied")
	if _, err := registry.Open(context.Background(), Principal{ID: "user-2"}, key, nil); !errors.Is(err, provider.authorizeErr) {
		t.Fatalf("authorize error = %v, want wrapped denial", err)
	}
	if provider.openCalls != 1 {
		t.Fatalf("open called after authorization failure: %d", provider.openCalls)
	}
}

func TestProviderRegistryRejectsInvalidOpenAndCopiesSnapshot(t *testing.T) {
	provider := newFakeProvider(protocol.ResourceTypeProjectIndex)
	registry := NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	key := protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}
	opened, err := registry.Open(context.Background(), Principal{}, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	openedBytes := opened.Snapshot.Content.InlineBytes()
	openedBytes[0] = '['
	provider.snapshotBytes[0] = '['
	if got := string(opened.Snapshot.Content.InlineBytes()); got != `{"items":[]}` {
		t.Fatalf("snapshot was not copied at the provider boundary: %q", got)
	}
	opened.Close()

	bad := newFakeProvider(protocol.ResourceTypeSessionIndex)
	bad.invalidOpen = true
	if err := registry.Register(bad); err != nil {
		t.Fatal(err)
	}
	badKey := protocol.ResourceKey{Type: protocol.ResourceTypeSessionIndex, ID: "project"}
	if _, err := registry.Open(context.Background(), Principal{}, badKey, nil); !errors.Is(err, ErrInvalidOpenedResource) {
		t.Fatalf("invalid open error = %v, want ErrInvalidOpenedResource", err)
	}
	if bad.closeCalls != 1 {
		t.Fatalf("invalid open close calls = %d, want 1", bad.closeCalls)
	}
}

func TestFakeProviderOpenUsesResumeDecisionAndLiveCursor(t *testing.T) {
	provider := newFakeProvider(protocol.ResourceTypeProjectIndex)
	registry := NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	key := protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}
	provider.Mutate("1")
	provider.Mutate("2")
	provider.Mutate("3")

	replay, err := registry.Open(context.Background(), Principal{}, key, &protocol.ResumeToken{StreamEpoch: provider.epoch, Sequence: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Sequence != 3 || replay.Decision.Action != SyncActionReplay || len(replay.Decision.Entries) != 2 {
		t.Fatalf("replay open = sequence %d decision %+v", replay.Sequence, replay.Decision)
	}
	if replay.Decision.Entries[0].Sequence != 2 || replay.Decision.Entries[1].Sequence != 3 || replay.Decision.ToSequence != 3 {
		t.Fatalf("replay entries = %+v", replay.Decision.Entries)
	}
	provider.Mutate("4")
	live := <-replay.Changes
	if live.StreamEpoch != provider.epoch || live.Sequence != 4 || live.PreviousSequence != 3 {
		t.Fatalf("live after replay = %+v", live)
	}
	replay.Close()

	current, err := registry.Open(context.Background(), Principal{}, key, &protocol.ResumeToken{StreamEpoch: provider.epoch, Sequence: "4"})
	if err != nil {
		t.Fatal(err)
	}
	if current.Decision.Action != SyncActionCurrent || current.Decision.Classification != ResumeCurrentExact || len(current.Decision.Entries) != 0 {
		t.Fatalf("current open = %+v", current.Decision)
	}
	current.Close()

	oldEpoch, err := registry.Open(context.Background(), Principal{}, key, &protocol.ResumeToken{StreamEpoch: "old-epoch", Sequence: "4"})
	if err != nil {
		t.Fatal(err)
	}
	if oldEpoch.Decision.Action != SyncActionResync || oldEpoch.Decision.Classification != ResumeEpochMismatch || len(oldEpoch.Decision.Entries) != 0 {
		t.Fatalf("epoch resync open = %+v", oldEpoch.Decision)
	}
	oldEpoch.Close()

	provider.journal.Clear()
	tooOld, err := registry.Open(context.Background(), Principal{}, key, &protocol.ResumeToken{StreamEpoch: provider.epoch, Sequence: "3"})
	if err != nil {
		t.Fatal(err)
	}
	if tooOld.Decision.Action != SyncActionResync || tooOld.Decision.Classification != ResumeTooOld {
		t.Fatalf("too-old open = %+v", tooOld.Decision)
	}
	tooOld.Close()
}

func TestJournalSequenceBoundsAndReplayClassification(t *testing.T) {
	journal := testJournal(t, JournalConfig{StreamEpoch: "epoch-1", MaxEntries: 3, MaxBytes: 10000})
	entries := appendChanges(t, journal, 4)
	if entries[0].Sequence != 1 || entries[0].PreviousSequence != 0 {
		t.Fatalf("first entry = %+v", entries[0])
	}
	if entries[3].Sequence != 4 || entries[3].PreviousSequence != 3 {
		t.Fatalf("last entry = %+v", entries[3])
	}
	stats := journal.Stats()
	if stats.Count != 3 || stats.FirstSequence != 2 || stats.LastSequence != 4 || stats.Bytes <= 0 || stats.Bytes > stats.MaxBytes {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	cases := []struct {
		name           string
		token          *protocol.ResumeToken
		classification ResumeClassification
		action         SyncAction
		reason         ResyncReason
		entries        int
	}{
		{"no token", nil, ResumeNoToken, SyncActionSnapshot, ResyncReasonNoResume, 0},
		{"current", &protocol.ResumeToken{StreamEpoch: "epoch-1", Sequence: "4"}, ResumeCurrentExact, SyncActionCurrent, "", 0},
		{"replay", &protocol.ResumeToken{StreamEpoch: "epoch-1", Sequence: "2"}, ResumeReplayAvailable, SyncActionReplay, "", 2},
		{"too old", &protocol.ResumeToken{StreamEpoch: "epoch-1", Sequence: "0"}, ResumeTooOld, SyncActionResync, ResyncReasonTooOld, 0},
		{"epoch mismatch", &protocol.ResumeToken{StreamEpoch: "epoch-old", Sequence: "4"}, ResumeEpochMismatch, SyncActionResync, ResyncReasonEpochMismatch, 0},
		{"ahead", &protocol.ResumeToken{StreamEpoch: "epoch-1", Sequence: "5"}, ResumeAhead, SyncActionResync, ResyncReasonAhead, 0},
		{"invalid sequence", &protocol.ResumeToken{StreamEpoch: "epoch-1", Sequence: "nope"}, ResumeInvalid, SyncActionResync, ResyncReasonInvalidResume, 0},
		{"invalid epoch", &protocol.ResumeToken{StreamEpoch: "", Sequence: "4"}, ResumeInvalid, SyncActionResync, ResyncReasonInvalidResume, 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			decision := journal.Decide(test.token)
			if decision.Classification != test.classification || decision.Action != test.action || decision.Reason != test.reason {
				t.Fatalf("decision = %+v", decision)
			}
			if len(decision.Entries) != test.entries {
				t.Fatalf("replay entries = %d, want %d", len(decision.Entries), test.entries)
			}
			for i, entry := range decision.Entries {
				if entry.Sequence != uint64(i+3) || entry.PreviousSequence != uint64(i+2) {
					t.Fatalf("replay entry %d = %+v", i, entry)
				}
			}
		})
	}
}

func TestJournalCopyIsolationAndOversizedChange(t *testing.T) {
	originalRaw := json.RawMessage(`{"op":"upsert","key":"one"}`)
	expectedRaw := string(originalRaw)
	change := ResourceChange{
		ResourceRevision: "1",
		Operations:       []protocol.ChangeOperation{{Op: "upsert", Raw: originalRaw}},
	}
	probe := testJournal(t, JournalConfig{StreamEpoch: "epoch", MaxEntries: 4, MaxBytes: 10000})
	probeEntry, err := probe.Append(change)
	if err != nil {
		t.Fatal(err)
	}
	if probeEntry.SizeBytes <= 1 {
		t.Fatalf("unexpected encoded size %d", probeEntry.SizeBytes)
	}
	change.Operations[0].Raw[0] = '['
	probeEntry.Change.Operations[0].Raw[0] = '['
	decision := probe.Decide(&protocol.ResumeToken{StreamEpoch: "epoch", Sequence: "0"})
	if got := string(decision.Entries[0].Change.Operations[0].Raw); got != expectedRaw {
		t.Fatalf("journal was not copy isolated: %q", got)
	}
	decision.Entries[0].Change.Operations[0].Raw[0] = '['
	again := probe.Decide(&protocol.ResumeToken{StreamEpoch: "epoch", Sequence: "0"})
	if got := string(again.Entries[0].Change.Operations[0].Raw); got != expectedRaw {
		t.Fatalf("replay decisions share mutable storage: %q", got)
	}

	small := testJournal(t, JournalConfig{StreamEpoch: "epoch", MaxEntries: 4, MaxBytes: probeEntry.SizeBytes - 1})
	if _, err := small.Append(ResourceChange{
		ResourceRevision: "1",
		Operations:       []protocol.ChangeOperation{{Op: "upsert", Raw: json.RawMessage(expectedRaw)}},
	}); !errors.Is(err, ErrChangeTooLarge) {
		t.Fatalf("oversized error = %v, want ErrChangeTooLarge", err)
	}
	stats := small.Stats()
	if stats.LastSequence != 0 || stats.Count != 0 || stats.Bytes != 0 {
		t.Fatalf("oversized append mutated journal: %+v", stats)
	}
}

func TestJournalByteBoundAndClearResetSemantics(t *testing.T) {
	probe := testJournal(t, JournalConfig{StreamEpoch: "epoch-1", MaxEntries: 10, MaxBytes: 10000})
	first, err := probe.Append(testChange("1", "upsert"))
	if err != nil {
		t.Fatal(err)
	}
	journal := testJournal(t, JournalConfig{StreamEpoch: "epoch-1", MaxEntries: 10, MaxBytes: first.SizeBytes})
	if _, err := journal.Append(testChange("1", "upsert")); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(testChange("2", "upsert")); err != nil {
		t.Fatal(err)
	}
	if stats := journal.Stats(); stats.Count != 1 || stats.Bytes > stats.MaxBytes || stats.FirstSequence != 2 {
		t.Fatalf("byte bound not enforced: %+v", stats)
	}

	journal.Clear()
	if stats := journal.Stats(); stats.Count != 0 || stats.LastSequence != 2 {
		t.Fatalf("clear semantics: %+v", stats)
	}
	if decision := journal.Decide(&protocol.ResumeToken{StreamEpoch: "epoch-1", Sequence: "1"}); decision.Classification != ResumeTooOld {
		t.Fatalf("cleared journal decision = %+v", decision)
	}
	if decision := journal.Decide(&protocol.ResumeToken{StreamEpoch: "epoch-1", Sequence: "2"}); decision.Classification != ResumeCurrentExact {
		t.Fatalf("current after clear = %+v", decision)
	}

	if err := journal.Reset("epoch-2"); err != nil {
		t.Fatal(err)
	}
	if journal.Epoch() != "epoch-2" || journal.LastSequence() != 0 || journal.Stats().Count != 0 {
		t.Fatalf("reset semantics: epoch=%q stats=%+v", journal.Epoch(), journal.Stats())
	}
	if err := journal.Reset("epoch-2"); !errors.Is(err, ErrEpochUnchanged) {
		t.Fatalf("same epoch reset error = %v", err)
	}
	if decision := journal.Decide(&protocol.ResumeToken{StreamEpoch: "epoch-1", Sequence: "2"}); decision.Classification != ResumeEpochMismatch || !decision.IsResync() {
		t.Fatalf("old epoch after reset = %+v", decision)
	}
}

func TestJournalConcurrentAppendRemainsContinuous(t *testing.T) {
	const total = 128
	journal := testJournal(t, JournalConfig{StreamEpoch: "epoch", MaxEntries: total, MaxBytes: total * 200})
	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := journal.Append(testChange(fmt.Sprint(i+1), "upsert")); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	decision := journal.Decide(&protocol.ResumeToken{StreamEpoch: "epoch", Sequence: "0"})
	if decision.Action != SyncActionReplay || len(decision.Entries) != total {
		t.Fatalf("concurrent replay = action %q entries %d", decision.Action, len(decision.Entries))
	}
	for i, entry := range decision.Entries {
		wantSequence := uint64(i + 1)
		if entry.Sequence != wantSequence || entry.PreviousSequence != uint64(i) {
			t.Fatalf("entry %d = %+v", i, entry)
		}
	}
}

func TestSubscriptionACKStateTracksFloorAndContiguousSending(t *testing.T) {
	subscription, err := NewSubscriptionState("epoch", 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.Ack("epoch", 50); !errors.Is(err, ErrAckBelowFloor) {
		t.Fatalf("ACK below floor = %v", err)
	}
	if _, err := subscription.Ack("epoch", 0); !errors.Is(err, ErrAckBelowFloor) {
		t.Fatalf("zero ACK below floor = %v", err)
	}
	ack, err := subscription.Ack("epoch", 100)
	if err != nil || ack.Duplicate || !subscription.HasAcknowledged() {
		t.Fatalf("first baseline ACK = %+v, %v", ack, err)
	}
	ack, err = subscription.Ack("epoch", 100)
	if err != nil || !ack.Duplicate {
		t.Fatalf("duplicate baseline ACK = %+v, %v", ack, err)
	}
	if err := subscription.MarkSent("epoch", 103); !errors.Is(err, ErrSentGap) {
		t.Fatalf("jumping MarkSent = %v", err)
	}
	for sequence := uint64(101); sequence <= 103; sequence++ {
		if err := subscription.MarkSent("epoch", sequence); err != nil {
			t.Fatalf("MarkSent(%d): %v", sequence, err)
		}
	}
	if err := subscription.MarkSent("epoch", 103); err != nil {
		t.Fatalf("duplicate MarkSent = %v", err)
	}
	if _, err := subscription.Ack("epoch", 99); !errors.Is(err, ErrAckBelowFloor) {
		t.Fatalf("ACK below floor after sends = %v", err)
	}
	if _, err := subscription.Ack("epoch", 102); err != nil {
		t.Fatalf("forward ACK = %v", err)
	}
	if _, err := subscription.Ack("epoch", 101); !errors.Is(err, ErrAckRegression) {
		t.Fatalf("regressing ACK = %v", err)
	}
	if _, err := subscription.Ack("other", 103); !errors.Is(err, ErrAckEpochMismatch) {
		t.Fatalf("wrong epoch ACK = %v", err)
	}

	replayState, err := NewSubscriptionStateForReplay("epoch", 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replayState.Ack("epoch", 100); !errors.Is(err, ErrAckBelowFloor) {
		t.Fatalf("replay baseline ACK = %v", err)
	}
	if err := replayState.MarkSent("epoch", 101); err != nil {
		t.Fatal(err)
	}
	if _, err := replayState.Ack("epoch", 101); err != nil {
		t.Fatalf("replay first ACK = %v", err)
	}
}

func TestSnapshotContentUnionBlobAndOperationValidation(t *testing.T) {
	inline := NewInlineSnapshotContent([]byte(`{"items":[]}`))
	if err := inline.Validate(); err != nil {
		t.Fatal(err)
	}
	bytesCopy := inline.InlineBytes()
	bytesCopy[0] = '['
	if string(inline.InlineBytes()) != `{"items":[]}` {
		t.Fatal("inline accessor exposed mutable storage")
	}
	if err := (SnapshotContent{}).Validate(); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("empty content error = %v", err)
	}
	if err := (SnapshotContent{Inline: []byte(`[]`)}).Validate(); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("array inline error = %v", err)
	}

	descriptor := protocol.BlobDescriptor{
		ID: "blob-1", URL: "/api/blobs/blob-1", ContentType: "application/json",
		Size: 10, SHA256: "hash", ETag: "etag", ExpiresAt: "2025-01-01T00:00:00Z",
	}
	blob := NewBlobSnapshotContent(descriptor)
	got, ok := blob.BlobDescriptor()
	if !ok || got != descriptor {
		t.Fatalf("blob descriptor = %+v, %v", got, ok)
	}
	got.ID = "changed"
	gotAgain, _ := blob.BlobDescriptor()
	if gotAgain.ID != descriptor.ID {
		t.Fatal("blob accessor exposed mutable descriptor")
	}

	badBlob := descriptor
	badBlob.Size = 9007199254740992
	if err := protocol.ValidateBlobDescriptor(badBlob); err == nil {
		t.Fatal("unsafe blob size was accepted")
	}
	badBlob = descriptor
	badBlob.ExpiresAt = "2025-01-01 00:00:00Z"
	if err := (SnapshotContent{Blob: &badBlob}).Validate(); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("invalid blob timestamp = %v", err)
	}
	if err := (ResourceChange{ResourceRevision: "1", Operations: []protocol.ChangeOperation{{Op: "   "}}}).Validate(); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("whitespace operation = %v", err)
	}
}

type fakeProvider struct {
	mu sync.Mutex

	publishMu sync.Mutex

	resourceType   protocol.ResourceType
	authorizeErr   error
	invalidOpen    bool
	authorizeCalls int
	openCalls      int
	closeCalls     int
	snapshotBytes  []byte
	subscribers    map[*LiveSubscription]struct{}
	journal        *Journal
	epoch          string
	revision       uint64
	capacity       int

	barrierCaptured chan struct{}
	releaseBarrier  chan struct{}
	captureOnce     sync.Once
}

func newFakeProvider(resourceType protocol.ResourceType) *fakeProvider {
	journal, err := NewJournal(JournalConfig{StreamEpoch: "fake-epoch", MaxEntries: 32, MaxBytes: 1 << 20})
	if err != nil {
		panic(err)
	}
	return &fakeProvider{
		resourceType:  resourceType,
		snapshotBytes: []byte(`{"items":[]}`),
		subscribers:   make(map[*LiveSubscription]struct{}),
		journal:       journal,
		epoch:         "fake-epoch",
		capacity:      16,
	}
}

func (p *fakeProvider) Type() protocol.ResourceType { return p.resourceType }

func (p *fakeProvider) Authorize(_ context.Context, _ Principal, _ protocol.ResourceKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorizeCalls++
	return p.authorizeErr
}

func (p *fakeProvider) Open(_ context.Context, _ protocol.ResourceKey, resume *protocol.ResumeToken) (OpenedResource, error) {
	p.mu.Lock()
	p.openCalls++
	sequence := p.journal.LastSequence()
	decision := p.journal.Decide(resume)
	live, err := NewLiveSubscription(p.epoch, sequence, p.capacity)
	if err != nil {
		p.mu.Unlock()
		return OpenedResource{}, err
	}
	p.subscribers[live] = struct{}{}
	snapshot := Snapshot{
		Content:          NewInlineSnapshotContent(p.snapshotBytes),
		ResourceRevision: protocol.ResourceRevision(fmt.Sprint(p.revision)),
	}
	if p.invalidOpen {
		p.mu.Unlock()
		return OpenedResource{Snapshot: Snapshot{}, StreamEpoch: p.epoch, Sequence: sequence, Decision: decision, LiveFromSequence: sequence + 1, Changes: live.Delivery().Entries, Terminal: live.Delivery().Terminal, Close: p.closeFunc(live)}, nil
	}
	if p.barrierCaptured != nil {
		p.captureOnce.Do(func() { close(p.barrierCaptured) })
		<-p.releaseBarrier
	}
	opened := OpenedResource{
		Snapshot:         snapshot,
		StreamEpoch:      p.epoch,
		Sequence:         sequence,
		Decision:         decision,
		LiveFromSequence: sequence + 1,
		Changes:          live.Delivery().Entries,
		Terminal:         live.Delivery().Terminal,
		Close:            p.closeFunc(live),
	}
	p.mu.Unlock()
	return opened, nil
}

func (p *fakeProvider) closeFunc(live *LiveSubscription) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			delete(p.subscribers, live)
			p.closeCalls++
			p.mu.Unlock()
			live.Close()
		})
	}
}

// Mutate serializes commit order with publishMu, appends under the owner lock,
// releases that lock, and only then performs nonblocking delivery. A consumer
// can therefore never block the resource mutation path.
func (p *fakeProvider) Mutate(revision string) JournalEntry {
	p.publishMu.Lock()
	p.mu.Lock()
	p.revision++
	entry, err := p.journal.Append(testChange(revision, "upsert"))
	if err != nil {
		p.mu.Unlock()
		p.publishMu.Unlock()
		panic(err)
	}
	subscribers := make([]*LiveSubscription, 0, len(p.subscribers))
	for subscriber := range p.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	p.mu.Unlock()
	for _, subscriber := range subscribers {
		if !subscriber.Offer(entry) && subscriber.Desynced() {
			p.mu.Lock()
			delete(p.subscribers, subscriber)
			p.mu.Unlock()
		}
	}
	p.publishMu.Unlock()
	return entry
}

func TestFakeProviderAtomicSnapshotBarrierAndNonblockingPublish(t *testing.T) {
	provider := newFakeProvider(protocol.ResourceTypeProjectIndex)
	provider.capacity = 1
	provider.barrierCaptured = make(chan struct{})
	provider.releaseBarrier = make(chan struct{})
	registry := NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}

	openedCh := make(chan OpenedResource, 1)
	errCh := make(chan error, 1)
	go func() {
		opened, err := registry.Open(context.Background(), Principal{ID: "test"}, protocol.ResourceKey{
			Type: protocol.ResourceTypeProjectIndex,
			ID:   "server",
		}, nil)
		if err != nil {
			errCh <- err
			return
		}
		openedCh <- opened
	}()
	<-provider.barrierCaptured

	mutationDone := make(chan struct{})
	var mutation JournalEntry
	go func() {
		mutation = provider.Mutate("1")
		close(mutationDone)
	}()
	close(provider.releaseBarrier)

	select {
	case err := <-errCh:
		t.Fatal(err)
	case opened := <-openedCh:
		if opened.Sequence != 0 || opened.Decision.ToSequence != 0 {
			t.Fatalf("barrier = sequence %d decision %+v, want zero", opened.Sequence, opened.Decision)
		}
		<-mutationDone
		if mutation.StreamEpoch != provider.epoch || mutation.Sequence != 1 || mutation.PreviousSequence != 0 || mutation.Sequence != opened.Sequence+1 {
			t.Fatalf("first mutation entry = %+v", mutation)
		}

		// Do not receive the first entry yet: it fills the bounded queue.
		// The second one must complete without a receiver and produce a
		// typed overflow terminal.
		provider.Mutate("2")
		live := <-opened.Changes
		if live.StreamEpoch != provider.epoch || live.Sequence != 1 || live.PreviousSequence != 0 || live.Sequence != opened.Sequence+1 {
			t.Fatalf("first live entry = %+v", live)
		}
		if live.Sequence != mutation.Sequence {
			t.Fatalf("live sequence %d differs from mutation %d", live.Sequence, mutation.Sequence)
		}
		terminal := <-opened.Terminal
		if terminal.Reason != LiveTerminalOverflow || !errors.Is(terminal.Err, ErrLiveDeliveryOverflow) {
			t.Fatalf("overflow terminal = %+v", terminal)
		}
		provider.Mutate("3")
		if _, ok := <-opened.Changes; ok {
			t.Fatal("desynced source delivered a later entry")
		}
		opened.Close()
	}
}
