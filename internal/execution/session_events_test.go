package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
	"github.com/rexzhao/simple-agent/internal/sessions"
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

func TestGetSessionChatItemsPageSupportsBeforeAndAfterCursors(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: true})
	for i := 1; i <= 6; i++ {
		message := model.Message{Role: model.MessageRoleUser, Content: fmt.Sprintf("message-%d", i)}
		if _, err := service.sessionStore.AppendItem(session.ID, sessions.SessionItemFromMessage(fmt.Sprintf("msg-%d", i), message)); err != nil {
			t.Fatalf("AppendItem(%d) error = %v", i, err)
		}
	}

	latest, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{Limit: 2})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(latest) error = %v", err)
	}
	if got := sessionItemContents(latest.Items); !sameStringSlice(got, []string{"message-5", "message-6"}) {
		t.Fatalf("latest contents = %#v", got)
	}
	if !latest.HasMoreBefore || latest.HasMoreAfter {
		t.Fatalf("latest page flags = before %t after %t", latest.HasMoreBefore, latest.HasMoreAfter)
	}

	before, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{BeforeSeq: latest.OldestSeq, Limit: 2})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(before) error = %v", err)
	}
	if got := sessionItemContents(before.Items); !sameStringSlice(got, []string{"message-3", "message-4"}) {
		t.Fatalf("before contents = %#v", got)
	}
	if !before.HasMoreBefore || !before.HasMoreAfter {
		t.Fatalf("before page flags = before %t after %t", before.HasMoreBefore, before.HasMoreAfter)
	}

	after, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{AfterSeq: before.NewestSeq, Limit: 1})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(after) error = %v", err)
	}
	if got := sessionItemContents(after.Items); !sameStringSlice(got, []string{"message-5"}) {
		t.Fatalf("after contents = %#v", got)
	}
	if !after.HasMoreBefore || !after.HasMoreAfter {
		t.Fatalf("after page flags = before %t after %t", after.HasMoreBefore, after.HasMoreAfter)
	}
}

func TestSessionToolDisplayArgumentsKeepsOnlyUsefulPresentationFields(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		arguments string
		want      string
	}{
		{name: "file path without content", toolName: "write_file", arguments: `{"path":"notes.txt","content":"secret body"}`, want: `{"path":"notes.txt"}`},
		{name: "shell command", toolName: "shell", arguments: `{"command":"go test ./...","timeout_ms":1000}`, want: `{"command":"go test ./..."}`},
		{name: "invalid json", toolName: "read_file", arguments: `{`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sessionToolDisplayArguments(test.toolName, test.arguments); got != test.want {
				t.Fatalf("sessionToolDisplayArguments() = %q, want %q", got, test.want)
			}
		})
	}
}

