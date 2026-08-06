package agent

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// AutoCompact is an optional safe-checkpoint callback invoked after a complete
// tool batch and any queued user input have been durably published, immediately
// before the next provider request. It may return replacement model history.
type AutoCompact func(context.Context, []model.Message) ([]model.Message, error)

// ToolCancellationRegistry tracks in-flight tool calls so they can be
// individually cancelled without aborting the entire agent turn. The agent
// loop registers each tool call before execution and unregisters it after.
// A nil registry is a no-op.
type ToolCancellationRegistry struct {
	mu   sync.Mutex
	stop map[string]context.CancelFunc
}

// NewToolCancellationRegistry returns an empty registry.
func NewToolCancellationRegistry() *ToolCancellationRegistry {
	return &ToolCancellationRegistry{stop: make(map[string]context.CancelFunc)}
}

// Register associates toolCallID with cancel. It returns a wrapped context
// derived from parent that is cancelled when Cancel is called for the same ID
// or when parent is cancelled. The returned cleanup function unregisters the
// entry and must be called when the tool call finishes.
func (r *ToolCancellationRegistry) Register(parent context.Context, toolCallID string) (context.Context, func()) {
	if r == nil {
		return parent, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.stop[toolCallID] = cancel
	r.mu.Unlock()
	return ctx, func() {
		cancel()
		r.mu.Lock()
		delete(r.stop, toolCallID)
		r.mu.Unlock()
	}
}

// Cancel cancels the in-flight tool call identified by toolCallID. It returns
// true if a tool call was found and cancelled.
func (r *ToolCancellationRegistry) Cancel(toolCallID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	cancel, ok := r.stop[toolCallID]
	if ok {
		// Cancellation is a one-shot control action. The gateway cache makes a
		// duplicate request_id idempotent; a different request must not report
		// an already-cancelled call as still active while the tool is unwinding.
		delete(r.stop, toolCallID)
	}
	r.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

type Options struct {
	Provider          model.Provider
	ToolExecutor      ToolExecutor
	MaxTurns          int
	TurnID            string
	Publisher         eventbus.Publisher
	ActivePromptDrain ActivePromptDrain
	AutoCompact       AutoCompact
	ToolCancel        *ToolCancellationRegistry
	// AssistantCheckpoint controls cumulative visible assistant checkpoints.
	// A nil policy uses DefaultAssistantCheckpointPolicy. Checkpoints are only
	// enabled when Publisher also implements eventbus.AssistantCheckpointPublisher.
	AssistantCheckpoint *AssistantCheckpointPolicy
	// NextTurnID enables the execution layer to give each provider request its
	// own durable Turn. When nil, the standalone agent API retains its
	// historical single-turn behavior.
	NextTurnID    func(iteration int) string
	TurnIDChanged func(turnID string)
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
		if iteration > 1 && options.NextTurnID != nil && options.Publisher != nil {
			if !publishDurable(events, options.Publisher, eventbus.TurnCompleted{TurnID: turnID}, "persist turn completion") {
				return
			}
			next := strings.TrimSpace(options.NextTurnID(iteration))
			if next == "" {
				events <- model.ErrorEvent{Err: fmt.Errorf("next agent turn id is blank")}
				return
			}
			turnID = next
			if !publishDurable(events, options.Publisher, eventbus.TurnStarted{TurnID: turnID}, "persist turn start") {
				return
			}
			if options.TurnIDChanged != nil {
				options.TurnIDChanged(turnID)
			}
		}
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

		assistantItemID := newAssistantItemID()
		var checkpoint assistantOutputCheckpoint
		if publisher, ok := options.Publisher.(eventbus.AssistantCheckpointPublisher); ok {
			checkpoint = newAssistantOutputCheckpoint(publisher, turnID, iteration, assistantItemID, options.AssistantCheckpoint)
		}
		assistantContent, reasoningContent, toolCalls, responseState, stopped := streamModelTurn(ctx, options.Provider, request, events, assistantItemID, checkpoint)
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
			events <- model.ErrorEvent{Err: fmt.Errorf("agent returned empty final response")}
			return
		}
		if !publishDurable(events, options.Publisher, eventbus.AssistantReady{TurnID: turnID, AgentIteration: iteration, ItemID: assistantItemID, Message: assistantMessage}, "persist assistant") {
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
			result := executeToolCall(ctx, options.ToolExecutor, enabledTools, toolCall, options.ToolCancel, events)
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
		if options.AutoCompact != nil {
			compacted, err := options.AutoCompact(ctx, copyMessages(messages))
			if err != nil {
				events <- model.ErrorEvent{Err: err, Message: "auto compact"}
				return
			}
			messages = compacted
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

type assistantOutputCheckpoint func(content string, force bool) (length int, committed bool, err error)

func newAssistantOutputCheckpoint(publisher eventbus.AssistantCheckpointPublisher, turnID string, iteration int, itemID string, policy *AssistantCheckpointPolicy) assistantOutputCheckpoint {
	if publisher == nil {
		return nil
	}
	config := DefaultAssistantCheckpointPolicy
	if policy != nil {
		config = *policy
	}
	lastLength := 0
	lastCheckpointAt := time.Time{}
	return func(content string, force bool) (int, bool, error) {
		if strings.TrimSpace(content) == "" {
			return lastLength, false, nil
		}
		length := runeCount(content)
		if length == lastLength && lastLength > 0 {
			return lastLength, false, nil
		}
		if !force && lastLength > 0 {
			newRunes := length - lastLength
			intervalElapsed := config.MinInterval <= 0 || time.Since(lastCheckpointAt) >= config.MinInterval
			charsReached := config.MinNewRunes > 0 && newRunes >= config.MinNewRunes
			if !charsReached && !intervalElapsed {
				return lastLength, false, nil
			}
		}
		if err := publisher.PublishAssistantCheckpoint(turnID, iteration, itemID, content); err != nil {
			return lastLength, false, err
		}
		lastLength = length
		lastCheckpointAt = time.Now()
		return lastLength, true, nil
	}
}

func runeCount(value string) int {
	return len([]rune(value))
}

func streamModelTurn(ctx context.Context, provider model.Provider, request model.Request, out chan<- model.Event, assistantItemID string, checkpoint assistantOutputCheckpoint) (string, string, []model.ToolCall, *model.ResponseState, bool) {
	var assistantContent strings.Builder
	var reasoningContent strings.Builder
	var toolCalls []model.ToolCall
	var responseState *model.ResponseState
	const maxAttempts = 5
	var idleRetried bool
	lastDurableTextLength := 0
	flushAssistant := func() error {
		if checkpoint == nil || assistantContent.Len() == 0 {
			return nil
		}
		length, _, err := checkpoint(assistantContent.String(), true)
		if err == nil {
			lastDurableTextLength = length
		}
		return err
	}
	failAfterPartialOutput := func(err error, message string) (string, string, []model.ToolCall, *model.ResponseState, bool) {
		if flushErr := flushAssistant(); flushErr != nil {
			out <- model.ErrorEvent{Err: flushErr, Message: "persist assistant output"}
			return assistantContent.String(), reasoningContent.String(), nil, nil, true
		}
		out <- model.ErrorEvent{Err: err, Message: message}
		return assistantContent.String(), reasoningContent.String(), nil, nil, true
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
				checkpointed := false
				if checkpoint != nil {
					length, committed, err := checkpoint(assistantContent.String(), false)
					if err != nil {
						out <- model.ErrorEvent{Err: err, Message: "persist assistant output"}
						return assistantContent.String(), reasoningContent.String(), nil, nil, true
					}
					lastDurableTextLength = length
					checkpointed = committed
				}
				if checkpoint != nil {
					event.AssistantItemID = assistantItemID
					event.DurableTextLength = lastDurableTextLength
					event.DurableCheckpointed = checkpointed
					outputEvent = event
				}
			case model.ReasoningDeltaEvent:
				madeProgress = true
				reasoningContent.WriteString(event.Text)
				// The assistant item is allocated before the provider stream
				// starts. Carry that identity on every reasoning delta just as
				// the visible text deltas carry it, so replay can reconcile the
				// transient step with the durable assistant projection.
				event.AssistantItemID = assistantItemID
				outputEvent = event
			case model.ToolCallDeltaEvent:
				madeProgress = true
			case model.ToolCallDoneEvent:
				madeProgress = true
				toolCalls = append(toolCalls, event.ToolCall)
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
		if err := flushAssistant(); err != nil {
			out <- model.ErrorEvent{Err: err, Message: "persist assistant output"}
			return assistantContent.String(), reasoningContent.String(), nil, nil, true
		}
		return assistantContent.String(), reasoningContent.String(), toolCalls, responseState, false
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

func executeToolCall(ctx context.Context, executor ToolExecutor, enabledTools map[string]struct{}, toolCall model.ToolCall, cancel *ToolCancellationRegistry, out chan<- model.Event) model.ToolResult {
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
	toolCtx, cleanup := cancel.Register(ctx, toolCall.ID)
	defer cleanup()
	result, err := executor.Execute(toolCtx, toolCall.Name, arguments)
	if err != nil {
		if errors.Is(err, context.Canceled) && toolCtx.Err() == context.Canceled && ctx.Err() == nil {
			return model.ToolResult{
				ToolCallID: toolCall.ID,
				Name:       toolCall.Name,
				Content:    "[tool execution cancelled by user]",
				IsError:    true,
			}
		}
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
