package agent

// Tool interface dispatch tests (B2 step 4, §16.2): the agent loop
// dispatches by Definition().Name. The kills are the two failure
// forms the interface replaced — name-blind dispatch (every call
// hitting the first tool) and unknown names escaping as run failures
// instead of tool.result is_error records.

import (
	"context"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/memory"
)

// recordingTool is a Tool that records its executions.
type recordingTool struct {
	name     string
	calls    *[]string
	response model.ToolResult
	err      error
}

func (t recordingTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: t.name}
}

func (t recordingTool) Execute(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
	*t.calls = append(*t.calls, t.name)
	return t.response, t.err
}

// TestToolDispatchesByName pins the interface's core contract: a tool
// call reaches the tool whose Definition().Name matches the call's
// Name — and only that tool. A name-blind dispatch (the mutation:
// every call hits one fixed tool) fails both directions of the
// assertion.
func TestToolDispatchesByName(t *testing.T) {
	var calls []string
	first := recordingTool{name: "alpha", calls: &calls, response: model.ToolResult{Content: "alpha ran"}}
	second := recordingTool{name: "beta", calls: &calls, response: model.ToolResult{Content: "beta ran"}}
	provider := &fakeProvider{turns: [][]model.Event{
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "c1", Name: "beta", Arguments: `{}`}}},
		{model.TextDeltaEvent{Text: "done"}, model.MessageDoneEvent{}},
	}}
	ag, err := New(Options{
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       memory.OpenStore(),
		Tools:         []Tool{first, second},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns", testModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Send("go"); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, session)
	// The run committed exactly one tool.result — from beta.
	records := session.SnapshotStream().Records
	var toolResults []string
	for _, r := range records {
		if r.Type == storage.TypeToolResult {
			toolResults = append(toolResults, r.Content)
		}
	}
	if len(toolResults) != 1 || toolResults[0] != "beta ran" {
		t.Fatalf("tool results = %v, want exactly [beta ran] — dispatch is name-blind", toolResults)
	}
	if len(calls) != 1 || calls[0] != "beta" {
		t.Fatalf("executed tools = %v, want exactly [beta]", calls)
	}
}

// TestUnknownToolCallIsErrorResult pins the unknown-name path: a call
// for a name that is not in the set lands as a tool.result is_error
// ("tool %q is not enabled") — the run is NOT failed, the model sees
// the refusal. The pre-interface shape had the same contract
// (executeToolCall's toolErrorResult); the interface must keep it.
func TestUnknownToolCallIsErrorResult(t *testing.T) {
	var calls []string
	only := recordingTool{name: "alpha", calls: &calls, response: model.ToolResult{Content: "alpha ran"}}
	provider := &fakeProvider{turns: [][]model.Event{
		{model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "c1", Name: "nope", Arguments: `{}`}}},
		{model.TextDeltaEvent{Text: "done"}, model.MessageDoneEvent{}},
	}}
	ag, err := New(Options{
		ModelResolver: NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       memory.OpenStore(),
		Tools:         []Tool{only},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession("test-ns", testModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Send("go"); err != nil {
		t.Fatalf("send: %v — an unknown tool name must not fail the run", err)
	}
	waitSettled(t, session)
	if len(calls) != 0 {
		t.Fatalf("the registered tool was executed for a foreign name: %v", calls)
	}
	records := session.SnapshotStream().Records
	var refusal string
	for _, r := range records {
		if r.Type == storage.TypeToolResult && r.IsError {
			refusal = r.Content
		}
	}
	if !strings.Contains(refusal, `"nope" is not enabled`) {
		t.Fatalf("error tool result = %q, want the not-enabled refusal", refusal)
	}
	// The run settled normally: a final assistant message exists.
	settled := false
	for _, r := range records {
		if r.Type == storage.TypeAssistantMessage {
			settled = true
		}
	}
	if !settled {
		t.Fatal("the run did not settle with an assistant message")
	}
}

// TestNewRejectsDuplicateToolNames: two tools with the same
// Definition().Name are a construction error — the dispatch set is
// name-keyed, a silent overwrite would strand one tool.
func TestNewRejectsDuplicateToolNames(t *testing.T) {
	var calls []string
	a := recordingTool{name: "dupe", calls: &calls}
	b := recordingTool{name: "dupe", calls: &calls}
	_, err := New(Options{
		ModelResolver: NewStaticResolver(&fakeProvider{}, nil, 0, 0, ""),
		Storage:       memory.OpenStore(),
		Tools:         []Tool{a, b},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate tool name") {
		t.Fatalf("duplicate names: err = %v, want the duplicate-tool error", err)
	}
}
