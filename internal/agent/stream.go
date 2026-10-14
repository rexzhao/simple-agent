package agent

import (
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
)

// SessionEvent is one item on the session's single ordered stream. The
// stream is session-level (never per-run), and every event carries a
// sequence number assigned under the session lock in the same critical
// section as the state change it describes — "state-change order =
// stream-event order" is structural, not conventional.
//
// The three concrete types carry NO model or provider information:
// per-session model selection (milestone B/C) means the stream must
// not assume a single model per serve. Model metadata lives only in
// Record payloads (session.created, assistant.message).
type SessionEvent interface{ isSessionEvent() }

// RecordCommitted is a durable record that has been written to storage
// and folded into the state. The Record is the full payload.
type RecordCommitted struct {
	Seq    uint64
	Record storage.Record
}

// Transient is a non-durable event (delta, started, completed, tool,
// steer lifecycle). It may be dropped for slow subscribers.
type Transient struct {
	Seq   uint64
	Event model.Event
}

// Status reports the session's control state. The terminal
// Running=false status for a run is sent on the guaranteed path (not
// droppable) and is the consumer's signal that the run has ended.
type Status struct {
	Seq        uint64
	Running    bool
	Compacting bool
}

func (RecordCommitted) isSessionEvent() {}
func (Transient) isSessionEvent()       {}
func (Status) isSessionEvent()          {}

// streamBuffer is the per-subscriber channel capacity.
const streamBuffer = 1024

// streamWatermark is the fraction of the buffer at which Transient
// events start being dropped, reserving the remainder for
// RecordCommitted and terminal Status (which must not be dropped
// merely because a subscriber is slow).
const streamWatermark = 3 * streamBuffer / 4

// streamSub is one registered stream subscriber.
type streamSub struct {
	events  chan SessionEvent
	lost    chan struct{} // closed only by the dispatcher on eviction
	evicted bool          // dispatcher-local: set during delivery, no lock needed
	// startSeq is the stream seq at registration time (under s.mu):
	// the dispatcher skips events with seq <= startSeq for this
	// subscriber — they are already in its Initial snapshot.
	startSeq uint64
}

// streamStateLocked captures the state half of a snapshot — transient
// slots, steer queue, status flags, and the current stream seq — without
// copying records. The caller holds s.mu. StartSeq is set here: the
// state and the seq are captured in one lock section, so "everything
// with seq <= StartSeq is reflected in this state" is structural.
func (s *Session) streamStateLocked() StreamSnapshot {
	var snap StreamSnapshot
	if s.state.Streaming != nil {
		st := *s.state.Streaming
		snap.Streaming = &st
	}
	if s.state.Running != nil {
		rt := *s.state.Running
		snap.Running = &rt
	}
	if s.state.Compacting != nil {
		ct := *s.state.Compacting
		snap.Compacting = &ct
	}
	snap.Steered = append([]storage.SteeredItem(nil), s.state.Steered...)
	snap.StatusRunning = s.currentRun != nil
	snap.StatusCompacting = s.state.Compacting != nil
	snap.StartSeq = s.streamSeq
	return snap
}

// streamSnapshotLocked captures the full snapshot: the state half plus
// a deep copy of the record history — the shared body of
// registerStreamLocked and SnapshotStream. The caller holds s.mu.
func (s *Session) streamSnapshotLocked() StreamSnapshot {
	snap := s.streamStateLocked()
	snap.Records = append([]storage.Record(nil), s.records...)
	return snap
}

// registerStreamLocked builds the snapshot, records the start seq, and
// registers the subscriber — all under s.mu (the caller holds it).
// Extracted so Stream() and deterministic tests share one code path.
func (s *Session) registerStreamLocked(sub *streamSub) StreamSnapshot {
	snap := s.streamSnapshotLocked()
	if s.streamSubs == nil {
		s.streamSubs = make(map[*streamSub]struct{})
	}
	s.streamSubs[sub] = struct{}{}
	// S1: the subscriber's seq starts AFTER everything already queued.
	// (snap.StartSeq is already s.streamSeq — set by streamStateLocked.)
	sub.startSeq = s.streamSeq
	return snap
}

// SnapshotStream returns an atomic state snapshot in the same shape as
// StreamSubscription.Initial(), WITHOUT subscribing — for consumers
// (the web hub seeding its mirror from a fresh subscription's
// Initial()) that need the atomic copy including the record history.
// It deep-copies every record: fine for hub creation and
// re-subscription (rare), wrong for per-request callers — those want
// SnapshotStreamState.
func (s *Session) SnapshotStream() StreamSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamSnapshotLocked()
}

