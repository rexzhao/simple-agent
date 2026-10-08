package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/storage"
	"github.com/rexzhao/simple-agent/internal/storage/memory"
)

// newTestAgent assembles an agent over an in-memory store.
func newTestAgent(t *testing.T, provider model.Provider, executor ToolExecutor, options ...func(*Options)) (*Agent, *Session) {
	t.Helper()
	optionsStruct := Options{
		Provider:     provider,
		ProviderName: "test",
		ModelID:      "model-test",
		Storage:      memory.OpenStore(),
		Tools:        []model.Tool{{Name: "echo"}},
		ToolExecutor: executor,
	}
	for _, apply := range options {
		apply(&optionsStruct)
	}
	ag, err := New(optionsStruct)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, err := ag.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return ag, session
}

// waitSettled waits for the active run to end and drains its events.
func waitSettled(t *testing.T, session *Session) []model.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for session.Running() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the run to settle")
		}
		time.Sleep(2 * time.Millisecond)
	}
	var events []model.Event
	timeout := time.After(5 * time.Second)
	for event := range mustEvents(t, session) {
		events = append(events, event)
		select {
		case <-timeout:
			t.Fatal("timed out draining run events")
		default:
		}
	}
	return events
}

func mustEvents(t *testing.T, session *Session) <-chan model.Event {
	t.Helper()
	events := session.Events()
	if events == nil {
		t.Fatal("no active run events channel")
	}
	return events
}

func recordTypes(records []storage.Record) []string {
	types := make([]string, len(records))
	for i, record := range records {
		types[i] = record.Type
	}
	return types
}

func TestSendPersistsInputAndFinalMessage(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "hel"}, model.TextDeltaEvent{Text: "lo"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, nil)

	if session.Persisted() {
		t.Fatal("fresh session reports persisted before any record")
	}
	if session.ID() == "" {
		t.Fatal("session id is empty at creation")
	}
	if err := session.Send("hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)

	sub := session.Subscribe()
	records := sub.Initial()
	sub.Stop()
	if want := []string{storage.TypeSessionCreated, storage.TypeUserInput, storage.TypeAssistantMessage}; !reflect.DeepEqual(recordTypes(records), want) {
		t.Fatalf("records = %v, want %v", recordTypes(records), want)
	}

	// The input and the answer share a run id, greppable in the log.
	if records[1].RunID == "" || records[1].RunID != records[2].RunID {
		t.Fatalf("run ids do not match: %q vs %q", records[1].RunID, records[2].RunID)
	}
	if records[2].Content != "hello" {
		t.Fatalf("assistant content = %q, want hello", records[2].Content)
	}
	if records[2].ItemID == "" || records[2].ItemID == records[1].ItemID {
		t.Fatalf("assistant item id = %q, want a distinct minted id", records[2].ItemID)
	}
	if !session.Persisted() {
		t.Fatal("session reports not persisted after commits")
	}

	snapshot := session.Snapshot()
	if len(snapshot.Context) != 2 {
		t.Fatalf("context length = %d, want user + assistant", len(snapshot.Context))
	}
	if snapshot.Streaming != nil {
		t.Fatal("streaming slot not cleared after commit")
	}
}

