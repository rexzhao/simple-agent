// Package sessionprojector translates durable turn events into session writes.
//
// A Projector assumes its caller has already enforced the session-level turn
// lock so only one projector writes a session at a time. The initial session
// must already have persisted metadata; TurnStarted uses that metadata record.
package sessionprojector

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type Projector struct {
	store *sessions.V2Store

	mu       sync.Mutex
	closed   bool
	done     chan struct{}
	requests chan projectorRequest
	wg       sync.WaitGroup

	session        sessions.SessionV2
	turnID         string
	finished       bool
	toolItems      map[string]string
	pendingItemIDs map[string]struct{}
}

type projectorRequest struct {
	event eventbus.Event
	ack   chan projectorResult
}

type projectorResult struct {
	seq int64
	err error
}

func New(store *sessions.V2Store, session sessions.SessionV2) (*Projector, error) {
	if store == nil {
		return nil, fmt.Errorf("session store is required")
	}
	if strings.TrimSpace(session.ID) == "" {
		return nil, fmt.Errorf("session id is required")
	}
	projector := &Projector{
		store:          store,
		done:           make(chan struct{}),
		requests:       make(chan projectorRequest),
		session:        session,
		toolItems:      make(map[string]string),
		pendingItemIDs: make(map[string]struct{}),
	}
	projector.wg.Add(1)
	go projector.run()
	return projector, nil
}

// Handler returns a durable event handler for eventbus.NewBus. The handler is
// synchronous: it waits until the projector goroutine writes the event and
// acknowledges the result.
func (p *Projector) Handler() eventbus.DurableHandler {
	return p.Handle
}

func (p *Projector) Handle(event eventbus.Event) error {
	_, err := p.HandleWithCheckpoint(event)
	return err
}

func (p *Projector) CheckpointHandler() eventbus.DurableCheckpointHandler {
	return p.HandleWithCheckpoint
}

func (p *Projector) HandleWithCheckpoint(event eventbus.Event) (int64, error) {
	if p == nil {
		return 0, fmt.Errorf("session projector is required")
	}
	if event == nil {
		return 0, fmt.Errorf("event is required")
	}
	if _, ok := event.(eventbus.DurableEvent); !ok {
		return 0, fmt.Errorf("session projector only handles durable events: %T", event)
	}

	ack := make(chan projectorResult, 1)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, eventbus.ErrClosed
	}
	p.requests <- projectorRequest{event: event, ack: ack}
	p.mu.Unlock()
	result := <-ack
	return result.seq, result.err
}

func (p *Projector) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.done)
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

func (p *Projector) run() {
	defer p.wg.Done()
	for {
		select {
		case request := <-p.requests:
			seq, err := p.handle(request.event)
			request.ack <- projectorResult{seq: seq, err: err}
		case <-p.done:
			return
		}
	}
}

func (p *Projector) handle(event eventbus.Event) (int64, error) {
	if p.finished {
		return 0, fmt.Errorf("turn %q already finished", p.turnID)
	}
	var err error
	switch event := event.(type) {
	case eventbus.TurnStarted:
		err = p.handleTurnStarted(event)
	case eventbus.CompactionRequested:
		err = p.handleCompactionRequested(event)
	case eventbus.TurnInputReady:
		err = p.handleTurnInputReady(event)
	case eventbus.AssistantReady:
		err = p.handleAssistantReady(event)
	case eventbus.ToolResultReady:
		err = p.handleToolResultReady(event)
	case eventbus.TurnCompleted:
		err = p.handleTurnCompleted(event)
	case eventbus.TurnInterrupted:
		err = p.handleTurnInterrupted(event)
	default:
		err = fmt.Errorf("unsupported projector event %T", event)
	}
	if err != nil {
		return 0, err
	}
	return p.session.LastSeq, nil
}

func (p *Projector) handleTurnStarted(event eventbus.TurnStarted) error {
	turnID, err := normalizeTurnID(event.TurnID)
	if err != nil {
		return err
	}
	if p.turnID != "" {
		return fmt.Errorf("turn %q already started", p.turnID)
	}
	metadata, err := p.store.MarkTurnRunning(p.session.ID, turnID)
	if err != nil {
		return err
	}
	p.session = mergeSessionMetadata(p.session, metadata)
	p.turnID = turnID
	return nil
}

