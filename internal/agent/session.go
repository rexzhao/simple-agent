package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
	"github.com/rexzhao/simple-agent/internal/storage"
)

// subscribeBuffer bounds one subscriber's channel. Commits block when a
// subscriber stalls: backpressure instead of dropped records. A stalled
// subscriber is released by Stop closing its done channel.
const subscribeBuffer = 1024

// subscription is one registered record subscriber. The records channel is
// never closed — closing it would race a commit's in-flight delivery —
// instead Stop closes done, which ends the reader's loop and unblocks a
// sender stuck on a full buffer.
type subscription struct {
	records chan storage.Record
	done    chan struct{}
}

// deliver sends the stored records in order, giving up as soon as the
// subscription is stopped.
func (sub *subscription) deliver(stored []storage.Record) {
	for _, record := range stored {
		select {
		case sub.records <- record:
		case <-sub.done:
			return
		}
	}
}

// providerRetryBackoff is the delay before retry attempt n (1-based).
var providerRetryBackoff = func(attempt int) time.Duration {
	return time.Duration(5*(1<<(attempt-1))) * time.Second
}

// Session is one conversation: the state layer above a dumb record store.
// commit is the only write path (append → fold → clear transient → notify,
// under one lock). Exactly one run may be active; Send while running is
// rejected.
type Session struct {
	agent   *Agent
	storage storage.Session

	mu      sync.RWMutex
	state   *storage.State
	records []storage.Record
	// pendingCreated is the session.created record of a fresh session,
	// committed ahead of the first real record. Nil on reopened sessions.
	pendingCreated *storage.Record
	running        bool
	cancel         context.CancelFunc
	events         chan model.Event
	done           chan struct{}
	subs           map[*subscription]struct{}
}

// ID returns the session ID, available before the first record.
func (s *Session) ID() string { return s.storage.ID() }

// Persisted reports whether the session has committed any record. An
// empty session (one never sent a message) leaves nothing on disk, so its
// id cannot be resumed later.
func (s *Session) Persisted() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records) > 0
}

// RecordCount returns how many records the session has committed. The
// CLI waits until its record printer has caught up with this count before
// drawing the next prompt, so output never lands behind the prompt.
func (s *Session) RecordCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records)
}

// Snapshot returns a deep copy of the state, including the transient
// streaming and running slots.
func (s *Session) Snapshot() storage.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return storage.CopyState(s.state)
}

// Running reports whether a run is active.
func (s *Session) Running() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// Events returns the transient event stream of the run: the active one,
// or the previous run's closed channel (receiving drains any buffered
// events, then returns false immediately).
func (s *Session) Events() <-chan model.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.events
}

// Subscription delivers committed records to one subscriber.
type Subscription struct {
	session *Session
	sub     *subscription
	initial []storage.Record
}

// Initial returns the records committed before the subscription was
// registered. The caller must process Initial fully before consuming
// Records: both streams are ordered on their own, and a record committed
// after registration arrives on Records even while Initial is still
// being processed. (The CLI is safe because it binds only while no run
// is active; a future WebUI bridge must respect this ordering too.)
func (s *Subscription) Initial() []storage.Record { return s.initial }

// Records returns the delivery channel. It is never closed; stop reading
// when Done is closed.
func (s *Subscription) Records() <-chan storage.Record { return s.sub.records }

// Done is closed by Stop. A reader selects on Records and Done:
//
//	for {
//		select {
//		case record := <-sub.Records():
//			// render
//		case <-sub.Done():
//			return
//		}
//	}
func (s *Subscription) Done() <-chan struct{} { return s.sub.done }

// Stop ends delivery. It is idempotent and never blocks: a commit blocked
// on this subscription's full buffer is released, not deadlocked.
func (s *Subscription) Stop() {
	s.session.mu.Lock()
	if _, ok := s.session.subs[s.sub]; ok {
		delete(s.session.subs, s.sub)
		close(s.sub.done)
	}
	s.session.mu.Unlock()
}

