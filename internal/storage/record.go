package storage

import (
	"fmt"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// Record types. The set is closed: Fold ignores unknown types so a newer
// writer never corrupts an older reader.
//
// The vocabulary is a conversation log: session metadata, user inputs,
// assistant messages, tool results, and failures. Every record is an item
// with an ItemID minted before persistence; run-scoped records carry the
// RunID of their run, so stateless consumers (grep) can query whole runs
// without tracking position.
const (
	TypeSessionCreated   = "session.created"
	TypeUserInput        = "user.input"
	TypeAssistantMessage = "assistant.message"
	TypeToolResult       = "tool.result"
	TypeRunFailed        = "run.failed"
	TypeContextCompacted = "context.compacted"
)

// MaxRecordBytes bounds a single JSONL line. Tool output larger than this is
// truncated by the tool layer before it reaches the log; the bound exists so
// a corrupt file cannot make the reader allocate unbounded memory.
const MaxRecordBytes = 8 * 1024 * 1024

// Record is one append-only log line and one item.
type Record struct {
	// ItemID is the record's identity, minted by the agent before the
	// record exists anywhere.
	ItemID string `json:"item,omitempty"`
	Type   string `json:"t"`
	Time   string `json:"ts"`
	// RunID groups the records of one run (a user input through the final
	// response). Absent on session.created.
	RunID string `json:"run,omitempty"`
	// Iter is diagnostic: the iteration (one logical model request plus
	// its tool executions) within the run.
	Iter int `json:"iter,omitempty"`

	// Session metadata (session.created).
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	ReasoningLevel string `json:"reasoning_level,omitempty"`
	CWD            string `json:"cwd,omitempty"`

	// Message payloads (user.input, assistant.message).
	Content   string         `json:"content,omitempty"`
	Reasoning string         `json:"reasoning,omitempty"`
	ToolCalls []WireToolCall `json:"tool_calls,omitempty"`

	// Tool result payload. ToolCallID belongs to the wire world: providers
	// pair a result with its call by this ID when the conversation is
	// replayed.
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

// replacementRune is what encoding/json substitutes for invalid UTF-8
// bytes when marshaling. Normalized uses the same character so that a
// normalized record is byte-identical to its persisted JSON form.
const replacementRune = "\uFFFD"

// Normalized returns the record with every text field coerced to valid
// UTF-8. Without this, json.Marshal silently replaces invalid bytes with
// U+FFFD at persistence time: the in-memory record, the folded state, and
// anything hashing either side (the web digest chain) would diverge from
// what is stored and from what a resume reads back. Sources of invalid
// bytes are real — raw command output (tools/shell), non-UTF-8 files,
// git diff on binaries. Storage implementations apply this at the append
// boundary; callers fold and deliver only the returned (normalized)
// record.
func (r Record) Normalized() Record {
	r.ItemID = strings.ToValidUTF8(r.ItemID, replacementRune)
	r.Type = strings.ToValidUTF8(r.Type, replacementRune)
	r.Time = strings.ToValidUTF8(r.Time, replacementRune)
	r.RunID = strings.ToValidUTF8(r.RunID, replacementRune)
	r.Provider = strings.ToValidUTF8(r.Provider, replacementRune)
	r.Model = strings.ToValidUTF8(r.Model, replacementRune)
	r.ReasoningLevel = strings.ToValidUTF8(r.ReasoningLevel, replacementRune)
	r.CWD = strings.ToValidUTF8(r.CWD, replacementRune)
	r.Content = strings.ToValidUTF8(r.Content, replacementRune)
	r.Reasoning = strings.ToValidUTF8(r.Reasoning, replacementRune)
	r.ToolCallID = strings.ToValidUTF8(r.ToolCallID, replacementRune)
	r.ToolName = strings.ToValidUTF8(r.ToolName, replacementRune)
	r.Error = strings.ToValidUTF8(r.Error, replacementRune)
	for i := range r.ToolCalls {
		r.ToolCalls[i].ID = strings.ToValidUTF8(r.ToolCalls[i].ID, replacementRune)
		r.ToolCalls[i].Name = strings.ToValidUTF8(r.ToolCalls[i].Name, replacementRune)
		r.ToolCalls[i].Arguments = strings.ToValidUTF8(r.ToolCalls[i].Arguments, replacementRune)
	}
	return r
}

// WireToolCalls converts model tool calls to their wire form.
func WireToolCalls(calls []model.ToolCall) []WireToolCall {
	if len(calls) == 0 {
		return nil
	}
	result := make([]WireToolCall, 0, len(calls))
	for _, call := range calls {
		result = append(result, WireToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	return result
}

// ModelToolCalls converts wire tool calls back to their model form.
func ModelToolCalls(calls []WireToolCall) []model.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	result := make([]model.ToolCall, 0, len(calls))
	for _, call := range calls {
		result = append(result, model.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	return result
}

// Durable reports whether the record must survive a power failure.
// Failures are durable so the reason a run died survives a crash;
// everything else in the vocabulary is already terminal content.
// Durable reports whether the log must flush this record to disk
// before acknowledging it (the durable record set). Every record type
// currently in existence is durable; the switch is kept as the single
// place a future transient record type declares itself.
func (r Record) Durable() bool {
	switch r.Type {
	case TypeSessionCreated, TypeUserInput, TypeAssistantMessage,
		TypeToolResult, TypeRunFailed, TypeContextCompacted:
		return true
	default:
		return false
	}
}

// NewTime returns the record timestamp format.
func NewTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Validate checks the invariant parts of a record. It is deliberately
// permissive about payload fields: Fold decides what each type means.
func (r Record) Validate() error {
	if r.Type == "" {
		return fmt.Errorf("record type is required")
	}
	if r.ItemID == "" {
		return fmt.Errorf("record item id is required")
	}
	if r.Time != "" {
		if _, err := time.Parse(time.RFC3339Nano, r.Time); err != nil {
			return fmt.Errorf("record time is not RFC3339: %w", err)
		}
	}
	return nil
}
