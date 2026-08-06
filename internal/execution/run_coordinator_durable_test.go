package execution

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

type gatedDurableAdmitter struct {
	entered      chan struct{}
	lookupsReady chan struct{}
	release      chan struct{}
	calls        atomic.Int32
	lookups      atomic.Int32
}

func (a *gatedDurableAdmitter) LookupSessionRun(context.Context, string, SessionMessageInput, string, string) (DurableRunAdmission, bool, error) {
	if a.lookups.Add(1) == 3 && a.lookupsReady != nil {
		close(a.lookupsReady)
	}
	return DurableRunAdmission{}, false, nil
}

func (a *gatedDurableAdmitter) AdmitSessionRun(context.Context, string, SessionMessageInput, string, string) (DurableRunAdmission, error) {
	a.calls.Add(1)
	select {
	case <-a.entered:
	default:
		close(a.entered)
	}
	<-a.release
	return DurableRunAdmission{Created: true, Status: SessionRunRunning}, nil
}

func (*gatedDurableAdmitter) FailAdmittedSessionRun(context.Context, string, string) error {
	return nil
}

type gatedDurableStarter struct {
	starts atomic.Int32
}

func (s *gatedDurableStarter) StartSessionRunWithInput(ctx context.Context, sessionID string, input SessionMessageInput, emit func(SessionStreamEvent)) *SessionRun {
	return s.StartSessionRunWithIDAdmitted(ctx, sessionID, input, "legacy", emit)
}

func (s *gatedDurableStarter) StartSessionRunWithID(ctx context.Context, sessionID string, input SessionMessageInput, runID string, emit func(SessionStreamEvent)) *SessionRun {
	return s.StartSessionRunWithIDAdmitted(ctx, sessionID, input, runID, emit)
}

func (s *gatedDurableStarter) StartSessionRunWithIDAdmitted(ctx context.Context, _ string, _ SessionMessageInput, _ string, _ func(SessionStreamEvent)) *SessionRun {
	s.starts.Add(1)
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

func TestSessionRunCoordinatorDurableAdmissionHasOneOwnerAndJoiners(t *testing.T) {
	admitter := &gatedDurableAdmitter{entered: make(chan struct{}), lookupsReady: make(chan struct{}), release: make(chan struct{})}
	starter := &gatedDurableStarter{}
	coordinator := NewSessionRunCoordinator(context.Background(), starter, SessionRunCoordinatorOptions{DurableAdmitter: admitter})
	defer coordinator.Close()

	firstResult := make(chan struct {
		run       *CoordinatedSessionRun
		admission DurableRunAdmission
		err       error
	}, 1)
	go func() {
		run, admission, err := coordinator.StartDurable("session-owner", SessionMessageInput{Content: "same"}, "run-owner", "fp-same", nil)
		firstResult <- struct {
			run       *CoordinatedSessionRun
			admission DurableRunAdmission
			err       error
		}{run, admission, err}
	}()
	select {
	case <-admitter.entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not enter durable admission gate")
	}

	joined := make(chan struct {
		run       *CoordinatedSessionRun
		admission DurableRunAdmission
		err       error
	}, 1)
	go func() {
		run, admission, err := coordinator.StartDurable("session-owner", SessionMessageInput{Content: "same"}, "run-owner", "fp-same", nil)
		joined <- struct {
			run       *CoordinatedSessionRun
			admission DurableRunAdmission
			err       error
		}{run, admission, err}
	}()
	crossSession := make(chan error, 1)
	go func() {
		_, _, err := coordinator.StartDurable("session-loser", SessionMessageInput{Content: "same"}, "run-owner", "fp-same", nil)
		crossSession <- err
	}()
	select {
	case <-admitter.lookupsReady:
	case <-time.After(time.Second):
		t.Fatal("same-identity and cross-session callers did not reach lookup before owner admission was released")
	}
	if got := admitter.calls.Load(); got != 1 {
		t.Fatalf("admission calls while owner is gated=%d, want one", got)
	}
	close(admitter.release)

	var first struct {
		run       *CoordinatedSessionRun
		admission DurableRunAdmission
		err       error
	}
	select {
	case first = <-firstResult:
	case <-time.After(time.Second):
		t.Fatal("owner did not finish admission")
	}
	if first.err != nil || first.run == nil || !first.admission.Created {
		t.Fatalf("owner result=%#v", first)
	}
	var second struct {
		run       *CoordinatedSessionRun
		admission DurableRunAdmission
		err       error
	}
	select {
	case second = <-joined:
	case <-time.After(time.Second):
		t.Fatal("joiner did not finish")
	}
	if second.err != nil || second.run != first.run || second.admission.Created || second.admission.Status != SessionRunRunning {
		t.Fatalf("joiner result=%#v, owner=%#v", second, first)
	}
	select {
	case err := <-crossSession:
		if !errors.Is(err, sessions.ErrIdempotencyConflict) {
			t.Fatalf("cross-session loser error=%v, want idempotency conflict", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cross-session loser did not finish")
	}
	if got := admitter.calls.Load(); got != 1 {
		t.Fatalf("admission calls=%d, want exactly one owner", got)
	}
	if got := starter.starts.Load(); got != 1 {
		t.Fatalf("starter calls=%d, want exactly one model owner", got)
	}
	first.run.Cancel()
	if _, err := first.run.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("owner wait error=%v", err)
	}
}

type legacyStableOnlyStarter struct{}

func (legacyStableOnlyStarter) StartSessionRunWithInput(context.Context, string, SessionMessageInput, func(SessionStreamEvent)) *SessionRun {
	return nil
}

func (legacyStableOnlyStarter) StartSessionRunWithID(context.Context, string, SessionMessageInput, string, func(SessionStreamEvent)) *SessionRun {
	return nil
}

func TestSessionRunCoordinatorRequiresAdmittedStarterBeforeDurableLookup(t *testing.T) {
	admitter := &gatedDurableAdmitter{entered: make(chan struct{}), release: make(chan struct{})}
	coordinator := NewSessionRunCoordinator(context.Background(), legacyStableOnlyStarter{}, SessionRunCoordinatorOptions{DurableAdmitter: admitter})
	defer coordinator.Close()
	_, _, err := coordinator.StartDurable("session-no-admitted-starter", SessionMessageInput{Content: "no"}, "run-no-admitted-starter", "fp", nil)
	if err == nil {
		t.Fatal("StartDurable accepted a legacy stable-ID starter")
	}
	if got := admitter.calls.Load(); got != 0 {
		t.Fatalf("durable admission calls=%d, want zero before starter capability check", got)
	}
}