// Subscribe atomically snapshots the committed records and registers a
// subscription carrying every later commit. No record can be missed or
// duplicated: the snapshot and the registration happen under one lock
// section, and commits deliver only to registered subscriptions.
func (s *Session) Subscribe() *Subscription {
	sub := &subscription{
		records: make(chan storage.Record, subscribeBuffer),
		done:    make(chan struct{}),
	}
	s.mu.Lock()
	records := append([]storage.Record(nil), s.records...)
	if s.subs == nil {
		s.subs = make(map[*subscription]struct{})
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	return &Subscription{session: s, sub: sub, initial: records}
}

// Send appends the user input durably and starts the run. It returns as
// soon as the run is launched; output arrives on Events and via Subscribe.
//
// The check, the durable append, and the running-flag transition happen in
// one lock section, so a concurrent Send can never interleave its records
// with another run's: the loser is rejected before anything is written.
func (s *Session) Send(text string) error {
	text = strings.TrimRight(text, "\r\n")
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("input is empty")
	}
	runID := newRunID()
	events := make(chan model.Event, 256)
	ctx, cancel := context.WithCancel(context.Background())

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		cancel()
		return fmt.Errorf("a run is already active; stop it first")
	}
	stored, err := s.commitLocked(storage.Record{
		ItemID: storage.NewUserInputID(), Type: storage.TypeUserInput,
		RunID: runID, Content: text,
	})
	if err != nil {
		s.mu.Unlock()
		cancel()
		return err
	}
	request := s.agent.buildRequest(s.state.Context)
	s.running = true
	s.cancel = cancel
	s.events = events
	done := make(chan struct{})
	s.done = done
	var subs []*subscription
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	// Notify before launching the run goroutine, so subscribers see the
	// user input ahead of anything the run commits.
	for _, sub := range subs {
		sub.deliver(stored)
	}
	go s.run(ctx, runID, request, events, done)
	return nil
}

// Stop cancels the active run. The run settles it as interrupted: no
// failure record, the user input simply stays unanswered.
func (s *Session) Stop() {
	s.mu.RLock()
	cancel := s.cancel
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

// Close stops any active run, waits for it to settle, and closes the
// storage session.
func (s *Session) Close() error {
	s.Stop()
	s.mu.RLock()
	done := s.done
	s.mu.RUnlock()
	if done != nil {
		<-done
	}
	return s.storage.Close()
}

// commit is the only write path: it stores the record (flushing the
// pending session.created first), folds it, clears the optional transient
// slot, and notifies subscribers — in commit order — after releasing the
// session lock. A stalled subscriber applies backpressure to commits but
// must never block snapshots, Send, or Close. Notification order equals
// commit order: commits are serialized by the single-flight discipline,
// and Send notifies before launching the run goroutine.
func (s *Session) commit(record storage.Record, clear func(*storage.State)) error {
	s.mu.Lock()
	stored, err := s.commitLocked(record)
	// clear runs even on error: it ends the transient slot's lifecycle
	// (the run is aborting anyway), which is independent of whether this
	// record reached the storage.
	if clear != nil {
		clear(s.state)
	}
	var subs []*subscription
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, sub := range subs {
		sub.deliver(stored)
	}
	return nil
}

// commitLocked stores one record (flushing the pending session.created of
// a fresh session ahead of it, so every record flows through this one
// write path), folds it into the durable state, and caches it. Callers
// hold s.mu. A record is folded only after storage accepted it, so the
// state never runs ahead of the log.
func (s *Session) commitLocked(record storage.Record) ([]storage.Record, error) {
	var stored []storage.Record
	if s.pendingCreated != nil {
		created, err := s.appendLocked(*s.pendingCreated)
		if err != nil {
			return nil, err
		}
		s.pendingCreated = nil
		stored = append(stored, created)
	}
	last, err := s.appendLocked(record)
	if err != nil {
		return nil, err
	}
	return append(stored, last), nil
}

// appendLocked performs the append → fold → cache sequence for a single
// record. Callers hold s.mu.
func (s *Session) appendLocked(record storage.Record) (storage.Record, error) {
	stored, err := s.storage.Append(record)
	if err != nil {
		return storage.Record{}, err
	}
	storage.Fold(s.state, stored)
	s.records = append(s.records, stored)
	return stored, nil
}

// beginAssistant opens the streaming slot for a message being generated.
// The message is an assistant message from birth: the run loop appends the
// streaming snapshot to the request history verbatim.
func (s *Session) beginAssistant(itemID string) {
	s.mu.Lock()
	s.state.Streaming = &storage.StreamingItem{
		ItemID:  itemID,
		Message: model.Message{Role: model.MessageRoleAssistant},
	}
	s.mu.Unlock()
}

// applyStreaming mutates the streaming message under the session lock.
// Deltas are the single accumulation point: the run loop holds no local
// copy.
func (s *Session) applyStreaming(itemID string, apply func(m *model.Message)) {
	s.mu.Lock()
	if item := s.state.Streaming; item != nil && item.ItemID == itemID {
		apply(&item.Message)
	}
	s.mu.Unlock()
}

// resetStreaming clears the accumulated content of the streaming message.
// Retries only happen after no provider progress, so this is defensive.
func (s *Session) resetStreaming(itemID string) {
	s.applyStreaming(itemID, func(m *model.Message) {
		m.Content = ""
		m.ReasoningContent = ""
		m.ToolCalls = nil
	})
}

// streamingSnapshot reads the streaming message.
func (s *Session) streamingSnapshot(itemID string, responseState *model.ResponseState) model.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if item := s.state.Streaming; item != nil && item.ItemID == itemID {
		message := item.Message
		message.ResponseState = responseState
		return message
	}
	// The fallback keeps an assistant role too: the snapshot is always an
	// assistant message, even if the slot was cleared mid-stream by a
	// future feature (compaction, /clear) — a zero role is a provider-side
	// HTTP 400.
	return model.Message{Role: model.MessageRoleAssistant, ResponseState: responseState}
}

