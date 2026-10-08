package sessionlog

import (
	"fmt"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
)

// Publisher adapts the agent loop's durable event vocabulary onto JSONL
// records. It implements eventbus.Publisher, so it can be handed to
// agent.Stream directly and the log becomes the single durable sink.
//
// Only events which carry conversation content are recorded: assistant
// messages and tool results. Turn boundaries and streaming checkpoints are
// deliberately not persisted — the log is a conversation, not an event trace.
//
// The current run ID is stamped onto every record so the fold can group an
// assistant message with the user input that prompted it.
type Publisher struct {
	session *Session
}

// NewPublisher returns a publisher writing durable events to session.
func NewPublisher(session *Session) *Publisher { return &Publisher{session: session} }

// SetRun sets the run ID stamped on subsequent records.
func (p *Publisher) SetRun(runID string) { p.session.setCurrentRun(runID) }

// Publish translates one durable event into one appended record. Events the
// log does not model are ignored rather than rejected, so the agent loop can
// publish its full vocabulary without a version dance.
func (p *Publisher) Publish(event eventbus.Event) error {
	if event == nil {
		return fmt.Errorf("event is required")
	}
	runID := p.session.currentRun()
	switch event := event.(type) {
	case eventbus.AssistantReady:
		_, err := p.session.Append(Record{
			Type: TypeAssistantMessage, RunID: runID, TurnID: event.TurnID, ItemID: event.ItemID,
			Iteration: event.AgentIteration, Role: string(model.MessageRoleAssistant),
			Content: event.Message.Content, Reasoning: event.Message.ReasoningContent,
			ToolCalls: wireToolCalls(event.Message.ToolCalls),
		})
		return err
	case eventbus.ToolResultReady:
		_, err := p.session.Append(Record{
			Type: TypeToolResult, RunID: runID, TurnID: event.TurnID, Iteration: event.AgentIteration,
			ToolCallID: event.Result.ToolCallID, ToolName: event.Result.Name,
			Content: event.Result.Content, IsError: event.Result.IsError,
		})
		return err
	default:
		// TurnStarted, TurnCompleted, TurnInterrupted, TurnInputReady,
		// AssistantTextCheckpoint and transient events carry no
		// conversation content; they are not recorded.
		return nil
	}
}
