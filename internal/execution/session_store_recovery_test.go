package execution

import (
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

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