// commitAssistantMessage commits the finished message and clears the
// streaming slot in the same lock section, so a snapshot never shows the
// message twice or not at all.
func (s *Session) commitAssistantMessage(itemID, runID string, iteration int, message model.Message) error {
	return s.commit(storage.Record{
		ItemID: itemID, Type: storage.TypeAssistantMessage, RunID: runID, Iter: iteration,
		Content: message.Content, Reasoning: message.ReasoningContent,
		ToolCalls: storage.WireToolCalls(message.ToolCalls),
	}, func(state *storage.State) { state.Streaming = nil })
}

// beginTool opens the running slot: the item ID of the future result is
// minted here, before the result exists.
func (s *Session) beginTool(resultItemID string, call model.ToolCall) {
	s.mu.Lock()
	s.state.Running = &storage.RunningTool{ItemID: resultItemID, ToolCall: call, StartedAt: time.Now()}
	s.mu.Unlock()
}

// commitToolResult commits the finished result and clears the running slot
// in the same lock section.
func (s *Session) commitToolResult(resultItemID, runID string, iteration int, result model.ToolResult) error {
	return s.commit(storage.Record{
		ItemID: resultItemID, Type: storage.TypeToolResult, RunID: runID, Iter: iteration,
		ToolCallID: result.ToolCallID, ToolName: result.Name,
		Content: result.Content, IsError: result.IsError,
	}, func(state *storage.State) { state.Running = nil })
}

// settle clears the run slot and the transient region. It is the last
// state change before the run's done channel closes: whatever the exit
// path, a settled session never shows a streaming message, a running
// tool, or an in-flight compaction in its Snapshot (the partial content
// lived only in the failure events).
func (s *Session) settle() {
	s.mu.Lock()
	s.running = false
	s.cancel = nil
	s.state.Streaming = nil
	s.state.Running = nil
	s.state.Compacting = nil
	s.mu.Unlock()
}

// Compact summarizes the folded context and replaces it with the summary.
// The compaction item's identity is minted before the model call; the
// context.compacted record lands only on success — a failed or
// interrupted compaction leaves the context untouched. Exclusive with
// Send: the run slot is held for the duration of the summary call.
func (s *Session) Compact() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("a run is already active; stop it first")
	}
	if len(s.state.Context) == 0 {
		s.mu.Unlock()
		return fmt.Errorf("nothing to compact")
	}
	itemID := storage.NewContextCompactedID()
	s.state.Compacting = &storage.CompactingItem{ItemID: itemID, StartedAt: time.Now()}
	request := s.agent.buildCompactionRequest(s.state.Context)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.running = true
	s.cancel = cancel
	s.done = done
	s.mu.Unlock()

	defer close(done)
	defer s.settle()
	defer cancel()

	summary, err := s.agent.summarizeContext(ctx, request)
	if err != nil {
		return fmt.Errorf("compact: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		return fmt.Errorf("compact: summary was empty")
	}
	return s.commit(storage.Record{
		ItemID: itemID, Type: storage.TypeContextCompacted, Content: summary,
	}, func(state *storage.State) { state.Compacting = nil })
}