func (p *Projector) handleCompactionRequested(event eventbus.CompactionRequested) error {
	if err := p.requireActiveTurn(event.TurnID); err != nil {
		return err
	}
	next, err := p.store.SaveCompactedTurn(p.session, event.Summary, event.Checkpoint, nil, event.Checkpoint.ReplacementHistory)
	if err != nil {
		return err
	}
	p.session = next
	return nil
}

func (p *Projector) handleTurnInputReady(event eventbus.TurnInputReady) error {
	turnID, err := p.requireActiveTurnID(event.TurnID)
	if err != nil {
		return err
	}
	if event.Message.Role != model.MessageRoleUser {
		return fmt.Errorf("turn input message role must be %q", model.MessageRoleUser)
	}
	existing := sessions.SessionItemIDs(p.session.Items)
	items := make([]sessions.SessionItem, 0, len(p.session.InstructionsSnapshot)+1)
	activeHistory := copyStrings(p.session.ActiveHistory)
	if len(activeHistory) == 0 {
		for _, message := range p.session.InstructionsSnapshot {
			itemID := sessions.NextSessionItemID(existing, message)
			item := sessions.SessionItemFromMessage(itemID, message)
			existing[itemID] = struct{}{}
			items = append(items, item)
			activeHistory = append(activeHistory, itemID)
		}
	}

	itemID := sessions.NextSessionItemID(existing, event.Message)
	item := sessions.SessionItemFromMessage(itemID, event.Message)
	item.TurnID = turnID
	items = append(items, item)
	activeHistory = append(activeHistory, itemID)

	next, err := p.store.AppendItemsAndReplaceActiveHistoryFromState(p.session.ID, p.session, items, activeHistory)
	if err != nil {
		return err
	}
	p.session = next
	return nil
}

func (p *Projector) handleAssistantReady(event eventbus.AssistantReady) error {
	turnID, err := p.requireActiveTurnID(event.TurnID)
	if err != nil {
		return err
	}
	if event.Message.Role != model.MessageRoleAssistant {
		return fmt.Errorf("assistant message role must be %q", model.MessageRoleAssistant)
	}

	existing := sessions.SessionItemIDs(p.session.Items)
	items := make([]sessions.SessionItem, 0, 1+len(event.Message.ToolCalls))
	activeHistory := copyStrings(p.session.ActiveHistory)
	seenToolCallIDs := make(map[string]struct{}, len(event.Message.ToolCalls))
	toolItemAdds := make(map[string]string, len(event.Message.ToolCalls))
	pendingItemAdds := make([]string, 0, len(event.Message.ToolCalls))

	assistantID := sessions.NextSessionItemID(existing, event.Message)
	existing[assistantID] = struct{}{}
	assistantItem := sessions.SessionItemFromMessage(assistantID, event.Message)
	assistantItem.TurnID = turnID
	items = append(items, assistantItem)
	activeHistory = append(activeHistory, assistantID)

	for _, toolCall := range event.Message.ToolCalls {
		if strings.TrimSpace(toolCall.ID) == "" {
			return fmt.Errorf("assistant tool call id is required")
		}
		if _, ok := seenToolCallIDs[toolCall.ID]; ok {
			return fmt.Errorf("assistant tool call %q is duplicated", toolCall.ID)
		}
		seenToolCallIDs[toolCall.ID] = struct{}{}
		if _, ok := p.toolItems[toolCall.ID]; ok {
			return fmt.Errorf("tool call %q already has a session item", toolCall.ID)
		}
		message := model.Message{
			Role:       model.MessageRoleTool,
			ToolCallID: toolCall.ID,
		}
		itemID := sessions.NextSessionItemID(existing, message)
		existing[itemID] = struct{}{}
		item := sessions.SessionItemFromMessage(itemID, message)
		item.TurnID = turnID
		item.Status = sessions.ItemStatusPending
		items = append(items, item)
		activeHistory = append(activeHistory, itemID)
		toolItemAdds[toolCall.ID] = itemID
		pendingItemAdds = append(pendingItemAdds, itemID)
	}

	next, err := p.store.AppendItemsAndReplaceActiveHistoryFromState(p.session.ID, p.session, items, activeHistory)
	if err != nil {
		return err
	}
	p.session = next
	for toolCallID, itemID := range toolItemAdds {
		p.toolItems[toolCallID] = itemID
	}
	for _, itemID := range pendingItemAdds {
		p.pendingItemIDs[itemID] = struct{}{}
	}
	return nil
}

