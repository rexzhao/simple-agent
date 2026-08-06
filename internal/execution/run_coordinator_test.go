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

type mailboxTestObserver struct {
	mu        sync.Mutex
	order     []string
	losses    chan string
	lossRuns  chan string
	events    chan SessionStreamEvent
	entered   chan struct{}
	block     chan struct{}
	blockOnce sync.Once
	// blockGates is a buffered sequence of one-shot gates used by tests which
	// need to force several independent overflow transitions on one mailbox.
	// The non-blocking receive means later runs are normal once the gates are
	// exhausted.
	blockGates   chan chan struct{}
	enteredGates chan struct{}
	lossBlock    chan struct{}
}

func (observer *mailboxTestObserver) RunAdmitted(run *CoordinatedSessionRun) {
	observer.record("admitted:" + run.ID())
	observer.maybeBlock()
}

func (observer *mailboxTestObserver) RunAdmissionFailed(run *CoordinatedSessionRun) {
	observer.record("admission_failed:" + run.ID())
}

func (observer *mailboxTestObserver) RunEvent(run *CoordinatedSessionRun, event SessionStreamEvent) {
	observer.record("event:" + run.ID())
	if observer.events != nil {
		observer.events <- event
	}
}

func (observer *mailboxTestObserver) RunSettled(run *CoordinatedSessionRun, _ SessionMessageResult, _ error) {
	observer.record("settled:" + run.ID())
}

func (observer *mailboxTestObserver) RunEventObserverLoss(run *CoordinatedSessionRun, reason string) {
	if observer.lossRuns != nil {
		observer.lossRuns <- run.ID()
	}
	if observer.losses != nil {
		observer.losses <- reason
	}
	if observer.lossBlock != nil {
		<-observer.lossBlock
	}
}

func (observer *mailboxTestObserver) record(value string) {
	observer.mu.Lock()
	observer.order = append(observer.order, value)
	observer.mu.Unlock()
}

func (observer *mailboxTestObserver) maybeBlock() {
	if observer.blockGates != nil {
		select {
		case gate := <-observer.blockGates:
			if observer.enteredGates != nil {
				observer.enteredGates <- struct{}{}
			}
			<-gate
		default:
		}
		return
	}
	if observer.block == nil {
		return
	}
	observer.blockOnce.Do(func() {
		if observer.entered != nil {
			close(observer.entered)
		}
		<-observer.block
	})
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

func TestSessionRunCoordinatorObserverMailboxDoesNotBlockProducer(t *testing.T) {
	starter := &synchronousCoordinatorTestStarter{}
	observer := &mailboxTestObserver{entered: make(chan struct{}), block: make(chan struct{})}
	coordinator := NewSessionRunCoordinator(context.Background(), starter, SessionRunCoordinatorOptions{
		NewRunID:              func() (string, error) { return "run-blocked-observer", nil },
		ObserverQueueMessages: 8,
	})
	unregister := coordinator.RegisterRunEventObserver(observer)

	startDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Start("session-blocked-observer", SessionMessageInput{Content: "start"}, nil)
		startDone <- err
	}()
	select {
	case <-startDone:
	case <-time.After(time.Second):
		t.Fatal("Start blocked on a permanently blocking observer")
	}
	select {
	case <-observer.entered:
	case <-time.After(time.Second):
		t.Fatal("observer did not enter its blocking callback")
	}
	close(observer.block)
	unregister()
	coordinator.Close()
}

