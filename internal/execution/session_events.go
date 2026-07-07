package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessionprojector"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

const defaultSessionChatItemsLimit = 50

type SessionItemsPage struct {
	Items         []SessionItem
	OldestSeq     int64
	NewestSeq     int64
	HasMoreBefore bool
	HasMoreAfter  bool
}

type SessionItem struct {
	Seq        int64
	ID         string
	TurnID     string
	Kind       string
	Visibility string
	Audience   string
	Message    *SessionItemMessage
}

type SessionItemMessage struct {
	Role    string
	Content *SessionItemMessageContent
}

type SessionItemMessageContent struct {
	Inline  string
	Preview string
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

func (s *Service) GetSessionChatItems(id string) (SessionItemsPage, error) {
	if s == nil || s.sessionStore == nil {
		return SessionItemsPage{}, fmt.Errorf("execution session store is not configured")
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
	page, hasMoreBefore := recentSessionItems(filtered, defaultSessionChatItemsLimit)
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
		HasMoreAfter:  false,
	}, nil
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
	if err := bus.Publish(eventbus.CompactionRequested{
		TurnID:     operationID,
		Summary:    result.Compaction.SummaryItem,
		Checkpoint: result.Compaction.Checkpoint,
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

func (s *Service) startSessionEventBridge(sessionID, turnID string, bus *eventbus.Bus, afterSeq int64, emit func(SessionStreamEvent)) func() {
	done := make(chan struct{})
	if s == nil || s.sessionStore == nil || bus == nil || emit == nil {
		close(done)
		return func() {}
	}
	events := bus.SubscribeLossless(64)
	go func() {
		defer close(done)
		lastSeq := afterSeq
		for event := range events {
			switch event := event.(type) {
			case eventbus.ModelEvent:
				if streamEvent, ok := sessionStreamEventFromModelEvent(turnID, event.Event); ok {
					emitSessionStreamEvent(emit, streamEvent)
				}
			case eventbus.DurableCommitted:
				nextSeq := s.emitPersistedSessionEventsThrough(sessionID, lastSeq, event.Seq, emit)
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

func (s *Service) emitPersistedSessionEventsThrough(sessionID string, afterSeq, throughSeq int64, emit func(SessionStreamEvent)) int64 {
	if s == nil || s.sessionStore == nil || emit == nil || throughSeq <= afterSeq {
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
		emitSessionStreamEvent(emit, streamEvent)
	}
	return throughSeq
}

func emitSessionStreamEvent(emit func(SessionStreamEvent), event SessionStreamEvent) {
	if emit == nil || event == nil {
		return
	}
	emit(event)
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

func sessionStreamEventFromModelEvent(turnID string, event model.Event) (SessionStreamEvent, bool) {
	switch event := event.(type) {
	case model.TextDeltaEvent:
		if event.Text == "" {
			return nil, false
		}
		return NewSessionStreamEvent("text.delta", map[string]any{
			"turn_id": turnID,
			"text":    event.Text,
		}), true
	case model.ToolCallDoneEvent:
		if event.ToolCall.Name == "" {
			return nil, false
		}
		return NewSessionStreamEvent("tool.started", map[string]any{
			"turn_id":      turnID,
			"tool_call_id": event.ToolCall.ID,
			"name":         event.ToolCall.Name,
		}), true
	case model.ToolResultEvent:
		if event.Result.Name == "" {
			return nil, false
		}
		return NewSessionStreamEvent("tool.finished", map[string]any{
			"turn_id":      turnID,
			"tool_call_id": event.Result.ToolCallID,
			"name":         event.Result.Name,
			"is_error":     event.Result.IsError,
		}), true
	default:
		return nil, false
	}
}

func (s *Service) sessionItemDTO(item sessions.SessionItem) (SessionItem, error) {
	dto := SessionItem{
		Seq:        item.Seq,
		ID:         item.ID,
		TurnID:     item.TurnID,
		Kind:       item.Kind,
		Visibility: item.Visibility,
		Audience:   item.Audience,
	}
	if item.Message == nil {
		return dto, nil
	}
	dto.Message = &SessionItemMessage{
		Role: string(item.Message.Role),
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
	if item.Content.Preview != "" {
		return "", item.Content.Preview, nil
	}
	return "", "", nil
}

func sessionItemVisibleInChat(item sessions.SessionItem) bool {
	if item.Kind != sessions.ItemKindMessage || item.Visibility != sessions.ItemVisibilityVisible || item.Message == nil {
		return false
	}
	switch item.Message.Role {
	case model.MessageRoleUser:
		return item.Audience == sessions.ItemAudienceUser
	case model.MessageRoleAssistant:
		return item.Audience == sessions.ItemAudienceModel
	default:
		return false
	}
}

func recentSessionItems(items []sessions.SessionItem, limit int) ([]sessions.SessionItem, bool) {
	if limit <= 0 || len(items) <= limit {
		return items, false
	}
	return items[len(items)-limit:], true
}

func sessionItemSeqBounds(items []sessions.SessionItem) (int64, int64) {
	if len(items) == 0 {
		return 0, 0
	}
	return items[0].Seq, items[len(items)-1].Seq
}
