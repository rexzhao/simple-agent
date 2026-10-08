package sessionlog

import (
	"fmt"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// Record types. The set is closed: fold ignores unknown types so a newer
// writer never corrupts an older reader.
//
// The vocabulary is a conversation log: session metadata, user inputs,
// assistant messages, tool results, and failures. Run lifecycle needs no
// records of its own — a user input starts its run, the absence of a failure
// means it completed, and an interruption simply leaves the input
// unanswered.
const (
	TypeSessionCreated   = "session.created"
	TypeUserInput        = "user.input"
	TypeAssistantMessage = "assistant.message"
	// TypeAssistantReady is the pre-rename spelling of TypeAssistantMessage,
	// accepted on read so logs written before the rename still fold.
	TypeAssistantReady    = "assistant.ready"
	TypeToolResult        = "tool.result"
	TypeRunFailed         = "run.failed"
	TypeCompactionCreated = "compaction.created"
)

// maxRecordBytes bounds a single JSONL line. Tool output larger than this is
// truncated by the tool layer before it reaches the log; the bound exists so a
// corrupt file cannot make the reader allocate unbounded memory.
const maxRecordBytes = 8 * 1024 * 1024

// Record is one append-only log line. Fields are shared across record types
// and omitted when empty so lines stay short and greppable.
type Record struct {
	Seq  int64  `json:"seq"`
	Type string `json:"t"`
	Time string `json:"ts"`

	// Identity scopes. RunID groups one user input with its assistant
	// messages and tool results; ItemID is the stable identity of one
	// assistant message.
	RunID     string `json:"run,omitempty"`
	TurnID    string `json:"turn,omitempty"`
	ItemID    string `json:"item,omitempty"`
	Iteration int    `json:"iter,omitempty"`

	// Session metadata (session.created).
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	CWD      string `json:"cwd,omitempty"`

	// Message payloads.
	Role      string         `json:"role,omitempty"`
	Content   string         `json:"content,omitempty"`
	Reasoning string         `json:"reasoning,omitempty"`
	ToolCalls []WireToolCall `json:"tool_calls,omitempty"`

	// Tool result payload.
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`

	// Failure payload (run.failed).
	Error string `json:"error,omitempty"`
}

// WireToolCall is the JSON form of model.ToolCall. The provider ID is not
// durable state: it is reconstructed by the provider adapter on request.
type WireToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func wireToolCalls(calls []model.ToolCall) []WireToolCall {
	if len(calls) == 0 {
		return nil
	}
	result := make([]WireToolCall, 0, len(calls))
	for _, call := range calls {
		result = append(result, WireToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	return result
}

func modelToolCalls(calls []WireToolCall) []model.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	result := make([]model.ToolCall, 0, len(calls))
	for _, call := range calls {
		result = append(result, model.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	return result
}

// durable reports whether the record must survive a power failure. Failures
// are durable so the reason a run died survives a crash; everything else in
// the vocabulary is already terminal content.
func (r Record) durable() bool {
	switch r.Type {
	case TypeSessionCreated, TypeUserInput, TypeAssistantMessage,
		TypeAssistantReady, TypeToolResult, TypeRunFailed, TypeCompactionCreated:
		return true
	default:
		return false
	}
}

func newTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Validate checks the invariant parts of a decoded record. It is deliberately
// permissive about payload fields: fold decides what each type means.
func (r Record) Validate() error {
	if r.Seq < 1 {
		return fmt.Errorf("record seq must be positive")
	}
	if r.Type == "" {
		return fmt.Errorf("record type is required")
	}
	if r.Time != "" {
		if _, err := time.Parse(time.RFC3339Nano, r.Time); err != nil {
			return fmt.Errorf("record time is not RFC3339: %w", err)
		}
	}
	return nil
}
