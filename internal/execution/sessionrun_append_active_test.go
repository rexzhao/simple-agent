package execution

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

// promptQueueSnapshot records one run.prompt_queue emission for assertions.
type promptQueueSnapshot struct {
	TurnID  string
	Prompts []string
}

// collectQueueEvents returns an emit func that records every run.prompt_queue
// snapshot (deep-copying the prompt list) in emission order.
func collectQueueEvents(mu *sync.Mutex, snapshots *[]promptQueueSnapshot) func(SessionStreamEvent) {
	return func(event SessionStreamEvent) {
		if event == nil {
			return
		}
		if eventType, _ := event["type"].(string); eventType != "run.prompt_queue" {
			return
		}
		turnID, _ := event["turn_id"].(string)
		var prompts []string
		if raw, ok := event["prompts"].([]activePrompt); ok {
			for _, prompt := range raw {
				prompts = append(prompts, prompt.Content)
			}
		}
		mu.Lock()
		*snapshots = append(*snapshots, promptQueueSnapshot{TurnID: turnID, Prompts: prompts})
		mu.Unlock()
	}
}

func lastQueueSnapshot(snapshots []promptQueueSnapshot) promptQueueSnapshot {
	if len(snapshots) == 0 {
		return promptQueueSnapshot{}
	}
	return snapshots[len(snapshots)-1]
}

// TestAppendActiveDrainedIntoActiveTurn verifies the primary path: a prompt
// appended while a turn is running is consumed by the active prompt drain at a
// safe checkpoint and injected into the same turn id, and the queue snapshot
// goes empty once drained.
func TestAppendActiveDrainedIntoActiveTurn(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	var mu sync.Mutex
	var drained [][]string
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			// Hold the turn open so the test can append while running.
			select {
			case <-release:
			case <-ctx.Done():
				return SessionTurnResult{}, ctx.Err()
			}
			if request.ActivePromptDrain == nil {
				t.Errorf("ActivePromptDrain is nil, want wired drain")
			} else if got := request.ActivePromptDrain(SessionActivePromptCheckpointBeforeTerminal); len(got) > 0 {
				contents := make([]string, 0, len(got))
				for _, message := range got {
					if message.Role != model.MessageRoleUser {
						t.Errorf("drained message role = %q, want user", message.Role)
					}
					contents = append(contents, message.Content)
				}
				mu.Lock()
				drained = append(drained, contents)
				mu.Unlock()
			}
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	var mu2 sync.Mutex
	var snapshots []promptQueueSnapshot
	emit := collectQueueEvents(&mu2, &snapshots)

	run := service.StartSessionRun(context.Background(), session.ID, "init", emit)
	// Wait until the turn is blocked inside the runner before appending.
	waitForRunningTurn(t, service, session.ID)

	if err := run.AppendActive("follow-up question"); err != nil {
		t.Fatalf("AppendActive() error = %v", err)
	}
	close(release)

	if _, err := run.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	mu.Lock()
	gotDrained := drained
	mu.Unlock()
	if len(gotDrained) != 1 || !sameStringSlice(gotDrained[0], []string{"follow-up question"}) {
		t.Fatalf("drained = %#v, want single drain of follow-up question", gotDrained)
	}

	// The queue snapshot ends empty: append published a non-empty snapshot, the
	// drain consumed it and published an empty one.
	mu2.Lock()
	gotSnapshots := append([]promptQueueSnapshot(nil), snapshots...)
	mu2.Unlock()
	if len(gotSnapshots) < 2 {
		t.Fatalf("queue snapshots = %#v, want at least append + drain", gotSnapshots)
	}
	if got := lastQueueSnapshot(gotSnapshots); len(got.Prompts) != 0 {
		t.Fatalf("last snapshot prompts = %#v, want empty after drain", got.Prompts)
	}
	// No follow-up turn ran: only one turn total.
	assertSessionTurnCount(t, service, session.ID, 1)
}

// TestAppendActiveRemainderRunsFollowUpTurn verifies the fallback principle:
// prompts still queued when the active turn settles are not dropped; they are
// sent together as a follow-up turn.
func TestAppendActiveRemainderRunsFollowUpTurn(t *testing.T) {
	home := t.TempDir()
	var mu sync.Mutex
	var contents []string
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			mu.Lock()
			contents = append(contents, request.Content)
			mu.Unlock()
			// Never drain via ActivePromptDrain: appended prompts remain queued
			// until the turn settles, forcing the follow-up path.
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer-"+request.Content)); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	var mu2 sync.Mutex
	var snapshots []promptQueueSnapshot
	emit := collectQueueEvents(&mu2, &snapshots)

	run := service.StartSessionRun(context.Background(), session.ID, "init", emit)
	waitForRunningTurn(t, service, session.ID)

	if err := run.AppendActive("first"); err != nil {
		t.Fatalf("AppendActive(first) error = %v", err)
	}
	if err := run.AppendActive("second"); err != nil {
		t.Fatalf("AppendActive(second) error = %v", err)
	}

	result, err := run.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "committed" {
		t.Fatalf("Wait() status = %q, want committed", result.Status)
	}

	// Two turns ran: the initial "init" turn, then a follow-up turn carrying
	// both queued prompts joined together.
	mu.Lock()
	gotContents := append([]string(nil), contents...)
	mu.Unlock()
	if len(gotContents) != 2 {
		t.Fatalf("turn contents = %#v, want [init, joined follow-up]", gotContents)
	}
	if gotContents[0] != "init" {
		t.Fatalf("turn 1 content = %q, want init", gotContents[0])
	}
	if gotContents[1] != "first\n\nsecond" {
		t.Fatalf("follow-up content = %q, want %q", gotContents[1], "first\n\nsecond")
	}
	assertSessionTurnCount(t, service, session.ID, 2)
}