// run drives one agent run: iterate (request the model, execute the tool
// calls it asked for) until the model returns a final response without
// tool calls, the iteration budget is exhausted, or the run is stopped.
func (s *Session) run(ctx context.Context, runID string, request model.Request, events chan model.Event, done chan struct{}) {
	defer close(done)
	defer close(events)
	defer s.settle()

	// A failure is recorded only when the run was not interrupted: an
	// interruption leaves the user input unanswered, which is its own
	// state. This defer runs before settle (LIFO).
	var failure string
	defer func() {
		if failure == "" || ctx.Err() != nil {
			return
		}
		_ = s.commit(storage.Record{
			ItemID: storage.NewRunFailedID(), Type: storage.TypeRunFailed,
			RunID: runID, Error: failure,
		}, nil)
	}()

	maxIterations := s.agent.maxIterations
	messages := append([]model.Message(nil), request.Messages...)
	enabledTools := enabledToolNames(request.Tools)

	for iteration := 1; iteration <= maxIterations; iteration++ {
		request.Messages = messages
		sendEvent(ctx, events, model.AgentIterationStartedEvent{Iteration: iteration})

		itemID := storage.NewAssistantMessageID()
		s.beginAssistant(itemID)
		sendEvent(ctx, events, model.AssistantMessageStartedEvent{ItemID: itemID, AgentIteration: iteration})
		message, turnFailure, stopped := s.streamModelTurn(ctx, request, events, itemID, iteration)
		if stopped {
			failure = turnFailure
			return
		}

		if isBlank(message) {
			sendEvent(ctx, events, model.AssistantMessageFailedEvent{ItemID: itemID, AgentIteration: iteration, Message: message})
			failure = "agent returned empty final response"
			sendEvent(ctx, events, model.ErrorEvent{Err: errors.New(failure)})
			return
		}
		if err := s.commitAssistantMessage(itemID, runID, iteration, message); err != nil {
			sendEvent(ctx, events, model.AssistantMessageFailedEvent{ItemID: itemID, AgentIteration: iteration, Message: message})
			failure = fmt.Sprintf("persist assistant: %v", err)
			sendEvent(ctx, events, model.ErrorEvent{Err: err, Message: "persist assistant"})
			return
		}
		sendEvent(ctx, events, model.AssistantMessageCompletedEvent{ItemID: itemID, AgentIteration: iteration, Message: message})
		messages = append(messages, message)
		if len(message.ToolCalls) == 0 {
			return
		}

		for _, toolCall := range message.ToolCalls {
			resultItemID := storage.NewToolResultID()
			s.beginTool(resultItemID, toolCall)
			result := executeToolCall(ctx, s.agent.toolExecutor, enabledTools, toolCall, resultItemID, events)
			if err := s.commitToolResult(resultItemID, runID, iteration, result); err != nil {
				failure = fmt.Sprintf("persist tool result: %v", err)
				sendEvent(ctx, events, model.ErrorEvent{Err: err, Message: "persist tool result"})
				return
			}
			sendEvent(ctx, events, model.ToolResultEvent{Result: result})
			messages = append(messages, model.Message{
				Role:       model.MessageRoleTool,
				Content:    result.Content,
				ToolCallID: result.ToolCallID,
				IsError:    result.IsError,
			})
		}

		if iteration == maxIterations {
			failure = fmt.Sprintf("agent reached max iterations %d before the model returned a final response", maxIterations)
			sendEvent(ctx, events, model.ErrorEvent{Err: errors.New(failure)})
			return
		}
	}
}

