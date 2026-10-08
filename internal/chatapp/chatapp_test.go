package chatapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessionlog"
)

// scriptedProvider plays a fixed sequence of provider responses across the
// agent loop's iterations. When stream is set it takes precedence, for
// providers that need to hang until cancellation.
type scriptedProvider struct {
	responses [][]model.Event
	stream    func(ctx context.Context) (<-chan model.Event, error)
	calls     int
	requests  []model.Request
}

func (p *scriptedProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if p.stream != nil {
		p.calls++
		p.requests = append(p.requests, request)
		return p.stream(ctx)
	}
	index := p.calls
	p.calls++
	p.requests = append(p.requests, request)
	if index >= len(p.responses) {
		return nil, errors.New("scripted provider exhausted")
	}
	events := make(chan model.Event, len(p.responses[index])+1)
	for _, event := range p.responses[index] {
		events <- event
	}
	close(events)
	return events, nil
}

func textResponse(text string) []model.Event {
	return []model.Event{
		model.TextDeltaEvent{Text: text},
		model.MessageDoneEvent{FinishReason: "stop"},
	}
}

func toolCallResponse(call model.ToolCall) []model.Event {
	return []model.Event{
		model.ToolCallDoneEvent{ToolCall: call},
		model.MessageDoneEvent{FinishReason: "tool_calls"},
	}
}

