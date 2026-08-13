// Package sessionprojector translates durable turn events into session writes.
//
// A Projector assumes its caller has already enforced the session-level turn
// lock so only one projector writes a session at a time. The initial session
// must already have persisted metadata; TurnStarted uses that metadata record.
package sessionprojector

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type Projector struct {
	store projectorStore

	mu       sync.Mutex
	closed   bool
	done     chan struct{}
	requests chan projectorRequest
	wg       sync.WaitGroup

	session        sessions.SessionV2
	runID          string
	turnID         string
	finished       bool
	toolItems      map[string]string
	assistantItems map[assistantItemKey]string
	pendingItemIDs map[string]struct{}
}

// assistantItemKey identifies one logical model message. Agent iterations are
// scoped to a turn: a queued/follow-up prompt may start a new turn whose first
// invocation is iteration one again. Keeping the turn in this key prevents a
// later turn from being mistaken for a continuation of the earlier message.
type assistantItemKey struct {
	turnID    string
	iteration int
}

// RunProjector routes durable events for a multi-request Run to one projector
// per model request. A model/tool iteration therefore gets its own Turn row,
// while the item projection and active history remain unchanged.
type RunProjector struct {
	store    projectorStore
	runID    string
	mu       sync.Mutex
	current  sessions.SessionV2
	byTurnID map[string]*Projector
}

type projectorStore interface {
	MarkTurnRunning(sessionID, turnID string) (sessions.SessionV2, error)
	SaveCompactedTurn(session sessions.SessionV2, summaryItem sessions.SessionItem, checkpoint sessions.CompactionCheckpoint, items []sessions.SessionItem, activeHistory []string) (sessions.SessionV2, error)
	AppendItemsAndReplaceActiveHistoryFromState(sessionID string, state sessions.SessionV2, items []sessions.SessionItem, itemIDs []string) (sessions.SessionV2, error)
	UpdateItemFromState(sessionID string, state sessions.SessionV2, item sessions.SessionItem) (sessions.SessionItem, sessions.SessionV2, error)
	ClearRunningTurn(sessionID, turnID string) (sessions.SessionV2, error)
	ClearInterruptedTurn(sessionID string) (sessions.SessionV2, error)
	MarkTurnInterrupted(sessionID, turnID string) (sessions.SessionV2, error)
}

type runProjectorStore interface {
	MarkTurnRunningForRun(sessionID, runID, turnID string) (sessions.SessionV2, error)
	CompleteTurnForRun(sessionID, runID, turnID string) (sessions.SessionV2, error)
	InterruptTurnForRun(sessionID, runID, turnID string) (sessions.SessionV2, error)
}

func NewRun(store projectorStore, session sessions.SessionV2, runID string) (*RunProjector, error) {
	if store == nil || strings.TrimSpace(session.ID) == "" || strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("session store, session id, and run id are required")
	}
	return &RunProjector{store: store, runID: strings.TrimSpace(runID), current: session, byTurnID: make(map[string]*Projector)}, nil
}

func (p *RunProjector) HandleWithCheckpoint(event eventbus.Event) (int64, error) {
	if p == nil || event == nil {
		return 0, fmt.Errorf("run projector and event are required")
	}
	turnID := eventTurnID(event)
	p.mu.Lock()
	defer p.mu.Unlock()
	projector := p.byTurnID[turnID]
	if started, ok := event.(eventbus.TurnStarted); ok {
		if strings.TrimSpace(started.TurnID) == "" {
			return 0, fmt.Errorf("turn id is required")
		}
		if projector != nil {
			return 0, fmt.Errorf("turn %q already started", started.TurnID)
		}
		loader, ok := p.store.(interface {
			LoadExecutionState(string) (sessions.SessionV2, error)
		})
		if !ok {
			return 0, fmt.Errorf("session store cannot load run projector state")
		}
		state, err := loader.LoadExecutionState(p.current.ID)
		if err != nil {
			return 0, err
		}
		projector, err = NewForRun(p.store, state, p.runID)
		if err != nil {
			return 0, err
		}
		p.byTurnID[started.TurnID] = projector
	}
	if projector == nil {
		return 0, fmt.Errorf("turn %q has not started", turnID)
	}
	seq, err := projector.HandleWithCheckpoint(event)
	if err != nil {
		return 0, err
	}
	if _, ok := event.(eventbus.TurnCompleted); ok {
		_ = projector.Close()
		delete(p.byTurnID, turnID)
	}
	if _, ok := event.(eventbus.TurnInterrupted); ok {
		_ = projector.Close()
		delete(p.byTurnID, turnID)
	}
	return seq, nil
}

func (p *RunProjector) CheckpointHandler() eventbus.DurableCheckpointHandler {
	return p.HandleWithCheckpoint
}

func (p *RunProjector) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, projector := range p.byTurnID {
		_ = projector.Close()
		delete(p.byTurnID, id)
	}
	return nil
}

