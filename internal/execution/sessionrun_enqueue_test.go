package execution

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recordingBlockingRunner blocks each turn on release (until closed) and
// records the active-history contents the provider saw at the start of each
// turn. Once release is closed, every turn proceeds immediately.
func recordingBlockingRunner(release <-chan struct{}, seen *[][]string) fakeExecutionTurnRunner {
	var mu sync.Mutex
	counter := 0
	return fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return SessionTurnResult{}, ctx.Err()
			}
			mu.Lock()
			counter++
			turnIndex := counter
			mu.Unlock()
			if msg, err := request.Session.MaterializeActiveHistory(); err == nil {
				mu.Lock()
				*seen = append(*seen, messageContents(msg))
				mu.Unlock()
			}
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer-"+request.Content)); err != nil {
				return SessionTurnResult{}, err
			}
			_ = turnIndex
			return SessionTurnResult{Incremental: true}, nil
		},
	}
}

func TestSessionRunEnqueueProcessesFIFOAndProviderSeesPriorDurableTurns(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	var seen [][]string
	service, _, session := newExecutionServiceWithSession(t, home, recordingBlockingRunner(release, &seen))

	run := service.StartSessionRun(context.Background(), session.ID, "init", nil)

	receipt1, err := run.Enqueue(PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: "q1"})
	if err != nil {
		t.Fatalf("Enqueue(q1) error = %v", err)
	}
	receipt2, err := run.Enqueue(PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: "q2"})
	if err != nil {
		t.Fatalf("Enqueue(q2) error = %v", err)
	}

	close(release)

	result, err := run.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "committed" || result.TurnID == "" || result.LastSeq == 0 {
		t.Fatalf("Wait() = %#v, want committed last turn", result)
	}
	if got := run.Status(); got != SessionRunCommitted {
		t.Fatalf("Status() = %q, want committed", got)
	}

	// FIFO: three turns ran in order, each seeing the prior turns' durable state.
	if len(seen) != 3 {
		t.Fatalf("turns run = %d, want 3", len(seen))
	}
	if got := seen[0]; !sameStringSlice(got, []string{}) {
		t.Fatalf("turn 1 active history = %#v, want empty", got)
	}
	if got := seen[1]; !sameStringSlice(got, []string{"init", "answer-init"}) {
		t.Fatalf("turn 2 active history = %#v, want init + answer-init", got)
	}
	if got := seen[2]; !sameStringSlice(got, []string{"init", "answer-init", "q1", "answer-q1"}) {
		t.Fatalf("turn 3 active history = %#v, want prior durable turns before q2", got)
	}

	r1, err := receipt1.Wait()
	if err != nil || r1.Status != "committed" || r1.TurnID == "" || r1.LastSeq == 0 {
		t.Fatalf("receipt1.Wait() = %#v/%v, want committed", r1, err)
	}
	r2, err := receipt2.Wait()
	if err != nil || r2.Status != "committed" || r2.TurnID == "" || r2.LastSeq == 0 {
		t.Fatalf("receipt2.Wait() = %#v/%v, want committed", r2, err)
	}
	// The run's final result is the last enqueued turn's result.
	if result.TurnID != r2.TurnID || result.LastSeq != r2.LastSeq {
		t.Fatalf("Wait() = %#v, want last receipt result %#v", result, r2)
	}
	if r1.LastSeq >= r2.LastSeq {
		t.Fatalf("receipt seqs not monotonic: r1=%d r2=%d", r1.LastSeq, r2.LastSeq)
	}
}

func TestSessionRunEnqueueReceiptWaitIsRepeatable(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	var seen [][]string
	service, _, session := newExecutionServiceWithSession(t, home, recordingBlockingRunner(release, &seen))

	run := service.StartSessionRun(context.Background(), session.ID, "init", nil)
	receipt, err := run.Enqueue(PromptEvent{Source: PromptSourceTUI, Mode: PromptModeEnqueueTurn, Content: "again", InputID: "in-1"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	close(release)

	_, _ = run.Wait()
	first, err := receipt.Wait()
	if err != nil {
		t.Fatalf("receipt.Wait() error = %v", err)
	}
	for i := 0; i < 3; i++ {
		got, err := receipt.Wait()
		if err != nil || got != first {
			t.Fatalf("receipt.Wait() repeat %d = %#v/%v, want %#v", i, got, err, first)
		}
	}
}

func TestSessionRunEnqueueRejectsInvalidModeAndSettled(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	var seen [][]string
	service, _, session := newExecutionServiceWithSession(t, home, recordingBlockingRunner(release, &seen))

	run := service.StartSessionRun(context.Background(), session.ID, "init", nil)

	// Invalid event (whitespace-only content) rejected with stable error.
	if _, err := run.Enqueue(PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: "  "}); !errors.Is(err, ErrPromptEventInvalid) {
		t.Fatalf("Enqueue(invalid) error = %v, want ErrPromptEventInvalid", err)
	}
	// append_active mode rejected with stable error, even while running.
	if _, err := run.Enqueue(PromptEvent{Source: PromptSourceStdin, Mode: PromptModeAppendActive, Content: "x"}); !errors.Is(err, ErrPromptModeNotSupported) {
		t.Fatalf("Enqueue(append_active) error = %v, want ErrPromptModeNotSupported", err)
	}

	close(release)
	if _, err := run.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	// After settle, even a valid enqueue_turn event is rejected.
	if _, err := run.Enqueue(PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: "late"}); !errors.Is(err, ErrSessionRunSettled) {
		t.Fatalf("Enqueue(settled) error = %v, want ErrSessionRunSettled", err)
	}
	if got := run.Status(); got != SessionRunCommitted {
		t.Fatalf("Status() = %q, want committed", got)
	}
}