func writeTestConfig(t *testing.T, root string) {
	t.Helper()
	providerDir := filepath.Join(root, "providers")
	if err := os.MkdirAll(providerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	providerYAML := `name: fake
base_url: http://127.0.0.1:1/v1
api_key: test-key
models:
  test-model:
    id: test-model
    type: openai-chat
    parameters:
      temperature: 0.1
`
	if err := os.WriteFile(filepath.Join(providerDir, "fake.yaml"), []byte(providerYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	rootYAML := "default_provider: fake\ndefault_model: test-model\nprovider_dir: providers\n"
	if err := os.WriteFile(filepath.Join(root, "sai.yaml"), []byte(rootYAML), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSendRunSettlesAndPersists(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root)
	app, err := New(Options{ServerRoot: root, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	scripted := &scriptedProvider{responses: [][]model.Event{
		textResponse("hello there"),
	}}
	app.provider = scripted

	session, err := app.NewSession()
	if err != nil {
		t.Fatal(err)
	}

	if err := session.Send("hi"); err != nil {
		t.Fatal(err)
	}
	waitForSettle(t, session)

	state := session.Snapshot()
	if len(state.Items) != 2 {
		t.Fatalf("items = %d, want 2 (user + assistant)", len(state.Items))
	}
	if state.Items[1].Content != "hello there" {
		t.Fatalf("assistant item = %+v", state.Items[1])
	}
	if len(state.Context) != 2 {
		t.Fatalf("context = %d messages, want 2", len(state.Context))
	}
	if session.Running() {
		t.Fatal("session still running")
	}
	session.Close()
}

func TestToolRoundTripPersistsContext(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root)
	app, err := New(Options{ServerRoot: root, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	scripted := &scriptedProvider{responses: [][]model.Event{
		// Iteration 1: assistant asks for a tool call.
		toolCallResponse(model.ToolCall{ID: "call-1", Name: "read_file", Arguments: `{"path":"sai.yaml"}`}),
		// Iteration 2: final text.
		textResponse("the config is fine"),
	}}
	app.provider = scripted

	session, _ := app.NewSession()
	if err := session.Send("check the config"); err != nil {
		t.Fatal(err)
	}
	waitForSettle(t, session)

	state := session.Snapshot()
	kinds := make([]string, 0, len(state.Items))
	for _, item := range state.Items {
		kinds = append(kinds, item.Kind)
	}
	want := "user,assistant,tool,assistant"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("item kinds = %v, want %v", kinds, strings.Split(want, ","))
	}

	// The second provider request must carry the tool call and its result in
	// context, ordered assistant(tool_calls) → tool result.
	if len(scripted.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(scripted.requests))
	}
	second := scripted.requests[1].Messages
	roles := make([]string, 0, len(second))
	for _, message := range second {
		roles = append(roles, string(message.Role))
	}
	// system, user, assistant, tool, assistant(from iteration 1's final? no—
	// iteration 2 request has: system, user, assistant(tool calls), tool
	if strings.Join(roles, ",") != "system,user,assistant,tool" {
		t.Fatalf("second request roles = %v", roles)
	}
	if second[2].ToolCalls[0].ID != "call-1" || second[3].ToolCallID != "call-1" {
		t.Fatalf("tool exchange not in context: %+v", second[2:])
	}
	session.Close()
}

func TestResumeSessionRebuildsContext(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root)
	app, _ := New(Options{ServerRoot: root, CWD: root})
	scripted := &scriptedProvider{responses: [][]model.Event{textResponse("first answer")}}
	app.provider = scripted

	session, _ := app.NewSession()
	if err := session.Send("first question"); err != nil {
		t.Fatal(err)
	}
	waitForSettle(t, session)
	id := session.ID()
	session.Close()

	resumed, err := app.ResumeSession(id)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	state := resumed.Snapshot()
	if len(state.Context) != 2 || state.Context[0].Content != "first question" {
		t.Fatalf("resumed context = %+v", state.Context)
	}

	// A follow-up send on the resumed session includes the prior exchange.
	scripted.responses = [][]model.Event{textResponse("second answer")}
	if err := resumed.Send("second question"); err != nil {
		t.Fatal(err)
	}
	waitForSettle(t, resumed)
	if len(scripted.requests[1].Messages) != 4 { // system, user, assistant, user
		t.Fatalf("follow-up request = %+v", scripted.requests[1].Messages)
	}
	resumed.Close()
}

func TestStopInterruptsRun(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root)
	app, _ := New(Options{ServerRoot: root, CWD: root})
	// A provider that hangs until cancelled.
	hanging := &scriptedProvider{stream: func(ctx context.Context) (<-chan model.Event, error) {
		events := make(chan model.Event)
		go func() {
			defer close(events)
			<-ctx.Done()
		}()
		return events, nil
	}}
	app.provider = hanging

	session, _ := app.NewSession()
	if err := session.Send("long running"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	session.Stop()
	waitForSettle(t, session)

	state := session.Snapshot()
	// An interrupted run needs no record: the user input simply stays
	// unanswered, and no failure item appears.
	if len(state.Items) != 1 || state.Items[0].Kind != sessionlog.ItemKindUser {
		t.Fatalf("interrupted items = %+v", state.Items)
	}
	if len(state.Context) != 1 {
		t.Fatalf("interrupted context = %d messages, want 1", len(state.Context))
	}
	session.Close()
}

func TestSendWhileRunningRejected(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root)
	app, _ := New(Options{ServerRoot: root, CWD: root})
	hanging := &scriptedProvider{stream: func(ctx context.Context) (<-chan model.Event, error) {
		events := make(chan model.Event)
		go func() { defer close(events); <-ctx.Done() }()
		return events, nil
	}}
	app.provider = hanging

	session, _ := app.NewSession()
	if err := session.Send("one"); err != nil {
		t.Fatal(err)
	}
	if err := session.Send("two"); err == nil {
		t.Fatal("expected rejection when a run is active")
	}
	session.Stop()
	waitForSettle(t, session)
	session.Close()
}

func waitForSettle(t *testing.T, session *Session) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !session.Running() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run did not settle in time")
}

func TestUnknownEnabledToolsAreSkipped(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root)
	// Enable builtins plus retired-stack tools that chatapp never registers.
	rootYAML := "default_provider: fake\ndefault_model: test-model\nprovider_dir: providers\ntools:\n  enabled: [read_file, session_models, session_start]\n"
	if err := os.WriteFile(filepath.Join(root, "sai.yaml"), []byte(rootYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := New(Options{ServerRoot: root, CWD: root})
	if err != nil {
		t.Fatalf("New failed on config with unknown tools: %v", err)
	}
	if len(app.toolSchemas) != 1 || app.toolSchemas[0].Name != "read_file" {
		t.Fatalf("tool schemas = %+v, want only read_file", app.toolSchemas)
	}
	if app.ToolNotice() == "" || !strings.Contains(app.ToolNotice(), "session_models") {
		t.Fatalf("tool notice = %q", app.ToolNotice())
	}
}

func TestAllUnknownToolsFallBackToBuiltins(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root)
	rootYAML := "default_provider: fake\ndefault_model: test-model\nprovider_dir: providers\ntools:\n  enabled: [session_models]\n"
	if err := os.WriteFile(filepath.Join(root, "sai.yaml"), []byte(rootYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := New(Options{ServerRoot: root, CWD: root})
	if err != nil {
		t.Fatalf("New failed on config with only unknown tools: %v", err)
	}
	if len(app.toolSchemas) == 0 {
		t.Fatal("fallback to builtins did not happen")
	}
}

func TestSanitizeContextPatchesDanglingToolCalls(t *testing.T) {
	// A run crashed mid-tool leaves an assistant tool call without its
	// result, followed by the next user input.
	input := []model.Message{
		{Role: model.MessageRoleUser, Content: "q1"},
		{Role: model.MessageRoleAssistant, Content: "checking", ToolCalls: []model.ToolCall{
			{ID: "call-1", Name: "read_file", Arguments: "{}"},
			{ID: "call-2", Name: "shell", Arguments: "{}"},
		}},
		{Role: model.MessageRoleUser, Content: "q2"},
	}
	patched := sanitizeContext(input)
	roles := make([]string, 0, len(patched))
	for _, message := range patched {
		roles = append(roles, string(message.Role))
	}
	// Synthesized results land between the tool call and the next user
	// message, in call order.
	want := "user,assistant,tool,tool,user"
	if strings.Join(roles, ",") != want {
		t.Fatalf("patched roles = %v, want %v", roles, strings.Split(want, ","))
	}
	if patched[2].ToolCallID != "call-1" || patched[3].ToolCallID != "call-2" {
		t.Fatalf("patched order = %q, %q", patched[2].ToolCallID, patched[3].ToolCallID)
	}
	if !patched[2].IsError || patched[2].Content != "[tool execution interrupted]" {
		t.Fatalf("patched result = %+v", patched[2])
	}
}

func TestSanitizeContextTrailingDanglingCalls(t *testing.T) {
	// A crash can also leave the dangling calls at the very end of the log.
	input := []model.Message{
		{Role: model.MessageRoleUser, Content: "q1"},
		{Role: model.MessageRoleAssistant, ToolCalls: []model.ToolCall{{ID: "call-1", Name: "shell", Arguments: "{}"}}},
	}
	patched := sanitizeContext(input)
	if len(patched) != 3 || patched[2].Role != model.MessageRoleTool || patched[2].ToolCallID != "call-1" {
		t.Fatalf("patched = %+v", patched)
	}
}

func TestSanitizeContextKeepsCompleteExchanges(t *testing.T) {
	input := []model.Message{
		{Role: model.MessageRoleUser, Content: "q1"},
		{Role: model.MessageRoleAssistant, ToolCalls: []model.ToolCall{{ID: "call-1", Name: "shell", Arguments: "{}"}}},
		{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: "ok"},
		{Role: model.MessageRoleAssistant, Content: "done"},
	}
	patched := sanitizeContext(input)
	if len(patched) != len(input) {
		t.Fatalf("patched = %d messages, want %d (nothing to fix)", len(patched), len(input))
	}
}

func TestFailedRunRecordsFailure(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root)
	app, _ := New(Options{ServerRoot: root, CWD: root})
	scripted := &scriptedProvider{}
	scripted.stream = func(ctx context.Context) (<-chan model.Event, error) {
		return nil, errors.New("connection refused")
	}
	app.provider = scripted

	session, _ := app.NewSession()
	defer session.Close()
	if err := session.Send("hello"); err != nil {
		t.Fatal(err)
	}
	waitForSettle(t, session)

	state := session.Snapshot()
	last := state.Items[len(state.Items)-1]
	if last.Kind != sessionlog.ItemKindError || last.Content == "" {
		t.Fatalf("failure item = %+v", last)
	}
	// The failure stays out of the model context.
	if len(state.Context) != 1 {
		t.Fatalf("context = %d messages, want 1", len(state.Context))
	}
}
