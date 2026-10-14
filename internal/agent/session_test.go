package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/storage"
	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
	"github.com/rexzhao/simple-agent/internal/storage/memory"
)

// testModel is the meta every test session records unless a test
// overrides it.
var testModel = storage.SessionMeta{Provider: "test", Model: "model-test"}

// newTestAgent assembles an agent over an in-memory store with a static
// resolver: every session resolves to the given provider and the
// default test budget. Tests that need per-session budgets use
// newBudgetAgent.
func newTestAgent(t *testing.T, provider model.Provider, executor ToolExecutor, options ...func(*Options)) (*Agent, *Session) {
	t.Helper()
	optionsStruct := Options{
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       memory.OpenStore(),
		Tools:         []model.ToolDefinition{{Name: "echo"}},
		ToolExecutor:  executor,
	}
	for _, apply := range options {
		apply(&optionsStruct)
	}
	ag, err := New(optionsStruct)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, err := ag.NewSession("test-ns", testModel)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return ag, session
}

// waitSettled waits for the active run to end.
func waitSettled(t *testing.T, session *Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for session.Running() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the run to settle")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// sendCollectEvents sends one input and collects the run's Transient
// events from the session stream until the terminal status — the
// stream-era replacement for draining the per-run channel after
// waitSettled (events of an unsubscribed run are gone; the subscription
// must exist before the Send).
func sendCollectEvents(t *testing.T, session *Session, text string) []model.Event {
	t.Helper()
	sub := session.Stream()
	defer sub.Stop()
	if err := session.Send(text); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var events []model.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-sub.Events():
			switch e := ev.(type) {
			case Transient:
				events = append(events, e.Event)
			case Status:
				if !e.Running {
					return events
				}
			}
		case <-timeout:
			t.Fatal("timed out waiting for the terminal status")
		}
	}
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

	records := session.SnapshotStream().Records
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

	events := sendCollectEvents(t, session, "use the tool")

	records := session.SnapshotStream().Records
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
	committed := session.SnapshotStream().Records
	if len(committed) != 2 || committed[1].Type != storage.TypeUserInput {
		t.Fatalf("committed records mid-stream = %v, want session.created + user.input only", recordTypes(committed))
	}

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

	records := session.SnapshotStream().Records
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

	events := sendCollectEvents(t, session, "hi")
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	if collectText(events) != "recovered" {
		t.Fatalf("text = %q, want recovered", collectText(events))
	}

	records := session.SnapshotStream().Records
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

	records := session.SnapshotStream().Records
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

	records := session.SnapshotStream().Records
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

	records := session.SnapshotStream().Records
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

	resumed, err := ag.OpenSession("test-ns", id)
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
	if len(snapshot.Context) != 1 || !strings.HasSuffix(snapshot.Context[0].Content, "the summary") {
		t.Fatalf("context after compact = %+v", snapshot.Context)
	}
	if snapshot.Compacting != nil {
		t.Fatal("compacting slot not cleared")
	}

	// The record landed, between the input and nothing else pending.
	records := session.SnapshotStream().Records
	found := false
	for _, record := range records {
		if record.Type == storage.TypeContextCompacted {
			found = true
			if !strings.HasSuffix(record.Content, "the summary") || record.ItemID == "" {
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
		continuation[0].Role != model.MessageRoleUser || !strings.HasSuffix(continuation[0].Content, "the summary") ||
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
	records := session.SnapshotStream().Records
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

func TestCompactSegmentsAcrossReopenWithJSONL(t *testing.T) {
	store, err := jsonl.OpenStore(filepath.Join(t.TempDir(), "data"))
	ns := projects.ProjectID(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "the answer"}},
		{model.TextDeltaEvent{Text: "the summary"}},
	}}
	ag, err := New(Options{
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       store,
		CWD:           "/w",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, err := ag.NewSession(ns, testModel)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := session.Send("question"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitSettled(t, session)
	if err := session.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The reopened session folds only the active segment: metadata and
	// the summary, never the archived pre-compaction history.
	resumed, err := ag.OpenSession(ns, id)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	snapshot := resumed.Snapshot()
	if len(snapshot.Context) != 1 || !strings.HasSuffix(snapshot.Context[0].Content, "the summary") {
		t.Fatalf("resumed context = %+v, want the summary alone", snapshot.Context)
	}
	if snapshot.Provider != "test" || snapshot.CWD == "" {
		t.Fatalf("session metadata lost across segments: %+v", snapshot)
	}
	if err := resumed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// MaterializeHeader: the header lands through the same write path as a
// commit (fold + records cache + storage), a second call is a no-op,
// and the pending slot is cleared.
func TestMaterializeHeader(t *testing.T) {
	_, session := newTestAgent(t, &fakeProvider{}, nil)

	if got := len(sessionRecords(t, session)); got != 0 {
		t.Fatalf("fresh session has %d records", got)
	}
	if err := session.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	records := sessionRecords(t, session)
	if len(records) != 1 || records[0].Type != storage.TypeSessionCreated {
		t.Fatalf("records = %+v, want only the header", records)
	}
	if snapshot := session.Snapshot(); snapshot.Title != "" || len(snapshot.Context) != 0 {
		t.Fatalf("header must fold into no context: %+v", snapshot)
	}

	// Second call: no-op (the pending slot is cleared).
	if err := session.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	if got := len(sessionRecords(t, session)); got != 1 {
		t.Fatalf("after second materialize: %d records, want 1", got)
	}
}

// TestSettleOrderCanary pins the C1 invariant from milestone A at the
// Done edge: the moment a Run's done channel closes, the session is
// fully retired — Running() is false, Start succeeds, Steer returns
// ErrNoRun. The teardown clears currentRun under the lock BEFORE
// closing done, so the done-edge observation guarantees retirement.
// (The earlier "wait until Running()==false" formulation was a
// tautology — it polled the same lock the teardown writes — and had
// no kill power against a reordering that closes done before settle.)
func TestSettleOrderCanary(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "done"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, nil)

	r, err := session.Start(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()

	// The Done edge: these must hold IMMEDIATELY, no polling, in this
	// order — Steer first (it's the one the old canary missed), then
	// Running, then Start.
	if _, err := session.Steer("no target"); err != ErrNoRun {
		t.Fatalf("Steer right after <-Done: %v, want ErrNoRun", err)
	}
	if session.Running() {
		t.Fatal("Running() still true right after <-Done — settle did not retire before done close")
	}
	if err := session.Send("immediate follow-up"); err != nil {
		t.Fatalf("Send right after <-Done: %v (currentRun not cleared?)", err)
	}
	waitSettled(t, session)
}

// TestRunErrSemantics pins the N1 contract on all five paths.
func TestRunErrSemantics(t *testing.T) {
	t.Run("FailedTurnRun", func(t *testing.T) {
		provider := &fakeProvider{turns: [][]model.Event{}} // no turns → provider error
		_, session := newTestAgent(t, provider, nil)
		r, err := session.Start(context.Background(), "hello")
		if err != nil {
			t.Fatal(err)
		}
		<-r.Done()
		if r.Err() == nil {
			t.Fatal("failed turn run: Err() = nil, want non-nil")
		}
		if errors.Is(r.Err(), ErrInterrupted) {
			t.Fatalf("failed turn run: Err() = ErrInterrupted, want a real failure")
		}
	})

	// The two interruption sub-tests are the WIRING-level coverage:
	// the table-driven tests pin the pure function's priority order,
	// but only these catch a call site passing the wrong argument
	// (e.g. ctx.Err() → nil would silently misclassify every
	// interruption as a failure — review verified with that exact
	// mutation that no other test in the repo catches it).
	t.Run("InterruptedTurnRun", func(t *testing.T) {
		gate := make(chan struct{})
		t.Cleanup(func() { close(gate) })
		provider := &ctxGatedProvider{gate: gate}
		_, session := newTestAgent(t, provider, nil)
		r, err := session.Start(context.Background(), "hello")
		if err != nil {
			t.Fatal(err)
		}
		r.Stop()
		<-r.Done()
		if !errors.Is(r.Err(), ErrInterrupted) {
			t.Fatalf("interrupted turn run: Err() = %v, want ErrInterrupted", r.Err())
		}
	})

	t.Run("InterruptedCompactRun", func(t *testing.T) {
		provider := &blockSummaryProvider{
			release: make(chan struct{}),
			entered: make(chan struct{}),
			turns:   [][]model.Event{{model.TextDeltaEvent{Text: "answer"}}},
		}
		t.Cleanup(func() { close(provider.release) })
		_, session := newTestAgent(t, provider, nil)
		if err := session.Send("hello"); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, session)

		r, err := session.StartCompact(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		<-provider.entered
		r.Stop()
		<-r.Done()
		if !errors.Is(r.Err(), ErrInterrupted) {
			t.Fatalf("interrupted compact: Err() = %v, want ErrInterrupted", r.Err())
		}
	})
}

// ctxGatedProvider blocks on a gate but honors ctx cancellation —
// unlike gatedProvider, which ignores ctx and hangs interrupted runs.
type ctxGatedProvider struct {
	gate chan struct{}
}

func (p *ctxGatedProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event)
	go func() {
		defer close(events)
		select {
		case events <- model.TextDeltaEvent{Text: "partial"}:
		case <-ctx.Done():
			return
		}
		select {
		case <-p.gate:
			events <- model.TextDeltaEvent{Text: " message"}
		case <-ctx.Done():
		}
	}()
	return events, nil
}

// TestTurnOutcomeTable pins the N1 priority order as a pure-function
// table: completed > interrupted > failed. Kill-verified by design —
// swapping the first two branches of turnOutcome makes the "completed
// + ctx cancelled" row fail.
func TestTurnOutcomeTable(t *testing.T) {
	ctxErr := context.Canceled
	for _, c := range []struct {
		name      string
		completed bool
		ctxErr    error
		failure   string
		want      error // nil = expect nil; ErrInterrupted = expect interrupted; else compare by message
	}{
		{"success", true, nil, "", nil},
		{"success + late stop", true, ctxErr, "", nil},
		{"success + late stop + stale failure", true, ctxErr, "stale", nil},
		{"interrupted", false, ctxErr, "", ErrInterrupted},
		{"interrupted + provider error", false, ctxErr, "provider", ErrInterrupted},
		{"failed", false, nil, "provider", errors.New("provider")},
		{"idle (no path — bug)", false, nil, "", errors.New("run ended without an outcome")},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := turnOutcome(c.completed, c.ctxErr, c.failure)
			if c.want == nil {
				if got != nil {
					t.Fatalf("turnOutcome(%v, %v, %q) = %v, want nil", c.completed, c.ctxErr, c.failure, got)
				}
				return
			}
			if c.want == ErrInterrupted {
				if !errors.Is(got, ErrInterrupted) {
					t.Fatalf("turnOutcome(%v, %v, %q) = %v, want ErrInterrupted", c.completed, c.ctxErr, c.failure, got)
				}
				return
			}
			// Failure and fallback both return NEW errors; compare by
			// message, not identity.
			if got == nil || got.Error() != c.want.Error() {
				t.Fatalf("turnOutcome(%v, %v, %q) = %v, want %q", c.completed, c.ctxErr, c.failure, got, c.want.Error())
			}
		})
	}
}

// TestCompactOutcomeTable pins the compact-run counterpart: success
// (err == nil) takes priority over a late Stop.
func TestCompactOutcomeTable(t *testing.T) {
	ctxErr := context.Canceled
	compactErr := errors.New("compact: summary was empty")
	for _, c := range []struct {
		name   string
		err    error
		ctxErr error
		want   error
	}{
		{"success", nil, nil, nil},
		{"success + late stop", nil, ctxErr, nil},
		{"interrupted", compactErr, ctxErr, ErrInterrupted},
		{"failed", compactErr, nil, compactErr},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := compactOutcome(c.err, c.ctxErr)
			if c.want == nil {
				if got != nil {
					t.Fatalf("compactOutcome(%v, %v) = %v, want nil", c.err, c.ctxErr, got)
				}
				return
			}
			if c.want == ErrInterrupted {
				if !errors.Is(got, ErrInterrupted) {
					t.Fatalf("compactOutcome(%v, %v) = %v, want ErrInterrupted", c.err, c.ctxErr, got)
				}
				return
			}
			if got == nil || got.Error() != c.want.Error() {
				t.Fatalf("compactOutcome(%v, %v) = %v, want %v", c.err, c.ctxErr, got, c.want)
			}
		})
	}
}

