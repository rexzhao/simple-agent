package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessionprojector"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

const defaultSessionChatItemsLimit = 50

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
	Role       string                     `json:"role"`
	Content    *SessionItemMessageContent `json:"content,omitempty"`
	ToolCallID string                     `json:"tool_call_id,omitempty"`
	ToolCalls  []SessionItemToolCall      `json:"tool_calls,omitempty"`
	IsError    bool                       `json:"is_error,omitempty"`
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
	event          SessionStreamEvent // verbatim event for non-delta ops
	isDelta        bool
	eventType      string
	turnID         string
	agentIteration int
	text           *strings.Builder // accumulator for delta ops
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
		if n := len(s.ops); n > 0 {
			last := &s.ops[n-1]
			if last.isDelta && last.eventType == eventType && last.turnID == turnID && last.agentIteration == agentIteration {
				last.text.WriteString(text)
				s.mu.Unlock()
				return
			}
		}
		builder := &strings.Builder{}
		builder.WriteString(text)
		s.ops = append(s.ops, sinkOp{
			isDelta:        true,
			eventType:      eventType,
			turnID:         turnID,
			agentIteration: agentIteration,
			text:           builder,
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
	if limit > 200 {
		return SessionItemsPage{}, fmt.Errorf("session item limit cannot exceed 200")
	}
	session, err := s.sessionStore.Load(id)
	if err != nil {
		return SessionItemsPage{}, err
	}

	filtered := make([]sessions.SessionItem, 0, len(session.Items))
	for _, item := range session.Items {
		if sessionItemVisibleInChat(item) {
			filtered = append(filtered, item)
		}
	}
	page, hasMoreBefore, hasMoreAfter := pagedSessionItems(filtered, options.BeforeSeq, options.AfterSeq, limit)
	items := make([]SessionItem, 0, len(page))
	for _, item := range page {
		dto, err := s.sessionItemDTO(item)
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

func (s *Service) GetSessionTurnFinalAssistantOutput(id, turnID string) (string, error) {
	if s == nil || s.sessionStore == nil {
		return "", fmt.Errorf("execution session store is not configured")
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return "", fmt.Errorf("turn id is required")
	}
	session, err := s.sessionStore.Load(id)
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
		text, err := s.sessionItemFullContent(item)
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
	if _, err := s.sessionStore.Load(id); err != nil {
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

	session, err := s.sessionStore.Load(id)
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

	session, err = s.sessionStore.Load(id)
	if err != nil {
		interruptOperation()
		return SessionCompactResult{}, err
	}
	session.ConfigPath = s.ConfigPath()
	if strings.TrimSpace(session.CWD) == "" {
		session.CWD = session.CreatedCWD
	}
	result, err := s.compactPlanner.PlanSessionCompaction(ctx, SessionCompactionRequest{
		Session:      session,
		SessionStore: s.sessionStore,
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

	saved, err := s.sessionStore.Load(id)
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

func (s *Service) startSessionEventBridge(sessionID, turnID string, bus *eventbus.Bus, afterSeq int64, showReasoning bool, submit func(SessionStreamEvent)) func() {
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
				nextSeq := s.emitPersistedSessionEventsThrough(sessionID, lastSeq, event.Seq, submit)
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

func (s *Service) emitPersistedSessionEventsThrough(sessionID string, afterSeq, throughSeq int64, submit func(SessionStreamEvent)) int64 {
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
		streamEvent, ok := sessionStreamEventFromPersistedEvent(event)
		if !ok {
			continue
		}
		submit(streamEvent)
	}
	return throughSeq
}

func sessionStreamEventFromPersistedEvent(event sessions.PersistedEvent) (SessionStreamEvent, bool) {
	switch event.Type {
	case sessions.RecordTypeItemAppended:
		return NewSessionStreamEvent("item.appended", map[string]any{
			"seq":     event.Seq,
			"item_id": event.ItemID,
		}), true
	case sessions.RecordTypeItemUpdated:
		return NewSessionStreamEvent("item.updated", map[string]any{
			"seq":     event.Seq,
			"item_id": event.ItemID,
		}), true
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
		return modelSessionStreamEvent("text.delta", turnID, agentIteration, map[string]any{
			"text": event.Text,
		}), true
	case model.ReasoningDeltaEvent:
		if !showReasoning || event.Text == "" {
			return nil, false
		}
		return modelSessionStreamEvent("reasoning.delta", turnID, agentIteration, map[string]any{
			"text": event.Text,
		}), true
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

func (s *Service) sessionItemDTO(item sessions.SessionItem) (SessionItem, error) {
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
	content, preview, err := s.sessionItemDisplayContent(item)
	if err != nil {
		return SessionItem{}, err
	}
	if content != "" || preview != "" {
		dto.Message.Content = &SessionItemMessageContent{
			Inline:  content,
			Preview: preview,
		}
	}
	return dto, nil
}

func (s *Service) sessionItemDisplayContent(item sessions.SessionItem) (string, string, error) {
	if item.Message != nil && item.Message.Content != "" {
		return item.Message.Content, "", nil
	}
	if item.Content == nil {
		return "", "", nil
	}
	if item.Content.Inline != "" {
		return item.Content.Inline, "", nil
	}
	if item.Content.Blob != nil && item.Message != nil && (item.Message.Role == model.MessageRoleUser || item.Message.Role == model.MessageRoleAssistant) {
		content, err := s.sessionItemFullContent(item)
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

func (s *Service) sessionItemFullContent(item sessions.SessionItem) (string, error) {
	if item.Message != nil && item.Message.Content != "" {
		return item.Message.Content, nil
	}
	if item.Content == nil {
		return "", nil
	}
	if item.Content.Inline != "" {
		return item.Content.Inline, nil
	}
	if item.Content.Blob != nil {
		raw, err := s.sessionStore.ReadBlob(*item.Content.Blob)
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

func sessionItemVisibleInChat(item sessions.SessionItem) bool {
	if item.Visibility != sessions.ItemVisibilityVisible || item.Message == nil {
		return false
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

func sessionToolDisplayArguments(name, arguments string) string {
	var parsed map[string]any
	if json.Unmarshal([]byte(arguments), &parsed) != nil {
		return ""
	}
	displayed := make(map[string]any)
	keys := []string{"path", "pattern", "query", "start_line", "line_count"}
	if name == "shell" {
		keys = []string{"command"}
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

func pagedSessionItems(items []sessions.SessionItem, beforeSeq, afterSeq int64, limit int) ([]sessions.SessionItem, bool, bool) {
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
	return items[pageStart:end], pageStart > 0, end < len(items)
}

func sessionItemSeqBounds(items []sessions.SessionItem) (int64, int64) {
	if len(items) == 0 {
		return 0, 0
	}
	return items[0].Seq, items[len(items)-1].Seq
}
