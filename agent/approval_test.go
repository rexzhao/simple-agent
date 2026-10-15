package agent

// Approval state machine tests (B2 step 7a, §16.3). The mechanism is
// exercised through the internal entry (awaitApproval) — the policy
// hook that returns Ask is step 7b; no test-only exports exist.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/memory"
)

// approvalTestWorld builds a session with a running tool slot held
// open, the precondition awaitApproval checks.
func approvalTestWorld(t *testing.T) (*Session, *fakeProvider) {
	t.Helper()
	provider := &fakeProvider{turns: nil}
	ag, err := New(Options{
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       memory.OpenStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns", testSessionMetaAgent())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, provider
}

// testSessionMetaAgent is the local literal (the exported constant
// was removed in B2 step 5).
func testSessionMetaAgent() storage.SessionMeta {
	return storage.SessionMeta{Provider: "test", Model: "model-test"}
}

// openToolSlot opens the running slot the way the run path does.
func openToolSlot(session *Session) (string, model.ToolCall) {
	call := model.ToolCall{ID: "c1", Name: "shell", Arguments: `{}`}
	itemID := storage.NewToolResultID()
	session.beginTool(itemID, call)
	return itemID, call
}

// TestApprovalPendingAndSnapshotSameCriticalSection pins the §16.3
// invariant: a subscriber either sees AwaitingApproval in its snapshot
// or receives the Pending event later — never neither. The kill
// (two separate lock sections) opens a window where the snapshot
// misses the flag AND the event has not been enqueued yet.
func TestApprovalPendingAndSnapshotSameCriticalSection(t *testing.T) {
	session, _ := approvalTestWorld(t)
	itemID, call := openToolSlot(session)

	// Subscribe BEFORE the pending state exists: the §14.2 protocol
	// (snapshot + later-seq events) then delivers the Pending event to
	// this subscriber. A subscriber connecting AFTER the section would
	// rebuild from the snapshot (which carries AwaitingApproval) —
	// both paths see the state, never neither.
	sub := session.Stream()
	defer sub.Stop()
	events := sub.Events()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.awaitApproval(ctx, call, itemID)
		done <- err
	}()

	// Record EVERY (StartSeq, flag) sample during the poll: the
	// same-section invariant implies no sample can show StartSeq >=
	// the event's seq while the flag is still false (the section that
	// assigned the seq also set the flag — an event-before-state split
	// violates it whenever sampled in the window), and the first
	// flag-true sample's StartSeq is >= the event's seq (a
	// state-before-event split violates that when sampled).
	deadline := time.Now().Add(2 * time.Second)
	var flagStartSeq uint64
	type sample struct {
		startSeq uint64
		flag     bool
	}
	var samples []sample
	for {
		snap := session.SnapshotStreamState()
		flag := snap.Running != nil && snap.Running.AwaitingApproval
		samples = append(samples, sample{startSeq: snap.StartSeq, flag: flag})
		if flag {
			flagStartSeq = snap.StartSeq
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot never carried AwaitingApproval")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The Pending event arrives on the subscribed stream. Two
	// same-critical-section invariants, both checked at the moment the
	// event is received:
	//  (a) the flag is ALREADY set when the event is received (an
	//      immediate consistency check);
	//  (b) the event's seq is <= the StartSeq of the snapshot that
	//      first showed the flag (a state-before-event split fails
	//      here whenever sampled in the window).
	sawPending := false
	var pendingSeq uint64
	for !sawPending {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case Transient:
				if pe, ok := e.Event.(model.ToolApprovalPendingEvent); ok {
					if pe.ItemID != itemID {
						t.Fatalf("Pending event for %q, want %q", pe.ItemID, itemID)
					}
					pendingSeq = e.Seq
					sawPending = true
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Pending event never arrived on the stream")
		}
	}
	// (a) receiving the event implies the state change already
	// happened (same section) — snapshot now.
	snapNow := session.SnapshotStreamState()
	if snapNow.Running == nil || !snapNow.Running.AwaitingApproval {
		t.Fatal("Pending event received but the flag is not set — the event was enqueued before the state change (split critical sections)")
	}

	// (b) the event's seq <= the first flag-visible snapshot's
	// StartSeq (same section) — a state-before-event split violates it
	// whenever the window is sampled.
	if pendingSeq > flagStartSeq {
		t.Fatalf("Pending event seq %d > flag-visible StartSeq %d — the state change and the event do not share the critical section", pendingSeq, flagStartSeq)
	}
	// (c) no sample shows StartSeq >= the event's seq with the flag
	// still false — an event-before-state split fails here whenever
	// the window is sampled (the client would have neither the
	// snapshot flag nor, later, the already-filtered event).
	for _, sm := range samples {
		if sm.startSeq >= pendingSeq && !sm.flag {
			t.Fatalf("sample StartSeq %d >= event seq %d with the flag false — the event was enqueued before the state change (split critical sections)", sm.startSeq, pendingSeq)
		}
	}

	cancel()
	<-done
}

// TestApprovalResolveApproved: allow=true resolves the wait, the
// Resolved event carries approved, and the flag clears.
func TestApprovalResolveApproved(t *testing.T) {
	session, _ := approvalTestWorld(t)
	itemID, call := openToolSlot(session)

	sub := session.Stream()
	defer sub.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	decision := make(chan approvalDecision, 1)
	go func() {
		d, err := session.awaitApproval(ctx, call, itemID)
		if err != nil {
			t.Errorf("awaitApproval: %v", err)
		}
		decision <- d
	}()

	waitForFlag(t, session, itemID)
	if err := session.ResolveApproval(itemID, true, ""); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	d := <-decision
	if !d.allow {
		t.Fatal("decision.allow = false, want true")
	}
	// The flag cleared.
	snap := session.SnapshotStreamState()
	if snap.Running != nil && snap.Running.AwaitingApproval {
		t.Fatal("AwaitingApproval still set after resolution")
	}
	// The Resolved(approved) event is on the stream.
	resolved := drainResolved(t, sub)
	if resolved.Outcome != ApprovalOutcomeApproved {
		t.Fatalf("outcome = %q, want approved", resolved.Outcome)
	}
}

// TestApprovalResolveDenied: the deny decision carries through.
func TestApprovalResolveDenied(t *testing.T) {
	session, _ := approvalTestWorld(t)
	itemID, call := openToolSlot(session)
	sub := session.Stream()
	defer sub.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	decision := make(chan approvalDecision, 1)
	go func() {
		d, _ := session.awaitApproval(ctx, call, itemID)
		decision <- d
	}()
	waitForFlag(t, session, itemID)
	if err := session.ResolveApproval(itemID, false, "not allowed"); err != nil {
		t.Fatal(err)
	}
	d := <-decision
	if d.allow || d.reason != "not allowed" {
		t.Fatalf("decision = %+v, want deny with reason", d)
	}
	resolved := drainResolved(t, sub)
	if resolved.Outcome != ApprovalOutcomeDenied {
		t.Fatalf("outcome = %q, want denied", resolved.Outcome)
	}
}

// TestApprovalCancel: ctx cancellation resolves as cancelled, leaves
// NO tool.result record (the interruption rule — sanitizeContext
// patches at the next assembly), and clears the pending state so a
// late ResolveApproval gets ErrApprovalUnknown.
func TestApprovalCancel(t *testing.T) {
	session, _ := approvalTestWorld(t)
	itemID, call := openToolSlot(session)
	sub := session.Stream()
	defer sub.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := session.awaitApproval(ctx, call, itemID)
		done <- err
	}()
	waitForFlag(t, session, itemID)

	recordsBefore := len(session.SnapshotStream().Records)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled wait returned nil error")
	}
	// No tool.result was written.
	records := session.SnapshotStream().Records
	if len(records) != recordsBefore {
		t.Fatalf("records grew %d -> %d during a cancelled approval", recordsBefore, len(records))
	}
	for _, r := range records {
		if r.Type == storage.TypeToolResult {
			t.Fatal("a cancelled approval wrote a tool.result")
		}
	}
	// The Resolved(cancelled) event is on the stream.
	resolved := drainResolved(t, sub)
	if resolved.Outcome != ApprovalOutcomeCancelled {
		t.Fatalf("outcome = %q, want cancelled", resolved.Outcome)
	}
	// A late resolve gets Unknown (the entry is gone).
	if err := session.ResolveApproval(itemID, true, ""); !errors.Is(err, ErrApprovalUnknown) {
		t.Fatalf("late resolve: %v, want ErrApprovalUnknown", err)
	}
}

// TestApprovalDoubleResolve: the second answer gets
// ErrApprovalResolved (distinct from unknown). The pending entry is
// set directly (in-package) instead of through awaitApproval — the
// old form raced the waiter's drain under -race -cpu=2 (the waiter
// occasionally consumed the first decision and cleared the entry
// between the two resolves, turning the second into the correct
// ErrApprovalUnknown — but that is not what this test pins). Setting
// the entry directly makes the two resolves purely sequential state
// transitions with no concurrent consumer.
func TestApprovalDoubleResolve(t *testing.T) {
	session, _ := approvalTestWorld(t)
	itemID, call := openToolSlot(session)
	// Install the pending ENTRY without starting a waiter — the same
	// pending-ENTRY shape awaitApproval's flag section produces
	// (itemID, call, buffered(1) decision channel). Note: the flag
	// section ALSO sets Running.AwaitingApproval and emits the Pending
	// event; the double-resolve semantics tested here depend only on
	// the pending entry itself.
	session.mu.Lock()
	session.pendingApproval = &pendingApproval{
		itemID:   itemID,
		call:     call,
		decision: make(chan approvalDecision, 1),
	}
	session.mu.Unlock()

	if err := session.ResolveApproval(itemID, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := session.ResolveApproval(itemID, false, "second"); !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("double resolve: %v, want ErrApprovalResolved", err)
	}
}

// TestApprovalUnknownItem: resolving a nonexistent item gets
// ErrApprovalUnknown.
func TestApprovalUnknownItem(t *testing.T) {
	session, _ := approvalTestWorld(t)
	if err := session.ResolveApproval("no-such-item", true, ""); !errors.Is(err, ErrApprovalUnknown) {
		t.Fatalf("unknown item: %v, want ErrApprovalUnknown", err)
	}
}

// TestApprovalConcurrentResolvers: many concurrent resolves — exactly
// one wins, the rest split between Resolved (during the waiter's
// handoff) and Unknown (after the entry is gone).
func TestApprovalConcurrentResolvers(t *testing.T) {
	session, _ := approvalTestWorld(t)
	itemID, call := openToolSlot(session)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan approvalDecision, 1)
	go func() {
		d, _ := session.awaitApproval(ctx, call, itemID)
		done <- d
	}()
	waitForFlag(t, session, itemID)

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := session.ResolveApproval(itemID, true, "")
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent resolves: %d wins, want exactly 1", wins)
	}
	<-done
}

