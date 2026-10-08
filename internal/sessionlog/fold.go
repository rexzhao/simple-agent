package sessionlog

import (
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// State is the fold of a session log. It is rebuilt on open and advanced
// incrementally under the session lock as records are appended.
//
// The fold serves exactly one production consumer: rebuilding the model
// request context. Display layers (the CLI prints raw records) derive their
// view from the records themselves.
type State struct {
	SessionID string
	Provider  string
	Model     string
	CWD       string
	Title     string

	CreatedAt time.Time
	UpdatedAt time.Time

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
		state.Context = append(state.Context, model.Message{Role: model.MessageRoleUser, Content: record.Content})
		if state.Title == "" {
			state.Title = summarizeTitle(record.Content)
		}
	case TypeAssistantMessage, TypeAssistantReady:
		calls := modelToolCalls(record.ToolCalls)
		state.Context = append(state.Context, model.Message{
			Role:             model.MessageRoleAssistant,
			Content:          record.Content,
			ReasoningContent: record.Reasoning,
			ToolCalls:        calls,
		})
	case TypeToolResult:
		state.Context = append(state.Context, model.Message{
			Role:       model.MessageRoleTool,
			Content:    record.Content,
			ToolCallID: record.ToolCallID,
			IsError:    record.IsError,
		})
	case TypeRunFailed:
		// A failed run leaves its error out of the context: committed is the
		// absence of failure, and an interruption simply leaves the user
		// input unanswered.
	case TypeCompactionCreated:
		// Reserved for the compaction fold; the minimal fold keeps the full
		// history visible.
	default:
		// Unknown record type: ignore for forward compatibility.
	}
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
