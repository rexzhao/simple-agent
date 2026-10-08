package sessionlog

import (
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// Item kinds.
const (
	ItemKindUser      = "user"
	ItemKindAssistant = "assistant"
	ItemKindTool      = "tool"
	ItemKindError     = "error"
)

// Tool result display statuses.
const (
	ToolStatusCompleted = "completed"
	ToolStatusError     = "error"
)

// Item is one display row in the folded session state.
type Item struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	TurnID string `json:"turn_id,omitempty"`
	RunID  string `json:"run_id,omitempty"`

	// Content is the user text, assistant text, or tool output.
	Content string `json:"content,omitempty"`
	// Reasoning is assistant reasoning text.
	Reasoning string `json:"reasoning,omitempty"`

	// ToolCalls are the calls of an assistant message.
	ToolCalls []model.ToolCall `json:"tool_calls,omitempty"`

	// Tool result fields.
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	Status     string `json:"status,omitempty"`

	// Seq is the sequence of the record which last touched the item.
	Seq int64 `json:"seq"`
}

// State is the fold of a session log. It is rebuilt on open and advanced
// incrementally under the session lock as records are appended.
type State struct {
	SessionID string
	Provider  string
	Model     string
	CWD       string
	Title     string

	CreatedAt time.Time
	UpdatedAt time.Time

	// Items is the display order: user inputs, assistant messages, and tool
	// results interleaved exactly as they were appended.
	Items []Item

	// Context is the model-facing history: the user, assistant and tool
	// messages the next request needs, in request order. A run which was
	// interrupted mid-tool can leave assistant tool calls without results;
	// the consumer (chatapp) patches those before sending.
	Context []model.Message

	LastSeq int64
}

func newState(sessionID string) *State {
	return &State{SessionID: sessionID}
}

// fold advances the state with one record. It is a total function: it never
// fails, and unknown record types are ignored so a newer writer cannot brick
// an older reader.
func fold(state *State, record Record) {
	state.LastSeq = record.Seq
	if record.Time != "" {
		state.UpdatedAt = parseTime(record.Time)
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
		state.Items = append(state.Items, Item{
			ID: record.RunID, Kind: ItemKindUser, RunID: record.RunID,
			Content: record.Content, Seq: record.Seq,
		})
		state.Context = append(state.Context, model.Message{Role: model.MessageRoleUser, Content: record.Content})
		if state.Title == "" {
			state.Title = summarizeTitle(record.Content)
		}
	case TypeAssistantMessage, TypeAssistantReady:
		calls := modelToolCalls(record.ToolCalls)
		if index := findItem(state.Items, record.ItemID); index >= 0 {
			item := &state.Items[index]
			item.Content = record.Content
			item.Reasoning = record.Reasoning
			item.ToolCalls = calls
			item.Seq = record.Seq
		} else {
			state.Items = append(state.Items, Item{
				ID: record.ItemID, Kind: ItemKindAssistant, TurnID: record.TurnID, RunID: record.RunID,
				Content: record.Content, Reasoning: record.Reasoning, ToolCalls: calls, Seq: record.Seq,
			})
		}
		state.Context = append(state.Context, model.Message{
			Role:             model.MessageRoleAssistant,
			Content:          record.Content,
			ReasoningContent: record.Reasoning,
			ToolCalls:        calls,
		})
	case TypeToolResult:
		state.Items = append(state.Items, Item{
			ID: record.ToolCallID, Kind: ItemKindTool, TurnID: record.TurnID, RunID: record.RunID,
			Content: record.Content, ToolCallID: record.ToolCallID, ToolName: record.ToolName,
			IsError: record.IsError, Seq: record.Seq,
		})
		if record.IsError {
			state.Items[len(state.Items)-1].Status = ToolStatusError
		} else {
			state.Items[len(state.Items)-1].Status = ToolStatusCompleted
		}
		state.Context = append(state.Context, model.Message{
			Role:       model.MessageRoleTool,
			Content:    record.Content,
			ToolCallID: record.ToolCallID,
			IsError:    record.IsError,
		})
	case TypeRunFailed:
		// A failed run leaves its error visible in the conversation; a
		// committed or interrupted run needs no record: committed is the
		// absence of failure, and an interruption simply leaves the user
		// input unanswered.
		state.Items = append(state.Items, Item{
			ID: record.RunID + "-error", Kind: ItemKindError, RunID: record.RunID,
			Content: record.Error, Seq: record.Seq,
		})
	case TypeCompactionCreated:
		// Reserved for the compaction fold; the minimal fold keeps the full
		// history visible.
	default:
		// Unknown record type: ignore for forward compatibility.
	}
}

func findItem(items []Item, id string) int {
	if id == "" {
		return -1
	}
	for index := range items {
		if items[index].ID == id {
			return index
		}
	}
	return -1
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

// summarizeTitle derives the session title from the first user input.
func summarizeTitle(content string) string {
	title := strings.TrimSpace(content)
	title = strings.ReplaceAll(title, "\n", " ")
	if len([]rune(title)) > 60 {
		title = string([]rune(title)[:60]) + "…"
	}
	return title
}
