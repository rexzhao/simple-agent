package agent

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

const DefaultMaxTurns = 8

var providerRetryBackoff = func(attempt int) time.Duration {
	return time.Duration(5*(1<<(attempt-1))) * time.Second
}

var assistantFallbackID uint64

type ToolExecutor interface {
	Execute(ctx context.Context, name string, arguments map[string]any) (model.ToolResult, error)
}

type Options struct {
	Provider     model.Provider
	ToolExecutor ToolExecutor
	MaxTurns     int
	TurnID       string
	Publisher    eventbus.Publisher
}

// AssistantCheckpointPolicy bounds write amplification while preserving an
// immediate first visible checkpoint. Either threshold may trigger a later
// checkpoint; terminal and provider-failure paths always force one.
type AssistantCheckpointPolicy struct {
	MinInterval time.Duration
	MinNewRunes int
}

var DefaultAssistantCheckpointPolicy = AssistantCheckpointPolicy{
	MinInterval: 75 * time.Millisecond,
	MinNewRunes: 64,
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
		request.Messages = messages
		events <- model.AgentIterationStartedEvent{Iteration: iteration}

		assistantItemID := newAssistantItemID()
		events <- model.AssistantMessageStartedEvent{ItemID: assistantItemID, AgentIteration: iteration}
		assistantContent, reasoningContent, toolCalls, responseState, messageRevision, stopped := streamModelTurn(ctx, options.Provider, request, events, assistantItemID, iteration)
		if stopped {
			return
		}

		assistantMessage := model.Message{
			Role:             model.MessageRoleAssistant,
			Content:          assistantContent,
			ReasoningContent: reasoningContent,
			ToolCalls:        toolCalls,
			ResponseState:    responseState,
		}
		if len(toolCalls) == 0 && strings.TrimSpace(assistantContent) == "" {
			events <- model.AssistantMessageFailedEvent{ItemID: assistantItemID, AgentIteration: iteration, Revision: messageRevision + 1, Message: assistantMessage}
			events <- model.ErrorEvent{Err: fmt.Errorf("agent returned empty final response")}
			return
		}
		if options.Publisher != nil {
			if err := options.Publisher.Publish(eventbus.AssistantReady{TurnID: turnID, AgentIteration: iteration, ItemID: assistantItemID, Message: assistantMessage}); err != nil {
				events <- model.AssistantMessageFailedEvent{ItemID: assistantItemID, AgentIteration: iteration, Revision: messageRevision + 1, Message: assistantMessage}
				events <- model.ErrorEvent{Err: err, Message: "persist assistant"}
				return
			}
		}
		events <- model.AssistantMessageCompletedEvent{ItemID: assistantItemID, AgentIteration: iteration, Revision: messageRevision + 1, Message: assistantMessage}
		messages = append(messages, assistantMessage)
		if len(toolCalls) == 0 {
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
		for blockIndex := range copied[i].ContentBlocks {
			if copied[i].ContentBlocks[blockIndex].ImageBlob != nil {
				ref := *copied[i].ContentBlocks[blockIndex].ImageBlob
				copied[i].ContentBlocks[blockIndex].ImageBlob = &ref
			}
		}
		copied[i].ToolCalls = append([]model.ToolCall(nil), messages[i].ToolCalls...)
		copied[i].ProviderItems = copyProviderItems(messages[i].ProviderItems)
		if messages[i].ResponseState != nil {
			state := copyResponseState(*messages[i].ResponseState)
			copied[i].ResponseState = &state
		}
	}
	return copied
}

func newAssistantItemID() string {
	var raw [12]byte
	if _, err := cryptorand.Read(raw[:]); err == nil {
		return fmt.Sprintf("assistant-%x", raw[:])
	}
	// Randomness failure must not turn an otherwise usable model response into
	// an item without identity. UnixNano is only a fallback; the normal path is
	// cryptographically random and collision resistant across restarts.
	return fmt.Sprintf("assistant-%d-%d", time.Now().UnixNano(), atomic.AddUint64(&assistantFallbackID, 1))
}

