package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestSessionStreamUsageEventIncludesCacheDetails(t *testing.T) {
	event, ok := sessionStreamEventFromModelEvent("turn-1", 2, model.UsageEvent{Usage: model.Usage{
		InputTokens: 10, OutputTokens: 5, TotalTokens: 25,
		CachedTokens: 8, CacheWriteTokens: 2, ReasoningTokens: 3,
	}}, true)
	if !ok {
		t.Fatal("sessionStreamEventFromModelEvent() ok = false, want true")
	}
	for key, want := range map[string]any{
		"type": "usage.updated", "turn_id": "turn-1", "agent_iteration": 2,
		"input_tokens": 10, "output_tokens": 5, "total_tokens": 25,
		"cached_tokens": 8, "cache_write_tokens": 2, "reasoning_tokens": 3,
	} {
		if got := event[key]; got != want {
			t.Fatalf("event[%q] = %#v, want %#v; event = %#v", key, got, want, event)
		}
	}
}

func TestSessionStreamMessageSnapshotCarriesAssistantItemBinding(t *testing.T) {
	event, ok := sessionStreamEventFromModelEvent("turn-1", 2, model.AssistantMessageUpdatedEvent{
		ItemID: "assistant-2", AgentIteration: 2, Revision: 1,
		Message: model.Message{Role: model.MessageRoleAssistant, ReasoningContent: "thinking"},
	}, true)
	if !ok {
		t.Fatal("sessionStreamEventFromModelEvent() ok = false, want true")
	}
	for key, want := range map[string]any{
		"type": "assistant.message.updated", "turn_id": "turn-1", "agent_iteration": 2,
		"reasoning": "thinking", "item_id": "assistant-2", "message_revision": "1",
	} {
		if got := event[key]; got != want {
			t.Fatalf("event[%q] = %#v, want %#v; event = %#v", key, got, want, event)
		}
	}
}

func TestSessionStreamProviderRetryEventIncludesBackoffDetails(t *testing.T) {
	event, ok := sessionStreamEventFromModelEvent("turn-1", 6, model.ProviderRetryEvent{
		Attempt:     2,
		MaxAttempts: 3,
		Delay:       time.Second,
		Reason:      "server_error",
	}, true)
	if !ok {
		t.Fatal("sessionStreamEventFromModelEvent() ok = false, want true")
	}
	for key, want := range map[string]any{
		"type": "provider.retrying", "turn_id": "turn-1", "agent_iteration": 6,
		"attempt": 2, "max_attempts": 3, "delay_ms": int64(1000), "reason": "server_error",
	} {
		if got := event[key]; got != want {
			t.Fatalf("event[%q] = %#v, want %#v; event = %#v", key, got, want, event)
		}
	}

	for _, reason := range []string{"rate_limited", "timeout", "transport"} {
		reasonEvent, ok := sessionStreamEventFromModelEvent("turn-1", 6, model.ProviderRetryEvent{
			Attempt:     3,
			MaxAttempts: 5,
			Delay:       time.Second,
			Reason:      reason,
		}, true)
		if !ok {
			t.Fatalf("sessionStreamEventFromModelEvent() ok = false for reason %q, want true", reason)
		}
		if got := reasonEvent["reason"]; got != reason {
			t.Fatalf("reason passthrough: event[reason] = %#v, want %q", got, reason)
		}
	}
}

func TestSessionToolDisplayArgumentsIncludesEditDiffInputs(t *testing.T) {
	displayed := sessionToolDisplayArguments("edit_file", `{"path":"notes.txt","old":"before","new":"after","extra":"hidden"}`)
	var arguments map[string]string
	if err := json.Unmarshal([]byte(displayed), &arguments); err != nil {
		t.Fatalf("displayed arguments are not JSON: %v", err)
	}
	want := map[string]string{"path": "notes.txt", "old": "before", "new": "after"}
	if len(arguments) != len(want) {
		t.Fatalf("displayed arguments = %#v, want %#v", arguments, want)
	}
	for key, value := range want {
		if arguments[key] != value {
			t.Fatalf("displayed arguments[%q] = %q, want %q; all = %#v", key, arguments[key], value, arguments)
		}
	}
}