func (p *Projector) handleToolResultReady(event eventbus.ToolResultReady) error {
	if err := p.requireActiveTurn(event.TurnID); err != nil {
		return err
	}
	if strings.TrimSpace(event.Result.ToolCallID) == "" {
		return fmt.Errorf("tool result tool call id is required")
	}
	itemID, ok := p.toolItems[event.Result.ToolCallID]
	if !ok {
		return fmt.Errorf("tool result references unknown tool call %q", event.Result.ToolCallID)
	}
	if _, ok := p.pendingItemIDs[itemID]; !ok {
		return fmt.Errorf("tool call %q is not pending", event.Result.ToolCallID)
	}

	status := sessions.ItemStatusCompleted
	if event.Result.IsError {
		status = sessions.ItemStatusError
	}
	message := model.Message{
		Role:       model.MessageRoleTool,
		Content:    event.Result.Content,
		ToolCallID: event.Result.ToolCallID,
		IsError:    event.Result.IsError,
	}
	_, next, err := p.store.UpdateItemFromState(p.session.ID, p.session, sessions.SessionItem{
		ID:      itemID,
		Status:  status,
		Message: &message,
	})
	if err != nil {
		return err
	}
	p.session = next
	delete(p.pendingItemIDs, itemID)
	return nil
}

func (p *Projector) handleTurnCompleted(event eventbus.TurnCompleted) error {
	turnID, err := p.requireActiveTurnID(event.TurnID)
	if err != nil {
		return err
	}
	if len(p.pendingItemIDs) > 0 {
		return fmt.Errorf("turn %q has pending tool items", turnID)
	}
	metadata, err := p.store.ClearRunningTurn(p.session.ID, turnID)
	if err != nil {
		return err
	}
	p.session = mergeSessionMetadata(p.session, metadata)
	p.finished = true
	return nil
}

func (p *Projector) handleTurnInterrupted(event eventbus.TurnInterrupted) error {
	turnID, err := p.requireActiveTurnID(event.TurnID)
	if err != nil {
		return err
	}
	pending := make([]string, 0, len(p.pendingItemIDs))
	for itemID := range p.pendingItemIDs {
		pending = append(pending, itemID)
	}
	sort.Strings(pending)
	for _, itemID := range pending {
		item, ok := sessionItemByID(p.session.Items, itemID)
		if !ok {
			return fmt.Errorf("pending tool item %q is missing from cached state", itemID)
		}
		_, next, err := p.store.UpdateItemFromState(p.session.ID, p.session, sessions.SessionItem{
			ID:      itemID,
			Status:  sessions.ItemStatusInterrupted,
			Message: item.Message,
			Content: item.Content,
		})
		if err != nil {
			return err
		}
		p.session = next
		delete(p.pendingItemIDs, itemID)
	}
	metadata, err := p.store.MarkTurnInterrupted(p.session.ID, turnID)
	if err != nil {
		return err
	}
	p.session = mergeSessionMetadata(p.session, metadata)
	p.finished = true
	return nil
}

func (p *Projector) requireActiveTurn(turnID string) error {
	_, err := p.requireActiveTurnID(turnID)
	return err
}

func (p *Projector) requireActiveTurnID(turnID string) (string, error) {
	turnID, err := normalizeTurnID(turnID)
	if err != nil {
		return "", err
	}
	if p.turnID == "" {
		return "", fmt.Errorf("turn has not started")
	}
	if p.turnID != turnID {
		return "", fmt.Errorf("event turn %q does not match active turn %q", turnID, p.turnID)
	}
	return turnID, nil
}

func normalizeTurnID(turnID string) (string, error) {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return "", fmt.Errorf("turn id is required")
	}
	return turnID, nil
}

func mergeSessionMetadata(state, metadata sessions.SessionV2) sessions.SessionV2 {
	metadata.Items = state.Items
	metadata.ActiveHistory = state.ActiveHistory
	metadata.Compactions = state.Compactions
	metadata.LastSeq = state.LastSeq
	return metadata
}

func sessionItemByID(items []sessions.SessionItem, id string) (sessions.SessionItem, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return sessions.SessionItem{}, false
}

func copyStrings(values []string) []string {
	return append([]string(nil), values...)
}
