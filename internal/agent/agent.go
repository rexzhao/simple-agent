package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
)

const DefaultMaxTurns = 8

type ToolExecutor interface {
	Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error)
}

// ActivePromptCheckpoint identifies a safe point in the agent turn where queued
// active prompts may be drained. It is passed to ActivePromptDrain so the
// callback can distinguish why it is being polled.
type ActivePromptCheckpoint int

const (
	// ActivePromptCheckpointBeforeProvider is the checkpoint before the first
	// provider request of the turn.
	ActivePromptCheckpointBeforeProvider ActivePromptCheckpoint = iota
	// ActivePromptCheckpointAfterToolBatch is the checkpoint after a complete
	// assistant tool-call batch with every tool result durably published.
	ActivePromptCheckpointAfterToolBatch
	// ActivePromptCheckpointBeforeTerminal is the checkpoint after a no-tool
	// assistant response, before terminal return.
	ActivePromptCheckpointBeforeTerminal
)

// ActivePromptDrain is an optional callback polled at safe checkpoints during an
// active agent turn. It receives the checkpoint being polled and returns queued
// user messages to append to the active turn history within the same TurnID.
// The agent loop never invokes it during a provider request or a tool call, and
// never between assistant tool calls and their tool results. A nil callback
// preserves the existing turn behavior.
type ActivePromptDrain func(ActivePromptCheckpoint) []model.Message

type Options struct {
	Provider          model.Provider
	ToolExecutor      ToolExecutor
	MaxTurns          int
	TurnID            string
	Publisher         eventbus.Publisher
	ActivePromptDrain ActivePromptDrain
}

type TurnResult struct {
	Messages []model.Message
}

func Stream(ctx context.Context, request model.Request, options Options) (<-chan model.Event, error) {
	events, _, err := stream(ctx, request, options, false)
	return events, err
}

func StreamWithResult(ctx context.Context, request model.Request, options Options) (<-chan model.Event, <-chan TurnResult, error) {
	return stream(ctx, request, options, true)
}

func stream(ctx context.Context, request model.Request, options Options, includeResult bool) (<-chan model.Event, <-chan TurnResult, error) {
	if options.Provider == nil {
		return nil, nil, fmt.Errorf("agent provider is required")
	}

	maxTurns := options.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}

	events := make(chan model.Event)
	var results chan TurnResult
	if includeResult {
		results = make(chan TurnResult, 1)
	}
	go run(ctx, request, options, maxTurns, events, results)
	return events, results, nil
}