func streamModelTurn(ctx context.Context, provider model.Provider, request model.Request, out chan<- model.Event, assistantItemID string, agentIteration int) (string, string, []model.ToolCall, *model.ResponseState, uint64, bool) {
	var assistantContent strings.Builder
	var assistantRunes int
	var assistantHasNonWhitespace bool
	var reasoningContent strings.Builder
	var toolCalls []model.ToolCall
	var responseState *model.ResponseState
	var messageRevision uint64
	var lastPublishedBytes int
	var lastPublishedAt time.Time
	const snapshotPublishBytes = 16 * 1024
	const snapshotPublishInterval = 50 * time.Millisecond
	messageSnapshot := func() model.Message {
		return model.Message{
			Role: model.MessageRoleAssistant, Content: assistantContent.String(), ReasoningContent: reasoningContent.String(),
			ToolCalls: append([]model.ToolCall(nil), toolCalls...), ResponseState: responseState,
		}
	}
	messageBytes := func() int {
		total := assistantContent.Len() + reasoningContent.Len()
		for _, call := range toolCalls {
			total += len(call.ID) + len(call.Name) + len(call.Arguments)
		}
		return total
	}
	publishMessageUpdate := func(force bool) {
		now := time.Now()
		currentBytes := messageBytes()
		if !force && currentBytes == lastPublishedBytes {
			return
		}
		if !force && messageRevision > 0 && currentBytes-lastPublishedBytes < snapshotPublishBytes && now.Sub(lastPublishedAt) < snapshotPublishInterval {
			return
		}
		messageRevision++
		out <- model.AssistantMessageUpdatedEvent{
			ItemID: assistantItemID, AgentIteration: agentIteration, Revision: messageRevision,
			Message: messageSnapshot(),
		}
		lastPublishedBytes = currentBytes
		lastPublishedAt = now
	}
	publishMessageFailure := func() {
		messageRevision++
		out <- model.AssistantMessageFailedEvent{ItemID: assistantItemID, AgentIteration: agentIteration, Revision: messageRevision, Message: messageSnapshot()}
	}
	const maxAttempts = 5
	var idleRetried bool
	failAfterPartialOutput := func(err error, message string) (string, string, []model.ToolCall, *model.ResponseState, uint64, bool) {
		publishMessageFailure()
		out <- model.ErrorEvent{Err: err, Message: message}
		return assistantContent.String(), reasoningContent.String(), nil, nil, messageRevision, true
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		stream, err := provider.Stream(ctx, request)
		if err != nil {
			// A failed Stream() call never made progress, so only the
			// classification, budget, and context decide whether to retry.
			if ctx.Err() == nil && attempt < maxAttempts && model.IsRetryableProviderError(err) {
				delay := providerRetryBackoff(attempt)
				out <- model.ProviderRetryEvent{
					Attempt:     attempt + 1,
					MaxAttempts: maxAttempts,
					Delay:       delay,
					Reason:      model.RetryReason(err),
				}
				if err := waitForProviderRetry(ctx, delay); err != nil {
					return failAfterPartialOutput(err, "retry model request")
				}
				continue
			}
			return failAfterPartialOutput(err, "request model")
		}

		madeProgress := false
		retry := false
	streamLoop:
		for event := range stream {
			outputEvent := event
			switch event := event.(type) {
			case model.TextDeltaEvent:
				madeProgress = true
				assistantContent.WriteString(event.Text)
				assistantRunes += utf8.RuneCountInString(event.Text)
				assistantHasNonWhitespace = assistantHasNonWhitespace || strings.TrimSpace(event.Text) != ""
				publishMessageUpdate(false)
			case model.ReasoningDeltaEvent:
				madeProgress = true
				reasoningContent.WriteString(event.Text)
				publishMessageUpdate(false)
			case model.ToolCallDeltaEvent:
				madeProgress = true
			case model.ToolCallDoneEvent:
				madeProgress = true
				toolCalls = append(toolCalls, event.ToolCall)
				publishMessageUpdate(true)
			case model.MessageDoneEvent, model.UsageEvent:
				madeProgress = true
			case model.ResponseStateEvent:
				state := copyResponseState(event.State)
				responseState = &state
				continue
			case model.ErrorEvent:
				idle := httpstream.IsStreamIdleTimeout(event.Err)
				if !madeProgress && attempt < maxAttempts && ctx.Err() == nil &&
					model.IsRetryableProviderError(event.Err) && (!idle || !idleRetried) {
					if idle {
						idleRetried = true
					}
					delay := providerRetryBackoff(attempt)
					out <- model.ProviderRetryEvent{
						Attempt:     attempt + 1,
						MaxAttempts: maxAttempts,
						Delay:       delay,
						Reason:      model.RetryReason(event.Err),
					}
					if err := waitForProviderRetry(ctx, delay); err != nil {
						return failAfterPartialOutput(err, "retry model request")
					}
					retry = true
					break streamLoop
				}
				return failAfterPartialOutput(event.Err, event.Message)
			}
			out <- outputEvent
		}
		if retry {
			continue
		}
		if err := ctx.Err(); err != nil {
			return failAfterPartialOutput(err, "stream model")
		}
		return assistantContent.String(), reasoningContent.String(), toolCalls, responseState, messageRevision, false
	}
	panic("unreachable")
}

func waitForProviderRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
	if !utf8.ValidString(raw) {
		return nil, fmt.Errorf("must be valid UTF-8")
	}
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