// SnapshotStreamState is the per-request variant of SnapshotStream:
// the same atomic lock section, but Records is left nil — the callers
// (the hub's connect-time snapshot frame, /api/state) source records
// from their own mirror and must not pay a full-history copy on every
// request or poll.
func (s *Session) SnapshotStreamState() StreamSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamStateLocked()
}

// StreamSnapshot is an atomic state snapshot: records + transient
// slots + steer queue + status + the stream position, all copied
// under one lock section (§14.2). Stream() subscriptions return it
// from Initial(); SnapshotStream/SnapshotStreamState return it
// without subscribing. The structural guarantee: everything enqueued
// with seq <= StartSeq is already reflected in the snapshot's state
// (state change and seq assignment share one lock section), and
// every event with seq > StartSeq arrives later on Events().
type StreamSnapshot struct {
	Records    []storage.Record
	Streaming  *storage.StreamingItem
	Running    *storage.RunningTool
	Compacting *storage.CompactingItem
	Steered    []storage.SteeredItem
	// Status mirrors the transient control state at snapshot time.
	StatusRunning    bool
	StatusCompacting bool
	// StartSeq is the stream seq at snapshot time (under s.mu): every
	// event later delivered on Events() has seq > StartSeq. Also set
	// by the non-subscribing SnapshotStreamState — the hub uses it as
	// the client's startSeq so replayed transients can be filtered
	// (S1's rule, applied one layer down at the SSE fan-out).
	StartSeq uint64
}

// StreamSubscription is the public handle for one stream subscriber:
// Initial() for the connect-time snapshot, Events()/Lost() for the
// ordered delivery, Stop() to release the subscription.
type StreamSubscription struct {
	session  *Session
	sub      *streamSub
	snapshot StreamSnapshot
}

// Initial returns the atomic state snapshot at subscription time:
// records, transient slots, steer queue, and status, all copied in
// the same lock section as the registration. Events delivered later
// on Events() have seq > snapshot.StartSeq — no event is missed or
// duplicated.
func (s *StreamSubscription) Initial() StreamSnapshot { return s.snapshot }

// Events returns the delivery channel. It is never closed; stop
// reading when Lost is closed (or after calling Stop).
func (s *StreamSubscription) Events() <-chan SessionEvent { return s.sub.events }

// Lost is closed by the dispatcher when this subscriber has been
// evicted (buffer overflow on a RecordCommitted or terminal Status).
// After receiving Lost, the caller must NOT read Events further —
// re-Stream() to recover.
func (s *StreamSubscription) Lost() <-chan struct{} { return s.sub.lost }

// Stop ends delivery. Idempotent; does not close Lost (that is the
// dispatcher's sole action).
func (s *StreamSubscription) Stop() {
	s.session.mu.Lock()
	if _, ok := s.session.streamSubs[s.sub]; ok {
		delete(s.session.streamSubs, s.sub)
	}
	s.session.mu.Unlock()
}

// streamQueueItem is one queued event awaiting dispatcher delivery.
type streamQueueItem struct {
	seq  uint64
	ev   SessionEvent
	drop bool // true for Transient below the watermark (delta dropping)
}

// enqueueStream appends one event to the stream queue under s.mu.
// The caller must hold s.mu. Transient events below the watermark are
// pre-marked as droppable (the dispatcher skips them for subscribers
// whose buffer is above the watermark line).
func (s *Session) enqueueStreamLocked(ev SessionEvent, droppable bool) {
	// B3: if the dispatcher was never started (no Stream() subscriber
	// has ever connected), there is no consumer — enqueueing would grow
	// the queue unboundedly. §14.2's "no subscriber: discard" rule.
	// streamStop is set under s.mu in Stream() and only read here under
	// the same lock — no race.
	if s.streamStop == nil {
		return
	}
	s.streamSeq++
	// Populate the Seq field on the concrete type (the public contract:
	// every SessionEvent carries its assigned sequence number).
	switch e := ev.(type) {
	case RecordCommitted:
		e.Seq = s.streamSeq
		ev = e
	case Transient:
		e.Seq = s.streamSeq
		ev = e
	case Status:
		e.Seq = s.streamSeq
		ev = e
	}
	item := streamQueueItem{seq: s.streamSeq, ev: ev, drop: droppable}
	s.streamQueue = append(s.streamQueue, item)
	// Signal the dispatcher (non-blocking: it may already be draining).
	select {
	case s.streamSignal <- struct{}{}:
	default:
	}
}