func TestSessionToolDisplayArgumentsIncludesApplyPatchContent(t *testing.T) {
	patch := "--- a/notes.txt\n+++ b/notes.txt\n@@ -1 +1 @@\n-old\n+new\n"
	raw, err := json.Marshal(map[string]string{"patch": patch, "extra": "hidden"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	displayed := sessionToolDisplayArguments("apply_patch", string(raw))
	var arguments map[string]string
	if err := json.Unmarshal([]byte(displayed), &arguments); err != nil {
		t.Fatalf("displayed arguments are not JSON: %v", err)
	}
	if got := arguments["patch"]; got != patch {
		t.Fatalf("displayed patch = %q, want %q", got, patch)
	}
	if len(arguments) != 1 {
		t.Fatalf("displayed arguments = %#v, want only patch", arguments)
	}
}

func TestSessionToolDisplayArgumentsIncludesGrepSearchMode(t *testing.T) {
	displayed := sessionToolDisplayArguments("grep_files", `{"path":"src","query":"foo.*bar","literal":true,"case_sensitive":false,"exclude":["vendor/**"]}`)
	var arguments map[string]any
	if err := json.Unmarshal([]byte(displayed), &arguments); err != nil {
		t.Fatalf("displayed arguments are not JSON: %v", err)
	}
	for key, want := range map[string]any{"path": "src", "query": "foo.*bar", "literal": true, "case_sensitive": false} {
		if arguments[key] != want {
			t.Fatalf("displayed arguments[%q] = %#v, want %#v; all = %#v", key, arguments[key], want, arguments)
		}
	}
	if _, ok := arguments["exclude"]; ok {
		t.Fatalf("displayed arguments = %#v, want exclude omitted", arguments)
	}
}

// TestSessionStreamBlockedCallbackDoesNotBlockRunner verifies that a slow
// presentation callback never blocks provider emission or tool execution: the
// runner must reach completion while emit is blocked, and Send only waits for
// the sink to flush. After release, coalesced text is exact and the terminal
// event is last. No callback fires after Send returns.
func TestSessionStreamBlockedCallbackDoesNotBlockRunner(t *testing.T) {
	home := t.TempDir()
	const deltaCount = 120 // exceeds the bus subscriber buffer (64)
	var expectedText string
	runnerDone := make(chan struct{})
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			request.Emit(model.AssistantMessageStartedEvent{ItemID: "assistant-blocked", AgentIteration: 1})
			for i := 0; i < deltaCount; i++ {
				chunk := fmt.Sprintf("[%d]", i)
				expectedText += chunk
				request.Emit(model.AssistantMessageUpdatedEvent{ItemID: "assistant-blocked", AgentIteration: 1, Revision: uint64(i + 1), Message: model.Message{Role: model.MessageRoleAssistant, Content: expectedText}})
			}
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "final")); err != nil {
				return SessionTurnResult{}, err
			}
			close(runnerDone)
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	release := make(chan struct{})
	var mu sync.Mutex
	var events []SessionStreamEvent
	emit := func(event SessionStreamEvent) {
		<-release
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}

	type sendResult struct {
		result SessionMessageResult
		err    error
	}
	sendDone := make(chan sendResult, 1)
	go func() {
		result, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", emit)
		sendDone <- sendResult{result: result, err: err}
	}()

	// The runner must complete even though every emit callback is blocked.
	select {
	case <-runnerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not complete; blocked emit prevented provider emission")
	}

	// Send may still be blocked flushing the sink; it must not have returned.
	select {
	case res := <-sendDone:
		t.Fatalf("Send returned before emit was released: %#v", res)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)

	select {
	case res := <-sendDone:
		if res.err != nil {
			t.Fatalf("SendSessionMessageWithEvents() error = %v", res.err)
		}
		if res.result.Status != "committed" {
			t.Fatalf("result = %#v, want committed", res.result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send did not return after release")
	}

	mu.Lock()
	defer mu.Unlock()
	types := sessionStreamEventTypes(events)
	if len(types) == 0 || types[0] != "turn.started" {
		t.Fatalf("first event = %#v, want turn.started", types)
	}
	if types[len(types)-1] != "turn.committed" {
		t.Fatalf("last event = %#v, want turn.committed", types)
	}
	if got := countString(types, "assistant.message.updated"); got > 2 {
		t.Fatalf("assistant message update count = %d, want at most 2", got)
	}
	if !sessionStreamEventsContain(events, "assistant.message.updated", "content", expectedText) {
		t.Fatalf("events do not contain final assistant snapshot %q", expectedText)
	}

	// No callback fires after Send returns.
	n := len(events)
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(events) != n {
		t.Fatalf("events grew after return: %d -> %d", n, len(events))
	}
}

func TestGetSessionChatItemsPageAlignTurnExtendsToWholeTurns(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: true})

	// Layout: turn-1 = items 1-2, turn-2 = items 3-6, turn-3 = items 7-8.
	turns := []string{"turn-1", "turn-1", "turn-2", "turn-2", "turn-2", "turn-2", "turn-3", "turn-3"}
	for index, turn := range turns {
		item := sessions.SessionItemFromMessage(fmt.Sprintf("msg-%d", index+1), model.Message{
			Role:    model.MessageRoleUser,
			Content: fmt.Sprintf("message-%d", index+1),
		})
		item.TurnID = turn
		if _, err := service.sessionStore.AppendItem(session.ID, item); err != nil {
			t.Fatalf("AppendItem(%d) error = %v", index+1, err)
		}
	}

	// Default (align off): the strict limit window starts mid-turn-2.
	plain, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{Limit: 4})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(plain) error = %v", err)
	}
	if got := sessionItemContents(plain.Items); !sameStringSlice(got, []string{"message-5", "message-6", "message-7", "message-8"}) {
		t.Fatalf("plain latest contents = %#v", got)
	}

	// align on: the oldest edge extends back to turn-2's first item, so the
	// page exceeds the limit instead of cutting a turn.
	aligned, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{Limit: 4, AlignTurn: true})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(aligned) error = %v", err)
	}
	if got := sessionItemContents(aligned.Items); !sameStringSlice(got, []string{"message-3", "message-4", "message-5", "message-6", "message-7", "message-8"}) {
		t.Fatalf("aligned latest contents = %#v", got)
	}
	if !aligned.HasMoreBefore || aligned.HasMoreAfter {
		t.Fatalf("aligned latest flags = before %t after %t", aligned.HasMoreBefore, aligned.HasMoreAfter)
	}

	// A turn longer than the limit still comes back whole.
	longTurn, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{Limit: 1, AlignTurn: true})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(long turn) error = %v", err)
	}
	if got := sessionItemContents(longTurn.Items); !sameStringSlice(got, []string{"message-7", "message-8"}) {
		t.Fatalf("long turn contents = %#v", got)
	}

	// An aligned before page that already starts at a turn boundary is unchanged.
	before, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{BeforeSeq: aligned.OldestSeq, Limit: 2, AlignTurn: true})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(before) error = %v", err)
	}
	if got := sessionItemContents(before.Items); !sameStringSlice(got, []string{"message-1", "message-2"}) {
		t.Fatalf("aligned before contents = %#v", got)
	}
	if before.HasMoreBefore || !before.HasMoreAfter {
		t.Fatalf("aligned before flags = before %t after %t", before.HasMoreBefore, before.HasMoreAfter)
	}

	// after pages keep their explicit oldest edge even with align on.
	after, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{AfterSeq: before.NewestSeq, Limit: 4, AlignTurn: true})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(after) error = %v", err)
	}
	if got := sessionItemContents(after.Items); !sameStringSlice(got, []string{"message-3", "message-4", "message-5", "message-6"}) {
		t.Fatalf("aligned after contents = %#v", got)
	}
}

