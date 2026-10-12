package storage

import (
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// State is the session's live state. It has two regions with different
// clocks:
//
//   - the durable region is advanced only by Fold over committed records;
//     it equals Fold of every record the storage accepted;
//   - the transient region is advanced only by streaming and execution
//     events; it is never persisted and is lost on restart.
//
// The invariant: State minus its transient region equals Fold of all
// storage records.
type State struct {
	// Durable region.
	SessionID string
	Provider  string
	Model     string
	CWD       string
	Title     string

	CreatedAt time.Time
	UpdatedAt time.Time

	// Context is the model-facing history: the user, assistant and tool
	// messages the next request needs, in request order. It is the wire
	// world: tool call ids pair exactly as the providers expect.
	Context []model.Message

	// Transient region.
	Streaming  *StreamingItem
	Running    *RunningTool
	Compacting *CompactingItem
	// Steered holds user inputs queued for mid-run injection (and, after
	// the run ends, delivered by the next one). Entries are display-and-
	// address-only until consumed: the drain commits a user.input record
	// carrying the SAME ItemID, so observers merge by identity.
	Steered []SteeredItem
}

// SteeredItem is one queued steer message. The ItemID is minted at
// enqueue time — identity precedes persistence.
type SteeredItem struct {
	ItemID string
	Text   string
}

// StreamingItem is the assistant message currently being generated. It is
// display-only: never folded, never persisted, never part of Context.
type StreamingItem struct {
	ItemID  string
	Message model.Message
}

// RunningTool is the tool execution currently in flight. ItemID is the
// identity of the tool.result which will be committed when it finishes, so
// a reconnecting observer can hold the slot and upsert seamlessly.
type RunningTool struct {
	ItemID    string
	ToolCall  model.ToolCall
	StartedAt time.Time
}

// CompactingItem is the context compaction currently in flight. ItemID is
// the identity of the context.compacted record which will land on success;
// a failed or interrupted compaction leaves nothing behind.
type CompactingItem struct {
	ItemID    string
	StartedAt time.Time
}

// NewState returns the empty state of a session.
func NewState(sessionID string) *State {
	return &State{SessionID: sessionID}
}

// Fold advances the durable region with one record. It is a total function:
// it never fails, and unknown record types are ignored so a newer writer
// cannot brick an older reader.
func Fold(state *State, record Record) {
	if record.Time != "" {
		if parsed := parseTime(record.Time); !parsed.IsZero() {
			state.UpdatedAt = parsed
		}
	}
	switch record.Type {
	case TypeSessionCreated:
		state.Provider = record.Provider
		state.Model = record.Model
		state.CWD = record.CWD
		if t, err := time.Parse(time.RFC3339Nano, record.Time); err == nil {
			state.CreatedAt = t
		}
	case TypeUserInput:
		state.Context = append(state.Context, model.Message{Role: model.MessageRoleUser, Content: record.Content})
		if state.Title == "" {
			state.Title = SummarizeTitle(record.Content)
		}
	case TypeAssistantMessage:
		state.Context = append(state.Context, model.Message{
			Role:             model.MessageRoleAssistant,
			Content:          record.Content,
			ReasoningContent: record.Reasoning,
			ToolCalls:        ModelToolCalls(record.ToolCalls),
		})
	case TypeToolResult:
		state.Context = append(state.Context, model.Message{
			Role:       model.MessageRoleTool,
			Content:    record.Content,
			ToolCallID: record.ToolCallID,
			IsError:    record.IsError,
		})
	case TypeRunFailed:
		// A failed run leaves its error out of the context: committed is
		// the absence of failure, and an interruption simply leaves the
		// user input unanswered.
	case TypeContextCompacted:
		// The compacted history is replaced by the summary: subsequent
		// requests see exactly one user message carrying it. Everything
		// before the compaction point — including any dangling tool
		// exchanges — is gone.
		state.Context = []model.Message{{Role: model.MessageRoleUser, Content: record.Content}}
	default:
		// Unknown record type: ignore for forward compatibility.
	}
}

// CopyState returns a deep copy, safe for any reader.
func CopyState(state *State) State {
	copied := *state
	copied.Context = append([]model.Message(nil), state.Context...)
	for i := range copied.Context {
		copied.Context[i].ToolCalls = append([]model.ToolCall(nil), state.Context[i].ToolCalls...)
	}
	if state.Streaming != nil {
		item := *state.Streaming
		item.Message.ToolCalls = append([]model.ToolCall(nil), state.Streaming.Message.ToolCalls...)
		copied.Streaming = &item
	}
	if state.Running != nil {
		running := *state.Running
		copied.Running = &running
	}
	if state.Compacting != nil {
		compacting := *state.Compacting
		copied.Compacting = &compacting
	}
	copied.Steered = append([]SteeredItem(nil), state.Steered...)
	return copied
}

// parseTime is a forgiving RFC3339 parser: an unparseable timestamp yields
// the zero time rather than an error, because timestamps are diagnostic and
// must never block a fold.
func parseTime(value string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t
	}
	return time.Time{}
}

// SummarizeTitle derives the session title from the first user input.
func SummarizeTitle(content string) string {
	title := strings.TrimSpace(content)
	title = strings.ReplaceAll(title, "\n", " ")
	if len([]rune(title)) > 60 {
		title = string([]rune(title)[:60]) + "…"
	}
	return title
}
