package wsgateway

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

type dispatcherFakeProvider struct {
	mu               sync.Mutex
	journal          *syncengine.Journal
	epoch            string
	revision         uint64
	subs             map[*syncengine.LiveSubscription]struct{}
	closed           atomic.Int64
	openCalls        atomic.Int64
	openStarted      chan struct{}
	openFinished     chan struct{}
	openGate         chan struct{}
	ignoreOpenCancel bool
}

func newDispatcherFakeProvider(t *testing.T) *dispatcherFakeProvider {
	t.Helper()
	journal, err := syncengine.NewBoundedJournal("dispatcher-epoch", 64, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return &dispatcherFakeProvider{journal: journal, epoch: "dispatcher-epoch", subs: make(map[*syncengine.LiveSubscription]struct{})}
}
func (p *dispatcherFakeProvider) Type() protocol.ResourceType {
	return protocol.ResourceTypeProjectIndex
}
func (p *dispatcherFakeProvider) Authorize(context.Context, syncengine.Principal, protocol.ResourceKey) error {
	return nil
}
func (p *dispatcherFakeProvider) Open(ctx context.Context, _ protocol.ResourceKey, resume *protocol.ResumeToken) (syncengine.OpenedResource, error) {
	p.openCalls.Add(1)
	if p.openFinished != nil {
		defer func() {
			select {
			case p.openFinished <- struct{}{}:
			default:
			}
		}()
	}
	if p.openStarted != nil {
		select {
		case p.openStarted <- struct{}{}:
		default:
		}
	}
	if p.openGate != nil {
		if p.ignoreOpenCancel {
			<-p.openGate
		} else {
			select {
			case <-p.openGate:
			case <-ctx.Done():
				return syncengine.OpenedResource{}, ctx.Err()
			}
		}
	}
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
		Snapshot:    syncengine.Snapshot{Content: syncengine.NewInlineSnapshotContent([]byte(`{"items":[]}`)), ResourceRevision: protocol.ResourceRevision("0")},
		StreamEpoch: p.epoch, Sequence: sequence, Decision: decision, LiveFromSequence: sequence + 1,
		Changes: live.Delivery().Entries, Terminal: live.Delivery().Terminal,
	}
	var once sync.Once
	opened.Close = func() {
		once.Do(func() { p.mu.Lock(); delete(p.subs, live); p.closed.Add(1); p.mu.Unlock(); live.Close() })
	}
	return opened, nil
}
func (p *dispatcherFakeProvider) mutate(t *testing.T, revision string) syncengine.JournalEntry {
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
func newDispatcherForTest(t *testing.T, provider *dispatcherFakeProvider, defs ...commands.CommandDefinition) (*testEndpoint, *Dispatcher) {
	return newDispatcherForTestWithOptions(t, provider, nil, defs...)
}

func newDispatcherForTestWithOptions(t *testing.T, provider *dispatcherFakeProvider, configure func(*DispatcherOptions), defs ...commands.CommandDefinition) (*testEndpoint, *Dispatcher) {
	return newDispatcherForTestWithGatewayOptions(t, provider, configure, nil, defs...)
}

func newDispatcherForTestWithGatewayOptions(t *testing.T, provider *dispatcherFakeProvider, configure func(*DispatcherOptions), configureGateway func(*Options), defs ...commands.CommandDefinition) (*testEndpoint, *Dispatcher) {
	t.Helper()
	registry := syncengine.NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	engine, err := syncengine.NewEngine(registry)
	if err != nil {
		t.Fatal(err)
	}
	commandRegistry, err := commands.NewRegistry(defs...)
	if err != nil {
		t.Fatal(err)
	}
	observer := newRecordingObserver()
	dispatcherOptions := DispatcherOptions{Engine: engine, Commands: commandRegistry, Observer: observer}
	if configure != nil {
		configure(&dispatcherOptions)
	}
	dispatcher, err := NewDispatcher(dispatcherOptions)
	if err != nil {
		t.Fatal(err)
	}
	gatewayOptions := Options{Handler: dispatcher, Observer: observer}
	if configureGateway != nil {
		configureGateway(&gatewayOptions)
	}
	endpoint := newTestEndpoint(t, gatewayOptions)
	return endpoint, dispatcher
}

func readKinds(t *testing.T, c *websocket.Conn, count int) []protocol.MessageType {
	t.Helper()
	kinds := make([]protocol.MessageType, 0, count)
	for i := 0; i < count; i++ {
		kinds = append(kinds, readProtocol(t, c).Kind())
	}
	return kinds
}
func writeSubscribe(t *testing.T, c *websocket.Conn, id string, resume *protocol.ResumeToken) {
	t.Helper()
	writeProtocol(t, c, protocol.SubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe-" + id}, Payload: protocol.SubscribePayload{SubscriptionID: id, Resource: protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}, Resume: resume}})
}

func TestDispatcherWebSocketSubscriptionOrderingAndResume(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	endpoint, _ := newDispatcherForTest(t, provider)
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeHello(t, connection, "sync-client")
	if _, ok := readProtocol(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	writeSubscribe(t, connection, "a", nil)
	messages := readKinds(t, connection, 2)
	if messages[0] != protocol.MessageTypeSubscribed || messages[1] != protocol.MessageTypeSnapshot {
		t.Fatalf("initial order=%v", messages)
	}
	// Snapshot baseline ACK succeeds silently; a duplicate also has no
	// success frame, while an epoch mismatch is rejected without advancing it.
	writeProtocol(t, connection, protocol.AckMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeAck, ID: "ack-snapshot"}, Payload: protocol.AckPayload{SubscriptionID: "a", StreamEpoch: provider.epoch, Sequence: "0"}})
	writeProtocol(t, connection, protocol.AckMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeAck, ID: "ack-duplicate"}, Payload: protocol.AckPayload{SubscriptionID: "a", StreamEpoch: provider.epoch, Sequence: "0"}})
	writeProtocol(t, connection, protocol.AckMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeAck, ID: "ack-epoch"}, Payload: protocol.AckPayload{SubscriptionID: "a", StreamEpoch: "wrong-epoch", Sequence: "0"}})
	if rejected, ok := readProtocol(t, connection).(protocol.ErrorMessage); !ok || rejected.Payload.Code != "ack_epoch_mismatch" {
		t.Fatalf("epoch ack=%#v", rejected)
	}
	writeProtocol(t, connection, protocol.AckMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeAck, ID: "ack-ahead"}, Payload: protocol.AckPayload{SubscriptionID: "a", StreamEpoch: provider.epoch, Sequence: protocol.Sequence("1")}})
	if rejected, ok := readProtocol(t, connection).(protocol.ErrorMessage); !ok || rejected.Payload.Code != "ack_ahead" {
		t.Fatalf("ahead ack=%#v", rejected)
	}
	entry := provider.mutate(t, "1")
	change, ok := readProtocol(t, connection).(protocol.ChangeMessage)
	if !ok || string(change.Payload.Sequence) != "1" || string(change.Payload.PreviousSequence) != "0" {
		t.Fatalf("live=%#v", change)
	}
	writeProtocol(t, connection, protocol.AckMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeAck, ID: "ack"}, Payload: protocol.AckPayload{SubscriptionID: "a", StreamEpoch: entry.StreamEpoch, Sequence: protocol.Sequence("1")}})
	writeProtocol(t, connection, protocol.AckMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeAck, ID: "ack-regression"}, Payload: protocol.AckPayload{SubscriptionID: "a", StreamEpoch: entry.StreamEpoch, Sequence: protocol.Sequence("0")}})
	if rejected, ok := readProtocol(t, connection).(protocol.ErrorMessage); !ok || rejected.Payload.Code != "ack_regression" {
		t.Fatalf("regression ack=%#v", rejected)
	}
	// A duplicate ID is rejected without replacing the live provider handle.
	writeSubscribe(t, connection, "a", nil)
	duplicate, ok := readProtocol(t, connection).(protocol.ErrorMessage)
	if !ok || duplicate.Payload.Code != "subscription_exists" {
		t.Fatalf("duplicate=%#v", duplicate)
	}

	writeProtocol(t, connection, protocol.UnsubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeUnsubscribe, ID: "unsub"}, Payload: protocol.UnsubscribePayload{SubscriptionID: "a"}})
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeUnsubscribed {
		t.Fatalf("unsubscribe=%s", message.Kind())
	}
	writeProtocol(t, connection, protocol.UnsubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeUnsubscribe, ID: "unsub-again"}, Payload: protocol.UnsubscribePayload{SubscriptionID: "a"}})
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeUnsubscribed {
		t.Fatalf("idempotent unsubscribe=%s", message.Kind())
	}
	if provider.closed.Load() != 1 {
		t.Fatalf("provider closes=%d", provider.closed.Load())
	}

	// A replay token emits no snapshot and preserves the strict replay-before-live order.
	provider.mutate(t, "2")
	resume := &protocol.ResumeToken{StreamEpoch: provider.epoch, Sequence: "0"}
	writeSubscribe(t, connection, "replay", resume)
	kinds := readKinds(t, connection, 3)
	if kinds[0] != protocol.MessageTypeSubscribed || kinds[1] != protocol.MessageTypeChange {
		t.Fatalf("replay order=%v", kinds)
	}
	if kinds[2] != protocol.MessageTypeChange {
		t.Fatalf("second replay change missing: %v", kinds)
	}
	writeProtocol(t, connection, protocol.AckMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeAck, ID: "ack-floor"}, Payload: protocol.AckPayload{SubscriptionID: "replay", StreamEpoch: provider.epoch, Sequence: "0"}})
	if rejected, ok := readProtocol(t, connection).(protocol.ErrorMessage); !ok || rejected.Payload.Code != "ack_below_floor" {
		t.Fatalf("floor ack=%#v", rejected)
	}
	writeProtocol(t, connection, protocol.UnsubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeUnsubscribe, ID: "unsub-replay"}, Payload: protocol.UnsubscribePayload{SubscriptionID: "replay"}})
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeUnsubscribed {
		t.Fatalf("replay unsubscribe=%s", message.Kind())
	}
	writeSubscribe(t, connection, "current", &protocol.ResumeToken{StreamEpoch: provider.epoch, Sequence: "2"})
	if kinds := readKinds(t, connection, 1); kinds[0] != protocol.MessageTypeSubscribed {
		t.Fatalf("current order=%v", kinds)
	}
	provider.mutate(t, "3")
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeChange {
		t.Fatalf("current live=%s", message.Kind())
	}
	writeProtocol(t, connection, protocol.UnsubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeUnsubscribe, ID: "unsub-current"}, Payload: protocol.UnsubscribePayload{SubscriptionID: "current"}})
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypeUnsubscribed {
		t.Fatalf("current unsubscribe=%s", message.Kind())
	}
	writeSubscribe(t, connection, "resync", &protocol.ResumeToken{StreamEpoch: "old-epoch", Sequence: "99"})
	if kinds := readKinds(t, connection, 3); kinds[0] != protocol.MessageTypeSubscribed || kinds[1] != protocol.MessageTypeResyncRequired || kinds[2] != protocol.MessageTypeSnapshot {
		t.Fatalf("resync order=%v", kinds)
	}
}