func TestGetSessionChatItemsPageAlignTurnStopsAtMissingTurnID(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: true})

	// Layout: item 1 = turn-1, item 2 untagged (legacy data), items 3-4 = turn-2.
	turns := []string{"turn-1", "", "turn-2", "turn-2"}
	for index, turn := range turns {
		item := sessions.SessionItemFromMessage(fmt.Sprintf("msg-%d", index+1), model.Message{
			Role:    model.MessageRoleUser,
			Content: fmt.Sprintf("message-%d", index+1),
		})
		item.TurnID = turn
		if _, err := service.sessionStore.AppendItem(session.ID, item); err != nil {
			t.Fatalf("AppendItem(%d) error = %v", index+1, err)
		}
	}

	// An untagged window edge is a boundary of its own: extension must not
	// pull the earlier turn-1 item across it.
	latest, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{Limit: 3, AlignTurn: true})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(latest) error = %v", err)
	}
	if got := sessionItemContents(latest.Items); !sameStringSlice(got, []string{"message-2", "message-3", "message-4"}) {
		t.Fatalf("latest contents = %#v", got)
	}

	// Turn extension still applies within the tagged items themselves.
	turnOnly, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{Limit: 1, AlignTurn: true})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(turn only) error = %v", err)
	}
	if got := sessionItemContents(turnOnly.Items); !sameStringSlice(got, []string{"message-3", "message-4"}) {
		t.Fatalf("turn only contents = %#v", got)
	}
}

func TestGetSessionChatItemsPageSupportsBeforeAndAfterCursors(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: true})
	for i := 1; i <= 6; i++ {
		message := model.Message{Role: model.MessageRoleUser, Content: fmt.Sprintf("message-%d", i)}
		if _, err := service.sessionStore.AppendItem(session.ID, sessions.SessionItemFromMessage(fmt.Sprintf("msg-%d", i), message)); err != nil {
			t.Fatalf("AppendItem(%d) error = %v", i, err)
		}
	}

	latest, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{Limit: 2})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(latest) error = %v", err)
	}
	if got := sessionItemContents(latest.Items); !sameStringSlice(got, []string{"message-5", "message-6"}) {
		t.Fatalf("latest contents = %#v", got)
	}
	if !latest.HasMoreBefore || latest.HasMoreAfter {
		t.Fatalf("latest page flags = before %t after %t", latest.HasMoreBefore, latest.HasMoreAfter)
	}

	before, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{BeforeSeq: latest.OldestSeq, Limit: 2})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(before) error = %v", err)
	}
	if got := sessionItemContents(before.Items); !sameStringSlice(got, []string{"message-3", "message-4"}) {
		t.Fatalf("before contents = %#v", got)
	}
	if !before.HasMoreBefore || !before.HasMoreAfter {
		t.Fatalf("before page flags = before %t after %t", before.HasMoreBefore, before.HasMoreAfter)
	}

	after, err := service.GetSessionChatItemsPage(session.ID, SessionItemsOptions{AfterSeq: before.NewestSeq, Limit: 1})
	if err != nil {
		t.Fatalf("GetSessionChatItemsPage(after) error = %v", err)
	}
	if got := sessionItemContents(after.Items); !sameStringSlice(got, []string{"message-5"}) {
		t.Fatalf("after contents = %#v", got)
	}
	if !after.HasMoreBefore || !after.HasMoreAfter {
		t.Fatalf("after page flags = before %t after %t", after.HasMoreBefore, after.HasMoreAfter)
	}
}

