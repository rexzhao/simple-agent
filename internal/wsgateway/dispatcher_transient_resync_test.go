package wsgateway

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

// transientResyncProvider returns a durable snapshot/live stream alongside a
// transient-only recovery notice. It models the session-content provider when
// the active-run stream is desynced but the durable resource continuity is
// intact.
type transientResyncProvider struct {
	mu      sync.Mutex
	journal *syncengine.Journal
	epoch   string
	subs    map[*syncengine.LiveSubscription]struct{}
	closed  atomic.Int64
	reason  string
}

func newTransientResyncProvider(t *testing.T, reason string) *transientResyncProvider {
	t.Helper()
	journal, err := syncengine.NewBoundedJournal("transient-epoch", 64, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return &transientResyncProvider{journal: journal, epoch: "transient-epoch", subs: make(map[*syncengine.LiveSubscription]struct{}), reason: reason}
}

func (p *transientResyncProvider) Type() protocol.ResourceType {
	return protocol.ResourceTypeSessionContent
}
func (p *transientResyncProvider) Authorize(context.Context, syncengine.Principal, protocol.ResourceKey) error {
	return nil
}
func (p *transientResyncProvider) Open(ctx context.Context, key protocol.ResourceKey, resume *protocol.ResumeToken) (syncengine.OpenedResource, error) {
	return p.OpenWithRunResume(ctx, key, resume, nil)
}
func (p *transientResyncProvider) OpenWithRunResume(ctx context.Context, key protocol.ResourceKey, resume *protocol.ResumeToken, _ *protocol.RunResumeToken) (syncengine.OpenedResource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sequence := p.journal.LastSequence()
	live, err := syncengine.NewLiveSubscription(p.epoch, sequence, 32)
	if err != nil {
		return syncengine.OpenedResource{}, err
	}
	p.subs[live] = struct{}{}
	decision := p.journal.Decide(resume)
	opened := syncengine.OpenedResource{
		Snapshot:    syncengine.Snapshot{Content: syncengine.NewInlineSnapshotContent([]byte(`{"schema_version":1,"session":{"id":"` + key.ID + `"},"active_run":null}`)), ResourceRevision: protocol.ResourceRevision("0")},
		StreamEpoch: p.epoch, Sequence: sequence, Decision: decision, LiveFromSequence: sequence + 1,
		Changes: live.Delivery().Entries, Terminal: live.Delivery().Terminal,
		TransientResync: p.reason,
	}
	var once sync.Once
	opened.Close = func() {
		once.Do(func() { p.mu.Lock(); delete(p.subs, live); p.closed.Add(1); p.mu.Unlock(); live.Close() })
	}
	return opened, nil
}
func (p *transientResyncProvider) mutate(t *testing.T, revision string) syncengine.JournalEntry {
	t.Helper()
	p.mu.Lock()
	entry, err := p.journal.Append(syncengine.ResourceChange{ResourceRevision: protocol.ResourceRevision(revision), Operations: []protocol.ChangeOperation{{Op: "upsert", Raw: json.RawMessage(`{"op":"upsert","key":"` + revision + `"}`)}}})
	if err != nil {
		p.mu.Unlock()
		t.Fatal(err)
	}
	list := make([]*syncengine.LiveSubscription, 0, len(p.subs))
	for sub := range p.subs {
		list = append(list, sub)
	}
	p.mu.Unlock()
	for _, sub := range list {
		sub.Offer(entry)
	}
	return entry
}

func TestDispatcherTransientResyncKeepsDurableSubscriptionAlive(t *testing.T) {
	provider := newTransientResyncProvider(t, "active_run_recovery_required")
	registry := syncengine.NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	engine, err := syncengine.NewEngine(registry)
	if err != nil {
		t.Fatal(err)
	}
	commandRegistry, err := NewCommandRegistry()
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{Engine: engine, Commands: commandRegistry, Observer: newRecordingObserver()})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := newTestEndpoint(t, Options{Handler: dispatcher, Observer: newRecordingObserver()})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeHello(t, connection, "transient-client")
	if _, ok := readProtocol(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	// A fresh subscribe with an active-run recovery notice must still deliver
	// subscribed + transient resync notice + durable snapshot, and keep the
	// subscription alive for subsequent durable changes.
	writeProtocol(t, connection, protocol.SubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe-t"}, Payload: protocol.SubscribePayload{SubscriptionID: "t", Resource: protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: "session-t"}, Resume: nil}})
	kinds := readKinds(t, connection, 3)
	if kinds[0] != protocol.MessageTypeSubscribed || kinds[1] != protocol.MessageTypeResyncRequired || kinds[2] != protocol.MessageTypeSnapshot {
		t.Fatalf("transient resync order=%v", kinds)
	}
	// The durable subscription must remain live: a subsequent durable change
	// is delivered, not a resubscribe loop.
	provider.mutate(t, "1")
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeChange {
		t.Fatalf("durable change after transient resync=%s", message.Kind())
	}
	if provider.closed.Load() != 0 {
		t.Fatalf("transient resync closed the durable subscription: closes=%d", provider.closed.Load())
	}
}