func run(ctx context.Context, request model.Request, options Options, maxTurns int, events chan<- model.Event, results chan<- TurnResult) {
	defer close(events)
	if results != nil {
		defer close(results)
	}

	turnID := strings.TrimSpace(options.TurnID)
	if options.Publisher != nil && turnID == "" {
		events <- model.ErrorEvent{Err: fmt.Errorf("agent turn id is required when publisher is configured"), Message: "persist turn"}
		return
	}

	messages := append([]model.Message(nil), request.Messages...)
	enabledTools := enabledToolNames(request.Tools)

	for iteration := 1; iteration <= maxTurns; iteration++ {
		// Checkpoint: drain queued active prompts before the first provider
		// request so appended user input is part of the initial turn history.
		if iteration == 1 {
			var ok bool
			messages, _, ok = drainActivePrompts(events, options.Publisher, options.ActivePromptDrain, ActivePromptCheckpointBeforeProvider, turnID, messages)
			if !ok {
				return
			}
		}

		request.Messages = messages
		events <- model.AgentIterationStartedEvent{Iteration: iteration}

		assistantContent, toolCalls, responseState, stopped := streamModelTurn(ctx, options.Provider, request, events)
		if stopped {
			return
		}

		assistantMessage := model.Message{
			Role:          model.MessageRoleAssistant,
			Content:       assistantContent,
			ToolCalls:     toolCalls,
			ResponseState: responseState,
		}
		if len(toolCalls) == 0 && strings.TrimSpace(assistantContent) == "" {
			events <- model.ErrorEvent{Err: fmt.Errorf("agent returned empty final response")}
			return
		}
		if !publishDurable(events, options.Publisher, eventbus.AssistantReady{TurnID: turnID, AgentIteration: iteration, Message: assistantMessage}, "persist assistant") {
			return
		}
		messages = append(messages, assistantMessage)
		if len(toolCalls) == 0 {
			// Checkpoint: after a no-tool assistant response, drain queued active
			// prompts before terminal return. Queued input extends the active
			// turn when turns remain; at the limit it is still published (never
			// silently dropped) and the run stops with a max_turns error.
			var appended int
			var ok bool
			messages, appended, ok = drainActivePrompts(events, options.Publisher, options.ActivePromptDrain, ActivePromptCheckpointBeforeTerminal, turnID, messages)
			if !ok {
				return
			}
			if appended > 0 {
				if iteration == maxTurns {
					events <- model.ErrorEvent{
						Err: fmt.Errorf("agent reached max_turns %d with queued input after final response", maxTurns),
					}
					return
				}
				continue
			}
			sendResult(results, messages)
			return
		}

		for _, toolCall := range toolCalls {
			result := executeToolCall(ctx, options.ToolExecutor, enabledTools, toolCall, events)
			if !publishDurable(events, options.Publisher, eventbus.ToolResultReady{TurnID: turnID, AgentIteration: iteration, Result: result}, "persist tool result") {
				return
			}
			events <- model.ToolResultEvent{Result: result}
			messages = append(messages, model.Message{
				Role:       model.MessageRoleTool,
				Content:    result.Content,
				ToolCallID: result.ToolCallID,
				IsError:    result.IsError,
			})
		}

		// Checkpoint: after a complete assistant tool-call batch with every tool
		// result durably published, drain queued active prompts before the next
		// provider request. Drained user input is never inserted between
		// assistant tool calls and their tool results.
		var ok bool
		messages, _, ok = drainActivePrompts(events, options.Publisher, options.ActivePromptDrain, ActivePromptCheckpointAfterToolBatch, turnID, messages)
		if !ok {
			return
		}

		if iteration == maxTurns {
			events <- model.ErrorEvent{
				Err: fmt.Errorf("agent reached max_turns %d before the model returned a final response", maxTurns),
			}
			return
		}
	}
}

func publishDurable(out chan<- model.Event, publisher eventbus.Publisher, event eventbus.Event, message string) bool {
	if publisher == nil {
		return true
	}
	if err := publisher.Publish(event); err != nil {
		out <- model.ErrorEvent{Err: err, Message: message}
		return false
	}
	return true
}

// drainActivePrompts polls the active prompt drain callback at a safe
// checkpoint. For every returned message it requires role user, durably
// publishes a separate eventbus.TurnInputReady with the shared turnID, and only
// then appends it to messages. It returns the updated messages slice, the
// number of messages appended, and ok=false if the run must stop because a
// drained message had a non-user role or a durable publish failed. A nil drain
// is a no-op.
func drainActivePrompts(out chan<- model.Event, publisher eventbus.Publisher, drain ActivePromptDrain, checkpoint ActivePromptCheckpoint, turnID string, messages []model.Message) ([]model.Message, int, bool) {
	if drain == nil {
		return messages, 0, true
	}
	appended := 0
	for _, msg := range drain(checkpoint) {
		if msg.Role != model.MessageRoleUser {
			out <- model.ErrorEvent{Err: fmt.Errorf("drained prompt must have role user, got %q", msg.Role), Message: "persist turn input"}
			return messages, appended, false
		}
		if !publishDurable(out, publisher, eventbus.TurnInputReady{TurnID: turnID, Message: msg}, "persist turn input") {
			return messages, appended, false
		}
		messages = append(messages, msg)
		appended++
	}
	return messages, appended, true
}

func sendResult(results chan<- TurnResult, messages []model.Message) {
	if results == nil {
		return
	}
	results <- TurnResult{Messages: copyMessages(messages)}
}

