package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// blockingExecutionRunner blocks its turn until either release is closed or the
// turn context is cancelled, mirroring a long-running provider turn.
func blockingExecutionRunner(release <-chan struct{}) fakeExecutionTurnRunner {
	return fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			select {
			case <-release:
				if err := request.Publisher.Publish(eventAssistant(request.TurnID, "done")); err != nil {
					return SessionTurnResult{}, err
				}
				return SessionTurnResult{Incremental: true}, nil
			case <-ctx.Done():
				return SessionTurnResult{}, ctx.Err()
			}
		},
	}
}

func TestSessionRunStartIsNonblocking(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	service, _, session := newExecutionServiceWithSession(t, home, blockingExecutionRunner(release))

	start := time.Now()
	run := service.StartSessionRun(context.Background(), session.ID, "hello", nil)
	elapsed := time.Since(start)

	if elapsed >= time.Second {
		t.Fatalf("StartSessionRun blocked for %v, want to return immediately", elapsed)
	}
	if got := run.Status(); got != SessionRunRunning {
		t.Fatalf("Status() = %q, want running before release", got)
	}

	close(release)
	result, err := run.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "committed" || result.TurnID == "" || result.LastSeq == 0 {
		t.Fatalf("Wait() = %#v, want committed result", result)
	}
	if got := run.Status(); got != SessionRunCommitted {
		t.Fatalf("Status() = %q, want committed after release", got)
	}
}

func TestSessionRunCancelPropagatesAndSettlesCancelled(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	defer close(release)
	service, _, session := newExecutionServiceWithSession(t, home, blockingExecutionRunner(release))

	run := service.StartSessionRun(context.Background(), session.ID, "hello", nil)
	if got := run.Status(); got != SessionRunRunning {
		t.Fatalf("Status() = %q, want running", got)
	}

	run.Cancel()

	result, err := run.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if result.Status != "" || result.TurnID != "" {
		t.Fatalf("Wait() = %#v, want zero result on cancel", result)
	}
	if got := run.Status(); got != SessionRunCancelled {
		t.Fatalf("Status() = %q, want cancelled", got)
	}
}

func TestSessionRunStatusesForCommittedFailedCancelled(t *testing.T) {
	t.Run("committed", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				if err := request.Publisher.Publish(eventAssistant(request.TurnID, "ok")); err != nil {
					return SessionTurnResult{}, err
				}
				return SessionTurnResult{Incremental: true}, nil
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)

		run := service.StartSessionRun(context.Background(), session.ID, "hello", nil)
		_, err := run.Wait()
		if err != nil {
			t.Fatalf("Wait() error = %v, want nil", err)
		}
		if got := run.Status(); got != SessionRunCommitted {
			t.Fatalf("Status() = %q, want committed", got)
		}
	})

	t.Run("failed", func(t *testing.T) {
		home := t.TempDir()
		runner := fakeExecutionTurnRunner{
			supports: true,
			run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
				return SessionTurnResult{}, errors.New("provider exploded")
			},
		}
		service, _, session := newExecutionServiceWithSession(t, home, runner)

		run := service.StartSessionRun(context.Background(), session.ID, "hello", nil)
		_, err := run.Wait()
		if !errors.Is(err, ErrTurnFailed) {
			t.Fatalf("Wait() error = %v, want ErrTurnFailed", err)
		}
		if got := run.Status(); got != SessionRunFailed {
			t.Fatalf("Status() = %q, want failed", got)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		home := t.TempDir()
		release := make(chan struct{})
		defer close(release)
		service, _, session := newExecutionServiceWithSession(t, home, blockingExecutionRunner(release))

		run := service.StartSessionRun(context.Background(), session.ID, "hello", nil)
		run.Cancel()
		_, err := run.Wait()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait() error = %v, want context.Canceled", err)
		}
		if got := run.Status(); got != SessionRunCancelled {
			t.Fatalf("Status() = %q, want cancelled", got)
		}
	})
}