func sessionItemContents(items []SessionItem) []string {
	contents := make([]string, 0, len(items))
	for _, item := range items {
		if item.Message != nil && item.Message.Content != nil {
			contents = append(contents, item.Message.Content.Inline)
		}
	}
	return contents
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
			request.Emit(model.AgentIterationStartedEvent{Iteration: 2})
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
	if got := countString(gotTypes, "agent.iteration.started"); got != 1 {
		t.Fatalf("agent.iteration.started count = %d, want 1", got)
	}
	for _, event := range events {
		eventType, _ := event["type"].(string)
		switch eventType {
		case "agent.iteration.started", "text.delta", "reasoning.delta", "tool.requested":
			if got, _ := event["agent_iteration"].(int); got != 2 {
				t.Fatalf("%s agent_iteration = %#v, want 2", eventType, event["agent_iteration"])
			}
		}
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

// turnFailedSecret is injected into the prompt and, where a runner/planner error
// is simulated, into that error's text. It must never appear in any
// SessionStreamEvent: turn.failed carries only a stable code and canned message.
const turnFailedSecret = "LEAKED-TURN-SECRET-7c9f"

// TestSessionStreamTurnFailedSafePayloadByStage verifies that every reachable
// failure stage after turn.started emits exactly one terminal turn.failed event
// (last, unique) carrying a stable code and short canned message, and that no
// stream event leaks the underlying error text, prompt, or other secret.
func TestSessionStreamTurnFailedSafePayloadByStage(t *testing.T) {
	prompt := "prompt " + turnFailedSecret

	t.Run("compaction_failed", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			plan: func(ctx context.Context, request SessionTurnRequest) (SessionCompactionResult, error) {
				return SessionCompactionResult{}, fmt.Errorf("compaction planner leaked %s", turnFailedSecret)
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(t, events, "compaction_failed", "compaction planning failed", turnFailedSecret)
	})

	t.Run("turn_input_failed", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			plan: func(ctx context.Context, request SessionTurnRequest) (SessionCompactionResult, error) {
				blockSessionSegmentsDir(t, home, request.Session.ID)
				return SessionCompactionResult{}, nil
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if err == nil || !strings.Contains(err.Error(), "could not save turn input") {
			t.Fatalf("error = %v, want could not save turn input", err)
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(t, events, "turn_input_failed", "could not save turn input", turnFailedSecret)
	})

	t.Run("runner_failed", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				request.Emit(model.TextDeltaEvent{Text: "partial output"})
				return SessionTurnResult{}, fmt.Errorf("runner leaked %s", turnFailedSecret)
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(t, events, "runner_failed", "turn runner failed", turnFailedSecret)
	})

	t.Run("model_request_timeout", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				return SessionTurnResult{}, fmt.Errorf("request model: %w", &httpstream.RequestTimeoutError{
					Timeout:  60 * time.Second,
					Attempts: 2,
				})
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		assertSafeTurnFailedTerminal(t, events, "model_request_timeout", "model service did not return response headers after 2 attempts (1m0s each)", turnFailedSecret)
	})

	t.Run("model_stream_idle_timeout", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				return SessionTurnResult{}, fmt.Errorf("request model: %w", &httpstream.StreamIdleTimeoutError{Timeout: 2 * time.Minute})
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		assertSafeTurnFailedTerminal(t, events, "model_stream_idle_timeout", "model response stream produced no data for 2m0s", turnFailedSecret)
	})

	t.Run("runner_not_incremental", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
					return SessionTurnResult{}, err
				}
				return SessionTurnResult{Incremental: false}, nil
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if err == nil || !strings.Contains(err.Error(), "turn runner did not use incremental persistence") {
			t.Fatalf("error = %v, want not-incremental", err)
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(t, events, "runner_not_incremental", "turn runner did not persist incrementally", turnFailedSecret)
	})

	t.Run("turn_completion_failed", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
					return SessionTurnResult{}, err
				}
				removeSessionMetadata(t, home, request.Session.ID)
				return SessionTurnResult{Incremental: true}, nil
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if err == nil || !strings.Contains(err.Error(), "could not clear running turn") {
			t.Fatalf("error = %v, want could not clear running turn", err)
		}
		if !strings.Contains(err.Error(), "session not found") {
			t.Fatalf("error = %v, want underlying metadata error", err)
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(t, events, "turn_completion_failed", "could not complete turn", turnFailedSecret)
	})

	t.Run("session_reload_failed", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
					return SessionTurnResult{}, err
				}
				corruptSessionSegments(t, home, request.Session.ID)
				return SessionTurnResult{Incremental: true}, nil
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if err == nil {
			t.Fatalf("error = nil, want session reload error")
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(t, events, "session_reload_failed", "could not reload session", turnFailedSecret)
	})
}

func assertSafeTurnFailedTerminal(t *testing.T, events []SessionStreamEvent, wantCode, wantMessage, secret string) {
	t.Helper()
	types := sessionStreamEventTypes(events)
	if len(types) == 0 || types[0] != "turn.started" {
		t.Fatalf("first event = %#v, want turn.started", types)
	}
	if got := countString(types, "turn.failed"); got != 1 {
		t.Fatalf("turn.failed count = %d, want 1; events = %#v", got, types)
	}
	failedIdx := indexOfString(types, "turn.failed")
	if failedIdx != len(types)-1 {
		t.Fatalf("turn.failed index = %d, want last; events = %#v", failedIdx, types)
	}
	failed := events[failedIdx]
	if got, _ := failed["code"].(string); got != wantCode {
		t.Fatalf("turn.failed code = %q, want %q", got, wantCode)
	}
	if got, _ := failed["message"].(string); got != wantMessage {
		t.Fatalf("turn.failed message = %q, want %q", got, wantMessage)
	}
	if got, _ := failed["turn_id"].(string); got == "" {
		t.Fatalf("turn.failed missing turn_id: %#v", failed)
	}
	for i, event := range events {
		if strings.Contains(fmt.Sprintf("%v", event), secret) {
			t.Fatalf("event %d leaks secret %q: %v", i, secret, event)
		}
	}
}

func blockSessionSegmentsDir(t *testing.T, home, sessionID string) {
	t.Helper()
	root, err := sessions.RootForHome(home)
	if err != nil {
		t.Fatalf("RootForHome error = %v", err)
	}
	segPath := filepath.Join(root, sessionID, "segments")
	if err := os.WriteFile(segPath, []byte("block"), 0o600); err != nil {
		t.Fatalf("block segments dir error = %v", err)
	}
}

func removeSessionMetadata(t *testing.T, home, sessionID string) {
	t.Helper()
	root, err := sessions.RootForHome(home)
	if err != nil {
		t.Fatalf("RootForHome error = %v", err)
	}
	if err := os.Remove(filepath.Join(root, sessionID, "meta.json")); err != nil {
		t.Fatalf("remove metadata error = %v", err)
	}
}

func corruptSessionSegments(t *testing.T, home, sessionID string) {
	t.Helper()
	root, err := sessions.RootForHome(home)
	if err != nil {
		t.Fatalf("RootForHome error = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(root, sessionID, "segments", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob segments error = %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no session segments found to corrupt for %s", sessionID)
	}
	for _, p := range matches {
		if err := os.WriteFile(p, []byte("not-valid-json\n"), 0o600); err != nil {
			t.Fatalf("corrupt segment %q error = %v", p, err)
		}
	}
}