func TestSessionRunEnqueueFailureDrainsReceiptsWithSameError(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	var mu sync.Mutex
	counter := 0
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return SessionTurnResult{}, ctx.Err()
			}
			mu.Lock()
			counter++
			n := counter
			mu.Unlock()
			if n == 2 {
				return SessionTurnResult{}, errors.New("provider exploded")
			}
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer-"+request.Content)); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	run := service.StartSessionRun(context.Background(), session.ID, "init", nil)
	receipt1, err := run.Enqueue(PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: "q1"})
	if err != nil {
		t.Fatalf("Enqueue(q1) error = %v", err)
	}
	receipt2, err := run.Enqueue(PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: "q2"})
	if err != nil {
		t.Fatalf("Enqueue(q2) error = %v", err)
	}
	close(release)

	_, err = run.Wait()
	if !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("Wait() error = %v, want ErrTurnFailed", err)
	}
	if got := run.Status(); got != SessionRunFailed {
		t.Fatalf("Status() = %q, want failed", got)
	}

	// The failing receipt (turn 2) and the unstarted receipt (turn 3) both get
	// the same effective error; neither is dropped.
	if _, err := receipt1.Wait(); !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("receipt1.Wait() error = %v, want ErrTurnFailed", err)
	}
	if _, err := receipt2.Wait(); !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("receipt2.Wait() error = %v, want ErrTurnFailed", err)
	}

	// After failure, no further enqueues are accepted.
	if _, err := run.Enqueue(PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: "late"}); !errors.Is(err, ErrSessionRunSettled) {
		t.Fatalf("Enqueue(after failure) error = %v, want ErrSessionRunSettled", err)
	}
}

func TestSessionRunEnqueueCancelDrainsReceiptsWithCanceled(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	service, _, session := newExecutionServiceWithSession(t, home, blockingExecutionRunner(release))

	run := service.StartSessionRun(context.Background(), session.ID, "init", nil)
	receipt1, err := run.Enqueue(PromptEvent{Source: PromptSourceMailbox, Mode: PromptModeEnqueueTurn, Content: "q1", MailboxTaskID: "task-1"})
	if err != nil {
		t.Fatalf("Enqueue(q1) error = %v", err)
	}
	receipt2, err := run.Enqueue(PromptEvent{Source: PromptSourceMailbox, Mode: PromptModeEnqueueTurn, Content: "q2", MailboxTaskID: "task-2"})
	if err != nil {
		t.Fatalf("Enqueue(q2) error = %v", err)
	}

	// Cancel while the initial turn is still blocked: the initial turn and every
	// already-accepted but unstarted receipt settle with context.Canceled.
	run.Cancel()

	_, err = run.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if got := run.Status(); got != SessionRunCancelled {
		t.Fatalf("Status() = %q, want cancelled", got)
	}
	if _, err := receipt1.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("receipt1.Wait() error = %v, want context.Canceled", err)
	}
	if _, err := receipt2.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("receipt2.Wait() error = %v, want context.Canceled", err)
	}

	close(release)
}

func TestSessionRunEnqueuePreservesSendAPIsWithoutEnqueue(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	result, err := service.SendSessionMessage(context.Background(), session.ID, "hello")
	if err != nil {
		t.Fatalf("SendSessionMessage() error = %v", err)
	}
	if result.Status != "committed" || result.TurnID != "turn-000001" {
		t.Fatalf("SendSessionMessage() = %#v, want committed turn-000001", result)
	}
	loaded, err := service.sessionStore.Load(session.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.RunningTurnID != "" {
		t.Fatalf("RunningTurnID = %q, want cleared", loaded.RunningTurnID)
	}
}

func TestSessionRunEnqueueVersusTerminalRaceNoLoss(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			// Tiny delay widens the window where Enqueue races the terminal
			// transition of the initial turn.
			select {
			case <-time.After(time.Millisecond):
			case <-ctx.Done():
				return SessionTurnResult{}, ctx.Err()
			}
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer-"+request.Content)); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	const iterations = 60
	type enqResult struct {
		receipt *PromptReceipt
		err     error
	}
	for i := 0; i < iterations; i++ {
		run := service.StartSessionRun(context.Background(), session.ID, "init", nil)
		resCh := make(chan enqResult, 1)
		go func() {
			receipt, err := run.Enqueue(PromptEvent{Source: PromptSourceStdin, Mode: PromptModeEnqueueTurn, Content: "race"})
			resCh <- enqResult{receipt: receipt, err: err}
		}()
		_, _ = run.Wait()
		res := <-resCh
		switch {
		case res.receipt != nil && res.err == nil:
			// Accepted: must be processed and committed, never dropped.
			rres, rerr := res.receipt.Wait()
			if rerr != nil || rres.Status != "committed" {
				t.Fatalf("iter %d: accepted receipt Wait = %#v/%v, want committed", i, rres, rerr)
			}
		case res.receipt == nil && errors.Is(res.err, ErrSessionRunSettled):
			// Explicitly rejected at the terminal transition: no loss.
		default:
			t.Fatalf("iter %d: Enqueue = (%#v, %v), want accepted+processed or ErrSessionRunSettled", i, res.receipt, res.err)
		}
	}
}