func TestSessionRunCoordinatorObserverMailboxReportsLossAndPreservesOrder(t *testing.T) {
	observer := &mailboxTestObserver{entered: make(chan struct{}), block: make(chan struct{}), losses: make(chan string, 1)}
	coordinator := NewSessionRunCoordinator(context.Background(), &synchronousCoordinatorTestStarter{}, SessionRunCoordinatorOptions{
		ObserverQueueMessages: 2,
		ObserverQueueBytes:    4096,
		NewRunID:              func() (string, error) { return "run-mailbox-overflow", nil },
	})
	unregister := coordinator.RegisterRunEventObserver(observer)
	run := &CoordinatedSessionRun{id: "run-mailbox-overflow", sessionID: "session-mailbox-overflow"}
	coordinator.notifyRunAdmittedObservers(run)
	select {
	case <-observer.entered:
	case <-time.After(time.Second):
		t.Fatal("observer did not enter admitted callback")
	}
	for index := 0; index < 8; index++ {
		coordinator.notifyRunEventObservers(run, NewSessionStreamEvent("text.delta", map[string]any{
			"turn_id": "turn", "text": fmt.Sprintf("%d", index),
		}))
	}
	close(observer.block)
	select {
	case reason := <-observer.losses:
		if reason == "" {
			t.Fatal("observer loss reason is empty")
		}
	case <-time.After(time.Second):
		t.Fatal("observer mailbox overflow produced no explicit loss signal")
	}
	observer.mu.Lock()
	order := append([]string(nil), observer.order...)
	observer.mu.Unlock()
	if len(order) == 0 || order[0] != "admitted:run-mailbox-overflow" {
		t.Fatalf("observer order = %#v, want admitted before loss", order)
	}
	unregister()
	coordinator.Close()
}

func TestSessionRunCoordinatorObserverMailboxLossCoversDiscardedRuns(t *testing.T) {
	observer := &mailboxTestObserver{
		entered:  make(chan struct{}),
		block:    make(chan struct{}),
		lossRuns: make(chan string, 4),
	}
	coordinator := NewSessionRunCoordinator(context.Background(), &synchronousCoordinatorTestStarter{}, SessionRunCoordinatorOptions{
		ObserverQueueMessages: 2,
		ObserverQueueBytes:    4096,
	})
	unregister := coordinator.RegisterRunEventObserver(observer)
	runA := &CoordinatedSessionRun{id: "run-loss-a", sessionID: "session-loss-a"}
	runB := &CoordinatedSessionRun{id: "run-loss-b", sessionID: "session-loss-b"}
	coordinator.notifyRunAdmittedObservers(runA)
	select {
	case <-observer.entered:
	case <-time.After(time.Second):
		t.Fatal("observer did not enter admitted callback")
	}
	coordinator.notifyRunEventObservers(runA, NewSessionStreamEvent("text.delta", map[string]any{"text": "a"}))
	coordinator.notifyRunEventObservers(runB, NewSessionStreamEvent("text.delta", map[string]any{"text": "b"}))
	coordinator.notifyRunEventObservers(runB, NewSessionStreamEvent("text.delta", map[string]any{"text": "overflow"}))
	close(observer.block)

	seen := make(map[string]bool)
	for len(seen) < 2 {
		select {
		case runID := <-observer.lossRuns:
			seen[runID] = true
		case <-time.After(time.Second):
			t.Fatalf("loss runs = %#v, want both discarded runs", seen)
		}
	}
	if !seen[runA.ID()] || !seen[runB.ID()] {
		t.Fatalf("loss runs = %#v, want %q and %q", seen, runA.ID(), runB.ID())
	}
	// A loss poisons only the affected runs. Once their terminal callbacks have
	// crossed the mailbox, the same registration must continue serving a later
	// run instead of leaving a stopped worker behind in coordinator.observers.
	coordinator.notifyRunEventObservers(runA, NewSessionStreamEvent("text.delta", map[string]any{"text": "poisoned"}))
	time.Sleep(20 * time.Millisecond)
	observer.mu.Lock()
	if containsObserverOrder(observer.order, "event:"+runA.ID()) {
		observer.mu.Unlock()
		t.Fatal("poisoned run event was delivered after overflow")
	}
	observer.mu.Unlock()
	coordinator.notifyRunSettledObservers(runA, SessionMessageResult{}, nil)
	coordinator.notifyRunSettledObservers(runB, SessionMessageResult{}, nil)
	waitForObserverOrder(t, observer, "settled:"+runA.ID())
	waitForObserverOrder(t, observer, "settled:"+runB.ID())
	runC := &CoordinatedSessionRun{id: "run-loss-future", sessionID: "session-loss-future"}
	coordinator.notifyRunAdmittedObservers(runC)
	waitForObserverOrder(t, observer, "admitted:"+runC.ID())
	coordinator.notifyRunEventObservers(runC, NewSessionStreamEvent("text.delta", map[string]any{"text": "future"}))
	waitForObserverOrder(t, observer, "event:"+runC.ID())
	coordinator.notifyRunSettledObservers(runC, SessionMessageResult{}, nil)
	waitForObserverOrder(t, observer, "settled:"+runC.ID())
	mailbox := coordinator.runObservers()[0]
	select {
	case <-mailbox.done:
		t.Fatal("observer mailbox became a zombie after loss")
	default:
	}
	unregister()
	coordinator.Close()
}

