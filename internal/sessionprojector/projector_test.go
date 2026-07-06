package sessionprojector

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestProjectorWritesTurnEventsSynchronously(t *testing.T) {
	store, projector, bus := newProjectorFixture(t, "session-1")

	publish(t, bus, eventbus.TurnStarted{TurnID: "turn-1"})
	loaded, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load() after TurnStarted error = %v", err)
	}
	if loaded.RunningTurnID != "turn-1" {
		t.Fatalf("RunningTurnID = %q, want turn-1", loaded.RunningTurnID)
	}

	publish(t, bus, eventbus.TurnInputReady{
		TurnID:  "turn-1",
		Message: model.Message{Role: model.MessageRoleUser, Content: "use tools"},
	})
	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() after TurnInputReady error = %v", err)
	}
	if replayed.LastSeq != 4 || !reflect.DeepEqual(replayed.ActiveHistory, []string{"msg-000001"}) {
		t.Fatalf("state after input = last %d active %#v, want last 4 active user", replayed.LastSeq, replayed.ActiveHistory)
	}

	publish(t, bus, eventbus.AssistantReady{
		TurnID: "turn-1",
		Message: model.Message{
			Role: model.MessageRoleAssistant,
			ToolCalls: []model.ToolCall{
				{ID: "call-a", Name: "read", Arguments: "{}"},
				{ID: "call-b", Name: "write", Arguments: "{}"},
			},
		},
	})
	publish(t, bus, eventbus.ToolResultReady{
		TurnID: "turn-1",
		Result: model.ToolResult{ToolCallID: "call-a", Content: "alpha"},
	})
	publish(t, bus, eventbus.ToolResultReady{
		TurnID: "turn-1",
		Result: model.ToolResult{ToolCallID: "call-b", Content: "bravo failed", IsError: true},
	})
	publish(t, bus, eventbus.TurnCompleted{TurnID: "turn-1"})

	loaded, err = store.Load("session-1")
	if err != nil {
		t.Fatalf("Load() final error = %v", err)
	}
	if loaded.RunningTurnID != "" {
		t.Fatalf("RunningTurnID = %q, want cleared", loaded.RunningTurnID)
	}
	if loaded.LastSeq != 12 {
		t.Fatalf("LastSeq = %d, want 12", loaded.LastSeq)
	}
	wantActive := []string{"msg-000001", "msg-000002", "msg-000003", "msg-000004"}
	if !reflect.DeepEqual(loaded.ActiveHistory, wantActive) {
		t.Fatalf("ActiveHistory = %#v, want %#v", loaded.ActiveHistory, wantActive)
	}
	if got := itemStatusesByToolCall(loaded.Items); !reflect.DeepEqual(got, map[string]string{
		"call-a": sessions.ItemStatusCompleted,
		"call-b": sessions.ItemStatusError,
	}) {
		t.Fatalf("tool statuses = %#v", got)
	}
	if content := toolItemContent(loaded.Items, "call-b"); content != "bravo failed" {
		t.Fatalf("call-b content = %q, want bravo failed", content)
	}

	if err := projector.Close(); err != nil {
		t.Fatalf("Projector.Close() error = %v", err)
	}
}

