package sessions

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	SessionInboxStatusPending   = "pending"
	SessionInboxStatusDelivered = "delivered"
	SessionInboxStatusConsumed  = "consumed"
)

// SessionInboxDelivery is a compact durable completion notification. The
// child output is intentionally not copied here; consumers read it through
// the normal durable session inspection/history APIs.
type SessionInboxDelivery struct {
	DeliveryID      string    `json:"delivery_id"`
	ChildSessionID  string    `json:"child_session_id"`
	ChildRunID      string    `json:"child_run_id"`
	ParentSessionID string    `json:"parent_session_id"`
	Status          string    `json:"status"`
	ChildStatus     string    `json:"child_status,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	SettledAt       time.Time `json:"settled_at,omitempty"`
	DeliveredAt     time.Time `json:"delivered_at,omitempty"`
	ConsumedAt      time.Time `json:"consumed_at,omitempty"`
	Attempt         int       `json:"attempt"`
	StartedRunID    string    `json:"started_run_id,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
}

// NewSessionCompletionDeliveryID makes the inbox key stable across retries
// and process restarts without storing the child result in parent state.
func NewSessionCompletionDeliveryID(parentSessionID, childSessionID, childRunID string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(parentSessionID) + "\x00" + strings.TrimSpace(childSessionID) + "\x00" + strings.TrimSpace(childRunID)))
	return "delivery-" + hex.EncodeToString(hash[:])
}

func (s *V2Store) RegisterSessionCompletion(parentSessionID, childSessionID, childRunID, deliveryID string, createdAt time.Time) (SessionInboxDelivery, error) {
	if err := validateV2SessionID(parentSessionID); err != nil {
		return SessionInboxDelivery{}, err
	}
	if err := validateV2SessionID(childSessionID); err != nil {
		return SessionInboxDelivery{}, err
	}
	childRunID = strings.TrimSpace(childRunID)
	deliveryID = strings.TrimSpace(deliveryID)
	if childRunID == "" || deliveryID == "" {
		return SessionInboxDelivery{}, fmt.Errorf("child run id and delivery id are required")
	}
	if createdAt.IsZero() {
		createdAt = s.now().UTC()
	}
	db, err := s.openInboxDB(parentSessionID)
	if err != nil {
		return SessionInboxDelivery{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return SessionInboxDelivery{}, err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT OR IGNORE INTO session_inbox(delivery_id, child_session_id, child_run_id, parent_session_id, status, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		deliveryID, childSessionID, childRunID, parentSessionID, SessionInboxStatusPending, createdAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return SessionInboxDelivery{}, err
	}
	delivery, err := scanSessionInboxDelivery(tx.QueryRow(`SELECT delivery_id, child_session_id, child_run_id, parent_session_id, status, child_status, created_at, settled_at, delivered_at, consumed_at, attempt, started_run_id, last_error FROM session_inbox WHERE delivery_id = ?`, deliveryID))
	if err != nil {
		return SessionInboxDelivery{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionInboxDelivery{}, err
	}
	return delivery, nil
}

func (s *V2Store) ListSessionInbox(sessionID string) ([]SessionInboxDelivery, error) {
	db, err := s.openInboxDB(sessionID)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT delivery_id, child_session_id, child_run_id, parent_session_id, status, child_status, created_at, settled_at, delivered_at, consumed_at, attempt, started_run_id, last_error FROM session_inbox ORDER BY created_at, delivery_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []SessionInboxDelivery
	for rows.Next() {
		delivery, err := scanSessionInboxDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return deliveries, nil
}

func (s *V2Store) MarkSessionCompletionDelivered(parentSessionID, childSessionID, childRunID, childStatus string, settledAt time.Time) error {
	if settledAt.IsZero() {
		settledAt = s.now().UTC()
	}
	db, err := s.openInboxDB(parentSessionID)
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := db.Exec(`UPDATE session_inbox SET status = CASE WHEN status = ? THEN status ELSE ? END, child_status = ?, settled_at = ?, delivered_at = CASE WHEN delivered_at = '' THEN ? ELSE delivered_at END WHERE child_session_id = ? AND child_run_id = ? AND parent_session_id = ?`,
		SessionInboxStatusConsumed, SessionInboxStatusDelivered, strings.TrimSpace(childStatus), settledAt.UTC().Format(time.RFC3339Nano), settledAt.UTC().Format(time.RFC3339Nano), childSessionID, childRunID, parentSessionID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("session completion subscription was not found")
	}
	return nil
}

// ClaimSessionCompletionDelivery reserves a stable parent run id. The claim
// is intentionally separate from starting the run: a crash between the two
// leaves enough durable information to either observe that run or retry it.
func (s *V2Store) ClaimSessionCompletionDelivery(parentSessionID, deliveryID, startedRunID string) (bool, error) {
	startedRunID = strings.TrimSpace(startedRunID)
	if startedRunID == "" {
		return false, fmt.Errorf("started run id is required")
	}
	db, err := s.openInboxDB(parentSessionID)
	if err != nil {
		return false, err
	}
	defer db.Close()
	result, err := db.Exec(`UPDATE session_inbox SET started_run_id = ?, attempt = attempt + 1, last_error = '' WHERE delivery_id = ? AND status = ? AND started_run_id = ''`, startedRunID, deliveryID, SessionInboxStatusDelivered)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count == 1, nil
}

func (s *V2Store) ClearSessionCompletionClaim(parentSessionID, deliveryID, startedRunID, lastError string) error {
	db, err := s.openInboxDB(parentSessionID)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE session_inbox SET started_run_id = '', last_error = ? WHERE delivery_id = ? AND status = ? AND started_run_id = ?`, strings.TrimSpace(lastError), deliveryID, SessionInboxStatusDelivered, startedRunID)
	return err
}

func (s *V2Store) ConsumeSessionCompletionDelivery(parentSessionID, deliveryID, startedRunID string, consumedAt time.Time) error {
	if consumedAt.IsZero() {
		consumedAt = s.now().UTC()
	}
	db, err := s.openInboxDB(parentSessionID)
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := db.Exec(`UPDATE session_inbox SET status = ?, consumed_at = ?, last_error = '' WHERE delivery_id = ? AND status = ? AND started_run_id = ?`, SessionInboxStatusConsumed, consumedAt.UTC().Format(time.RFC3339Nano), deliveryID, SessionInboxStatusDelivered, startedRunID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("session completion delivery claim was not found")
	}
	return nil
}

func (s *V2Store) RejectSessionCompletionDelivery(parentSessionID, deliveryID, reason string, consumedAt time.Time) error {
	if consumedAt.IsZero() {
		consumedAt = s.now().UTC()
	}
	db, err := s.openInboxDB(parentSessionID)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE session_inbox SET status = ?, consumed_at = ?, last_error = ? WHERE delivery_id = ? AND status != ?`, SessionInboxStatusConsumed, consumedAt.UTC().Format(time.RFC3339Nano), strings.TrimSpace(reason), deliveryID, SessionInboxStatusConsumed)
	return err
}

func (s *V2Store) GetRun(sessionID, runID string) (RunRecord, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return RunRecord{}, fmt.Errorf("run id is required")
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return RunRecord{}, err
	}
	defer db.Close()
	var run RunRecord
	var started, settled string
	if err := db.QueryRow(`SELECT id, previous_run_id, status, input_payload, started_at, settled_at FROM runs WHERE id = ?`, runID).Scan(&run.ID, &run.PreviousRunID, &run.Status, &run.InputPayload, &started, &settled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunRecord{}, fmt.Errorf("%w: run %s", ErrNotFound, runID)
		}
		return RunRecord{}, err
	}
	run.StartedAt, err = time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return RunRecord{}, err
	}
	if settled != "" {
		run.SettledAt, err = time.Parse(time.RFC3339Nano, settled)
		if err != nil {
			return RunRecord{}, err
		}
	}
	return run, nil
}