// streamModelTurn streams one logical model request (including its
// retries) into the session's streaming slot, forwarding transient events.
// It returns the accumulated message. When stopped is true the run must
// end and failure carries the reason (an interruption returns stopped
// with a failure the caller's recorder discards).
func (s *Session) streamModelTurn(ctx context.Context, request model.Request, out chan<- model.Event, assistantItemID string, agentIteration int) (model.Message, string, bool) {
	var responseState *model.ResponseState
	publishMessageFailure := func() {
		sendEvent(ctx, out, model.AssistantMessageFailedEvent{
			ItemID: assistantItemID, AgentIteration: agentIteration,
			Message: s.streamingSnapshot(assistantItemID, responseState),
		})
	}
	const maxAttempts = 5
	var idleRetried bool
	failAfterPartialOutput := func(err error, message string) (model.Message, string, bool) {
		publishMessageFailure()
		failure := eventMessage(model.ErrorEvent{Err: err, Message: message})
		sendEvent(ctx, out, model.ErrorEvent{Err: err, Message: message})
		return s.streamingSnapshot(assistantItemID, responseState), failure, true
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			// Retries only happen after no provider progress, so the
			// streaming content is already empty; clear defensively.
			s.resetStreaming(assistantItemID)
		}
		stream, err := s.agent.provider.Stream(ctx, request)
		if err != nil {
			// A failed Stream() call never made progress, so only the
			// classification, budget, and context decide whether to retry.
			if ctx.Err() == nil && attempt < maxAttempts && model.IsRetryableProviderError(err) {
				delay := providerRetryBackoff(attempt)
				sendEvent(ctx, out, model.ProviderRetryEvent{
					Attempt: attempt + 1, MaxAttempts: maxAttempts,
					Delay: delay, Reason: model.RetryReason(err),
				})
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
				s.applyStreaming(assistantItemID, func(m *model.Message) { m.Content += event.Text })
			case model.ReasoningDeltaEvent:
				madeProgress = true
				s.applyStreaming(assistantItemID, func(m *model.Message) { m.ReasoningContent += event.Text })
			case model.ToolCallDeltaEvent:
				madeProgress = true
			case model.ToolCallDoneEvent:
				madeProgress = true
				call := event.ToolCall
				if call.ID == "" {
					// The provider issued no id; mint one. Providers pair
					// assistant tool calls with results by id, so a minted
					// id is valid as long as both sides agree on it.
					call.ID = storage.NewToolCallID()
				}
				s.applyStreaming(assistantItemID, func(m *model.Message) { m.ToolCalls = append(m.ToolCalls, call) })
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
					sendEvent(ctx, out, model.ProviderRetryEvent{
						Attempt: attempt + 1, MaxAttempts: maxAttempts,
						Delay: delay, Reason: model.RetryReason(event.Err),
					})
					if err := waitForProviderRetry(ctx, delay); err != nil {
						return failAfterPartialOutput(err, "retry model request")
					}
					retry = true
					break streamLoop
				}
				return failAfterPartialOutput(event.Err, event.Message)
			}
			sendEvent(ctx, out, outputEvent)
		}
		if retry {
			continue
		}
		if err := ctx.Err(); err != nil {
			return failAfterPartialOutput(err, "stream model")
		}
		return s.streamingSnapshot(assistantItemID, responseState), "", false
	}
	panic("unreachable")
}

// sendEvent delivers one transient event unless the run was stopped: a
// stopped run's remaining events are dropped rather than blocking the run
// goroutine on a full channel (the durable story of an interruption is
// the unanswered user input).
func sendEvent(ctx context.Context, out chan<- model.Event, event model.Event) {
	select {
	case out <- event:
	case <-ctx.Done():
	}
}

// waitForProviderRetry sleeps for the backoff delay unless the run is
// stopped first.
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

// executeToolCall validates and executes one call, publishing lifecycle
// events. Validation failures never emit ToolStartedEvent. resultItemID
// is the identity of the tool.result this call will produce, carried on
// the started event so in-flight observers can hold the slot.
func executeToolCall(ctx context.Context, executor ToolExecutor, enabledTools map[string]struct{}, toolCall model.ToolCall, resultItemID string, out chan<- model.Event) model.ToolResult {
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

	sendEvent(ctx, out, model.ToolStartedEvent{ToolCall: toolCall, ItemID: resultItemID})
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
