package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/rexzhao/simple-agent/internal/cli/turnview"
	"github.com/rexzhao/simple-agent/internal/execution"
)

func TestTUIModelCtrlCQuitsWhenIdle(t *testing.T) {
	model := newTUIModel(context.Background(), nil, "session-1", "sai", nil)

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("Ctrl+C while idle returned nil cmd, want quit cmd")
	}
}

func TestTUIModelCtrlCCancelsActiveTurn(t *testing.T) {
	cancelled := make(chan struct{})
	model := newTUIModel(context.Background(), nil, "session-1", "sai", nil)
	model.send = func(ctx context.Context, sessionID, prompt string, emit func(execution.SessionStreamEvent)) (execution.SessionMessageResult, error) {
		<-ctx.Done()
		close(cancelled)
		return execution.SessionMessageResult{TurnID: "turn-1"}, ctx.Err()
	}

	_ = model.startTurn("hello", nil)
	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil {
		t.Fatalf("Ctrl+C while active returned cmd %#v, want nil", cmd)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("active turn was not cancelled")
	}
}

func TestTUIModelExitCommandsQuitWhenIdle(t *testing.T) {
	for _, input := range []string{"/exit", "/quit"} {
		t.Run(input, func(t *testing.T) {
			model := newTUIModel(context.Background(), nil, "session-1", "sai", nil)
			model.input = input
			_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd == nil {
				t.Fatalf("%s returned nil cmd, want quit cmd", input)
			}
		})
	}
}

func TestTUIModelCompactCommand(t *testing.T) {
	called := false
	model := newTUIModel(context.Background(), nil, "session-1", "sai", nil)
	model.compact = func(ctx context.Context, sessionID string) (execution.SessionCompactResult, error) {
		called = true
		if sessionID != "session-1" {
			t.Fatalf("CompactSession sessionID = %q, want session-1", sessionID)
		}
		return execution.SessionCompactResult{}, nil
	}
	model.input = "/compact"

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/compact returned nil cmd")
	}
	_, _ = model.Update(cmd())
	if !called {
		t.Fatal("compact callback was not called")
	}
	assertTUIBlock(t, model, turnview.BlockSystem, "compacted session context", "completed")
}

func TestTUIModelPendingSessionCreatedOnFirstPrompt(t *testing.T) {
	created := false
	model := newTUIModel(context.Background(), nil, "", "sai", nil)
	model.createSession = func() (execution.SessionDetail, error) {
		created = true
		return execution.SessionDetail{
			ID:           "session-1",
			Provider:     "paperhub",
			ModelProfile: "glm",
			ModelID:      "glm-5.2",
		}, nil
	}
	model.send = func(ctx context.Context, sessionID, prompt string, emit func(execution.SessionStreamEvent)) (execution.SessionMessageResult, error) {
		<-ctx.Done()
		return execution.SessionMessageResult{TurnID: "turn-1"}, ctx.Err()
	}
	model.input = "hello"

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("first prompt returned nil create-session cmd")
	}
	if !model.creating || !model.active || model.sessionID != "" {
		t.Fatalf("model before create = active %v creating %v session %q, want creating active with no session", model.active, model.creating, model.sessionID)
	}
	_, startCmd := model.Update(cmd())
	if !created || model.sessionID != "session-1" || model.view.Status.ModelID != "glm-5.2" {
		t.Fatalf("model after create = created %v session %q status %#v", created, model.sessionID, model.view.Status)
	}
	if startCmd == nil {
		t.Fatal("session create did not start turn")
	}
	if model.activeCancel != nil {
		model.activeCancel()
	}
}

func TestTUIModelPendingMailboxSessionCreateFailureFailsTask(t *testing.T) {
	ctx := context.Background()
	queue := newMailboxQueue()
	if _, err := queue.post("mailbox needs session"); err != nil {
		t.Fatalf("post() error = %v", err)
	}
	task, err := queue.dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue() error = %v", err)
	}

	model := newTUIModel(ctx, nil, "", "sai", queue)
	model.createSession = func() (execution.SessionDetail, error) {
		return execution.SessionDetail{}, errors.New("bad config")
	}
	cmd := model.submitPrompt(task.Prompt, task)
	if cmd == nil {
		t.Fatal("mailbox prompt without session returned nil create command")
	}
	_, _ = model.Update(cmd())

	snapshot, _ := queue.get(task.ID)
	if snapshot.Status != mailboxTaskFailed || snapshot.Error != "bad config" {
		t.Fatalf("task snapshot = %#v, want failed bad config", snapshot)
	}
	assertTUIBlock(t, model, turnview.BlockMailbox, "mailbox task "+task.ID+" failed", "failed")
	assertTUIBlock(t, model, turnview.BlockSystem, "session create failed", "failed")
}

