package projectindex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

// ProjectSummary is the complete, authoritative project list projection. It
// intentionally contains no UI state: selection, session counts, and other
// page-derived values belong to consumers of this resource.
type ProjectSummary struct {
	ID          string    `json:"id"`
	Root        string    `json:"root"`
	DisplayName string    `json:"display_name"`
	Archived    bool      `json:"archived"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (s ProjectSummary) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		ID          string `json:"id"`
		Root        string `json:"root"`
		DisplayName string `json:"display_name"`
		Archived    bool   `json:"archived"`
		CreatedAt   string `json:"created_at"`
		UpdatedAt   string `json:"updated_at"`
	}{
		ID: s.ID, Root: s.Root, DisplayName: s.DisplayName, Archived: s.Archived,
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: s.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}

func (s *ProjectSummary) UnmarshalJSON(data []byte) error {
	if s == nil {
		return fmt.Errorf("project summary is nil")
	}
	if !isJSONObject(data) {
		return fmt.Errorf("project summary must be a JSON object")
	}
	fields, err := decodeUniqueObject(data, "project summary", []string{"id", "root", "display_name", "archived", "created_at", "updated_at"})
	if err != nil {
		return err
	}
	stringValue := func(raw json.RawMessage, name string, allowEmpty bool) (string, error) {
		if len(raw) == 0 || string(raw) == "null" {
			return "", fmt.Errorf("%s is required", name)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || (!allowEmpty && strings.TrimSpace(value) == "") {
			return "", fmt.Errorf("%s must be a string", name)
		}
		return value, nil
	}
	id, err := stringValue(fields["id"], "id", false)
	if err != nil {
		return err
	}
	root, err := stringValue(fields["root"], "root", false)
	if err != nil {
		return err
	}
	displayName, err := stringValue(fields["display_name"], "display_name", true)
	if err != nil {
		return err
	}
	var archived bool
	if err := json.Unmarshal(fields["archived"], &archived); err != nil {
		return fmt.Errorf("archived must be a boolean")
	}
	parseTime := func(raw json.RawMessage, name string) (time.Time, error) {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
			return time.Time{}, fmt.Errorf("%s must be an RFC3339 timestamp", name)
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.IsZero() {
			return time.Time{}, fmt.Errorf("%s must be an RFC3339 timestamp", name)
		}
		return parsed, nil
	}
	createdAt, err := parseTime(fields["created_at"], "created_at")
	if err != nil {
		return err
	}
	updatedAt, err := parseTime(fields["updated_at"], "updated_at")
	if err != nil {
		return err
	}
	*s = ProjectSummary{ID: id, Root: root, DisplayName: displayName, Archived: archived, CreatedAt: createdAt, UpdatedAt: updatedAt}
	return s.Validate()
}

func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

// decodeUniqueObject is the strict object boundary for this resource. The
// standard encoding/json struct decoder rejects unknown fields but silently
// accepts a repeated known key; using tokens lets us reject both duplicate
// keys and trailing JSON while preserving each value as raw JSON for typed
// field decoding below.
func decodeUniqueObject(data []byte, name string, allowed []string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("decode %s: invalid UTF-8", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	delim, ok := first.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("decode %s: must be a JSON object", name)
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	fields := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode %s key: %w", name, err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("decode %s: object key is not a string", name)
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("decode %s: duplicate field %q", name, key)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("decode %s field %q: %w", name, key, err)
		}
		fields[key] = raw
	}
	end, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode %s object: %w", name, err)
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return nil, fmt.Errorf("decode %s object: missing close", name)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode %s: trailing data", name)
		}
		return nil, fmt.Errorf("decode %s: trailing data: %w", name, err)
	}
	for key := range fields {
		if _, ok := allowedSet[key]; !ok {
			return nil, fmt.Errorf("decode %s: unknown field %q", name, key)
		}
	}
	for _, key := range allowed {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("decode %s: missing field %q", name, key)
		}
	}
	return fields, nil
}

func (s ProjectSummary) Validate() error {
	if !validProjectID(s.ID) {
		return fmt.Errorf("id must be a canonical project id")
	}
	if strings.TrimSpace(s.Root) == "" || !utf8.ValidString(s.Root) {
		return fmt.Errorf("root must be a non-empty UTF-8 string")
	}
	if !utf8.ValidString(s.DisplayName) {
		return fmt.Errorf("display_name must be valid UTF-8")
	}
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() {
		return fmt.Errorf("created_at and updated_at are required")
	}
	return nil
}

func validProjectID(id string) bool {
	return projectstore.ValidateProjectID(id) == nil
}

// ProjectIndexSnapshot is the bounded typed collection sent over the sync
// protocol. Ordering is the same deterministic ordering used by projects.Store.
type ProjectIndexSnapshot struct {
	Projects []ProjectSummary `json:"projects"`
}

func (s ProjectIndexSnapshot) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Projects []ProjectSummary `json:"projects"`
	}{Projects: s.Projects})
}

func (s *ProjectIndexSnapshot) UnmarshalJSON(data []byte) error {
	if s == nil {
		return fmt.Errorf("project index snapshot is nil")
	}
	if !isJSONObject(data) {
		return fmt.Errorf("project index snapshot must be a JSON object")
	}
	fields, err := decodeUniqueObject(data, "project index snapshot", []string{"projects"})
	if err != nil {
		return err
	}
	if len(fields["projects"]) == 0 || string(fields["projects"]) == "null" {
		return fmt.Errorf("projects is required")
	}
	var projects []ProjectSummary
	if err := json.Unmarshal(fields["projects"], &projects); err != nil {
		return fmt.Errorf("decode projects: %w", err)
	}
	snapshot := ProjectIndexSnapshot{Projects: projects}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	*s = snapshot
	return nil
}

func (s ProjectIndexSnapshot) Validate() error {
	seen := make(map[string]struct{}, len(s.Projects))
	for _, project := range s.Projects {
		if err := project.Validate(); err != nil {
			return err
		}
		if _, exists := seen[project.ID]; exists {
			return fmt.Errorf("duplicate project id %q", project.ID)
		}
		seen[project.ID] = struct{}{}
	}
	return nil
}

func sortSummaries(summaries []ProjectSummary) {
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].CreatedAt.Equal(summaries[j].CreatedAt) {
			return summaries[i].ID < summaries[j].ID
		}
		return summaries[i].CreatedAt.Before(summaries[j].CreatedAt)
	})
}

// SummaryFromProject is the sole storage-to-resource mapping. It consumes the
// execution-owned projects.Store model rather than an HTTP DTO.
func SummaryFromProject(project projectstore.Project) ProjectSummary {
	return ProjectSummary{
		ID: project.ID, Root: project.Root, DisplayName: project.DisplayName,
		Archived: project.Archived, CreatedAt: project.CreatedAt, UpdatedAt: project.UpdatedAt,
	}
}

type Operation struct {
	Op    string
	Key   string
	Value *ProjectSummary
}

const (
	OperationUpsert = "upsert"
	OperationRemove = "remove"
)

func (o Operation) Validate() error {
	if !validProjectID(o.Key) {
		return fmt.Errorf("operation key is not a canonical project id")
	}
	switch o.Op {
	case OperationUpsert:
		if o.Value == nil {
			return fmt.Errorf("upsert value is required")
		}
		if o.Value.ID != o.Key {
			return fmt.Errorf("upsert key does not match project id")
		}
		return o.Value.Validate()
	case OperationRemove:
		if o.Value != nil {
			return fmt.Errorf("remove must not contain a value")
		}
		return nil
	default:
		return fmt.Errorf("invalid project index operation %q", o.Op)
	}
}

type Change struct {
	ResourceRevision string
	Operations       []Operation
}

func (c Change) ToResourceChange() (syncengine.ResourceChange, error) {
	if _, err := protocol.ParseUint64Decimal(c.ResourceRevision); err != nil {
		return syncengine.ResourceChange{}, fmt.Errorf("invalid resource revision: %w", err)
	}
	if len(c.Operations) == 0 {
		return syncengine.ResourceChange{}, fmt.Errorf("change must contain an operation")
	}
	operations := make([]protocol.ChangeOperation, 0, len(c.Operations))
	for _, operation := range c.Operations {
		if err := operation.Validate(); err != nil {
			return syncengine.ResourceChange{}, err
		}
		data, err := json.Marshal(struct {
			Op    string          `json:"op"`
			Key   string          `json:"key"`
			Value *ProjectSummary `json:"value,omitempty"`
		}{operation.Op, operation.Key, operation.Value})
		if err != nil {
			return syncengine.ResourceChange{}, err
		}
		operations = append(operations, protocol.ChangeOperation{Op: operation.Op, Raw: data})
	}
	return syncengine.ResourceChange{ResourceRevision: protocol.ResourceRevision(c.ResourceRevision), Operations: operations}, nil
}

type CommittedChangeKind string

const (
	CommittedProjectUpsert CommittedChangeKind = "project.upsert"
	CommittedProjectRemove CommittedChangeKind = "project.remove"
)

// CommittedChange is emitted by execution after a project-store mutation has
// committed. The provider never receives a web request or command callback.
type CommittedChange struct {
	Kind      CommittedChangeKind
	ProjectID string
	Project   *projectstore.Project
}

type ChangeSink interface {
	PublishCommitted(CommittedChange) error
}

type InvalidationSink interface {
	Invalidate(reason string) error
}
