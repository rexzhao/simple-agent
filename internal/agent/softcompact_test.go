package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rexzhao/simple-agent/internal/storage"
	"github.com/rexzhao/simple-agent/model"
)

// failSummaryProvider answers normal turns but fails every summary
// request (detected by the summary frame in the last message), with a
// switch to heal it mid-test.
type failSummaryProvider struct {
	turns     [][]model.Event
	fail      atomic.Bool
	summaryCh chan struct{} // closed when a summary request arrives
	summaries atomic.Int64  // count of summary requests seen
}

func isSummaryRequest(request model.Request) bool {
	// The compaction request carries the compaction prompt as its
	// system message (summaryFrame is the RESULT prefix, not in the
	// request).
	return len(request.Messages) > 0 &&
		request.Messages[0].Role == model.MessageRoleSystem &&
		strings.Contains(request.Messages[0].Content, "Summarize the conversation so far")
}

func (p *failSummaryProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if isSummaryRequest(request) {
		p.summaries.Add(1)
		if p.summaryCh != nil {
			select {
			case <-p.summaryCh:
			default:
				close(p.summaryCh)
			}
		}
		if p.fail.Load() {
			events := make(chan model.Event)
			close(events) // empty stream → "summary was empty" error
			return events, nil
		}
	}
	if len(p.turns) == 0 {
		events := make(chan model.Event)
		close(events)
		return events, nil
	}
	turn := p.turns[0]
	p.turns = p.turns[1:]
	events := make(chan model.Event, len(turn))
	for _, event := range turn {
		events <- event
	}
	close(events)
	return events, nil
}

// TestSoftCompactFailureContinuesRun: under soft pressure a failing
// summary degrades to a notice — the run completes with an assistant
// answer, no compaction record lands, and the compaction_failed event
// is on the wire.
func TestSoftCompactFailureContinuesRun(t *testing.T) {
	provider := &failSummaryProvider{
		fail:      atomic.Bool{},
		summaryCh: make(chan struct{}),
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "answer one"}},
			{model.TextDeltaEvent{Text: "answer two"}},
			{model.TextDeltaEvent{Text: "answer three"}},
			{model.TextDeltaEvent{Text: "answer four"}},
		},
	}
	provider.fail.Store(true)
	_, session := autoCompactAgent(t, provider, nil, nil)

	if err := session.Send("first"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	// Cross the soft threshold: the summary fails, the run continues.
	events := sendCollectEvents(t, session, strings.Repeat("工", 75_000))

	// The run COMPLETED: the last record is the assistant answer, not a
	// failure.
	records := compactedRecordsAll(t, session)
	last := records[len(records)-1]
	if last.Type != storage.TypeAssistantMessage {
		t.Fatalf("run outcome = %s, want a completed assistant.message (soft failure must not fail the run)", last.Type)
	}
	if len(compactedRecords(t, session)) != 0 {
		t.Fatal("a failed compaction committed a record")
	}
	sawFailedNotice := false
	for _, ev := range events {
		if _, ok := ev.(model.CompactionFailedEvent); ok {
			sawFailedNotice = true
		}
	}
	if !sawFailedNotice {
		t.Fatal("no compaction_failed event on the wire")
	}

	// The counted cooldown boundary: after the armed failure (1 summary
	// request so far), ONE new input must not produce a retry, the
	// SECOND must. Counting requests distinguishes "no retry" from
	// "retried and failed again", which record-level assertions cannot.
	afterArm := provider.summaries.Load()
	if err := session.Send("cooldown probe"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	if got := provider.summaries.Load(); got != afterArm {
		t.Fatalf("summary retried after ONE new input: %d -> %d", afterArm, got)
	}
	if err := session.Send("cooldown elapsed"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	if got := provider.summaries.Load(); got != afterArm+1 {
		t.Fatalf("summary not retried after two new inputs: %d -> %d (want one retry)", afterArm, got)
	}
	if len(compactedRecords(t, session)) != 0 {
		t.Fatal("the retried (failing) compaction committed a record")
	}
}

// TestSoftCompactFailureHealsAfterCooldown: after two new inputs (the
// cooldown cadence) and with the summary provider healed, the next
// soft-pressure run compacts successfully.
func TestSoftCompactFailureHealsAfterCooldown(t *testing.T) {
	provider := &failSummaryProvider{
		fail: atomic.Bool{},
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "answer one"}},                                                          // run 1 model
			{model.TextDeltaEvent{Text: "answer two"}},                                                          // run 2 model (after the failed summary)
			{model.TextDeltaEvent{Text: "answer three"}},                                                        // run 3 model (cooldown input 1)
			{model.TextDeltaEvent{Text: "summary ok " + strings.Repeat("word ", 10)}, model.MessageDoneEvent{}}, // run 4 pre-turn: the healed summary
			{model.TextDeltaEvent{Text: "answer four"}},                                                         // run 4 model
		},
	}
	provider.fail.Store(true)
	_, session := autoCompactAgent(t, provider, nil, nil)

	if err := session.Send("first"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	if err := session.Send(strings.Repeat("工", 75_000)); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	if len(compactedRecords(t, session)) != 0 {
		t.Fatal("precondition: the failing summary must not have compacted")
	}

	// Heal the provider. Input one stays inside the cooldown; input two
	// releases it — run four's pre-turn summary succeeds and the
	// compaction lands for real.
	provider.fail.Store(false)
	for range 2 {
		if err := session.Send(strings.Repeat("工", 5_000)); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, session)
	}
	if got := provider.summaries.Load(); got != 2 {
		t.Fatalf("summary requests = %d, want 2 (the failed one + the healed retry)", got)
	}
	if len(compactedRecords(t, session)) != 1 {
		t.Fatalf("compactions after heal = %d, want 1", len(compactedRecords(t, session)))
	}
}

