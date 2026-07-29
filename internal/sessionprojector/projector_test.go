package sessionprojector

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
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
		TurnID:         "turn-1",
		AgentIteration: 1,
		Message: model.Message{
			Role: model.MessageRoleAssistant,
			ToolCalls: []model.ToolCall{
				{ID: "call-a", Name: "read", Arguments: "{}"},
				{ID: "call-b", Name: "write", Arguments: "{}"},
			},
		},
	})
	publish(t, bus, eventbus.ToolResultReady{
		TurnID:         "turn-1",
		AgentIteration: 1,
		Result:         model.ToolResult{ToolCallID: "call-a", Content: "alpha"},
	})
	publish(t, bus, eventbus.ToolResultReady{
		TurnID:         "turn-1",
		AgentIteration: 1,
		Result:         model.ToolResult{ToolCallID: "call-b", Content: "bravo failed", IsError: true},
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
	for _, item := range loaded.Items {
		if item.Message == nil || (item.Message.Role != model.MessageRoleAssistant && item.Message.Role != model.MessageRoleTool) {
			continue
		}
		if item.AgentIteration != 1 {
			t.Fatalf("item %q AgentIteration = %d, want 1", item.ID, item.AgentIteration)
		}
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
	compactionContext := contextwindow.Metadata{
		ContextWindow:        400000,
		LastInputTokens:      1200,
		LastCachedTokens:     900,
		LastCacheWriteTokens: 200,
		LastUsageSource:      string(contextwindow.UsageSourceProvider),
	}

	publish(t, bus, eventbus.TurnStarted{TurnID: "turn-1"})
	publish(t, bus, eventbus.CompactionRequested{
		TurnID:     "turn-1",
		Summary:    summary,
		Checkpoint: checkpoint,
		Context:    &compactionContext,
	})
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
	stored, err := store.Load("session-1")
	if err != nil {
		t.Fatalf("Load(compacted) error = %v", err)
	}
	if !reflect.DeepEqual(stored.Context, compactionContext) {
		t.Fatalf("Context = %#v, want compact usage context %#v", stored.Context, compactionContext)
	}
}

func TestProjectorWithFakeBusAndStoreRecordsOrderedLifecycle(t *testing.T) {
	initial := sessions.SessionV2{
		ID:           "session-1",
		Provider:     "test",
		ModelProfile: "test",
		ModelID:      "test",
		LastSeq:      1,
		Items: []sessions.SessionItem{{
			ID:         "old-user",
			Seq:        1,
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "old"},
		}},
		ActiveHistory: []string{"old-user"},
	}
	store := newFakeProjectorStore(initial)
	projector, err := New(store, initial)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer projector.Close()
	bus := fakeProjectorBus{handler: projector.HandleWithCheckpoint}

	publishFake(t, &bus, store, eventbus.TurnStarted{TurnID: "turn-1"}, 1)
	if store.session.RunningTurnID != "turn-1" {
		t.Fatalf("RunningTurnID = %q, want turn-1 after synchronous TurnStarted", store.session.RunningTurnID)
	}

	summary := sessions.SessionItem{
		ID:         "summary-1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityHidden,
		Audience:   sessions.ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "summary"},
	}
	checkpoint := sessions.CompactionCheckpoint{
		ID:                    "checkpoint-1",
		SummaryItemID:         summary.ID,
		PreviousActiveHistory: []string{"old-user"},
		ReplacementHistory:    []string{summary.ID},
	}
	publishFake(t, &bus, store, eventbus.CompactionRequested{TurnID: "turn-1", Summary: summary, Checkpoint: checkpoint}, 2)
	if !reflect.DeepEqual(store.session.ActiveHistory, []string{summary.ID}) {
		t.Fatalf("ActiveHistory after compaction = %#v, want summary replacement", store.session.ActiveHistory)
	}

	publishFake(t, &bus, store, eventbus.TurnInputReady{
		TurnID:  "turn-1",
		Message: model.Message{Role: model.MessageRoleUser, Content: "new"},
	}, 3)
	userID := store.calls[2].itemIDs[0]
	if !reflect.DeepEqual(store.calls[2].activeHistory, []string{summary.ID, userID}) {
		t.Fatalf("TurnInputReady active history = %#v, want post-compaction summary then user %q", store.calls[2].activeHistory, userID)
	}

	publishFake(t, &bus, store, eventbus.AssistantReady{
		TurnID: "turn-1",
		Message: model.Message{
			Role:      model.MessageRoleAssistant,
			Content:   "round one",
			ToolCalls: []model.ToolCall{{ID: "call-1", Name: "tool", Arguments: "{}"}},
		},
	}, 4)
	roundOneAssistantID := store.calls[3].itemIDs[0]
	roundOneToolID := store.calls[3].itemIDs[1]
	if !reflect.DeepEqual(store.calls[3].activeHistory, []string{summary.ID, userID, roundOneAssistantID, roundOneToolID}) {
		t.Fatalf("round one active history = %#v, want user plus assistant/tool", store.calls[3].activeHistory)
	}

	publishFake(t, &bus, store, eventbus.ToolResultReady{
		TurnID: "turn-1",
		Result: model.ToolResult{ToolCallID: "call-1", Content: "one"},
	}, 5)
	if store.calls[4].name != "update_item" || store.calls[4].itemIDs[0] != roundOneToolID {
		t.Fatalf("round one update call = %#v, want update of %q after AssistantReady append", store.calls[4], roundOneToolID)
	}

	publishFake(t, &bus, store, eventbus.AssistantReady{
		TurnID: "turn-1",
		Message: model.Message{
			Role:      model.MessageRoleAssistant,
			Content:   "round two",
			ToolCalls: []model.ToolCall{{ID: "call-2", Name: "tool", Arguments: "{}"}},
		},
	}, 6)
	roundTwoAssistantID := store.calls[5].itemIDs[0]
	roundTwoToolID := store.calls[5].itemIDs[1]
	wantRoundTwoActive := []string{summary.ID, userID, roundOneAssistantID, roundOneToolID, roundTwoAssistantID, roundTwoToolID}
	if !reflect.DeepEqual(store.calls[5].activeHistory, wantRoundTwoActive) {
		t.Fatalf("round two active history = %#v, want %#v", store.calls[5].activeHistory, wantRoundTwoActive)
	}

	publishFake(t, &bus, store, eventbus.ToolResultReady{
		TurnID: "turn-1",
		Result: model.ToolResult{ToolCallID: "call-2", Content: "two"},
	}, 7)
	if store.calls[6].name != "update_item" || store.calls[6].itemIDs[0] != roundTwoToolID {
		t.Fatalf("round two update call = %#v, want update of %q after second AssistantReady append", store.calls[6], roundTwoToolID)
	}

	publishFake(t, &bus, store, eventbus.TurnCompleted{TurnID: "turn-1"}, 9)
	gotCallNames := store.callNames()
	wantCallNames := []string{
		"mark_running",
		"save_compacted_turn",
		"append_items",
		"append_items",
		"update_item",
		"append_items",
		"update_item",
		"clear_running",
		"clear_interrupted",
	}
	if !reflect.DeepEqual(gotCallNames, wantCallNames) {
		t.Fatalf("store call order = %#v, want %#v", gotCallNames, wantCallNames)
	}
	if got := countString(gotCallNames, "clear_running"); got != 1 {
		t.Fatalf("clear_running calls = %d, want 1", got)
	}
	if store.session.RunningTurnID != "" {
		t.Fatalf("RunningTurnID after completion = %q, want cleared", store.session.RunningTurnID)
	}
	if got := itemStatusesByToolCall(store.session.Items); !reflect.DeepEqual(got, map[string]string{
		"call-1": sessions.ItemStatusCompleted,
		"call-2": sessions.ItemStatusCompleted,
	}) {
		t.Fatalf("tool statuses = %#v, want both completed", got)
	}
	assertContiguousSeqs(t, store.recordSeqs, 2, store.session.LastSeq)
	for i, checkpointSeq := range bus.checkpoints {
		if checkpointSeq < initial.LastSeq || checkpointSeq > store.session.LastSeq {
			t.Fatalf("checkpoint[%d] = %d, want within durable seq range [%d,%d]", i, checkpointSeq, initial.LastSeq, store.session.LastSeq)
		}
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
	if len(events) < toolCount {
		t.Fatalf("persisted events count = %d, want at least %d", len(events), toolCount)
	}
	updated := events[len(events)-toolCount:]
	seenUpdatedItems := make(map[string]struct{}, toolCount)
	firstUpdateSeq := replayed.LastSeq - toolCount + 1
	for i, event := range updated {
		if event.Type != sessions.RecordTypeItemUpdated {
			t.Fatalf("persisted event tail[%d] type = %q, want %q: %#v", i, event.Type, sessions.RecordTypeItemUpdated, updated)
		}
		if want := firstUpdateSeq + int64(i); event.Seq != want {
			t.Fatalf("persisted item.updated seq at tail[%d] = %d, want %d: %#v", i, event.Seq, want, updated)
		}
		if _, ok := seenUpdatedItems[event.ItemID]; ok {
			t.Fatalf("duplicate item.updated event for item %q: %#v", event.ItemID, updated)
		}
		seenUpdatedItems[event.ItemID] = struct{}{}
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

type fakeProjectorBus struct {
	handler     eventbus.DurableCheckpointHandler
	checkpoints []int64
}

func publishFake(t *testing.T, bus *fakeProjectorBus, store *fakeProjectorStore, event eventbus.Event, wantCalls int) {
	t.Helper()

	seq, err := bus.handler(event)
	if err != nil {
		t.Fatalf("fake publish %T error = %v", event, err)
	}
	bus.checkpoints = append(bus.checkpoints, seq)
	if len(store.calls) != wantCalls {
		t.Fatalf("fake publish %T returned after %d store calls, want %d: %#v", event, len(store.calls), wantCalls, store.calls)
	}
	if seq != store.session.LastSeq {
		t.Fatalf("fake publish %T checkpoint = %d, store LastSeq = %d", event, seq, store.session.LastSeq)
	}
}

type fakeProjectorStore struct {
	session    sessions.SessionV2
	calls      []fakeProjectorStoreCall
	recordSeqs []int64
}

type fakeProjectorStoreCall struct {
	name          string
	seq           int64
	itemIDs       []string
	activeHistory []string
}

func newFakeProjectorStore(session sessions.SessionV2) *fakeProjectorStore {
	return &fakeProjectorStore{session: cloneProjectorTestSession(session)}
}

func (s *fakeProjectorStore) MarkTurnRunning(sessionID, turnID string) (sessions.SessionV2, error) {
	if err := s.requireSession(sessionID); err != nil {
		return sessions.SessionV2{}, err
	}
	s.session.RunningTurnID = turnID
	s.calls = append(s.calls, fakeProjectorStoreCall{name: "mark_running", seq: s.session.LastSeq})
	return cloneProjectorTestSession(s.session), nil
}

func (s *fakeProjectorStore) SaveCompactedTurn(session sessions.SessionV2, summaryItem sessions.SessionItem, checkpoint sessions.CompactionCheckpoint, items []sessions.SessionItem, activeHistory []string) (sessions.SessionV2, error) {
	if err := s.requireCachedState(session); err != nil {
		return sessions.SessionV2{}, err
	}
	s.nextRecordSeq()
	summaryItem.Seq = s.nextRecordSeq()
	s.session.Items = append(s.session.Items, cloneProjectorTestItem(summaryItem))
	s.nextRecordSeq()
	s.session.Compactions = append(s.session.Compactions, checkpoint)
	for _, item := range items {
		item.Seq = s.nextRecordSeq()
		s.session.Items = append(s.session.Items, cloneProjectorTestItem(item))
	}
	s.nextRecordSeq()
	s.session.ActiveHistory = copyStrings(activeHistory)
	s.nextRecordSeq()
	s.calls = append(s.calls, fakeProjectorStoreCall{
		name:          "save_compacted_turn",
		seq:           s.session.LastSeq,
		itemIDs:       []string{summaryItem.ID},
		activeHistory: copyStrings(s.session.ActiveHistory),
	})
	return cloneProjectorTestSession(s.session), nil
}

func (s *fakeProjectorStore) AppendItemsAndReplaceActiveHistoryFromState(sessionID string, state sessions.SessionV2, items []sessions.SessionItem, itemIDs []string) (sessions.SessionV2, error) {
	if err := s.requireSession(sessionID); err != nil {
		return sessions.SessionV2{}, err
	}
	if err := s.requireCachedState(state); err != nil {
		return sessions.SessionV2{}, err
	}
	s.nextRecordSeq()
	added := make([]string, 0, len(items))
	for _, item := range items {
		item.Seq = s.nextRecordSeq()
		s.session.Items = append(s.session.Items, cloneProjectorTestItem(item))
		added = append(added, item.ID)
	}
	s.nextRecordSeq()
	s.session.ActiveHistory = copyStrings(itemIDs)
	s.nextRecordSeq()
	s.calls = append(s.calls, fakeProjectorStoreCall{
		name:          "append_items",
		seq:           s.session.LastSeq,
		itemIDs:       added,
		activeHistory: copyStrings(s.session.ActiveHistory),
	})
	return cloneProjectorTestSession(s.session), nil
}

func (s *fakeProjectorStore) UpdateItemFromState(sessionID string, state sessions.SessionV2, item sessions.SessionItem) (sessions.SessionItem, sessions.SessionV2, error) {
	if err := s.requireSession(sessionID); err != nil {
		return sessions.SessionItem{}, sessions.SessionV2{}, err
	}
	if err := s.requireCachedState(state); err != nil {
		return sessions.SessionItem{}, sessions.SessionV2{}, err
	}
	index := -1
	for i, existing := range s.session.Items {
		if existing.ID == item.ID {
			index = i
			break
		}
	}
	if index < 0 {
		return sessions.SessionItem{}, sessions.SessionV2{}, fmt.Errorf("fake item %q not found", item.ID)
	}
	updated := s.session.Items[index]
	updated.Message = cloneProjectorTestMessagePtr(item.Message)
	updated.Content = cloneProjectorTestContentPtr(item.Content)
	updated.Status = item.Status
	s.nextRecordSeq()
	s.session.Items[index] = updated
	s.calls = append(s.calls, fakeProjectorStoreCall{
		name:    "update_item",
		seq:     s.session.LastSeq,
		itemIDs: []string{item.ID},
	})
	return cloneProjectorTestItem(updated), cloneProjectorTestSession(s.session), nil
}

func (s *fakeProjectorStore) ClearRunningTurn(sessionID, turnID string) (sessions.SessionV2, error) {
	if err := s.requireSession(sessionID); err != nil {
		return sessions.SessionV2{}, err
	}
	if s.session.RunningTurnID != turnID {
		return sessions.SessionV2{}, fmt.Errorf("fake running turn = %q, want %q", s.session.RunningTurnID, turnID)
	}
	s.session.RunningTurnID = ""
	s.calls = append(s.calls, fakeProjectorStoreCall{name: "clear_running", seq: s.session.LastSeq})
	return cloneProjectorTestSession(s.session), nil
}

func (s *fakeProjectorStore) ClearInterruptedTurn(sessionID string) (sessions.SessionV2, error) {
	if err := s.requireSession(sessionID); err != nil {
		return sessions.SessionV2{}, err
	}
	s.session.InterruptedTurnID = ""
	s.session.InterruptedAt = time.Time{}
	s.calls = append(s.calls, fakeProjectorStoreCall{name: "clear_interrupted", seq: s.session.LastSeq})
	return cloneProjectorTestSession(s.session), nil
}

func (s *fakeProjectorStore) MarkTurnInterrupted(sessionID, turnID string) (sessions.SessionV2, error) {
	if err := s.requireSession(sessionID); err != nil {
		return sessions.SessionV2{}, err
	}
	s.session.RunningTurnID = ""
	s.session.InterruptedTurnID = turnID
	s.calls = append(s.calls, fakeProjectorStoreCall{name: "mark_interrupted", seq: s.session.LastSeq})
	return cloneProjectorTestSession(s.session), nil
}

func (s *fakeProjectorStore) nextRecordSeq() int64 {
	s.session.LastSeq++
	s.recordSeqs = append(s.recordSeqs, s.session.LastSeq)
	return s.session.LastSeq
}

func (s *fakeProjectorStore) requireSession(sessionID string) error {
	if sessionID != s.session.ID {
		return fmt.Errorf("fake session id = %q, want %q", sessionID, s.session.ID)
	}
	return nil
}

func (s *fakeProjectorStore) requireCachedState(state sessions.SessionV2) error {
	if state.ID != s.session.ID {
		return fmt.Errorf("fake cached state session id = %q, want %q", state.ID, s.session.ID)
	}
	if state.LastSeq != s.session.LastSeq {
		return fmt.Errorf("fake cached state LastSeq = %d, want %d", state.LastSeq, s.session.LastSeq)
	}
	if !reflect.DeepEqual(state.ActiveHistory, s.session.ActiveHistory) {
		return fmt.Errorf("fake cached state ActiveHistory = %#v, want %#v", state.ActiveHistory, s.session.ActiveHistory)
	}
	return nil
}

func (s *fakeProjectorStore) callNames() []string {
	names := make([]string, 0, len(s.calls))
	for _, call := range s.calls {
		names = append(names, call.name)
	}
	return names
}

func assertContiguousSeqs(t *testing.T, seqs []int64, first, last int64) {
	t.Helper()
	if len(seqs) == 0 {
		t.Fatal("record seqs empty, want durable records")
	}
	if seqs[0] != first || seqs[len(seqs)-1] != last {
		t.Fatalf("record seq range = [%d,%d], want [%d,%d]: %#v", seqs[0], seqs[len(seqs)-1], first, last, seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("record seqs not contiguous at %d: %#v", i, seqs)
		}
	}
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func cloneProjectorTestSession(session sessions.SessionV2) sessions.SessionV2 {
	session.Items = cloneProjectorTestItems(session.Items)
	session.ActiveHistory = copyStrings(session.ActiveHistory)
	session.Compactions = append([]sessions.CompactionCheckpoint(nil), session.Compactions...)
	return session
}

func cloneProjectorTestItems(items []sessions.SessionItem) []sessions.SessionItem {
	out := make([]sessions.SessionItem, 0, len(items))
	for _, item := range items {
		out = append(out, cloneProjectorTestItem(item))
	}
	return out
}

func cloneProjectorTestItem(item sessions.SessionItem) sessions.SessionItem {
	item.Message = cloneProjectorTestMessagePtr(item.Message)
	item.Content = cloneProjectorTestContentPtr(item.Content)
	return item
}

func cloneProjectorTestMessagePtr(message *model.Message) *model.Message {
	if message == nil {
		return nil
	}
	clone := *message
	clone.ToolCalls = append([]model.ToolCall(nil), message.ToolCalls...)
	return &clone
}

func cloneProjectorTestContentPtr(content *sessions.StoredContent) *sessions.StoredContent {
	if content == nil {
		return nil
	}
	clone := *content
	if content.Blob != nil {
		blob := *content.Blob
		clone.Blob = &blob
	}
	return &clone
}