func TestSessionRunWaitIsRepeatable(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "ok")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	run := service.StartSessionRun(context.Background(), session.ID, "hello", nil)

	first, err := run.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	for i := 0; i < 3; i++ {
		result, err := run.Wait()
		if err != nil {
			t.Fatalf("Wait() repeat %d error = %v", i, err)
		}
		if result != first {
			t.Fatalf("Wait() repeat %d = %#v, want %#v", i, result, first)
		}
	}
}

func TestSessionRunCancelAfterSettleChangesNothing(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "ok")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	run := service.StartSessionRun(context.Background(), session.ID, "hello", nil)
	if _, err := run.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if got := run.Status(); got != SessionRunCommitted {
		t.Fatalf("Status() = %q, want committed before cancel", got)
	}

	run.Cancel()
	run.Cancel()

	if got := run.Status(); got != SessionRunCommitted {
		t.Fatalf("Status() = %q, want committed after cancel", got)
	}
	result, err := run.Wait()
	if err != nil || result.Status != "committed" {
		t.Fatalf("Wait() after cancel = %#v/%v, want committed result", result, err)
	}
}

func TestSessionRunSyncSendPreservesEventsAndResult(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			request.Emit(model.AssistantMessageStartedEvent{ItemID: "assistant-sync", AgentIteration: 1})
			request.Emit(model.AssistantMessageUpdatedEvent{ItemID: "assistant-sync", AgentIteration: 1, Revision: 1, Message: model.Message{Role: model.MessageRoleAssistant, Content: "streamed"}})
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)
	var events []SessionStreamEvent

	result, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", func(event SessionStreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("SendSessionMessageWithEvents() error = %v", err)
	}
	if result.Status != "committed" || result.TurnID != "turn-000001" || result.LastSeq == 0 {
		t.Fatalf("SendSessionMessageWithEvents() = %#v, want committed result", result)
	}
	types := sessionStreamEventTypes(events)
	if len(types) < 4 || types[0] != "turn.started" || types[len(types)-1] != "turn.committed" {
		t.Fatalf("event types = %#v, want turn.started first and turn.committed last", types)
	}
	if !stringSliceContains(types, "assistant.message.updated") {
		t.Fatalf("event types = %#v, want contain assistant.message.updated", types)
	}
	if !sessionStreamEventsContain(events, "assistant.message.updated", "content", "streamed") {
		t.Fatalf("events = %#v, want streamed message snapshot", events)
	}
}

func TestSessionRunSyncSendBusyForHeldWriteLock(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			t.Fatal("RunSessionTurn should not be called while write lock is held")
			return SessionTurnResult{}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)
	service.sessionWriteLockTimeout = 20 * time.Millisecond
	lock, err := service.sessionStore.AcquireSessionWriteLock(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("AcquireSessionWriteLock() error = %v", err)
	}
	defer lock.Release()

	start := time.Now()
	run := service.StartSessionRun(context.Background(), session.ID, "busy", nil)
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("StartSessionRun blocked for %v, want to return immediately", elapsed)
	}
	_, err = run.Wait()
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("Wait() error = %v, want ErrSessionBusy", err)
	}
	if got := run.Status(); got != SessionRunFailed {
		t.Fatalf("Status() = %q, want failed for busy lock", got)
	}
}

func TestSessionRunParentContextCancellationSettlesCancelled(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	defer close(release)
	service, _, session := newExecutionServiceWithSession(t, home, blockingExecutionRunner(release))

	ctx, cancel := context.WithCancel(context.Background())
	run := service.StartSessionRun(ctx, session.ID, "hello", nil)
	if got := run.Status(); got != SessionRunRunning {
		t.Fatalf("Status() = %q, want running", got)
	}

	cancel()

	_, err := run.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if got := run.Status(); got != SessionRunCancelled {
		t.Fatalf("Status() = %q, want cancelled", got)
	}
}
