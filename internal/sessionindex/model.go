package sessionindex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

// SessionStatus is the closed set of statuses exposed by session_index. The
// durable store uses a different vocabulary (committed/cancelled); mapping is
// kept here so transport and storage DTOs do not leak into the domain model.
type SessionStatus string

const (
	StatusIdle        SessionStatus = "idle"
	StatusQueued      SessionStatus = "queued"
	StatusRunning     SessionStatus = "running"
	StatusCompleted   SessionStatus = "completed"
	StatusFailed      SessionStatus = "failed"
	StatusInterrupted SessionStatus = "interrupted"
)

// SessionSummary is the complete list projection. ParentSessionID and RunID
// are represented as empty strings in the domain and as explicit JSON null in
// the wire schema when absent. They are never omitted, which keeps the schema
// stable and prevents a client from confusing "not present" with a partial
// patch.
type SessionSummary struct {
	SessionID        string        `json:"session_id"`
	ProjectID        string        `json:"project_id"`
	ParentSessionID  string        `json:"parent_session_id"`
	DisplayName      string        `json:"display_name"`
	Archived         bool          `json:"archived"`
	Status           SessionStatus `json:"status"`
	RunID            string        `json:"run_id"`
	ResourceRevision string        `json:"resource_revision"`
	UpdatedAt        time.Time     `json:"updated_at"`
	HasUnreadResult  bool          `json:"has_unread_result"`
}

// MarshalJSON is intentionally explicit rather than relying on omitempty.
// This is the one stable wire representation used by both snapshots and
// upsert operations.
func (s SessionSummary) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var parent *string
	if s.ParentSessionID != "" {
		value := s.ParentSessionID
		parent = &value
	}
	var run *string
	if s.RunID != "" {
		value := s.RunID
		run = &value
	}
	return json.Marshal(struct {
		SessionID        string        `json:"session_id"`
		ProjectID        string        `json:"project_id"`
		ParentSessionID  *string       `json:"parent_session_id"`
		DisplayName      string        `json:"display_name"`
		Archived         bool          `json:"archived"`
		Status           SessionStatus `json:"status"`
		RunID            *string       `json:"run_id"`
		ResourceRevision string        `json:"resource_revision"`
		UpdatedAt        string        `json:"updated_at"`
		HasUnreadResult  bool          `json:"has_unread_result"`
	}{
		SessionID: s.SessionID, ProjectID: s.ProjectID, ParentSessionID: parent,
		DisplayName: s.DisplayName, Archived: s.Archived, Status: s.Status,
		RunID: run, ResourceRevision: s.ResourceRevision,
		UpdatedAt:       s.UpdatedAt.UTC().Format(time.RFC3339Nano),
		HasUnreadResult: s.HasUnreadResult,
	})
}

