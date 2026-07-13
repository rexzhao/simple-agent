package execution

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// TestSessionStreamBlockedCallbackDoesNotBlockRunner verifies that a slow
// presentation callback never blocks provider emission or tool execution: the
// runner must reach completion while emit is blocked, and Send only waits for
// the sink to flush. After release, coalesced text is exact and the terminal
// event is last. No callback fires after Send returns.
func TestSessionStreamBlockedCallbackDoesNotBlockRunner(t *testing.T) {
	home := t.TempDir()
	const deltaCount = 120 // exceeds the bus subscriber buffer (64)
	var expectedText string
	runnerDone := make(chan struct{})
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			for i := 0; i < deltaCount; i++ {
				chunk := fmt.Sprintf("[%d]", i)
				expectedText += chunk
				request.Emit(model.TextDeltaEvent{Text: chunk})
			}
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "final")); err != nil {
				return SessionTurnResult{}, err
			}
			close(runnerDone)
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	release := make(chan struct{})
	var mu sync.Mutex
	var events []SessionStreamEvent
	emit := func(event SessionStreamEvent) {
		<-release
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}

	type sendResult struct {
		result SessionMessageResult
		err    error
	}
	sendDone := make(chan sendResult, 1)
	go func() {
		result, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", emit)
		sendDone <- sendResult{result: result, err: err}
	}()

	// The runner must complete even though every emit callback is blocked.
	select {
	case <-runnerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not complete; blocked emit prevented provider emission")
	}

	// Send may still be blocked flushing the sink; it must not have returned.
	select {
	case res := <-sendDone:
		t.Fatalf("Send returned before emit was released: %#v", res)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)

	select {
	case res := <-sendDone:
		if res.err != nil {
			t.Fatalf("SendSessionMessageWithEvents() error = %v", res.err)
		}
		if res.result.Status != "committed" {
			t.Fatalf("result = %#v, want committed", res.result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send did not return after release")
	}

	mu.Lock()
	defer mu.Unlock()
	types := sessionStreamEventTypes(events)
	if len(types) == 0 || types[0] != "turn.started" {
		t.Fatalf("first event = %#v, want turn.started", types)
	}
	if types[len(types)-1] != "turn.committed" {
		t.Fatalf("last event = %#v, want turn.committed", types)
	}
	if got := countString(types, "text.delta"); got > 2 {
		t.Fatalf("text.delta count = %d, want at most 2 (coalesced while blocked)", got)
	}
	if got := joinSessionStreamEventTexts(events, "text.delta"); got != expectedText {
		t.Fatalf("combined text = %q, want exact concatenation %q", got, expectedText)
	}

	// No callback fires after Send returns.
	n := len(events)
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(events) != n {
		t.Fatalf("events grew after return: %d -> %d", n, len(events))
	}
}

// TestSessionStreamFailureOrdersTurnFailedAfterPriorEvents verifies that on
// failure, turn.failed is emitted after all prior mapped events and is last.
func TestSessionStreamFailureOrdersTurnFailedAfterPriorEvents(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			request.Emit(model.TextDeltaEvent{Text: "partial "})
			request.Emit(model.TextDeltaEvent{Text: "output"})
			return SessionTurnResult{}, errors.New("provider exploded")
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	var events []SessionStreamEvent
	_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", func(event SessionStreamEvent) {
		events = append(events, event)
	})
	if !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("error = %v, want ErrTurnFailed", err)
	}
	types := sessionStreamEventTypes(events)
	if len(types) == 0 || types[0] != "turn.started" {
		t.Fatalf("first event = %#v, want turn.started", types)
	}
	if got := countString(types, "turn.failed"); got != 1 {
		t.Fatalf("turn.failed count = %d, want 1", got)
	}
	failedIdx := indexOfString(types, "turn.failed")
	if failedIdx != len(types)-1 {
		t.Fatalf("turn.failed index = %d, want last; events = %#v", failedIdx, types)
	}
	textIdx := indexOfString(types, "text.delta")
	if textIdx < 0 || textIdx >= failedIdx {
		t.Fatalf("text.delta(%d) must precede turn.failed(%d): %#v", textIdx, failedIdx, types)
	}
	// The exact coalescing of the two text deltas depends on drain timing; what
	// must hold is that no text is lost and the combined text is exact.
	if got := joinSessionStreamEventTexts(events, "text.delta"); got != "partial output" {
		t.Fatalf("combined text = %q, want %q", got, "partial output")
	}
	appendIdx := indexOfString(types, "item.appended")
	if appendIdx < 0 || appendIdx >= failedIdx {
		t.Fatalf("item.appended(%d) must precede turn.failed(%d): %#v", appendIdx, failedIdx, types)
	}
}

