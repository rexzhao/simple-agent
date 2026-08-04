package execution

import (
	"context"
	"errors"
	"testing"

	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestSessionContinueCreatesLinkedRunWithoutDuplicatingUserInput(t *testing.T) {
	calls := 0
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(_ context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			calls++
			messages, err := request.Session.MaterializeActiveHistory()
			if err != nil {
				return SessionTurnResult{}, err
			}
			if request.ResumeContext {
				if len(messages) != 1 || messages[0].Role != model.MessageRoleUser || messages[0].Content != "keep this once" {
					t.Fatalf("Continue history = %#v, want one durable user message", messages)
				}
				if err := request.Publisher.Publish(eventAssistant(request.TurnID, "continued")); err != nil {
					return SessionTurnResult{}, err
				}
				return SessionTurnResult{Incremental: true}, nil
			}
			return SessionTurnResult{}, errors.New("simulated interruption")
		},
	}
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), runner)

	first := service.StartSessionRun(context.Background(), session.ID, "keep this once", nil)
	if _, err := first.Wait(); !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("first Wait() error = %v, want ErrTurnFailed", err)
	}
	state, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(after failure) error = %v", err)
	}
	if state.InterruptedRunID == "" || state.InterruptedTurnID == "" || state.LastRunStatus != sessions.RunStatusFailed || state.RunningRunID != "" {
		t.Fatalf("state after failure = %#v, want interrupted run/turn and no running run", state)
	}
	if err := service.ValidateContinue(session.ID); err != nil {
		t.Fatalf("ValidateContinue() error = %v", err)
	}

	continued := service.StartSessionRunWithInput(context.Background(), session.ID, SessionMessageInput{Continue: true}, nil)
	if _, err := continued.Wait(); err != nil {
		t.Fatalf("Continue Wait() error = %v", err)
	}
	runs, err := service.sessionStore.ListRuns(session.ID)
	if err != nil {
		t.Fatalf("ListRuns() error = %v", err)
	}
	if len(runs) != 2 || runs[1].PreviousRunID != runs[0].ID || runs[1].Status != "committed" {
		t.Fatalf("runs = %#v, want linked failed then committed runs", runs)
	}
	firstTurns, err := service.sessionStore.ListTurns(session.ID, runs[0].ID)
	if err != nil {
		t.Fatalf("ListTurns(first run) error = %v", err)
	}
	secondTurns, err := service.sessionStore.ListTurns(session.ID, runs[1].ID)
	if err != nil {
		t.Fatalf("ListTurns(second run) error = %v", err)
	}
	if len(firstTurns) != 1 || len(secondTurns) != 1 || firstTurns[0].ID == secondTurns[0].ID || secondTurns[0].Ordinal != 1 {
		t.Fatalf("run turns = first %#v second %#v, want unique session ids and ordinal reset", firstTurns, secondTurns)
	}
	state, err = service.sessionStore.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("LoadExecutionState() error = %v", err)
	}
	messages, err := state.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if len(messages) != 2 || messages[0].Content != "keep this once" || messages[1].Content != "continued" {
		t.Fatalf("active messages = %#v, want original user once and continued answer", messages)
	}
	if calls != 2 {
		t.Fatalf("runner calls = %d, want 2", calls)
	}
}

func TestSessionRunPersistsOneTurnPerModelRequest(t *testing.T) {
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(_ context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			if err := request.Publisher.Publish(eventbus.TurnCompleted{TurnID: request.TurnID}); err != nil {
				return SessionTurnResult{}, err
			}
			if err := request.Publisher.Publish(eventbus.TurnStarted{TurnID: "turn-model-2"}); err != nil {
				return SessionTurnResult{}, err
			}
			if err := request.Publisher.Publish(eventAssistant("turn-model-2", "answer from second request")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), runner)
	result, err := service.SendSessionMessage(context.Background(), session.ID, "one request starts the run")
	if err != nil {
		t.Fatalf("SendSessionMessage() error = %v", err)
	}
	turns, err := service.sessionStore.ListTurns(session.ID, result.RunID)
	if err != nil {
		t.Fatalf("ListTurns() error = %v", err)
	}
	if len(turns) != 2 || turns[0].Ordinal != 1 || turns[1].Ordinal != 2 || turns[0].ID == turns[1].ID {
		t.Fatalf("turns = %#v, want two distinct ordinal turns", turns)
	}
	if turns[0].Status != sessions.TurnStatusCommitted || turns[1].Status != sessions.TurnStatusCommitted {
		t.Fatalf("turn statuses = %#v, want committed", turns)
	}
}

func TestSessionRunDoesNotReinterruptCompletedTurnWhenNextRequestFails(t *testing.T) {
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(_ context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			if err := request.Publisher.Publish(eventbus.TurnCompleted{TurnID: request.TurnID}); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{}, errors.New("failed before next model request")
		},
	}
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), runner)
	run := service.StartSessionRun(context.Background(), session.ID, "one request", nil)
	if _, err := run.Wait(); !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("Wait() error = %v, want ErrTurnFailed", err)
	}
	runs, err := service.sessionStore.ListRuns(session.ID)
	if err != nil {
		t.Fatalf("ListRuns() error = %v", err)
	}
	turns, err := service.sessionStore.ListTurns(session.ID, runs[0].ID)
	if err != nil {
		t.Fatalf("ListTurns() error = %v", err)
	}
	if len(turns) != 1 || turns[0].Status != sessions.TurnStatusCommitted {
		t.Fatalf("turns = %#v, want one committed turn after internal completion", turns)
	}
	state, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.InterruptedRunID != runs[0].ID || state.InterruptedTurnID != turns[0].ID || state.LastRunStatus != sessions.RunStatusFailed {
		t.Fatalf("state = %#v, want failed run anchored to committed turn", state)
	}
}
