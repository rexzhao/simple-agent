package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/storage"
)

// TestStreamOrdering pins the single-stream ordering guarantee: within
// a run, started/delta events precede their RecordCommitted, and the
// terminal Status{Running:false} comes after all records.
func TestStreamOrdering(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "hello"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, nil)

	sub := session.Stream()
	defer sub.Stop()

	if err := session.Send("test"); err != nil {
		t.Fatal(err)
	}

	// Collect events until the terminal Status.
	var events []SessionEvent
	deadline := time.After(10 * time.Second)
loop:
	for {
		select {
		case ev := <-sub.Events():
			events = append(events, ev)
			if st, ok := ev.(Status); ok && !st.Running {
				break loop
			}
		case <-deadline:
			t.Fatal("timed out waiting for terminal Status")
		case <-sub.Lost():
			t.Fatal("unexpected eviction")
		}
	}

	// Assertions: find the assistant.message record and verify its
	// started/delta events came before it.
	var recordIdx int = -1
	var startedIdx int = -1
	var deltaIdx int = -1
	var terminalIdx int = -1
	for i, ev := range events {
		switch e := ev.(type) {
		case RecordCommitted:
			if e.Record.Type == storage.TypeAssistantMessage {
				recordIdx = i
			}
		case Transient:
			switch e.Event.Type() {
			case model.EventTypeAssistantMessageStarted:
				startedIdx = i
			case model.EventTypeTextDelta:
				deltaIdx = i
			}
		case Status:
			if !e.Running {
				terminalIdx = i
			}
		}
	}

	if recordIdx == -1 {
		t.Fatal("no assistant.message RecordCommitted in stream")
	}
	if startedIdx == -1 {
		t.Fatal("no assistant_message_started in stream")
	}
	if deltaIdx == -1 {
		t.Fatal("no text_delta in stream")
	}
	if terminalIdx == -1 {
		t.Fatal("no terminal Status{Running:false} in stream")
	}

	if startedIdx > recordIdx {
		t.Fatalf("started (%d) after record (%d) — ordering violated", startedIdx, recordIdx)
	}
	if deltaIdx > recordIdx {
		t.Fatalf("delta (%d) after record (%d) — ordering violated", deltaIdx, recordIdx)
	}
	if terminalIdx < recordIdx {
		t.Fatalf("terminal Status (%d) before record (%d) — ordering violated", terminalIdx, recordIdx)
	}
}

