package execution

import (
	"context"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestDurableRunAdmissionRecoveryDoesNotReplayModelWork(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: true})
	input := SessionMessageInput{Content: "durable command input"}
	admission, err := service.AdmitSessionRun(context.Background(), session.ID, input, "run-restart-admitted", "fingerprint-restart")
	if err != nil || !admission.Created || admission.Status != SessionRunRunning {
		t.Fatalf("initial durable admission=%#v err=%v", admission, err)
	}
	state, err := service.SessionStore().LoadState(session.ID)
	if err != nil || state.RunningRunID != "run-restart-admitted" {
		t.Fatalf("admitted state=%#v err=%v", state, err)
	}

	restarted, err := NewServiceWithOptions(home, ServiceOptions{TurnRunner: fakeExecutionTurnRunner{supports: true}})
	if err != nil {
		t.Fatalf("restart service error=%v", err)
	}
	retry, err := restarted.AdmitSessionRun(context.Background(), session.ID, input, "run-restart-admitted", "fingerprint-restart")
	if err != nil || retry.Created || retry.Status != SessionRunInterrupted {
		t.Fatalf("restart retry=%#v err=%v, want interrupted non-created", retry, err)
	}
	runs, err := restarted.SessionStore().ListRuns(session.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != sessions.RunStatusInterrupted {
		t.Fatalf("recovered runs=%#v err=%v", runs, err)
	}
}

func TestDurableRunAdmissionRebuildsServiceAndCoordinatorWithoutReplay(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: true})
	input := SessionMessageInput{Content: "rebuild coordinator input"}
	admission, err := service.AdmitSessionRun(context.Background(), session.ID, input, "run-rebuild-coordinator", "fingerprint-rebuild")
	if err != nil || !admission.Created {
		t.Fatalf("initial admission=%#v err=%v", admission, err)
	}

	var modelCalls int
	restarted, err := NewServiceWithOptions(home, ServiceOptions{TurnRunner: fakeExecutionTurnRunner{
		supports: true,
		run: func(context.Context, SessionTurnRequest) (SessionTurnResult, error) {
			modelCalls++
			return SessionTurnResult{Incremental: true}, nil
		},
	}})
	if err != nil {
		t.Fatalf("rebuild service error=%v", err)
	}
	coordinator := NewSessionRunCoordinator(context.Background(), restarted, SessionRunCoordinatorOptions{DurableAdmitter: restarted})
	defer coordinator.Close()
	run, retry, err := coordinator.StartDurable(session.ID, input, "run-rebuild-coordinator", "fingerprint-rebuild", nil)
	if err != nil || run != nil || retry.Created || retry.Status != SessionRunInterrupted {
		t.Fatalf("rebuild retry run=%#v admission=%#v err=%v, want interrupted lookup only", run, retry, err)
	}
	if modelCalls != 0 {
		t.Fatalf("restart retry invoked model runner %d times", modelCalls)
	}
}

func TestDurableRunAdmissionFailureBeforeClaimCanRetry(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: false})
	input := SessionMessageInput{Content: "retry after validation failure"}
	if _, err := service.AdmitSessionRun(context.Background(), session.ID, input, "run-pre-admission-retry", "fingerprint-pre-admission"); err == nil {
		t.Fatal("unsupported runner was durably admitted")
	}
	service.turnRunner = fakeExecutionTurnRunner{supports: true}
	admission, err := service.AdmitSessionRun(context.Background(), session.ID, input, "run-pre-admission-retry", "fingerprint-pre-admission")
	if err != nil || !admission.Created || admission.Status != SessionRunRunning {
		t.Fatalf("retry admission=%#v err=%v", admission, err)
	}
}

func TestNewServiceRecoversRunningSessionBeforeUse(t *testing.T) {
	home := t.TempDir()
	root, err := sessions.RootForHome(home)
	if err != nil {
		t.Fatalf("RootForHome() error = %v", err)
	}
	store := sessions.NewV2Store(root)
	session, err := store.SaveMetadata(sessions.SessionV2{ID: "recover-session"})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	if _, err := store.MarkTurnRunning(session.ID, "stale-turn"); err != nil {
		t.Fatalf("MarkTurnRunning() error = %v", err)
	}

	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if service == nil {
		t.Fatal("NewService() returned nil service")
	}
	recovered, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if recovered.RunningTurnID != "" {
		t.Fatalf("RunningTurnID = %q, want cleared", recovered.RunningTurnID)
	}
	if recovered.InterruptedTurnID != "stale-turn" || recovered.InterruptedAt.IsZero() {
		t.Fatalf("recovered interrupted state = %#v, want stale turn retained", recovered)
	}
	if _, err := store.MarkTurnRunning(session.ID, "new-turn"); err != nil {
		t.Fatalf("MarkTurnRunning(new) error = %v", err)
	}
}

func TestNewServiceRecoversRunTurnAndAllowsNextRun(t *testing.T) {
	home := t.TempDir()
	root, err := sessions.RootForHome(home)
	if err != nil {
		t.Fatalf("RootForHome() error = %v", err)
	}
	store := sessions.NewV2Store(root)
	session, err := store.SaveMetadata(sessions.SessionV2{ID: "recover-run-session"})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	if _, err := store.CreateRun(session.ID, "run-stale", "", nil, time.Now()); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	if _, err := store.StartTurn(session.ID, "run-stale", "turn-stale", 0, time.Now()); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	if _, err := NewService(home); err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	state, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.RunningRunID != "" || state.RunningTurnID != "" || state.InterruptedRunID != "run-stale" || state.InterruptedTurnID != "turn-stale" {
		t.Fatalf("recovered state = %#v, want stale run/turn interrupted", state)
	}
	runs, err := store.ListRuns(session.ID)
	if err != nil {
		t.Fatalf("ListRuns() error = %v", err)
	}
	turns, err := store.ListTurns(session.ID, "run-stale")
	if err != nil {
		t.Fatalf("ListTurns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].Status != sessions.RunStatusInterrupted || len(turns) != 1 || turns[0].Status != sessions.TurnStatusInterrupted {
		t.Fatalf("recovered lifecycle = runs %#v turns %#v", runs, turns)
	}
	if _, err := store.CreateRun(session.ID, "run-next", "run-stale", nil, time.Now()); err != nil {
		t.Fatalf("CreateRun(next) error = %v, want immediate next run admission", err)
	}
}
