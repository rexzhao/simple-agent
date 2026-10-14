package model

import (
	"context"
	"encoding/json"
	"time"
)

// Provider is a model transport: one Stream call is one conversational
// request; the returned channel delivers the response as events and is
// closed when the response ends (or the request fails after retries).
type Provider interface {
	Stream(ctx context.Context, request Request) (<-chan Event, error)
}

// Request is one conversational request to a model: the message
// history, the tool definitions, and the provider parameters.
type Request struct {
	Model         string
	Messages      []Message
	Tools         []ToolDefinition
	Parameters    map[string]any
	SessionID     string
	DeveloperRole MessageRole
	// MaxTokens is the provider-required output token cap. Anthropic Messages
	// injects it as max_tokens (the API requires the field) and OpenAI
	// Responses as max_output_tokens when the caller did not already set them
	// in Parameters. Codex and OpenAI Chat never inject it: their endpoints
	// reject the parameter with HTTP 400.
	MaxTokens int
}

// MessageRole is a message's role in the conversation. The wire roles
// (system/developer/user/assistant/tool) map onto each provider's API;
// MessageRoleProvider marks provider-specific items (Anthropic thinking
// blocks, Responses reasoning items) that ride in history verbatim.
type MessageRole string

const (
	MessageRoleSystem    MessageRole = "system"
	MessageRoleDeveloper MessageRole = "developer"
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleTool      MessageRole = "tool"
	MessageRoleProvider  MessageRole = "provider"
)

// Message is one entry of the conversation history, in the unified
// shape every provider adapter translates to and from.
type Message struct {
	Role             MessageRole
	Content          string
	ReasoningContent string
	ContentBlocks    []InputContentBlock
	ToolCallID       string
	ToolCalls        []ToolCall
	IsError          bool
	ResponseState    *ResponseState
	ProviderItems    []ProviderItem
}

// ProviderItem is an opaque provider input/output item. Origin and Model scope
// the payload so an encrypted checkpoint is never replayed to a different
// endpoint or model by accident.
type ProviderItem struct {
	Origin string
	Model  string
	Data   json.RawMessage
}

// InputContentBlock represents a provider input content block. ImageURL is a
// data URL while the message is in memory; ImageBlob is the durable reference
// used by session storage and is hydrated back into ImageURL before requests.
type InputContentBlock struct {
	Type                  string
	Text                  string
	ImageURL              string
	ImageBlob             *BlobRef `json:"image_blob,omitempty"`
	FileID                string
	Detail                string
	PromptCacheBreakpoint bool
}

// ResponseState contains the provider-owned identifiers and opaque reasoning
// items required to continue or manually replay an OpenAI Responses turn.
// Response output text remains in Message.Content so existing session blob
// storage continues to own large visible content.
type ResponseState struct {
	ID             string
	Origin         string
	Model          string
	Stored         bool
	MessageID      string
	MessagePhase   string
	ReasoningItems []json.RawMessage
	OutputItems    []json.RawMessage
}

// ToolDefinition is the provider-facing tool shape: name, description,
// and JSON schema. Pure data — the agent pairs it with execution (the
// agent.Tool interface).
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolCall is the model's request to execute a tool. ID is the
// agent-minted identity used for record addressing; ProviderID is the
// wire identity (tool_call_id / tool_use_id) used for context pairing
// — the agent mints one when the provider returns none.
type ToolCall struct {
	ID         string
	ProviderID string
	Name       string
	Arguments  string
}

// ToolResult is the outcome of one tool call, delivered back to the
// model as the tool role message. IsError carries refusals and
// failures — the model sees them and can adjust.
type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string
	IsError    bool
}

// EventType discriminates the Event variants (the streaming protocol
// between providers and the agent loop).
type EventType string

const (
	EventTypeAgentIterationStarted     EventType = "agent_iteration_started"
	EventTypeAssistantMessageStarted   EventType = "assistant_message_started"
	EventTypeAssistantMessageCompleted EventType = "assistant_message_completed"
	EventTypeAssistantMessageFailed    EventType = "assistant_message_failed"
	EventTypeTextDelta                 EventType = "text_delta"
	EventTypeReasoningDelta            EventType = "reasoning_delta"
	EventTypeMessageDone               EventType = "message_done"
	EventTypeToolCallDelta             EventType = "tool_call_delta"
	EventTypeToolCallDone              EventType = "tool_call_done"
	EventTypeToolStarted               EventType = "tool_started"
	EventTypeToolResult                EventType = "tool_result"
	EventTypeCompactionStarted         EventType = "compaction_started"
	EventTypeToolApprovalPending       EventType = "tool_approval_pending"
	EventTypeToolApprovalResolved      EventType = "tool_approval_resolved"
	EventTypeCompactionCompleted       EventType = "compaction_completed"
	EventTypeCompactionFailed          EventType = "compaction_failed"
	EventTypeContextPressure           EventType = "context_pressure"
	EventTypeUsage                     EventType = "usage"
	EventTypeResponseState             EventType = "response_state"
	EventTypeProviderRetry             EventType = "provider_retry"
	EventTypeError                     EventType = "error"
	EventTypeSteerQueued               EventType = "steer_queued"
	EventTypeSteerUpdated              EventType = "steer_updated"
	EventTypeSteerWithdrawn            EventType = "steer_withdrawn"
)