func TestTUIModelMailboxWaitsOnlyWhenIdle(t *testing.T) {
	model := newTUIModel(context.Background(), nil, "session-1", "sai", newMailboxQueue())
	model.active = true
	if cmd := model.waitMailboxCmd(); cmd != nil {
		t.Fatalf("waitMailboxCmd while active = %#v, want nil", cmd)
	}
	model.active = false
	if cmd := model.waitMailboxCmd(); cmd == nil {
		t.Fatal("waitMailboxCmd while idle = nil, want command")
	}
}

func TestTUIModelDefersMailboxMessageResolvedDuringActiveTurn(t *testing.T) {
	ctx := context.Background()
	queue := newMailboxQueue()
	if _, err := queue.post("queued while active"); err != nil {
		t.Fatalf("post() error = %v", err)
	}
	task, err := queue.dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue() error = %v", err)
	}

	model := newTUIModel(ctx, nil, "session-1", "sai", queue)
	model.active = true
	model.waitingMailbox = true
	_, cmd := model.Update(tuiMailboxMsg{task: task})
	if cmd != nil {
		t.Fatalf("mailbox message during active turn returned cmd %#v, want nil", cmd)
	}
	if model.deferredTask != task {
		t.Fatalf("deferredTask = %#v, want task", model.deferredTask)
	}
	snapshot, _ := queue.get(task.ID)
	if snapshot.Status != mailboxTaskQueued {
		t.Fatalf("task status after deferred mailbox msg = %q, want queued", snapshot.Status)
	}

	model.send = func(ctx context.Context, sessionID, prompt string, emit func(execution.SessionStreamEvent)) (execution.SessionMessageResult, error) {
		return execution.SessionMessageResult{TurnID: "turn-1"}, nil
	}
	model.finalOutput = func(sessionID, turnID string) (string, error) {
		return "final assistant output", nil
	}
	model.eventsOpen = false
	model.sendDoneSeen = true
	cmd = model.finishTurnIfReady()
	if cmd == nil {
		t.Fatal("finishTurnIfReady did not start deferred mailbox task")
	}
	snapshot, _ = queue.get(task.ID)
	if snapshot.Status != mailboxTaskRunning {
		t.Fatalf("task status after active turn finished = %q, want running", snapshot.Status)
	}
	_, _ = model.Update(receiveTUIEventClosed(t, model.activeEventCh))
	_, _ = model.Update(receiveTUISendDone(t, model.activeDoneCh))
	snapshot, _ = queue.get(task.ID)
	if snapshot.Status != mailboxTaskCompleted {
		t.Fatalf("task status after deferred turn = %q, want completed", snapshot.Status)
	}
}

func TestTUIModelMailboxTaskCompletesWithFinalOutputOnly(t *testing.T) {
	ctx := context.Background()
	queue := newMailboxQueue()
	if _, err := queue.post("review prompt"); err != nil {
		t.Fatalf("post() error = %v", err)
	}
	task, err := queue.dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue() error = %v", err)
	}

	model := newTUIModel(ctx, nil, "session-1", "sai", queue)
	model.send = func(ctx context.Context, sessionID, prompt string, emit func(execution.SessionStreamEvent)) (execution.SessionMessageResult, error) {
		return execution.SessionMessageResult{TurnID: "turn-1"}, nil
	}
	model.finalOutput = func(sessionID, turnID string) (string, error) {
		return "final assistant output", nil
	}

	_ = model.startTurn(task.Prompt, task)
	eventClosed := receiveTUIEventClosed(t, model.activeEventCh)
	_, _ = model.Update(eventClosed)
	done := receiveTUISendDone(t, model.activeDoneCh)
	_, _ = model.Update(done)

	snapshot, ok := queue.get(task.ID)
	if !ok {
		t.Fatalf("task %s missing", task.ID)
	}
	if snapshot.Status != mailboxTaskCompleted || snapshot.Result != "final assistant output" {
		t.Fatalf("task snapshot = %#v, want completed final output", snapshot)
	}
	assertTUIBlock(t, model, turnview.BlockMailbox, "mailbox task "+task.ID+" completed", "completed")
}

func receiveTUIEventClosed(t *testing.T, ch <-chan execution.SessionStreamEvent) tuiEventMsg {
	t.Helper()
	select {
	case event, ok := <-ch:
		return tuiEventMsg{event: event, ok: ok}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event channel close")
		return tuiEventMsg{}
	}
}

func receiveTUISendDone(t *testing.T, ch <-chan attachSendResult) tuiSendDoneMsg {
	t.Helper()
	select {
	case result := <-ch:
		return tuiSendDoneMsg{result: result.result, err: result.err, task: result.mailboxTask}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for send result")
		return tuiSendDoneMsg{}
	}
}

func assertTUIBlock(t *testing.T, model *tuiModel, kind turnview.BlockKind, title, status string) {
	t.Helper()
	for _, block := range model.view.Blocks {
		if block.Kind == kind && block.Title == title && block.Status == status {
			return
		}
	}
	t.Fatalf("missing TUI block kind=%s title=%q status=%q in %#v", kind, title, status, model.view.Blocks)
}
