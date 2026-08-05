package wsgateway

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

type reservationConnection struct {
	mu        sync.Mutex
	info      ConnectionInfo
	messages  []protocol.Message
	sendErr   error
	messageCh chan protocol.Message
}

type changeErrorConnection struct {
	reservationConnection
	err error
}

func (c *changeErrorConnection) Send(message protocol.Message) error {
	if message.Kind() == protocol.MessageTypeChange {
		return c.err
	}
	return c.reservationConnection.Send(message)
}

type ackOnWrittenConnection struct {
	reservationConnection
	dispatcher         *Dispatcher
	ctx                context.Context
	targetKind         protocol.MessageType
	targetSubscription string
	ackEpoch           string
	ackSequence        protocol.Sequence
	ackOnce            sync.Once
	ackDone            chan struct{}
	ackErr             error
}

type gatedReplayConnection struct {
	reservationConnection
	started chan struct{}
	release chan struct{}
	first   sync.Once
}

func (c *gatedReplayConnection) Send(message protocol.Message) error {
	return c.SendWithOptions(message, SendOptions{})
}

func (c *gatedReplayConnection) SendWithOptions(message protocol.Message, options SendOptions) error {
	if err := c.reservationConnection.Send(message); err != nil {
		return err
	}
	if message.Kind() == protocol.MessageTypeChange {
		c.first.Do(func() {
			close(c.started)
			<-c.release
		})
	}
	if options.OnWritten != nil {
		options.OnWritten()
	}
	return nil
}

func (c *ackOnWrittenConnection) Send(message protocol.Message) error {
	return c.SendWithOptions(message, SendOptions{})
}

func (c *ackOnWrittenConnection) SendWithOptions(message protocol.Message, options SendOptions) error {
	if err := c.reservationConnection.Send(message); err != nil {
		return err
	}
	if options.OnWritten != nil {
		options.OnWritten()
		if message.Kind() == c.targetKind && messageSubscriptionID(message) == c.targetSubscription {
			c.ackOnce.Do(func() {
				go func() {
					c.ackErr = c.dispatcher.Handle(c.ctx, c, protocol.AckMessage{
						Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeAck, ID: "ack-on-written"},
						Payload:  protocol.AckPayload{SubscriptionID: c.targetSubscription, StreamEpoch: c.ackEpoch, Sequence: c.ackSequence},
					})
					close(c.ackDone)
				}()
				<-c.ackDone
			})
		}
	}
	return nil
}

func (c *reservationConnection) Info() ConnectionInfo { return c.info }
func (c *reservationConnection) Send(message protocol.Message) error {
	if c.sendErr != nil {
		return c.sendErr
	}
	c.mu.Lock()
	c.messages = append(c.messages, message)
	c.mu.Unlock()
	if c.messageCh != nil {
		select {
		case c.messageCh <- message:
		default:
		}
	}
	return nil
}

func TestDispatcherInitialSendFailureReleasesReservationAndClosesOnce(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	registry := syncengine.NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	engine, err := syncengine.NewEngine(registry)
	if err != nil {
		t.Fatal(err)
	}
	commandRegistry, err := commands.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{Engine: engine, Commands: commandRegistry})
	if err != nil {
		t.Fatal(err)
	}
	connection := &reservationConnection{info: ConnectionInfo{ConnectionID: "send-failure", Principal: "p"}, sendErr: ErrConnectionClosed}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = dispatcher.Handle(ctx, connection, protocol.SubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe"}, Payload: protocol.SubscribePayload{SubscriptionID: "failed", Resource: protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}}})
	if err != nil {
		t.Fatal(err)
	}
	waitForProviderCloses(t, provider, 1)
	if provider.closed.Load() != 1 {
		t.Fatalf("provider closes=%d, want exactly one", provider.closed.Load())
	}
	if dispatcher.SubscriptionCount() != 0 {
		t.Fatalf("subscriptions after send failure=%d", dispatcher.SubscriptionCount())
	}
	cancel()
	waitForDispatcherState(t, dispatcher, 0, 0)
}

