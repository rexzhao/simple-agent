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

	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
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

const chainVersion = "sai/v2/"

// chainGenesis returns h_0 of segment 0: the digest before any record
// of a never-compacted session.
func chainGenesis(sessionID string) string {
	return chainGenesisAt(sessionID, "")
}

// chainGenesisAt is the active segment's genesis (§14.4): a segment
// that begins after a compaction ([session.created, context.compacted,
// …]) seeds from the compaction's ItemID — derivable both at truncation
// time and at fresh Open (it IS the segment's second record), so the
// per-segment chain needs no extra storage and no segment-header
// changes. Restart consistency is structural: both sides read the same
// segment and compute the same seed.
func chainGenesisAt(sessionID, compactedItemID string) string {
	seed := sessionID
	if compactedItemID != "" {
		seed = sessionID + "/" + compactedItemID
	}
	sum := sha256.Sum256([]byte(chainVersion + seed))
	return hex.EncodeToString(sum[:])
}

// segmentGenesis derives the active segment's genesis from its record
// shape (§14.4) — a PURE data derivation, no panics: headerless
// segments are real disk states (a hand-edited or unreadable first
// line means no session.created copy, and the jsonl segment switch
// then starts the next segment with the compaction alone; a fresh
// session's log can equally be headerless after manual truncation).
// A malformed segment must degrade to a deterministic chain (clients
// resync through the miss/window path), never wedge the serve.
//
//	[session.created, context.compacted, …] → seeded by records[1]
//	[context.compacted, …] (no header copy)  → seeded by records[0]
//	anything else (segment 0, headerless 0)  → segment-0 genesis
//
// Truncation and fresh Open read the same segment and run the same
// rule, so restart consistency holds for every shape.
func segmentGenesis(sessionID string, records []storage.Record) string {
	switch {
	case len(records) > 1 &&
		records[0].Type == storage.TypeSessionCreated &&
		records[1].Type == storage.TypeContextCompacted:
		return chainGenesisAt(sessionID, records[1].ItemID)
	case len(records) > 0 && records[0].Type == storage.TypeContextCompacted:
		return chainGenesisAt(sessionID, records[0].ItemID)
	default:
		return chainGenesis(sessionID)
	}
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
	// v2: reasoning_level joined the tuple (§15.2 — the sai/v2/ bump).
	// New renderable fields append at the END, never between existing
	// members, and always with a version bump.
	field(r.ReasoningLevel)
	return b.String()
}

// Frame is one SSE message. Polymorphic by Kind: "snapshot" (connection
// setup: anchor, window/resumed records, run state), "record" (a
// committed record), "event" (a transient run event), "status" (run
// state change, chain head, notices).
type Frame struct {
	Kind string `json:"kind"`

	// snapshot
	Anchor    string          `json:"anchor,omitempty"` // digest before the first delivered record
	Resumed   bool            `json:"resumed,omitempty"`
	Total     int             `json:"total,omitempty"` // records held by the server
	More      bool            `json:"more,omitempty"`  // history exists before the anchor
	Records   json.RawMessage `json:"records,omitempty"`
	Running   *bool           `json:"running,omitempty"`
	Streaming *StreamingWire  `json:"streaming,omitempty"` // in-flight assistant item
	Tool      *ToolWire       `json:"tool,omitempty"`      // in-flight tool execution
	Steered   []SteeredWire   `json:"steered,omitempty"`   // queued steer messages

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
	Outcome string `json:"outcome,omitempty"` // tool_approval_resolved: approved / denied / cancelled
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

// StreamingWire is the in-flight assistant item of a mid-stream
// connection: enough to draw the row, nothing more. It is a DTO on
// purpose — marshaling the internal StreamingItem leaks untagged field
// names (and the whole Message shape) onto the wire.
type StreamingWire struct {
	Item      string `json:"item"`
	Content   string `json:"content,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

// ToolWire is the in-flight tool execution of a mid-stream connection.
// CallID is the model's tool-call id: the events that continue this
// execution (tool_result) key by it, and a reconnecting client needs
// the SAME key to attach the eventual result to this part. Item is the
// eventual tool.result record's own identity.
type ToolWire struct {
	Item   string `json:"item"`
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Args   string `json:"args,omitempty"`
	// AwaitingApproval marks the slot as blocked on the host's
	// approval decision (§16.3): the client renders the approval card.
	// The digest chain folds records only — wire fields never affect
	// it, so sai/v2/ does not bump.
	AwaitingApproval bool `json:"awaiting_approval,omitempty"`
}

// SteeredWire is one queued steer message: addressable by identity from
// enqueue time; the eventual user.input record carries the same ItemID,
// so the row merges in place when it lands.
type SteeredWire struct {
	Item string `json:"item"`
	Text string `json:"text"`
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
	case model.CompactionFailedEvent:
		f.Type, f.Item, f.Text = string(e.Type()), e.ItemID, e.Reason
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
	case model.SteerQueuedEvent:
		f.Type, f.Item, f.Text = string(e.Type()), e.ItemID, e.Text
	case model.SteerUpdatedEvent:
		f.Type, f.Item, f.Text = string(e.Type()), e.ItemID, e.Text
	case model.SteerWithdrawnEvent:
		f.Type, f.Item = string(e.Type()), e.ItemID
	case model.ToolApprovalPendingEvent:
		// The approval card's payload (§16.3): the item is the future
		// tool.result's identity — the same id POST /approval takes.
		f.Type, f.Item, f.Name, f.Args = string(e.Type()), e.ItemID, e.Name, e.Arguments
	case model.ToolApprovalResolvedEvent:
		f.Type, f.Item, f.Outcome = string(e.Type()), e.ItemID, e.Outcome
	default:
		return Frame{}, false
	}
	return f, true
}
