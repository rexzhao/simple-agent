package model

import (
	"context"
	"encoding/json"
	"time"
)

type Provider interface {
	Stream(ctx context.Context, request Request) (<-chan Event, error)
}

// CompactionProvider is implemented by providers that expose a stateless
// compaction endpoint. The returned items are provider-owned and must be
// persisted and replayed without interpreting their payload.
type CompactionProvider interface {
	Compact(ctx context.Context, request Request) (CompactionResult, error)
}

type CompactionResult struct {
	Items []ProviderItem
	Usage Usage
}

type Request struct {
	Model         string
	Messages      []Message
	Tools         []Tool
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

type MessageRole string

const (
	MessageRoleSystem    MessageRole = "system"
	MessageRoleDeveloper MessageRole = "developer"
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleTool      MessageRole = "tool"
	MessageRoleProvider  MessageRole = "provider"
)

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

type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

type ToolCall struct {
	ID         string
	ProviderID string
	Name       string
	Arguments  string
}

type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string
	IsError    bool
}

type EventType string

const (
	EventTypeAgentIterationStarted     EventType = "agent_iteration_started"
	EventTypeAssistantMessageStarted   EventType = "assistant_message_started"
	EventTypeAssistantMessageUpdated   EventType = "assistant_message_updated"
	EventTypeAssistantMessageCompleted EventType = "assistant_message_completed"
	EventTypeAssistantMessageFailed    EventType = "assistant_message_failed"
	EventTypeTextDelta                 EventType = "text_delta"
	EventTypeReasoningDelta            EventType = "reasoning_delta"
	EventTypeMessageDone               EventType = "message_done"
	EventTypeToolCallDelta             EventType = "tool_call_delta"
	EventTypeToolCallDone              EventType = "tool_call_done"
	EventTypeToolStarted               EventType = "tool_started"
	EventTypeToolResult                EventType = "tool_result"
	EventTypeUsage                     EventType = "usage"
	EventTypeResponseState             EventType = "response_state"
	EventTypeProviderRetry             EventType = "provider_retry"
	EventTypeError                     EventType = "error"
)

type Event interface {
	Type() EventType
}

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

// AssistantMessageUpdatedEvent carries the complete accumulated message at a
// monotonic revision. Complete snapshots make delivery idempotent: a later
// update repairs a missed earlier frame without replaying provider deltas.
type AssistantMessageUpdatedEvent struct {
	ItemID         string
	AgentIteration int
	Revision       uint64
	Message        Message
}

func (AssistantMessageUpdatedEvent) Type() EventType {
	return EventTypeAssistantMessageUpdated
}

// AssistantMessageCompletedEvent is emitted only after the final durable
// AssistantReady projection succeeds, so the public lifecycle cannot claim a
// completed message that failed to commit.
type AssistantMessageCompletedEvent struct {
	ItemID         string
	AgentIteration int
	Revision       uint64
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
	Revision       uint64
	Message        Message
}

func (AssistantMessageFailedEvent) Type() EventType {
	return EventTypeAssistantMessageFailed
}

func (AgentIterationStartedEvent) Type() EventType {
	return EventTypeAgentIterationStarted
}

type TextDeltaEvent struct {
	Text string
}

func (TextDeltaEvent) Type() EventType {
	return EventTypeTextDelta
}

type ReasoningDeltaEvent struct {
	Text string
}

func (ReasoningDeltaEvent) Type() EventType {
	return EventTypeReasoningDelta
}

type MessageDoneEvent struct {
	Message      Message
	FinishReason string
}

func (MessageDoneEvent) Type() EventType {
	return EventTypeMessageDone
}

type ToolCallDeltaEvent struct {
	Index          int
	ID             string
	Name           string
	ArgumentsDelta string
}

func (ToolCallDeltaEvent) Type() EventType {
	return EventTypeToolCallDelta
}

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
}

func (ToolStartedEvent) Type() EventType {
	return EventTypeToolStarted
}

type ToolResultEvent struct {
	Result ToolResult
}

func (ToolResultEvent) Type() EventType {
	return EventTypeToolResult
}

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

type UsageEvent struct {
	Usage Usage
}

func (UsageEvent) Type() EventType {
	return EventTypeUsage
}

type ResponseStateEvent struct {
	State ResponseState
}

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

type ErrorEvent struct {
	Err     error
	Message string
}

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
