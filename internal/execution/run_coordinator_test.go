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

type synchronousCoordinatorTestStarter struct {
	admitted                  *bool
	emittedBeforeRegistration *bool
}

type cancellationBlockingCoordinatorStarter struct {
	started   chan struct{}
	startOnce sync.Once
	returnRun bool
}

func (starter *cancellationBlockingCoordinatorStarter) StartSessionRunWithInput(
	ctx context.Context,
	_ string,
	_ SessionMessageInput,
	_ func(SessionStreamEvent),
) *SessionRun {
	starter.startOnce.Do(func() { close(starter.started) })
	<-ctx.Done()
	if !starter.returnRun {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	run := &SessionRun{
		cancel:    cancel,
		done:      make(chan struct{}),
		status:    SessionRunRunning,
		accepting: true,
	}
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

func (starter *synchronousCoordinatorTestStarter) StartSessionRunWithInput(
	ctx context.Context,
	_ string,
	_ SessionMessageInput,
	emit func(SessionStreamEvent),
) *SessionRun {
	if starter.admitted != nil && !*starter.admitted && starter.emittedBeforeRegistration != nil {
		*starter.emittedBeforeRegistration = true
	}
	if emit != nil {
		emit(NewSessionStreamEvent("turn.started", map[string]any{"turn_id": "turn-sync"}))
	}
	_, cancel := context.WithCancel(ctx)
	run := &SessionRun{
		cancel:    cancel,
		done:      make(chan struct{}),
		status:    SessionRunCommitted,
		accepting: false,
		result:    SessionMessageResult{Status: "committed", TurnID: "turn-sync"},
	}
	close(run.done)
	return run
}

func TestSessionRunCoordinatorAdmitsReplayBeforeSynchronousStarterEvent(t *testing.T) {
	admitted := false
	emittedBeforeRegistration := false
	starter := &synchronousCoordinatorTestStarter{
		admitted:                  &admitted,
		emittedBeforeRegistration: &emittedBeforeRegistration,
	}
	var coordinator *SessionRunCoordinator
	coordinator = NewSessionRunCoordinator(context.Background(), starter, SessionRunCoordinatorOptions{
		NewRunID: func() (string, error) { return "run-sync-admission", nil },
		OnRunAdmitted: func(run *CoordinatedSessionRun) error {
			if got, ok := coordinator.Get(run.ID()); !ok || got != run {
				t.Fatalf("admitted run lookup = %#v/%t, want reserved handle", got, ok)
			}
			admitted = true
			return nil
		},
	})
	defer coordinator.Close()

	run, err := coordinator.Start("session-sync-admission", SessionMessageInput{Content: "sync"}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if emittedBeforeRegistration {
		t.Fatal("synchronous starter emitted before coordinator admission callback")
	}
	if _, err := run.Wait(); err != nil {
		t.Fatalf("run.Wait() error = %v", err)
	}
}

func TestSessionRunCoordinatorRejectsConcurrentStartsForOneSession(t *testing.T) {
	starter := newCoordinatorTestStarter()
	var idMu sync.Mutex
	nextID := 0
	coordinator := NewSessionRunCoordinator(context.Background(), starter, SessionRunCoordinatorOptions{
		NewRunID: func() (string, error) {
			idMu.Lock()
			defer idMu.Unlock()
			nextID++
			return fmt.Sprintf("run-concurrent-%d", nextID), nil
		},
	})
	defer coordinator.Close()

	const attempts = 16
	results := make(chan struct {
		run *CoordinatedSessionRun
		err error
	}, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := coordinator.Start("session-concurrent", SessionMessageInput{Content: "one"}, nil)
			results <- struct {
				run *CoordinatedSessionRun
				err error
			}{run: run, err: err}
		}()
	}
	wg.Wait()
	close(results)

	var admitted *CoordinatedSessionRun
	busy := 0
	for result := range results {
		if result.err == nil {
			if admitted != nil {
				t.Fatal("more than one concurrent start was admitted")
			}
			admitted = result.run
			continue
		}
		if errors.Is(result.err, ErrSessionBusy) {
			busy++
			continue
		}
		t.Fatalf("concurrent Start() error = %v, want ErrSessionBusy", result.err)
	}
	if admitted == nil || busy != attempts-1 {
		t.Fatalf("concurrent admission result = run:%v busy:%d, want one run and %d busy errors", admitted != nil, busy, attempts-1)
	}
	admitted.Cancel()
	if _, err := admitted.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("admitted run.Wait() error = %v, want context.Canceled", err)
	}
}

func TestSessionRunCoordinatorCloseCancelsAdmissionBeforeWaiting(t *testing.T) {
	for _, returnRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("return-run-%t", returnRun), func(t *testing.T) {
			starter := &cancellationBlockingCoordinatorStarter{
				started:   make(chan struct{}),
				returnRun: returnRun,
			}
			coordinator := NewSessionRunCoordinator(context.Background(), starter, SessionRunCoordinatorOptions{
				NewRunID: func() (string, error) { return "run-close-admission", nil },
			})
			t.Cleanup(coordinator.Close)

			startResult := make(chan error, 1)
			go func() {
				_, err := coordinator.Start("session-close-admission", SessionMessageInput{Content: "wait"}, nil)
				startResult <- err
			}()
			select {
			case <-starter.started:
			case <-time.After(time.Second):
				t.Fatal("starter did not begin")
			}

			closeDone := make(chan struct{})
			go func() {
				coordinator.Close()
				close(closeDone)
			}()
			select {
			case <-closeDone:
			case <-time.After(time.Second):
				t.Fatal("Close() deadlocked while starter waited for context cancellation")
			}
			select {
			case err := <-startResult:
				if returnRun && err != nil {
					t.Fatalf("Start() error = %v, want successful admission before shutdown", err)
				}
				if !returnRun && err == nil {
					t.Fatal("Start() succeeded after starter rolled back admission")
				}
			case <-time.After(time.Second):
				t.Fatal("Start() did not finish after coordinator cancellation")
			}

			coordinator.mu.Lock()
			byIDCount := len(coordinator.byID)
			activeCount := len(coordinator.activeBySession)
			coordinator.mu.Unlock()
			if byIDCount != 0 || activeCount != 0 {
				t.Fatalf("coordinator indexes after Close() = byID:%d active:%d, want empty", byIDCount, activeCount)
			}
		})
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

func TestSessionRunCoordinatorObservesEventsAndSettlementForAllStarts(t *testing.T) {
	starter := newCoordinatorTestStarter()
	events := make(chan SessionStreamEvent, 1)
	settled := make(chan struct {
		runID  string
		result SessionMessageResult
		err    error
	}, 1)
	coordinator := NewSessionRunCoordinator(context.Background(), starter, SessionRunCoordinatorOptions{
		NewRunID: func() (string, error) { return "run-observed", nil },
		OnRunEvent: func(run *CoordinatedSessionRun, event SessionStreamEvent) {
			if run.ID() != "run-observed" || run.SessionID() != "session-observed" {
				t.Errorf("observed run = %q/%q", run.ID(), run.SessionID())
			}
			events <- event
		},
		OnRunSettled: func(run *CoordinatedSessionRun, result SessionMessageResult, err error) {
			settled <- struct {
				runID  string
				result SessionMessageResult
				err    error
			}{runID: run.ID(), result: result, err: err}
		},
	})
	defer coordinator.Close()

	run, err := coordinator.Start("session-observed", SessionMessageInput{Content: "observe"}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case event := <-events:
		if event["type"] != "turn.started" || event["turn_id"] != "turn-session-observed" {
			t.Fatalf("observed event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not observe run event")
	}

	starter.complete("session-observed")
	if _, err := run.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	select {
	case observed := <-settled:
		if observed.runID != run.ID() || observed.result.Status != "committed" || observed.err != nil {
			t.Fatalf("settled observation = %#v", observed)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not observe run settlement")
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
