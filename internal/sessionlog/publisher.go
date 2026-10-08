package sessionlog

import (
	"fmt"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
)

// Publisher adapts the agent loop's durable event vocabulary onto JSONL
// records. It implements eventbus.Publisher and
// eventbus.AssistantCheckpointPublisher, so it can be handed to agent.Stream
// directly and the log becomes the single durable sink.
//
// The current run ID is stamped onto every record so the fold can group a
// session turn with the user input that started it.
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
	case eventbus.TurnStarted:
		_, err := p.session.Append(Record{Type: TypeTurnStarted, RunID: runID, TurnID: event.TurnID})
		return err
	case eventbus.TurnCompleted:
		_, err := p.session.Append(Record{Type: TypeTurnCompleted, RunID: runID, TurnID: event.TurnID})
		return err
	case eventbus.TurnInterrupted:
		_, err := p.session.Append(Record{Type: TypeTurnInterrupted, RunID: runID, TurnID: event.TurnID})
		return err
	case eventbus.TurnInputReady:
		_, err := p.session.Append(Record{Type: TypeTurnInput, RunID: runID, TurnID: event.TurnID, Content: event.Message.Content})
		return err
	case eventbus.AssistantReady:
		_, err := p.session.Append(Record{
			Type: TypeAssistantReady, RunID: runID, TurnID: event.TurnID, ItemID: event.ItemID,
			Iteration: event.AgentIteration, Role: string(model.MessageRoleAssistant),
			Content: event.Message.Content, Reasoning: event.Message.ReasoningContent,
			ToolCalls: wireToolCalls(event.Message.ToolCalls),
		})
		return err
	case eventbus.AssistantTextCheckpoint:
		_, err := p.session.Append(Record{
			Type: TypeAssistantCheckpoint, RunID: runID, TurnID: event.TurnID, ItemID: event.ItemID,
			Iteration: event.AgentIteration, Content: event.Content,
		})
		return err
	case eventbus.ToolResultReady:
		_, err := p.session.Append(Record{
			Type: TypeToolResult, RunID: runID, TurnID: event.TurnID, Iteration: event.AgentIteration,
			ToolCallID: event.Result.ToolCallID, ToolName: event.Result.Name,
			Content: event.Result.Content, IsError: event.Result.IsError,
		})
		return err
	case eventbus.ModelEvent:
		// Transient by contract; never durable.
		return nil
	default:
		return nil
	}
}

// PublishAssistantCheckpoint implements eventbus.AssistantCheckpointPublisher.
func (p *Publisher) PublishAssistantCheckpoint(turnID string, agentIteration int, itemID, content string) error {
	return p.Publish(eventbus.AssistantTextCheckpoint{
		TurnID: turnID, AgentIteration: agentIteration, ItemID: itemID, Content: content,
	})
}