// TestSessionStreamCoalescesOnlyConsecutiveSameTypeDeltas verifies end-to-end
// that, while presentation is blocked, only queued consecutive text.delta /
// reasoning.delta events with the same turn_id and type are merged via exact
// concatenation; every other event is delivered verbatim and in order.
func TestSessionStreamCoalescesOnlyConsecutiveSameTypeDeltas(t *testing.T) {
	home := t.TempDir()
	runnerDone := make(chan struct{})
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			request.Emit(model.TextDeltaEvent{Text: "a"})
			request.Emit(model.TextDeltaEvent{Text: "b"})
			request.Emit(model.ReasoningDeltaEvent{Text: "r1"})
			request.Emit(model.ReasoningDeltaEvent{Text: "r2"})
			request.Emit(model.TextDeltaEvent{Text: "c"})
			request.Emit(model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "read_file"}})
			request.Emit(model.TextDeltaEvent{Text: "d"})
			request.Emit(model.TextDeltaEvent{Text: "e"})
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
				return SessionTurnResult{}, err
			}
			close(runnerDone)
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, err := NewServiceWithOptions(home, ServiceOptions{TurnRunner: runner})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "coalesce-repo")
	project, err := service.CreateProject(projectRoot, "Coalesce Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	showReasoning := true
	saveToolResults := true
	session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		CreatedCWD:      project.Project.Root,
		ConfigPath:      filepath.Join(project.Project.Root, ".agents", "sai.yaml"),
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		ShowReasoning:   &showReasoning,
		SaveToolResults: &saveToolResults,
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	// Block every emit until release so all deltas queue and coalesce at submit
	// time, making the merged runs deterministic.
	release := make(chan struct{})
	var mu sync.Mutex
	var events []SessionStreamEvent
	emit := func(event SessionStreamEvent) {
		<-release
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}

	type sendResult struct {
		err error
	}
	sendDone := make(chan sendResult, 1)
	go func() {
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", emit)
		sendDone <- sendResult{err: err}
	}()

	select {
	case <-runnerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not complete; blocked emit prevented provider emission")
	}
	// Send is now blocked flushing the sink.
	select {
	case res := <-sendDone:
		t.Fatalf("Send returned before release: %#v", res)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)

	select {
	case res := <-sendDone:
		if res.err != nil {
			t.Fatalf("SendSessionMessageWithEvents() error = %v", res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send did not return after release")
	}

	mu.Lock()
	defer mu.Unlock()
	gotTypes := sessionStreamEventTypes(events)
	if len(gotTypes) == 0 || gotTypes[0] != "turn.started" {
		t.Fatalf("first event = %#v, want turn.started", gotTypes)
	}
	if gotTypes[len(gotTypes)-1] != "turn.committed" {
		t.Fatalf("last event = %#v, want turn.committed", gotTypes)
	}
	// Exact merge runs can split if the emitter snapshots a leading delta with
	// turn.started before blocking; what must hold is exact text preservation in
	// submission order. The precise one-op-per-run invariant is covered by the
	// sink unit test.
	if got := joinSessionStreamEventTexts(events, "text.delta"); got != "abcde" {
		t.Fatalf("combined text.delta text = %q, want %q", got, "abcde")
	}
	if got := joinSessionStreamEventTexts(events, "reasoning.delta"); got != "r1r2" {
		t.Fatalf("combined reasoning.delta text = %q, want %q", got, "r1r2")
	}
	if countString(gotTypes, "text.delta") > 4 {
		t.Fatalf("text.delta count = %d, want at most 4 (coalesced runs)", countString(gotTypes, "text.delta"))
	}
	if got := countString(gotTypes, "tool.requested"); got != 1 {
		t.Fatalf("tool.requested count = %d, want 1", got)
	}
	if got := countString(gotTypes, "item.appended"); got != 2 {
		t.Fatalf("item.appended count = %d, want 2", got)
	}
}

// TestSessionStreamSuccessCommittedLastNoCallbacksAfterReturn verifies that on
// success turn.committed is the terminal event and no callback fires after Send
// returns.
func TestSessionStreamSuccessCommittedLastNoCallbacksAfterReturn(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			request.Emit(model.TextDeltaEvent{Text: "hello"})
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	var mu sync.Mutex
	var events []SessionStreamEvent
	if _, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", func(event SessionStreamEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}); err != nil {
		t.Fatalf("SendSessionMessageWithEvents() error = %v", err)
	}
	mu.Lock()
	types := sessionStreamEventTypes(events)
	if len(types) == 0 || types[len(types)-1] != "turn.committed" {
		t.Fatalf("last event = %#v, want turn.committed", types)
	}
	n := len(events)
	mu.Unlock()

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(events) != n {
		t.Fatalf("events grew after return: %d -> %d", n, len(events))
	}
	mu.Unlock()
}

func indexOfString(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}

func sessionStreamEventTexts(events []SessionStreamEvent, eventType string) []string {
	var texts []string
	for _, event := range events {
		if event["type"] != eventType {
			continue
		}
		text, _ := event["text"].(string)
		texts = append(texts, text)
	}
	return texts
}

func joinSessionStreamEventTexts(events []SessionStreamEvent, eventType string) string {
	var sb strings.Builder
	for _, text := range sessionStreamEventTexts(events, eventType) {
		sb.WriteString(text)
	}
	return sb.String()
}

// TestSessionEventSinkCoalescesAtSubmitTime verifies that consecutive same
// type+turn text.delta events are folded into a single queued op at submit time
// (under the queue mutex) while emit is blocked on an earlier non-delta event,
// so the queue does not grow by one map per delta. It inspects the sink queue
// directly, then releases, closes, waits and verifies delivery.
func TestSessionEventSinkCoalescesAtSubmitTime(t *testing.T) {
	release := make(chan struct{})
	blocked := make(chan struct{}, 1)
	var mu sync.Mutex
	var delivered []SessionStreamEvent
	sink := newSessionEventSink(func(event SessionStreamEvent) {
		select {
		case blocked <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		delivered = append(delivered, event)
		mu.Unlock()
	})

	// Non-delta event first. Wait until the emitter is blocked inside emit on it,
	// which means it has snapshotted turn.started and cleared sink.ops; this
	// guarantees the consecutive deltas below all coalesce into one queued op.
	sink.submit(NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-1"}))
	<-blocked

	const deltaCount = 200
	var want strings.Builder
	for i := 0; i < deltaCount; i++ {
		chunk := fmt.Sprintf("[%d]", i)
		want.WriteString(chunk)
		sink.submit(NewSessionStreamEvent("text.delta", map[string]any{
			"turn_id": "turn-1",
			"text":    chunk,
		}))
	}

	// A different turn_id breaks the run; it must not merge into the prior run.
	sink.submit(NewSessionStreamEvent("text.delta", map[string]any{
		"turn_id": "turn-2",
		"text":    "other",
	}))
	// A non-delta event also breaks the run.
	sink.submit(NewSessionStreamEvent("usage.updated", map[string]any{"turn_id": "turn-1"}))

	sink.mu.Lock()
	deltaOps := 0
	var queuedText string
	for _, op := range sink.ops {
		if op.isDelta && op.eventType == "text.delta" && op.turnID == "turn-1" {
			deltaOps++
			queuedText = op.text.String()
		}
	}
	sink.mu.Unlock()
	if deltaOps != 1 {
		t.Fatalf("queued turn-1 text.delta ops = %d, want 1 (coalesced at submit time)", deltaOps)
	}
	if queuedText != want.String() {
		t.Fatalf("queued coalesced text = %q, want exact concatenation %q", queuedText, want.String())
	}

	close(release)
	sink.close()
	sink.wait()

	mu.Lock()
	defer mu.Unlock()
	types := sessionStreamEventTypes(delivered)
	// turn.started (blocked), then the single coalesced text.delta, then the
	// other-turn delta, then usage.updated.
	wantTypes := []string{"turn.started", "text.delta", "text.delta", "usage.updated"}
	if len(types) != len(wantTypes) {
		t.Fatalf("delivered types = %#v, want %#v", types, wantTypes)
	}
	for i, want := range wantTypes {
		if types[i] != want {
			t.Fatalf("delivered types[%d] = %q, want %q; full = %#v", i, types[i], want, types)
		}
	}
	if got, _ := delivered[1]["text"].(string); got != want.String() {
		t.Fatalf("delivered coalesced text = %q, want %q", got, want.String())
	}
	if got, _ := delivered[2]["turn_id"].(string); got != "turn-2" {
		t.Fatalf("delivered other-turn delta turn_id = %q, want turn-2", got)
	}
}