func TestDispatcherLiveSendErrorRemovesSubscriptionAndClosesProvider(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	registry := syncengine.NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	engine, err := syncengine.NewEngine(registry)
	if err != nil {
		t.Fatal(err)
	}
	commandRegistry, err := commands.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	observer := newRecordingObserver()
	dispatcher, err := NewDispatcher(DispatcherOptions{Engine: engine, Commands: commandRegistry, Observer: observer})
	if err != nil {
		t.Fatal(err)
	}
	connection := &changeErrorConnection{
		reservationConnection: reservationConnection{info: ConnectionInfo{ConnectionID: "live-send-error", Principal: "p"}},
		err:                   errors.New("deterministic live send failure"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := dispatcher.Handle(ctx, connection, protocol.SubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe"},
		Payload:  protocol.SubscribePayload{SubscriptionID: "live", Resource: protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}},
	}); err != nil {
		t.Fatal(err)
	}
	waitForDispatcherState(t, dispatcher, 1, 1)
	provider.mutate(t, "1")
	waitForDispatcherState(t, dispatcher, 1, 0)
	waitForProviderCloses(t, provider, 1)
	messages := connection.Messages()
	if len(messages) != 2 || messages[0].Kind() != protocol.MessageTypeSubscribed || messages[1].Kind() != protocol.MessageTypeSnapshot {
		t.Fatalf("live send error messages=%#v", messages)
	}
	closedEvents := 0
	for _, event := range observer.Events() {
		if event.Kind == EventSubscriptionClosed && event.SubscriptionID == "live" {
			closedEvents++
		}
	}
	if closedEvents != 1 {
		t.Fatalf("subscription closed events=%d, want one", closedEvents)
	}
	cancel()
	waitForDispatcherState(t, dispatcher, 0, 0)
}

func TestDispatcherActivatesBeforeAckableInitialFrames(t *testing.T) {
	cases := []struct {
		name       string
		replay     bool
		targetKind protocol.MessageType
		sequence   protocol.Sequence
	}{
		{name: "snapshot", targetKind: protocol.MessageTypeSnapshot, sequence: "0"},
		{name: "replay", replay: true, targetKind: protocol.MessageTypeChange, sequence: "1"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			provider := newDispatcherFakeProvider(t)
			if test.replay {
				provider.mutate(t, "1")
			}
			registry := syncengine.NewProviderRegistry()
			if err := registry.Register(provider); err != nil {
				t.Fatal(err)
			}
			engine, err := syncengine.NewEngine(registry)
			if err != nil {
				t.Fatal(err)
			}
			commandRegistry, err := commands.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			dispatcher, err := NewDispatcher(DispatcherOptions{Engine: engine, Commands: commandRegistry})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			connection := &ackOnWrittenConnection{
				reservationConnection: reservationConnection{info: ConnectionInfo{ConnectionID: "ack-on-written-" + test.name, Principal: "p"}},
				dispatcher:            dispatcher, ctx: ctx, targetKind: test.targetKind, targetSubscription: "ackable", ackEpoch: provider.epoch, ackSequence: test.sequence, ackDone: make(chan struct{}),
			}
			message := protocol.SubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe"}, Payload: protocol.SubscribePayload{SubscriptionID: "ackable", Resource: protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}}}
			if test.replay {
				message.Payload.Resume = &protocol.ResumeToken{StreamEpoch: provider.epoch, Sequence: "0"}
			}
			if err := dispatcher.Handle(ctx, connection, message); err != nil {
				t.Fatal(err)
			}
			select {
			case <-connection.ackDone:
				if connection.ackErr != nil {
					t.Fatal(connection.ackErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ACK was not attempted from writer confirmation")
			}
			waitForDispatcherState(t, dispatcher, 1, 1)
			for _, sent := range connection.Messages() {
				if errorMessage, ok := sent.(protocol.ErrorMessage); ok && errorMessage.Payload.Code == "subscription_not_found" {
					t.Fatalf("initial ACK raced activation: %#v", errorMessage)
				}
			}
			cancel()
			waitForDispatcherState(t, dispatcher, 0, 0)
			waitForProviderCloses(t, provider, 1)
		})
	}
}

func TestDispatcherLongReplayUnsubscribeWaitsInitialBarrier(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	for i := 1; i <= 16; i++ {
		provider.mutate(t, strconv.Itoa(i))
	}
	registry := syncengine.NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	engine, err := syncengine.NewEngine(registry)
	if err != nil {
		t.Fatal(err)
	}
	commandRegistry, err := commands.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{Engine: engine, Commands: commandRegistry})
	if err != nil {
		t.Fatal(err)
	}
	connection := &gatedReplayConnection{
		reservationConnection: reservationConnection{info: ConnectionInfo{ConnectionID: "long-replay", Principal: "p"}},
		started:               make(chan struct{}), release: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscribeDone := make(chan error, 1)
	go func() {
		subscribeDone <- dispatcher.Handle(ctx, connection, protocol.SubscribeMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe"},
			Payload: protocol.SubscribePayload{
				SubscriptionID: "replay",
				Resource:       protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"},
				Resume:         &protocol.ResumeToken{StreamEpoch: provider.epoch, Sequence: "0"},
			},
		})
	}()
	<-connection.started
	unsubscribeDone := make(chan error, 1)
	go func() {
		unsubscribeDone <- dispatcher.Handle(ctx, connection, protocol.UnsubscribeMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeUnsubscribe, ID: "unsubscribe"},
			Payload:  protocol.UnsubscribePayload{SubscriptionID: "replay"},
		})
	}()
	close(connection.release)
	if err := <-unsubscribeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-subscribeDone; err != nil {
		t.Fatal(err)
	}
	messages := connection.Messages()
	unsubscribedIndex := -1
	for index, message := range messages {
		if message.Kind() == protocol.MessageTypeUnsubscribed {
			unsubscribedIndex = index
			break
		}
	}
	if unsubscribedIndex < 0 {
		t.Fatalf("unsubscribed missing from long replay messages=%#v", messages)
	}
	for _, message := range messages[unsubscribedIndex+1:] {
		if message.Kind() == protocol.MessageTypeSubscribed || message.Kind() == protocol.MessageTypeChange || message.Kind() == protocol.MessageTypeSnapshot {
			t.Fatalf("late initial frame after unsubscribe: %s", message.Kind())
		}
	}
	waitForDispatcherState(t, dispatcher, 1, 0)
	cancel()
	waitForDispatcherState(t, dispatcher, 0, 0)
	waitForProviderCloses(t, provider, 1)
}

