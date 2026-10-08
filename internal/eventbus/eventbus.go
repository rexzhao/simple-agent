// Package eventbus defines the durable-event contract between the agent
// loop and its persistence sink. The agent publishes conversation content
// (assistant messages, tool results); the sessionlog publisher turns each
// into one append-only record.
package eventbus

import (
	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	KindAssistantReady  = "assistant.ready"
	KindToolResultReady = "tool_result.ready"
)

// Event is any publishable event.
type Event interface {
	Kind() string
}

// DurableEvent marks an event which must be persisted before the agent loop
// proceeds.
type DurableEvent interface {
	Event
	durableEvent()
}

// Publisher is the agent loop's durable sink. Publish must block until the
// event is durable; an error aborts the run.
type Publisher interface {
	Publish(Event) error
}

// AssistantReady is the finalized assistant message of one agent iteration.
// ItemID is stable for the logical message across the transient stream.
type AssistantReady struct {
	TurnID         string
	AgentIteration int
	ItemID         string
	Message        model.Message
}

func (AssistantReady) Kind() string  { return KindAssistantReady }
func (AssistantReady) durableEvent() {}

// ToolResultReady is one completed tool execution.
type ToolResultReady struct {
	TurnID         string
	AgentIteration int
	Result         model.ToolResult
}

func (ToolResultReady) Kind() string  { return KindToolResultReady }
func (ToolResultReady) durableEvent() {}