func copyMessages(messages []model.Message) []model.Message {
	copied := append([]model.Message(nil), messages...)
	for i := range copied {
		copied[i].ContentBlocks = append([]model.InputContentBlock(nil), messages[i].ContentBlocks...)
		copied[i].ToolCalls = append([]model.ToolCall(nil), messages[i].ToolCalls...)
		copied[i].ProviderItems = copyProviderItems(messages[i].ProviderItems)
		if messages[i].ResponseState != nil {
			state := copyResponseState(*messages[i].ResponseState)
			copied[i].ResponseState = &state
		}
	}
	return copied
}

func streamModelTurn(ctx context.Context, provider model.Provider, request model.Request, out chan<- model.Event) (string, []model.ToolCall, *model.ResponseState, bool) {
	stream, err := provider.Stream(ctx, request)
	if err != nil {
		out <- model.ErrorEvent{Err: err, Message: "request model"}
		return "", nil, nil, true
	}

	var assistantContent strings.Builder
	var toolCalls []model.ToolCall
	var responseState *model.ResponseState
	for event := range stream {
		switch event := event.(type) {
		case model.TextDeltaEvent:
			assistantContent.WriteString(event.Text)
		case model.ToolCallDoneEvent:
			toolCalls = append(toolCalls, event.ToolCall)
		case model.ResponseStateEvent:
			state := copyResponseState(event.State)
			responseState = &state
			continue
		case model.ErrorEvent:
			out <- event
			return assistantContent.String(), nil, nil, true
		}
		out <- event
	}
	return assistantContent.String(), toolCalls, responseState, false
}

func copyResponseState(state model.ResponseState) model.ResponseState {
	state.ReasoningItems = append([]json.RawMessage(nil), state.ReasoningItems...)
	for index := range state.ReasoningItems {
		state.ReasoningItems[index] = append(json.RawMessage(nil), state.ReasoningItems[index]...)
	}
	if state.OutputItems != nil {
		state.OutputItems = append([]json.RawMessage(nil), state.OutputItems...)
		for index := range state.OutputItems {
			state.OutputItems[index] = append(json.RawMessage(nil), state.OutputItems[index]...)
		}
	}
	return state
}

func copyProviderItems(items []model.ProviderItem) []model.ProviderItem {
	copied := append([]model.ProviderItem(nil), items...)
	for index := range copied {
		copied[index].Data = append(json.RawMessage(nil), items[index].Data...)
	}
	return copied
}

func executeToolCall(ctx context.Context, executor ToolExecutor, enabledTools map[string]struct{}, toolCall model.ToolCall, out chan<- model.Event) model.ToolResult {
	arguments, err := parseToolArguments(toolCall.Arguments)
	if err != nil {
		return toolErrorResult(toolCall, "invalid tool arguments: %v", err)
	}
	if _, ok := enabledTools[toolCall.Name]; !ok {
		return toolErrorResult(toolCall, "tool %q is not enabled", toolCall.Name)
	}
	if executor == nil {
		return toolErrorResult(toolCall, "tool executor is not configured")
	}

	out <- model.ToolStartedEvent{ToolCall: toolCall}
	result, err := executor.Execute(ctx, toolCall.Name, arguments)
	if err != nil {
		return toolErrorResult(toolCall, "%v", err)
	}
	if result.ToolCallID == "" {
		result.ToolCallID = toolCall.ID
	}
	if result.Name == "" {
		result.Name = toolCall.Name
	}
	return limitToolResultOutput(result)
}

func parseToolArguments(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}

	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()

	var arguments map[string]any
	if err := decoder.Decode(&arguments); err != nil {
		return nil, err
	}
	if arguments == nil {
		return nil, fmt.Errorf("must be a JSON object")
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("must contain a single JSON object")
	}
	return arguments, nil
}

func toolErrorResult(toolCall model.ToolCall, format string, args ...any) model.ToolResult {
	return limitToolResultOutput(model.ToolResult{
		ToolCallID: toolCall.ID,
		Name:       toolCall.Name,
		Content:    fmt.Sprintf(format, args...),
		IsError:    true,
	})
}

func enabledToolNames(toolSchemas []model.Tool) map[string]struct{} {
	enabled := make(map[string]struct{}, len(toolSchemas))
	for _, tool := range toolSchemas {
		enabled[tool.Name] = struct{}{}
	}
	return enabled
}