func TestSessionRunCoordinatorObserverMailboxTerminalTriggerIsRetainedAfterLoss(t *testing.T) {
	observer := &mailboxTestObserver{
		entered:   make(chan struct{}),
		block:     make(chan struct{}),
		losses:    make(chan string, 1),
		lossBlock: make(chan struct{}),
	}
	coordinator := NewSessionRunCoordinator(context.Background(), &synchronousCoordinatorTestStarter{}, SessionRunCoordinatorOptions{
		ObserverQueueMessages: 1,
		ObserverQueueBytes:    4096,
	})
	unregister := coordinator.RegisterRunEventObserver(observer)
	run := &CoordinatedSessionRun{id: "run-terminal-trigger", sessionID: "session-terminal-trigger"}
	coordinator.notifyRunAdmittedObservers(run)
	select {
	case <-observer.entered:
	case <-time.After(time.Second):
		t.Fatal("observer did not enter admitted callback")
	}
	// The event fills the mailbox. The settled delivery is the operation which
	// detects overflow, so it must not disappear with the discarded suffix.
	coordinator.notifyRunEventObservers(run, NewSessionStreamEvent("text.delta", map[string]any{"text": "queued"}))
	coordinator.notifyRunSettledObservers(run, SessionMessageResult{Status: "committed"}, errors.New("terminal error that must not be retained"))
	close(observer.block)
	select {
	case <-observer.losses:
	case <-time.After(time.Second):
		t.Fatal("terminal-triggered overflow produced no loss marker")
	}
	mailbox := coordinator.runObservers()[0]
	mailbox.mu.Lock()
	active, poisoned, pending := len(mailbox.activeRuns), len(mailbox.poisonedRuns), len(mailbox.pendingTerminals)
	mailbox.mu.Unlock()
	if active != 1 || poisoned != 1 || pending != 1 {
		t.Fatalf("state during loss callback = active %d, poisoned %d, pending terminals %d; want 1/1/1", active, poisoned, pending)
	}
	// lossBlock keeps the recovery notification ahead of terminal cleanup. The
	// terminal callback can now run and remove both source-owned fences.
	close(observer.lossBlock)
	waitForObserverOrder(t, observer, "settled:"+run.ID())
	waitForMailboxRunState(t, mailbox, 0, 0, 0)
	unregister()
	coordinator.Close()
}

func TestSessionRunCoordinatorObserverMailboxQueuedTerminalSurvivesLaterOverflow(t *testing.T) {
	observer := &mailboxTestObserver{
		entered:   make(chan struct{}),
		block:     make(chan struct{}),
		losses:    make(chan string, 1),
		lossBlock: make(chan struct{}),
	}
	coordinator := NewSessionRunCoordinator(context.Background(), &synchronousCoordinatorTestStarter{}, SessionRunCoordinatorOptions{
		ObserverQueueMessages: 2,
		ObserverQueueBytes:    4096,
	})
	unregister := coordinator.RegisterRunEventObserver(observer)
	run := &CoordinatedSessionRun{id: "run-queued-terminal", sessionID: "session-queued-terminal"}
	coordinator.notifyRunAdmittedObservers(run)
	select {
	case <-observer.entered:
	case <-time.After(time.Second):
		t.Fatal("observer did not enter admitted callback")
	}
	// The terminal is already queued. A later event triggers overflow while the
	// worker is still blocked in admission; overflow must retain that queued
	// terminal before replacing the queue with loss.
	coordinator.notifyRunSettledObservers(run, SessionMessageResult{Status: "committed"}, nil)
	coordinator.notifyRunEventObservers(run, NewSessionStreamEvent("text.delta", map[string]any{"text": "one"}))
	coordinator.notifyRunEventObservers(run, NewSessionStreamEvent("text.delta", map[string]any{"text": "overflow"}))
	close(observer.block)
	select {
	case <-observer.losses:
	case <-time.After(time.Second):
		t.Fatal("queued-terminal overflow produced no loss marker")
	}
	mailbox := coordinator.runObservers()[0]
	mailbox.mu.Lock()
	pending := len(mailbox.pendingTerminals)
	mailbox.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending terminals during loss = %d, want 1", pending)
	}
	close(observer.lossBlock)
	waitForObserverOrder(t, observer, "settled:"+run.ID())
	waitForMailboxRunState(t, mailbox, 0, 0, 0)
	unregister()
	coordinator.Close()
}

