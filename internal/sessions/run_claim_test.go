package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestPromptAppendClaimIsDurableAtMostOnceTombstone(t *testing.T) {
	store := NewV2Store(t.TempDir())
	const sessionID = "session-prompt-claim"
	const runID = "run-prompt-claim"
	const operationID = "operation-prompt-claim"
	if _, err := store.SaveMetadata(SessionV2{ID: sessionID}); err != nil {
		t.Fatal(err)
	}

	lock, err := store.AcquirePromptAppendAdmissionLock(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	claim, created, err := store.ClaimPromptAppendWhileLocked(context.Background(), sessionID, runID, operationID, "  exact  ", time.Time{})
	if err != nil || !created || claim.Status != PromptAppendStatusAdmitted {
		t.Fatalf("initial prompt claim=%#v created=%v err=%v", claim, created, err)
	}
	if err := store.SetPromptAppendClaimStatus(operationID, PromptAppendStatusApplied, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	lock, err = store.AcquirePromptAppendAdmissionLock(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	retry, created, err := store.ClaimPromptAppendWhileLocked(context.Background(), sessionID, runID, operationID, "  exact  ", time.Time{})
	if err != nil || created || retry.Status != PromptAppendStatusApplied {
		t.Fatalf("applied retry claim=%#v created=%v err=%v", retry, created, err)
	}
	if _, _, err := store.ClaimPromptAppendWhileLocked(context.Background(), sessionID, runID, operationID, "different", time.Time{}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different content error=%v, want idempotency conflict", err)
	}
	_ = lock.Release()

	const crashedOperation = "operation-prompt-crash"
	lock, err = store.AcquirePromptAppendAdmissionLock(context.Background(), crashedOperation)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.ClaimPromptAppendWhileLocked(context.Background(), sessionID, runID, crashedOperation, "crash boundary", time.Time{}); err != nil || !created {
		t.Fatalf("crash-boundary admission created=%v err=%v", created, err)
	}
	_ = lock.Release() // model a process death before queue append/status
	lock, err = store.AcquirePromptAppendAdmissionLock(context.Background(), crashedOperation)
	if err != nil {
		t.Fatal(err)
	}
	recovered, created, err := store.ClaimPromptAppendWhileLocked(context.Background(), sessionID, runID, crashedOperation, "crash boundary", time.Time{})
	if err != nil || created || recovered.Status != PromptAppendStatusOutcomeUnknown {
		t.Fatalf("recovered claim=%#v created=%v err=%v, want outcome_unknown tombstone", recovered, created, err)
	}
	_ = lock.Release()
}

func TestPromptAppendClaimStoresOnlyContentDigest(t *testing.T) {
	store := NewV2Store(t.TempDir())
	const sessionID = "session-prompt-digest"
	const runID = "run-prompt-digest"
	const operationID = "operation-prompt-digest"
	const sensitivePrompt = "sensitive prompt must not enter the global claim index"

	lock, err := store.AcquirePromptAppendAdmissionLock(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	claim, created, err := store.ClaimPromptAppendWhileLocked(context.Background(), sessionID, runID, operationID, sensitivePrompt, time.Time{})
	if err != nil || !created {
		t.Fatalf("claim=%#v created=%v err=%v", claim, created, err)
	}
	if claim.ContentSHA256 != PromptAppendContentSHA256(sensitivePrompt) {
		t.Fatalf("content digest=%q, want %q", claim.ContentSHA256, PromptAppendContentSHA256(sensitivePrompt))
	}
	if err := store.SetPromptAppendClaimStatus(operationID, PromptAppendStatusApplied, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	db, err := store.openRunClaimsDB(false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA table_info(prompt_append_claims)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seenDigest, seenContent := false, false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		seenDigest = seenDigest || name == "content_sha256"
		seenContent = seenContent || name == "content"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seenDigest || seenContent {
		t.Fatalf("prompt claim schema has digest=%v raw-content=%v", seenDigest, seenContent)
	}
	var storedDigest string
	if err := db.QueryRow(`SELECT content_sha256 FROM prompt_append_claims WHERE operation_id = ?`, operationID).Scan(&storedDigest); err != nil {
		t.Fatal(err)
	}
	if storedDigest == sensitivePrompt || storedDigest != PromptAppendContentSHA256(sensitivePrompt) {
		t.Fatalf("stored content value=%q, want only SHA-256 digest", storedDigest)
	}
}

func TestRunAdmissionLockSerializesClaimOnlyWindow(t *testing.T) {
	root := t.TempDir()
	firstStore := NewV2Store(root)
	secondStore := NewV2Store(root)
	first, err := firstStore.AcquireRunAdmissionLock(context.Background(), "run-lock-window")
	if err != nil {
		t.Fatalf("first AcquireRunAdmissionLock() error = %v", err)
	}
	defer first.Release()

	acquired := make(chan struct{})
	var second *RunAdmissionLock
	var secondErr atomic.Value
	go func() {
		lock, err := secondStore.AcquireRunAdmissionLock(context.Background(), "run-lock-window")
		if err != nil {
			secondErr.Store(err)
			return
		}
		second = lock
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second coordinator acquired the run lock during the first claim-only window")
	case <-time.After(50 * time.Millisecond):
	}
	if err := first.Release(); err != nil {
		t.Fatalf("first lock release error = %v", err)
	}
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatalf("second coordinator did not acquire the run lock after release; err=%v", secondErr.Load())
	}
	if second == nil {
		t.Fatal("second coordinator returned no lock")
	}
	if err := second.Release(); err != nil {
		t.Fatalf("second lock release error = %v", err)
	}
}

func TestRunArtifactsStayBelowClaimsNamespaceWithoutReservingPublicIDs(t *testing.T) {
	root := t.TempDir()
	store := NewV2Store(root)
	ids := []string{"run-claims.db", ".run-admission-locks"}
	for _, id := range ids {
		if err := ValidateSessionID(id); err != nil {
			t.Fatalf("ValidateSessionID(%q) = %v, want allowed", id, err)
		}
		if err := ValidateSessionCreateID(id); err != nil {
			t.Fatalf("ValidateSessionCreateID(%q) = %v, want allowed", id, err)
		}
		if _, err := store.SaveMetadata(SessionV2{ID: id, ProjectID: "project-" + id}); err != nil {
			t.Fatalf("SaveMetadata(%q) error = %v", id, err)
		}
		if _, err := store.LoadState(id); err != nil {
			t.Fatalf("LoadState(%q) error = %v", id, err)
		}
	}

	if _, created, err := store.ClaimRun(context.Background(), "run-claims.db", "run-artifact-namespace", "fingerprint-artifact", time.Now().UTC()); err != nil || !created {
		t.Fatalf("ClaimRun() = created=%v err=%v, want created", created, err)
	}
	if info, err := os.Stat(filepath.Join(root, "run-claims.db")); err != nil || !info.IsDir() {
		t.Fatalf("public run-claims.db session path = %v/%v, want session directory", info, err)
	}
	if info, err := os.Stat(filepath.Join(root, runAdmissionLockDirectory)); err != nil || !info.IsDir() {
		t.Fatalf("public .run-admission-locks session path = %v/%v, want session directory", info, err)
	}

	databasePath := filepath.Join(root, sessionClaimsDirName, runClaimsDatabaseDirectory, runClaimsDatabaseFileName)
	if info, err := os.Stat(databasePath); err != nil || info.IsDir() {
		t.Fatalf("internal run claims database = %v/%v, want private file", info, err)
	}
	lockDirectory := filepath.Join(root, sessionClaimsDirName, runAdmissionLockDirectory)
	if info, err := os.Stat(lockDirectory); err != nil || !info.IsDir() {
		t.Fatalf("internal run lock directory = %v/%v, want directory", info, err)
	}

	states, err := store.ListStates(V2ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListStates() error = %v", err)
	}
	if len(states) != len(ids) {
		t.Fatalf("ListStates() = %#v, want exactly the two public sessions", states)
	}
	for _, id := range ids {
		found := false
		for _, state := range states {
			if state.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ListStates() omitted public session %q: %#v", id, states)
		}
	}
}

func TestRunClaimRejectsCorruptStatusAndSecondaryClaimFailureDoesNotBreakAuthority(t *testing.T) {
	root := t.TempDir()
	store := NewV2Store(root)
	session, err := store.SaveMetadata(SessionV2{ID: "session-claim-authority", ProjectID: "project-claim-authority"})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	started := time.Now().UTC()
	if _, err := store.CreateRunWithFingerprint(session.ID, "run-claim-authority", "", []byte(`{"content":"hello"}`), "fingerprint-authority", started); err != nil {
		t.Fatalf("CreateRunWithFingerprint() error = %v", err)
	}
	if _, created, err := store.ClaimRun(context.Background(), session.ID, "run-claim-authority", "fingerprint-authority", started); err != nil || !created {
		t.Fatalf("ClaimRun() = created=%v err=%v, want created", created, err)
	}

	store.runClaimStatusWriter = func(string, string, time.Time) error {
		return errors.New("injected claim index failure")
	}
	probe := &mutationProbe{ready: make(chan struct{}), all: make(chan Mutation, 2)}
	registration := store.RegisterMutationSink(probe)
	defer registration.Unregister()
	state, err := store.SetRunStatus(session.ID, "run-claim-authority", RunStatusCommitted, time.Now().UTC())
	if err != nil {
		t.Fatalf("SetRunStatus() returned secondary claim failure = %v", err)
	}
	mutation := waitMutation(t, probe.all)
	if mutation.SessionID != session.ID || mutation.Revision != state.LastSeq || mutation.Deleted {
		t.Fatalf("terminal mutation = %#v, want authoritative settlement revision", mutation)
	}
	if state.LastRunID != "run-claim-authority" || state.LastRunStatus != RunStatusCommitted || state.RunningRunID != "" {
		t.Fatalf("authoritative state after terminal commit = %#v", state)
	}
	run, err := store.GetRun(session.ID, "run-claim-authority")
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if run.Status != RunStatusCommitted {
		t.Fatalf("authoritative run status = %q, want committed", run.Status)
	}
	claim, err := store.GetRunClaim("run-claim-authority")
	if err != nil {
		t.Fatalf("GetRunClaim() after injected writer error = %v", err)
	}
	if claim.Status != RunStatusRunning {
		t.Fatalf("claim status after injected writer error = %q, want stale running", claim.Status)
	}
	store.runClaimStatusWriter = nil
	if err := store.SetRunClaimStatus("run-claim-authority", RunStatusCommitted, run.SettledAt); err != nil {
		t.Fatalf("claim reconciliation error = %v", err)
	}
	claim, err = store.GetRunClaim("run-claim-authority")
	if err != nil || claim.Status != RunStatusCommitted {
		t.Fatalf("reconciled claim = %#v err=%v", claim, err)
	}

	corruptSession, err := store.SaveMetadata(SessionV2{ID: "session-corrupt-claim"})
	if err != nil {
		t.Fatalf("SaveMetadata(corrupt) error = %v", err)
	}
	if _, err := store.CreateRunWithFingerprint(corruptSession.ID, "run-corrupt-claim", "", nil, "fingerprint-corrupt", started); err != nil {
		t.Fatalf("CreateRunWithFingerprint(corrupt) error = %v", err)
	}
	if _, created, err := store.ClaimRun(context.Background(), corruptSession.ID, "run-corrupt-claim", "fingerprint-corrupt", started); err != nil || !created {
		t.Fatalf("ClaimRun(corrupt) = created=%v err=%v", created, err)
	}
	db, err := store.openRunClaimsDB(false)
	if err != nil {
		t.Fatalf("openRunClaimsDB() error = %v", err)
	}
	if _, err := db.Exec(`UPDATE run_claims SET status = 'corrupt' WHERE run_id = ?`, "run-corrupt-claim"); err != nil {
		db.Close()
		t.Fatalf("corrupt claim update error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close claims db error = %v", err)
	}
	if _, err := store.GetRunClaim("run-corrupt-claim"); err == nil || !errors.Is(err, ErrInvalidRunClaimStatus) {
		t.Fatalf("GetRunClaim(corrupt) error = %v, want invalid status", err)
	}
	if _, _, err := store.ClaimRun(context.Background(), corruptSession.ID, "run-corrupt-claim", "fingerprint-corrupt", started); err == nil || !errors.Is(err, ErrInvalidRunClaimStatus) {
		t.Fatalf("ClaimRun(corrupt) error = %v, want invalid status", err)
	}
}

func TestRunClaimMirrorDoesNotRevertTerminalStatusAfterStaleRunningRepair(t *testing.T) {
	store := NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(SessionV2{ID: "session-claim-mirror-race"})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	started := time.Now().UTC()
	if _, err := store.CreateRunWithFingerprint(session.ID, "run-claim-mirror-race", "", nil, "fingerprint-mirror", started); err != nil {
		t.Fatalf("CreateRunWithFingerprint() error = %v", err)
	}
	if _, created, err := store.ClaimRun(context.Background(), session.ID, "run-claim-mirror-race", "fingerprint-mirror", started); err != nil || !created {
		t.Fatalf("ClaimRun() = created=%v err=%v, want created", created, err)
	}
	claim, err := store.GetRunClaim("run-claim-mirror-race")
	if err != nil || !claim.SettledAt.IsZero() {
		t.Fatalf("initial running claim = %#v err=%v, want zero settled_at", claim, err)
	}
	if err := store.SetRunClaimStatus("run-claim-mirror-race", RunStatusRunning, time.Now().UTC()); err != nil {
		t.Fatalf("running claim repair error = %v", err)
	}
	claim, err = store.GetRunClaim("run-claim-mirror-race")
	if err != nil || !claim.SettledAt.IsZero() {
		t.Fatalf("running claim after repair = %#v err=%v, want zero settled_at", claim, err)
	}

	// Model a stale lookup which read the running row before the authoritative
	// terminal transaction. The terminal mirror wins, and the late running
	// repair is a SQL no-op rather than a status rollback.
	terminalAt := time.Now().UTC()
	if _, err := store.SetRunStatus(session.ID, "run-claim-mirror-race", RunStatusCommitted, terminalAt); err != nil {
		t.Fatalf("SetRunStatus() error = %v", err)
	}
	if err := store.SetRunClaimStatus("run-claim-mirror-race", RunStatusRunning, time.Now().UTC()); err != nil {
		t.Fatalf("stale running repair error = %v", err)
	}
	claim, err = store.GetRunClaim("run-claim-mirror-race")
	if err != nil {
		t.Fatalf("GetRunClaim() after stale repair error = %v", err)
	}
	if claim.Status != RunStatusCommitted || claim.SettledAt.IsZero() {
		t.Fatalf("claim after stale running repair = %#v, want committed with settled_at", claim)
	}

	deleteSession, err := store.SaveMetadata(SessionV2{ID: "session-claim-delete-race"})
	if err != nil {
		t.Fatalf("SaveMetadata(delete) error = %v", err)
	}
	if _, err := store.CreateRunWithFingerprint(deleteSession.ID, "run-claim-delete-race", "", nil, "fingerprint-delete-race", started); err != nil {
		t.Fatalf("CreateRunWithFingerprint(delete) error = %v", err)
	}
	if _, created, err := store.ClaimRun(context.Background(), deleteSession.ID, "run-claim-delete-race", "fingerprint-delete-race", started); err != nil || !created {
		t.Fatalf("ClaimRun(delete) = created=%v err=%v, want created", created, err)
	}
	if err := store.Delete(deleteSession.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.SetRunClaimStatus("run-claim-delete-race", RunStatusRunning, time.Now().UTC()); err != nil {
		t.Fatalf("stale running repair after delete error = %v", err)
	}
	claim, err = store.GetRunClaim("run-claim-delete-race")
	if err != nil {
		t.Fatalf("GetRunClaim() after delete stale repair error = %v", err)
	}
	if claim.Status != RunStatusInterrupted || claim.SettledAt.IsZero() {
		t.Fatalf("claim after delete stale repair = %#v, want interrupted with settled_at", claim)
	}
}

func TestDeleteReconcilesCommandRunClaimBeforeRemovingSession(t *testing.T) {
	store := NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(SessionV2{ID: "session-delete-claim-run"})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	started := time.Now().UTC()
	if _, err := store.CreateRunWithFingerprint(session.ID, "run-delete-claim", "", nil, "fingerprint-delete", started); err != nil {
		t.Fatalf("CreateRunWithFingerprint() error = %v", err)
	}
	if _, created, err := store.ClaimRun(context.Background(), session.ID, "run-delete-claim", "fingerprint-delete", started); err != nil || !created {
		t.Fatalf("ClaimRun() = created=%v err=%v", created, err)
	}
	if err := store.Delete(session.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	claim, err := store.GetRunClaim("run-delete-claim")
	if err != nil {
		t.Fatalf("GetRunClaim() after Delete() error = %v", err)
	}
	if claim.Status != RunStatusInterrupted {
		t.Fatalf("deleted session run claim status = %q, want interrupted", claim.Status)
	}
}
