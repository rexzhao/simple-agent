package execution

import (
	"encoding/json"
	"strings"
	"sync"
)

const defaultLifecycleSubscriberBuffer = 64

// LifecycleEvent is a durable-state or run lifecycle notification. Payload is
// the complete JSON object sent to an SSE client; it includes Type as well as
// the event-specific fields so consumers can use either the SSE event name or
// the JSON body.
type LifecycleEvent struct {
	Type    string
	Payload []byte
}

// NewLifecycleEvent creates a lifecycle event without retaining references to
// the caller's fields. The payloads used by the service are JSON-compatible;
// if a future caller supplies an unsupported value, the event is reduced to a
// type-only payload rather than allowing a malformed notification to escape.
func NewLifecycleEvent(eventType string, fields map[string]any) LifecycleEvent {
	eventType = strings.TrimSpace(eventType)
	body := make(map[string]any, len(fields)+1)
	body["type"] = eventType
	for key, value := range fields {
		body[key] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		payload = []byte(`{"type":"lifecycle.event_encoding_failed"}`)
	}
	return LifecycleEvent{Type: eventType, Payload: payload}
}

// LifecycleHubOptions controls the bounded queue assigned to each
// subscriber. A subscriber that does not consume quickly enough is removed;
// it must reconnect and bootstrap instead of making an execution coordinator
// wait on a client connection.
type LifecycleHubOptions struct {
	SubscriberBuffer int
}

func (options LifecycleHubOptions) withDefaults() LifecycleHubOptions {
	if options.SubscriberBuffer <= 0 {
		options.SubscriberBuffer = defaultLifecycleSubscriberBuffer
	}
	return options
}

// LifecycleHub is a process-local, best-effort fan-out for session and run
// lifecycle events. It has no replay cursor: clients use bootstrap APIs after
// reconnecting or after a slow subscription is closed.
type LifecycleHub struct {
	mu      sync.Mutex
	closed  bool
	options LifecycleHubOptions
	subs    map[*LifecycleSubscription]struct{}
}

// LifecycleSubscription is one bounded lifecycle event stream. Close is
// idempotent and may be called while the hub is publishing.
type LifecycleSubscription struct {
	hub    *LifecycleHub
	events chan LifecycleEvent
	once   sync.Once
}

func NewLifecycleHub() *LifecycleHub {
	return NewLifecycleHubWithOptions(LifecycleHubOptions{})
}

func NewLifecycleHubWithOptions(options LifecycleHubOptions) *LifecycleHub {
	return &LifecycleHub{
		options: options.withDefaults(),
		subs:    make(map[*LifecycleSubscription]struct{}),
	}
}

// Subscribe registers a bounded subscriber. A subscription created after hub
// shutdown is already closed and receives no events.
func (hub *LifecycleHub) Subscribe() *LifecycleSubscription {
	if hub == nil {
		closed := make(chan LifecycleEvent)
		close(closed)
		return &LifecycleSubscription{events: closed}
	}
	subscription := &LifecycleSubscription{
		hub:    hub,
		events: make(chan LifecycleEvent, hub.options.SubscriberBuffer),
	}
	hub.mu.Lock()
	if hub.closed {
		close(subscription.events)
	} else {
		hub.subs[subscription] = struct{}{}
	}
	hub.mu.Unlock()
	return subscription
}

// Events returns the subscription's receive-only event channel.
func (subscription *LifecycleSubscription) Events() <-chan LifecycleEvent {
	if subscription == nil || subscription.events == nil {
		closed := make(chan LifecycleEvent)
		close(closed)
		return closed
	}
	return subscription.events
}

// C is a short alias for Events for select-heavy consumers.
func (subscription *LifecycleSubscription) C() <-chan LifecycleEvent {
	return subscription.Events()
}

// Close removes the subscription from its hub and closes its event channel.
func (subscription *LifecycleSubscription) Close() {
	if subscription == nil {
		return
	}
	subscription.once.Do(func() {
		if subscription.hub == nil {
			if subscription.events != nil {
				close(subscription.events)
			}
			return
		}
		hub := subscription.hub
		hub.mu.Lock()
		if _, ok := hub.subs[subscription]; ok {
			delete(hub.subs, subscription)
			close(subscription.events)
		}
		hub.mu.Unlock()
	})
}

// Unsubscribe is equivalent to subscription.Close and is provided to keep
// ownership explicit at call sites that manage a hub directly.
func (hub *LifecycleHub) Unsubscribe(subscription *LifecycleSubscription) {
	if subscription != nil {
		subscription.Close()
	}
}

// Publish fans out an event without waiting for any subscriber. A full queue
// is considered a stale/slow client: it is closed and removed so the client
// can reconnect and obtain a fresh bootstrap snapshot.
func (hub *LifecycleHub) Publish(event LifecycleEvent) {
	if hub == nil || strings.TrimSpace(event.Type) == "" || len(event.Payload) == 0 {
		return
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed {
		return
	}
	for subscription := range hub.subs {
		select {
		case subscription.events <- event:
		default:
			delete(hub.subs, subscription)
			close(subscription.events)
		}
	}
}

// Close closes all active subscriptions and rejects new ones.
func (hub *LifecycleHub) Close() {
	if hub == nil {
		return
	}
	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		return
	}
	hub.closed = true
	for subscription := range hub.subs {
		delete(hub.subs, subscription)
		close(subscription.events)
	}
	hub.mu.Unlock()
}