type terminalOpened struct {
	changes  chan syncengine.JournalEntry
	terminal chan syncengine.LiveTerminal
	close    sync.Once
}

type terminalProvider struct {
	mu     sync.Mutex
	opened *terminalOpened
	closed atomic.Int64
	reason syncengine.LiveTerminalReason
}

func (p *terminalProvider) Type() protocol.ResourceType { return protocol.ResourceTypeProjectIndex }
func (p *terminalProvider) Authorize(context.Context, syncengine.Principal, protocol.ResourceKey) error {
	return nil
}
func (p *terminalProvider) Open(context.Context, protocol.ResourceKey, *protocol.ResumeToken) (syncengine.OpenedResource, error) {
	journal, err := syncengine.NewBoundedJournal("terminal-epoch", 8, 1024)
	if err != nil {
		return syncengine.OpenedResource{}, err
	}
	opened := &terminalOpened{changes: make(chan syncengine.JournalEntry, 1), terminal: make(chan syncengine.LiveTerminal, 1)}
	p.mu.Lock()
	p.opened = opened
	p.mu.Unlock()
	result := syncengine.OpenedResource{Snapshot: syncengine.Snapshot{Content: syncengine.NewInlineSnapshotContent([]byte(`{"items":[]}`)), ResourceRevision: "0"}, StreamEpoch: "terminal-epoch", Sequence: 0, Decision: journal.Decide(nil), LiveFromSequence: 1, Changes: opened.changes, Terminal: opened.terminal}
	result.Close = func() { opened.close.Do(func() { p.closed.Add(1); close(opened.changes); close(opened.terminal) }) }
	return result, nil
}
func (p *terminalProvider) trigger() {
	p.mu.Lock()
	opened := p.opened
	reason := p.reason
	p.mu.Unlock()
	if opened != nil {
		opened.terminal <- syncengine.LiveTerminal{Reason: reason, StreamEpoch: "terminal-epoch", LastSequence: 0}
	}
}

