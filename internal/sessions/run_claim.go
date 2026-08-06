package sessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RunClaim is the durable, server-wide identity claim for a command-owned
// run. Session run rows remain the lifecycle authority; this small shared
// table prevents an explicit run ID from being claimed by another session and
// closes the cross-session race between independent session databases.
type RunClaim struct {
	RunID            string
	SessionID        string
	InputFingerprint string
	Status           string
	StartedAt        time.Time
	SettledAt        time.Time
}

const (
	// These are directories below the already-reserved .session-claims root,
	// not entries in the public session namespace. Keeping the SQLite file one
	// level below runClaimsDatabaseDirectory also lets a deleted session whose
	// ID is "run-claims.db" retain its claim directory without colliding with
	// the shared database.
	runClaimsDatabaseDirectory = "run-claims.db"
	runClaimsDatabaseFileName  = "claims.db"
	runAdmissionLockDirectory  = ".run-admission-locks"
)

// RunAdmissionLock spans the shared claim and the per-session run row
// admission. Unlike the session writer lock, its inode is keyed only by the
// stable run identity, so independent coordinators cannot observe a claim-only
// window and decide that the owner has died while it is still committing the
// session row.
type RunAdmissionLock struct {
	file     *os.File
	path     string
	released bool
}

func (s *V2Store) AcquireRunAdmissionLock(ctx context.Context, runID string) (*RunAdmissionLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	if err := ValidateRunID(runID); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(runID)))
	if err := ensurePrivateDirectory(filepath.Join(s.root, sessionClaimsDirName)); err != nil {
		return nil, fmt.Errorf("create session claims root for run lock: %w", err)
	}
	lockDirectory := filepath.Join(s.root, sessionClaimsDirName, runAdmissionLockDirectory)
	if err := ensurePrivateDirectory(lockDirectory); err != nil {
		return nil, fmt.Errorf("create run admission lock directory: %w", err)
	}
	path := filepath.Join(lockDirectory, hex.EncodeToString(digest[:])+".lock")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open run admission lock %q: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("chmod run admission lock %q: %w", path, err)
	}
	if err := lockSessionWriteFile(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire run admission lock %q: %w", path, err)
	}
	return &RunAdmissionLock{file: file, path: path}, nil
}

func (l *RunAdmissionLock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	return errors.Join(unlockSessionWriteFile(l.file), l.file.Close())
}

// ClaimRun atomically claims runID for one session and normalized input. The
// claim is the admission boundary: once its insert commits, a retry must not
// start model work unless the same durable lifecycle row is already known to
// be the original run. A process crash between this commit and the per-session
// run transaction is therefore recovered as interrupted, never replayed.
func (s *V2Store) ClaimRun(ctx context.Context, sessionID, runID, inputFingerprint string, startedAt time.Time) (RunClaim, bool, error) {
	lock, err := s.AcquireRunAdmissionLock(ctx, runID)
	if err != nil {
		return RunClaim{}, false, err
	}
	defer func() { _ = lock.Release() }()
	return s.ClaimRunWhileLocked(ctx, sessionID, runID, inputFingerprint, startedAt)
}

