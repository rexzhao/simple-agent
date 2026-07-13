package execution

import (
	"context"
	"fmt"
	"testing"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/eventbus"
	eventlog "github.com/rexzhao/simple-agent/internal/logging"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/tools"
)

// recordingProvider is a minimal model.Provider that records each request and
// returns canned event batches per turn.
type recordingProvider struct {
	turns    [][]model.Event
	requests []model.Request
}

func (p *recordingProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if len(p.requests) >= len(p.turns) {
		return nil, fmt.Errorf("unexpected model request %d", len(p.requests)+1)
	}
	p.requests = append(p.requests, request)
	turn := len(p.requests) - 1
	events := make(chan model.Event, len(p.turns[turn]))
	for _, event := range p.turns[turn] {
		events <- event
	}
	close(events)
	return events, nil
}

// recordingPublisher is a minimal eventbus.Publisher that records published
// events in order.
type recordingPublisher struct {
	events []eventbus.Event
}

func (p *recordingPublisher) Publish(event eventbus.Event) error {
	p.events = append(p.events, event)
	return nil
}

// TestAgentRunnerRunSessionTurnAdaptsActivePromptDrain proves that a
// SessionActivePromptDrain is adapted by AgentTurnRunner into the agent-loop
// active prompt drain: drained user messages reach the provider at the correct
// checkpoint, and the SessionActivePromptCheckpoint values map to the three
// agent-loop checkpoints in order.
func TestAgentRunnerRunSessionTurnAdaptsActivePromptDrain(t *testing.T) {
	provider := &recordingProvider{
		turns: [][]model.Event{
			{
				model.ToolCallDoneEvent{
					ToolCall: model.ToolCall{ID: "call_1", Name: "echo", Arguments: `{}`},
				},
			},
			{model.TextDeltaEvent{Text: "done"}},
		},
	}
	registry := tools.NewRegistry()
	if err := registry.Register(model.Tool{Name: "echo"}, tools.ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		return model.ToolResult{Name: "echo", Content: "echoed"}, nil
	})); err != nil {
		t.Fatalf("register echo tool: %v", err)
	}
	logger, err := eventlog.Open("", eventlog.Attributes{})
	if err != nil {
		t.Fatalf("open no-op logger: %v", err)
	}
	publisher := &recordingPublisher{}
	runtime := &agentRunnerRuntime{
		provider:     provider,
		modelID:      "model-test",
		maxTurns:     4,
		logger:       logger,
		toolSchemas:  []model.Tool{{Name: "echo"}},
		toolExecutor: runToolExecutor{builtins: registry},
		session:      sessions.SessionV2{},
	}

	var gotCheckpoints []SessionActivePromptCheckpoint
	drain := SessionActivePromptDrain(func(cp SessionActivePromptCheckpoint) []model.Message {
		gotCheckpoints = append(gotCheckpoints, cp)
		if cp == SessionActivePromptCheckpointAfterToolBatch {
			return []model.Message{{Role: model.MessageRoleUser, Content: "drained"}}
		}
		return nil
	})

	messages, err := runtime.runSessionTurn(context.Background(), "hello", sessionTurnRunOptions{
		publisher:         publisher,
		turnID:            "turn-1",
		activePromptDrain: drain,
	})
	if err != nil {
		t.Fatalf("runSessionTurn() error = %v", err)
	}

	// The provider was called twice (tool-call turn, then final turn).
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}

	// Drained user message reached the provider: the second request carries it
	// as a separate user item after the tool result, proving the callback's
	// messages flow into the request history.
	second := provider.requests[1].Messages
	if len(second) != 4 {
		t.Fatalf("second request messages = %d, want 4: %#v", len(second), second)
	}
	if second[0].Role != model.MessageRoleUser || second[0].Content != "hello" {
		t.Fatalf("second[0] = %#v, want user hello", second[0])
	}
	if second[1].Role != model.MessageRoleAssistant || len(second[1].ToolCalls) != 1 || second[1].ToolCalls[0].ID != "call_1" {
		t.Fatalf("second[1] = %#v, want assistant with call_1", second[1])
	}
	if second[2].Role != model.MessageRoleTool || second[2].Content != "echoed" || second[2].ToolCallID != "call_1" {
		t.Fatalf("second[2] = %#v, want tool result echoed for call_1", second[2])
	}
	if second[3].Role != model.MessageRoleUser || second[3].Content != "drained" {
		t.Fatalf("second[3] = %#v, want user drained (drained message reached provider)", second[3])
	}

	// The final assistant response is part of the returned messages.
	if len(messages) != 5 || messages[4].Role != model.MessageRoleAssistant || messages[4].Content != "done" {
		t.Fatalf("returned messages = %#v, want final assistant done", messages)
	}

	// Checkpoint values map correctly: BeforeProvider (turn 1) ->
	// AfterToolBatch (turn 1) -> BeforeTerminal (turn 2).
	wantCheckpoints := []SessionActivePromptCheckpoint{
		SessionActivePromptCheckpointBeforeProvider,
		SessionActivePromptCheckpointAfterToolBatch,
		SessionActivePromptCheckpointBeforeTerminal,
	}
	if len(gotCheckpoints) != len(wantCheckpoints) {
		t.Fatalf("checkpoints = %#v, want %#v", gotCheckpoints, wantCheckpoints)
	}
	for i, cp := range gotCheckpoints {
		if cp != wantCheckpoints[i] {
			t.Fatalf("checkpoints = %#v, want %#v", gotCheckpoints, wantCheckpoints)
		}
	}
}