func TestDispatcherTerminalLiveSourceResyncsAndStopsForOverflowAndGap(t *testing.T) {
	for _, reason := range []syncengine.LiveTerminalReason{syncengine.LiveTerminalOverflow, syncengine.LiveTerminalSequence} {
		t.Run(string(reason), func(t *testing.T) {
			provider := &terminalProvider{reason: reason}
			registry := syncengine.NewProviderRegistry()
			if err := registry.Register(provider); err != nil {
				t.Fatal(err)
			}
			engine, err := syncengine.NewEngine(registry)
			if err != nil {
				t.Fatal(err)
			}
			commandRegistry, err := commands.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			dispatcher, err := NewDispatcher(DispatcherOptions{Engine: engine, Commands: commandRegistry})
			if err != nil {
				t.Fatal(err)
			}
			connection := &reservationConnection{info: ConnectionInfo{ConnectionID: "terminal-" + string(reason), Principal: "p"}, messageCh: make(chan protocol.Message, 8)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := dispatcher.Handle(ctx, connection, protocol.SubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe"}, Payload: protocol.SubscribePayload{SubscriptionID: "terminal", Resource: protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}}}); err != nil {
				t.Fatal(err)
			}
			message := waitForMessage(t, connection.messageCh)
			if message.Kind() != protocol.MessageTypeSubscribed {
				t.Fatalf("first message=%s", message.Kind())
			}
			message = waitForMessage(t, connection.messageCh)
			if message.Kind() != protocol.MessageTypeSnapshot {
				t.Fatalf("snapshot=%s", message.Kind())
			}
			provider.trigger()
			message = waitForMessage(t, connection.messageCh)
			resync, ok := message.(protocol.ResyncRequiredMessage)
			if !ok || resync.Payload.Reason != string(reason) {
				t.Fatalf("resync=%#v", message)
			}
			waitForDispatcherState(t, dispatcher, 1, 0)
			waitForClosedCount(t, 1, provider.closed.Load)
			if provider.closed.Load() != 1 {
				t.Fatalf("provider closes=%d", provider.closed.Load())
			}
			cancel()
			waitForDispatcherState(t, dispatcher, 0, 0)
		})
	}
}

func waitForMessage(t *testing.T, messages <-chan protocol.Message) protocol.Message {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(5 * time.Second):
		t.Fatal("message timeout")
		return nil
	}
}
func (c *reservationConnection) Messages() []protocol.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.Message(nil), c.messages...)
}

func TestDispatcherSubscriptionReservationsBoundConcurrentOpen(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	provider.openStarted = make(chan struct{}, 4)
	provider.openGate = make(chan struct{})
	registry := syncengine.NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	engine, err := syncengine.NewEngine(registry)
	if err != nil {
		t.Fatal(err)
	}
	commandsRegistry, err := commands.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{Engine: engine, Commands: commandsRegistry, MaxSubscriptions: 2})
	if err != nil {
		t.Fatal(err)
	}
	connection := &reservationConnection{info: ConnectionInfo{ConnectionID: "reservation-connection", Principal: "p"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resource := protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}
	open := func(id string, result chan<- error) {
		result <- dispatcher.Handle(ctx, connection, protocol.SubscribeMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe-" + id},
			Payload:  protocol.SubscribePayload{SubscriptionID: id, Resource: resource},
		})
	}
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go open("one", firstDone)
	go open("two", secondDone)
	<-provider.openStarted
	<-provider.openStarted
	thirdDone := make(chan error, 1)
	go open("three", thirdDone)
	if err := <-thirdDone; err != nil {
		t.Fatal(err)
	}
	messages := connection.Messages()
	if len(messages) != 1 {
		t.Fatalf("reservation rejection messages=%d, want one", len(messages))
	}
	rejected, ok := messages[0].(protocol.ErrorMessage)
	if !ok || rejected.Payload.Code != "subscription_limit" {
		t.Fatalf("reservation rejection=%#v", messages[0])
	}
	if provider.openCalls.Load() != 2 {
		t.Fatalf("provider Open calls=%d, want 2", provider.openCalls.Load())
	}
	close(provider.openGate)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	waitForDispatcherState(t, dispatcher, 1, 2)
	if got := dispatcher.SubscriptionCount(); got != 2 {
		t.Fatalf("active subscriptions=%d, want 2", got)
	}
	cancel()
	waitForDispatcherState(t, dispatcher, 0, 0)
	waitForProviderCloses(t, provider, 2)
	if provider.closed.Load() != 2 {
		t.Fatalf("provider closes=%d, want 2", provider.closed.Load())
	}
}

