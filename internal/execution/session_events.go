package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessionprojector"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

const (
	defaultSessionChatItemsLimit = 50
	maximumSessionChatItemsLimit = 200
)

const maxSessionStreamToolContentRunes = 4096

type SessionItemsPage struct {
	Items         []SessionItem `json:"items"`
	OldestSeq     int64         `json:"oldest_seq"`
	NewestSeq     int64         `json:"newest_seq"`
	HasMoreBefore bool          `json:"has_more_before"`
	HasMoreAfter  bool          `json:"has_more_after"`
}

type SessionItemsOptions struct {
	BeforeSeq int64
	AfterSeq  int64
	Limit     int
	// AlignTurn extends the page's oldest edge backwards so the page never
	// starts mid-turn: with AlignTurn set, a latest or before page always
	// contains at least one complete turn and may exceed Limit for long
	// turns. AfterSeq pages are never extended (their oldest edge is an
	// explicit cursor).
	AlignTurn bool
}

type SessionItem struct {
	Seq            int64               `json:"seq"`
	ID             string              `json:"id"`
	TurnID         string              `json:"turn_id,omitempty"`
	AgentIteration int                 `json:"agent_iteration,omitempty"`
	CreatedAt      time.Time           `json:"created_at"`
	Kind           string              `json:"kind"`
	Visibility     string              `json:"visibility"`
	Audience       string              `json:"audience"`
	Status         string              `json:"status,omitempty"`
	Message        *SessionItemMessage `json:"message,omitempty"`
}

type SessionItemMessage struct {
	Role    string                     `json:"role"`
	Content *SessionItemMessageContent `json:"content,omitempty"`
	// Reasoning is included only when the session's show_reasoning setting is
	// enabled. It is persisted on the assistant message, so terminal history
	// does not depend on the transient reasoning.delta stream.
	Reasoning  string                       `json:"reasoning,omitempty"`
	Images     []SessionItemImageAttachment `json:"images,omitempty"`
	ToolCallID string                       `json:"tool_call_id,omitempty"`
	ToolCalls  []SessionItemToolCall        `json:"tool_calls,omitempty"`
	IsError    bool                         `json:"is_error,omitempty"`
}