func TestSessionToolDisplayArgumentsKeepsOnlyUsefulPresentationFields(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		arguments string
		want      string
	}{
		{name: "file path without content", toolName: "write_file", arguments: `{"path":"notes.txt","content":"secret body"}`, want: `{"path":"notes.txt"}`},
		{name: "shell command", toolName: "shell", arguments: `{"command":"go test ./...","timeout_ms":1000}`, want: `{"command":"go test ./..."}`},
		{name: "invalid json", toolName: "read_file", arguments: `{`, want: ""},
		{name: "session_start keeps every field", toolName: "session_start", arguments: `{"name":"Review","provider":"paperhub","model":"grok-4.5","prompt":"please review the plan"}`, want: `{"name":"Review","provider":"paperhub","model":"grok-4.5","prompt":"please review the plan"}`},
		{name: "session_send keeps every field", toolName: "session_send", arguments: `{"session_id":"20260729T101411.113294500Z-edc0c394","mode":"steer","message":"please retry"}`, want: `{"session_id":"20260729T101411.113294500Z-edc0c394","mode":"steer","message":"please retry"}`},
		{name: "session tool invalid json", toolName: "session_wait", arguments: `{`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sessionToolDisplayArguments(test.toolName, test.arguments); got != test.want {
				t.Fatalf("sessionToolDisplayArguments() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWebEvalPresentationSummaryIsSharedByStreamAndHistory(t *testing.T) {
	raw := `{"code":"你好","timeout_ms":5000}`
	want := `{"code_bytes":6,"timeout_ms":5000}`
	for _, event := range []model.Event{
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "requested", Name: WebEvalToolName, Arguments: raw}},
		model.ToolStartedEvent{ToolCall: model.ToolCall{ID: "started", Name: WebEvalToolName, Arguments: raw}},
	} {
		mapped, ok := sessionStreamEventFromModelEvent("turn-1", 1, event, true)
		if !ok {
			t.Fatalf("sessionStreamEventFromModelEvent(%T) ok=false", event)
		}
		if got := mapped["arguments"]; got != want {
			t.Fatalf("%T presentation arguments = %#v, want %q", event, got, want)
		}
	}

	store := sessions.NewV2Store(t.TempDir())
	service := &Service{sessionStore: store}
	state, err := store.SaveMetadata(sessions.SessionV2{ID: "history-session", ShowReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	dto, err := service.sessionItemDTOWithState(state.ID, sessions.SessionItem{
		ID:      "assistant-item",
		Message: &model.Message{Role: model.MessageRoleAssistant, ToolCalls: []model.ToolCall{{ID: "history", Name: WebEvalToolName, Arguments: raw}}},
	}, state)
	if err != nil {
		t.Fatal(err)
	}
	if dto.Message == nil || len(dto.Message.ToolCalls) != 1 || dto.Message.ToolCalls[0].Arguments != want {
		t.Fatalf("history presentation DTO = %#v, want web.eval summary %q", dto.Message, want)
	}

	for _, input := range []string{`{`, `{"code":""}`, `{"code_bytes":0}`, `{"code_bytes":-1}`, `{"code":"secret","timeout_ms":99}`} {
		if got := sessionToolDisplayArguments(WebEvalToolName, input); got != webEvalPresentationRedacted {
			t.Fatalf("malformed web.eval presentation %q -> %q, want redacted", input, got)
		}
	}
	if got := sessionToolDisplayArguments(WebEvalToolName, want); got != want {
		t.Fatalf("summary was not idempotent: got %q, want %q", got, want)
	}
}

func TestWebEvalToolProgressDoesNotProducePresentationEvent(t *testing.T) {
	event, ok := sessionStreamEventFromModelEvent("turn-1", 1, model.ToolCallDeltaEvent{
		ID: "call-1", Name: WebEvalToolName, ArgumentsDelta: `{"code":"secret"}`,
	}, true)
	if ok || event != nil {
		t.Fatalf("web.eval progress event = %#v, ok=%v; want no presentation event", event, ok)
	}
}

func sessionItemContents(items []SessionItem) []string {
	contents := make([]string, 0, len(items))
	for _, item := range items {
		if item.Message != nil && item.Message.Content != nil {
			contents = append(contents, item.Message.Content.Inline)
		}
	}
	return contents
}

// TestSessionStreamFailureOrdersTurnFailedAfterPriorEvents verifies that on
// failure, turn.failed is emitted after all prior mapped events and is last.
func TestSessionStreamFailureOrdersTurnFailedAfterPriorEvents(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			request.Emit(model.AssistantMessageStartedEvent{ItemID: "assistant-failed", AgentIteration: 1})
			request.Emit(model.AssistantMessageUpdatedEvent{ItemID: "assistant-failed", AgentIteration: 1, Revision: 1, Message: model.Message{Role: model.MessageRoleAssistant, Content: "partial "}})
			request.Emit(model.AssistantMessageUpdatedEvent{ItemID: "assistant-failed", AgentIteration: 1, Revision: 2, Message: model.Message{Role: model.MessageRoleAssistant, Content: "partial output"}})
			return SessionTurnResult{}, errors.New("provider exploded")
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	var events []SessionStreamEvent
	_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", func(event SessionStreamEvent) {
		events = append(events, event)
	})
	if !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("error = %v, want ErrTurnFailed", err)
	}
	types := sessionStreamEventTypes(events)
	if len(types) == 0 || types[0] != "turn.started" {
		t.Fatalf("first event = %#v, want turn.started", types)
	}
	if got := countString(types, "turn.failed"); got != 1 {
		t.Fatalf("turn.failed count = %d, want 1", got)
	}
	failedIdx := indexOfString(types, "turn.failed")
	if failedIdx != len(types)-1 {
		t.Fatalf("turn.failed index = %d, want last; events = %#v", failedIdx, types)
	}
	textIdx := indexOfString(types, "assistant.message.updated")
	if textIdx < 0 || textIdx >= failedIdx {
		t.Fatalf("assistant.message.updated(%d) must precede turn.failed(%d): %#v", textIdx, failedIdx, types)
	}
	// The exact coalescing of the two text deltas depends on drain timing; what
	// must hold is that no text is lost and the combined text is exact.
	if !sessionStreamEventsContain(events, "assistant.message.updated", "content", "partial output") {
		t.Fatalf("events = %#v, want final partial snapshot", events)
	}
	appendIdx := indexOfString(types, "item.created")
	if appendIdx < 0 || appendIdx >= failedIdx {
		t.Fatalf("item.created(%d) must precede turn.failed(%d): %#v", appendIdx, failedIdx, types)
	}
}

