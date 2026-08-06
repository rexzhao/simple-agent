package syncengine

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

// TransientEvent is a run-local event. It has no durable stream sequence and
// is never acknowledged by SubscriptionState.
type TransientEvent struct {
	RunEpoch string
	RunID    string
	Cursor   protocol.RunCursor
	Event    json.RawMessage
	Bytes    int
}

func (e TransientEvent) Validate() error {
	if e.RunEpoch == "" || e.RunID == "" {
		return fmt.Errorf("transient event run identity is required")
	}
	if err := protocol.ValidateRunCursor(e.Cursor); err != nil {
		return fmt.Errorf("transient event cursor: %w", err)
	}
	if err := protocol.ValidateSubscriptionEvent(e.Event); err != nil {
		return fmt.Errorf("transient event payload: %w", err)
	}
	decoded, err := protocol.DecodeSubscriptionEvent(e.Event)
	if err != nil {
		return err
	}
	if decoded.RunID != e.RunID || decoded.RunCursor != e.Cursor {
		return fmt.Errorf("transient event payload identity does not match delivery identity")
	}
	if e.Bytes <= 0 {
		e.Bytes = len(e.Event)
	}
	if e.Bytes < len(e.Event) {
		return fmt.Errorf("transient event byte accounting is smaller than payload")
	}
	return nil
}

// TransientTerminal is a resource-local end signal. Overflow is terminal for
// this subscription/run only; other subscriptions and the WebSocket control
// channel remain usable.
type TransientTerminal struct {
	Reason error
}

// TransientDelivery is the live run stream attached to one subscription.
// Consume releases its message+byte reservation after the gateway has taken
// ownership of the event. It is intentionally separate from durable live
// delivery so a transient event can never advance durable ACK state.
type TransientDelivery struct {
	Events   <-chan TransientEvent
	Terminal <-chan TransientTerminal
	Consume  func(TransientEvent)
	Close    func()
}

type TransientSubscription struct {
	runEpoch    string
	runID       string
	maxMessages int
	maxBytes    int

	mu          sync.Mutex
	closed      bool
	desynced    bool
	last        uint64
	queuedBytes int
	events      chan TransientEvent
	terminal    chan TransientTerminal
}

func NewTransientSubscription(runEpoch, runID string, last uint64, maxMessages, maxBytes int) (*TransientSubscription, error) {
	if runEpoch == "" || maxMessages <= 0 || maxBytes <= 0 {
		return nil, fmt.Errorf("invalid transient subscription bounds or identity")
	}
	return &TransientSubscription{
		runEpoch: runEpoch, runID: runID, last: last,
		maxMessages: maxMessages, maxBytes: maxBytes,
		events:   make(chan TransientEvent, maxMessages),
		terminal: make(chan TransientTerminal, 1),
	}, nil
}

func (s *TransientSubscription) Delivery() TransientDelivery {
	return TransientDelivery{Events: s.events, Terminal: s.terminal, Consume: s.Consume, Close: s.Close}
}

func (s *TransientSubscription) Offer(event TransientEvent) bool {
	if s == nil {
		return false
	}
	if err := event.Validate(); err != nil {
		s.Desync(err)
		return false
	}
	cursor, err := protocol.ParseUint64Decimal(string(event.Cursor))
	if err != nil {
		s.Desync(err)
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.desynced {
		return false
	}
	decoded, err := protocol.DecodeSubscriptionEvent(event.Event)
	if err != nil {
		s.desyncLocked(err)
		return false
	}
	if decoded.Type == protocol.SubscriptionEventRunStarted && cursor == 1 && event.RunID != s.runID {
		s.runID, s.runEpoch, s.last = event.RunID, event.RunEpoch, 0
	}
	if s.runID == "" {
		if decoded.Type != protocol.SubscriptionEventRunStarted || cursor != 1 {
			s.desyncLocked(fmt.Errorf("transient stream has no active run"))
			return false
		}
		s.runID, s.runEpoch, s.last = event.RunID, event.RunEpoch, 0
	}
	if event.RunEpoch != s.runEpoch || event.RunID != s.runID || cursor != s.last+1 {
		s.desyncLocked(fmt.Errorf("transient cursor is not continuous"))
		return false
	}
	bytes := event.Bytes
	if bytes <= 0 {
		bytes = len(event.Event)
	}
	if len(s.events) >= s.maxMessages || s.queuedBytes+bytes > s.maxBytes {
		s.desyncLocked(fmt.Errorf("transient delivery queue overflow"))
		return false
	}
	select {
	case s.events <- event:
		s.last = cursor
		s.queuedBytes += bytes
		return true
	default:
		s.desyncLocked(fmt.Errorf("transient delivery queue overflow"))
		return false
	}
}

func (s *TransientSubscription) Consume(event TransientEvent) {
	if s == nil {
		return
	}
	bytes := event.Bytes
	if bytes <= 0 {
		bytes = len(event.Event)
	}
	s.mu.Lock()
	if s.queuedBytes >= bytes {
		s.queuedBytes -= bytes
	} else {
		s.queuedBytes = 0
	}
	s.mu.Unlock()
}

func (s *TransientSubscription) Desync(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.desyncLocked(err)
	s.mu.Unlock()
}

func (s *TransientSubscription) desyncLocked(err error) {
	if s.closed || s.desynced {
		return
	}
	s.desynced = true
	select {
	case s.terminal <- TransientTerminal{Reason: err}:
	default:
	}
}

func (s *TransientSubscription) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.events)
	close(s.terminal)
	s.mu.Unlock()
}

func (s *TransientSubscription) RunCursor() protocol.RunCursor {
	if s == nil {
		return "0"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return protocol.RunCursor(fmt.Sprintf("%d", s.last))
}