func TestProjectorMaintainsToolMapAcrossRounds(t *testing.T) {
	store, projector, bus := newProjectorFixture(t, "session-1")
	defer projector.Close()

	publish(t, bus, eventbus.TurnStarted{TurnID: "turn-1"})
	publish(t, bus, eventbus.TurnInputReady{TurnID: "turn-1", Message: model.Message{Role: model.MessageRoleUser, Content: "multi"}})
	publish(t, bus, eventbus.AssistantReady{
		TurnID: "turn-1",
		Message: model.Message{
			Role:      model.MessageRoleAssistant,
			ToolCalls: []model.ToolCall{{ID: "call-1", Name: "one", Arguments: "{}"}},
		},
	})
	publish(t, bus, eventbus.ToolResultReady{TurnID: "turn-1", Result: model.ToolResult{ToolCallID: "call-1", Content: "one"}})

	before, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() before duplicate error = %v", err)
	}
	if err := bus.Publish(eventbus.ToolResultReady{TurnID: "turn-1", Result: model.ToolResult{ToolCallID: "call-1", Content: "duplicate"}}); err == nil {
		t.Fatal("duplicate ToolResultReady error = nil, want error")
	}
	after, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() after duplicate error = %v", err)
	}
	if after.LastSeq != before.LastSeq {
		t.Fatalf("LastSeq after duplicate result = %d, want unchanged %d", after.LastSeq, before.LastSeq)
	}

	publish(t, bus, eventbus.AssistantReady{
		TurnID: "turn-1",
		Message: model.Message{
			Role:      model.MessageRoleAssistant,
			Content:   "second round",
			ToolCalls: []model.ToolCall{{ID: "call-2", Name: "two", Arguments: "{}"}},
		},
	})
	publish(t, bus, eventbus.ToolResultReady{TurnID: "turn-1", Result: model.ToolResult{ToolCallID: "call-2", Content: "two"}})
	publish(t, bus, eventbus.TurnCompleted{TurnID: "turn-1"})

	loaded, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := itemStatusesByToolCall(loaded.Items); !reflect.DeepEqual(got, map[string]string{
		"call-1": sessions.ItemStatusCompleted,
		"call-2": sessions.ItemStatusCompleted,
	}) {
		t.Fatalf("tool statuses = %#v", got)
	}
	if got := toolItemContent(loaded.Items, "call-2"); got != "two" {
		t.Fatalf("call-2 content = %q, want two", got)
	}
}

