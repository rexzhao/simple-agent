package sessionlog

import (
	"fmt"
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
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	TurnID string `json:"turn_id,omitempty"`
	RunID string `json:"run_id,omitempty"`

	// Content is the user text, assistant text, or tool output.
	Content string `json:"content,omitempty"`
	// Reasoning is assistant reasoning text.
	Reasoning string `json:"reasoning,omitempty"`
	// Streaming marks an assistant item still being checkpointed; the
	// assistant.ready record finalizes it.
	Streaming bool `json:"streaming,omitempty"`

	// ToolCalls are the calls of a finalized assistant item.
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

	// Items is the display order: user inputs, assistant messages (final or
	// streaming), and tool results interleaved exactly as they were appended.
	Items []Item

	// Context is the model-facing history: the user, assistant and tool
	// messages the next request needs, in request order. Streaming
	// checkpoints never enter the context.
	Context []model.Message

	// Runs maps run ID to settle status; a run which started but has not
	// settled reports RunStatusRunning.
	Runs map[string]string
	// ActiveRun is the run of the last user input.
	ActiveRun string

	LastSeq int64
}

func newState(sessionID string) *State {
	return &State{SessionID: sessionID, Runs: make(map[string]string)}
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
	case TypeTurnInput:
		state.Items = append(state.Items, Item{
			ID: fmt.Sprintf("%s-%d", record.TurnID, record.Seq), Kind: ItemKindUser, TurnID: record.TurnID,
			Content: record.Content, Seq: record.Seq,
		})
		state.Context = append(state.Context, model.Message{Role: model.MessageRoleUser, Content: record.Content})
	case TypeRunStarted:
		state.Runs[record.RunID] = RunStatusRunning
		state.ActiveRun = record.RunID
	case TypeRunSettled:
		state.Runs[record.RunID] = record.Status
		if state.ActiveRun == record.RunID {
			state.ActiveRun = ""
		}
		// A settled run cannot stream any more: finalize its display-only
		// checkpoint items so a resumed session does not show a live caret
		// over output that will never grow. Finalized content still never
		// enters the model context.
		for index := range state.Items {
			if state.Items[index].RunID == record.RunID && state.Items[index].Streaming {
				state.Items[index].Streaming = false
			}
		}
		if record.Error != "" {
			state.Items = append(state.Items, Item{
				ID: record.RunID + "-error", Kind: ItemKindError, RunID: record.RunID,
				Content: record.Error, Seq: record.Seq,
			})
		}
	case TypeTurnStarted, TypeTurnCompleted, TypeTurnInterrupted:
		// Turn boundaries are durable facts but carry no display or context
		// payload in the minimal fold.
	case TypeAssistantCheckpoint:
		// Cumulative content: the checkpoint replaces whatever the item
		// showed so far, so a delayed writer can never reorder a prefix.
		if index := findItem(state.Items, record.ItemID); index >= 0 {
			// A checkpoint for an already-finalized item is out of order
			// (only reachable from a hand-crafted log); never regress a
			// terminal record back to a streaming state.
			if !state.Items[index].Streaming {
				break
			}
			item := &state.Items[index]
			item.Content = record.Content
			item.Streaming = true
			item.Seq = record.Seq
			break
		}
		state.Items = append(state.Items, Item{
			ID: record.ItemID, Kind: ItemKindAssistant, TurnID: record.TurnID, RunID: record.RunID,
			Content: record.Content, Streaming: true, Seq: record.Seq,
		})
	case TypeAssistantReady:
		calls := modelToolCalls(record.ToolCalls)
		if index := findItem(state.Items, record.ItemID); index >= 0 {
			item := &state.Items[index]
			item.Content = record.Content
			item.Reasoning = record.Reasoning
			item.ToolCalls = calls
			item.Streaming = false
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