// TestAgentRunnerNilActivePromptDrainIsNoOp proves that a nil
// SessionActivePromptDrain adapts to a nil agent-loop drain: the turn proceeds
// normally with no TurnInputReady published and no drained messages.
func TestAgentRunnerNilActivePromptDrainIsNoOp(t *testing.T) {
	provider := &recordingProvider{
		turns: [][]model.Event{
			{model.TextDeltaEvent{Text: "final"}},
		},
	}
	logger, err := eventlog.Open("", eventlog.Attributes{})
	if err != nil {
		t.Fatalf("open no-op logger: %v", err)
	}
	publisher := &recordingPublisher{}
	runtime := &agentRunnerRuntime{
		provider: provider,
		modelID:  "model-test",
		maxTurns: 4,
		logger:   logger,
		session:  sessions.SessionV2{},
	}

	messages, err := runtime.runSessionTurn(context.Background(), "hello", sessionTurnRunOptions{
		publisher: publisher,
		turnID:    "turn-1",
		// activePromptDrain intentionally nil.
	})
	if err != nil {
		t.Fatalf("runSessionTurn() error = %v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.requests))
	}
	if len(messages) != 2 || messages[1].Role != model.MessageRoleAssistant || messages[1].Content != "final" {
		t.Fatalf("returned messages = %#v, want user hello + assistant final", messages)
	}
	for _, event := range publisher.events {
		if event.Kind() == eventbus.KindTurnInputReady {
			t.Fatalf("unexpected TurnInputReady event with nil drain: %#v", event)
		}
	}
}

// TestToSessionActivePromptCheckpointMapsExhaustively proves the explicit
// switch maps each agent checkpoint to the matching execution checkpoint, and
// that an unrecognized agent checkpoint fails loudly (panic) rather than
// silently mis-mapping.
func TestToSessionActivePromptCheckpointMapsExhaustively(t *testing.T) {
	tests := []struct {
		name      string
		input     agent.ActivePromptCheckpoint
		want      SessionActivePromptCheckpoint
		wantPanic bool
	}{
		{
			name:  "before_provider",
			input: agent.ActivePromptCheckpointBeforeProvider,
			want:  SessionActivePromptCheckpointBeforeProvider,
		},
		{
			name:  "after_tool_batch",
			input: agent.ActivePromptCheckpointAfterToolBatch,
			want:  SessionActivePromptCheckpointAfterToolBatch,
		},
		{
			name:  "before_terminal",
			input: agent.ActivePromptCheckpointBeforeTerminal,
			want:  SessionActivePromptCheckpointBeforeTerminal,
		},
		{
			name:      "unexpected value panics",
			input:     agent.ActivePromptCheckpoint(99),
			wantPanic: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if tt.wantPanic {
					if r := recover(); r == nil {
						t.Fatalf("toSessionActivePromptCheckpoint(%d) did not panic, want panic", tt.input)
					}
					return
				}
				if r := recover(); r != nil {
					t.Fatalf("toSessionActivePromptCheckpoint(%d) panicked unexpectedly: %v", tt.input, r)
				}
			}()
			got := toSessionActivePromptCheckpoint(tt.input)
			if tt.wantPanic {
				t.Fatalf("toSessionActivePromptCheckpoint(%d) = %d, want panic", tt.input, got)
			}
			if got != tt.want {
				t.Fatalf("toSessionActivePromptCheckpoint(%d) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// TestAdaptActivePromptDrainNilIsNoOp proves a nil execution drain adapts to a
// nil agent-loop drain.
func TestAdaptActivePromptDrainNilIsNoOp(t *testing.T) {
	if got := adaptActivePromptDrain(nil); got != nil {
		t.Fatalf("adaptActivePromptDrain(nil) = %v, want nil", got)
	}
}
