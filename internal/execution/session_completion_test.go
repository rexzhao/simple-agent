package execution

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestSessionStartContinueParentWakesAfterChildSettlement(t *testing.T) {
	parentStarted := make(chan struct{})
	releaseParent := make(chan struct{})
	var mu sync.Mutex
	parentCalls := 0
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			mu.Lock()
			isParent := request.Session.ParentSessionID == ""
			if isParent {
				parentCalls++
			}
			call := parentCalls
			mu.Unlock()
			if isParent && call == 1 {
				close(parentStarted)
				select {
				case <-releaseParent:
				case <-ctx.Done():
					return SessionTurnResult{}, ctx.Err()
				}
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, parent, _, _ := newSessionToolTestSessions(t, t.TempDir(), runner)
	coordinator := NewSessionRunCoordinator(context.Background(), service, SessionRunCoordinatorOptions{})
	service.SetSessionRunCoordinator(coordinator)
	defer func() {
		service.ClearSessionRunCoordinator(coordinator)
		coordinator.Close()
	}()

	parentRun, err := coordinator.Start(parent.ID, SessionMessageInput{Content: "parent work"}, nil)
	if err != nil {
		t.Fatalf("start parent: %v", err)
	}
	select {
	case <-parentStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("parent did not start")
	}

	executor := &sessionToolExecutor{service: service, coordinator: coordinator, caller: parent}
	result := executeSessionTool(t, executor, ToolSessionStart, map[string]any{
		"prompt":    "child work",
		"on_settle": "continue_parent",
	})
	payload := decodeSessionToolPayload(t, result)
	childID := requiredPayloadString(t, payload, "session_id")
	childRunID := requiredPayloadString(t, payload, "run_id")
	if payload["on_settle"] != "continue_parent" {
		t.Fatalf("session_start on_settle payload = %#v", payload)
	}

	waitForCompletionTest(t, func() bool {
		deliveries, err := service.sessionStore.ListSessionInbox(parent.ID)
		if err != nil || len(deliveries) != 1 {
			return false
		}
		return deliveries[0].ChildSessionID == childID && deliveries[0].ChildRunID == childRunID && deliveries[0].Status == sessions.SessionInboxStatusDelivered
	})
	mu.Lock()
	if parentCalls != 1 {
		t.Fatalf("parent calls while active = %d, want 1", parentCalls)
	}
	mu.Unlock()

	close(releaseParent)
	if _, err := parentRun.Wait(); err != nil {
		t.Fatalf("parent initial run: %v", err)
	}
	stableDelivery := sessions.NewSessionCompletionDeliveryID(parent.ID, childID, childRunID)
	stableRunID := completionParentRunID(stableDelivery)
	waitForCompletionTest(t, func() bool {
		run, err := service.sessionStore.GetRun(parent.ID, stableRunID)
		return err == nil && run.Status != sessions.RunStatusRunning
	})
	waitForCompletionTest(t, func() bool {
		deliveries, err := service.sessionStore.ListSessionInbox(parent.ID)
		return err == nil && len(deliveries) == 1 && deliveries[0].Status == sessions.SessionInboxStatusConsumed
	})

	parentRuns, err := service.sessionStore.ListRuns(parent.ID)
	if err != nil {
		t.Fatalf("list parent runs: %v", err)
	}
	var initial, continuation sessions.RunRecord
	for _, run := range parentRuns {
		switch run.ID {
		case parentRun.ID():
			initial = run
		case stableRunID:
			continuation = run
		}
	}
	if initial.ID == "" || continuation.ID == "" || continuation.PreviousRunID != initial.ID {
		t.Fatalf("parent run chain = stable %q runs %#v initial %#v continuation %#v", stableRunID, parentRuns, initial, continuation)
	}
	var input SessionMessageInput
	if err := json.Unmarshal(continuation.InputPayload, &input); err != nil {
		t.Fatalf("decode continuation input: %v", err)
	}
	if !input.Internal || input.DeliveryID != stableDelivery || !strings.Contains(input.Content, childID) || !strings.Contains(input.Content, childRunID) {
		t.Fatalf("continuation input = %#v", input)
	}
	mu.Lock()
	if parentCalls != 2 {
		t.Fatalf("parent calls after child settlement = %d, want 2", parentCalls)
	}
	mu.Unlock()
}

func TestSessionSendOnSettleWakesParentAfterChildRunSettles(t *testing.T) {
	parentStarted := make(chan struct{})
	releaseParent := make(chan struct{})
	releaseChild := make(chan struct{})
	var mu sync.Mutex
	parentCalls := 0
	childCalls := 0
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			mu.Lock()
			isParent := request.Session.ParentSessionID == ""
			if isParent {
				parentCalls++
			} else {
				childCalls++
			}
			call := childCalls
			mu.Unlock()
			if isParent {
				if parentCalls == 1 {
					close(parentStarted)
					select {
					case <-releaseParent:
					case <-ctx.Done():
						return SessionTurnResult{}, ctx.Err()
					}
				}
				return SessionTurnResult{Incremental: true}, nil
			}
			// The child run the parent sent into (via session_send on_settle).
			if call == 1 {
				close(releaseChild)
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, parent, child, _ := newSessionToolTestSessions(t, t.TempDir(), runner)
	coordinator := NewSessionRunCoordinator(context.Background(), service, SessionRunCoordinatorOptions{})
	service.SetSessionRunCoordinator(coordinator)
	defer func() {
		service.ClearSessionRunCoordinator(coordinator)
		coordinator.Close()
	}()
	executor := &sessionToolExecutor{service: service, coordinator: coordinator, caller: parent}

	parentRun, err := coordinator.Start(parent.ID, SessionMessageInput{Content: "parent work"}, nil)
	if err != nil {
		t.Fatalf("start parent: %v", err)
	}
	select {
	case <-parentStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("parent did not start")
	}

	// Parent sends a message to its idle child with on_settle=continue_parent.
	sendResult := executeSessionTool(t, executor, ToolSessionSend, map[string]any{
		"session_id": child.ID, "message": "child task", "on_settle": "continue_parent",
	})
	sendPayload := decodeSessionToolPayload(t, sendResult)
	if sendPayload["delivery"] != "started" || sendPayload["on_settle"] != "continue_parent" {
		t.Fatalf("session_send on_settle payload = %#v", sendPayload)
	}
	childRunID := requiredPayloadString(t, sendPayload, "run_id")
	select {
	case <-releaseChild:
	case <-time.After(5 * time.Second):
		t.Fatal("child run did not start")
	}

	// The parent is still active, so the wakeup must wait until the parent
	// settles. The inbox delivery is registered but not yet dispatched.
	waitForCompletionTest(t, func() bool {
		deliveries, err := service.sessionStore.ListSessionInbox(parent.ID)
		if err != nil || len(deliveries) != 1 {
			return false
		}
		return deliveries[0].ChildSessionID == child.ID && deliveries[0].ChildRunID == childRunID && deliveries[0].Status == sessions.SessionInboxStatusPending
	})
	mu.Lock()
	if parentCalls != 1 {
		t.Fatalf("parent calls while active = %d, want 1", parentCalls)
	}
	mu.Unlock()

	// Release the parent. The completion dispatcher then starts the parent's
	// continuation run with the stable delivery id.
	close(releaseParent)
	if _, err := parentRun.Wait(); err != nil {
		t.Fatalf("parent initial run: %v", err)
	}
	stableDelivery := sessions.NewSessionCompletionDeliveryID(parent.ID, child.ID, childRunID)
	stableRunID := completionParentRunID(stableDelivery)
	waitForCompletionTest(t, func() bool {
		run, err := service.sessionStore.GetRun(parent.ID, stableRunID)
		return err == nil && run.Status != sessions.RunStatusRunning
	})
	waitForCompletionTest(t, func() bool {
		deliveries, err := service.sessionStore.ListSessionInbox(parent.ID)
		return err == nil && len(deliveries) == 1 && deliveries[0].Status == sessions.SessionInboxStatusConsumed
	})
	mu.Lock()
	if parentCalls != 2 {
		t.Fatalf("parent calls after child settlement = %d, want 2", parentCalls)
	}
	mu.Unlock()

	parentRuns, err := service.sessionStore.ListRuns(parent.ID)
	if err != nil {
		t.Fatalf("list parent runs: %v", err)
	}
	var initial, continuation sessions.RunRecord
	for _, run := range parentRuns {
		switch run.ID {
		case parentRun.ID():
			initial = run
		case stableRunID:
			continuation = run
		}
	}
	if initial.ID == "" || continuation.ID == "" || continuation.PreviousRunID != initial.ID {
		t.Fatalf("parent run chain = stable %q runs %#v initial %#v continuation %#v", stableRunID, parentRuns, initial, continuation)
	}
	var input SessionMessageInput
	if err := json.Unmarshal(continuation.InputPayload, &input); err != nil {
		t.Fatalf("decode continuation input: %v", err)
	}
	if !input.Internal || input.DeliveryID != stableDelivery || !strings.Contains(input.Content, child.ID) || !strings.Contains(input.Content, childRunID) {
		t.Fatalf("continuation input = %#v", input)
	}
}

func TestSessionStartOnSettleDefaultsToNoneAndRejectsInvalidValue(t *testing.T) {
	service, parent, _, _ := newSessionToolTestSessions(t, t.TempDir(), fakeExecutionTurnRunner{supports: true})
	coordinator := NewSessionRunCoordinator(context.Background(), service, SessionRunCoordinatorOptions{})
	service.SetSessionRunCoordinator(coordinator)
	defer func() {
		service.ClearSessionRunCoordinator(coordinator)
		coordinator.Close()
	}()
	executor := &sessionToolExecutor{service: service, coordinator: coordinator, caller: parent}
	defaultResult := executeSessionTool(t, executor, ToolSessionStart, map[string]any{"prompt": "plain"})
	defaultPayload := decodeSessionToolPayload(t, defaultResult)
	if defaultPayload["on_settle"] != "none" {
		t.Fatalf("default on_settle payload = %#v", defaultPayload)
	}
	invalid := executeSessionTool(t, executor, ToolSessionStart, map[string]any{"prompt": "bad", "on_settle": "later"})
	assertSessionToolErrorCode(t, invalid, "invalid_arguments")
	blank := executeSessionTool(t, executor, ToolSessionStart, map[string]any{"prompt": "bad", "on_settle": ""})
	assertSessionToolErrorCode(t, blank, "invalid_arguments")
}

func waitForCompletionTest(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition did not become true")
	}
}
