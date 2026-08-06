package projects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// These errors are deliberately transport-neutral. The WebSocket command
// adapter maps them to stable codes while keeping the durable store details
// out of the wire response.
var (
	ErrIdempotencyConflict     = errors.New("project create idempotency conflict")
	ErrOperationOutcomeUnknown = errors.New("project create outcome is unknown")
	ErrOperationNotApplied     = errors.New("project create was not applied")
)

const (
	projectOperationVersion    = 1
	projectOperationPending    = "pending"
	projectOperationApplied    = "applied"
	projectOperationNotApplied = "not_applied"
)

type projectCreateClaim struct {
	Version         int    `json:"version"`
	Revision        int    `json:"revision"`
	OperationID     string `json:"operation_id"`
	Fingerprint     string `json:"fingerprint"`
	ProjectID       string `json:"project_id"`
	RootHash        string `json:"root_hash"`
	DisplayNameHash string `json:"display_name_hash"`
	State           string `json:"state"`
	Created         bool   `json:"created"`
}

// CreateIdempotent is the durable project-create primitive used by the typed
// command. The operation claim is committed before a new project write and
// the outcome is committed after it. A process that dies between those two
// writes can inspect the deterministic project row on the next attempt; it
// never blindly creates a second project or reports not_applied merely
// because the outcome write was interrupted.
//
// The claim stores only hashes of the canonical root and display name. The
// project ID is retained as the minimum recovery identity; no absolute path
// or user text is put in the operation record.
func (s *Store) CreateIdempotent(ctx context.Context, operationID, fingerprint, root, displayName string) (Project, bool, error) {
	if err := s.requireRoot(); err != nil {
		return Project{}, false, err
	}
	if err := ValidateOperationID(operationID); err != nil {
		return Project{}, false, err
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" || len(fingerprint) > 128 {
		return Project{}, false, fmt.Errorf("project create fingerprint is invalid")
	}
	canonicalRoot, err := CanonicalRoot(root)
	if err != nil {
		return Project{}, false, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	projectID := projectIDForRoot(canonicalRoot)
	operationLock, err := s.acquireProjectOperationLock(ctx, operationID)
	if err != nil {
		return Project{}, false, err
	}
	defer func() { _ = operationLock.Release() }()
	rootLock, err := s.acquireProjectCreateLock(ctx, projectID)
	if err != nil {
		return Project{}, false, err
	}
	defer func() { _ = rootLock.Release() }()

	rootHash := projectOperationHash(projectRootKey(canonicalRoot))
	displayName = strings.TrimSpace(displayName)
	displayNameHash := projectOperationHash(displayName)
	claim, err := s.readProjectCreateClaim(operationID)
	if err != nil {
		return Project{}, false, err
	}
	if claim != nil {
		claimValue := *claim
		if claimValue.OperationID != operationID || claimValue.Fingerprint != fingerprint {
			return Project{}, false, fmt.Errorf("%w: operation %q", ErrIdempotencyConflict, operationID)
		}
		if claimValue.ProjectID != projectID || claimValue.RootHash != rootHash || claimValue.DisplayNameHash != displayNameHash {
			return Project{}, false, fmt.Errorf("%w: operation %q", ErrIdempotencyConflict, operationID)
		}
		switch claimValue.State {
		case projectOperationApplied:
			return s.projectForCreateClaim(claimValue)
		case projectOperationNotApplied:
			return Project{}, false, fmt.Errorf("%w: operation %q", ErrOperationNotApplied, operationID)
		case projectOperationPending:
			// A project row with the same canonical root and normalized display
			// name is the only safe recovery evidence for a pending claim. A
			// different name means another independent create may have won the
			// root race; do not adopt it or replay the side effect.
			if existing, found, loadErr := s.loadCreateProject(canonicalRoot); loadErr != nil {
				return Project{}, false, loadErr
			} else if found {
				if existing.DisplayName != displayName {
					return Project{}, false, fmt.Errorf("%w: operation %q", ErrOperationOutcomeUnknown, operationID)
				}
				claimValue.State = projectOperationApplied
				claimValue.Revision = 2
				claimValue.Created = true
				if err := s.writeProjectCreateClaim(claimValue); err != nil {
					return Project{}, false, fmt.Errorf("%w: %v", ErrOperationOutcomeUnknown, err)
				}
				return existing, true, nil
			}
			return s.applyNewProjectCreate(ctx, claimValue, canonicalRoot, displayName)
		default:
			return Project{}, false, fmt.Errorf("%w: invalid operation state", ErrOperationOutcomeUnknown)
		}
	}

	// Check the real store before claiming an already-existing root. This
	// preserves Store.Create's existing duplicate-root behavior (return the
	// existing project without renaming it) and makes a crash before the
	// applied claim recoverable without guessing whether a new row was made.
	if existing, found, err := s.loadCreateProject(canonicalRoot); err != nil {
		return Project{}, false, err
	} else if found {
		claimValue := projectCreateClaim{
			Version: projectOperationVersion, Revision: 1, OperationID: operationID,
			Fingerprint: fingerprint, ProjectID: existing.ID, RootHash: rootHash,
			DisplayNameHash: displayNameHash, State: projectOperationApplied,
			Created: false,
		}
		if err := s.writeProjectCreateClaim(claimValue); err != nil {
			return Project{}, false, fmt.Errorf("%w: %v", ErrOperationOutcomeUnknown, err)
		}
		return existing, false, nil
	}

	claim = &projectCreateClaim{
		Version: projectOperationVersion, Revision: 1, OperationID: operationID,
		Fingerprint: fingerprint, ProjectID: projectID, RootHash: rootHash,
		DisplayNameHash: displayNameHash, State: projectOperationPending,
	}
	if err := s.writeProjectCreateClaim(*claim); err != nil {
		return Project{}, false, fmt.Errorf("%w: %v", ErrOperationOutcomeUnknown, err)
	}
	return s.applyNewProjectCreate(ctx, *claim, canonicalRoot, displayName)
}

func (s *Store) applyNewProjectCreate(ctx context.Context, claim projectCreateClaim, canonicalRoot, displayName string) (Project, bool, error) {
	if err := ctx.Err(); err != nil {
		return Project{}, false, err
	}
	project, created, err := s.createCanonical(canonicalRoot, displayName)
	if err != nil {
		if existing, found, loadErr := s.loadCreateProject(canonicalRoot); loadErr == nil && found {
			if existing.DisplayName != displayName {
				return Project{}, false, fmt.Errorf("%w: operation %q", ErrOperationOutcomeUnknown, claim.OperationID)
			}
			claim.State = projectOperationApplied
			claim.Revision = 2
			claim.ProjectID = existing.ID
			claim.Created = false
			if claimErr := s.writeProjectCreateClaim(claim); claimErr != nil {
				return Project{}, false, fmt.Errorf("%w: %v", ErrOperationOutcomeUnknown, claimErr)
			}
			return existing, false, nil
		}
		// We can only record not_applied after checking that no project row
		// became visible. If that check itself is inconclusive, the caller gets
		// outcome_unknown rather than a dangerous replay decision.
		if _, found, loadErr := s.loadCreateProject(canonicalRoot); loadErr != nil || found {
			if loadErr != nil {
				return Project{}, false, fmt.Errorf("%w: %v", ErrOperationOutcomeUnknown, loadErr)
			}
			return Project{}, false, fmt.Errorf("%w: operation %q", ErrOperationOutcomeUnknown, claim.OperationID)
		}
		claim.State = projectOperationNotApplied
		claim.Revision = 2
		if claimErr := s.writeProjectCreateClaim(claim); claimErr != nil {
			return Project{}, false, fmt.Errorf("%w: %v", ErrOperationOutcomeUnknown, claimErr)
		}
		return Project{}, false, fmt.Errorf("%w: operation %q", ErrOperationNotApplied, claim.OperationID)
	}
	claim.State = projectOperationApplied
	claim.Revision = 2
	claim.ProjectID = project.ID
	claim.Created = created
	if err := s.writeProjectCreateClaim(claim); err != nil {
		// The project row is already durable. Leave the pending claim in place
		// if replacement failed; a later retry will recover from the row.
		return project, created, fmt.Errorf("%w: %v", ErrOperationOutcomeUnknown, err)
	}
	return project, created, nil
}

func (s *Store) loadCreateProject(canonicalRoot string) (Project, bool, error) {
	project, found, err := s.findByRoot(canonicalRoot)
	return project, found, err
}

func (s *Store) projectForCreateClaim(claim projectCreateClaim) (Project, bool, error) {
	project, err := s.Load(claim.ProjectID)
	if err == nil {
		return project, claim.Created, nil
	}
	if errors.Is(err, ErrNotFound) {
		// The durable operation outcome remains stable even if a later
		// archive-first delete removed the project row.
		return Project{ID: claim.ProjectID}, claim.Created, nil
	}
	return Project{}, false, err
}

func (s *Store) readProjectCreateClaim(operationID string) (*projectCreateClaim, error) {
	data, err := os.ReadFile(s.projectOperationClaimPath(operationID))
	if errors.Is(err, os.ErrNotExist) {
		// A valid outcome without its base should not normally be possible
		// because records are append-only. Still honor it if encountered (for
		// example after external repair) rather than allowing the operation ID
		// to be claimed by a different fingerprint.
		outcomeData, outcomeErr := os.ReadFile(s.projectOperationOutcomePath(operationID))
		if errors.Is(outcomeErr, os.ErrNotExist) {
			return nil, nil
		}
		if outcomeErr != nil {
			return nil, fmt.Errorf("read project operation outcome: %w", outcomeErr)
		}
		outcome, decodeErr := decodeProjectCreateClaim(outcomeData)
		if decodeErr != nil {
			return nil, decodeErr
		}
		return &outcome, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read project operation claim: %w", err)
	}
	claim, err := decodeProjectCreateClaim(data)
	if err != nil {
		return nil, err
	}
	// The base claim is intentionally never replaced. A transition is an
	// append-only sibling record, so a failed/interrupted outcome write leaves
	// this pending claim available for crash recovery and fingerprint checks.
	outcomeData, outcomeErr := os.ReadFile(s.projectOperationOutcomePath(operationID))
	if outcomeErr == nil {
		outcome, decodeErr := decodeProjectCreateClaim(outcomeData)
		if decodeErr == nil && outcome.Revision > claim.Revision {
			if outcome.OperationID != claim.OperationID || outcome.Fingerprint != claim.Fingerprint || outcome.ProjectID != claim.ProjectID || outcome.RootHash != claim.RootHash || outcome.DisplayNameHash != claim.DisplayNameHash {
				return nil, fmt.Errorf("invalid project operation outcome")
			}
			claim = outcome
		}
	} else if !errors.Is(outcomeErr, os.ErrNotExist) {
		// A malformed/newer outcome must not erase or replace the valid base
		// claim. Keeping the base is the safe recovery choice; the project row
		// and canonical root checks below can still resolve a pending create.
		_ = outcomeErr
	}
	return &claim, nil
}

func (s *Store) writeProjectCreateClaim(claim projectCreateClaim) error {
	if err := os.MkdirAll(s.projectOperationClaimsDir(), 0o700); err != nil {
		return fmt.Errorf("create project operation claim directory: %w", err)
	}
	data, err := json.Marshal(claim)
	if err != nil {
		return fmt.Errorf("marshal project operation claim: %w", err)
	}
	data = append(data, '\n')
	basePath := s.projectOperationClaimPath(claim.OperationID)
	if _, err := os.Stat(basePath); errors.Is(err, os.ErrNotExist) {
		if _, outcomeErr := os.Stat(s.projectOperationOutcomePath(claim.OperationID)); outcomeErr == nil {
			return fmt.Errorf("project operation outcome exists without base claim")
		} else if !errors.Is(outcomeErr, os.ErrNotExist) {
			return fmt.Errorf("inspect project operation outcome: %w", outcomeErr)
		}
		return s.writeProjectOperationRecord(basePath, data)
	} else if err != nil {
		return fmt.Errorf("inspect project operation claim: %w", err)
	}
	if claim.Revision <= 1 {
		return fmt.Errorf("project operation claim already exists")
	}
	return s.writeProjectOperationRecord(s.projectOperationOutcomePath(claim.OperationID), data)
}

func (s *Store) projectOperationClaimsDir() string {
	return filepath.Join(s.root, ".project-operations", "claims")
}

func (s *Store) projectOperationLocksDir() string {
	return filepath.Join(s.root, ".project-operations", "locks")
}

func (s *Store) projectOperationClaimPath(operationID string) string {
	return filepath.Join(s.projectOperationClaimsDir(), projectOperationHash(operationID)+".json")
}

func (s *Store) projectOperationOutcomePath(operationID string) string {
	return filepath.Join(s.projectOperationClaimsDir(), projectOperationHash(operationID)+".outcome.json")
}

func decodeProjectCreateClaim(data []byte) (projectCreateClaim, error) {
	var claim projectCreateClaim
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&claim); err != nil {
		return projectCreateClaim{}, fmt.Errorf("parse project operation claim: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return projectCreateClaim{}, fmt.Errorf("parse project operation claim: trailing data")
	}
	if claim.Version != projectOperationVersion || claim.Revision < 1 || claim.OperationID == "" || claim.Fingerprint == "" || claim.ProjectID == "" || claim.RootHash == "" || claim.DisplayNameHash == "" {
		return projectCreateClaim{}, fmt.Errorf("invalid project operation claim")
	}
	return claim, nil
}

// writeProjectOperationRecord creates a new immutable record. Unlike
// writeFileAtomicPrivate it never removes an existing destination on Windows;
// rename is only attempted into a path that did not exist when the write
// started. This is used for both the base claim and its outcome sibling.
func writeProjectOperationRecord(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".project-operation-*.tmp")
	if err != nil {
		return fmt.Errorf("create project operation temporary file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod project operation temporary file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write project operation temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync project operation temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close project operation temporary file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install project operation record: %w", err)
	}
	cleanup = false
	return nil
}

func projectOperationHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