func TestToolRoundTripCommitsRecords(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "echo", Arguments: `{}`}}},
		{model.TextDeltaEvent{Text: "final"}, model.MessageDoneEvent{}},
	}}
	executor := &fakeToolExecutor{result: model.ToolResult{Content: "tool output"}}
	_, session := newTestAgent(t, provider, executor)

	if err := session.Send("use the tool"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	events := waitSettled(t, session)

	sub := session.Subscribe()
	records := sub.Initial()
	sub.Stop()
	if want := []string{
		storage.TypeSessionCreated, storage.TypeUserInput,
		storage.TypeAssistantMessage, storage.TypeToolResult,
		storage.TypeAssistantMessage,
	}; !reflect.DeepEqual(recordTypes(records), want) {
		t.Fatalf("records = %v, want %v", recordTypes(records), want)
	}
	toolResult := records[3]
	if toolResult.ToolCallID != "call-1" || toolResult.ToolName != "echo" || toolResult.Content != "tool output" {
		t.Fatalf("tool result record = %+v", toolResult)
	}
	if toolResult.Iter != records[2].Iter || toolResult.Iter != 1 {
		t.Fatalf("iteration mismatch: %d vs %d", toolResult.Iter, records[2].Iter)
	}

	// The provider saw the paired wire history on its second request.
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	second := provider.requests[1].Messages
	if len(second) != 3 {
		t.Fatalf("second request messages = %d, want user+assistant+tool", len(second))
	}
	// The in-run history carries explicit roles — the streaming snapshot is
	// an assistant message, not a zero-value one (a real provider rejects
	// empty roles with HTTP 400).
	wantRoles := []model.MessageRole{model.MessageRoleUser, model.MessageRoleAssistant, model.MessageRoleTool}
	for i, want := range wantRoles {
		if second[i].Role != want {
			t.Fatalf("second request messages[%d].Role = %q, want %q", i, second[i].Role, want)
		}
	}
	if second[2].Role != model.MessageRoleTool || second[2].ToolCallID != "call-1" {
		t.Fatalf("tool message not replayed: %+v", second[2])
	}

	// Transient stream carried the tool lifecycle in order, and the
	// started event named the result item before it existed.
	var lifecycle []string
	var startedItemID string
	for _, event := range events {
		switch event := event.(type) {
		case model.ToolCallDoneEvent:
			lifecycle = append(lifecycle, "requested")
		case model.ToolStartedEvent:
			lifecycle = append(lifecycle, "started")
			startedItemID = event.ItemID
		case model.ToolResultEvent:
			lifecycle = append(lifecycle, "finished")
		}
	}
	if want := []string{"requested", "started", "finished"}; !reflect.DeepEqual(lifecycle, want) {
		t.Fatalf("tool lifecycle = %v, want %v", lifecycle, want)
	}
	if startedItemID == "" || startedItemID != toolResult.ItemID {
		t.Fatalf("ToolStartedEvent.ItemID = %q, want the tool.result item id %q", startedItemID, toolResult.ItemID)
	}
}