// TestAppendActiveDroppedOnFailure verifies that when the active turn fails or
// is cancelled, still-queued prompts are not silently sent later; the queue is
// cleared and the published snapshot goes empty.
func TestAppendActiveDroppedOnFailure(t *testing.T) {
	home := t.TempDir()
	release := make(chan struct{})
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return SessionTurnResult{}, ctx.Err()
			}
			return SessionTurnResult{}, errors.New("provider exploded")
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	var mu2 sync.Mutex
	var snapshots []promptQueueSnapshot
	emit := collectQueueEvents(&mu2, &snapshots)

	run := service.StartSessionRun(context.Background(), session.ID, "init", emit)
	waitForRunningTurn(t, service, session.ID)

	if err := run.AppendActive("never sent"); err != nil {
		t.Fatalf("AppendActive() error = %v", err)
	}
	close(release)

	if _, err := run.Wait(); !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("Wait() error = %v, want ErrTurnFailed", err)
	}

	mu2.Lock()
	got := lastQueueSnapshot(snapshots)
	mu2.Unlock()
	if len(got.Prompts) != 0 {
		t.Fatalf("last snapshot prompts = %#v, want empty after failure", got.Prompts)
	}
	// The queued prompt was not sent as a follow-up turn.
	assertSessionTurnCount(t, service, session.ID, 1)
}

// TestAppendActiveRejectsSettledAndEmpty verifies acceptance boundaries.
func TestAppendActiveRejectsSettledAndEmpty(t *testing.T) {
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

	run := service.StartSessionRun(context.Background(), session.ID, "init", nil)
	if err := run.AppendActive("   "); err == nil {
		t.Fatalf("AppendActive(blank) error = nil, want non-empty content error")
	}
	if _, err := run.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if err := run.AppendActive("late"); !errors.Is(err, ErrSessionRunSettled) {
		t.Fatalf("AppendActive(settled) error = %v, want ErrSessionRunSettled", err)
	}
}

// TestAppendActiveRemoveQueued verifies a not-yet-sent prompt can be removed by
// id, the snapshot reflects the removal, and the removed prompt is never sent.
func TestAppendActiveRemoveQueued(t *testing.T) {
	home := t.TempDir()
	var mu sync.Mutex
	var contents []string
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			mu.Lock()
			contents = append(contents, request.Content)
			mu.Unlock()
			// Do not drain: appended prompts stay queued until settle, forcing
			// the follow-up path for whatever remains after removal.
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer-"+request.Content)); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	var mu2 sync.Mutex
	var snapshots []promptQueueSnapshot
	emit := collectQueueEvents(&mu2, &snapshots)

	run := service.StartSessionRun(context.Background(), session.ID, "init", emit)
	waitForRunningTurn(t, service, session.ID)

	if err := run.AppendActive("keep"); err != nil {
		t.Fatalf("AppendActive(keep) error = %v", err)
	}
	if err := run.AppendActive("drop"); err != nil {
		t.Fatalf("AppendActive(drop) error = %v", err)
	}

	// Remove the "drop" prompt by its id (ap-2, the second accepted append).
	if !run.RemoveActive("ap-2") {
		t.Fatalf("RemoveActive(ap-2) = false, want true")
	}
	// Removing an unknown or already-removed id is a no-op.
	if run.RemoveActive("ap-2") {
		t.Fatalf("RemoveActive(ap-2) again = true, want false")
	}
	if run.RemoveActive("ap-999") {
		t.Fatalf("RemoveActive(ap-999) = true, want false")
	}

	if _, err := run.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	// Only "keep" was sent as the follow-up turn.
	mu.Lock()
	gotContents := append([]string(nil), contents...)
	mu.Unlock()
	if len(gotContents) != 2 || gotContents[0] != "init" || gotContents[1] != "keep" {
		t.Fatalf("turn contents = %#v, want [init keep]", gotContents)
	}

	// The snapshot after removal holds only "keep", and the queue ends empty.
	mu2.Lock()
	gotSnapshots := append([]promptQueueSnapshot(nil), snapshots...)
	mu2.Unlock()
	var sawKeepOnly bool
	for _, snapshot := range gotSnapshots {
		if sameStringSlice(snapshot.Prompts, []string{"keep"}) {
			sawKeepOnly = true
		}
	}
	if !sawKeepOnly {
		t.Fatalf("snapshots = %#v, want one holding only keep after removal", gotSnapshots)
	}
	if got := lastQueueSnapshot(gotSnapshots); len(got.Prompts) != 0 {
		t.Fatalf("last snapshot prompts = %#v, want empty", got.Prompts)
	}
}

// waitForRunningTurn blocks until the session's running turn id is set, so the
// test can AppendActive against an in-flight turn deterministically.
func waitForRunningTurn(t *testing.T, service *Service, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, err := service.sessionStore.Load(sessionID)
		if err == nil && session.RunningTurnID != "" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %s did not enter running state", sessionID)
}

// assertSessionTurnCount loads the session and counts committed turn items by
// scanning user messages, used to verify how many turns actually ran.
func assertSessionTurnCount(t *testing.T, service *Service, sessionID string, want int) {
	t.Helper()
	session, err := service.sessionStore.Load(sessionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	users := 0
	for _, item := range session.Items {
		if item.Message != nil && item.Message.Role == model.MessageRoleUser {
			users++
		}
	}
	if users != want {
		t.Fatalf("user message count = %d, want %d (items=%#v)", users, want, sessionItemIDs(session.Items))
	}
}
