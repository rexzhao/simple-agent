package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// blockCallProvider blocks exactly one provider call (by call number,
// 1-based) until released; every call emits a plain text answer. The
// blocked call honors ctx so Stop can end a gated run.
type blockCallProvider struct {
	mu      sync.Mutex
	call    int
	block   int
	release chan struct{}
}

func (p *blockCallProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	p.mu.Lock()
	p.call++
	n := p.call
	p.mu.Unlock()
	if n == p.block {
		select {
		case <-p.release:
		case <-ctx.Done():
			// Canceled while gated: an empty (failed) stream.
			events := make(chan model.Event)
			close(events)
			return events, nil
		}
	}
	events := make(chan model.Event, 2)
	go func() {
		defer close(events)
		events <- model.TextDeltaEvent{Text: "answer " + request.Messages[len(request.Messages)-1].Content}
		events <- model.MessageDoneEvent{}
	}()
	return events, nil
}

// TestSteerDuringManualCompactNoPanic pins the P0 fix: a manual
// Compact holds the run slot with running=true and NO events channel.
// The previous shape — running=true with the PREVIOUS run's closed
// channel still referenced — made Steer panic on the send (with s.mu
// held: one recovered panic then wedged the session). Steer must be
// rejected (ErrNoRun: nothing drains the queue until the next Send),
// queue edits must work, and nothing may panic or deadlock.
func TestSteerDuringManualCompactNoPanic(t *testing.T) {
	provider := &blockCallProvider{block: 2, release: make(chan struct{})}
	// maxIterations=1: a steer queued during the (single) iteration hits
	// the budget edge at the final-response branch, where the queue is
	// DELIBERATELY kept for a future run — exactly the leftover-queue
	// state a manual Compact can then run into.
	_, session := newTestAgent(t, provider, nil, func(o *Options) { o.MaxIterations = 1 })

	// Turn 1: settles normally; context now non-empty (compact needs it).
	if err := session.Send("hello"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// Turn 2: BLOCKED mid-run; steer queues against it. Releasing (not
	// Stop) lets the run reach its final response at the budget edge —
	// the queued item survives for a future run.
	if err := session.Send("world"); err != nil {
		t.Fatal(err)
	}
	// Wait until the model call is in flight: the iteration-top drain
	// has passed (a steer landing before it is consumed by THIS run —
	// correct behavior, but the leftover-queue state needs it after).
	waitForStreaming(t, session)
	item, err := session.Steer("queued text")
	if err != nil {
		t.Fatalf("Steer while run active: %v", err)
	}
	close(provider.release)
	waitSettled(t, session)
	if snap := session.Snapshot(); len(snap.Steered) != 1 {
		t.Fatalf("queued steer after budget-edge run: %d, want 1 (kept for a future run)", len(snap.Steered))
	}

	// Manual Compact: block the NEXT provider call (the summary) so the
	// compaction holds the slot while we poke at it. A fresh release
	// channel — the old one was closed to release turn 2, and a closed
	// channel un-blocks every future gate.
	release2 := make(chan struct{})
	provider.mu.Lock()
	provider.block = provider.call + 1
	provider.release = release2
	provider.mu.Unlock()
	compactErr := make(chan error, 1)
	go func() { compactErr <- session.Compact() }()
	// Wait until compaction holds the slot.
	waitForCompacting(t, session, compactErr)

	// THE CASES: none of these may panic (which would wedge s.mu) or
	// deadlock. Steer is rejected — no run will consume the queue.
	if _, err := session.Steer("during compact"); err == nil {
		t.Fatal("Steer during manual compact accepted: nothing drains the queue")
	} else if err != ErrNoRun {
		t.Fatalf("Steer during manual compact: %v, want ErrNoRun", err)
	}
	if err := session.UpdateSteer(item, "edited during compact"); err != nil {
		t.Fatalf("UpdateSteer during compact: %v", err)
	}
	if err := session.WithdrawSteer(item); err != nil {
		t.Fatalf("WithdrawSteer during compact: %v", err)
	}
	// The session stays responsive.
	_ = session.Events()
	_ = session.Snapshot()

	close(release2)
	select {
	case err := <-compactErr:
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Compact never finished — deadlock")
	}

	// And the session is fully usable afterwards.
	if err := session.Send("after compact"); err != nil {
		t.Fatalf("Send after compact: %v", err)
	}
	waitSettled(t, session)
}

func waitForStreaming(t *testing.T, session *Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if session.Running() && session.Snapshot().Streaming != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the model call to start")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitForCompacting(t *testing.T, session *Session, compactErr chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot := session.Snapshot()
		if session.Running() && snapshot.Compacting != nil {
			return
		}
		select {
		case err := <-compactErr:
			t.Fatalf("Compact returned early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the compaction to hold the slot")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