func TestDispatcherCommandIdempotencyAcrossConnections(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var executions atomic.Int64
	definition := commands.CommandDefinition{Name: "fake.wait", SchemaVersion: 1, Validate: func(raw json.RawMessage) error { return nil }, Execute: func(ctx context.Context, request commands.CommandRequest) (json.RawMessage, error) {
		executions.Add(1)
		close(started)
		select {
		case <-release:
			return json.RawMessage(`{"ok":true}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	endpoint, _ := newDispatcherForTest(t, provider, definition)
	one := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer one.Close(websocket.StatusNormalClosure, "done")
	two := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer two.Close(websocket.StatusNormalClosure, "done")
	writeHello(t, one, "one")
	_ = readProtocol(t, one)
	writeHello(t, two, "two")
	_ = readProtocol(t, two)
	command := func(id string) protocol.CommandMessage {
		return protocol.CommandMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: id}, Payload: protocol.CommandPayload{Name: "fake.wait", SchemaVersion: 1, RequestID: "same-request", Arguments: json.RawMessage(`{"b":2,"a":1}`)}}
	}
	writeProtocol(t, one, command("cmd-one"))
	if message := readProtocol(t, one); message.Kind() != protocol.MessageTypeCommandAccepted {
		t.Fatalf("accepted=%s message=%#v", message.Kind(), message)
	}
	waitClosed(t, started, "command start")
	writeProtocol(t, two, command("cmd-two"))
	if message := readProtocol(t, two); message.Kind() != protocol.MessageTypeCommandAccepted {
		t.Fatalf("dedup accepted=%s", message.Kind())
	}
	close(release)
	for _, c := range []*websocket.Conn{one, two} {
		message := readProtocol(t, c)
		result, ok := message.(protocol.CommandResultMessage)
		if !ok || result.Payload.Status != protocol.CommandStatusSucceeded {
			t.Fatalf("result=%#v", message)
		}
	}
	if executions.Load() != 1 {
		t.Fatalf("executions=%d", executions.Load())
	}
	// JSON key order and whitespace are semantic equivalents for the same request.
	writeProtocol(t, one, protocol.CommandMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "cmd-retry"}, Payload: protocol.CommandPayload{Name: "fake.wait", SchemaVersion: 1, RequestID: "same-request", Arguments: json.RawMessage(" { \"a\": 1, \"b\": 2 } ")}})
	message := readProtocol(t, one)
	if message.Kind() != protocol.MessageTypeCommandResult {
		t.Fatalf("cached result=%s", message.Kind())
	}
	if executions.Load() != 1 {
		t.Fatal("cached retry executed again")
	}
}

func TestDispatcherRejectsInflightAndCleansResources(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	release := make(chan struct{})
	var count atomic.Int64
	definition := commands.CommandDefinition{Name: "fake.block", SchemaVersion: 1, Execute: func(ctx context.Context, _ commands.CommandRequest) (json.RawMessage, error) {
		count.Add(1)
		select {
		case <-release:
			return json.RawMessage(`{}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	endpoint, dispatcher := newDispatcherForTest(t, provider, definition)
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	writeHello(t, connection, "limits")
	_ = readProtocol(t, connection)
	for i := 0; i < DefaultMaxInflightCommands; i++ {
		writeProtocol(t, connection, protocol.CommandMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "c" + string(rune(i))}, Payload: protocol.CommandPayload{Name: "fake.block", SchemaVersion: 1, RequestID: "r" + string(rune(i)), Arguments: json.RawMessage(`{}`)}})
		if m := readProtocol(t, connection); m.Kind() != protocol.MessageTypeCommandAccepted {
			t.Fatalf("accepted %d=%s message=%#v", i, m.Kind(), m)
		}
	}
	writeProtocol(t, connection, protocol.CommandMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "overflow"}, Payload: protocol.CommandPayload{Name: "fake.block", SchemaVersion: 1, RequestID: "overflow", Arguments: json.RawMessage(`{}`)}})
	if m := readProtocol(t, connection); m.Kind() != protocol.MessageTypeCommandResult || m.(protocol.CommandResultMessage).Payload.Error.Code != "inflight_limit" {
		t.Fatalf("limit=%#v", m)
	}
	close(release)
	for i := 0; i < DefaultMaxInflightCommands; i++ {
		if m := readProtocol(t, connection); m.Kind() != protocol.MessageTypeCommandResult {
			t.Fatalf("result %d=%s", i, m.Kind())
		}
	}
	if count.Load() != DefaultMaxInflightCommands {
		t.Fatalf("executions=%d", count.Load())
	}
	writeSubscribe(t, connection, "cleanup", nil)
	_ = readProtocol(t, connection)
	_ = readProtocol(t, connection)
	if err := connection.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Log(err)
	}
	select {
	case <-endpoint.observer.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("connection did not finish cleanup")
	}
	waitForDispatcherState(t, dispatcher, 0, 0)
	waitForProviderCloses(t, provider, 1)
	if provider.closed.Load() == 0 {
		t.Fatal("connection cleanup did not close provider")
	}
}