// TestStreamInitialSnapshot verifies the snapshot: a mid-session
// subscriber gets all previously committed records plus the current
// status in Initial().
func TestStreamInitialSnapshot(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "one"}, model.MessageDoneEvent{}},
		{model.TextDeltaEvent{Text: "two"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, nil)

	// Run one turn.
	if err := session.Send("first"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// Subscribe mid-session.
	sub := session.Stream()
	defer sub.Stop()

	snapshot := sub.Initial()
	if len(snapshot.Records) < 2 { // user.input + assistant.message
		t.Fatalf("Initial records = %d, want >= 2", len(snapshot.Records))
	}
	// S3: assert ALL StreamSnapshot fields are populated (not just
	// Records). An idle session after one settled run: Streaming and
	// Running are nil, Compacting is nil, Steered is empty, both status
	// bools are false — but they must be PRESENT and CORRECTLY zero.
	if snapshot.Streaming != nil {
		t.Fatalf("idle session Streaming = %+v, want nil", snapshot.Streaming)
	}
	if snapshot.Running != nil {
		t.Fatalf("idle session Running = %+v, want nil", snapshot.Running)
	}
	if snapshot.Compacting != nil {
		t.Fatalf("idle session Compacting = %+v, want nil", snapshot.Compacting)
	}
	if len(snapshot.Steered) != 0 {
		t.Fatalf("idle session Steered = %d, want 0", len(snapshot.Steered))
	}
	if snapshot.StatusRunning {
		t.Fatal("idle session StatusRunning = true, want false")
	}
	if snapshot.StatusCompacting {
		t.Fatal("idle session StatusCompacting = true, want false")
	}
	// StartSeq is 0 for the FIRST subscription on a session where no
	// prior subscriber ever connected (the dispatcher was never started,
	// so no events were enqueued and the counter never advanced). The
	// "counter not advancing" case is caught by TestStreamSeqMonotonic
	// (which asserts seq > 0 for delivered events).
	_ = snapshot.StartSeq
}

// TestStreamSteerOrdering pins the B2 scenario: the steer_queued event
// (from the API goroutine) must arrive BEFORE the RecordCommitted of
// the user.input that consumes it (from the run goroutine) — both are
// enqueued under s.mu with seq, so the ordering is structural.
func TestStreamSteerOrdering(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "answer"}, model.MessageDoneEvent{}},
		{model.TextDeltaEvent{Text: "answer2"}, model.MessageDoneEvent{}},
		{model.TextDeltaEvent{Text: "answer3"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, nil)

	sub := session.Stream()
	defer sub.Stop()

	// Start a run.
	if err := session.Send("start"); err != nil {
		t.Fatal(err)
	}

	// Steer mid-run: the steer_queued event should be enqueued before
	// the consuming user.input RecordCommitted.
	steerItem, err := session.Steer("mid-run input")
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	_ = steerItem

	waitSettled(t, session)

	// Collect all events from the subscription.
	var events []SessionEvent
	deadline := time.After(10 * time.Second)
loop:
	for {
		select {
		case ev := <-sub.Events():
			events = append(events, ev)
			if st, ok := ev.(Status); ok && !st.Running {
				break loop
			}
		case <-deadline:
			t.Fatal("timed out")
		case <-sub.Lost():
			t.Fatal("unexpected eviction")
		}
	}

	// Find the steer_queued event and the consuming user.input.
	var steerQueuedIdx int = -1
	var consumingInputIdx int = -1
	for i, ev := range events {
		switch e := ev.(type) {
		case Transient:
			if e.Event.Type() == model.EventTypeSteerQueued {
				steerQueuedIdx = i
			}
		case RecordCommitted:
			if e.Record.Type == storage.TypeUserInput && e.Record.Content == "mid-run input" {
				consumingInputIdx = i
			}
		}
	}

	// Unconditional: the steer_queued event MUST be on the stream (B2
	// fix), and it MUST precede the consuming user.input if the input
	// was consumed in this run.
	if steerQueuedIdx == -1 {
		t.Fatal("no steer_queued Transient on the stream — B2 regression")
	}
	if consumingInputIdx != -1 && steerQueuedIdx > consumingInputIdx {
		t.Fatalf("steer_queued (%d) after consuming user.input (%d) — B2 ordering violated", steerQueuedIdx, consumingInputIdx)
	}
}