// TestHardCompactFailureStillFailsRun: hard pressure keeps the run
// failing (the request cannot be sent safely) — the downgrade is
// soft-only.
func TestHardCompactFailureStillFailsRun(t *testing.T) {
	provider := &failSummaryProvider{
		fail: atomic.Bool{},
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "answer one"}},
		},
	}
	provider.fail.Store(true)
	_, session := autoCompactAgent(t, provider, nil, nil)

	if err := session.Send("first"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	// Cross the HARD threshold with a failing summary.
	if err := session.Send(strings.Repeat("工", 90_000)); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	records := compactedRecordsAll(t, session)
	last := records[len(records)-1]
	if last.Type != storage.TypeRunFailed {
		t.Fatalf("run outcome = %s, want run.failed under hard pressure", last.Type)
	}
}

// TestInterruptedCompactIsNotSoftFailure: a ctx cancellation during
// the summary call propagates as interruption — no run.failed record.
func TestInterruptedCompactIsNotSoftFailure(t *testing.T) {
	provider := &blockSummaryProvider{
		release: make(chan struct{}),
		entered: make(chan struct{}),
		turns:   [][]model.Event{{model.TextDeltaEvent{Text: "answer one"}}},
	}
	t.Cleanup(func() { close(provider.release) })
	_, session := autoCompactAgent(t, provider, nil, nil)

	if err := session.Send("first"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	// A run that crosses soft pressure and blocks inside the summary.
	go func() {
		_ = session.Send(strings.Repeat("工", 75_000))
	}()
	<-provider.entered
	session.Stop()
	waitSettled(t, session)

	records := compactedRecordsAll(t, session)
	for _, r := range records {
		if r.Type == storage.TypeRunFailed {
			t.Fatal("interruption during compaction recorded run.failed")
		}
		if r.Type == storage.TypeContextCompacted {
			t.Fatal("interrupted compaction committed a record")
		}
	}
}

// blockSummaryProvider blocks every summary request until released.
type blockSummaryProvider struct {
	release chan struct{}
	entered chan struct{}
	turns   [][]model.Event
}

func (p *blockSummaryProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if isSummaryRequest(request) {
		if p.entered != nil {
			select {
			case <-p.entered:
			default:
				close(p.entered)
			}
		}
		events := make(chan model.Event)
		go func() {
			select {
			case <-p.release:
			case <-ctx.Done():
			}
			close(events)
		}()
		return events, nil
	}
	if len(p.turns) == 0 {
		events := make(chan model.Event)
		close(events)
		return events, nil
	}
	turn := p.turns[0]
	p.turns = p.turns[1:]
	events := make(chan model.Event, len(turn))
	for _, event := range turn {
		events <- event
	}
	close(events)
	return events, nil
}

func compactedRecordsAll(t *testing.T, session *Session) []storage.Record {
	t.Helper()
	records := session.SnapshotStream().Records
	return records
}

// TestSoftCooldownAfterSuccessfulCompact pins the success-side reset:
// a committed compaction clears inputsSinceCompacted, so the next soft
// attempt waits for two NEW inputs — the same cadence the failure
// cooldown imposes. The reset lives on one line in appendLocked; this
// test is what makes its removal visible (without it the counter keeps
// growing and every post-compaction soft crossing re-compacts).
func TestSoftCooldownAfterSuccessfulCompact(t *testing.T) {
	provider := &failSummaryProvider{
		fail: atomic.Bool{},
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "answer one"}},                                                             // run 1 model
			{model.TextDeltaEvent{Text: "summary ok " + strings.Repeat("word ", 10)}, model.MessageDoneEvent{}},    // run 2: the soft-pressure summary (succeeds)
			{model.TextDeltaEvent{Text: "answer two"}},                                                             // run 2 model
			{model.TextDeltaEvent{Text: "answer three"}},                                                           // run 3 model (inside the cooldown: no summary)
			{model.TextDeltaEvent{Text: "summary again " + strings.Repeat("word ", 10)}, model.MessageDoneEvent{}}, // run 4: the retry summary
			{model.TextDeltaEvent{Text: "answer four"}},                                                            // run 4 model
		},
	}
	_, session := autoCompactAgent(t, provider, nil, nil)

	if err := session.Send("first"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	// Never compacted: the first soft crossing attempts immediately.
	if err := session.Send(strings.Repeat("工", 75_000)); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	if got := provider.summaries.Load(); got != 1 {
		t.Fatalf("summary requests after the first soft crossing = %d, want 1 (never-compacted sessions attempt immediately)", got)
	}
	if len(compactedRecords(t, session)) != 1 {
		t.Fatal("the soft compaction did not land")
	}

	// ONE new input since the compaction: the cooldown holds.
	if err := session.Send(strings.Repeat("工", 75_000)); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	if got := provider.summaries.Load(); got != 1 {
		t.Fatalf("summary retried after ONE new input since the compaction: %d (inputsSinceCompacted reset missing?)", got)
	}

	// The SECOND new input releases it: exactly one more summary.
	if err := session.Send(strings.Repeat("工", 75_000)); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	if got := provider.summaries.Load(); got != 2 {
		t.Fatalf("summary requests after two new inputs = %d, want 2 (the second compaction)", got)
	}
	if len(compactedRecords(t, session)) != 1 {
		t.Fatalf("compactions = %d, want 1 in the truncated active segment", len(compactedRecords(t, session)))
	}
}