func waitForFlag(t *testing.T, session *Session, itemID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := session.SnapshotStreamState()
		if snap.Running != nil && snap.Running.ItemID == itemID && snap.Running.AwaitingApproval {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("AwaitingApproval never set")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func drainResolved(t *testing.T, sub *StreamSubscription) model.ToolApprovalResolvedEvent {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-sub.Events():
			if tr, ok := ev.(Transient); ok {
				if r, ok := tr.Event.(model.ToolApprovalResolvedEvent); ok {
					return r
				}
			}
		case <-deadline:
			t.Fatal("Resolved event never arrived")
		}
	}
}

// TestApprovalMechanismInvisibleWithoutAsk pins the §16.3 default
// baseline: the approval mechanism (landed in 7a) changes NOTHING for
// a run that never asks — the record sequence and the event type
// sequence are byte-for-byte the pre-7a behavior. The literal below
// was captured from the current implementation; the golden comparison
// is the executable form of "零 hooks = 现状不变".
func TestApprovalMechanismInvisibleWithoutAsk(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "echo", Arguments: `{}`}}},
		{model.TextDeltaEvent{Text: "final"}, model.MessageDoneEvent{}},
	}}
	ag, err := New(Options{
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       memory.OpenStore(),
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns", testSessionMetaAgent())
	if err != nil {
		t.Fatal(err)
	}

	// Subscribe first: the full event stream of the run.
	sub := session.Stream()
	defer sub.Stop()
	if err := session.Send("use the tool"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// Record sequence (types).
	var recordTypes []string
	for _, r := range session.SnapshotStream().Records {
		recordTypes = append(recordTypes, string(r.Type))
	}
	wantRecords := []string{
		storage.TypeSessionCreated, storage.TypeUserInput,
		storage.TypeAssistantMessage, storage.TypeToolResult,
		storage.TypeAssistantMessage,
	}
	if len(recordTypes) != len(wantRecords) {
		t.Fatalf("record types = %v, want %v", recordTypes, wantRecords)
	}
	for i := range wantRecords {
		if recordTypes[i] != wantRecords[i] {
			t.Fatalf("record[%d] = %s, want %s", i, recordTypes[i], wantRecords[i])
		}
	}

	// Event type sequence — the golden baseline. No approval events
	// appear: the mechanism is invisible without an Ask.
	var eventTypes []string
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case ev := <-sub.Events():
			switch e := ev.(type) {
			case RecordCommitted:
				eventTypes = append(eventTypes, "record:"+string(e.Record.Type))
			case Transient:
				eventTypes = append(eventTypes, "transient:"+string(e.Event.Type()))
			case Status:
				eventTypes = append(eventTypes, "status")
				if !e.Running {
					break collect
				}
			}
		case <-deadline:
			t.Fatalf("stream never settled; events so far: %v", eventTypes)
		}
	}
	wantEvents := []string{
		"record:session.created", "record:user.input", "status",
		"transient:agent_iteration_started",
		"transient:assistant_message_started",
		"transient:tool_call_done",
		"record:assistant.message",
		"transient:assistant_message_completed",
		"record:tool.result", "transient:tool_result",
		"transient:agent_iteration_started",
		"transient:assistant_message_started",
		"transient:text_delta", "transient:message_done",
		"record:assistant.message",
		"transient:assistant_message_completed",
		"status",
	}
	if len(eventTypes) != len(wantEvents) {
		t.Fatalf("event types = %v\nwant %v", eventTypes, wantEvents)
	}
	for i := range wantEvents {
		if eventTypes[i] != wantEvents[i] {
			t.Fatalf("event[%d] = %s, want %s (full: %v)", i, eventTypes[i], wantEvents[i], eventTypes)
		}
	}
}

