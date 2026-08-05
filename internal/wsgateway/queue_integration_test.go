package wsgateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

type ownedFrameEvent struct {
	message protocol.Message
	owner   string
}

type deterministicOwnedConnection struct {
	mu         sync.Mutex
	info       ConnectionInfo
	frames     []ownedFrameEvent
	framesCh   chan ownedFrameEvent
	aChanges   int
	aConfirmed int
	aRecovery  int
}

func (c *deterministicOwnedConnection) Info() ConnectionInfo { return c.info }
func (c *deterministicOwnedConnection) Send(message protocol.Message) error {
	return c.SendWithOptions(message, SendOptions{})
}
func (c *deterministicOwnedConnection) SendWithOptions(message protocol.Message, options SendOptions) error {
	owner := options.SubscriptionID
	if owner == "" {
		owner = messageSubscriptionID(message)
	}
	if change, ok := message.(protocol.ChangeMessage); ok && change.Payload.SubscriptionID == "a" {
		c.mu.Lock()
		c.aChanges++
		changeNumber := c.aChanges
		c.mu.Unlock()
		if changeNumber == 2 {
			c.record(protocol.ResyncRequiredMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeResyncRequired, ID: "recovery-a"}, Payload: protocol.ResyncRequiredPayload{SubscriptionID: "a", Resource: change.Payload.Resource, Reason: "outbound_overflow"}}, "a")
			c.mu.Lock()
			c.aRecovery++
			c.mu.Unlock()
			return ErrSubscriptionDesynced
		}
	}
	c.record(message, owner)
	if options.OnWritten != nil {
		options.OnWritten()
		if owner == "a" && message.Kind() == protocol.MessageTypeChange {
			c.mu.Lock()
			c.aConfirmed++
			c.mu.Unlock()
		}
	}
	return nil
}
func (c *deterministicOwnedConnection) record(message protocol.Message, owner string) {
	event := ownedFrameEvent{message: message, owner: owner}
	c.mu.Lock()
	c.frames = append(c.frames, event)
	c.mu.Unlock()
	select {
	case c.framesCh <- event:
	default:
	}
}
func (c *deterministicOwnedConnection) stats() (aChanges, confirmed, recovery int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.aChanges, c.aConfirmed, c.aRecovery
}

func TestDispatcherQueueOverflowRemovesOnlyOwnerAndKeepsOtherSubscription(t *testing.T) {
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
	connection := &deterministicOwnedConnection{info: ConnectionInfo{ConnectionID: "owned-integration", Principal: "p"}, framesCh: make(chan ownedFrameEvent, 32)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, id := range []string{"a", "b"} {
		if err := dispatcher.Handle(ctx, connection, protocol.SubscribeMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe-" + id}, Payload: protocol.SubscribePayload{SubscriptionID: id, Resource: protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}}}); err != nil {
			t.Fatal(err)
		}
	}
	waitForDispatcherState(t, dispatcher, 1, 2)
	provider.mutate(t, "1")
	waitForOwnedChanges(t, connection, 2)
	provider.mutate(t, "2")
	waitForOwnedEventSet(t, connection, 2, func(event ownedFrameEvent) bool {
		return event.owner == "a" && event.message.Kind() == protocol.MessageTypeResyncRequired
	}, func(event ownedFrameEvent) bool {
		return event.owner == "b" && event.message.Kind() == protocol.MessageTypeChange
	})
	waitForDispatcherState(t, dispatcher, 1, 1)
	aChanges, confirmed, recovery := connection.stats()
	if aChanges != 2 || confirmed != 1 || recovery != 1 {
		t.Fatalf("A overflow stats changes=%d confirmed=%d recovery=%d", aChanges, confirmed, recovery)
	}
	provider.mutate(t, "3")
	waitForOwnedEvent(t, connection, func(event ownedFrameEvent) bool {
		return event.owner == "b" && event.message.Kind() == protocol.MessageTypeChange
	})
	aChanges, confirmed, recovery = connection.stats()
	if aChanges != 2 || confirmed != 1 || recovery != 1 {
		t.Fatalf("A continued after purge changes=%d confirmed=%d recovery=%d", aChanges, confirmed, recovery)
	}
	cancel()
	waitForDispatcherState(t, dispatcher, 0, 0)
	waitForProviderCloses(t, provider, 2)
	if provider.closed.Load() != 2 {
		t.Fatalf("provider close count=%d, want A+B", provider.closed.Load())
	}
}

func waitForOwnedChanges(t *testing.T, connection *deterministicOwnedConnection, count int) {
	t.Helper()
	seen := 0
	deadline := time.After(5 * time.Second)
	for seen < count {
		select {
		case event := <-connection.framesCh:
			if event.message.Kind() == protocol.MessageTypeChange {
				seen++
			}
		case <-deadline:
			t.Fatalf("saw %d/%d owned changes", seen, count)
		}
	}
}
func waitForOwnedEvent(t *testing.T, connection *deterministicOwnedConnection, want func(ownedFrameEvent) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-connection.framesCh:
			if want(event) {
				return
			}
		case <-deadline:
			t.Fatal("owned frame event timeout")
		}
	}
}

func waitForOwnedEventSet(t *testing.T, connection *deterministicOwnedConnection, count int, wants ...func(ownedFrameEvent) bool) {
	t.Helper()
	matched := make([]bool, len(wants))
	remaining := count
	deadline := time.After(5 * time.Second)
	for remaining > 0 {
		select {
		case event := <-connection.framesCh:
			for i, want := range wants {
				if !matched[i] && want(event) {
					matched[i] = true
					remaining--
					break
				}
			}
		case <-deadline:
			t.Fatal("owned frame event set timeout")
		}
	}
}