func TestDispatcherPendingUnsubscribeCancelsOpenAndReleasesReservation(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	provider.openStarted = make(chan struct{}, 1)
	provider.openGate = make(chan struct{})
	provider.ignoreOpenCancel = true
	registry := syncengine.NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	engine, err := syncengine.NewEngine(registry)
	if err != nil {
		t.Fatal(err)
	}
	commandsRegistry, err := commands.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{Engine: engine, Commands: commandsRegistry, MaxSubscriptions: 1})
	if err != nil {
		t.Fatal(err)
	}
	connection := &reservationConnection{info: ConnectionInfo{ConnectionID: "pending-unsubscribe", Principal: "p"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resource := protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}
	subscribeDone := make(chan error, 1)
	go func() {
		subscribeDone <- dispatcher.Handle(ctx, connection, protocol.SubscribeMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe-pending"},
			Payload:  protocol.SubscribePayload{SubscriptionID: "pending", Resource: resource},
		})
	}()
	<-provider.openStarted
	if err := dispatcher.Handle(ctx, connection, protocol.UnsubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeUnsubscribe, ID: "unsubscribe-pending"},
		Payload:  protocol.UnsubscribePayload{SubscriptionID: "pending"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-subscribeDone; err != nil {
		t.Fatal(err)
	}
	messages := connection.Messages()
	if len(messages) != 1 || messages[0].Kind() != protocol.MessageTypeUnsubscribed {
		t.Fatalf("pending unsubscribe messages=%#v", messages)
	}
	if provider.openCalls.Load() != 1 || dispatcher.SubscriptionCount() != 0 {
		t.Fatalf("pending reservation leaked: opens=%d subscriptions=%d", provider.openCalls.Load(), dispatcher.SubscriptionCount())
	}
	// Cancellation does not release the hard task capacity while the old
	// provider call is still unwinding. The same ID remains a stable duplicate
	// and a stream of different IDs cannot create more Open tasks.
	if err := dispatcher.Handle(ctx, connection, protocol.SubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe-same"},
		Payload:  protocol.SubscribePayload{SubscriptionID: "pending", Resource: resource},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if err := dispatcher.Handle(ctx, connection, protocol.SubscribeMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe-replacement-" + strconv.Itoa(i)},
			Payload:  protocol.SubscribePayload{SubscriptionID: "replacement-" + strconv.Itoa(i), Resource: resource},
		}); err != nil {
			t.Fatal(err)
		}
	}
	messages = connection.Messages()
	if len(messages) != 10 {
		t.Fatalf("replacement while old Open blocked messages=%#v", messages)
	}
	if rejected, ok := messages[1].(protocol.ErrorMessage); !ok || rejected.Payload.Code != "subscription_exists" {
		t.Fatalf("same ID while old Open blocked response=%#v", messages[1])
	}
	for index := 2; index < len(messages); index++ {
		rejected, ok := messages[index].(protocol.ErrorMessage)
		if !ok || rejected.Payload.Code != "subscription_limit" {
			t.Fatalf("replacement while old Open blocked response[%d]=%#v", index, messages[index])
		}
	}
	if provider.openCalls.Load() != 1 {
		t.Fatalf("replacement loop bypassed task capacity: Open calls=%d", provider.openCalls.Load())
	}
	close(provider.openGate)
	waitForProviderCloses(t, provider, 1)
	if err := dispatcher.Handle(ctx, connection, protocol.SubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe-after-gate"},
		Payload:  protocol.SubscribePayload{SubscriptionID: "pending", Resource: resource},
	}); err != nil {
		t.Fatal(err)
	}
	<-provider.openStarted
	waitForDispatcherState(t, dispatcher, 1, 1)
	cancel()
	waitForDispatcherState(t, dispatcher, 0, 0)
	waitForProviderCloses(t, provider, 2)
}

func waitForDispatcherState(t *testing.T, dispatcher *Dispatcher, connections, subscriptions int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if dispatcher.ConnectionCount() == connections && dispatcher.SubscriptionCount() == subscriptions {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("dispatcher state connections=%d subscriptions=%d, want %d/%d", dispatcher.ConnectionCount(), dispatcher.SubscriptionCount(), connections, subscriptions)
		}
	}
}

func waitForProviderCloses(t *testing.T, provider *dispatcherFakeProvider, count int64) {
	waitForClosedCount(t, count, provider.closed.Load)
}

func waitForClosedCount(t *testing.T, count int64, current func() int64) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for current() < count {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("provider closes=%d, want at least %d", current(), count)
		}
	}
}
