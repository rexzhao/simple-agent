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
	"unicode/utf8"
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
	// Prompt append is deliberately smaller than the generic WebSocket frame
	// limit.  The command carries text only; binary/image input has a separate
	// contract and must not be smuggled through this operation claim.
	MaxPromptAppendContentBytes      = 64 * 1024
	PromptAppendStatusAdmitted       = "admitted"
	PromptAppendStatusApplied        = "applied"
	PromptAppendStatusNotApplied     = "not_applied"
	PromptAppendStatusOutcomeUnknown = "outcome_unknown"
)

// RunAdmissionLock is the private inode lock used by both run admission and
// prompt-append admission. Unlike the session writer lock, its inode is keyed
// only by the stable operation identity, so independent coordinators cannot
// observe a claim-only window and decide that the owner has died while it is
// still committing the associated durable boundary.
type RunAdmissionLock struct {
	file     *os.File
	path     string
	released bool
}

func (s *V2Store) AcquireRunAdmissionLock(ctx context.Context, runID string) (*RunAdmissionLock, error) {
	return s.acquireIdentityAdmissionLock(ctx, runID, ValidateRunID, "run")
}

// AcquirePromptAppendAdmissionLock serializes one stable prompt operation
// across server processes.  It intentionally uses the same private lock root
// as run admission, but the inode is keyed by operation_id rather than by the
// target run.  Two different append operations may therefore proceed in
// parallel while retries of one operation have one owner.
func (s *V2Store) AcquirePromptAppendAdmissionLock(ctx context.Context, operationID string) (*RunAdmissionLock, error) {
	return s.acquireIdentityAdmissionLock(ctx, operationID, ValidateOperationID, "prompt append")
}