func TestStreamingSlotVisibleWhileGenerating(t *testing.T) {
	provider := &gatedProvider{gate: make(chan struct{})}
	_, session := newTestAgent(t, provider, nil)

	if err := session.Send("hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Wait until the run is mid-stream: the streaming slot holds the
	// partial message, identity included, before anything is committed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot := session.Snapshot()
		if snapshot.Streaming != nil {
			if snapshot.Streaming.Message.Content != "partial" {
				t.Fatalf("streaming content = %q, want partial", snapshot.Streaming.Message.Content)
			}
			if snapshot.Streaming.ItemID == "" {
				t.Fatal("streaming item id is empty")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("streaming slot never appeared")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Only the input is committed while the message is still generating.
	sub := session.Subscribe()
	committed := sub.Initial()
	if len(committed) != 2 || committed[1].Type != storage.TypeUserInput {
		t.Fatalf("committed records mid-stream = %v, want session.created + user.input only", recordTypes(committed))
	}
	sub.Stop()

	close(provider.gate)
	waitSettled(t, session)
	if session.Snapshot().Streaming != nil {
		t.Fatal("streaming slot not cleared after commit")
	}
}

func TestProviderErrorRecordsRunFailed(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{{}}, streamErrs: []error{errors.New("permanent failure")}}
	_, session := newTestAgent(t, provider, nil)

	if err := session.Send("hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)

	sub := session.Subscribe()
	records := sub.Initial()
	sub.Stop()
	if want := []string{storage.TypeSessionCreated, storage.TypeUserInput, storage.TypeRunFailed}; !reflect.DeepEqual(recordTypes(records), want) {
		t.Fatalf("records = %v, want %v", recordTypes(records), want)
	}
	if !strings.Contains(records[2].Error, "permanent failure") {
		t.Fatalf("run.failed error = %q", records[2].Error)
	}
	// The failed run leaves its error out of the model context, and the
	// half-generated message out of the Snapshot.
	snapshot := session.Snapshot()
	if len(snapshot.Context) != 1 || snapshot.Context[0].Role != model.MessageRoleUser {
		t.Fatalf("context after failure = %+v, want the unanswered input only", snapshot.Context)
	}
	if snapshot.Streaming != nil || snapshot.Running != nil {
		t.Fatalf("transient slots survive failure: streaming=%v running=%v", snapshot.Streaming, snapshot.Running)
	}
}

func TestRetryBeforeProgressRecovers(t *testing.T) {
	originalBackoff := providerRetryBackoff
	providerRetryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { providerRetryBackoff = originalBackoff })

	provider := &fakeProvider{turns: [][]model.Event{
		{model.ErrorEvent{Err: &model.ProviderError{Message: "server_error: temporary failure (code server_error)"}}},
		{model.TextDeltaEvent{Text: "recovered"}},
	}}
	_, session := newTestAgent(t, provider, nil)

	if err := session.Send("hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	events := waitSettled(t, session)
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	if collectText(events) != "recovered" {
		t.Fatalf("text = %q, want recovered", collectText(events))
	}

	sub := session.Subscribe()
	records := sub.Initial()
	sub.Stop()
	if want := []string{storage.TypeSessionCreated, storage.TypeUserInput, storage.TypeAssistantMessage}; !reflect.DeepEqual(recordTypes(records), want) {
		t.Fatalf("records = %v, want %v", recordTypes(records), want)
	}
	if records[2].Content != "recovered" {
		t.Fatalf("assistant content = %q, want recovered (retry content not reset)", records[2].Content)
	}
}

func TestStopInterruptsRunAndClosesCleanly(t *testing.T) {
	provider := &gatedProvider{gate: make(chan struct{})}
	_, session := newTestAgent(t, provider, nil)

	if err := session.Send("hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Wait for the streaming slot, then interrupt.
	deadline := time.Now().Add(5 * time.Second)
	for session.Snapshot().Streaming == nil {
		if time.Now().After(deadline) {
			t.Fatal("streaming slot never appeared")
		}
		time.Sleep(2 * time.Millisecond)
	}
	session.Stop()
	close(provider.gate)
	waitSettled(t, session)

	sub := session.Subscribe()
	records := sub.Initial()
	sub.Stop()
	// An interruption leaves the input unanswered: no assistant message,
	// no run.failed.
	if want := []string{storage.TypeSessionCreated, storage.TypeUserInput}; !reflect.DeepEqual(recordTypes(records), want) {
		t.Fatalf("records after interruption = %v, want %v", recordTypes(records), want)
	}
	// The transient region is cleared on settle: no ghost streaming
	// message or running tool survives in the Snapshot.
	snapshot := session.Snapshot()
	if snapshot.Streaming != nil || snapshot.Running != nil {
		t.Fatalf("transient slots survive interruption: streaming=%v running=%v", snapshot.Streaming, snapshot.Running)
	}
	// Close must not hang on the settled run.
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSendWhileRunningRejected(t *testing.T) {
	provider := &gatedProvider{gate: make(chan struct{})}
	_, session := newTestAgent(t, provider, nil)

	if err := session.Send("first"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := session.Send("second"); err == nil {
		t.Fatal("Send while running accepted")
	}
	session.Stop()
	close(provider.gate)
	waitSettled(t, session)
}

func TestMaxIterationsRecordsFailure(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "echo", Arguments: `{}`}}},
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-2", Name: "echo", Arguments: `{}`}}},
	}}
	executor := &fakeToolExecutor{result: model.ToolResult{Content: "ok"}}
	_, session := newTestAgent(t, provider, executor, func(options *Options) {
		options.MaxIterations = 2
	})

	if err := session.Send("loop forever"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)

	sub := session.Subscribe()
	records := sub.Initial()
	sub.Stop()
	want := []string{
		storage.TypeSessionCreated, storage.TypeUserInput,
		storage.TypeAssistantMessage, storage.TypeToolResult,
		storage.TypeAssistantMessage, storage.TypeToolResult,
		storage.TypeRunFailed,
	}
	if !reflect.DeepEqual(recordTypes(records), want) {
		t.Fatalf("records = %v, want %v", recordTypes(records), want)
	}
	if !strings.Contains(records[len(records)-1].Error, "max iterations 2") {
		t.Fatalf("run.failed error = %q", records[len(records)-1].Error)
	}
}

func TestMintsToolCallIDWhenProviderOmitsIt(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "echo", Arguments: `{}`}}},
		{model.TextDeltaEvent{Text: "done"}},
	}}
	executor := &fakeToolExecutor{result: model.ToolResult{Content: "ok"}}
	_, session := newTestAgent(t, provider, executor)

	if err := session.Send("hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)

	sub := session.Subscribe()
	records := sub.Initial()
	sub.Stop()
	assistant := records[2]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID == "" {
		t.Fatalf("minted tool call id missing: %+v", assistant.ToolCalls)
	}
	// The result pairs with the minted id, so the wire replay stays
	// self-consistent.
	if records[3].ToolCallID != assistant.ToolCalls[0].ID {
		t.Fatalf("tool result id = %q, want %q", records[3].ToolCallID, assistant.ToolCalls[0].ID)
	}
}