type SessionItemImageAttachment struct {
	Hash      string `json:"hash"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
}

type SessionImage struct {
	Data      []byte
	MediaType string
}

type SessionItemToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

type SessionItemMessageContent struct {
	Inline  string `json:"inline,omitempty"`
	Preview string `json:"preview,omitempty"`
}

type SessionStreamEvent map[string]any

func NewSessionStreamEvent(eventType string, fields map[string]any) SessionStreamEvent {
	event := make(SessionStreamEvent, len(fields)+1)
	for key, value := range fields {
		event[key] = value
	}
	event["type"] = eventType
	return event
}

// sessionEventSink is the single ordering boundary for presentation of session
// stream events. It decouples the durable bus (which stays synchronous) from a
// potentially slow emit callback: bus events are submitted here without blocking
// on emit, and a dedicated goroutine drains the queue serially.
//
// Coalescing happens at submit time under the queue mutex: a queued, consecutive
// text.delta / reasoning.delta with the same turn_id and type is folded into the
// trailing queued delta via exact concatenation, so a blocked emit cannot grow
// the queue by one map per delta. Caller-owned event maps are never mutated; a
// merged delta is rebuilt from its accumulator when drained. Every other event
// is delivered verbatim, in submission order, with no drops or reordering. The
// terminal event (turn.committed / turn.failed) is submitted last and flushed
// before close+wait returns, so no callback fires after the caller returns.
type sessionEventSink struct {
	emit func(SessionStreamEvent)

	mu     sync.Mutex
	cond   *sync.Cond
	ops    []sinkOp
	closed bool
	done   chan struct{}
}

type sinkOp struct {
	event               SessionStreamEvent // verbatim event for non-delta ops
	isDelta             bool
	eventType           string
	turnID              string
	agentIteration      int
	text                *strings.Builder // accumulator for delta ops
	assistantItemID     string
	durableTextLength   int
	durableCheckpointed bool
	hasAssistantBinding bool
}

func newSessionEventSink(emit func(SessionStreamEvent)) *sessionEventSink {
	s := &sessionEventSink{
		emit: emit,
		done: make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	go s.run()
	return s
}

func (s *sessionEventSink) submit(event SessionStreamEvent) {
	if s == nil || event == nil {
		return
	}
	eventType, _ := event["type"].(string)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if eventType == "text.delta" || eventType == "reasoning.delta" {
		text, _ := event["text"].(string)
		turnID, _ := event["turn_id"].(string)
		agentIteration, _ := event["agent_iteration"].(int)
		assistantItemID, _ := event["item_id"].(string)
		hasAssistantBinding := assistantItemID != ""
		durableTextLength, _ := sessionStreamInteger(event["durable_text_length"])
		durableCheckpointed, _ := event["durable_checkpointed"].(bool)
		if n := len(s.ops); n > 0 {
			last := &s.ops[n-1]
			if last.isDelta && last.eventType == eventType && last.turnID == turnID && last.agentIteration == agentIteration &&
				last.hasAssistantBinding == hasAssistantBinding &&
				(!hasAssistantBinding || (last.assistantItemID == assistantItemID &&
					(eventType != "text.delta" ||
						(last.durableCheckpointed == durableCheckpointed && last.durableTextLength == durableTextLength)))) {
				last.text.WriteString(text)
				s.mu.Unlock()
				return
			}
		}
		builder := &strings.Builder{}
		builder.WriteString(text)
		s.ops = append(s.ops, sinkOp{
			isDelta:             true,
			eventType:           eventType,
			turnID:              turnID,
			agentIteration:      agentIteration,
			text:                builder,
			assistantItemID:     assistantItemID,
			durableTextLength:   durableTextLength,
			durableCheckpointed: durableCheckpointed,
			hasAssistantBinding: hasAssistantBinding,
		})
	} else {
		s.ops = append(s.ops, sinkOp{event: event})
	}
	s.cond.Signal()
	s.mu.Unlock()
}

func (s *sessionEventSink) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.cond.Signal()
	s.mu.Unlock()
}

func (s *sessionEventSink) wait() {
	if s == nil {
		return
	}
	<-s.done
}

func (s *sessionEventSink) run() {
	defer close(s.done)
	for {
		s.mu.Lock()
		for len(s.ops) == 0 && !s.closed {
			s.cond.Wait()
		}
		ops := s.ops
		s.ops = nil
		s.mu.Unlock()

		for _, op := range ops {
			if op.isDelta {
				fields := map[string]any{"turn_id": op.turnID, "text": op.text.String()}
				if op.agentIteration > 0 {
					fields["agent_iteration"] = op.agentIteration
				}
				if op.hasAssistantBinding {
					fields["item_id"] = op.assistantItemID
					if op.eventType == "text.delta" {
						fields["durable_text_length"] = op.durableTextLength
						fields["durable_checkpointed"] = op.durableCheckpointed
					}
				}
				s.emit(NewSessionStreamEvent(op.eventType, fields))
			} else {
				s.emit(op.event)
			}
		}

		s.mu.Lock()
		empty := len(s.ops) == 0 && s.closed
		s.mu.Unlock()
		if empty {
			return
		}
	}
}

func sessionStreamInteger(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		if value != float64(int(value)) {
			return 0, false
		}
		return int(value), true
	default:
		return 0, false
	}
}

func submitSessionStreamEvent(submit func(SessionStreamEvent), event SessionStreamEvent) {
	if submit != nil && event != nil {
		submit(event)
	}
}

func (s *Service) GetSessionChatItems(id string) (SessionItemsPage, error) {
	return s.GetSessionChatItemsPage(id, SessionItemsOptions{})
}

func (s *Service) GetSessionChatItemsPage(id string, options SessionItemsOptions) (SessionItemsPage, error) {
	if s == nil || s.sessionStore == nil {
		return SessionItemsPage{}, fmt.Errorf("execution session store is not configured")
	}
	if options.BeforeSeq < 0 || options.AfterSeq < 0 {
		return SessionItemsPage{}, fmt.Errorf("session item cursors must be non-negative")
	}
	if options.BeforeSeq > 0 && options.AfterSeq > 0 {
		return SessionItemsPage{}, fmt.Errorf("before_seq and after_seq cannot be combined")
	}
	limit := options.Limit
	if limit <= 0 {
		limit = defaultSessionChatItemsLimit
	}
	if limit > maximumSessionChatItemsLimit {
		return SessionItemsPage{}, fmt.Errorf("session item limit cannot exceed %d", maximumSessionChatItemsLimit)
	}
	page, err := s.sessionStore.ReadHistoryPage(id, sessions.HistoryPageOptions{
		BeforeSeq:   options.BeforeSeq,
		AfterSeq:    options.AfterSeq,
		Limit:       limit,
		AlignTurn:   options.AlignTurn,
		VisibleOnly: true,
	})
	if err != nil {
		return SessionItemsPage{}, err
	}
	return s.sessionItemsPageFromStorePage(id, page)
}

func (s *Service) sessionItemsPageFromStorePage(sessionID string, page sessions.HistoryPage) (SessionItemsPage, error) {
	state, err := s.sessionStore.LoadState(sessionID)
	if err != nil {
		return SessionItemsPage{}, err
	}
	return s.sessionItemsPageFromStorePageWithReasoning(sessionID, page, state.ShowReasoning)
}

func (s *Service) sessionItemsPageFromStorePageWithReasoning(sessionID string, page sessions.HistoryPage, showReasoning bool) (SessionItemsPage, error) {
	items := make([]SessionItem, 0, len(page.Items))
	for _, item := range page.Items {
		dto, err := s.sessionItemDTOWithReasoning(sessionID, item, showReasoning)
		if err != nil {
			return SessionItemsPage{}, err
		}
		items = append(items, dto)
	}
	return SessionItemsPage{
		Items:         items,
		OldestSeq:     page.OldestSeq,
		NewestSeq:     page.NewestSeq,
		HasMoreBefore: page.HasMoreBefore,
		HasMoreAfter:  page.HasMoreAfter,
	}, nil
}

// buildItemsPage derives a chat items page from an already-loaded execution
// state without re-reading the store. It remains useful to callers that have
// already made the explicit execution-state load.
func (s *Service) buildItemsPage(session sessions.SessionV2, beforeSeq, afterSeq int64, limit int, alignTurn bool) (SessionItemsPage, error) {
	filtered := make([]sessions.SessionItem, 0, len(session.Items))
	for _, item := range session.Items {
		if sessionItemVisibleInChat(item) {
			filtered = append(filtered, item)
		}
	}
	page, hasMoreBefore, hasMoreAfter := pagedSessionItems(filtered, beforeSeq, afterSeq, limit, alignTurn)
	items := make([]SessionItem, 0, len(page))
	for _, item := range page {
		dto, err := s.sessionItemDTOWithReasoning(session.ID, item, session.ShowReasoning)
		if err != nil {
			return SessionItemsPage{}, err
		}
		items = append(items, dto)
	}
	oldestSeq, newestSeq := sessionItemSeqBounds(page)
	return SessionItemsPage{
		Items:         items,
		OldestSeq:     oldestSeq,
		NewestSeq:     newestSeq,
		HasMoreBefore: hasMoreBefore,
		HasMoreAfter:  hasMoreAfter,
	}, nil
}

// ReadSessionImage returns an image blob only when it is referenced by a
// visible message in the specified session. It deliberately does not expose a
// generic blob lookup endpoint.
func (s *Service) ReadSessionImage(id, hash string) (SessionImage, error) {
	if s == nil || s.sessionStore == nil {
		return SessionImage{}, fmt.Errorf("execution session store is not configured")
	}
	hash = strings.ToLower(strings.TrimSpace(hash))
	if hash == "" {
		return SessionImage{}, fmt.Errorf("image hash is required")
	}
	session, err := s.sessionStore.LoadExecutionState(id)
	if err != nil {
		return SessionImage{}, err
	}
	for _, item := range session.Items {
		if !sessionItemVisibleInChat(item) || item.Message == nil {
			continue
		}
		for _, block := range item.Message.ContentBlocks {
			if block.Type != "input_image" || block.ImageBlob == nil || !strings.EqualFold(block.ImageBlob.Hash, hash) {
				continue
			}
			mediaType, supported := model.NormalizeImageMediaType(block.ImageBlob.MediaType)
			if !supported {
				return SessionImage{}, fmt.Errorf("image %q has unsupported media type", hash)
			}
			raw, err := s.sessionStore.ReadBlobForSession(session.ID, *block.ImageBlob)
			if err != nil {
				return SessionImage{}, err
			}
			return SessionImage{Data: raw, MediaType: mediaType}, nil
		}
	}
	return SessionImage{}, fmt.Errorf("%w: image %q", sessions.ErrNotFound, hash)
}

func (s *Service) GetSessionTurnFinalAssistantOutput(id, turnID string) (string, error) {
	if s == nil || s.sessionStore == nil {
		return "", fmt.Errorf("execution session store is not configured")
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return "", fmt.Errorf("turn id is required")
	}
	session, err := s.sessionStore.LoadExecutionState(id)
	if err != nil {
		return "", err
	}

	var output string
	for _, item := range session.Items {
		if item.TurnID != turnID || !sessionItemVisibleInChat(item) {
			continue
		}
		if item.Message == nil || item.Message.Role != model.MessageRoleAssistant || item.Audience != sessions.ItemAudienceModel {
			continue
		}
		text, err := s.sessionItemFullContent(id, item)
		if err != nil {
			return "", err
		}
		output = text
	}
	return output, nil
}

func (s *Service) CompactSession(ctx context.Context, id string) (SessionCompactResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || s.sessionStore == nil {
		return SessionCompactResult{}, fmt.Errorf("execution session store is not configured")
	}
	if s.compactPlanner == nil {
		return SessionCompactResult{}, fmt.Errorf("compact planner is not configured")
	}
	if _, err := s.sessionStore.LoadState(id); err != nil {
		return SessionCompactResult{}, err
	}

	lockCtx := ctx
	cancelLock := func() {}
	if s.sessionWriteLockTimeout > 0 {
		lockCtx, cancelLock = context.WithTimeout(ctx, s.sessionWriteLockTimeout)
	}
	writeLock, err := s.sessionStore.AcquireSessionWriteLock(lockCtx, id)
	cancelLock()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return SessionCompactResult{}, ErrSessionBusy
		}
		return SessionCompactResult{}, err
	}
	defer func() {
		_ = writeLock.Release()
	}()

	session, err := s.sessionStore.LoadExecutionState(id)
	if err != nil {
		return SessionCompactResult{}, err
	}
	if strings.TrimSpace(session.CWD) == "" {
		session.CWD = session.CreatedCWD
	}
	operationID := nextSessionCompactID(session)
	projector, err := sessionprojector.New(s.sessionStore, session)
	if err != nil {
		return SessionCompactResult{}, fmt.Errorf("could not start session projector")
	}
	defer projector.Close()

	bus := eventbus.NewBusWithCheckpoint(projector.CheckpointHandler())
	defer bus.Close()

	operationStarted := false
	operationClosed := false
	interruptOperation := func() {
		if !operationStarted || operationClosed {
			return
		}
		if err := bus.Publish(eventbus.TurnInterrupted{TurnID: operationID}); err != nil {
			_, _ = s.sessionStore.MarkTurnInterrupted(id, operationID)
		}
		operationClosed = true
	}

	if err := bus.Publish(eventbus.TurnStarted{TurnID: operationID}); err != nil {
		return SessionCompactResult{}, fmt.Errorf("could not mark compact running")
	}
	operationStarted = true

	session, err = s.sessionStore.LoadExecutionState(id)
	if err != nil {
		interruptOperation()
		return SessionCompactResult{}, err
	}
	session.ConfigPath = s.ConfigPath()
	if strings.TrimSpace(session.CWD) == "" {
		session.CWD = session.CreatedCWD
	}
	result, err := s.compactPlanner.PlanSessionCompaction(ctx, SessionCompactionRequest{
		Session:        session,
		SessionStore:   s.sessionStore,
		SessionService: s,
		RunCoordinator: s.sessionRunCoordinator(),
	})
	if err != nil {
		interruptOperation()
		return SessionCompactResult{}, ErrTurnFailed
	}
	if err := publishCompactionUsage(bus, result.Compaction.Usage); err != nil {
		interruptOperation()
		return SessionCompactResult{}, fmt.Errorf("could not publish compaction usage")
	}
	if err := bus.Publish(eventbus.CompactionRequested{
		TurnID:     operationID,
		Summary:    result.Compaction.SummaryItem,
		Checkpoint: result.Compaction.Checkpoint,
		Context:    result.Compaction.Context,
	}); err != nil {
		interruptOperation()
		return SessionCompactResult{}, fmt.Errorf("could not compact session")
	}
	if err := bus.Publish(eventbus.TurnCompleted{TurnID: operationID}); err != nil {
		interruptOperation()
		return SessionCompactResult{}, fmt.Errorf("could not clear compact turn")
	}
	operationClosed = true

	saved, err := s.sessionStore.LoadState(id)
	if err != nil {
		return SessionCompactResult{}, err
	}
	return SessionCompactResult{
		Status:        "committed",
		CompactionID:  result.Compaction.Checkpoint.ID,
		SummaryItemID: result.Compaction.SummaryItem.ID,
		LastSeq:       saved.LastSeq,
	}, nil
}

func (s *Service) startSessionEventBridge(sessionID, turnID, runID string, bus *eventbus.Bus, afterSeq int64, showReasoning bool, submit func(SessionStreamEvent)) func() {
	done := make(chan struct{})
	if s == nil || s.sessionStore == nil || bus == nil || submit == nil {
		close(done)
		return func() {}
	}
	events := bus.SubscribeLossless(64)
	go func() {
		defer close(done)
		lastSeq := afterSeq
		agentIteration := 0
		for event := range events {
			switch event := event.(type) {
			case eventbus.ModelEvent:
				if started, ok := event.Event.(model.AgentIterationStartedEvent); ok {
					agentIteration = started.Iteration
				}
				if streamEvent, ok := sessionStreamEventFromModelEvent(turnID, agentIteration, event.Event, showReasoning); ok {
					submit(streamEvent)
				}
			case eventbus.DurableCommitted:
				if started, ok := event.Event.(eventbus.TurnStarted); ok {
					turnID = started.TurnID
					agentIteration = 0
				}
				nextSeq := s.emitPersistedSessionEventsThrough(sessionID, runID, turnID, lastSeq, event.Seq, submit)
				if nextSeq > lastSeq {
					lastSeq = nextSeq
				}
			}
		}
	}()
	return func() {
		<-done
	}
}

func (s *Service) emitPersistedSessionEventsThrough(sessionID, runID, turnID string, afterSeq, throughSeq int64, submit func(SessionStreamEvent)) int64 {
	if s == nil || s.sessionStore == nil || submit == nil || throughSeq <= afterSeq {
		return afterSeq
	}
	events, err := s.sessionStore.PersistedEventsAfter(sessionID, afterSeq)
	if err != nil {
		return afterSeq
	}
	for _, event := range events {
		if event.Seq > throughSeq {
			break
		}
		streamEvent, ok := s.sessionStreamEventFromPersistedEvent(sessionID, runID, turnID, event, throughSeq)
		if !ok {
			continue
		}
		submit(streamEvent)
	}
	return throughSeq
}

func (s *Service) sessionStreamEventFromPersistedEvent(sessionID, runID, turnID string, event sessions.PersistedEvent, revision int64) (SessionStreamEvent, bool) {
	switch event.Type {
	case sessions.RecordTypeItemAppended, sessions.RecordTypeItemUpdated:
		item, err := s.committedPersistedItem(sessionID, event)
		if err != nil || !sessionItemVisibleInChat(item) {
			// Hidden/model-only/provider-private records still advance the
			// session watermark. They are deliberately absent from the
			// projection stream; run SSE IDs remain the transport gap signal.
			return nil, false
		}
		dto, err := s.sessionItemDTO(sessionID, item)
		if err != nil {
			return nil, false
		}
		fields := map[string]any{
			"session_id": sessionID,
			"seq":        event.Seq,
			"item_id":    item.ID,
			"revision":   strconv.FormatInt(revision, 10),
			"item":       dto,
		}
		resolvedTurnID := dto.TurnID
		if resolvedTurnID == "" {
			resolvedTurnID = turnID
		}
		if resolvedTurnID != "" {
			fields["turn_id"] = resolvedTurnID
		}
		if strings.TrimSpace(runID) != "" {
			fields["run_id"] = runID
		}
		if item.Message != nil && item.Message.Role == model.MessageRoleAssistant {
			// The public DTO may contain only a preview for blob-backed content;
			// this additive length lets the transient projection discard exactly
			// the committed prefix without inspecting or matching text.
			if content, err := s.sessionItemFullContent(sessionID, item); err == nil {
				fields["assistant_text_length"] = utf8.RuneCountInString(content)
			}
		}
		eventType := "item.appended"
		if event.Type == sessions.RecordTypeItemUpdated {
			eventType = "item.updated"
		}
		return NewSessionStreamEvent(eventType, fields), true
	case sessions.RecordTypeCompactionCreated:
		return NewSessionStreamEvent("compaction.created", map[string]any{
			"seq":           event.Seq,
			"compaction_id": event.CompactionID,
		}), true
	case sessions.RecordTypeActiveHistoryReplaced:
		return NewSessionStreamEvent("active_history.replaced", map[string]any{
			"seq": event.Seq,
		}), true
	default:
		return nil, false
	}
}

// committedPersistedItem returns the item as it was written by the durable
// record. Reading the event payload is important when the lossless bridge is
// briefly behind the writer: a later update must not make an earlier
// item.appended notification contain the later contents. Updates overwrite
// the event payload sequence with their record sequence, so restore the
// immutable creation sequence from the item projection.
func (s *Service) committedPersistedItem(sessionID string, event sessions.PersistedEvent) (sessions.SessionItem, error) {
	var item sessions.SessionItem
	if event.Item != nil {
		item = *event.Item
	} else {
		loaded, err := s.sessionStore.ReadItem(sessionID, event.ItemID)
		if err != nil {
			return sessions.SessionItem{}, err
		}
		item = loaded
	}
	if event.Type == sessions.RecordTypeItemUpdated {
		projected, err := s.sessionStore.ReadItem(sessionID, event.ItemID)
		if err != nil {
			return sessions.SessionItem{}, err
		}
		item.Seq = projected.Seq
	}
	return item, nil
}

func sessionStreamEventFromModelEvent(turnID string, agentIteration int, event model.Event, showReasoning bool) (SessionStreamEvent, bool) {
	switch event := event.(type) {
	case model.AgentIterationStartedEvent:
		if event.Iteration <= 0 {
			return nil, false
		}
		return modelSessionStreamEvent("agent.iteration.started", turnID, event.Iteration, nil), true
	case model.TextDeltaEvent:
		if event.Text == "" {
			return nil, false
		}
		fields := map[string]any{
			"text": event.Text,
		}
		if event.AssistantItemID != "" {
			fields["item_id"] = event.AssistantItemID
			fields["durable_text_length"] = event.DurableTextLength
			fields["durable_checkpointed"] = event.DurableCheckpointed
		}
		return modelSessionStreamEvent("text.delta", turnID, agentIteration, fields), true
	case model.ReasoningDeltaEvent:
		if !showReasoning || event.Text == "" {
			return nil, false
		}
		fields := map[string]any{"text": event.Text}
		if event.AssistantItemID != "" {
			fields["item_id"] = event.AssistantItemID
		}
		return modelSessionStreamEvent("reasoning.delta", turnID, agentIteration, fields), true
	case model.ToolCallDoneEvent:
		if event.ToolCall.Name == "" {
			return nil, false
		}
		return modelSessionStreamEvent("tool.requested", turnID, agentIteration, map[string]any{
			"tool_call_id": event.ToolCall.ID,
			"name":         event.ToolCall.Name,
			"arguments":    sessionToolDisplayArguments(event.ToolCall.Name, event.ToolCall.Arguments),
		}), true
	case model.ToolStartedEvent:
		if event.ToolCall.Name == "" {
			return nil, false
		}
		return modelSessionStreamEvent("tool.started", turnID, agentIteration, map[string]any{
			"tool_call_id": event.ToolCall.ID,
			"name":         event.ToolCall.Name,
			"arguments":    sessionToolDisplayArguments(event.ToolCall.Name, event.ToolCall.Arguments),
		}), true
	case model.ToolResultEvent:
		if event.Result.Name == "" {
			return nil, false
		}
		return modelSessionStreamEvent("tool.finished", turnID, agentIteration, map[string]any{
			"tool_call_id": event.Result.ToolCallID,
			"name":         event.Result.Name,
			"is_error":     event.Result.IsError,
			"content":      sessionStreamToolContent(event.Result.Content),
		}), true
	case model.UsageEvent:
		return modelSessionStreamEvent("usage.updated", turnID, agentIteration, map[string]any{
			"input_tokens":       event.Usage.InputTokens,
			"output_tokens":      event.Usage.OutputTokens,
			"total_tokens":       event.Usage.TotalTokens,
			"cached_tokens":      event.Usage.CachedTokens,
			"cache_write_tokens": event.Usage.CacheWriteTokens,
			"reasoning_tokens":   event.Usage.ReasoningTokens,
		}), true
	case model.ProviderRetryEvent:
		return modelSessionStreamEvent("provider.retrying", turnID, agentIteration, map[string]any{
			"attempt":      event.Attempt,
			"max_attempts": event.MaxAttempts,
			"delay_ms":     event.Delay.Milliseconds(),
			"reason":       event.Reason,
		}), true
	default:
		return nil, false
	}
}

func modelSessionStreamEvent(eventType, turnID string, agentIteration int, fields map[string]any) SessionStreamEvent {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["turn_id"] = turnID
	if agentIteration > 0 {
		fields["agent_iteration"] = agentIteration
	}
	return NewSessionStreamEvent(eventType, fields)
}

func (s *Service) sessionItemDTO(sessionID string, item sessions.SessionItem) (SessionItem, error) {
	state, err := s.sessionStore.LoadState(sessionID)
	if err != nil {
		return SessionItem{}, err
	}
	return s.sessionItemDTOWithReasoning(sessionID, item, state.ShowReasoning)
}

func (s *Service) sessionItemDTOWithReasoning(sessionID string, item sessions.SessionItem, showReasoning bool) (SessionItem, error) {
	dto := SessionItem{
		Seq:            item.Seq,
		ID:             item.ID,
		TurnID:         item.TurnID,
		AgentIteration: item.AgentIteration,
		CreatedAt:      item.CreatedAt,
		Kind:           item.Kind,
		Visibility:     item.Visibility,
		Audience:       item.Audience,
		Status:         item.Status,
	}
	if item.Message == nil {
		return dto, nil
	}
	dto.Message = &SessionItemMessage{
		Role:       string(item.Message.Role),
		ToolCallID: item.Message.ToolCallID,
		IsError:    item.Message.IsError,
	}
	if showReasoning && item.Message.Role == model.MessageRoleAssistant {
		dto.Message.Reasoning = item.Message.ReasoningContent
	}
	if len(item.Message.ToolCalls) > 0 {
		dto.Message.ToolCalls = make([]SessionItemToolCall, 0, len(item.Message.ToolCalls))
		for _, toolCall := range item.Message.ToolCalls {
			dto.Message.ToolCalls = append(dto.Message.ToolCalls, SessionItemToolCall{
				ID:        toolCall.ID,
				Name:      toolCall.Name,
				Arguments: sessionToolDisplayArguments(toolCall.Name, toolCall.Arguments),
			})
		}
	}
	content, preview, err := s.sessionItemDisplayContent(sessionID, item)
	if err != nil {
		return SessionItem{}, err
	}
	if content != "" || preview != "" {
		dto.Message.Content = &SessionItemMessageContent{
			Inline:  content,
			Preview: preview,
		}
	}
	dto.Message.Images = sessionItemImageAttachments(item)
	return dto, nil
}

func sessionItemImageAttachments(item sessions.SessionItem) []SessionItemImageAttachment {
	if item.Message == nil {
		return nil
	}
	attachments := make([]SessionItemImageAttachment, 0, len(item.Message.ContentBlocks))
	for _, block := range item.Message.ContentBlocks {
		if block.Type != "input_image" || block.ImageBlob == nil {
			continue
		}
		mediaType, supported := model.NormalizeImageMediaType(block.ImageBlob.MediaType)
		if !supported || strings.TrimSpace(block.ImageBlob.Hash) == "" || block.ImageBlob.SizeBytes <= 0 {
			continue
		}
		attachments = append(attachments, SessionItemImageAttachment{
			Hash:      block.ImageBlob.Hash,
			MediaType: mediaType,
			SizeBytes: block.ImageBlob.SizeBytes,
		})
	}
	if len(attachments) == 0 {
		return nil
	}
	return attachments
}

func (s *Service) sessionItemDisplayContent(sessionID string, item sessions.SessionItem) (string, string, error) {
	if item.Message != nil {
		if item.Message.Content != "" {
			return item.Message.Content, "", nil
		}
		if text := inputContentBlockText(item.Message.ContentBlocks); text != "" {
			return text, "", nil
		}
	}
	if item.Content == nil {
		return "", "", nil
	}
	if item.Content.Inline != "" {
		return item.Content.Inline, "", nil
	}
	if item.Content.Blob != nil && item.Message != nil && (item.Message.Role == model.MessageRoleUser || item.Message.Role == model.MessageRoleAssistant) {
		content, err := s.sessionItemFullContent(sessionID, item)
		if err != nil {
			return "", "", err
		}
		return content, "", nil
	}
	if item.Content.Preview != "" {
		return "", item.Content.Preview, nil
	}
	return "", "", nil
}

func (s *Service) sessionItemFullContent(sessionID string, item sessions.SessionItem) (string, error) {
	if item.Message != nil {
		if item.Message.Content != "" {
			return item.Message.Content, nil
		}
		if text := inputContentBlockText(item.Message.ContentBlocks); text != "" {
			return text, nil
		}
	}
	if item.Content == nil {
		return "", nil
	}
	if item.Content.Inline != "" {
		return item.Content.Inline, nil
	}
	if item.Content.Blob != nil {
		raw, err := s.sessionStore.ReadBlobForSession(sessionID, *item.Content.Blob)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
	if item.Content.Preview != "" {
		return item.Content.Preview, nil
	}
	return "", nil
}

func inputContentBlockText(blocks []model.InputContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if (block.Type == "" || block.Type == "input_text") && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func sessionItemVisibleInChat(item sessions.SessionItem) bool {
	if item.Visibility != sessions.ItemVisibilityVisible || item.Message == nil {
		return false
	}
	// Visible compaction records are the durable "context compacted" divider
	// in the chat timeline. Hidden model-audience compaction payloads (the
	// summary and remote provider items) stay out of the chat.
	if item.Kind == sessions.ItemKindCompaction {
		return item.Audience == sessions.ItemAudienceUser
	}
	if item.Kind != sessions.ItemKindMessage {
		return false
	}
	switch item.Message.Role {
	case model.MessageRoleUser:
		return item.Audience == sessions.ItemAudienceUser
	case model.MessageRoleAssistant:
		return item.Audience == sessions.ItemAudienceModel
	case model.MessageRoleTool:
		return item.Audience == sessions.ItemAudienceModel
	default:
		return false
	}
}

func sessionStreamToolContent(content string) string {
	runes := []rune(content)
	if len(runes) <= maxSessionStreamToolContentRunes {
		return content
	}
	return string(runes[:maxSessionStreamToolContentRunes]) + "\n…"
}

// sessionOrchestrationToolNames are the session_* tools. Their arguments are
// displayed in full in the UI: collapsed rows summarize the call (target
// session, model, message snippet) and expanded rows show the exact request
// payload, so none of their fields are filtered out.
var sessionOrchestrationToolNames = map[string]bool{
	"session_models":  true,
	"session_start":   true,
	"session_search":  true,
	"session_get":     true,
	"session_history": true,
	"session_send":    true,
	"session_wait":    true,
	"session_stop":    true,
}

func sessionToolDisplayArguments(name, arguments string) string {
	var parsed map[string]any
	if json.Unmarshal([]byte(arguments), &parsed) != nil {
		return ""
	}
	if sessionOrchestrationToolNames[name] {
		return arguments
	}
	displayed := make(map[string]any)
	keys := []string{"path", "pattern", "query", "start_line", "line_count"}
	if name == "shell" {
		keys = []string{"command"}
	} else if name == "edit_file" {
		keys = []string{"path", "old", "new"}
	} else if name == "apply_patch" {
		keys = []string{"patch"}
	} else if name == "grep_files" {
		keys = []string{"path", "query", "literal", "case_sensitive"}
	}
	for _, key := range keys {
		if value, ok := parsed[key]; ok {
			displayed[key] = value
		}
	}
	if len(displayed) == 0 {
		return ""
	}
	raw, err := json.Marshal(displayed)
	if err != nil {
		return ""
	}
	return string(raw)
}

func recentSessionItems(items []sessions.SessionItem, limit int) ([]sessions.SessionItem, bool) {
	if limit <= 0 || len(items) <= limit {
		return items, false
	}
	return items[len(items)-limit:], true
}

func pagedSessionItems(items []sessions.SessionItem, beforeSeq, afterSeq int64, limit int, alignTurn bool) ([]sessions.SessionItem, bool, bool) {
	start := 0
	end := len(items)
	if beforeSeq > 0 {
		for end > 0 && items[end-1].Seq >= beforeSeq {
			end--
		}
	} else if afterSeq > 0 {
		for start < len(items) && items[start].Seq <= afterSeq {
			start++
		}
	}

	if afterSeq > 0 {
		pageEnd := end
		if pageEnd-start > limit {
			pageEnd = start + limit
		}
		return items[start:pageEnd], start > 0, pageEnd < len(items)
	}
	pageStart := start
	if end-pageStart > limit {
		pageStart = end - limit
	}
	if alignTurn {
		// Extend the oldest edge backwards until the page starts at a turn
		// boundary, so a latest/before page never cuts a turn and always
		// contains at least one complete turn. Items without a turn id are
		// boundaries of their own and never trigger extension.
		for pageStart > 0 && items[pageStart].TurnID != "" && items[pageStart-1].TurnID == items[pageStart].TurnID {
			pageStart--
		}
	}
	return items[pageStart:end], pageStart > 0, end < len(items)
}

func sessionItemSeqBounds(items []sessions.SessionItem) (int64, int64) {
	if len(items) == 0 {
		return 0, 0
	}
	return items[0].Seq, items[len(items)-1].Seq
}