// TestApprovalCancelWinsOverLateResolve pins the CANCELLATION WINS
// rule: after the run's ctx is cancelled, a resolve that lands must
// NOT make the wait return an approval — the select race is settled
// by rechecking ctx. Deterministic per iteration; the loop proves it
// is not a coin flip (the pre-fix code won this race ~50% of the
// time).
func TestApprovalCancelWinsOverLateResolve(t *testing.T) {
	for i := 0; i < 200; i++ {
		session, _ := approvalTestWorld(t)
		itemID, call := openToolSlot(session)
		sub := session.Stream()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := session.awaitApproval(ctx, call, itemID)
			done <- err
		}()
		waitForFlag(t, session, itemID)

		// Cancel FIRST, then resolve immediately (the opus-specified
		// ordering): the outcome must be cancelled — the recheck makes
		// this deterministic even when the select's two branches race.
		// (A resolve that completes BEFORE the cancellation legitimately
		// returns the approval; that interleaving cannot be forced
		// from this side, which is why the test cancels first.)
		cancel()
		_ = session.ResolveApproval(itemID, true, "")
		if err := <-done; err == nil {
			t.Fatalf("iteration %d: cancelled+resolved wait returned nil — the approval beat the cancellation", i)
		}
		resolved := drainResolved(t, sub)
		if resolved.Outcome != ApprovalOutcomeCancelled {
			t.Fatalf("iteration %d: outcome = %q, want cancelled (CANCELLATION WINS)", i, resolved.Outcome)
		}
		sub.Stop()
		session.Close()
	}
}