// Event is one streaming step of a model response: text/reasoning
// deltas, tool-call deltas and completions, usage, retry notices, and
// errors. The channel closes when the response ends.
type Event interface {
	Type() EventType
}

// AgentIterationStartedEvent opens an iteration (one logical request
// plus its tool executions).
type AgentIterationStartedEvent struct {
	Iteration int
}

// AssistantMessageStartedEvent establishes the stable identity of one
// assistant message before provider streaming begins. Provider deltas remain
// internal input to the agent accumulator; consumers update this entity by
// ItemID instead of manufacturing an unbound text tail.
type AssistantMessageStartedEvent struct {
	ItemID         string
	AgentIteration int
}

func (AssistantMessageStartedEvent) Type() EventType {
	return EventTypeAssistantMessageStarted
}

// AssistantMessageCompletedEvent is emitted only after the final durable
// AssistantReady projection succeeds, so the public lifecycle cannot claim a
// completed message that failed to commit.
type AssistantMessageCompletedEvent struct {
	ItemID         string
	AgentIteration int
	Message        Message
}

func (AssistantMessageCompletedEvent) Type() EventType {
	return EventTypeAssistantMessageCompleted
}

// AssistantMessageFailedEvent closes a message lifecycle that was started but
// could not be durably completed. Message is the latest accumulated snapshot.
type AssistantMessageFailedEvent struct {
	ItemID         string
	AgentIteration int
	Message        Message
}

func (AssistantMessageFailedEvent) Type() EventType {
	return EventTypeAssistantMessageFailed
}

func (AgentIterationStartedEvent) Type() EventType {
	return EventTypeAgentIterationStarted
}

// TextDeltaEvent appends a chunk of assistant text.
type TextDeltaEvent struct {
	Text string
}

func (TextDeltaEvent) Type() EventType {
	return EventTypeTextDelta
}

// ReasoningDeltaEvent appends a chunk of reasoning content
// (Anthropic thinking / interleaved reasoning).
type ReasoningDeltaEvent struct {
	Text string
}

func (ReasoningDeltaEvent) Type() EventType {
	return EventTypeReasoningDelta
}

// MessageDoneEvent marks the assistant message complete — the final
// text and tool calls are settled.
type MessageDoneEvent struct {
	Message      Message
	FinishReason string
}

func (MessageDoneEvent) Type() EventType {
	return EventTypeMessageDone
}

// ToolCallDeltaEvent appends an incremental piece of one tool call
// (streaming argument fragments).
type ToolCallDeltaEvent struct {
	Index          int
	ID             string
	Name           string
	ArgumentsDelta string
}

func (ToolCallDeltaEvent) Type() EventType {
	return EventTypeToolCallDelta
}

// ToolCallDoneEvent completes one tool call of the message.
type ToolCallDoneEvent struct {
	ToolCall ToolCall
}

func (ToolCallDoneEvent) Type() EventType {
	return EventTypeToolCallDone
}

// ToolStartedEvent is emitted after a tool call has passed argument
// validation, the enabled-tool check, and the executor check, immediately
// before the executor runs. It maps to the runtime tool.started lifecycle.
type ToolStartedEvent struct {
	ToolCall ToolCall
	// ItemID is the identity of the tool.result this call will produce,
	// minted when execution starts, so observers can address the outcome
	// before it exists.
	ItemID string
}

func (ToolStartedEvent) Type() EventType {
	return EventTypeToolStarted
}

// ToolResultEvent carries one finished tool result back to the
// consumer of the stream.
type ToolResultEvent struct {
	Result ToolResult
}

func (ToolResultEvent) Type() EventType {
	return EventTypeToolResult
}

// ToolApprovalPendingEvent announces that a tool call is awaiting the
// host's approval decision (the policy hook returned Ask). ItemID is
// the identity of the future tool.result.
type ToolApprovalPendingEvent struct {
	ItemID    string
	Name      string
	Arguments string
}

func (ToolApprovalPendingEvent) Type() EventType {
	return EventTypeToolApprovalPending
}

// ToolApprovalResolvedEvent closes a pending approval: the host
// approved or denied it, or the wait was cancelled (run stop, session
// close).
type ToolApprovalResolvedEvent struct {
	ItemID  string
	Outcome string // approved / denied / cancelled
}