func (s *SessionSummary) UnmarshalJSON(data []byte) error {
	if s == nil {
		return fmt.Errorf("session summary is nil")
	}
	if !isJSONObject(data) {
		return fmt.Errorf("session summary must be a JSON object")
	}
	var wire struct {
		SessionID        json.RawMessage `json:"session_id"`
		ProjectID        json.RawMessage `json:"project_id"`
		ParentSessionID  json.RawMessage `json:"parent_session_id"`
		DisplayName      json.RawMessage `json:"display_name"`
		Archived         json.RawMessage `json:"archived"`
		Status           json.RawMessage `json:"status"`
		RunID            json.RawMessage `json:"run_id"`
		ResourceRevision json.RawMessage `json:"resource_revision"`
		UpdatedAt        json.RawMessage `json:"updated_at"`
		HasUnreadResult  json.RawMessage `json:"has_unread_result"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return fmt.Errorf("decode session summary: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("decode session summary: trailing data")
	}
	decodeRequiredString := func(raw json.RawMessage, name string) (string, error) {
		if len(raw) == 0 || string(raw) == "null" {
			return "", fmt.Errorf("%s is required", name)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("%s must be a string", name)
		}
		return value, nil
	}
	decodeNullableString := func(raw json.RawMessage, name string) (string, error) {
		if len(raw) == 0 {
			return "", fmt.Errorf("%s is required (use null when empty)", name)
		}
		if string(raw) == "null" {
			return "", nil
		}
		value, err := decodeRequiredString(raw, name)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s must be null when empty", name)
		}
		return value, nil
	}
	sessionID, err := decodeRequiredString(wire.SessionID, "session_id")
	if err != nil {
		return err
	}
	projectID, err := decodeRequiredString(wire.ProjectID, "project_id")
	if err != nil {
		return err
	}
	displayName, err := decodeRequiredString(wire.DisplayName, "display_name")
	if err != nil {
		return err
	}
	statusValue, err := decodeRequiredString(wire.Status, "status")
	if err != nil {
		return err
	}
	revision, err := decodeRequiredString(wire.ResourceRevision, "resource_revision")
	if err != nil {
		return err
	}
	parent, err := decodeNullableString(wire.ParentSessionID, "parent_session_id")
	if err != nil {
		return err
	}
	runID, err := decodeNullableString(wire.RunID, "run_id")
	if err != nil {
		return err
	}
	if len(wire.UpdatedAt) == 0 || string(wire.UpdatedAt) == "null" {
		return fmt.Errorf("updated_at is required")
	}
	var timestamp string
	if err := json.Unmarshal(wire.UpdatedAt, &timestamp); err != nil || strings.TrimSpace(timestamp) == "" {
		return fmt.Errorf("updated_at must be an RFC3339 timestamp")
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil || updatedAt.IsZero() {
		return fmt.Errorf("updated_at must be an RFC3339 timestamp")
	}
	if len(wire.Archived) == 0 || len(wire.HasUnreadResult) == 0 {
		return fmt.Errorf("archived and has_unread_result are required")
	}
	var archived, unread bool
	if err := json.Unmarshal(wire.Archived, &archived); err != nil {
		return fmt.Errorf("archived must be a boolean")
	}
	if err := json.Unmarshal(wire.HasUnreadResult, &unread); err != nil {
		return fmt.Errorf("has_unread_result must be a boolean")
	}
	*s = SessionSummary{
		SessionID: sessionID, ProjectID: projectID,
		DisplayName: displayName, Archived: archived,
		Status: SessionStatus(statusValue), ResourceRevision: revision,
		UpdatedAt: updatedAt, HasUnreadResult: unread,
		ParentSessionID: parent, RunID: runID,
	}
	return s.Validate()
}

func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

// Validate enforces the wire-level invariants before a summary can enter an
// operation, snapshot, or journal. Resource revisions are decimal strings
// because the protocol deliberately does not use JSON numbers for sequences.
func (s SessionSummary) Validate() error {
	if strings.TrimSpace(s.SessionID) == "" {
		return fmt.Errorf("session_id is required")
	}
	if strings.TrimSpace(s.ProjectID) == "" {
		return fmt.Errorf("project_id is required")
	}
	switch s.Status {
	case StatusIdle, StatusQueued, StatusRunning, StatusCompleted, StatusFailed, StatusInterrupted:
	default:
		return fmt.Errorf("invalid status %q", s.Status)
	}
	if s.Status == StatusQueued || s.Status == StatusRunning || s.Status == StatusCompleted || s.Status == StatusFailed || s.Status == StatusInterrupted {
		if strings.TrimSpace(s.RunID) == "" {
			return fmt.Errorf("run_id is required for status %q", s.Status)
		}
	}
	if s.Status == StatusIdle && strings.TrimSpace(s.RunID) != "" {
		return fmt.Errorf("run_id must be empty for idle status")
	}
	if !utf8.ValidString(s.DisplayName) {
		return fmt.Errorf("display_name must be valid UTF-8")
	}
	// Empty display names are valid because the durable SessionV2 model
	// permits them; clients sort them before named sessions.
	if strings.TrimSpace(s.ResourceRevision) == "" {
		return fmt.Errorf("resource_revision is required")
	}
	if _, err := protocol.ParseUint64Decimal(s.ResourceRevision); err != nil {
		return fmt.Errorf("resource_revision must be an unsigned decimal string: %w", err)
	}
	if s.UpdatedAt.IsZero() {
		return fmt.Errorf("updated_at is required")
	}
	return nil
}

// SessionIndexSnapshot is the typed collection snapshot. Ordering is stable:
// display name (case-sensitive), then session id. Clients must still treat
// operations as keyed updates rather than array positions.
type SessionIndexSnapshot struct {
	Sessions []SessionSummary `json:"sessions"`
}

func (s SessionIndexSnapshot) Validate() error {
	seen := make(map[string]struct{}, len(s.Sessions))
	for _, summary := range s.Sessions {
		if err := summary.Validate(); err != nil {
			return err
		}
		if _, exists := seen[summary.SessionID]; exists {
			return fmt.Errorf("duplicate session_id %q", summary.SessionID)
		}
		seen[summary.SessionID] = struct{}{}
	}
	return nil
}

func sortSummaries(summaries []SessionSummary) {
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].DisplayName != summaries[j].DisplayName {
			return summaries[i].DisplayName < summaries[j].DisplayName
		}
		return summaries[i].SessionID < summaries[j].SessionID
	})
}

// SummaryFromSession is the only storage-to-index mapping. It deliberately
// consumes the durable domain state, not an HTTP metadata DTO.
func SummaryFromSession(session sessions.SessionV2, archived bool) SessionSummary {
	status := StatusIdle
	runID := ""
	if session.RunningRunID != "" || session.CurrentRunID != "" {
		status = StatusRunning
		runID = session.RunningRunID
		if runID == "" {
			runID = session.CurrentRunID
		}
	} else {
		runID = session.LastRunID
		if runID == "" {
			runID = session.LatestRunID
		}
		switch session.LastRunStatus {
		case sessions.RunStatusCommitted:
			status = StatusCompleted
		case sessions.RunStatusFailed:
			status = StatusFailed
		case sessions.RunStatusInterrupted, sessions.RunStatusCancelled:
			status = StatusInterrupted
		case sessions.RunStatusRunning:
			status = StatusRunning
			if runID == "" {
				runID = session.LatestRunID
			}
		}
	}
	updatedAt := session.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = session.CreatedAt
	}
	return SessionSummary{
		SessionID: session.ID, ProjectID: session.ProjectID,
		ParentSessionID: session.ParentSessionID, DisplayName: session.DisplayName,
		Archived: archived, Status: status, RunID: runID,
		ResourceRevision: strconv.FormatInt(session.LastSeq, 10),
		UpdatedAt:        updatedAt, HasUnreadResult: session.HasUnreadResult,
	}
}

// Operation is typed until the final protocol adapter. Upserts always carry a
// complete summary; there is no partial-patch representation in this package.
type Operation struct {
	Op    string
	Key   string
	Value *SessionSummary
}

const (
	OperationUpsert = "upsert"
	OperationRemove = "remove"
)

func (o Operation) Validate() error {
	if o.Key == "" {
		return fmt.Errorf("operation key is required")
	}
	switch o.Op {
	case OperationUpsert:
		if o.Value == nil {
			return fmt.Errorf("upsert value is required")
		}
		if o.Value.SessionID != o.Key {
			return fmt.Errorf("upsert key does not match session_id")
		}
		return o.Value.Validate()
	case OperationRemove:
		if o.Value != nil {
			return fmt.Errorf("remove must not contain a value")
		}
		return nil
	default:
		return fmt.Errorf("invalid session index operation %q", o.Op)
	}
}

// Change is a typed provider change. It is adapted to protocol.ChangeOperation
// exactly once in ToResourceChange, at the sync engine boundary.
type Change struct {
	ResourceRevision string
	Operations       []Operation
}

func (c Change) ToResourceChange() (syncengine.ResourceChange, error) {
	if _, parseErr := protocol.ParseUint64Decimal(c.ResourceRevision); parseErr != nil {
		return syncengine.ResourceChange{}, fmt.Errorf("invalid resource revision: %w", parseErr)
	}
	if len(c.Operations) == 0 {
		return syncengine.ResourceChange{}, fmt.Errorf("change must contain an operation")
	}
	operations := make([]protocol.ChangeOperation, 0, len(c.Operations))
	for _, operation := range c.Operations {
		if err := operation.Validate(); err != nil {
			return syncengine.ResourceChange{}, err
		}
		data, marshalErr := json.Marshal(struct {
			Op    string          `json:"op"`
			Key   string          `json:"key"`
			Value *SessionSummary `json:"value,omitempty"`
		}{operation.Op, operation.Key, operation.Value})
		if marshalErr != nil {
			return syncengine.ResourceChange{}, marshalErr
		}
		operations = append(operations, protocol.ChangeOperation{Op: operation.Op, Raw: data})
	}
	return syncengine.ResourceChange{ResourceRevision: protocol.ResourceRevision(c.ResourceRevision), Operations: operations}, nil
}

// CommittedChange is the typed internal adapter emitted after a durable
// session/project mutation has committed. A missing Summary is only used by
// lifecycle adapters that must reload durable state; providers never invent a
// summary when that reload fails.
type CommittedChange struct {
	Kind      CommittedChangeKind
	ProjectID string
	SessionID string
	RunID     string
	Summary   *SessionSummary
}

type CommittedChangeKind string

const (
	CommittedSessionUpsert   CommittedChangeKind = "session.upsert"
	CommittedSessionRemove   CommittedChangeKind = "session.remove"
	CommittedRunStarted      CommittedChangeKind = "run.started"
	CommittedRunSettled      CommittedChangeKind = "run.settled"
	CommittedSessionMarkRead CommittedChangeKind = "session.mark_read"
	CommittedProjectRefresh  CommittedChangeKind = "project.refresh"
)

type ChangeSink interface {
	PublishCommitted(CommittedChange) error
}

// InvalidationSink is an optional extension of ChangeSink. Keeping it
// separate preserves older test/application adapters while making projection
// failure explicit for the C1 provider.
type InvalidationSink interface {
	InvalidateProject(projectID, reason string) error
}