func TestSessionRunCoordinatorObserverMailboxOverflowTerminalCleanupStaysBounded(t *testing.T) {
	const rounds = 16
	observer := &mailboxTestObserver{
		blockGates:   make(chan chan struct{}, rounds),
		enteredGates: make(chan struct{}, rounds),
	}
	coordinator := NewSessionRunCoordinator(context.Background(), &synchronousCoordinatorTestStarter{}, SessionRunCoordinatorOptions{
		ObserverQueueMessages: 1,
		ObserverQueueBytes:    4096,
	})
	unregister := coordinator.RegisterRunEventObserver(observer)
	mailbox := coordinator.runObservers()[0]
	for index := 0; index < rounds; index++ {
		run := &CoordinatedSessionRun{id: fmt.Sprintf("run-repeated-%d", index), sessionID: fmt.Sprintf("session-repeated-%d", index)}
		gate := make(chan struct{})
		observer.blockGates <- gate
		coordinator.notifyRunAdmittedObservers(run)
		select {
		case <-observer.enteredGates:
		case <-time.After(time.Second):
			t.Fatalf("observer did not block run %s", run.ID())
		}
		coordinator.notifyRunEventObservers(run, NewSessionStreamEvent("text.delta", map[string]any{"text": "queued"}))
		// With one queue slot, this terminal is itself the overflow trigger.
		coordinator.notifyRunSettledObservers(run, SessionMessageResult{}, nil)
		close(gate)
		waitForObserverOrder(t, observer, "settled:"+run.ID())
		waitForMailboxRunState(t, mailbox, 0, 0, 0)
	}
	// The same mailbox must remain a live registration after every recovery.
	for _, id := range []string{"run-future-b", "run-future-c"} {
		run := &CoordinatedSessionRun{id: id, sessionID: id}
		coordinator.notifyRunAdmittedObservers(run)
		waitForObserverOrder(t, observer, "admitted:"+id)
		coordinator.notifyRunEventObservers(run, NewSessionStreamEvent("text.delta", map[string]any{"text": id}))
		waitForObserverOrder(t, observer, "event:"+id)
		coordinator.notifyRunSettledObservers(run, SessionMessageResult{}, nil)
		waitForObserverOrder(t, observer, "settled:"+id)
		waitForMailboxRunState(t, mailbox, 0, 0, 0)
	}
	unregister()
	coordinator.Close()
}

func waitForMailboxRunState(t *testing.T, mailbox *runObserverMailbox, active, poisoned, pending int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mailbox.mu.Lock()
		gotActive, gotPoisoned, gotPending := len(mailbox.activeRuns), len(mailbox.poisonedRuns), len(mailbox.pendingTerminals)
		mailbox.mu.Unlock()
		if gotActive == active && gotPoisoned == poisoned && gotPending == pending {
			return
		}
		time.Sleep(time.Millisecond)
	}
	mailbox.mu.Lock()
	gotActive, gotPoisoned, gotPending := len(mailbox.activeRuns), len(mailbox.poisonedRuns), len(mailbox.pendingTerminals)
	mailbox.mu.Unlock()
	t.Fatalf("mailbox run state = active %d, poisoned %d, pending %d; want %d/%d/%d", gotActive, gotPoisoned, gotPending, active, poisoned, pending)
}

func containsObserverOrder(order []string, want string) bool {
	for _, value := range order {
		if value == want {
			return true
		}
	}
	return false
}

