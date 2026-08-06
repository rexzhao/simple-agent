package sessions

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	sessionClaimsDirName = ".session-claims"
	sessionClaimFileName = "claim.json"
	maxSessionClaimBytes = 16 * 1024
)

// sessionClaim is intentionally smaller than SessionV2. A deleted session's
// durable identity must remain reserved without retaining its conversation,
// provider configuration, or other private content.
type sessionClaim struct {
	Version     int       `json:"version"`
	SessionID   string    `json:"session_id"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	ProjectID   string    `json:"project_id,omitempty"`
	DeletedAt   time.Time `json:"deleted_at"`
}

func (s *V2Store) sessionClaimsDir(id string) string {
	return filepath.Join(s.root, sessionClaimsDirName, id)
}

func (s *V2Store) sessionClaimPath(id string) string {
	return filepath.Join(s.sessionClaimsDir(id), sessionClaimFileName)
}

func (s *V2Store) readSessionClaim(id string) (*sessionClaim, error) {
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	if err := validateV2SessionID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.sessionClaimPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) > maxSessionClaimBytes {
		return nil, corruptedSessionError(id, "session claim is too large")
	}
	var claim sessionClaim
	if err := json.Unmarshal(data, &claim); err != nil {
		return nil, corruptedSessionError(id, "parse session claim: %v", err)
	}
	if claim.Version != 1 || claim.SessionID != id || claim.DeletedAt.IsZero() || len(claim.Fingerprint) > 128 {
		return nil, corruptedSessionError(id, "invalid session claim")
	}
	return &claim, nil
}

// writeSessionClaim durably installs the delete tombstone. The caller owns
// the stable per-ID OS lock. A temp file, file fsync, same-directory rename,
// and directory fsync ensure a crash cannot expose the business deletion
// without its claim. Existing identical claims are safe to reuse when a
// delete is retried after a crash.
func (s *V2Store) writeSessionClaim(id, fingerprint, projectID string) error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	if err := validateV2SessionID(id); err != nil {
		return err
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if len(fingerprint) > 128 {
		return fmt.Errorf("session claim fingerprint is invalid")
	}
	if existing, err := s.readSessionClaim(id); err != nil {
		return err
	} else if existing != nil {
		if existing.Fingerprint != fingerprint || existing.ProjectID != strings.TrimSpace(projectID) {
			return fmt.Errorf("%w: session %q already has a different delete claim", ErrIdempotencyConflict, id)
		}
		return nil
	}
	claimDir := s.sessionClaimsDir(id)
	if err := os.MkdirAll(claimDir, 0o700); err != nil {
		return fmt.Errorf("create session claims directory %q: %w", claimDir, err)
	}
	claim := sessionClaim{
		Version:     1,
		SessionID:   id,
		Fingerprint: fingerprint,
		ProjectID:   strings.TrimSpace(projectID),
		DeletedAt:   s.now().UTC(),
	}
	data, err := json.Marshal(claim)
	if err != nil {
		return fmt.Errorf("marshal session claim %q: %w", id, err)
	}
	if len(data) > maxSessionClaimBytes {
		return fmt.Errorf("session claim %q is too large", id)
	}
	temp, err := os.CreateTemp(claimDir, ".claim-*")
	if err != nil {
		return fmt.Errorf("create session claim temp file %q: %w", id, err)
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	if err := temp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod session claim %q: %w", id, err)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write session claim %q: %w", id, err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync session claim %q: %w", id, err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close session claim %q: %w", id, err)
	}
	if err := os.Rename(tempPath, s.sessionClaimPath(id)); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("install session claim %q: %w", id, err)
	}
	if err := syncDirectory(claimDir); err != nil {
		return fmt.Errorf("sync session claim directory %q: %w", id, err)
	}
	// Persist the directory entries as well as the file contents. Without
	// syncing both ancestors, a crash immediately after the first claim for a
	// store could lose the newly-created claim directory even though claim.json
	// itself was synced.
	if err := syncDirectory(filepath.Join(s.root, sessionClaimsDirName)); err != nil {
		return fmt.Errorf("sync session claims root %q: %w", id, err)
	}
	if err := syncDirectory(s.root); err != nil {
		return fmt.Errorf("sync session root for claim %q: %w", id, err)
	}
	return nil
}

func syncDirectory(path string) error {
	// Windows does not support opening a directory for fsync. The file's
	// Sync plus same-directory Rename still gives the supported durability
	// boundary there; Unix-like systems additionally fsync the directory entry.
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

func (s *V2Store) loadCreateFingerprint(id string) (string, error) {
	db, err := s.openSessionDB(id, false)
	if err != nil {
		return "", err
	}
	defer db.Close()
	// Old databases are readable without this column. Delete is a writer
	// operation under the stable ID lock, so it may perform this one-time DDL
	// migration before capturing the legacy empty fingerprint.
	if err := ensureCreateFingerprintColumn(db); err != nil {
		return "", err
	}
	var fingerprint string
	if err := db.QueryRow(`SELECT create_fingerprint FROM state WHERE singleton = 1`).Scan(&fingerprint); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return "", fmt.Errorf("read session create fingerprint %q: %w", id, err)
	}
	return fingerprint, nil
}