func eventTurnID(event eventbus.Event) string {
	switch e := event.(type) {
	case eventbus.TurnStarted:
		return e.TurnID
	case eventbus.CompactionRequested:
		return e.TurnID
	case eventbus.TurnInputReady:
		return e.TurnID
	case eventbus.AssistantReady:
		return e.TurnID
	case eventbus.AssistantTextCheckpoint:
		return e.TurnID
	case eventbus.ToolResultReady:
		return e.TurnID
	case eventbus.TurnCompleted:
		return e.TurnID
	case eventbus.TurnInterrupted:
		return e.TurnID
	default:
		return ""
	}
}

type projectorRequest struct {
	event eventbus.Event
	ack   chan projectorResult
}

type projectorResult struct {
	seq int64
	err error
}

func New(store projectorStore, session sessions.SessionV2) (*Projector, error) {
	return newProjector(store, session, "")
}

// NewForRun binds the projector's turn lifecycle to an explicit durable Run.
// New remains available for compaction and older low-level callers.
func NewForRun(store projectorStore, session sessions.SessionV2, runID string) (*Projector, error) {
	return newProjector(store, session, strings.TrimSpace(runID))
}

func newProjector(store projectorStore, session sessions.SessionV2, runID string) (*Projector, error) {
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
		runID:          runID,
		toolItems:      make(map[string]string),
		assistantItems: make(map[assistantItemKey]string),
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
	case eventbus.AssistantTextCheckpoint:
		err = p.handleAssistantTextCheckpoint(event)
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
	var metadata sessions.SessionV2
	if p.runID != "" {
		runStore, ok := p.store.(runProjectorStore)
		if !ok {
			return fmt.Errorf("session store does not support run turns")
		}
		metadata, err = runStore.MarkTurnRunningForRun(p.session.ID, p.runID, turnID)
	} else {
		metadata, err = p.store.MarkTurnRunning(p.session.ID, turnID)
	}
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
	if event.Context != nil {
		p.session.Context = *event.Context
	}
	record := compactionRecordItem(event.Checkpoint, p.turnID)
	next, err := p.store.SaveCompactedTurn(p.session, event.Summary, event.Checkpoint, []sessions.SessionItem{record}, event.Checkpoint.ReplacementHistory)
	if err != nil {
		return err
	}
	p.session = next
	return nil
}

// compactionRecordItem builds the durable, user-visible record marking where
// a compaction cut the chat timeline. Unlike the hidden model-audience
// summary, it renders as a one-line divider in the conversation. It is
// deliberately not part of the replacement (active) history, so the model
// never sees it. The id derives from the summary item id, which is validated
// non-empty and unique per session.
func compactionRecordItem(checkpoint sessions.CompactionCheckpoint, turnID string) sessions.SessionItem {
	text := "Context compacted"
	if checkpoint.Trigger == "auto" {
		text = "Context compacted automatically"
	}
	return sessions.SessionItem{
		ID:         checkpoint.SummaryItemID + "-record",
		TurnID:     turnID,
		CreatedAt:  checkpoint.CreatedAt,
		Kind:       sessions.ItemKindCompaction,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: text},
	}
}

func (p *Projector) handleTurnInputReady(event eventbus.TurnInputReady) error {
	turnID, err := p.requireActiveTurnID(event.TurnID)
	if err != nil {
		return err
	}
	if event.Message.Role != model.MessageRoleUser && event.Message.Role != model.MessageRoleDeveloper {
		return fmt.Errorf("turn input message role must be %q or %q", model.MessageRoleUser, model.MessageRoleDeveloper)
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
	items := make([]sessions.SessionItem, 0, len(event.Message.ToolCalls))
	activeHistory := copyStrings(p.session.ActiveHistory)
	seenToolCallIDs := make(map[string]struct{}, len(event.Message.ToolCalls))
	toolItemAdds := make(map[string]string, len(event.Message.ToolCalls))
	pendingItemAdds := make([]string, 0, len(event.Message.ToolCalls))

	assistantID := strings.TrimSpace(event.ItemID)
	key := assistantItemKey{turnID: turnID, iteration: event.AgentIteration}
	if assistantID == "" && event.AgentIteration > 0 {
		assistantID = p.assistantItems[key]
	}
	assistantItem, assistantExists := sessionItemByID(p.session.Items, assistantID)
	if assistantID != "" && assistantExists {
		if assistantItem.Message == nil || assistantItem.Message.Role != model.MessageRoleAssistant {
			return fmt.Errorf("assistant item %q has a non-assistant projection", assistantID)
		}
		if previous := p.assistantItems[key]; event.AgentIteration > 0 && previous != "" && previous != assistantID {
			return fmt.Errorf("assistant iteration %d in turn %q changed item identity from %q to %q", event.AgentIteration, turnID, previous, assistantID)
		}
		if !reflect.DeepEqual(*assistantItem.Message, event.Message) {
			_, next, err := p.store.UpdateItemFromState(p.session.ID, p.session, sessions.SessionItem{
				ID:      assistantID,
				Message: &event.Message,
			})
			if err != nil {
				return err
			}
			p.session = next
			assistantItem, _ = sessionItemByID(p.session.Items, assistantID)
		}
	} else {
		if assistantID == "" {
			assistantID = sessions.NextSessionItemID(existing, event.Message)
		}
		if _, alreadyUsed := existing[assistantID]; alreadyUsed {
			return fmt.Errorf("assistant item %q already exists", assistantID)
		}
		assistantItem = sessions.SessionItemFromMessage(assistantID, event.Message)
		assistantItem.TurnID = turnID
		assistantItem.AgentIteration = event.AgentIteration
		items = append(items, assistantItem)
		activeHistory = append(activeHistory, assistantID)
		existing[assistantID] = struct{}{}
	}
	if event.AgentIteration > 0 {
		p.assistantItems[key] = assistantID
	}

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
		item.AgentIteration = event.AgentIteration
		item.Status = sessions.ItemStatusPending
		items = append(items, item)
		activeHistory = append(activeHistory, itemID)
		toolItemAdds[toolCall.ID] = itemID
		pendingItemAdds = append(pendingItemAdds, itemID)
	}

	if len(items) > 0 {
		next, err := p.store.AppendItemsAndReplaceActiveHistoryFromState(p.session.ID, p.session, items, activeHistory)
		if err != nil {
			return err
		}
		p.session = next
	}
	for toolCallID, itemID := range toolItemAdds {
		p.toolItems[toolCallID] = itemID
	}
	for _, itemID := range pendingItemAdds {
		p.pendingItemIDs[itemID] = struct{}{}
	}
	return nil
}

