package turnview

import (
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/execution"
)

func TestStateApplyAggregatesBlocksAndStatus(t *testing.T) {
	state := New()
	state.SetSession("session-1", "paperhub", "glm", "glm-5.2")
	state.AddInput("user", "hello")

	for _, event := range []execution.SessionStreamEvent{
		execution.NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-1"}),
		execution.NewSessionStreamEvent("reasoning.delta", map[string]any{"turn_id": "turn-1", "text": "think"}),
		execution.NewSessionStreamEvent("reasoning.delta", map[string]any{"turn_id": "turn-1", "text": " more"}),
		execution.NewSessionStreamEvent("text.delta", map[string]any{"turn_id": "turn-1", "text": "final"}),
		execution.NewSessionStreamEvent("tool.started", map[string]any{"turn_id": "turn-1", "tool_call_id": "call-1", "name": "read_file"}),
		execution.NewSessionStreamEvent("tool.finished", map[string]any{"turn_id": "turn-1", "tool_call_id": "call-1", "name": "read_file"}),
		execution.NewSessionStreamEvent("usage.updated", map[string]any{"turn_id": "turn-1", "input_tokens": 11, "output_tokens": 7, "total_tokens": 18}),
		execution.NewSessionStreamEvent("turn.committed", map[string]any{"turn_id": "turn-1"}),
	} {
		state.Apply(event)
	}

	if state.Status.SessionID != "session-1" || state.Status.TurnID != "turn-1" || state.Status.TurnStatus != "completed" {
		t.Fatalf("status = %#v, want session and completed turn", state.Status)
	}
	if state.Status.TotalTokens != 18 || state.Status.ToolCount != 1 {
		t.Fatalf("status usage/tools = %#v, want usage and one tool", state.Status)
	}
	assertBlock(t, state, BlockInput, "hello", "queued")
	assertBlock(t, state, BlockReasoning, "think more", "running")
	assertBlock(t, state, BlockAssistant, "final", "running")
	assertBlock(t, state, BlockTool, "", "completed")
}

func TestStateDoesNotLeakToolResultBody(t *testing.T) {
	state := New()
	state.Apply(execution.NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-1"}))
	state.Apply(execution.NewSessionStreamEvent("tool.started", map[string]any{
		"turn_id":      "turn-1",
		"tool_call_id": "call-1",
		"name":         "shell",
	}))
	state.Apply(execution.NewSessionStreamEvent("tool.finished", map[string]any{
		"turn_id":      "turn-1",
		"tool_call_id": "call-1",
		"name":         "shell",
		"is_error":     true,
		"result":       "secret output",
		"preview":      "secret preview",
	}))

	for _, block := range state.Blocks {
		if strings.Contains(block.Text, "secret") {
			t.Fatalf("block leaked tool result body: %#v", block)
		}
	}
	assertBlock(t, state, BlockTool, "", "failed")
}

func TestStateMailboxAndFailureBlocks(t *testing.T) {
	state := New()
	state.SetMailbox("http://127.0.0.1:3984/mcp")
	state.AddMailboxStart("task_000001")
	state.Apply(execution.NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-1"}))
	state.Apply(execution.NewSessionStreamEvent("turn.failed", map[string]any{"turn_id": "turn-1", "message": "turn failed"}))
	state.AddMailboxEnd("task_000001", "cancelled")

	if state.Status.Mailbox == "" || state.Status.TurnStatus != "failed" {
		t.Fatalf("status = %#v, want mailbox and failed turn", state.Status)
	}
	assertBlock(t, state, BlockMailbox, "", "running")
	assertBlock(t, state, BlockError, "turn failed", "failed")
	assertBlock(t, state, BlockMailbox, "", "cancelled")
}

func assertBlock(t *testing.T, state *State, kind BlockKind, text, status string) {
	t.Helper()
	for _, block := range state.Blocks {
		if block.Kind != kind {
			continue
		}
		if block.Text == text && block.Status == status {
			return
		}
	}
	t.Fatalf("missing block kind=%s text=%q status=%q in %#v", kind, text, status, state.Blocks)
}