func TestOpenSessionRebuildsContextAndContinues(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "first answer"}},
		{model.TextDeltaEvent{Text: "second answer"}},
	}}
	ag, session := newTestAgent(t, provider, nil)
	if err := session.Send("first question"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	resumed, err := ag.OpenSession(id)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	snapshot := resumed.Snapshot()
	if len(snapshot.Context) != 2 || snapshot.Context[0].Content != "first question" {
		t.Fatalf("resumed context = %+v", snapshot.Context)
	}
	if err := resumed.Send("second question"); err != nil {
		t.Fatalf("Send after resume: %v", err)
	}
	waitSettled(t, resumed)
	// The second request carried the whole folded history.
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	if got := provider.requests[1].Messages; len(got) != 3 || got[0].Content != "first question" {
		t.Fatalf("second request messages = %+v, want folded history", got)
	}
}

func TestSanitizeContextPatchesDanglingToolCalls(t *testing.T) {
	// An interrupted run leaves an assistant tool call without its result.
	context := []model.Message{
		{Role: model.MessageRoleUser, Content: "run a tool"},
		{Role: model.MessageRoleAssistant, ToolCalls: []model.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: model.MessageRoleTool, ToolCallID: "a", Content: "ok"},
	}
	sanitized := sanitizeContext(context)
	want := []model.MessageRole{model.MessageRoleUser, model.MessageRoleAssistant, model.MessageRoleTool, model.MessageRoleTool}
	if len(sanitized) != len(want) {
		t.Fatalf("sanitized length = %d, want %d", len(sanitized), len(want))
	}
	for i := range want {
		if sanitized[i].Role != want[i] {
			t.Fatalf("sanitized[%d].Role = %s, want %s", i, sanitized[i].Role, want[i])
		}
	}
	if !sanitized[3].IsError || sanitized[3].ToolCallID != "b" {
		t.Fatalf("placeholder result = %+v, want interrupted error for call b", sanitized[3])
	}
}

func TestSanitizeContextKeepsCompleteExchanges(t *testing.T) {
	context := []model.Message{
		{Role: model.MessageRoleUser, Content: "hi"},
		{Role: model.MessageRoleAssistant, ToolCalls: []model.ToolCall{{ID: "a"}}},
		{Role: model.MessageRoleTool, ToolCallID: "a", Content: "ok"},
		{Role: model.MessageRoleUser, Content: "again"},
	}
	if got := sanitizeContext(context); len(got) != len(context) {
		t.Fatalf("sanitizeContext rewrote a complete history: %+v", got)
	}
}

func TestParseToolArguments(t *testing.T) {
	raw := string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})
	if _, err := parseToolArguments(raw); err == nil || !strings.Contains(err.Error(), "valid UTF-8") {
		t.Fatalf("parseToolArguments(invalid UTF-8) error = %v, want UTF-8 validation", err)
	}
	arguments, err := parseToolArguments(`{"x":"ok"}`)
	if err != nil || arguments["x"] != "ok" {
		t.Fatalf("parseToolArguments(valid) = %#v, err=%v", arguments, err)
	}
}

// fakeProvider serves one turn of events per Stream call.
type fakeProvider struct {
	turns      [][]model.Event
	streamErrs []error
	requests   []model.Request
}

func (p *fakeProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if len(p.requests) >= len(p.turns) {
		return nil, fmt.Errorf("unexpected model request %d", len(p.requests)+1)
	}
	turn := len(p.requests)
	p.requests = append(p.requests, request)

	if turn < len(p.streamErrs) && p.streamErrs[turn] != nil {
		return nil, p.streamErrs[turn]
	}
	events := make(chan model.Event, len(p.turns[turn]))
	for _, event := range p.turns[turn] {
		events <- event
	}
	close(events)
	return events, nil
}

// gatedProvider streams a partial message, then blocks on gate before
// finishing: a run which can be observed mid-stream and interrupted.
type gatedProvider struct {
	gate chan struct{}
}

func (p *gatedProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event)
	go func() {
		defer close(events)
		events <- model.TextDeltaEvent{Text: "partial"}
		<-p.gate
		events <- model.TextDeltaEvent{Text: " message"}
	}()
	return events, nil
}

type fakeToolExecutor struct {
	result model.ToolResult
	err    error
}

func (e *fakeToolExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error) {
	return e.result, e.err
}

func collectText(events []model.Event) string {
	var text strings.Builder
	for _, event := range events {
		if event, ok := event.(model.TextDeltaEvent); ok {
			text.WriteString(event.Text)
		}
	}
	return text.String()
}

