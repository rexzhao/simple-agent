// Package web serves the sai web UI: a log view over the session record
// stream with a runtime self-check (digest chain) instead of a chat
// renderer. See docs/webui.md for the design.
package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/storage"
)

// Digest chain (docs/webui.md §4.1).
//
// The chain makes "what the page holds" verifiable against "what the
// server delivered": h_k is the digest after folding the first k records,
// computed identically on both sides from DECODED record fields (never
// from re-serialized JSON — Go and JS escapers differ on <>& and U+2028).
// It doubles as a resumable cursor: a client sends its h, the server
// finds it in its index and sends only the suffix. A miss costs nothing
// but bandwidth (window replay), which is what makes the cursor safe.
//
// The tuple covers every field the page renders or keys on; the encoding
// is length-prefixed so it is injective; the version prefix freezes the
// tuple — a new renderable field means a version bump and a resync for
// every client.

const chainVersion = "sai/v1/"

// chainGenesis returns h_0, the digest before any record.
func chainGenesis(sessionID string) string {
	sum := sha256.Sum256([]byte(chainVersion + sessionID))
	return hex.EncodeToString(sum[:])
}

// chainStep folds one record onto the chain.
func chainStep(prev string, r storage.Record) string {
	sum := sha256.Sum256([]byte(prev + "\n" + chainInput(r)))
	return hex.EncodeToString(sum[:])
}

// chainInput builds the length-prefixed encoding of one record's tuple.
// field(s) = decimal length, ':', the bytes.
func chainInput(r storage.Record) string {
	var b strings.Builder
	field := func(s string) {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	field(r.ItemID)
	field(r.Type)
	field(r.Time)
	field(r.Content)
	field(r.Reasoning)
	if r.IsError {
		field("1")
	} else {
		field("0")
	}
	field(r.ToolCallID)
	field(r.ToolName)
	field(r.Error)
	b.WriteString(strconv.Itoa(len(r.ToolCalls)))
	b.WriteByte('#')
	for _, call := range r.ToolCalls {
		field(call.Name)
		field(call.Arguments)
		field(call.ID)
	}
	field(r.Provider)
	field(r.Model)
	field(r.CWD)
	return b.String()
}

// Frame is one SSE message. Polymorphic by Kind: "snapshot" (connection
// setup: anchor, window/resumed records, run state), "record" (a
// committed record), "event" (a transient run event), "status" (run
// state change, chain head, notices).
type Frame struct {
	Kind string `json:"kind"`

	// snapshot
	Anchor   string          `json:"anchor,omitempty"` // digest before the first delivered record
	Resumed  bool            `json:"resumed,omitempty"`
	Total    int             `json:"total,omitempty"` // records held by the server
	More     bool            `json:"more,omitempty"`  // history exists before the anchor
	Records  json.RawMessage `json:"records,omitempty"`
	Running  *bool           `json:"running,omitempty"`
	Streamin json.RawMessage `json:"streaming,omitempty"` // in-flight assistant item
	Tool     json.RawMessage `json:"tool,omitempty"`      // in-flight tool execution

	// record
	Record json.RawMessage `json:"r,omitempty"`

	// event
	Type    string `json:"t,omitempty"`
	Item    string `json:"item,omitempty"`
	Text    string `json:"text,omitempty"`
	CallID  string `json:"call_id,omitempty"`
	Name    string `json:"name,omitempty"`
	Args    string `json:"args,omitempty"`
	Err     bool   `json:"is_err,omitempty"`
	Est     int    `json:"est,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Try     int    `json:"try,omitempty"`
	TryMax  int    `json:"try_max,omitempty"`
	DelayMS int64  `json:"delay_ms,omitempty"`
	Reason  string `json:"reason,omitempty"`

	// status
	Digest     string `json:"d,omitempty"`
	Count      int    `json:"n,omitempty"`
	Compacting *bool  `json:"compacting,omitempty"`
	Notice     string `json:"notice,omitempty"`
}

func encodeFrame(f Frame) []byte {
	data, err := json.Marshal(f)
	if err != nil {
		// Frame fields are plain strings/ints and pre-encoded JSON;
		// marshaling cannot fail.
		panic(fmt.Sprintf("web: encode frame: %v", err))
	}
	return data
}

// encodeRecordFrame wraps one committed record.
func encodeRecordFrame(r storage.Record) []byte {
	encoded, _ := json.Marshal(r)
	return encodeFrame(Frame{Kind: "record", Record: encoded})
}

// encodeEventFrame maps a transient run event onto the wire. Events are
// best-effort display sugar; the durable story is always the record that
// supersedes them by ItemID.
func encodeEventFrame(event model.Event) (Frame, bool) {
	f := Frame{Kind: "event"}
	switch e := event.(type) {
	case model.AgentIterationStartedEvent:
		f.Type = string(e.Type())
	case model.AssistantMessageStartedEvent:
		f.Type, f.Item = string(e.Type()), e.ItemID
	case model.AssistantMessageCompletedEvent:
		f.Type, f.Item = string(e.Type()), e.ItemID
	case model.AssistantMessageFailedEvent:
		f.Type, f.Item = string(e.Type()), e.ItemID
	case model.TextDeltaEvent:
		f.Type, f.Text = string(e.Type()), e.Text
	case model.ReasoningDeltaEvent:
		f.Type, f.Text = string(e.Type()), e.Text
	case model.MessageDoneEvent:
		f.Type = string(e.Type())
	case model.ToolCallDeltaEvent:
		f.Type, f.CallID, f.Name, f.Args = string(e.Type()), e.ID, e.Name, e.ArgumentsDelta
	case model.ToolCallDoneEvent:
		f.Type, f.CallID, f.Name, f.Args = string(e.Type()), e.ToolCall.ID, e.ToolCall.Name, e.ToolCall.Arguments
	case model.ToolStartedEvent:
		f.Type, f.Item, f.CallID, f.Name, f.Args = string(e.Type()), e.ItemID, e.ToolCall.ID, e.ToolCall.Name, e.ToolCall.Arguments
	case model.ToolResultEvent:
		f.Type, f.CallID, f.Name, f.Err, f.Text = string(e.Type()), e.Result.ToolCallID, e.Result.Name, e.Result.IsError, e.Result.Content
	case model.CompactionStartedEvent:
		f.Type, f.Item = string(e.Type()), e.ItemID
	case model.CompactionCompletedEvent:
		f.Type, f.Item = string(e.Type()), e.ItemID
	case model.ContextPressureEvent:
		f.Type, f.Est, f.Limit = string(e.Type()), e.EstimatedTokens, e.HardInputLimit
	case model.UsageEvent:
		f.Type = string(e.Type())
	case model.ResponseStateEvent:
		f.Type = string(e.Type())
	case model.ProviderRetryEvent:
		f.Type, f.Try, f.TryMax, f.DelayMS, f.Reason = string(e.Type()), e.Attempt, e.MaxAttempts, e.Delay.Milliseconds(), e.Reason
	case model.ErrorEvent:
		f.Type, f.Text = string(e.Type()), e.Message
	default:
		return Frame{}, false
	}
	return f, true
}