func TestProjectorBootstrapsRuntimeContextOnFirstInput(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(sessions.SessionV2{
		ID:           "session-1",
		Provider:     "test",
		ModelProfile: "test",
		ModelID:      "test",
		InstructionsSnapshot: []model.Message{
			{Role: model.MessageRoleSystem, Content: "system rules"},
			{Role: model.MessageRoleDeveloper, Content: "developer rules"},
		},
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	projector, err := New(store, session)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer projector.Close()
	bus := eventbus.NewBus(projector.Handler())
	defer bus.Close()

	publish(t, bus, eventbus.TurnStarted{TurnID: "turn-1"})
	publish(t, bus, eventbus.TurnInputReady{TurnID: "turn-1", Message: model.Message{Role: model.MessageRoleUser, Content: "hello"}})

	loaded, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := itemIDs(loaded.Items), []string{"runtime-000001", "runtime-000002", "msg-000003"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("item IDs = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(loaded.ActiveHistory, []string{"runtime-000001", "runtime-000002", "msg-000003"}) {
		t.Fatalf("ActiveHistory = %#v, want runtime items then user", loaded.ActiveHistory)
	}
	for i, item := range loaded.Items[:2] {
		if item.TurnID != "" || item.Kind != sessions.ItemKindRuntimeContext || item.Visibility != sessions.ItemVisibilityHidden {
			t.Fatalf("runtime item[%d] = %#v, want hidden runtime context without TurnID", i, item)
		}
	}
	if loaded.Items[2].TurnID != "turn-1" {
		t.Fatalf("user item TurnID = %q, want turn-1", loaded.Items[2].TurnID)
	}
	messages, err := store.MaterializeActiveHistory(loaded)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !reflect.DeepEqual(got, []string{"system rules", "developer rules", "hello"}) {
		t.Fatalf("materialized messages = %#v, want snapshot then user", got)
	}
}

func TestProjectorDoesNotDuplicateRuntimeContextWhenActiveHistoryExists(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	_, err := store.SaveMetadata(sessions.SessionV2{
		ID:           "session-1",
		Provider:     "test",
		ModelProfile: "test",
		ModelID:      "test",
		InstructionsSnapshot: []model.Message{
			{Role: model.MessageRoleSystem, Content: "system rules"},
		},
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	if _, err := store.AppendItemsAndReplaceActiveHistory("session-1", []sessions.SessionItem{
		sessions.SessionItemFromMessage("runtime-000001", model.Message{Role: model.MessageRoleSystem, Content: "system rules"}),
	}, []string{"runtime-000001"}); err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory(seed) error = %v", err)
	}
	loaded, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load(seed) error = %v", err)
	}
	projector, err := New(store, loaded)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer projector.Close()
	bus := eventbus.NewBus(projector.Handler())
	defer bus.Close()

	publish(t, bus, eventbus.TurnStarted{TurnID: "turn-1"})
	publish(t, bus, eventbus.TurnInputReady{TurnID: "turn-1", Message: model.Message{Role: model.MessageRoleUser, Content: "hello"}})

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got := countRuntimeContextItems(replayed.Items); got != 1 {
		t.Fatalf("runtime context item count = %d, want 1", got)
	}
	if got, want := replayed.ActiveHistory, []string{"runtime-000001", "msg-000002"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ActiveHistory = %#v, want %#v", got, want)
	}
}

func TestProjectorRefreshesCachedStateAfterCompaction(t *testing.T) {
	root := t.TempDir()
	store := sessions.NewV2Store(root)
	if _, err := store.SaveMetadata(sessions.SessionV2{ID: "session-1", Provider: "test", ModelProfile: "test", ModelID: "test"}); err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	if _, err := store.AppendItemsAndReplaceActiveHistory("session-1", []sessions.SessionItem{
		{
			ID:         "old-user",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "old"},
		},
		{
			ID:         "old-assistant",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "old answer"},
		},
	}, []string{"old-user", "old-assistant"}); err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory(seed) error = %v", err)
	}
	loaded, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load(seed) error = %v", err)
	}
	projector, err := New(store, loaded)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer projector.Close()
	bus := eventbus.NewBus(projector.Handler())
	defer bus.Close()

	summary := sessions.SessionItem{
		ID:         "summary-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "summary"},
	}
	checkpoint := sessions.CompactionCheckpoint{
		ID:                    "checkpoint-1",
		SummaryItemID:         "summary-1",
		PreviousActiveHistory: loaded.ActiveHistory,
		ReplacementHistory:    []string{"summary-1"},
	}

	publish(t, bus, eventbus.TurnStarted{TurnID: "turn-1"})
	publish(t, bus, eventbus.CompactionRequested{TurnID: "turn-1", Summary: summary, Checkpoint: checkpoint})
	publish(t, bus, eventbus.TurnInputReady{TurnID: "turn-1", Message: model.Message{Role: model.MessageRoleUser, Content: "new"}})

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if replayed.LastSeq != 14 {
		t.Fatalf("LastSeq = %d, want 14", replayed.LastSeq)
	}
	if len(replayed.ActiveHistory) != 2 || replayed.ActiveHistory[0] != "summary-1" {
		t.Fatalf("ActiveHistory = %#v, want summary then new user", replayed.ActiveHistory)
	}
	if replayed.ActiveHistory[1] == "old-user" || replayed.ActiveHistory[1] == "old-assistant" {
		t.Fatalf("ActiveHistory used stale pre-compaction item: %#v", replayed.ActiveHistory)
	}
	user, ok := sessionItemByID(replayed.Items, replayed.ActiveHistory[1])
	if !ok || user.Message == nil || user.Message.Role != model.MessageRoleUser || user.Message.Content != "new" {
		t.Fatalf("new active user item = %#v, ok %v", user, ok)
	}
}