func (p *Projector) handleAssistantTextCheckpoint(event eventbus.AssistantTextCheckpoint) error {
	turnID, err := p.requireActiveTurnID(event.TurnID)
	if err != nil {
		return err
	}
	if event.AgentIteration <= 0 {
		return fmt.Errorf("assistant iteration must be positive")
	}
	itemID := strings.TrimSpace(event.ItemID)
	if itemID == "" {
		return fmt.Errorf("assistant item id is required")
	}
	if strings.TrimSpace(event.Content) == "" {
		// Whitespace-only provider output is not a visible assistant message.
		return nil
	}
	key := assistantItemKey{turnID: turnID, iteration: event.AgentIteration}
	if existingID := p.assistantItems[key]; existingID != "" && existingID != itemID {
		return fmt.Errorf("assistant iteration %d in turn %q changed item identity from %q to %q", event.AgentIteration, turnID, existingID, itemID)
	}
	if existing, ok := sessionItemByID(p.session.Items, itemID); ok {
		if existing.Message == nil || existing.Message.Role != model.MessageRoleAssistant {
			return fmt.Errorf("assistant item %q has a non-assistant projection", itemID)
		}
		message := *existing.Message
		message.Content = event.Content
		_, next, err := p.store.UpdateItemFromState(p.session.ID, p.session, sessions.SessionItem{
			ID:      itemID,
			Message: &message,
		})
		if err != nil {
			return err
		}
		p.session = next
		p.assistantItems[key] = itemID
		return nil
	}
	if _, used := sessions.SessionItemIDs(p.session.Items)[itemID]; used {
		return fmt.Errorf("assistant item %q already exists", itemID)
	}
	message := model.Message{Role: model.MessageRoleAssistant, Content: event.Content}
	item := sessions.SessionItemFromMessage(itemID, message)
	item.TurnID = turnID
	item.AgentIteration = event.AgentIteration
	activeHistory := append(copyStrings(p.session.ActiveHistory), itemID)
	next, err := p.store.AppendItemsAndReplaceActiveHistoryFromState(p.session.ID, p.session, []sessions.SessionItem{item}, activeHistory)
	if err != nil {
		return err
	}
	p.session = next
	p.assistantItems[key] = itemID
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
	var metadata sessions.SessionV2
	if p.runID != "" {
		runStore, ok := p.store.(runProjectorStore)
		if !ok {
			return fmt.Errorf("session store does not support run turns")
		}
		metadata, err = runStore.CompleteTurnForRun(p.session.ID, p.runID, turnID)
	} else {
		metadata, err = p.store.ClearRunningTurn(p.session.ID, turnID)
		if err == nil {
			metadata, err = p.store.ClearInterruptedTurn(p.session.ID)
		}
	}
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
	var metadata sessions.SessionV2
	if p.runID != "" {
		runStore, ok := p.store.(runProjectorStore)
		if !ok {
			return fmt.Errorf("session store does not support run turns")
		}
		metadata, err = runStore.InterruptTurnForRun(p.session.ID, p.runID, turnID)
	} else {
		// A compaction turn is a virtual operation, not a real run. Interrupting
		// it must clear the running-turn marker without writing a run-scoped
		// interrupted state: MarkTurnInterrupted would leave InterruptedRunID
		// empty while setting InterruptedTurnID/InterruptedAt, which poisons the
		// session so it can no longer be opened or continued.
		metadata, err = p.store.ClearRunningTurn(p.session.ID, turnID)
	}
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
