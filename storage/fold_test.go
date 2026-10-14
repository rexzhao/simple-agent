package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/model"
)

func foldRecords(t *testing.T, records []Record) State {
	t.Helper()
	state := NewState("s-test")
	for _, record := range records {
		Fold(state, record)
	}
	return *state
}

func TestFoldRebuildsConversationContext(t *testing.T) {
	state := foldRecords(t, []Record{
		{ItemID: "h1", Type: TypeSessionCreated, Time: "2026-01-02T03:04:05Z", Provider: "p", Model: "m", CWD: "/w"},
		{ItemID: "u1", Type: TypeUserInput, RunID: "run-1", Content: "hello", Time: "2026-01-02T03:04:06Z"},
		{ItemID: "a1", Type: TypeAssistantMessage, RunID: "run-1", Content: "hi",
			ToolCalls: []WireToolCall{{ID: "call-1", Name: "shell", Arguments: "{}"}}, Time: "2026-01-02T03:04:07Z"},
		{ItemID: "t1", Type: TypeToolResult, RunID: "run-1", ToolCallID: "call-1", ToolName: "shell", Content: "ok", Time: "2026-01-02T03:04:08Z"},
		{ItemID: "a2", Type: TypeAssistantMessage, RunID: "run-1", Content: "done", Time: "2026-01-02T03:04:09Z"},
	})
	if state.Title != "hello" {
		t.Fatalf("Title = %q, want first user input", state.Title)
	}
	if state.Provider != "p" || state.Model != "m" || state.CWD != "/w" {
		t.Fatalf("session metadata not folded: %+v", state)
	}
	if len(state.Context) != 4 {
		t.Fatalf("Context length = %d, want 4", len(state.Context))
	}
	roles := []model.MessageRole{state.Context[0].Role, state.Context[1].Role, state.Context[2].Role, state.Context[3].Role}
	want := []model.MessageRole{model.MessageRoleUser, model.MessageRoleAssistant, model.MessageRoleTool, model.MessageRoleAssistant}
	for i := range roles {
		if roles[i] != want[i] {
			t.Fatalf("Context[%d].Role = %s, want %s", i, roles[i], want[i])
		}
	}
	if len(state.Context[1].ToolCalls) != 1 || state.Context[1].ToolCalls[0].ID != "call-1" {
		t.Fatalf("assistant tool calls not folded: %+v", state.Context[1].ToolCalls)
	}
	if state.Context[2].ToolCallID != "call-1" {
		t.Fatalf("tool result call id not folded: %q", state.Context[2].ToolCallID)
	}
	if state.UpdatedAt != time.Date(2026, 1, 2, 3, 4, 9, 0, time.UTC) {
		t.Fatalf("UpdatedAt = %v, want last record time", state.UpdatedAt)
	}
}

func TestFoldRunFailedStaysOutOfContext(t *testing.T) {
	state := foldRecords(t, []Record{
		{ItemID: "u1", Type: TypeUserInput, Content: "hello"},
		{ItemID: "f1", Type: TypeRunFailed, Error: "boom"},
	})
	if len(state.Context) != 1 {
		t.Fatalf("Context length = %d, want the user input only", len(state.Context))
	}
}

func TestFoldIgnoresUnknownTypesAndBadTimes(t *testing.T) {
	state := foldRecords(t, []Record{
		{ItemID: "x", Type: "future.record.kind"},
		{ItemID: "u1", Type: TypeUserInput, Content: "hi", Time: "not-a-time"},
	})
	if len(state.Context) != 1 {
		t.Fatalf("Context length = %d, want 1", len(state.Context))
	}
	if !state.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt = %v, want zero for unparseable time", state.UpdatedAt)
	}
}

func TestCopyStateDeepCopiesTransientRegion(t *testing.T) {
	state := NewState("s")
	state.Context = []model.Message{{ToolCalls: []model.ToolCall{{ID: "c"}}}}
	state.Streaming = &StreamingItem{ItemID: "a1", Message: model.Message{ToolCalls: []model.ToolCall{{ID: "c"}}}}
	state.Running = &RunningTool{ItemID: "t1"}

	copied := CopyState(state)
	copied.Context[0].ToolCalls[0].ID = "changed"
	copied.Streaming.Message.ToolCalls[0].ID = "changed"
	copied.Streaming.ItemID = "changed"
	copied.Running.ItemID = "changed"

	if state.Context[0].ToolCalls[0].ID != "c" || state.Streaming.Message.ToolCalls[0].ID != "c" ||
		state.Streaming.ItemID != "a1" || state.Running.ItemID != "t1" {
		t.Fatalf("CopyState shared memory with the original: %+v", state)
	}
}

func TestValidateRequiresTypeAndItemID(t *testing.T) {
	if err := (Record{ItemID: "i", Type: TypeUserInput}).Validate(); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	if err := (Record{Type: TypeUserInput}).Validate(); err == nil || !strings.Contains(err.Error(), "item id") {
		t.Fatalf("missing item id accepted: %v", err)
	}
	if err := (Record{ItemID: "i"}).Validate(); err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("missing type accepted: %v", err)
	}
}

func TestItemIDsAreUniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		id := NewItemID("a")
		if !strings.HasPrefix(id, "a") || len(id) != len("a")+26 {
			t.Fatalf("item id %q has unexpected shape", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != 100 {
		t.Fatalf("item id collisions: %d unique of 100", len(seen))
	}
}

func TestFoldContextCompactedReplacesContext(t *testing.T) {
	state := foldRecords(t, []Record{
		{ItemID: "u1", Type: TypeUserInput, Content: "q1"},
		{ItemID: "a1", Type: TypeAssistantMessage, Content: "a1"},
		{ItemID: "u2", Type: TypeUserInput, Content: "q2"},
		{ItemID: "x1", Type: TypeContextCompacted, Content: "summary of the work"},
		{ItemID: "u3", Type: TypeUserInput, Content: "continue"},
	})
	if len(state.Context) != 2 {
		t.Fatalf("Context length = %d, want summary + later input", len(state.Context))
	}
	if state.Context[0].Role != model.MessageRoleUser || state.Context[0].Content != "summary of the work" {
		t.Fatalf("summary message = %+v", state.Context[0])
	}
	if state.Context[1].Content != "continue" {
		t.Fatalf("post-compaction input lost: %+v", state.Context[1])
	}
}