// TestSessionStreamCoalescesOnlyConsecutiveSameTypeDeltas verifies end-to-end
// that, while presentation is blocked, queued consecutive message snapshots
// for one item are replaced by the newest revision; every other event remains
// ordered.
func TestSessionStreamCoalescesOnlyConsecutiveMessageSnapshots(t *testing.T) {
	home := t.TempDir()
	runnerDone := make(chan struct{})
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			request.Emit(model.AgentIterationStartedEvent{Iteration: 2})
			request.Emit(model.AssistantMessageStartedEvent{ItemID: "assistant-coalesce", AgentIteration: 2})
			request.Emit(model.AssistantMessageUpdatedEvent{ItemID: "assistant-coalesce", AgentIteration: 2, Revision: 1, Message: model.Message{Role: model.MessageRoleAssistant, Content: "a"}})
			request.Emit(model.AssistantMessageUpdatedEvent{ItemID: "assistant-coalesce", AgentIteration: 2, Revision: 2, Message: model.Message{Role: model.MessageRoleAssistant, Content: "ab", ReasoningContent: "r1r2"}})
			request.Emit(model.AssistantMessageUpdatedEvent{ItemID: "assistant-coalesce", AgentIteration: 2, Revision: 3, Message: model.Message{Role: model.MessageRoleAssistant, Content: "abc", ReasoningContent: "r1r2"}})
			request.Emit(model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "read_file"}})
			request.Emit(model.AssistantMessageUpdatedEvent{ItemID: "assistant-coalesce", AgentIteration: 2, Revision: 4, Message: model.Message{Role: model.MessageRoleAssistant, Content: "abcde", ReasoningContent: "r1r2"}})
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
				return SessionTurnResult{}, err
			}
			close(runnerDone)
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, err := NewServiceWithOptions(home, ServiceOptions{TurnRunner: runner})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "coalesce-repo")
	project, err := service.CreateProject(projectRoot, "Coalesce Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	showReasoning := true
	saveToolResults := true
	session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		CreatedCWD:      project.Project.Root,
		ConfigPath:      filepath.Join(project.Project.Root, ".agents", "sai.yaml"),
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		ShowReasoning:   &showReasoning,
		SaveToolResults: &saveToolResults,
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	// Block every emit until release so all deltas queue and coalesce at submit
	// time, making the merged runs deterministic.
	release := make(chan struct{})
	var mu sync.Mutex
	var events []SessionStreamEvent
	emit := func(event SessionStreamEvent) {
		<-release
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}

	type sendResult struct {
		err error
	}
	sendDone := make(chan sendResult, 1)
	go func() {
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", emit)
		sendDone <- sendResult{err: err}
	}()

	select {
	case <-runnerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not complete; blocked emit prevented provider emission")
	}
	// Send is now blocked flushing the sink.
	select {
	case res := <-sendDone:
		t.Fatalf("Send returned before release: %#v", res)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)

	select {
	case res := <-sendDone:
		if res.err != nil {
			t.Fatalf("SendSessionMessageWithEvents() error = %v", res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send did not return after release")
	}

	mu.Lock()
	defer mu.Unlock()
	gotTypes := sessionStreamEventTypes(events)
	if len(gotTypes) == 0 || gotTypes[0] != "turn.started" {
		t.Fatalf("first event = %#v, want turn.started", gotTypes)
	}
	if gotTypes[len(gotTypes)-1] != "turn.committed" {
		t.Fatalf("last event = %#v, want turn.committed", gotTypes)
	}
	// Exact merge runs can split if the emitter snapshots a leading delta with
	// turn.started before blocking; what must hold is exact text preservation in
	// submission order. The precise one-op-per-run invariant is covered by the
	// sink unit test.
	if !sessionStreamEventsContain(events, "assistant.message.updated", "content", "abcde") {
		t.Fatalf("events = %#v, want latest cumulative snapshot", events)
	}
	if got := countString(gotTypes, "tool.requested"); got != 1 {
		t.Fatalf("tool.requested count = %d, want 1", got)
	}
	if got := countString(gotTypes, "agent.iteration.started"); got != 1 {
		t.Fatalf("agent.iteration.started count = %d, want 1", got)
	}
	for _, event := range events {
		eventType, _ := event["type"].(string)
		switch eventType {
		case "agent.iteration.started", "assistant.message.started", "assistant.message.updated", "tool.requested":
			if got, _ := event["agent_iteration"].(int); got != 2 {
				t.Fatalf("%s agent_iteration = %#v, want 2", eventType, event["agent_iteration"])
			}
		}
	}
	if got := countString(gotTypes, "item.appended"); got != 0 {
		t.Fatalf("item.appended count = %d, want no new public appended events", got)
	}
	if got := countString(gotTypes, "item.created"); got != 2 {
		t.Fatalf("item.created count = %d, want user and assistant creation", got)
	}
}

// TestSessionStreamSuccessCommittedLastNoCallbacksAfterReturn verifies that on
// success turn.committed is the terminal event and no callback fires after Send
// returns.
func TestSessionStreamSuccessCommittedLastNoCallbacksAfterReturn(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			request.Emit(model.TextDeltaEvent{Text: "hello"})
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	var mu sync.Mutex
	var events []SessionStreamEvent
	if _, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", func(event SessionStreamEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}); err != nil {
		t.Fatalf("SendSessionMessageWithEvents() error = %v", err)
	}
	mu.Lock()
	types := sessionStreamEventTypes(events)
	if len(types) == 0 || types[len(types)-1] != "turn.committed" {
		t.Fatalf("last event = %#v, want turn.committed", types)
	}
	n := len(events)
	mu.Unlock()

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(events) != n {
		t.Fatalf("events grew after return: %d -> %d", n, len(events))
	}
	mu.Unlock()
}

func indexOfString(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}

func sessionStreamEventTexts(events []SessionStreamEvent, eventType string) []string {
	var texts []string
	for _, event := range events {
		if event["type"] != eventType {
			continue
		}
		text, _ := event["text"].(string)
		texts = append(texts, text)
	}
	return texts
}

func joinSessionStreamEventTexts(events []SessionStreamEvent, eventType string) string {
	var sb strings.Builder
	for _, text := range sessionStreamEventTexts(events, eventType) {
		sb.WriteString(text)
	}
	return sb.String()
}

// TestSessionEventSinkCoalescesAtSubmitTime verifies that consecutive snapshots
// for the same assistant item are replaced by the newest revision while emit is
// blocked on an earlier event.
func TestSessionEventSinkCoalescesAtSubmitTime(t *testing.T) {
	release := make(chan struct{})
	blocked := make(chan struct{}, 1)
	var mu sync.Mutex
	var delivered []SessionStreamEvent
	sink := newSessionEventSink(func(event SessionStreamEvent) {
		select {
		case blocked <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		delivered = append(delivered, event)
		mu.Unlock()
	})

	// Non-delta event first. Wait until the emitter is blocked inside emit on it,
	// which means it has snapshotted turn.started and cleared sink.ops; this
	// guarantees the consecutive deltas below all coalesce into one queued op.
	sink.submit(NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-1"}))
	<-blocked

	const updateCount = 200
	for i := 0; i < updateCount; i++ {
		sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{
			"turn_id": "turn-1", "item_id": "assistant-1",
			"message_revision": fmt.Sprintf("%d", i+1), "content": fmt.Sprintf("[%d]", i),
		}))
	}

	// A different turn_id breaks the run; it must not merge into the prior run.
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{
		"turn_id": "turn-2", "item_id": "assistant-2", "message_revision": "1", "content": "other",
	}))
	// A non-delta event also breaks the run.
	sink.submit(NewSessionStreamEvent("usage.updated", map[string]any{"turn_id": "turn-1"}))

	sink.mu.Lock()
	updateOps := 0
	for _, op := range sink.ops {
		if op.event["type"] == "assistant.message.updated" && op.event["item_id"] == "assistant-1" {
			updateOps++
		}
	}
	sink.mu.Unlock()
	if updateOps != 1 {
		t.Fatalf("queued assistant-1 updates = %d, want 1", updateOps)
	}

	close(release)
	sink.close()
	sink.wait()

	mu.Lock()
	defer mu.Unlock()
	types := sessionStreamEventTypes(delivered)
	// turn.started (blocked), then the latest snapshot, the other-turn snapshot,
	// and usage.updated.
	wantTypes := []string{"turn.started", "assistant.message.updated", "assistant.message.updated", "usage.updated"}
	if len(types) != len(wantTypes) {
		t.Fatalf("delivered types = %#v, want %#v", types, wantTypes)
	}
	for i, want := range wantTypes {
		if types[i] != want {
			t.Fatalf("delivered types[%d] = %q, want %q; full = %#v", i, types[i], want, types)
		}
	}
	if delivered[1]["message_revision"] != "200" || delivered[1]["content"] != "[199]" {
		t.Fatalf("delivered latest snapshot = %#v", delivered[1])
	}
	if got, _ := delivered[2]["turn_id"].(string); got != "turn-2" {
		t.Fatalf("delivered other-turn delta turn_id = %q, want turn-2", got)
	}
}