func waitForObserverOrder(t *testing.T, observer *mailboxTestObserver, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		observer.mu.Lock()
		order := append([]string(nil), observer.order...)
		observer.mu.Unlock()
		if containsObserverOrder(order, want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	observer.mu.Lock()
	order := append([]string(nil), observer.order...)
	observer.mu.Unlock()
	t.Fatalf("observer order = %#v, want %q", order, want)
}

func TestSessionRunCoordinatorObserverMailboxPreservesNormalRunOrder(t *testing.T) {
	observer := &mailboxTestObserver{}
	coordinator := NewSessionRunCoordinator(context.Background(), &synchronousCoordinatorTestStarter{}, SessionRunCoordinatorOptions{
		NewRunID: func() (string, error) { return "run-mailbox-order", nil },
	})
	unregister := coordinator.RegisterRunEventObserver(observer)
	run, err := coordinator.Start("session-mailbox-order", SessionMessageInput{Content: "order"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Wait(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		observer.mu.Lock()
		complete := len(observer.order) >= 3
		observer.mu.Unlock()
		if complete {
			break
		}
		time.Sleep(time.Millisecond)
	}
	observer.mu.Lock()
	order := append([]string(nil), observer.order...)
	observer.mu.Unlock()
	want := []string{"admitted:run-mailbox-order", "event:run-mailbox-order", "settled:run-mailbox-order"}
	if len(order) < len(want) {
		t.Fatalf("observer order = %#v, want %#v", order, want)
	}
	for index, expected := range want {
		if order[index] != expected {
			t.Fatalf("observer order = %#v, want prefix %#v", order, want)
		}
	}
	unregister()
	coordinator.Close()
}

func TestSessionRunCoordinatorObserverMailboxDeepCopiesEventBeforeReturn(t *testing.T) {
	observer := &mailboxTestObserver{events: make(chan SessionStreamEvent, 1)}
	coordinator := NewSessionRunCoordinator(context.Background(), &synchronousCoordinatorTestStarter{}, SessionRunCoordinatorOptions{
		NewRunID: func() (string, error) { return "run-mailbox-copy", nil },
	})
	unregister := coordinator.RegisterRunEventObserver(observer)
	run := &CoordinatedSessionRun{id: "run-mailbox-copy", sessionID: "session-mailbox-copy"}
	nested := map[string]any{"value": "original"}
	event := NewSessionStreamEvent("tool.progress", map[string]any{
		"turn_id": "turn", "nested": nested, "items": []any{map[string]any{"value": "original-item"}},
	})
	coordinator.notifyRunEventObservers(run, event)
	nested["value"] = "mutated"
	event["turn_id"] = "mutated-turn"
	event["items"].([]any)[0].(map[string]any)["value"] = "mutated-item"
	select {
	case got := <-observer.events:
		if got["turn_id"] != "turn" {
			t.Fatalf("copied event turn_id = %#v, want turn", got["turn_id"])
		}
		copiedNested, ok := got["nested"].(map[string]any)
		if !ok || copiedNested["value"] != "original" {
			t.Fatalf("copied nested value = %#v, want original", got["nested"])
		}
		copiedItems, ok := got["items"].([]any)
		if !ok || copiedItems[0].(map[string]any)["value"] != "original-item" {
			t.Fatalf("copied nested slice = %#v", got["items"])
		}
	case <-time.After(time.Second):
		t.Fatal("observer did not receive copied event")
	}
	unregister()
	coordinator.Close()
}

func TestSessionRunCoordinatorObserverUnregisterConcurrentWithNotifications(t *testing.T) {
	observer := &mailboxTestObserver{}
	coordinator := NewSessionRunCoordinator(context.Background(), &synchronousCoordinatorTestStarter{}, SessionRunCoordinatorOptions{
		ObserverQueueMessages: 32,
		NewRunID:              func() (string, error) { return "run-mailbox-race", nil },
	})
	unregister := coordinator.RegisterRunEventObserver(observer)
	run := &CoordinatedSessionRun{id: "run-mailbox-race", sessionID: "session-mailbox-race"}
	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for event := 0; event < 32; event++ {
				coordinator.notifyRunEventObservers(run, NewSessionStreamEvent("text.delta", map[string]any{"n": event}))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		unregister()
	}()
	wg.Wait()
	coordinator.Close()
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