func TestProjectorInterruptsPendingTools(t *testing.T) {
	store, projector, bus := newProjectorFixture(t, "session-1")
	defer projector.Close()

	publish(t, bus, eventbus.TurnStarted{TurnID: "turn-1"})
	publish(t, bus, eventbus.TurnInputReady{TurnID: "turn-1", Message: model.Message{Role: model.MessageRoleUser, Content: "cancel"}})
	publish(t, bus, eventbus.AssistantReady{
		TurnID: "turn-1",
		Message: model.Message{
			Role: model.MessageRoleAssistant,
			ToolCalls: []model.ToolCall{
				{ID: "call-a", Name: "a", Arguments: "{}"},
				{ID: "call-b", Name: "b", Arguments: "{}"},
			},
		},
	})
	publish(t, bus, eventbus.TurnInterrupted{TurnID: "turn-1"})

	loaded, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.RunningTurnID != "" || loaded.InterruptedTurnID != "turn-1" {
		t.Fatalf("lifecycle = running %q interrupted %q, want interrupted turn-1", loaded.RunningTurnID, loaded.InterruptedTurnID)
	}
	if got := itemStatusesByToolCall(loaded.Items); !reflect.DeepEqual(got, map[string]string{
		"call-a": sessions.ItemStatusInterrupted,
		"call-b": sessions.ItemStatusInterrupted,
	}) {
		t.Fatalf("tool statuses = %#v", got)
	}
}

func TestProjectorSerializesConcurrentToolResults(t *testing.T) {
	const toolCount = 12

	store, projector, bus := newProjectorFixture(t, "session-1")
	defer projector.Close()

	toolCalls := make([]model.ToolCall, 0, toolCount)
	for i := 0; i < toolCount; i++ {
		toolCalls = append(toolCalls, model.ToolCall{ID: fmt.Sprintf("call-%02d", i), Name: "tool", Arguments: "{}"})
	}
	publish(t, bus, eventbus.TurnStarted{TurnID: "turn-1"})
	publish(t, bus, eventbus.TurnInputReady{TurnID: "turn-1", Message: model.Message{Role: model.MessageRoleUser, Content: "parallel"}})
	publish(t, bus, eventbus.AssistantReady{TurnID: "turn-1", Message: model.Message{Role: model.MessageRoleAssistant, ToolCalls: toolCalls}})

	var wg sync.WaitGroup
	errs := make(chan error, toolCount)
	for _, toolCall := range toolCalls {
		toolCall := toolCall
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- bus.Publish(eventbus.ToolResultReady{
				TurnID: "turn-1",
				Result: model.ToolResult{ToolCallID: toolCall.ID, Content: "done " + toolCall.ID},
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ToolResultReady publish error = %v", err)
		}
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if want := int64(8 + 2*toolCount); replayed.LastSeq != want {
		t.Fatalf("LastSeq = %d, want %d", replayed.LastSeq, want)
	}
	for _, toolCall := range toolCalls {
		if status := itemStatusesByToolCall(replayed.Items)[toolCall.ID]; status != sessions.ItemStatusCompleted {
			t.Fatalf("%s status = %q, want completed", toolCall.ID, status)
		}
	}
	events, err := store.PersistedEventsAfter("session-1", 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatalf("persisted events not increasing at %d: %#v", i, events)
		}
	}
}

func TestProjectorValidationAndClose(t *testing.T) {
	store, projector, bus := newProjectorFixture(t, "session-1")

	if err := projector.Handle(eventbus.ModelEvent{Event: model.TextDeltaEvent{Text: "live"}}); err == nil {
		t.Fatal("Handle(transient) error = nil, want error")
	}
	publish(t, bus, eventbus.TurnStarted{TurnID: "turn-1"})
	publish(t, bus, eventbus.TurnInputReady{TurnID: "turn-1", Message: model.Message{Role: model.MessageRoleUser, Content: "validate"}})
	before, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() before invalid event error = %v", err)
	}
	if err := bus.Publish(eventbus.ToolResultReady{TurnID: "turn-1", Result: model.ToolResult{ToolCallID: "missing", Content: "nope"}}); err == nil {
		t.Fatal("unknown ToolResultReady error = nil, want error")
	}
	after, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() after invalid event error = %v", err)
	}
	if after.LastSeq != before.LastSeq {
		t.Fatalf("LastSeq after invalid event = %d, want unchanged %d", after.LastSeq, before.LastSeq)
	}

	if err := bus.Publish(eventbus.AssistantReady{
		TurnID: "turn-1",
		Message: model.Message{
			Role: model.MessageRoleAssistant,
			ToolCalls: []model.ToolCall{
				{ID: "duplicate", Name: "tool", Arguments: "{}"},
				{ID: "duplicate", Name: "tool", Arguments: "{}"},
			},
		},
	}); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("AssistantReady with duplicate tool calls error = %v, want duplicated", err)
	}
	afterDuplicate, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() after duplicate assistant error = %v", err)
	}
	if afterDuplicate.LastSeq != before.LastSeq {
		t.Fatalf("LastSeq after duplicate assistant = %d, want unchanged %d", afterDuplicate.LastSeq, before.LastSeq)
	}

	publish(t, bus, eventbus.AssistantReady{
		TurnID:  "turn-1",
		Message: model.Message{Role: model.MessageRoleAssistant, ToolCalls: []model.ToolCall{{ID: "call-1", Name: "tool", Arguments: "{}"}}},
	})
	if err := bus.Publish(eventbus.TurnCompleted{TurnID: "turn-1"}); err == nil || !strings.Contains(err.Error(), "pending tool items") {
		t.Fatalf("TurnCompleted with pending error = %v, want pending tool items", err)
	}
	loaded, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load() after pending completion error = %v", err)
	}
	if loaded.RunningTurnID != "turn-1" {
		t.Fatalf("RunningTurnID = %q, want still running after rejected completion", loaded.RunningTurnID)
	}

	if err := projector.Close(); err != nil {
		t.Fatalf("Projector.Close() error = %v", err)
	}
	if err := projector.Handle(eventbus.TurnInterrupted{TurnID: "turn-1"}); !errors.Is(err, eventbus.ErrClosed) {
		t.Fatalf("Handle() after Close error = %v, want %v", err, eventbus.ErrClosed)
	}
}