func TestSessionEventSinkOverflowIsAnExplicitRecoveryBoundary(t *testing.T) {
	release := make(chan struct{})
	blocked := make(chan struct{}, 1)
	var mu sync.Mutex
	var types []string
	sink := newSessionEventSinkWithBounds(func(event SessionStreamEvent) {
		if len(types) == 0 {
			select {
			case blocked <- struct{}{}:
			default:
			}
		}
		<-release
		mu.Lock()
		if event != nil {
			typeName, _ := event["type"].(string)
			types = append(types, typeName)
		}
		mu.Unlock()
	}, 2, 1<<20)

	// Keep the first callback blocked so the following operations remain in
	// the bounded queue. The third queued operation must not be silently
	// dropped while later operations continue: it transitions the sink to the
	// explicit recovery marker and clears the pending queue.
	sink.submit(NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-1"}))
	<-blocked
	sink.submit(NewSessionStreamEvent("usage.updated", map[string]any{"n": 1}))
	sink.submit(NewSessionStreamEvent("usage.updated", map[string]any{"n": 2}))
	sink.submit(NewSessionStreamEvent("usage.updated", map[string]any{"n": 3}))
	sink.close()
	close(release)
	sink.wait()

	mu.Lock()
	got := append([]string(nil), types...)
	mu.Unlock()
	want := []string{"turn.started", "run.resync_required"}
	if len(got) != len(want) {
		t.Fatalf("sink delivered types = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sink delivered types[%d] = %q, want %q; full = %#v", i, got[i], want[i], got)
		}
	}
	sink.mu.Lock()
	if sink.queuedBytes != 0 || !sink.overflowed || !sink.markerDelivered {
		t.Fatalf("sink after overflow = queuedBytes:%d overflowed:%t markerDelivered:%t", sink.queuedBytes, sink.overflowed, sink.markerDelivered)
	}
	sink.mu.Unlock()
}

