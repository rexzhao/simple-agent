package sessioncontent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

var errTransientFrameTooLarge = errors.New("transient event exceeds frame bound")

// subscriptionEventFromExecution is the only WebSocket event mapping. It
// translates the shared execution vocabulary into the strict subscription
// union.
func subscriptionEventFromExecution(source execution.SessionStreamEvent, sessionID, runID string) (protocol.TransientSubscriptionEvent, bool, error) {
	if source == nil {
		return protocol.TransientSubscriptionEvent{}, false, nil
	}
	typeName, _ := source["type"].(string)
	event := protocol.TransientSubscriptionEvent{Type: protocol.SubscriptionEventType(typeName), SessionID: sessionID, RunID: runID, TurnID: stringValue(source, "turn_id"), AgentIteration: intValue(source, "agent_iteration")}
	switch typeName {
	case "assistant.message.started", "assistant.message.updated", "assistant.message.completed", "assistant.message.failed":
		event.Type = protocol.SubscriptionEventType(typeName)
		event.ItemID = stringValue(source, "item_id")
		event.MessageRevision = stringValue(source, "message_revision")
		if event.ItemID == "" || event.MessageRevision == "" || event.AgentIteration <= 0 {
			return protocol.TransientSubscriptionEvent{}, false, fmt.Errorf("%s lacks stable message identity or revision", typeName)
		}
		if typeName != "assistant.message.started" {
			event.SnapshotOmitted, _ = source["snapshot_omitted"].(bool)
			event.AssistantContent = stringValueAllowEmpty(source, "content")
			event.Reasoning = stringValueAllowEmpty(source, "reasoning")
			if event.SnapshotOmitted {
				event.AssistantContent, event.Reasoning, event.ToolCalls = "", "", nil
			} else if raw, err := json.Marshal(source["tool_calls"]); err != nil {
				return protocol.TransientSubscriptionEvent{}, false, err
			} else if err := json.Unmarshal(raw, &event.ToolCalls); err != nil {
				return protocol.TransientSubscriptionEvent{}, false, fmt.Errorf("assistant tool calls are invalid: %w", err)
			}
		}
	case "tool.requested", "tool.started", "tool.running":
		if typeName == "tool.requested" {
			event.Type = protocol.SubscriptionEventToolRequested
		} else {
			event.Type = protocol.SubscriptionEventToolRunning
		}
		event.ToolCallID, event.Name, event.Arguments = stringValue(source, "tool_call_id"), stringValue(source, "name"), stringValue(source, "arguments")
		if event.ToolCallID == "" || event.Name == "" || event.AgentIteration <= 0 {
			return protocol.TransientSubscriptionEvent{}, false, fmt.Errorf("tool lifecycle event lacks stable identity")
		}
	case "tool.progress":
		event.Type = protocol.SubscriptionEventToolProgress
		event.ToolCallID, event.Name = stringValue(source, "tool_call_id"), stringValue(source, "name")
		event.ArgumentsDelta = stringValue(source, "arguments_delta")
		if event.ToolCallID == "" || event.Name == "" || event.ArgumentsDelta == "" || event.AgentIteration <= 0 {
			return protocol.TransientSubscriptionEvent{}, false, fmt.Errorf("tool progress lacks stable identity or delta")
		}
	case "tool.finished":
		event.Type = protocol.SubscriptionEventToolFinished
		event.ToolCallID, event.Name, event.ToolContent = stringValue(source, "tool_call_id"), stringValue(source, "name"), stringValue(source, "content")
		value, ok := source["is_error"].(bool)
		if !ok {
			return protocol.TransientSubscriptionEvent{}, false, fmt.Errorf("tool finished is_error is invalid")
		}
		event.IsError = &value
		if event.ToolCallID == "" || event.Name == "" || event.AgentIteration <= 0 {
			return protocol.TransientSubscriptionEvent{}, false, fmt.Errorf("tool finished lacks stable identity")
		}
	case "run.prompt_queue":
		event.Type = protocol.SubscriptionEventPromptQueue
		if raw, err := json.Marshal(source["prompts"]); err != nil {
			return protocol.TransientSubscriptionEvent{}, false, err
		} else if err := json.Unmarshal(raw, &event.Prompts); err != nil {
			return protocol.TransientSubscriptionEvent{}, false, fmt.Errorf("prompt queue is invalid: %w", err)
		}
	case "run.prompt_appended":
		event.Type = protocol.SubscriptionEventPromptAppended
		if raw, err := json.Marshal(source["prompts"]); err != nil {
			return protocol.TransientSubscriptionEvent{}, false, err
		} else if err := json.Unmarshal(raw, &event.AppendedPrompts); err != nil {
			return protocol.TransientSubscriptionEvent{}, false, fmt.Errorf("appended prompt list is invalid: %w", err)
		}
	case "turn.failed":
		event.Type = protocol.SubscriptionEventTurnFailed
		event.Code = stringValue(source, "code")
		event.Message = stringValue(source, "message")
		if event.TurnID == "" || event.Code == "" || event.Message == "" {
			return protocol.TransientSubscriptionEvent{}, false, fmt.Errorf("turn failure lacks bounded diagnostic identity")
		}
	default:
		// usage, provider retry, compaction and durable projection notices are
		// intentionally not transient session-content events in D2. The durable
		// projector remains their source of truth.
		return protocol.TransientSubscriptionEvent{}, false, nil
	}
	return event, true, nil
}