// ClaimRunWhileLocked is the claim half of the admission transaction. The
// caller must hold AcquireRunAdmissionLock for runID through the subsequent
// session run-row commit.
func (s *V2Store) ClaimRunWhileLocked(ctx context.Context, sessionID, runID, inputFingerprint string, startedAt time.Time) (RunClaim, bool, error) {
	if err := s.requireRoot(); err != nil {
		return RunClaim{}, false, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return RunClaim{}, false, err
	}
	if err := ValidateRunID(runID); err != nil {
		return RunClaim{}, false, err
	}
	inputFingerprint = strings.TrimSpace(inputFingerprint)
	if inputFingerprint == "" || len(inputFingerprint) > 128 {
		return RunClaim{}, false, fmt.Errorf("run input fingerprint is invalid")
	}
	if startedAt.IsZero() {
		startedAt = s.now().UTC()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	db, err := s.openRunClaimsDB(true)
	if err != nil {
		return RunClaim{}, false, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return RunClaim{}, false, fmt.Errorf("begin run claim: %w", err)
	}
	defer tx.Rollback()
	var claim RunClaim
	var started, settled string
	err = tx.QueryRow(`SELECT run_id, session_id, input_fingerprint, status, started_at, settled_at FROM run_claims WHERE run_id = ?`, runID).Scan(&claim.RunID, &claim.SessionID, &claim.InputFingerprint, &claim.Status, &started, &settled)
	if err == nil {
		claim.StartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err == nil && settled != "" {
			claim.SettledAt, err = time.Parse(time.RFC3339Nano, settled)
		}
		if err != nil {
			return RunClaim{}, false, fmt.Errorf("parse run claim %q: %w", runID, err)
		}
		if err := validateRunClaimStatus(claim.Status); err != nil {
			return RunClaim{}, false, fmt.Errorf("corrupt run claim %q: %w", runID, err)
		}
		if claim.SessionID != sessionID || claim.InputFingerprint != inputFingerprint {
			return RunClaim{}, false, fmt.Errorf("%w: run %q", ErrIdempotencyConflict, runID)
		}
		return claim, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RunClaim{}, false, fmt.Errorf("read run claim %q: %w", runID, err)
	}
	started = startedAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO run_claims(run_id, session_id, input_fingerprint, status, started_at, settled_at) VALUES(?, ?, ?, ?, ?, '')`, runID, sessionID, inputFingerprint, RunStatusRunning, started); err != nil {
		return RunClaim{}, false, fmt.Errorf("insert run claim %q: %w", runID, err)
	}
	if err := tx.Commit(); err != nil {
		return RunClaim{}, false, fmt.Errorf("commit run claim %q: %w", runID, err)
	}
	return RunClaim{RunID: runID, SessionID: sessionID, InputFingerprint: inputFingerprint, Status: RunStatusRunning, StartedAt: startedAt.UTC()}, true, nil
}

// GetRunClaim reads the shared claim. ErrNotFound is used for consistency
// with session records and allows callers to distinguish an unclaimed legacy
// REST run from a claimed command run.
func (s *V2Store) GetRunClaim(runID string) (RunClaim, error) {
	if err := s.requireRoot(); err != nil {
		return RunClaim{}, err
	}
	if err := ValidateRunID(runID); err != nil {
		return RunClaim{}, err
	}
	db, err := s.openRunClaimsDB(false)
	if err != nil {
		return RunClaim{}, err
	}
	defer db.Close()
	var claim RunClaim
	var started, settled string
	if err := db.QueryRow(`SELECT run_id, session_id, input_fingerprint, status, started_at, settled_at FROM run_claims WHERE run_id = ?`, strings.TrimSpace(runID)).Scan(&claim.RunID, &claim.SessionID, &claim.InputFingerprint, &claim.Status, &started, &settled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunClaim{}, fmt.Errorf("%w: run claim %s", ErrNotFound, runID)
		}
		return RunClaim{}, err
	}
	claim.StartedAt, err = time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return RunClaim{}, err
	}
	if settled != "" {
		claim.SettledAt, err = time.Parse(time.RFC3339Nano, settled)
		if err != nil {
			return RunClaim{}, err
		}
	}
	if err := validateRunClaimStatus(claim.Status); err != nil {
		return RunClaim{}, fmt.Errorf("corrupt run claim %q: %w", runID, err)
	}
	return claim, nil
}

// SetRunClaimStatus mirrors an authoritative session run status after the
// session transaction has committed. It is intentionally idempotent, clears
// settled_at for running claims, and never permits a terminal mirror to move
// back to running. It is a no-op for legacy runs which were never
// command-claimed.
func (s *V2Store) SetRunClaimStatus(runID, status string, settledAt time.Time) error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	if err := ValidateRunID(runID); err != nil {
		return err
	}
	if err := validateRunClaimStatus(status); err != nil {
		return err
	}
	if status == RunStatusRunning {
		// A running claim has no terminal timestamp. In particular, a lookup
		// repair of an authoritative running row must not manufacture one.
		settledAt = time.Time{}
	} else if settledAt.IsZero() {
		settledAt = s.now().UTC()
	}
	if s.runClaimStatusWriter != nil {
		return s.runClaimStatusWriter(runID, status, settledAt)
	}
	return s.setRunClaimStatus(runID, status, settledAt)
}

func (s *V2Store) setRunClaimStatus(runID, status string, settledAt time.Time) error {
	if status == RunStatusRunning {
		settledAt = time.Time{}
	} else if settledAt.IsZero() {
		settledAt = s.now().UTC()
	}
	db, err := s.openRunClaimsDB(false)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	defer db.Close()
	if status == RunStatusRunning {
		// A stale running repair must never roll a terminal mirror back. The
		// condition is evaluated by SQLite, so this remains safe across
		// processes and independent V2Store instances.
		_, err = db.Exec(`UPDATE run_claims SET status = ?, settled_at = '' WHERE run_id = ? AND status = ?`, status, strings.TrimSpace(runID), RunStatusRunning)
		return err
	}
	// Terminal repair is allowed from either running or a previously terminal
	// mirror. This lets the authoritative session row repair a failed claim
	// index, while the running branch above prevents terminal -> running.
	_, err = db.Exec(`UPDATE run_claims SET status = ?, settled_at = ? WHERE run_id = ? AND status IN (?, ?, ?, ?, ?)`, status, settledAt.UTC().Format(time.RFC3339Nano), strings.TrimSpace(runID), RunStatusRunning, RunStatusCommitted, RunStatusFailed, RunStatusInterrupted, RunStatusCancelled)
	return err
}

func validateRunClaimStatus(status string) error {
	switch status {
	case RunStatusRunning, RunStatusCommitted, RunStatusFailed, RunStatusInterrupted, RunStatusCancelled:
		return nil
	default:
		return fmt.Errorf("%w %q", ErrInvalidRunClaimStatus, status)
	}
}

// reconcileRunClaimsForSession is used immediately before a session directory
// is removed. Command-owned claims must not outlive their authority as
// running after deletion; a still-running row is closed as interrupted at
// this deletion boundary. Legacy rows have no fingerprint and are ignored.
func (s *V2Store) reconcileRunClaimsForSession(sessionID string) error {
	hasRuns, err := s.sessionRunsTableExists(sessionID)
	if err != nil {
		return err
	}
	if !hasRuns {
		return nil
	}
	runs, err := s.ListRuns(sessionID)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.InputFingerprint == "" || ValidateRunID(run.ID) != nil {
			continue
		}
		status := run.Status
		if status == RunStatusRunning {
			status = RunStatusInterrupted
		}
		lock, err := s.AcquireRunAdmissionLock(context.Background(), run.ID)
		if err != nil {
			return err
		}
		err = s.SetRunClaimStatus(run.ID, status, run.SettledAt)
		_ = lock.Release()
		if err != nil {
			return fmt.Errorf("reconcile run claim %q before session deletion: %w", run.ID, err)
		}
	}
	return nil
}

func (s *V2Store) sessionRunsTableExists(sessionID string) (bool, error) {
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return false, err
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA table_info(runs)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

func (s *V2Store) openRunClaimsDB(create bool) (*sql.DB, error) {
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.root, sessionClaimsDirName, runClaimsDatabaseDirectory, runClaimsDatabaseFileName)
	if !create {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: run claims", ErrNotFound)
			}
			return nil, err
		}
	} else {
		if err := ensurePrivateDirectory(filepath.Join(s.root, sessionClaimsDirName)); err != nil {
			return nil, fmt.Errorf("create session claims root for run claims: %w", err)
		}
		if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("create run claim directory: %w", err)
		}
	}
	mode := "rw"
	if create {
		mode = "rwc"
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_txlock=immediate&mode=" + mode + "&_pragma=busy_timeout%285000%29&_pragma=synchronous%28FULL%29"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open run claims database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if create {
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS run_claims (
			run_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			input_fingerprint TEXT NOT NULL,
			status TEXT NOT NULL,
			started_at TEXT NOT NULL,
			settled_at TEXT NOT NULL DEFAULT ''
		)`); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize run claims database: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("chmod run claims database %q: %w", path, err)
		}
	}
	return db, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

// ValidateRunID is the explicit stable run identity boundary. It is kept
// separate from legacy session IDs so tightening this command contract does
// not invalidate old on-disk session names.
func ValidateRunID(id string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	if len(id) > maxSessionCreateIDLength {
		return fmt.Errorf("run id is too long")
	}
	return nil
}
