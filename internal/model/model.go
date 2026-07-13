package model

import "context"

type Provider interface {
	Stream(ctx context.Context, request Request) (<-chan Event, error)
}

type Request struct {
	Model      string
	Messages   []Message
	Tools      []Tool
	Parameters map[string]any
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
	Role       MessageRole
	Content    string
	ToolCallID string
	ToolCalls  []ToolCall
	IsError    bool
}

type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string
	IsError    bool
}

type EventType string

const (
	EventTypeTextDelta          EventType = "text_delta"
	EventTypeReasoningDelta     EventType = "reasoning_delta"
	EventTypeMessageDone        EventType = "message_done"
	EventTypeToolCallDelta      EventType = "tool_call_delta"
	EventTypeToolCallDone       EventType = "tool_call_done"
	EventTypeToolStarted        EventType = "tool_started"
	EventTypeToolResult         EventType = "tool_result"
	EventTypeUsage              EventType = "usage"
	EventTypeSubagentCompletion EventType = "subagent_completion"
	EventTypeError              EventType = "error"
)

type Event interface {
	Type() EventType
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
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

type UsageEvent struct {
	Usage Usage
}

func (UsageEvent) Type() EventType {
	return EventTypeUsage
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