// TestSuccessfulTurnRunErrNil pins F1's wiring: a completed turn run's
// Err() is nil. If someone deletes `completed = true` from the execute
// path, the fallback branch returns "run ended without an outcome"
// and this test fails — the pure-function table can't catch that
// (it only tests the function's logic, not the call site's arguments).
func TestSuccessfulTurnRunErrNil(t *testing.T) {
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "done"}, model.MessageDoneEvent{}},
	}}
	_, session := newTestAgent(t, provider, nil)
	r, err := session.Start(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	if r.Err() != nil {
		t.Fatalf("successful turn run: Err() = %v, want nil (completed flag not set?)", r.Err())
	}
}

// TestCooldownCountersRebuiltAtOpen pins §14.4's Open-time rebuild: the
// since-compacted counter is derived from the active segment (the user
// inputs after the last compaction), the has-compacted marker is set,
// and the soft-attempt counter stays memory-only (a restart re-attempts
// immediately).
func TestCooldownCountersRebuiltAtOpen(t *testing.T) {
	store := memory.OpenStore()
	provider := &fakeProvider{turns: [][]model.Event{
		{model.TextDeltaEvent{Text: "answer"}},
		{model.TextDeltaEvent{Text: "the summary"}},
		{model.TextDeltaEvent{Text: "more"}},
	}}
	ag, err := New(Options{
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       store,
		CWD:           "/w",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns", testModel)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"one", "two"} {
		if err := session.Send(input); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, session)
	}
	if err := session.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := session.Send("three"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)

	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := ag.OpenSession("test-ns", id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.mu.RLock()
	sinceCompacted, hasCompacted, armed := reopened.inputsSinceCompacted, reopened.hasCompacted, reopened.softAttemptArmed
	reopened.mu.RUnlock()
	if sinceCompacted != 1 {
		t.Fatalf("inputsSinceCompacted = %d, want 1 (the input after the compaction)", sinceCompacted)
	}
	if !hasCompacted {
		t.Fatal("hasCompacted not rebuilt from the active segment")
	}
	if armed {
		t.Fatal("the soft-attempt cooldown must be memory-only, not rebuilt at Open")
	}
}

// TestCompactionBoundsRecordMemory pins §14.4's memory bound: the
// in-process record stream is the ACTIVE SEGMENT, not the flat history
// — after several compaction cycles with turns between, the records
// stay at [session.created, compaction, <this segment's records>]
// while the session has committed far more overall.
func TestCompactionBoundsRecordMemory(t *testing.T) {
	store := memory.OpenStore()
	turns := make([][]model.Event, 0, 12)
	for i := 0; i < 5; i++ {
		turns = append(turns,
			[]model.Event{model.TextDeltaEvent{Text: "answer"}},
			[]model.Event{model.TextDeltaEvent{Text: "summary"}}, // the compaction summary
		)
	}
	turns = append(turns, []model.Event{model.TextDeltaEvent{Text: "final"}})
	provider := &fakeProvider{turns: turns}
	ag, err := New(Options{
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       store,
		CWD:           "/w",
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns", testModel)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	committed := 1 // session.created
	for i := 0; i < 5; i++ {
		if err := session.Send(fmt.Sprintf("turn %d", i)); err != nil {
			t.Fatal(err)
		}
		waitSettled(t, session)
		committed += 2
		if err := session.Compact(); err != nil {
			t.Fatal(err)
		}
		committed++
	}
	if err := session.Send("after"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	committed += 2

	records := session.SnapshotStream().Records
	// The active segment: [created, compacted, user.input, assistant].
	if len(records) != 4 {
		t.Fatalf("records = %d (%v), want the 4-record active segment — memory is not bounded to it (committed ≈ %d)",
			len(records), recordTypes(records), committed)
	}
	if records[1].Type != storage.TypeContextCompacted {
		t.Fatalf("segment structure = %v, want [created, compacted, ...]", recordTypes(records))
	}
}