func TestSessionEventSinkCoalescedDeltaBytesCanOverflow(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	events := make(chan SessionStreamEvent, 4)
	sink := newSessionEventSinkWithBounds(func(event SessionStreamEvent) {
		once.Do(func() {
			close(started)
			<-release
		})
		events <- event
	}, 16, 800)
	// The first delta is admitted; repeated coalescing must charge the added
	// UTF-8 bytes rather than only the one queued message.
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{"turn_id": "turn-1", "item_id": "assistant-1", "message_revision": "1", "content": strings.Repeat("a", 80)}))
	<-started
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{"turn_id": "turn-1", "item_id": "assistant-1", "message_revision": "2", "content": strings.Repeat("b", 160)}))
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{"turn_id": "turn-1", "item_id": "assistant-1", "message_revision": "3", "content": strings.Repeat("c", 400)}))
	sink.close()
	close(release)
	sink.wait()
	var got []string
	for {
		select {
		case event := <-events:
			if event != nil {
				typeName, _ := event["type"].(string)
				got = append(got, typeName)
			}
		default:
			if len(got) != 2 || got[0] != "assistant.message.updated" || got[1] != "run.resync_required" {
				t.Fatalf("byte overflow delivered types = %#v", got)
			}
			return
		}
	}
}

func TestSessionEventSinkKeepsLatestAssistantRevision(t *testing.T) {
	release := make(chan struct{})
	blocked := make(chan struct{}, 1)
	var mu sync.Mutex
	var delivered []SessionStreamEvent
	sink := newSessionEventSink(func(event SessionStreamEvent) {
		select {
		case blocked <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		delivered = append(delivered, event)
		mu.Unlock()
	})
	sink.submit(NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-1"}))
	<-blocked
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{
		"turn_id": "turn-1", "agent_iteration": 1, "content": "a", "item_id": "assistant-1", "message_revision": "1",
	}))
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{
		"turn_id": "turn-1", "agent_iteration": 1, "content": "ab", "item_id": "assistant-1", "message_revision": "2",
	}))
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{
		"turn_id": "turn-1", "agent_iteration": 1, "content": "abc", "item_id": "assistant-1", "message_revision": "3",
	}))
	close(release)
	sink.close()
	sink.wait()

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 {
		t.Fatalf("delivered events = %#v, want turn.started plus latest snapshot", delivered)
	}
	if delivered[1]["message_revision"] != "3" || delivered[1]["content"] != "abc" || delivered[1]["item_id"] != "assistant-1" {
		t.Fatalf("latest snapshot = %#v", delivered[1])
	}
}

func TestSessionEventSinkUsesCompleteMessageIdentityAndMonotonicRevision(t *testing.T) {
	release := make(chan struct{})
	blocked := make(chan struct{}, 1)
	var delivered []SessionStreamEvent
	var mu sync.Mutex
	sink := newSessionEventSink(func(event SessionStreamEvent) {
		select {
		case blocked <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		delivered = append(delivered, event)
		mu.Unlock()
	})
	sink.submit(NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-0"}))
	<-blocked
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{"turn_id": "turn-1", "agent_iteration": 1, "item_id": "same", "message_revision": "2", "content": "new"}))
	// Older revision for the same identity cannot replace the queued snapshot.
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{"turn_id": "turn-1", "agent_iteration": 1, "item_id": "same", "message_revision": "1", "content": "old"}))
	// The same item ID in another turn is a distinct message and cannot merge.
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{"turn_id": "turn-2", "agent_iteration": 1, "item_id": "same", "message_revision": "1", "content": "other"}))
	close(release)
	sink.close()
	sink.wait()
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 3 || delivered[1]["content"] != "new" || delivered[2]["content"] != "other" {
		t.Fatalf("delivered snapshots = %#v", delivered)
	}
}

func TestSessionEventSinkPreservesMessageIdentityAcrossReplay(t *testing.T) {
	release := make(chan struct{})
	blocked := make(chan struct{}, 1)
	var mu sync.Mutex
	var delivered []SessionStreamEvent
	sink := newSessionEventSink(func(event SessionStreamEvent) {
		select {
		case blocked <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		delivered = append(delivered, event)
		mu.Unlock()
	})
	sink.submit(NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-1"}))
	<-blocked
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{
		"turn_id": "turn-1", "agent_iteration": 1, "item_id": "assistant-1", "message_revision": "1", "reasoning": "old ", "content": "",
	}))
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{
		"turn_id": "turn-1", "agent_iteration": 1, "item_id": "assistant-1", "message_revision": "2", "reasoning": "old reasoning", "content": "",
	}))
	// A new assistant item is a new logical reasoning stream even when the
	// provider text and turn/iteration happen to be identical.
	sink.submit(NewSessionStreamEvent("assistant.message.updated", map[string]any{
		"turn_id": "turn-1", "agent_iteration": 1, "item_id": "assistant-2", "message_revision": "1", "reasoning": "same", "content": "",
	}))
	close(release)
	sink.close()
	sink.wait()

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 3 {
		t.Fatalf("delivered events = %#v, want turn.started plus two message snapshots", delivered)
	}
	if delivered[1]["item_id"] != "assistant-1" || delivered[1]["reasoning"] != "old reasoning" {
		t.Fatalf("first reasoning replay = %#v, want assistant-1 with coalesced text", delivered[1])
	}
	if delivered[2]["item_id"] != "assistant-2" || delivered[2]["reasoning"] != "same" {
		t.Fatalf("second reasoning replay = %#v, want assistant-2 identity", delivered[2])
	}
}

// turnFailedSecret is injected into the prompt and, where a runner/planner error
// is simulated, into that error's text. It must never appear in any
// SessionStreamEvent: turn.failed carries only a stable code and canned message.
const turnFailedSecret = "LEAKED-TURN-SECRET-7c9f"