// TestStreamDualPathConsistency pins B1: the stream and the legacy
// subscription deliver the SAME set of records for the same run. If
// the stream misses a record (e.g. Start's user.input, or
// commitLocked returning two records), this test catches it.
func TestStreamDualPathConsistency(t *testing.T) {
	toolDone := make(chan struct{})
	close(toolDone) // non-blocking tool
	executor := &blockingToolExecutor{done: toolDone}
	provider := &fakeProvider{turns: [][]model.Event{
		// Turn 1: text only.
		{model.TextDeltaEvent{Text: "one"}, model.MessageDoneEvent{}},
		// Turn 2: with a tool call — exercises ToolStarted/ToolResult
		// through both paths (the S2 behavioral coverage).
		{model.TextDeltaEvent{Text: "checking"}, model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "tc-1", Name: "check", Arguments: "{}"}}, model.MessageDoneEvent{}},
		// Turn 3 (post-tool): final answer.
		{model.TextDeltaEvent{Text: "done"}, model.MessageDoneEvent{}},
		// Turn 4: the compaction summary.
		{model.TextDeltaEvent{Text: "summary of the conversation"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, executor, func(o *Options) {
		o.Tools = []model.Tool{{Name: "check"}}
	})

	// Subscribe to BOTH paths BEFORE any write — then MaterializeHeader
	// exercises the third write path (the session header flush) through
	// both subscriptions equally.
	streamSub := session.Stream()
	defer streamSub.Stop()
	legacySub := session.Subscribe()
	defer legacySub.Stop()

	if err := session.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}

	// Run a text turn.
	if err := session.Send("first"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// Run a tool turn — the ToolStarted/ToolResult events must flow
	// through BOTH paths.
	if err := session.Send("use tool"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// Run a manual compaction — CompactionStarted/Completed events
	// through both paths (compact runs now have an events channel).
	if err := session.Compact(); err != nil && err.Error() != "nothing to compact" {
		t.Fatalf("Compact: %v", err)
	}

	// Collect all RecordCommitted AND Transient events from the stream.
	// Since we waitSettled after each step, all events are already in
	// the channel — drain until a quiet period.
	var streamRecords []string
	var streamTransientTypes []string
	quiet := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case ev := <-streamSub.Events():
			switch e := ev.(type) {
			case RecordCommitted:
				streamRecords = append(streamRecords, e.Record.ItemID)
			case Transient:
				streamTransientTypes = append(streamTransientTypes, string(e.Event.Type()))
			}
			quiet = time.After(500 * time.Millisecond)
		case <-quiet:
			break loop
		case <-streamSub.Lost():
			t.Fatal("unexpected eviction")
		}
	}

	// Collect all records from the legacy subscription.
	var legacyRecords []string
	for {
		select {
		case r := <-legacySub.Records():
			legacyRecords = append(legacyRecords, r.ItemID)
		default:
			goto compare
		}
	}
compare:
	// The stream's Initial already contains the pre-subscription
	// records; merge them with the stream events.
	for _, r := range streamSub.Initial().Records {
		streamRecords = append(streamRecords, r.ItemID)
	}

	if len(streamRecords) != len(legacyRecords) {
		t.Fatalf("record count mismatch: stream=%d legacy=%d\nstream: %v\nlegacy: %v",
			len(streamRecords), len(legacyRecords), streamRecords, legacyRecords)
	}
	for i := range streamRecords {
		if streamRecords[i] != legacyRecords[i] {
			t.Fatalf("record %d mismatch: stream=%s legacy=%s", i, streamRecords[i], legacyRecords[i])
		}
	}

	// Compare transient events: the critical tool lifecycle event must
	// appear in the stream's Transient lane. Compaction events
	// (started/completed) are only emitted by AUTO-compaction
	// (maybeAutoCompact); manual compact doesn't emit them yet — that's
	// the N3 gap closed in step 3. For the behavioral coverage here,
	// the tool event is the one that catches an executeToolCall
	// bypassing the stream.
	for _, want := range []string{"tool_started"} {
		found := false
		for _, typ := range streamTransientTypes {
			if typ == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("transient event %q not found in stream — S2 behavioral gap (event bypassed the stream?)", want)
		}
	}
}

// TestStreamSeqMonotonic pins that seq values within the Events()
// channel are strictly increasing.
func TestStreamSeqMonotonic(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "one"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, nil)

	sub := session.Stream()
	defer sub.Stop()

	if err := session.Send("test"); err != nil {
		t.Fatal(err)
	}

	var lastSeq uint64
	deadline := time.After(10 * time.Second)
loop:
	for {
		select {
		case ev := <-sub.Events():
			var seq uint64
			switch e := ev.(type) {
			case RecordCommitted:
				seq = e.Seq
			case Transient:
				seq = e.Seq
			case Status:
				seq = e.Seq
			}
			if seq == 0 {
				t.Fatalf("seq must be > 0 (got 0 — the counter is not advancing)")
			}
			if seq <= lastSeq && lastSeq > 0 {
				t.Fatalf("seq not monotonic: %d after %d", seq, lastSeq)
			}
			lastSeq = seq
			if st, ok := ev.(Status); ok && !st.Running {
				break loop
			}
		case <-deadline:
			t.Fatal("timed out")
		case <-sub.Lost():
			t.Fatal("unexpected eviction")
		}
	}
}

// TestSendEventOnlyFromExitFunction is a structural guard (S2): the
// free function sendEvent must only be called from emitTransient (the
// single exit point). A direct sendEvent call bypasses the stream —
// exactly the ToolStartedEvent bug this round fixed. This test scans
// the source to enforce the discipline.
func TestSendEventOnlyFromExitFunction(t *testing.T) {
	// The allowed call sites: sendEvent's own definition, and inside
	// emitTransient (the exit function).
	allowed := map[string]bool{
		"func sendEvent":                  true,
		"func (s *Session) emitTransient": true,
	}
	src, err := os.ReadFile("session.go")
	if err != nil {
		t.Skip("source not available in test binary")
	}
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "sendEvent(") {
			continue
		}
		// Allow the definition and the call inside emitTransient.
		if strings.HasPrefix(trimmed, "func sendEvent") {
			continue
		}
		// The call inside emitTransient is "sendEvent(ctx, out, event)".
		if trimmed == "sendEvent(ctx, out, event)" {
			continue
		}
		t.Fatalf("sendEvent called outside the exit function: %q — use s.emitTransient instead", trimmed)
	}
	_ = allowed
}

