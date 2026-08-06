package execution

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

type promptAppendGatedStarter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (starter *promptAppendGatedStarter) StartSessionRunWithInput(ctx context.Context, _ string, _ SessionMessageInput, _ func(SessionStreamEvent)) *SessionRun {
	return starter.newRun(ctx)
}

func (starter *promptAppendGatedStarter) StartSessionRunWithID(ctx context.Context, _ string, _ SessionMessageInput, _ string, _ func(SessionStreamEvent)) *SessionRun {
	return starter.newRun(ctx)
}

func (starter *promptAppendGatedStarter) StartSessionRunWithIDAdmitted(ctx context.Context, _ string, _ SessionMessageInput, _ string, _ func(SessionStreamEvent)) *SessionRun {
	starter.once.Do(func() { close(starter.entered) })
	select {
	case <-starter.release:
		return starter.newRun(ctx)
	case <-ctx.Done():
		return nil
	}
}

func (starter *promptAppendGatedStarter) newRun(ctx context.Context) *SessionRun {
	runCtx, cancel := context.WithCancel(ctx)
	run := &SessionRun{cancel: cancel, done: make(chan struct{}), status: SessionRunRunning, accepting: true}
	go func() {
		<-runCtx.Done()
		run.mu.Lock()
		run.status = SessionRunCancelled
		run.err = context.Canceled
		run.accepting = false
		run.mu.Unlock()
		close(run.done)
	}()
	return run
}

func newPromptAppendTestCoordinator(t *testing.T, service *Service, starter *promptAppendGatedStarter) *SessionRunCoordinator {
	t.Helper()
	coordinator := NewSessionRunCoordinator(context.Background(), starter, SessionRunCoordinatorOptions{DurableAdmitter: service})
	service.SetSessionRunCoordinator(coordinator)
	t.Cleanup(func() { coordinator.Close() })
	return coordinator
}

func startPromptAppendTestRun(t *testing.T, coordinator *SessionRunCoordinator, sessionID, runID string) *CoordinatedSessionRun {
	t.Helper()
	result := make(chan struct {
		run *CoordinatedSessionRun
		err error
	}, 1)
	go func() {
		run, _, err := coordinator.StartDurable(sessionID, SessionMessageInput{Content: "initial"}, runID, "prompt-append-test-start", nil)
		result <- struct {
			run *CoordinatedSessionRun
			err error
		}{run, err}
	}()
	select {
	case completed := <-result:
		if completed.err != nil {
			t.Fatal(completed.err)
		}
		return completed.run
	case <-time.After(5 * time.Second):
		t.Fatal("prompt append test run did not start")
		return nil
	}
}

func activeQueueContents(run *CoordinatedSessionRun) []string {
	sessionRun := run.sessionRun()
	if sessionRun == nil {
		return nil
	}
	sessionRun.mu.Lock()
	defer sessionRun.mu.Unlock()
	contents := make([]string, 0, len(sessionRun.activeQueue))
	for _, prompt := range sessionRun.activeQueue {
		contents = append(contents, prompt.Content)
	}
	return contents
}

func TestPromptAppendCompletionFailureIsOutcomeUnknownAndNeverReplayed(t *testing.T) {
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), fakeExecutionTurnRunner{supports: true})
	starter := &promptAppendGatedStarter{entered: make(chan struct{}), release: make(chan struct{})}
	coordinator := newPromptAppendTestCoordinator(t, service, starter)
	go func() { close(starter.release) }()
	run := startPromptAppendTestRun(t, coordinator, session.ID, "run-prompt-append-unknown")

	injected := false
	service.promptAppendStatusWriter = func(operationID, status string, settledAt time.Time) error {
		if status == sessions.PromptAppendStatusApplied && !injected {
			injected = true
			return errors.New("injected applied completion failure")
		}
		return service.SessionStore().SetPromptAppendClaimStatus(operationID, status, settledAt)
	}
	const content = "  exactly once despite uncertain completion  "
	if _, err := service.AppendPromptDurable(context.Background(), session.ID, run.ID(), "operation-prompt-append-unknown", content); !errors.Is(err, ErrPromptAppendOutcomeUnknown) {
		t.Fatalf("first append error=%v, want outcome_unknown", err)
	}
	claim, err := service.SessionStore().GetPromptAppendClaim("operation-prompt-append-unknown")
	if err != nil || claim.Status != sessions.PromptAppendStatusOutcomeUnknown {
		t.Fatalf("uncertain claim=%#v err=%v, want outcome_unknown", claim, err)
	}
	if got := activeQueueContents(run); len(got) != 1 || got[0] != content {
		t.Fatalf("queue after completion failure=%#v, want one exact prompt", got)
	}

	if _, err := service.AppendPromptDurable(context.Background(), session.ID, run.ID(), "operation-prompt-append-unknown", content); !errors.Is(err, ErrPromptAppendOutcomeUnknown) {
		t.Fatalf("retry error=%v, want outcome_unknown", err)
	}
	if got := activeQueueContents(run); len(got) != 1 || got[0] != content {
		t.Fatalf("queue after uncertain retry=%#v, want one exact prompt", got)
	}
}

func TestPromptAppendRejectsProvisionalRunUntilStarterReady(t *testing.T) {
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), fakeExecutionTurnRunner{supports: true})
	starter := &promptAppendGatedStarter{entered: make(chan struct{}), release: make(chan struct{})}
	coordinator := newPromptAppendTestCoordinator(t, service, starter)
	startResult := make(chan error, 1)
	go func() {
		_, _, err := coordinator.StartDurable(session.ID, SessionMessageInput{Content: "initial"}, "run-prompt-append-provisional", "prompt-append-provisional", nil)
		startResult <- err
	}()
	select {
	case <-starter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("starter did not reach provisional gate")
	}
	active, ok := coordinator.ActiveForSession(session.ID)
	if !ok || active == nil || active.ID() != "run-prompt-append-provisional" {
		t.Fatalf("provisional active handle=%#v/%t", active, ok)
	}
	if active.ActivePromptReady() {
		t.Fatal("provisional handle reported prompt readiness before starter returned")
	}
	const operationID = "operation-prompt-append-provisional"
	const content = "retry after starter readiness"
	if _, err := service.AppendPromptDurable(context.Background(), session.ID, active.ID(), operationID, content); !errors.Is(err, ErrPromptAppendRunNotActive) {
		hadClaim, claimErr := service.SessionStore().GetPromptAppendClaim(operationID)
		t.Fatalf("provisional append error=%v claim=%#v/%v, want run_not_active without claim", err, hadClaim, claimErr)
	}
	if _, err := service.SessionStore().GetPromptAppendClaim(operationID); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("provisional append left claim: %v", err)
	}

	close(starter.release)
	select {
	case err := <-startResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("starter did not complete after release")
	}
	if !active.ActivePromptReady() {
		t.Fatal("starter-returned handle did not become prompt-ready")
	}
	if result, err := service.AppendPromptDurable(context.Background(), session.ID, active.ID(), operationID, content); err != nil || !result.Accepted {
		t.Fatalf("post-readiness append result=%#v err=%v", result, err)
	}
	if result, err := service.AppendPromptDurable(context.Background(), session.ID, active.ID(), operationID, content); err != nil || !result.Accepted {
		t.Fatalf("post-readiness retry result=%#v err=%v", result, err)
	}
	if got := activeQueueContents(active); len(got) != 1 || got[0] != content {
		t.Fatalf("post-readiness queue=%#v, want one exact prompt", got)
	}
}
