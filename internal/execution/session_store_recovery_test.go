package execution

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

func admitInterruptedTestRun(t *testing.T, service *Service, sessionID, runID, turnID string) {
	t.Helper()
	store := service.SessionStore()
	if _, err := store.CreateRun(sessionID, runID, "", []byte(`{"content":"original"}`), time.Now().UTC()); err != nil {
		t.Fatalf("CreateRun(%q) error = %v", runID, err)
	}
	if _, err := store.StartTurn(sessionID, runID, turnID, 0, time.Now().UTC()); err != nil {
		t.Fatalf("StartTurn(%q) error = %v", turnID, err)
	}
	if _, err := store.SetTurnStatus(sessionID, runID, turnID, sessions.TurnStatusFailed, time.Now().UTC()); err != nil {
		t.Fatalf("SetTurnStatus(%q) error = %v", turnID, err)
	}
	if _, err := store.SetRunStatus(sessionID, runID, sessions.RunStatusFailed, time.Now().UTC()); err != nil {
		t.Fatalf("SetRunStatus(%q) error = %v", runID, err)
	}
}

func TestDurableContinueAdmissionBindsInterruptedRunAndTurn(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: true})
	admitInterruptedTestRun(t, service, session.ID, "run-interrupted-target", "turn-interrupted-target")

	input := SessionMessageInput{Continue: true}
	admission, err := service.AdmitSessionRun(context.Background(), session.ID, input, "run-continue-bound", "fingerprint-continue-bound")
	if err != nil || !admission.Created || admission.Status != SessionRunRunning {
		t.Fatalf("continue admission=%#v err=%v", admission, err)
	}
	runs, err := service.SessionStore().ListRuns(session.ID)
	if err != nil {
		t.Fatalf("ListRuns() error = %v", err)
	}
	var continued sessions.RunRecord
	for _, run := range runs {
		if run.ID == "run-continue-bound" {
			continued = run
		}
	}
	if continued.PreviousRunID != "run-interrupted-target" || continued.Status != sessions.RunStatusRunning {
		t.Fatalf("continue row=%#v, want previous interrupted target", continued)
	}
	var payload SessionMessageInput
	if err := json.Unmarshal(continued.InputPayload, &payload); err != nil {
		t.Fatalf("unmarshal continue payload: %v", err)
	}
	if !payload.Continue || payload.Content != "" || payload.ContinueTargetRunID != "run-interrupted-target" || payload.ContinueTargetTurnID != "turn-interrupted-target" {
		t.Fatalf("continue payload=%#v, want server-bound target and no content", payload)
	}

	// Move the session's current interrupted marker to a later failed run. A
	// retry of the original stable identity must still resolve the recorded row
	// and status, not reinterpret it against this newer marker.
	if _, err := service.SessionStore().StartTurn(session.ID, "run-continue-bound", "turn-continue-bound", 0, time.Now().UTC()); err != nil {
		t.Fatalf("StartTurn(continue) error = %v", err)
	}
	if _, err := service.SessionStore().SetTurnStatus(session.ID, "run-continue-bound", "turn-continue-bound", sessions.TurnStatusFailed, time.Now().UTC()); err != nil {
		t.Fatalf("SetTurnStatus(continue) error = %v", err)
	}
	if _, err := service.SessionStore().SetRunStatus(session.ID, "run-continue-bound", sessions.RunStatusFailed, time.Now().UTC()); err != nil {
		t.Fatalf("SetRunStatus(continue) error = %v", err)
	}
	state, err := service.SessionStore().LoadState(session.ID)
	if err != nil || state.InterruptedRunID != "run-continue-bound" {
		t.Fatalf("current interrupted state=%#v err=%v", state, err)
	}
	retry, err := service.AdmitSessionRun(context.Background(), session.ID, input, "run-continue-bound", "fingerprint-continue-bound")
	if err != nil || retry.Created || retry.Status != SessionRunFailed {
		t.Fatalf("bound retry=%#v err=%v, want recorded failed status", retry, err)
	}
	runs, err = service.SessionStore().ListRuns(session.ID)
	if err != nil {
		t.Fatalf("ListRuns(retry) error = %v", err)
	}
	for _, run := range runs {
		if run.ID == "run-continue-bound" && run.PreviousRunID != "run-interrupted-target" {
			t.Fatalf("retry changed durable target: %#v", run)
		}
	}
}

func TestDurableContinueValidationFailureBeforeClaimCanRetry(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: false})
	admitInterruptedTestRun(t, service, session.ID, "run-continue-precondition", "turn-continue-precondition")
	input := SessionMessageInput{Continue: true}
	if _, err := service.AdmitSessionRun(context.Background(), session.ID, input, "run-continue-precondition-retry", "fingerprint-continue-precondition"); err == nil {
		t.Fatal("unsupported continue runner was durably admitted")
	}
	if runs, err := service.SessionStore().ListRuns(session.ID); err != nil {
		t.Fatal(err)
	} else if len(runs) != 1 {
		t.Fatalf("pre-admission failure created durable run rows=%#v", runs)
	}
	service.turnRunner = fakeExecutionTurnRunner{supports: true}
	admission, err := service.AdmitSessionRun(context.Background(), session.ID, input, "run-continue-precondition-retry", "fingerprint-continue-precondition")
	if err != nil || !admission.Created || admission.Status != SessionRunRunning {
		t.Fatalf("retry continue admission=%#v err=%v", admission, err)
	}
}

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
