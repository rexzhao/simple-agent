package syncengine

import (
	"errors"
	"strings"
	"sync"
)

var (
	ErrLiveDeliveryOverflow = errors.New("live delivery overflow; resync required")
	ErrLiveSequenceGap      = errors.New("live delivery sequence gap; resync required")
)

// LiveTerminalReason is a typed terminal condition for a bounded live source.
type LiveTerminalReason string

const (
	LiveTerminalClosed   LiveTerminalReason = "closed"
	LiveTerminalOverflow LiveTerminalReason = "overflow"
	LiveTerminalSequence LiveTerminalReason = "sequence_gap"
)

// LiveTerminal is delivered once, after which the entry channel is closed. An
// overflow is never represented by silently dropping one entry and continuing;
// the consumer must use a snapshot/resync path.
type LiveTerminal struct {
	Reason       LiveTerminalReason
	Err          error
	StreamEpoch  string
	LastSequence uint64
}

// LiveDelivery is a bounded, nonblocking source. Entries are copied before
// enqueueing. Terminal has capacity one and is closed after its single value.
type LiveDelivery struct {
	Entries  <-chan JournalEntry
	Terminal <-chan LiveTerminal
}

// LiveSubscription implements the delivery primitive used by providers. It
// starts immediately after baselineSequence and accepts only the next
// contiguous journal entry. Offer never waits for a consumer.
type LiveSubscription struct {
	mu sync.Mutex

	epoch         string
	nextSequence  uint64
	entries       chan JournalEntry
	terminal      chan LiveTerminal
	closed        bool
	desynced      bool
	lastDelivered uint64
}

func NewLiveSubscription(streamEpoch string, baselineSequence uint64, capacity int) (*LiveSubscription, error) {
	if strings.TrimSpace(streamEpoch) == "" {
		return nil, ErrInvalidEpoch
	}
	if capacity <= 0 {
		return nil, ErrInvalidDeliveryCapacity
	}
	if baselineSequence == ^uint64(0) {
		return nil, ErrSequenceExhausted
	}
	return &LiveSubscription{
		epoch:         streamEpoch,
		nextSequence:  baselineSequence + 1,
		entries:       make(chan JournalEntry, capacity),
		terminal:      make(chan LiveTerminal, 1),
		lastDelivered: baselineSequence,
	}, nil
}

func (s *LiveSubscription) Delivery() LiveDelivery {
	if s == nil {
		return LiveDelivery{}
	}
	return LiveDelivery{Entries: s.entries, Terminal: s.terminal}
}

// Offer is nonblocking. It returns false once the subscription has desynced or
// closed; in either case the terminal channel has the reason for recovery.
func (s *LiveSubscription) Offer(entry JournalEntry) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.desynced {
		return false
	}
	if entry.StreamEpoch != s.epoch || entry.Sequence != s.nextSequence || entry.Sequence == 0 || entry.PreviousSequence != entry.Sequence-1 {
		s.terminateLocked(LiveTerminal{
			Reason:       LiveTerminalSequence,
			Err:          ErrLiveSequenceGap,
			StreamEpoch:  s.epoch,
			LastSequence: s.lastDelivered,
		})
		return false
	}
	select {
	case s.entries <- entry.Clone():
		s.lastDelivered = entry.Sequence
		s.nextSequence++
		return true
	default:
		s.terminateLocked(LiveTerminal{
			Reason:       LiveTerminalOverflow,
			Err:          ErrLiveDeliveryOverflow,
			StreamEpoch:  s.epoch,
			LastSequence: s.lastDelivered,
		})
		return false
	}
}

func (s *LiveSubscription) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.desynced {
		return
	}
	s.terminateLocked(LiveTerminal{
		Reason:       LiveTerminalClosed,
		StreamEpoch:  s.epoch,
		LastSequence: s.lastDelivered,
	})
}

func (s *LiveSubscription) terminateLocked(terminal LiveTerminal) {
	if s.closed || s.desynced {
		return
	}
	if terminal.Reason == LiveTerminalClosed {
		s.closed = true
	} else {
		s.desynced = true
	}
	s.terminal <- terminal
	close(s.entries)
	close(s.terminal)
}

func (s *LiveSubscription) Desynced() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desynced
}