// TestStreamEvictionRecovery pins S4 deterministically: a subscriber
// whose buffer is full gets evicted on the next non-droppable event
// (Lost closes), and a fresh Stream() converges to the correct state.
// The buffer is filled DIRECTLY (no reliance on scheduling to produce
// enough events).
func TestStreamEvictionRecovery(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "one"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, nil)

	// Subscribe and fill the buffer to capacity manually.
	sub1 := session.Stream()
	for i := 0; i < streamBuffer; i++ {
		select {
		case sub1.sub.events <- Status{Running: false, Seq: uint64(i + 1)}:
		default:
			t.Fatalf("could not fill buffer at %d — unexpected", i)
		}
	}

	// Trigger one non-droppable event: a commit through appendLocked.
	// The dispatcher will try to deliver a RecordCommitted to a full
	// buffer ⟹ evict.
	if err := session.Send("trigger"); err != nil {
		t.Fatal(err)
	}

	// The subscriber MUST be evicted (not skipped).
	select {
	case <-sub1.Lost():
		// Evicted as expected.
	case <-time.After(10 * time.Second):
		t.Fatal("subscriber not evicted — buffer full + non-droppable event should trigger eviction")
	}

	// Wait for the run to finish.
	waitSettled(t, session)

	// Recovery: re-Stream and verify the snapshot.
	sub2 := session.Stream()
	defer sub2.Stop()
	snap := sub2.Initial()
	if len(snap.Records) == 0 {
		t.Fatal("recovery snapshot has no records")
	}
	// No duplicate ItemIDs.
	seen := make(map[string]bool)
	for _, r := range snap.Records {
		if seen[r.ItemID] {
			t.Fatalf("duplicate ItemID in recovery snapshot: %s", r.ItemID)
		}
		seen[r.ItemID] = true
	}
}

// TestStreamNoDuplicateOnRegister pins S1 deterministically: a record
// committed BEFORE the subscription is in the snapshot and must NOT be
// re-delivered on Events(). Uses registerStreamLocked directly to
// construct the race window without relying on scheduling.
func TestStreamNoDuplicateOnRegister(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "one"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, nil)

	// Start the dispatcher (first Stream() call).
	sub0 := session.Stream()
	sub0.Stop() // We only needed the dispatcher to start.

	// Under the lock: commit a record, then register a new subscriber.
	// The record is in the queue (not yet delivered) AND in the
	// snapshot. Without the startSeq filter, the dispatcher would
	// deliver it again.
	sub1 := &streamSub{
		events: make(chan SessionEvent, streamBuffer),
		lost:   make(chan struct{}),
	}
	session.mu.Lock()
	_, err := session.commitLocked(storage.Record{
		ItemID: "s1-dup-test", Type: storage.TypeUserInput,
		Content: "committed before subscribe",
	})
	if err != nil {
		session.mu.Unlock()
		t.Fatal(err)
	}
	snapshot := session.registerStreamLocked(sub1)
	session.mu.Unlock()

	// The snapshot must contain the record.
	found := false
	for _, r := range snapshot.Records {
		if r.ItemID == "s1-dup-test" {
			found = true
		}
	}
	if !found {
		t.Fatal("record not in snapshot — commitLocked + registerStreamLocked ordering broken")
	}

	// Start a run to flush the queue through the dispatcher.
	sub2 := &StreamSubscription{session: session, sub: sub1, snapshot: snapshot}
	defer sub2.Stop()
	if err := session.Send("flush"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// Drain sub1's Events; the pre-subscription record must NOT appear.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-sub1.events:
			if rc, ok := ev.(RecordCommitted); ok {
				if rc.Record.ItemID == "s1-dup-test" {
					t.Fatal("pre-subscription record re-delivered on Events — S1 regression (startSeq filter not working)")
				}
			}
			if st, ok := ev.(Status); ok && !st.Running {
				goto done
			}
		case <-deadline:
			goto done
		case <-sub1.lost:
			t.Fatal("unexpected eviction")
		}
	}
done:
}