// TestSessionStreamTurnFailedSafePayloadByStage verifies that every reachable
// failure stage after turn.started emits exactly one terminal turn.failed event
// (last, unique) carrying a stable code and short canned message, and that no
// stream event leaks the underlying error text, prompt, or other secret.
func TestSessionStreamTurnFailedSafePayloadByStage(t *testing.T) {
	prompt := "prompt " + turnFailedSecret

	t.Run("compaction_failed", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			plan: func(ctx context.Context, request SessionTurnRequest) (SessionCompactionResult, error) {
				return SessionCompactionResult{}, fmt.Errorf("compaction planner leaked %s", turnFailedSecret)
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(t, events, "compaction_failed", "compaction planning failed", turnFailedSecret)
	})

	t.Run("runner_failed", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				request.Emit(model.TextDeltaEvent{Text: "partial output"})
				return SessionTurnResult{}, fmt.Errorf("runner leaked %s", turnFailedSecret)
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(t, events, "runner_failed", "turn runner failed", turnFailedSecret)
	})

	t.Run("context_window_exceeded", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				return SessionTurnResult{}, fmt.Errorf("request model: %w", &contextwindow.BudgetExceededError{
					EstimatedInputTokens: 1200,
					ContextWindow:        1000,
				})
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		assertSafeTurnFailedTerminal(
			t,
			events,
			"context_window_exceeded",
			"estimated context usage reached the model context window (1200/1000 tokens)",
			turnFailedSecret,
		)
	})

	t.Run("model_request_timeout", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				return SessionTurnResult{}, fmt.Errorf("request model: %w", &httpstream.RequestTimeoutError{
					Timeout:  60 * time.Second,
					Attempts: 2,
				})
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		assertSafeTurnFailedTerminal(t, events, "model_request_timeout", "model service did not return response headers after 2 attempts (1m0s each)", turnFailedSecret)
	})

	t.Run("model_stream_idle_timeout", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				return SessionTurnResult{}, fmt.Errorf("request model: %w", &httpstream.StreamIdleTimeoutError{Timeout: 2 * time.Minute})
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		assertSafeTurnFailedTerminal(t, events, "model_stream_idle_timeout", "model response stream produced no data for 2m0s", turnFailedSecret)
	})

	t.Run("model_http_error", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				return SessionTurnResult{}, fmt.Errorf("request model: %w", &httpstream.StatusError{
					StatusCode: 429,
					Status:     "429 Too Many Requests",
					Body:       `{"error":{"message":"slow down and try later"}}`,
					Attempts:   2,
				})
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(
			t,
			events,
			"model_http_error",
			`429: model provider asked us to slow down; try again later after 2 attempts`,
			turnFailedSecret,
		)
	})

	t.Run("model_http_error_without_body", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				return SessionTurnResult{}, fmt.Errorf("request model: %w", &httpstream.StatusError{
					StatusCode: 500,
					Body:       "   ",
				})
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		assertSafeTurnFailedTerminal(t, events, "model_http_error", "model provider is temporarily unavailable (HTTP 500)", turnFailedSecret)
	})

	t.Run("model_provider_error", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				return SessionTurnResult{}, fmt.Errorf("read stream: %w", &model.ProviderError{Message: "rate_limit_error: Your rate limit is exceeded (code 429)"})
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(t, events, "model_provider_error", "model provider asked us to slow down; try again later", turnFailedSecret)
	})

	t.Run("model_connection_failed", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				return SessionTurnResult{}, fmt.Errorf("request model: %w", &net.OpError{Op: "dial", Err: fmt.Errorf("connection refused")})
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("error = %v, want ErrTurnFailed", err)
		}
		assertSafeTurnFailedTerminal(t, events, "model_connection_failed", "could not reach the model provider (connection failed)", turnFailedSecret)
	})

	t.Run("runner_not_incremental", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
					return SessionTurnResult{}, err
				}
				return SessionTurnResult{Incremental: false}, nil
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)
		var events []SessionStreamEvent
		_, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, prompt, func(event SessionStreamEvent) {
			events = append(events, event)
		})
		if err == nil || !strings.Contains(err.Error(), "turn runner did not use incremental persistence") {
			t.Fatalf("error = %v, want not-incremental", err)
		}
		if strings.Contains(err.Error(), turnFailedSecret) {
			t.Fatalf("returned error leaks secret: %v", err)
		}
		assertSafeTurnFailedTerminal(t, events, "runner_not_incremental", "turn runner did not persist incrementally", turnFailedSecret)
	})

}

func assertSafeTurnFailedTerminal(t *testing.T, events []SessionStreamEvent, wantCode, wantMessage, secret string) {
	t.Helper()
	types := sessionStreamEventTypes(events)
	if len(types) == 0 || types[0] != "turn.started" {
		t.Fatalf("first event = %#v, want turn.started", types)
	}
	if got := countString(types, "turn.failed"); got != 1 {
		t.Fatalf("turn.failed count = %d, want 1; events = %#v", got, types)
	}
	failedIdx := indexOfString(types, "turn.failed")
	if failedIdx != len(types)-1 {
		t.Fatalf("turn.failed index = %d, want last; events = %#v", failedIdx, types)
	}
	failed := events[failedIdx]
	if got, _ := failed["code"].(string); got != wantCode {
		t.Fatalf("turn.failed code = %q, want %q", got, wantCode)
	}
	if got, _ := failed["message"].(string); got != wantMessage {
		t.Fatalf("turn.failed message = %q, want %q", got, wantMessage)
	}
	if got, _ := failed["turn_id"].(string); got == "" {
		t.Fatalf("turn.failed missing turn_id: %#v", failed)
	}
	for i, event := range events {
		if strings.Contains(fmt.Sprintf("%v", event), secret) {
			t.Fatalf("event %d leaks secret %q: %v", i, secret, event)
		}
	}
}