// streamDispatcher is the sole sender to subscriber channels. It
// drains the queue outside s.mu, delivering events in seq order.
// Transient events are dropped for subscribers above the watermark;
// RecordCommitted and terminal Status evict the subscriber (close
// Lost, remove from the fan-out list).
func (s *Session) streamDispatcher() {
	defer close(s.streamDone)
	for {
		select {
		case <-s.streamStop:
			// Drain any remaining queue, then evict everyone.
			s.drainStreamQueue()
			s.evictAllStreamSubs()
			return
		case <-s.streamSignal:
			s.drainStreamQueue()
		}
	}
}

// drainStreamQueue pops all queued events and delivers them.
func (s *Session) drainStreamQueue() {
	for {
		s.mu.Lock()
		if len(s.streamQueue) == 0 {
			s.mu.Unlock()
			return
		}
		item := s.streamQueue[0]
		s.streamQueue = s.streamQueue[1:]
		var subs []*streamSub
		for sub := range s.streamSubs {
			subs = append(subs, sub)
		}
		s.mu.Unlock()

		for _, sub := range subs {
			s.deliverToSubscriber(sub, item)
		}
	}
}

// deliverToSubscriber sends one event to one subscriber, applying the
// backpressure policy from design §14.2.
func (s *Session) deliverToSubscriber(sub *streamSub, item streamQueueItem) {
	// S1: skip events at or below the subscriber's registration seq —
	// they are already in its Initial snapshot.
	if item.seq <= sub.startSeq {
		return
	}
	// Dispatcher-local evicted check (N2: only this goroutine sets it).
	if sub.evicted {
		return
	}

	// Watermark check for droppable Transient events.
	if item.drop {
		if len(sub.events) >= streamWatermark {
			return // drop this delta; RecordCommitted will catch up
		}
	}

	select {
	case sub.events <- item.ev:
	default:
		// Buffer full. RecordCommitted and terminal Status evict;
		// droppable Transient is silently dropped.
		if !item.drop {
			// Evict: close Lost, remove from fan-out list.
			s.mu.Lock()
			delete(s.streamSubs, sub)
			s.mu.Unlock()
			sub.evicted = true
			close(sub.lost)
		}
	}
}

// evictAllStreamSubs closes Lost for every remaining subscriber.
func (s *Session) evictAllStreamSubs() {
	s.mu.Lock()
	for sub := range s.streamSubs {
		if !sub.evicted {
			sub.evicted = true
			delete(s.streamSubs, sub)
			close(sub.lost)
		}
	}
	s.mu.Unlock()
}

// Stream returns a new stream subscription: a snapshot of the current
// state (records + transient slots + steered queue + status) plus
// every later SessionEvent in strict seq order.
func (s *Session) Stream() *StreamSubscription {
	sub := &streamSub{
		events: make(chan SessionEvent, streamBuffer),
		lost:   make(chan struct{}),
	}

	s.mu.Lock()
	// Post-close behavior: the session is already shut down — return a
	// subscription whose Lost is already closed, so the caller's
	// reconnect loop terminates immediately instead of hanging. This
	// covers both "dispatcher was started then stopped" and "dispatcher
	// was never started" (streamClosed is set by Close regardless).
	if s.streamClosed {
		s.mu.Unlock()
		close(sub.lost)
		return &StreamSubscription{session: s, sub: sub}
	}
	s.streamOnce.Do(func() {
		s.streamSignal = make(chan struct{}, 1)
		s.streamStop = make(chan struct{})
		s.streamDone = make(chan struct{})
		go s.streamDispatcher()
	})

	// S1/S3: build the snapshot, record startSeq, and register —
	// all in one lock section via the shared code path.
	snapshot := s.registerStreamLocked(sub)
	s.mu.Unlock()

	return &StreamSubscription{session: s, sub: sub, snapshot: snapshot}
}

// closeStream stops the dispatcher. Called from Session.Close.
func (s *Session) closeStream() {
	// B4: read the channels under the lock — a concurrent Stream()
	// writes streamStop/streamDone under s.mu, and reading them here
	// without the lock is a data race.
	s.mu.Lock()
	stop, done := s.streamStop, s.streamDone
	s.mu.Unlock()
	if stop == nil {
		return // dispatcher never started
	}
	select {
	case <-stop:
		// Already closed.
	default:
		close(stop)
	}
	<-done
}