func (ToolApprovalResolvedEvent) Type() EventType {
	return EventTypeToolApprovalResolved
}

// CompactionStartedEvent opens an in-run context compaction. ItemID is
// the identity of the context.compacted record which will land on
// success; a skipped or failed compaction never completes it.
type CompactionStartedEvent struct {
	ItemID string
}

func (CompactionStartedEvent) Type() EventType {
	return EventTypeCompactionStarted
}

// CompactionCompletedEvent closes a successful in-run compaction.
type CompactionCompletedEvent struct {
	ItemID string
}

func (CompactionCompletedEvent) Type() EventType {
	return EventTypeCompactionCompleted
}

// CompactionFailedEvent reports an in-run compaction that failed
// without committing anything. Under soft pressure the run continues
// with the original request (the failure is a notice, not a run
// failure); under hard pressure the run fails right after this event.
type CompactionFailedEvent struct {
	ItemID string
	Reason string
}

func (CompactionFailedEvent) Type() EventType {
	return EventTypeCompactionFailed
}

// ContextPressureEvent reports that the request exceeded the hard input
// budget but no compaction was possible (nothing to summarize). There
// is no local preflight — the estimate is heuristic — so the request is
// sent anyway and the provider decides.
type ContextPressureEvent struct {
	EstimatedTokens int
	HardInputLimit  int
}

func (ContextPressureEvent) Type() EventType {
	return EventTypeContextPressure
}

// Usage is the token accounting of a request, as reported by the
// provider.
type Usage struct {
	// InputTokens excludes tokens reported in CachedTokens and
	// CacheWriteTokens. The four token buckets are mutually exclusive.
	InputTokens      int
	OutputTokens     int
	TotalTokens      int
	CachedTokens     int
	CacheWriteTokens int
	ReasoningTokens  int
}

// UsageFromInclusiveInput converts OpenAI-style usage, where inputTokens
// includes cached reads and cache writes, into the disjoint Usage buckets used
// throughout the agent. Provider-reported totals are intentionally ignored so
// TotalTokens always equals the sum of those buckets.
func UsageFromInclusiveInput(inputTokens, outputTokens, cachedTokens, cacheWriteTokens, reasoningTokens int) Usage {
	uncachedInputTokens := inputTokens - cachedTokens - cacheWriteTokens
	if uncachedInputTokens < 0 {
		uncachedInputTokens = 0
	}
	return Usage{
		InputTokens:      uncachedInputTokens,
		OutputTokens:     outputTokens,
		TotalTokens:      uncachedInputTokens + outputTokens + cachedTokens + cacheWriteTokens,
		CachedTokens:     cachedTokens,
		CacheWriteTokens: cacheWriteTokens,
		ReasoningTokens:  reasoningTokens,
	}
}

// UsageEvent reports token usage for the request.
type UsageEvent struct {
	Usage Usage
}

func (UsageEvent) Type() EventType {
	return EventTypeUsage
}

// ResponseStateEvent carries provider-internal state (OpenAI
// Responses reasoning items) that must round-trip to continue a
// conversation on that provider.
type ResponseStateEvent struct {
	State ResponseState
}

// ProviderRetryEvent announces an upcoming retry after a retryable
// provider error (attempt, delay, reason).
type ProviderRetryEvent struct {
	Attempt     int
	MaxAttempts int
	Delay       time.Duration
	Reason      string
}

func (ProviderRetryEvent) Type() EventType {
	return EventTypeProviderRetry
}

func (ResponseStateEvent) Type() EventType {
	return EventTypeResponseState
}

// ErrorEvent reports a failure after retries were exhausted; the
// channel closes right after.
type ErrorEvent struct {
	Err     error
	Message string
}

// SteerQueuedEvent announces a user input queued for mid-run injection.
// The ItemID is the identity the eventual user.input record will carry.
type SteerQueuedEvent struct {
	ItemID string
	Text   string
}

func (SteerQueuedEvent) Type() EventType { return EventTypeSteerQueued }

// SteerUpdatedEvent announces an edit to a still-queued steer message.
type SteerUpdatedEvent struct {
	ItemID string
	Text   string
}

func (SteerUpdatedEvent) Type() EventType { return EventTypeSteerUpdated }

// SteerWithdrawnEvent announces a queued steer message was withdrawn
// before consumption.
type SteerWithdrawnEvent struct {
	ItemID string
}

func (SteerWithdrawnEvent) Type() EventType { return EventTypeSteerWithdrawn }

func (ErrorEvent) Type() EventType {
	return EventTypeError
}

// ProviderError reports a failure returned by the model provider itself —
// an in-stream error event or an HTTP error status. The message is the
// provider's own text and is safe to surface to operators.
type ProviderError struct {
	Message string
}

func (e *ProviderError) Error() string { return e.Message }