// isTransientExecutionEvent is intentionally only a vocabulary check. It is
// used on the producer's no-subscriber path so durable projection notices
// (item.created, active_history.replaced, usage, and similar events) do
// not manufacture a transient gap. A recognized but malformed transient
// event still counts as a loss and is handled by the owner as recovery
// required; silently ignoring it would claim a cursor continuity that was
// never established.
func isTransientExecutionEvent(source execution.SessionStreamEvent) bool {
	if source == nil {
		return false
	}
	typeName, ok := source["type"].(string)
	if !ok {
		return false
	}
	switch typeName {
	case "assistant.message.started", "assistant.message.updated", "assistant.message.completed", "assistant.message.failed", "tool.requested", "tool.started", "tool.running", "tool.progress", "tool.finished", "run.prompt_queue", "run.prompt_appended", "turn.failed":
		return true
	}
	return false
}

func stringValue(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return value
}

func stringValueAllowEmpty(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return value
}

func intValue(fields map[string]any, key string) int {
	value, _ := integerValue(fields[key])
	return value
}

func integerValue(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, true
	case int8:
		return int(value), true
	case int16:
		return int(value), true
	case int32:
		return int(value), true
	case int64:
		return int(value), int64(int(value)) == value
	case uint:
		return int(value), uint(int(value)) == value
	case uint64:
		return int(value), uint64(int(value)) == value
	case float64:
		return int(value), value == float64(int(value))
	default:
		return 0, false
	}
}

func estimateRunEventBytes(event execution.SessionStreamEvent) int {
	if event == nil {
		return 128
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return DefaultTransientQueueBytes + 1
	}
	return len(encoded) + 32*1024
}

func preflightSubscriptionEventFrame(event json.RawMessage, sessionID string) (int, error) {
	// The provider does not know the eventual subscription id or gateway
	// envelope id. Use the protocol maximum for both fields so this is a
	// conservative complete-message preflight, not an event-payload estimate.
	message := protocol.SubscriptionEventMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscriptionEvent, ID: stringValueOfLength(protocol.MaxWireIdentifierBytes)},
		Payload: protocol.SubscriptionEventPayload{
			SubscriptionID: stringValueOfLength(protocol.MaxWireIdentifierBytes),
			Resource:       protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: sessionID},
			Event:          append(json.RawMessage(nil), event...),
		},
	}
	encoded, err := protocol.EncodeMessage(message)
	if err != nil {
		return 0, fmt.Errorf("preflight transient subscription event: %w", err)
	}
	if len(encoded) > protocol.DefaultMaxMessageBytes {
		return 0, fmt.Errorf("%w: %d bytes", errTransientFrameTooLarge, protocol.DefaultMaxMessageBytes)
	}
	return len(encoded), nil
}

func stringValueOfLength(length int) string {
	return strings.Repeat("x", length)
}
