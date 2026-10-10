package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// steerGatedProvider streams a final answer and then blocks on a gate,
// so the test can steer while the run is mid-flight.
type steerGatedProvider struct {
	mu    sync.Mutex
	reqs  []model.Request
	turns int
	gate  chan struct{}
}

func (p *steerGatedProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, request)
	p.turns++
	p.mu.Unlock()

	events := make(chan model.Event, 4)
	go func() {
		defer close(events)
		events <- model.TextDeltaEvent{Text: "answer"}
		<-p.gate // hold the stream open until the test releases it
		events <- model.TextDeltaEvent{Text: " done"}
	}()
	return events, nil
}

func TestSteerInjectsIntoNextRequest(t *testing.T) {
	provider := &steerGatedProvider{gate: make(chan struct{})}
	_, session := newTestAgent(t, provider, nil)

	if err := session.Send("start"); err != nil {
		t.Fatal(err)
	}
	// Wait until the first request is in flight, then steer twice.
	time.Sleep(50 * time.Millisecond)
	id1, err := session.Steer("first steer")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := session.Steer("second steer")
	if err != nil {
		t.Fatal(err)
	}
	close(provider.gate)
	waitSettled(t, session)

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.reqs) < 2 {
		t.Fatalf("expected a followup request, got %d", len(provider.reqs))
	}
	// The second request contains both steered messages, in order,
	// after the original conversation — drain-all semantics.
	last := provider.reqs[len(provider.reqs)-1].Messages
	var texts []string
	for _, m := range last {
		if m.Role == model.MessageRoleUser {
			texts = append(texts, m.Content)
		}
	}
	if len(texts) != 3 || texts[1] != "first steer" || texts[2] != "second steer" {
		t.Fatalf("user messages in followup request = %v", texts)
	}

	// The committed records carry the same ItemIDs minted at enqueue.
	records := sessionRecords(t, session)
	byID := map[string]string{}
	for _, r := range records {
		if r.Type == "user.input" {
			byID[r.ItemID] = r.Content
		}
	}
	if byID[id1] != "first steer" || byID[id2] != "second steer" {
		t.Fatalf("steered records lost their enqueue-time identity: %v", byID)
	}
}

func TestSteerRequiresRun(t *testing.T) {
	provider := &fakeProvider{}
	_, session := newTestAgent(t, provider, nil)
	if _, err := session.Steer("x"); err != ErrNoRun {
		t.Fatalf("idle Steer = %v, want ErrNoRun", err)
	}
}

func TestSteerWithdrawAndUpdate(t *testing.T) {
	provider := &steerGatedProvider{gate: make(chan struct{})}
	_, session := newTestAgent(t, provider, nil)

	if err := session.Send("start"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	keep, err := session.Steer("keep me")
	if err != nil {
		t.Fatal(err)
	}
	drop, err := session.Steer("drop me")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.WithdrawSteer(drop); err != nil {
		t.Fatal(err)
	}
	if err := session.UpdateSteer(keep, "keep me, edited"); err != nil {
		t.Fatal(err)
	}
	if err := session.WithdrawSteer("nonexistent"); err != ErrNotQueued {
		t.Fatalf("withdraw unknown = %v, want ErrNotQueued", err)
	}
	// Updating a withdrawn entry fails: it is gone.
	if err := session.UpdateSteer(drop, "x"); err != ErrNotQueued {
		t.Fatalf("update withdrawn = %v, want ErrNotQueued", err)
	}
	close(provider.gate)
	waitSettled(t, session)

	for _, r := range sessionRecords(t, session) {
		if r.Type == "user.input" && r.ItemID == drop {
			t.Fatal("withdrawn steer was consumed")
		}
	}
	found := false
	for _, r := range sessionRecords(t, session) {
		if r.Type == "user.input" && r.ItemID == keep {
			found = true
			if r.Content != "keep me, edited" {
				t.Fatalf("committed content = %q, want the edited text", r.Content)
			}
		}
	}
	if !found {
		t.Fatal("kept steer was not consumed")
	}
}

// A steer queued in the race window after the final drain check stays
// queued and is delivered by the next run.
func TestSteerLeftoverDeliveredByNextRun(t *testing.T) {
	provider := &steerGatedProvider{gate: make(chan struct{})}
	_, session := newTestAgent(t, provider, nil)

	if err := session.Send("start"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	id, err := session.Steer("late")
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot shows it queued.
	if snap := session.Snapshot(); len(snap.Steered) != 1 || snap.Steered[0].ItemID != id {
		t.Fatalf("queued steer not in snapshot: %+v", snap.Steered)
	}
	close(provider.gate)
	waitSettled(t, session)
	// The leftover remains queued after settle (race window) — it is
	// delivered when the next run starts.
	provider2 := provider
	_ = provider2

	// A second run consumes the leftover at its first iteration.
	if err := session.Send("next"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	delivered := false
	for _, r := range sessionRecords(t, session) {
		if r.ItemID == id && r.Content == "late" {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("leftover steer was never delivered")
	}
}

func sessionRecords(t *testing.T, session *Session) []recordView {
	t.Helper()
	sub := session.Subscribe()
	defer sub.Stop()
	var out []recordView
	for _, r := range sub.Initial() {
		out = append(out, recordView{ItemID: r.ItemID, Type: r.Type, Content: r.Content})
	}
	return out
}

type recordView struct {
	ItemID  string
	Type    string
	Content string
}

var _ = strings.TrimSpace // keep strings imported for future asserts

// TestSteerSettleRaceCanary is a regression sentinel for the send-on-
// closed-channel panic: steering concurrently with runs that settle
// immediately. The Steer send must stay inside the session lock — a
// select with default does not protect a closed channel.
func TestSteerSettleRaceCanary(t *testing.T) {
	// An endless instant provider: followup continuations also consume
	// streams, so a finite turn script would exhaust and fall into the
	// retry backoff.
	_, session := newTestAgent(t, &endlessProvider{}, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = session.Steer("race")
				time.Sleep(200 * time.Microsecond) // throttle: the queue would outgrow the drains
			}
		}()
	}
	for i := 0; i < 50; i++ {
		_ = session.Send("go")
		waitSettled(t, session)
	}
	close(stop)
	wg.Wait()
}

// endlessProvider answers instantly, forever.
type endlessProvider struct{}

func (p *endlessProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event, 1)
	events <- model.TextDeltaEvent{Text: "x"}
	close(events)
	return events, nil
}