func (s *V2Store) acquireIdentityAdmissionLock(ctx context.Context, identity string, validate func(string) error, kind string) (*RunAdmissionLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	identity = strings.TrimSpace(identity)
	if err := validate(identity); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(identity))
	if err := ensurePrivateDirectory(filepath.Join(s.root, sessionClaimsDirName)); err != nil {
		return nil, fmt.Errorf("create session claims root for %s lock: %w", kind, err)
	}
	lockDirectory := filepath.Join(s.root, sessionClaimsDirName, runAdmissionLockDirectory)
	if err := ensurePrivateDirectory(lockDirectory); err != nil {
		return nil, fmt.Errorf("create %s admission lock directory: %w", kind, err)
	}
	path := filepath.Join(lockDirectory, hex.EncodeToString(digest[:])+".lock")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s admission lock %q: %w", kind, path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("chmod %s admission lock %q: %w", kind, path, err)
	}
	if err := lockSessionWriteFile(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire %s admission lock %q: %w", kind, path, err)
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

// PromptAppendClaim is the durable tombstone for one run.prompt.append
// operation. The operation is admitted before the process-memory queue is
// touched. A stale admitted row is outcome_unknown because the SQLite claim
// cannot prove whether the in-memory queue mutation happened before a crash.
// The content itself is never persisted in this server-wide index; only its
// exact UTF-8 SHA-256 digest is retained.
type PromptAppendClaim struct {
	OperationID   string
	SessionID     string
	RunID         string
	ContentSHA256 string
	Status        string
	AdmittedAt    time.Time
	SettledAt     time.Time
}

// ClaimPromptAppendWhileLocked creates or resolves an operation tombstone.
// The caller must hold AcquirePromptAppendAdmissionLock for operationID for
// the complete claim -> queue append -> status transition sequence.
func (s *V2Store) ClaimPromptAppendWhileLocked(ctx context.Context, sessionID, runID, operationID, content string, admittedAt time.Time) (PromptAppendClaim, bool, error) {
	if err := s.requireRoot(); err != nil {
		return PromptAppendClaim{}, false, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return PromptAppendClaim{}, false, err
	}
	if err := ValidateRunID(runID); err != nil {
		return PromptAppendClaim{}, false, err
	}
	if err := ValidateOperationID(operationID); err != nil {
		return PromptAppendClaim{}, false, err
	}
	if strings.TrimSpace(content) == "" || len(content) > MaxPromptAppendContentBytes {
		return PromptAppendClaim{}, false, fmt.Errorf("prompt append content is invalid")
	}
	if !utf8.ValidString(content) {
		return PromptAppendClaim{}, false, fmt.Errorf("prompt append content is not valid UTF-8")
	}
	contentSHA256 := PromptAppendContentSHA256(content)
	if admittedAt.IsZero() {
		admittedAt = s.now().UTC()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	db, err := s.openRunClaimsDB(true)
	if err != nil {
		return PromptAppendClaim{}, false, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return PromptAppendClaim{}, false, fmt.Errorf("begin prompt append claim: %w", err)
	}
	defer tx.Rollback()
	var claim PromptAppendClaim
	var admitted, settled string
	err = tx.QueryRow(`SELECT operation_id, session_id, run_id, content_sha256, status, admitted_at, settled_at FROM prompt_append_claims WHERE operation_id = ?`, operationID).
		Scan(&claim.OperationID, &claim.SessionID, &claim.RunID, &claim.ContentSHA256, &claim.Status, &admitted, &settled)
	if err == nil {
		claim.AdmittedAt, err = time.Parse(time.RFC3339Nano, admitted)
		if err == nil && settled != "" {
			claim.SettledAt, err = time.Parse(time.RFC3339Nano, settled)
		}
		if err != nil {
			return PromptAppendClaim{}, false, fmt.Errorf("parse prompt append claim %q: %w", operationID, err)
		}
		if err := validatePromptAppendClaimStatus(claim.Status); err != nil {
			return PromptAppendClaim{}, false, fmt.Errorf("corrupt prompt append claim %q: %w", operationID, err)
		}
		if claim.SessionID != sessionID || claim.RunID != runID || claim.ContentSHA256 != contentSHA256 {
			return PromptAppendClaim{}, false, fmt.Errorf("%w: prompt operation %q", ErrIdempotencyConflict, operationID)
		}
		// An admitted row can only be owned by the caller while its inode lock is
		// held. Reaching this branch means that owner has released the lock, so
		// the queue side is no longer knowable. Tombstone it permanently as
		// outcome_unknown: it may already be in the queue.
		if claim.Status == PromptAppendStatusAdmitted {
			settled = s.now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.Exec(`UPDATE prompt_append_claims SET status = ?, settled_at = ? WHERE operation_id = ? AND status = ?`, PromptAppendStatusOutcomeUnknown, settled, operationID, PromptAppendStatusAdmitted); err != nil {
				return PromptAppendClaim{}, false, fmt.Errorf("resolve prompt append claim %q: %w", operationID, err)
			}
			claim.Status = PromptAppendStatusOutcomeUnknown
			claim.SettledAt, _ = time.Parse(time.RFC3339Nano, settled)
		}
		if err := tx.Commit(); err != nil {
			return PromptAppendClaim{}, false, fmt.Errorf("commit prompt append claim resolution %q: %w", operationID, err)
		}
		return claim, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PromptAppendClaim{}, false, fmt.Errorf("read prompt append claim %q: %w", operationID, err)
	}
	admitted = admittedAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO prompt_append_claims(operation_id, session_id, run_id, content_sha256, status, admitted_at, settled_at) VALUES(?, ?, ?, ?, ?, ?, '')`, operationID, sessionID, runID, contentSHA256, PromptAppendStatusAdmitted, admitted); err != nil {
		return PromptAppendClaim{}, false, fmt.Errorf("insert prompt append claim %q: %w", operationID, err)
	}
	if err := tx.Commit(); err != nil {
		return PromptAppendClaim{}, false, fmt.Errorf("commit prompt append claim %q: %w", operationID, err)
	}
	return PromptAppendClaim{OperationID: operationID, SessionID: sessionID, RunID: runID, ContentSHA256: contentSHA256, Status: PromptAppendStatusAdmitted, AdmittedAt: admittedAt.UTC()}, true, nil
}

func (s *V2Store) GetPromptAppendClaim(operationID string) (PromptAppendClaim, error) {
	if err := s.requireRoot(); err != nil {
		return PromptAppendClaim{}, err
	}
	if err := ValidateOperationID(operationID); err != nil {
		return PromptAppendClaim{}, err
	}
	db, err := s.openRunClaimsDB(true)
	if err != nil {
		return PromptAppendClaim{}, err
	}
	defer db.Close()
	var claim PromptAppendClaim
	var admitted, settled string
	err = db.QueryRow(`SELECT operation_id, session_id, run_id, content_sha256, status, admitted_at, settled_at FROM prompt_append_claims WHERE operation_id = ?`, strings.TrimSpace(operationID)).
		Scan(&claim.OperationID, &claim.SessionID, &claim.RunID, &claim.ContentSHA256, &claim.Status, &admitted, &settled)
	if errors.Is(err, sql.ErrNoRows) {
		return PromptAppendClaim{}, fmt.Errorf("%w: prompt append claim %s", ErrNotFound, operationID)
	}
	if err != nil {
		return PromptAppendClaim{}, err
	}
	claim.AdmittedAt, err = time.Parse(time.RFC3339Nano, admitted)
	if err != nil {
		return PromptAppendClaim{}, err
	}
	if settled != "" {
		claim.SettledAt, err = time.Parse(time.RFC3339Nano, settled)
		if err != nil {
			return PromptAppendClaim{}, err
		}
	}
	if err := validatePromptAppendClaimStatus(claim.Status); err != nil {
		return PromptAppendClaim{}, fmt.Errorf("corrupt prompt append claim %q: %w", operationID, err)
	}
	return claim, nil
}

// SetPromptAppendClaimStatus is idempotent. Only admitted may move to a
// terminal state; a terminal claim is never reopened by a retry.
func (s *V2Store) SetPromptAppendClaimStatus(operationID, status string, settledAt time.Time) error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	if err := ValidateOperationID(operationID); err != nil {
		return err
	}
	if err := validatePromptAppendClaimStatus(status); err != nil {
		return err
	}
	if status == PromptAppendStatusAdmitted {
		return fmt.Errorf("prompt append claim cannot be reopened")
	}
	if settledAt.IsZero() {
		settledAt = s.now().UTC()
	}
	db, err := s.openRunClaimsDB(false)
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := db.Exec(`UPDATE prompt_append_claims SET status = ?, settled_at = ? WHERE operation_id = ? AND status = ?`, status, settledAt.UTC().Format(time.RFC3339Nano), strings.TrimSpace(operationID), PromptAppendStatusAdmitted)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	claim, err := s.GetPromptAppendClaim(operationID)
	if err != nil {
		return err
	}
	if claim.Status == status {
		return nil
	}
	return fmt.Errorf("prompt append claim %q is already terminal", operationID)
}

func validatePromptAppendClaimStatus(status string) error {
	switch status {
	case PromptAppendStatusAdmitted, PromptAppendStatusApplied, PromptAppendStatusNotApplied, PromptAppendStatusOutcomeUnknown:
		return nil
	default:
		return fmt.Errorf("invalid prompt append claim status %q", status)
	}
}

// PromptAppendContentSHA256 fingerprints the exact UTF-8 bytes of content.
// SessionID and RunID remain separate durable columns and are compared as part
// of the operation identity; the prompt body itself is not retained globally.
func PromptAppendContentSHA256(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
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
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS prompt_append_claims (
			operation_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			content_sha256 TEXT NOT NULL,
			status TEXT NOT NULL,
			admitted_at TEXT NOT NULL,
			settled_at TEXT NOT NULL DEFAULT ''
		)`); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize prompt append claims database: %w", err)
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

// ValidateOperationID is the stable client-owned identity boundary for a
// durable prompt append.  Operation IDs use the same path-safe alphabet as
// run IDs because they are also used as namespaced lock identities, but they
// are a distinct semantic key and are never substituted for request_id.
func ValidateOperationID(id string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	if len(id) > maxSessionCreateIDLength {
		return fmt.Errorf("operation id is too long")
	}
	return nil
}