func newProjectorFixture(t *testing.T, sessionID string) (*sessions.V2Store, *Projector, *eventbus.Bus) {
	t.Helper()

	store := sessions.NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(sessions.SessionV2{
		ID:           sessionID,
		Provider:     "test",
		ModelProfile: "test",
		ModelID:      "test",
	})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	projector, err := New(store, session)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = projector.Close()
	})
	bus := eventbus.NewBus(projector.Handler())
	t.Cleanup(func() {
		bus.Close()
	})
	return store, projector, bus
}

func publish(t *testing.T, bus *eventbus.Bus, event eventbus.Event) {
	t.Helper()
	if err := bus.Publish(event); err != nil {
		t.Fatalf("Publish(%T) error = %v", event, err)
	}
}

func itemStatusesByToolCall(items []sessions.SessionItem) map[string]string {
	statuses := make(map[string]string)
	for _, item := range items {
		if item.Message == nil || item.Message.Role != model.MessageRoleTool {
			continue
		}
		statuses[item.Message.ToolCallID] = item.Status
	}
	return statuses
}

func toolItemContent(items []sessions.SessionItem, toolCallID string) string {
	for _, item := range items {
		if item.Message != nil && item.Message.Role == model.MessageRoleTool && item.Message.ToolCallID == toolCallID {
			return item.Message.Content
		}
	}
	return ""
}

func itemIDs(items []sessions.SessionItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func messageContents(messages []model.Message) []string {
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return contents
}

func countRuntimeContextItems(items []sessions.SessionItem) int {
	count := 0
	for _, item := range items {
		if item.Kind == sessions.ItemKindRuntimeContext {
			count++
		}
	}
	return count
}
