package execution

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type coordinatorTestStarter struct {
	mu      sync.Mutex
	runs    map[string]*SessionRun
	release map[string]chan struct{}
}

func newCoordinatorTestStarter() *coordinatorTestStarter {
	return &coordinatorTestStarter{
		runs:    make(map[string]*SessionRun),
		release: make(map[string]chan struct{}),
	}
}

func (starter *coordinatorTestStarter) StartSessionRunWithInput(ctx context.Context, sessionID string, _ SessionMessageInput, emit func(SessionStreamEvent)) *SessionRun {
	runCtx, cancel := context.WithCancel(ctx)
	run := &SessionRun{
		cancel:    cancel,
		done:      make(chan struct{}),
		status:    SessionRunRunning,
		accepting: true,
	}
	release := make(chan struct{})
	starter.mu.Lock()
	starter.runs[sessionID] = run
	starter.release[sessionID] = release
	starter.mu.Unlock()

	go func() {
		if emit != nil {
			emit(NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-" + sessionID}))
		}
		var err error
		select {
		case <-release:
		case <-runCtx.Done():
			err = context.Canceled
		}
		run.mu.Lock()
		run.accepting = false
		if err != nil {
			run.status = SessionRunCancelled
			run.err = err
		} else {
			run.status = SessionRunCommitted
			run.result = SessionMessageResult{Status: "committed", TurnID: "turn-" + sessionID}
		}
		run.mu.Unlock()
		close(run.done)
	}()
	return run
}

func (starter *coordinatorTestStarter) complete(sessionID string) {
	starter.mu.Lock()
	release := starter.release[sessionID]
	starter.mu.Unlock()
	close(release)
}

func TestSessionRunCoordinatorEnforcesSessionAndGlobalAdmission(t *testing.T) {
	starter := newCoordinatorTestStarter()
	nextID := 0
	coordinator := NewSessionRunCoordinator(context.Background(), starter, SessionRunCoordinatorOptions{
		MaxConcurrentRuns: 1,
		NewRunID: func() (string, error) {
			nextID++
			return fmt.Sprintf("run-%d", nextID), nil
		},
	})
	defer coordinator.Close()

	first, err := coordinator.Start("session-a", SessionMessageInput{Content: "one"}, nil)
	if err != nil {
		t.Fatalf("Start(first) error = %v", err)
	}
	if first.ID() != "run-1" || first.SessionID() != "session-a" {
		t.Fatalf("first run = %q/%q, want run-1/session-a", first.ID(), first.SessionID())
	}
	if _, err := coordinator.Start("session-a", SessionMessageInput{Content: "duplicate"}, nil); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("Start(same session) error = %v, want ErrSessionBusy", err)
	}
	if _, err := coordinator.Start("session-b", SessionMessageInput{Content: "over capacity"}, nil); !errors.Is(err, ErrSessionRunCoordinatorCapacity) {
		t.Fatalf("Start(over capacity) error = %v, want ErrSessionRunCoordinatorCapacity", err)
	}

	active, ok := coordinator.ActiveForSession("session-a")
	if !ok || active != first {
		t.Fatalf("ActiveForSession() = %#v/%t, want first run", active, ok)
	}
	var descriptors []SessionRunDescriptor
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		descriptors = coordinator.ActiveRuns()
		if len(descriptors) == 1 && descriptors[0].TurnID == "turn-session-a" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(descriptors) != 1 || descriptors[0].RunID != first.ID() || descriptors[0].TurnID != "turn-session-a" {
		t.Fatalf("ActiveRuns() = %#v, want active first run with observed turn", descriptors)
	}

	starter.complete("session-a")
	if _, err := first.Wait(); err != nil {
		t.Fatalf("first.Wait() error = %v", err)
	}
	waitForCoordinatorRunRemoval(t, coordinator, first.ID())

	second, err := coordinator.Start("session-b", SessionMessageInput{Content: "after release"}, nil)
	if err != nil {
		t.Fatalf("Start(after release) error = %v", err)
	}
	second.Cancel()
	if _, err := second.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("second.Wait() error = %v, want context.Canceled", err)
	}
}

func TestSessionRunCoordinatorCloseCancelsRunsAndRejectsStarts(t *testing.T) {
	starter := newCoordinatorTestStarter()
	coordinator := NewSessionRunCoordinator(context.Background(), starter, SessionRunCoordinatorOptions{
		NewRunID: func() (string, error) { return "run-close", nil },
	})
	run, err := coordinator.Start("session-close", SessionMessageInput{Content: "wait"}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	coordinator.Close()
	if _, err := run.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("run.Wait() after Close error = %v, want context.Canceled", err)
	}
	if _, err := coordinator.Start("session-new", SessionMessageInput{Content: "no"}, nil); !errors.Is(err, ErrSessionRunCoordinatorClosed) {
		t.Fatalf("Start() after Close error = %v, want ErrSessionRunCoordinatorClosed", err)
	}
	if got := coordinator.ActiveRuns(); len(got) != 0 {
		t.Fatalf("ActiveRuns() after Close = %#v, want empty", got)
	}
}

func waitForCoordinatorRunRemoval(t *testing.T, coordinator *SessionRunCoordinator, runID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := coordinator.Get(runID); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("coordinator run %q was not removed", runID)
}
