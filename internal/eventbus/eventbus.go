package eventbus

import (
	"errors"
	"fmt"
	"sync"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

const (
	KindTurnStarted         = "turn.started"
	KindCompactionRequested = "compaction.requested"
	KindTurnInputReady      = "turn.input_ready"
	KindAssistantReady      = "assistant.ready"
	KindToolResultReady     = "tool_result.ready"
	KindTurnCompleted       = "turn.completed"
	KindTurnInterrupted     = "turn.interrupted"
	KindModelEvent          = "model.event"
)

const defaultSubscriberBuffer = 64

var ErrClosed = errors.New("eventbus closed")

type Event interface {
	Kind() string
}

type DurableEvent interface {
	Event
	durableEvent()
}

type TransientEvent interface {
	Event
	transientEvent()
}

type Publisher interface {
	Publish(Event) error
}

type DurableHandler func(Event) error

type DurableCheckpointHandler func(Event) (int64, error)

type DurableCommitted struct {
	Event Event
	Seq   int64
}

func (e DurableCommitted) Kind() string {
	if e.Event == nil {
		return "durable.committed"
	}
	return e.Event.Kind()
}

// Bus serializes durable events within a single bus. It does not provide
// cross-bus or cross-session locking; callers still need a session-level turn lock.
type Bus struct {
	handler           DurableHandler
	checkpointHandler DurableCheckpointHandler

	durableMu sync.Mutex
	closeOnce sync.Once
	mu        sync.RWMutex
	closed    bool
	done      chan struct{}
	subs      map[chan Event]subscriber
}

type subscriber struct {
	lossless bool
}

func NewBus(handler DurableHandler) *Bus {
	return &Bus{
		handler: handler,
		done:    make(chan struct{}),
		subs:    make(map[chan Event]subscriber),
	}
}

func NewBusWithCheckpoint(handler DurableCheckpointHandler) *Bus {
	return &Bus{
		checkpointHandler: handler,
		done:              make(chan struct{}),
		subs:              make(map[chan Event]subscriber),
	}
}

func (b *Bus) Publish(event Event) error {
	if event == nil {
		return fmt.Errorf("event is required")
	}
	if _, ok := event.(DurableEvent); ok {
		return b.publishDurable(event)
	}
	if _, ok := event.(TransientEvent); ok {
		return b.fanout(event)
	}
	return fmt.Errorf("unsupported event type %T", event)
}

func (b *Bus) Subscribe() <-chan Event {
	return b.subscribe(defaultSubscriberBuffer, false)
}

func (b *Bus) SubscribeLossless(buffer int) <-chan Event {
	if buffer <= 0 {
		buffer = defaultSubscriberBuffer
	}
	return b.subscribe(buffer, true)
}

func (b *Bus) subscribe(buffer int, lossless bool) <-chan Event {
	if buffer < 0 {
		buffer = 0
	}
	ch := make(chan Event, buffer)
	if b.isDone() {
		close(ch)
		return ch
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch
	}
	b.subs[ch] = subscriber{lossless: lossless}
	return ch
}

func (b *Bus) Close() {
	b.closeOnce.Do(func() {
		close(b.done)

		b.durableMu.Lock()
		defer b.durableMu.Unlock()

		b.mu.Lock()
		defer b.mu.Unlock()
		if b.closed {
			return
		}
		b.closed = true
		for ch := range b.subs {
			close(ch)
			delete(b.subs, ch)
		}
	})
}

func (b *Bus) publishDurable(event Event) error {
	b.durableMu.Lock()
	defer b.durableMu.Unlock()

	if err := b.ensureOpen(); err != nil {
		return err
	}
	if b.handler == nil {
		if b.checkpointHandler == nil {
			return fmt.Errorf("durable event handler is required")
		}
		committedSeq, err := b.checkpointHandler(event)
		if err != nil {
			return err
		}
		if committedSeq > 0 {
			return b.fanout(DurableCommitted{Event: event, Seq: committedSeq})
		}
		return b.fanout(event)
	}
	if err := b.handler(event); err != nil {
		return err
	}
	return b.fanout(event)
}

func (b *Bus) fanout(event Event) error {
	if b.isDone() {
		return ErrClosed
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return ErrClosed
	}
	for ch, sub := range b.subs {
		if sub.lossless {
			select {
			case ch <- event:
			case <-b.done:
				return ErrClosed
			}
			continue
		}
		select {
		case ch <- event:
		case <-b.done:
			return ErrClosed
		default:
		}
	}
	return nil
}

func (b *Bus) ensureOpen() error {
	if b.isDone() {
		return ErrClosed
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return ErrClosed
	}
	return nil
}

func (b *Bus) isDone() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

type TurnStarted struct {
	TurnID string
}

func (TurnStarted) Kind() string  { return KindTurnStarted }
func (TurnStarted) durableEvent() {}

type CompactionRequested struct {
	TurnID     string
	Summary    sessions.SessionItem
	Checkpoint sessions.CompactionCheckpoint
	Context    *contextwindow.Metadata
}

func (CompactionRequested) Kind() string  { return KindCompactionRequested }
func (CompactionRequested) durableEvent() {}

type TurnInputReady struct {
	TurnID  string
	Message model.Message
}

func (TurnInputReady) Kind() string  { return KindTurnInputReady }
func (TurnInputReady) durableEvent() {}

type AssistantReady struct {
	TurnID         string
	AgentIteration int
	Message        model.Message
}

func (AssistantReady) Kind() string  { return KindAssistantReady }
func (AssistantReady) durableEvent() {}

type ToolResultReady struct {
	TurnID         string
	AgentIteration int
	Result         model.ToolResult
}

func (ToolResultReady) Kind() string  { return KindToolResultReady }
func (ToolResultReady) durableEvent() {}

type TurnCompleted struct {
	TurnID string
}

func (TurnCompleted) Kind() string  { return KindTurnCompleted }
func (TurnCompleted) durableEvent() {}

type TurnInterrupted struct {
	TurnID string
}

func (TurnInterrupted) Kind() string  { return KindTurnInterrupted }
func (TurnInterrupted) durableEvent() {}

type ModelEvent struct {
	Event model.Event
}

func (ModelEvent) Kind() string    { return KindModelEvent }
func (ModelEvent) transientEvent() {}