func TestUnsubscribeDuringCommitNotifyDoesNotPanic(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{{model.TextDeltaEvent{Text: "x"}}}}
	_, session := newTestAgent(t, provider, nil)

	sub := session.Subscribe()
	// Saturate the subscription's buffer from a background goroutine, so a
	// commit's delivery is stuck in progress while Stop races it: the
	// records channel must never be closed underneath a delivery.
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; i < subscribeBuffer+10; i++ {
			_ = session.commit(storage.Record{
				ItemID: storage.NewUserInputID(), Type: storage.TypeUserInput,
				RunID: "run-test", Content: "fill",
			}, nil)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	// The delivery is stuck on the full buffer; Stop must release it —
	// not panic (send on closed channel), not deadlock.
	sub.Stop()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not release the blocked commit delivery")
	}

	// The session stays usable after the race.
	if err := session.commit(storage.Record{
		ItemID: storage.NewUserInputID(), Type: storage.TypeUserInput,
		RunID: "run-test", Content: "after stop",
	}, nil); err != nil {
		t.Fatalf("commit after Stop: %v", err)
	}
}

func TestCompactReplacesContextAndPersists(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "first answer"}},
		{model.TextDeltaEvent{Text: "the summary"}},
		{model.TextDeltaEvent{Text: "the answer"}},
	}}
	_, session := newTestAgent(t, provider, nil)
	if err := session.Send("first question"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)
	if err := session.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// The compacted context is exactly one summary message.
	snapshot := session.Snapshot()
	if len(snapshot.Context) != 1 || snapshot.Context[0].Content != "the summary" {
		t.Fatalf("context after compact = %+v", snapshot.Context)
	}
	if snapshot.Compacting != nil {
		t.Fatal("compacting slot not cleared")
	}

	// The record landed, between the input and nothing else pending.
	sub := session.Subscribe()
	records := sub.Initial()
	sub.Stop()
	found := false
	for _, record := range records {
		if record.Type == storage.TypeContextCompacted {
			found = true
			if record.Content != "the summary" || record.ItemID == "" {
				t.Fatalf("compaction record = %+v", record)
			}
			if record.RunID != "" {
				t.Fatalf("compaction is not a run: %+v", record)
			}
		}
	}
	if !found {
		t.Fatalf("no context.compacted record in %v", recordTypes(records))
	}

	// The summary call carried the compaction prompt and no tools.
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	summaryRequest := provider.requests[1]
	if summaryRequest.Tools != nil {
		t.Fatalf("compaction request offered tools: %+v", summaryRequest.Tools)
	}
	if summaryRequest.Messages[0].Role != model.MessageRoleSystem ||
		!strings.Contains(summaryRequest.Messages[0].Content, "Summarize the conversation") {
		t.Fatalf("compaction prompt missing: %+v", summaryRequest.Messages[0])
	}

	// A later Send continues from the summary.
	if err := session.Send("next"); err != nil {
		t.Fatalf("Send after compact: %v", err)
	}
	waitSettled(t, session)
	continuation := provider.requests[2].Messages
	if len(continuation) != 2 ||
		continuation[0].Role != model.MessageRoleUser || continuation[0].Content != "the summary" ||
		continuation[1].Content != "next" {
		t.Fatalf("continuation request = %+v, want summary + next input", continuation)
	}
}

func TestCompactFailureLeavesContextUntouched(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "first answer"}},
		{model.ErrorEvent{Err: errors.New("summary provider down")}},
	}}
	_, session := newTestAgent(t, provider, nil)
	if err := session.Send("question"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)

	err := session.Compact()
	if err == nil || !strings.Contains(err.Error(), "summary provider down") {
		t.Fatalf("Compact error = %v, want the provider failure", err)
	}
	sub := session.Subscribe()
	records := sub.Initial()
	sub.Stop()
	for _, record := range records {
		if record.Type == storage.TypeContextCompacted {
			t.Fatalf("failed compaction left a record: %+v", record)
		}
	}
	snapshot := session.Snapshot()
	if len(snapshot.Context) != 2 || snapshot.Compacting != nil {
		t.Fatalf("context changed by failed compaction: %+v", snapshot)
	}
}

func TestCompactGuards(t *testing.T) {
	provider := &gatedProvider{gate: make(chan struct{})}
	_, session := newTestAgent(t, provider, nil)

	// Empty context: nothing to compact.
	if err := session.Compact(); err == nil || !strings.Contains(err.Error(), "nothing to compact") {
		t.Fatalf("empty compact = %v, want rejection", err)
	}

	// While a run is active: rejected.
	if err := session.Send("hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := session.Compact(); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("compact while running = %v, want rejection", err)
	}
	session.Stop()
	close(provider.gate)
	waitSettled(t, session)
}