func (s *V2Store) openInboxDB(sessionID string) (*sql.DB, error) {
	// session_inbox is part of the one supported session.db schema. Ordinary
	// reads and writes must not perform schema repair or act as an old-format
	// fallback; a missing table is a corrupt/unsupported database and should be
	// reported by the actual query.
	return s.openSessionDB(sessionID, false)
}

type sessionInboxScanner interface {
	Scan(...any) error
}

func scanSessionInboxDelivery(scanner sessionInboxScanner) (SessionInboxDelivery, error) {
	var delivery SessionInboxDelivery
	var created, settled, delivered, consumed string
	if err := scanner.Scan(&delivery.DeliveryID, &delivery.ChildSessionID, &delivery.ChildRunID, &delivery.ParentSessionID, &delivery.Status, &delivery.ChildStatus, &created, &settled, &delivered, &consumed, &delivery.Attempt, &delivery.StartedRunID, &delivery.LastError); err != nil {
		return SessionInboxDelivery{}, err
	}
	var err error
	delivery.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return SessionInboxDelivery{}, err
	}
	if settled != "" {
		delivery.SettledAt, err = time.Parse(time.RFC3339Nano, settled)
		if err != nil {
			return SessionInboxDelivery{}, err
		}
	}
	if delivered != "" {
		delivery.DeliveredAt, err = time.Parse(time.RFC3339Nano, delivered)
		if err != nil {
			return SessionInboxDelivery{}, err
		}
	}
	if consumed != "" {
		delivery.ConsumedAt, err = time.Parse(time.RFC3339Nano, consumed)
		if err != nil {
			return SessionInboxDelivery{}, err
		}
	}
	return delivery, nil
}
