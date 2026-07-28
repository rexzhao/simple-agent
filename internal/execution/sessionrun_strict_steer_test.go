package execution

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestTrySteerDrainsIntoCurrentTurn(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var drained []string
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return SessionTurnResult{}, ctx.Err()
			}
			messages := request.ActivePromptDrain(SessionActivePromptCheckpointBeforeTerminal)
			mu.Lock()
			for _, message := range messages {
				drained = append(drained, message.Content)
			}
			mu.Unlock()
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), runner)
	run := service.StartSessionRun(context.Background(), session.ID, "initial", nil)
	<-entered

	if err := run.TrySteer("strict follow-up"); err != nil {
		t.Fatalf("TrySteer() error = %v", err)
	}
	close(release)
	if _, err := run.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	mu.Lock()
	got := append([]string(nil), drained...)
	mu.Unlock()
	if !sameStringSlice(got, []string{"strict follow-up"}) {
		t.Fatalf("drained steer messages = %#v, want strict follow-up", got)
	}
	assertSessionTurnCount(t, service, session.ID, 1)
}

func TestTrySteerRejectsSealedTurnWhileWebAppendKeepsFollowUp(t *testing.T) {
	sealed := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var turnContents []string
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			mu.Lock()
			turnContents = append(turnContents, request.Content)
			mu.Unlock()
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer-"+request.Content)); err != nil {
				return SessionTurnResult{}, err
			}
			if got := request.ActivePromptDrain(SessionActivePromptCheckpointBeforeTerminal); len(got) != 0 {
				return SessionTurnResult{}, fmt.Errorf("unexpected prompts at terminal seal: %#v", got)
			}
			if request.Content == "initial" {
				once.Do(func() { close(sealed) })
				select {
				case <-release:
				case <-ctx.Done():
					return SessionTurnResult{}, ctx.Err()
				}
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), runner)
	run := service.StartSessionRun(context.Background(), session.ID, "initial", nil)
	<-sealed

	if got := run.Status(); got != SessionRunRunning {
		t.Fatalf("Status() = %q, want running while sealed turn has not returned", got)
	}
	if err := run.TrySteer("too late"); !errors.Is(err, ErrSessionNotSteerable) {
		t.Fatalf("TrySteer(sealed) error = %v, want ErrSessionNotSteerable", err)
	}
	// The Web policy intentionally remains no-loss: the same terminal window
	// accepts AppendActive and runs it as a follow-up turn.
	if err := run.AppendActive("web follow-up"); err != nil {
		t.Fatalf("AppendActive(sealed) error = %v", err)
	}
	close(release)
	if _, err := run.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	mu.Lock()
	gotContents := append([]string(nil), turnContents...)
	mu.Unlock()
	if !sameStringSlice(gotContents, []string{"initial", "web follow-up"}) {
		t.Fatalf("turn contents = %#v, want initial plus Web follow-up", gotContents)
	}
}

func TestTrySteerVersusTerminalSealIsLinearizable(t *testing.T) {
	const iterations = 200
	for iteration := 0; iteration < iterations; iteration++ {
		run := &SessionRun{accepting: true}
		run.setActiveTurn("turn-race", nil)
		drain := run.activePromptDrain()
		start := make(chan struct{})
		steerResult := make(chan error, 1)
		drainResult := make(chan []model.Message, 1)

		go func() {
			<-start
			steerResult <- run.TrySteer("race")
		}()
		go func() {
			<-start
			drainResult <- drain(SessionActivePromptCheckpointBeforeTerminal)
		}()
		close(start)

		err := <-steerResult
		messages := <-drainResult
		switch {
		case err == nil:
			if len(messages) != 1 || messages[0].Role != model.MessageRoleUser || messages[0].Content != "race" {
				t.Fatalf("iteration %d: accepted steer drained as %#v, want exactly one user message", iteration, messages)
			}
		case errors.Is(err, ErrSessionNotSteerable):
			if len(messages) != 0 {
				t.Fatalf("iteration %d: rejected steer still drained as %#v", iteration, messages)
			}
		default:
			t.Fatalf("iteration %d: TrySteer() error = %v", iteration, err)
		}
	}
}

func TestTrySteerRejectsBlankAndInactiveRun(t *testing.T) {
	run := &SessionRun{accepting: true}
	if err := run.TrySteer("message"); !errors.Is(err, ErrSessionNotSteerable) {
		t.Fatalf("TrySteer(inactive) error = %v, want ErrSessionNotSteerable", err)
	}
	run.setActiveTurn("turn-active", nil)
	if err := run.TrySteer("   "); err == nil || errors.Is(err, ErrSessionNotSteerable) {
		t.Fatalf("TrySteer(blank) error = %v, want validation error", err)
	}
	run.clearActiveTurn()
	if err := run.TrySteer("late"); !errors.Is(err, ErrSessionNotSteerable) {
		t.Fatalf("TrySteer(cleared) error = %v, want ErrSessionNotSteerable", err)
	}
}