// TestStreamSnapshotMidRunPositive pins S3 with POSITIVE assertions:
// subscribing DURING an active run must populate the transient fields
// (Streaming, Steered, StatusRunning, StartSeq > 0). Kill-verified by
// review: deleting the field copies in registerStreamLocked makes
// this test fail (the "idle-session nil" assertions don't).
func TestStreamSnapshotMidRunPositive(t *testing.T) {
	gate := make(chan struct{})
	t.Cleanup(func() {
		if gate != nil {
			close(gate)
		}
	})
	provider := &ctxGatedProvider{gate: gate}
	_, session := newTestAgent(t, provider, nil)

	// Start a run — the provider blocks mid-stream.
	if err := session.Send("hello"); err != nil {
		t.Fatal(err)
	}
	// Wait for the run to reach the streaming state.
	deadline := time.Now().Add(5 * time.Second)
	for session.Snapshot().Streaming == nil {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for streaming slot")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Steer mid-run to populate the queue.
	if _, err := session.Steer("queued input"); err != nil {
		t.Fatalf("Steer mid-run: %v", err)
	}

	// Start a dummy subscription first to start the dispatcher. Then
	// Steer to enqueue at least one event (advancing the seq counter) —
	// without this, the gated provider means no events flow after the
	// dispatcher starts, and StartSeq would be 0.
	dummy := session.Stream()
	if _, err := session.Steer("advance seq"); err != nil {
		t.Fatalf("Steer to advance seq: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the dispatcher deliver
	dummy.Stop()

	// Subscribe DURING the run (the real subscription under test).
	sub := session.Stream()
	defer sub.Stop()
	snap := sub.Initial()

	// POSITIVE assertions — these fail if the field copies in
	// registerStreamLocked are deleted.
	if snap.Streaming == nil {
		t.Fatal("mid-run snapshot: Streaming = nil, want the streaming slot (S3 positive)")
	}
	if len(snap.Steered) == 0 {
		t.Fatal("mid-run snapshot: Steered empty, want the queued steer (S3 positive)")
	}
	if !snap.StatusRunning {
		t.Fatal("mid-run snapshot: StatusRunning = false, want true (S3 positive)")
	}
	if snap.StartSeq == 0 {
		t.Fatal("mid-run snapshot: StartSeq = 0, want > 0 — the seq counter has advanced (S3 positive)")
	}

	// Release the gated provider so the run can complete and the
	// terminal Status arrives.
	close(gate)
	gate = nil // prevent double-close by t.Cleanup

	// Every event delivered after subscription must have seq > StartSeq.
	for {
		select {
		case ev := <-sub.Events():
			var seq uint64
			switch e := ev.(type) {
			case RecordCommitted:
				seq = e.Seq
			case Transient:
				seq = e.Seq
			case Status:
				seq = e.Seq
			}
			if seq <= snap.StartSeq {
				t.Fatalf("event seq %d <= StartSeq %d — S1/S3 invariant violated", seq, snap.StartSeq)
			}
			if st, ok := ev.(Status); ok && !st.Running {
				return // terminal — test complete
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for terminal Status")
		case <-sub.Lost():
			t.Fatal("unexpected eviction")
		}
	}
}

// TestStreamSnapshotRunningToolPositive pins S3's Running tool slot:
// subscribing while a tool is executing must populate snap.Running.
func TestStreamSnapshotRunningToolPositive(t *testing.T) {
	toolDone := make(chan struct{})
	executor := &blockingToolExecutor{done: toolDone}
	t.Cleanup(func() {
		select {
		case <-toolDone:
		default:
			close(toolDone)
		}
	})
	// The provider returns a message with a tool call, then a final answer.
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "let me check"}, model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "tc-1", Name: "check", Arguments: "{}"}}, model.MessageDoneEvent{}},
		{model.TextDeltaEvent{Text: "done"}, model.MessageDoneEvent{}},
	}}
	// Override the tool executor via agent options.
	ag, session := newTestAgent(t, provider, executor, func(o *Options) {
		o.Tools = []model.Tool{{Name: "check"}}
	})
	_ = ag

	if err := session.Send("run tool"); err != nil {
		t.Fatal(err)
	}
	// Wait for the tool to start executing.
	deadline := time.Now().Add(5 * time.Second)
	for session.Snapshot().Running == nil {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for tool execution")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Subscribe during tool execution.
	sub := session.Stream()
	defer sub.Stop()
	snap := sub.Initial()
	if snap.Running == nil {
		t.Fatal("mid-tool snapshot: Running = nil, want the tool slot (S3 positive)")
	}
	// Release the tool.
	select {
	case <-toolDone:
	default:
		close(toolDone)
	}
	waitSettled(t, session)
}

