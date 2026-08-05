package syncengine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	ErrAckBelowFloor      = errors.New("ack is below the subscription floor")
	ErrSentGap            = errors.New("sent sequence skipped an unsent sequence")
	ErrSentBeforeBaseline = errors.New("sent sequence precedes the replay baseline")
)

// SubscriptionState tracks only transport delivery progress for one
// subscription. NewSubscriptionState's baseline is an already sent snapshot
// sequence. It is not a claim that later sequences were sent.
type SubscriptionState struct {
	mu sync.RWMutex

	epoch        string
	lastSent     uint64
	ackFloor     uint64
	nextSequence uint64
	hasSent      bool
	acknowledged uint64
	hasAck       bool
}

// NewSubscriptionState initializes a subscription whose snapshot baseline has
// already been sent. For example, baseline 100 permits ACK 100, then 101,
// but rejects ACK 99 and MarkSent(103) until 101 and 102 are sent.
func NewSubscriptionState(streamEpoch string, baseline uint64) (*SubscriptionState, error) {
	if strings.TrimSpace(streamEpoch) == "" {
		return nil, ErrInvalidEpoch
	}
	if baseline == ^uint64(0) {
		return nil, ErrSequenceExhausted
	}
	return &SubscriptionState{
		epoch:        streamEpoch,
		lastSent:     baseline,
		ackFloor:     baseline,
		nextSequence: baseline + 1,
		hasSent:      true,
	}, nil
}

// NewSubscriptionStateAtSnapshot is the explicit name for the baseline-sent
// constructor. NewSubscriptionState remains the compact form.
func NewSubscriptionStateAtSnapshot(streamEpoch string, snapshotSequence uint64) (*SubscriptionState, error) {
	return NewSubscriptionState(streamEpoch, snapshotSequence)
}

// NewSubscriptionStateForReplay initializes a subscription before replay
// entries are sent. resumeSequence is the last sequence already represented
// by the client's resume token; it is not treated as sent by this subscription.
// The first MarkSent must therefore be resumeSequence+1, and no ACK is valid
// until that first entry has actually been sent.
func NewSubscriptionStateForReplay(streamEpoch string, resumeSequence uint64) (*SubscriptionState, error) {
	if strings.TrimSpace(streamEpoch) == "" {
		return nil, ErrInvalidEpoch
	}
	if resumeSequence == ^uint64(0) {
		return nil, ErrSequenceExhausted
	}
	return &SubscriptionState{
		epoch:        streamEpoch,
		lastSent:     resumeSequence,
		ackFloor:     resumeSequence + 1,
		nextSequence: resumeSequence + 1,
	}, nil
}

// NewSubscriptionStateForLive is the same cursor-before-send semantics used
// when a current resume has no replay entries. The first ACK is impossible
// until the first live entry after the cursor has been sent.
func NewSubscriptionStateForLive(streamEpoch string, cursorSequence uint64) (*SubscriptionState, error) {
	return NewSubscriptionStateForReplay(streamEpoch, cursorSequence)
}

func (s *SubscriptionState) Epoch() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.epoch
}

func (s *SubscriptionState) LastSent() uint64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastSent
}

func (s *SubscriptionState) Acknowledged() uint64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.acknowledged
}

func (s *SubscriptionState) HasAcknowledged() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hasAck
}

// MarkSent records a snapshot or change that the gateway has actually handed
// to its transport writer. A duplicate of the current sent sequence is
// idempotent. Otherwise only the strict next sequence is accepted; this
// prevents a state jump from implying delivery of a missing entry.
func (s *SubscriptionState) MarkSent(streamEpoch string, sequence uint64) error {
	if s == nil {
		return fmt.Errorf("subscription state is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if streamEpoch != s.epoch {
		return ErrSentEpochMismatch
	}
	if s.hasSent && sequence == s.lastSent {
		return nil
	}
	if !s.hasSent && sequence < s.nextSequence {
		return ErrSentBeforeBaseline
	}
	if sequence != s.nextSequence {
		if sequence < s.nextSequence {
			return ErrSentRegression
		}
		return ErrSentGap
	}
	s.lastSent = sequence
	s.hasSent = true
	if sequence == ^uint64(0) {
		s.nextSequence = sequence
	} else {
		s.nextSequence = sequence + 1
	}
	return nil
}

type AckResult struct {
	Duplicate bool
	Sequence  uint64
}

// Ack accepts a sequence only when it belongs to this stream and has already
// been sent. The first ACK has an explicit floor and is not mistaken for a
// duplicate merely because its numeric value is zero. Repeating the current
// ACK is idempotent. A future ACK is rejected rather than advancing delivery
// state speculatively.
func (s *SubscriptionState) Ack(streamEpoch string, sequence uint64) (AckResult, error) {
	if s == nil {
		return AckResult{}, fmt.Errorf("subscription state is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if streamEpoch != s.epoch {
		return AckResult{}, ErrAckEpochMismatch
	}
	if sequence < s.ackFloor {
		return AckResult{}, ErrAckBelowFloor
	}
	if !s.hasSent || sequence > s.lastSent {
		return AckResult{}, ErrAckAhead
	}
	if s.hasAck {
		if sequence < s.acknowledged {
			return AckResult{}, ErrAckRegression
		}
		if sequence == s.acknowledged {
			return AckResult{Duplicate: true, Sequence: sequence}, nil
		}
	}
	s.acknowledged = sequence
	s.hasAck = true
	return AckResult{Sequence: sequence}, nil
}
