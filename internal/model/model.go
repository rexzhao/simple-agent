package model

import (
	"context"
	"encoding/json"
)

type Provider interface {
	Stream(ctx context.Context, request Request) (<-chan Event, error)
}

type Request struct {
	Model      string
	Messages   []Message
	Tools      []Tool
	Parameters map[string]any
	SessionID  string
}

type MessageRole string

const (
	MessageRoleSystem    MessageRole = "system"
	MessageRoleDeveloper MessageRole = "developer"
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleTool      MessageRole = "tool"
)

type Message struct {
	Role          MessageRole
	Content       string
	ContentBlocks []InputContentBlock
	ToolCallID    string
	ToolCalls     []ToolCall
	IsError       bool
	ResponseState *ResponseState
}

// InputContentBlock represents a Responses API input content block. Other
// adapters may continue to use Message.Content until they add equivalent
// multimodal support.
type InputContentBlock struct {
	Type                  string
	Text                  string
	ImageURL              string
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
	EventTypeAgentIterationStarted EventType = "agent_iteration_started"
	EventTypeTextDelta             EventType = "text_delta"
	EventTypeReasoningDelta        EventType = "reasoning_delta"
	EventTypeMessageDone           EventType = "message_done"
	EventTypeToolCallDelta         EventType = "tool_call_delta"
	EventTypeToolCallDone          EventType = "tool_call_done"
	EventTypeToolStarted           EventType = "tool_started"
	EventTypeToolResult            EventType = "tool_result"
	EventTypeUsage                 EventType = "usage"
	EventTypeResponseState         EventType = "response_state"
	EventTypeSubagentCompletion    EventType = "subagent_completion"
	EventTypeError                 EventType = "error"
)

type Event interface {
	Type() EventType
}

type AgentIterationStartedEvent struct {
	Iteration int
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
	InputTokens      int
	OutputTokens     int
	TotalTokens      int
	CachedTokens     int
	CacheWriteTokens int
	ReasoningTokens  int
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

func (ResponseStateEvent) Type() EventType {
	return EventTypeResponseState
}

type SubagentCompletionEvent struct {
	JobID       string
	AgentID     string
	DisplayName string
	JobName     string
	Status      string
}

func (SubagentCompletionEvent) Type() EventType {
	return EventTypeSubagentCompletion
}

type ErrorEvent struct {
	Err     error
	Message string
}

func (ErrorEvent) Type() EventType {
	return EventTypeError
}