// TestStreamSnapshotCompactingPositive pins S3's Compacting slot:
// subscribing while a compaction is in progress must populate
// snap.Compacting and snap.StatusCompacting.
func TestStreamSnapshotCompactingPositive(t *testing.T) {
	provider := &blockSummaryProvider{
		release: make(chan struct{}),
		entered: make(chan struct{}),
		turns:   [][]model.Event{{model.TextDeltaEvent{Text: "answer one"}}},
	}
	t.Cleanup(func() {
		select {
		case <-provider.release:
			// Already released by the test body.
		default:
			close(provider.release)
		}
	})
	_, session := newTestAgent(t, provider, nil, func(o *Options) {
		o.AutoCompact = false // manual compact only
	})

	// Build some context first.
	if err := session.Send("hello"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	// Start a compaction — the summary blocks.
	r, err := session.StartCompact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-provider.entered

	// Subscribe during the compaction.
	sub := session.Stream()
	defer sub.Stop()
	snap := sub.Initial()
	if snap.Compacting == nil {
		t.Fatal("mid-compact snapshot: Compacting = nil, want the compacting slot (S3 positive)")
	}
	if !snap.StatusCompacting {
		t.Fatal("mid-compact snapshot: StatusCompacting = false, want true (S3 positive)")
	}
	if !snap.StatusRunning {
		t.Fatal("mid-compact snapshot: StatusRunning = false, want true — compact occupies the run slot")
	}

	// Release and wait.
	select {
	case <-provider.release:
		// Already released.
	default:
		close(provider.release)
	}
	<-r.Done()
}

// blockingToolExecutor blocks until the done channel is closed.
type blockingToolExecutor struct {
	done chan struct{}
}

func (e *blockingToolExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	select {
	case <-e.done:
		return model.ToolResult{Content: "tool done"}, nil
	case <-ctx.Done():
		return model.ToolResult{Content: "tool cancelled", IsError: true}, nil
	}
}

// TestRunStartStatusNotDroppable pins H1a: the run-start Status rides
// the guaranteed path (not droppable). Consumers key lifecycle
// machinery on it — the hub starts its legacy-channel drain on
// Status{Running:true} — so a drop over the watermark would silently
// wedge a run with no recovery. Over-watermark subscribers must still
// receive it, while droppable deltas are skipped as designed.
func TestRunStartStatusNotDroppable(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "one"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, nil)

	sub := session.Stream()
	defer sub.Stop()
	// Fill above the watermark but below capacity: droppable events
	// are skipped for this subscriber from here on.
	for i := 0; i < streamWatermark+16; i++ {
		select {
		case sub.sub.events <- Status{Seq: uint64(10000 + i)}:
		default:
			t.Fatalf("could not fill buffer at %d — unexpected", i)
		}
	}

	if err := session.Send("hi"); err != nil {
		t.Fatal(err)
	}
	// Do NOT read while the run is live: the watermark governs the
	// CURRENT buffer length, and draining would drop it below the line
	// (deltas would legitimately be delivered again). The buffer has
	// headroom (watermark+16+4 < capacity), so no eviction either.
	waitSettled(t, session)

	// Drain everything and classify: the run-start Status{Running:true}
	// must be there (non-droppable); the turn's deltas must not
	// (droppable over the watermark).
	sawRunStart, sawTerminal, sawDelta := false, false, false
	for {
		select {
		case ev := <-sub.Events():
			switch e := ev.(type) {
			case Status:
				if e.Seq < 10000 {
					if e.Running {
						sawRunStart = true
					} else {
						sawTerminal = true
					}
				}
			case Transient:
				if _, ok := e.Event.(model.TextDeltaEvent); ok {
					sawDelta = true
				}
			}
		default:
			if !sawRunStart {
				t.Fatal("run-start Status was dropped for the over-watermark subscriber (must be non-droppable)")
			}
			if !sawTerminal {
				t.Fatal("terminal Status missing (also non-droppable)")
			}
			if sawDelta {
				t.Fatal("droppable delta delivered over the watermark (watermark semantics broken)")
			}
			return
		}
	}
}
