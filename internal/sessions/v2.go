package sessions

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	VersionV2 = 2

	SessionCreatedByUser  = "user"
	SessionCreatedByAgent = "agent"

	ItemKindMessage        = "message"
	ItemKindCompaction     = "compaction"
	ItemKindRuntimeContext = "runtime_context"

	ItemVisibilityVisible = "visible"
	ItemVisibilityHidden  = "hidden"
	ItemVisibilityDebug   = "debug"

	ItemAudienceUser     = "user"
	ItemAudienceModel    = "model"
	ItemAudienceInternal = "internal"

	RecordTypeItemAppended          = "item.appended"
	RecordTypeItemUpdated           = "item.updated"
	RecordTypeActiveHistoryReplaced = "active_history.replaced"
	RecordTypeCompactionCreated     = "compaction.created"
	RecordTypeRunStarted            = "run.started"
	RecordTypeRunSettled            = "run.settled"
	RecordTypeResultMarkedRead      = "result.marked_read"
	RecordTypeTurnRunning           = "turn.running"
	RecordTypeTurnCleared           = "turn.cleared"
	RecordTypeTurnInterrupted       = "turn.interrupted"
	RecordTypeInterruptedCleared    = "interrupted.cleared"
	RecordTypeTransactionBegin      = "transaction.begin"
	RecordTypeTransactionCommit     = "transaction.commit"
)

const (
	ItemStatusPending     = "pending"
	ItemStatusCompleted   = "completed"
	ItemStatusError       = "error"
	ItemStatusInterrupted = "interrupted"
)

const (
	RunStatusRunning     = "running"
	RunStatusCommitted   = "committed"
	RunStatusFailed      = "failed"
	RunStatusInterrupted = "interrupted"
	RunStatusCancelled   = "cancelled"

	TurnStatusRunning     = "running"
	TurnStatusCommitted   = "committed"
	TurnStatusFailed      = "failed"
	TurnStatusInterrupted = "interrupted"
)

const (
	largeContentBlobBytes     = 4 * 1024
	storedContentPreviewBytes = 240
	blobsDirName              = "blobs"
)

var (
	ErrNotFound         = errors.New("session not found")
	ErrCorruptedSession = errors.New("corrupted session")
)

const interruptedToolResultContent = "[tool execution interrupted]"

type InstructionSource struct {
	Role   model.MessageRole `json:"role"`
	Source string            `json:"source"`
	Path   string            `json:"path,omitempty"`
}

type SessionItem struct {
	ID             string         `json:"id"`
	TurnID         string         `json:"turn_id,omitempty"`
	AgentIteration int            `json:"agent_iteration,omitempty"`
	Seq            int64          `json:"seq,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	Kind           string         `json:"kind"`
	Visibility     string         `json:"visibility"`
	Audience       string         `json:"audience"`
	Status         string         `json:"status,omitempty"`
	Message        *model.Message `json:"message,omitempty"`
	Content        *StoredContent `json:"content,omitempty"`
}

// RunRecord is the durable lifecycle record for one user request. Continue
// resumes durable active history; it never treats InputPayload as a new user
// message.
type RunRecord struct {
	ID            string    `json:"id"`
	PreviousRunID string    `json:"previous_run_id,omitempty"`
	Status        string    `json:"status"`
	InputPayload  []byte    `json:"input_payload,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	SettledAt     time.Time `json:"settled_at,omitempty"`
}

type TurnRecord struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Ordinal   int       `json:"ordinal"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	SettledAt time.Time `json:"settled_at,omitempty"`
}

type CompactionCheckpoint struct {
	ID                    string    `json:"id"`
	CreatedAt             time.Time `json:"created_at"`
	Reason                string    `json:"reason"`
	Phase                 string    `json:"phase"`
	Trigger               string    `json:"trigger"`
	SummaryItemID         string    `json:"summary_item_id"`
	FromItemID            string    `json:"from_item_id,omitempty"`
	ToItemID              string    `json:"to_item_id,omitempty"`
	PreviousActiveHistory []string  `json:"previous_active_history,omitempty"`
	ReplacementHistory    []string  `json:"replacement_history"`
	SummaryProvider       string    `json:"summary_provider,omitempty"`
	SummaryModel          string    `json:"summary_model,omitempty"`
}

type StoredContent struct {
	Inline  string   `json:"inline,omitempty"`
	Blob    *BlobRef `json:"blob,omitempty"`
	Preview string   `json:"preview,omitempty"`
}

type BlobRef = model.BlobRef

type DebugSettings struct {
	RequestBodies bool `json:"request_bodies"`
}

type SessionV2 struct {
	ID                string    `json:"id"`
	Version           int       `json:"version"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	DisplayName       string    `json:"display_name,omitempty"`
	CreatedBy         string    `json:"created_by,omitempty"`
	ParentSessionID   string    `json:"parent_session_id,omitempty"`
	RootSessionID     string    `json:"root_session_id,omitempty"`
	SpawnDepth        int       `json:"spawn_depth,omitempty"`
	Archived          bool      `json:"archived,omitempty"`
	ArchivedAt        time.Time `json:"archived_at,omitempty"`
	LastUsedAt        time.Time `json:"last_used_at"`
	CurrentRunID      string    `json:"current_run_id,omitempty"`
	RunningRunID      string    `json:"running_run_id,omitempty"`
	RunningTurnID     string    `json:"running_turn_id,omitempty"`
	RunningStartedAt  time.Time `json:"running_started_at,omitempty"`
	InterruptedRunID  string    `json:"interrupted_run_id,omitempty"`
	InterruptedTurnID string    `json:"interrupted_turn_id,omitempty"`
	InterruptedAt     time.Time `json:"interrupted_at,omitempty"`
	LatestRunID       string    `json:"latest_run_id,omitempty"`
	LastRunID         string    `json:"last_run_id,omitempty"`
	LastRunStatus     string    `json:"last_run_status,omitempty"`
	// HasUnreadResult is an additive state-schema field. Older state_json rows
	// unmarshal it as false, so this is a backwards-compatible migration of
	// the existing compact state row rather than a second unread database.
	HasUnreadResult      bool                   `json:"has_unread_result"`
	Provider             string                 `json:"provider"`
	ModelProfile         string                 `json:"model_profile"`
	ModelID              string                 `json:"model_id"`
	Pricing              *config.ModelPricing   `json:"pricing,omitempty"`
	ReasoningLevel       string                 `json:"reasoning_level,omitempty"`
	ModelParameters      map[string]any         `json:"model_parameters,omitempty"`
	CWD                  string                 `json:"cwd"`
	ProjectID            string                 `json:"project_id,omitempty"`
	CreatedCWD           string                 `json:"created_cwd,omitempty"`
	ConfigPath           string                 `json:"config_path,omitempty"`
	ConfigDir            string                 `json:"config_dir,omitempty"`
	EnabledTools         []string               `json:"enabled_tools,omitempty"`
	EnabledMCP           []string               `json:"enabled_mcp,omitempty"`
	EnabledSkills        []string               `json:"enabled_skills,omitempty"`
	ShowReasoning        bool                   `json:"show_reasoning"`
	FullAccess           bool                   `json:"full_access,omitempty"`
	Debug                DebugSettings          `json:"debug,omitempty"`
	DebugConfigured      bool                   `json:"debug_configured,omitempty"`
	InstructionsSnapshot []model.Message        `json:"instructions_snapshot,omitempty"`
	InstructionSources   []InstructionSource    `json:"instruction_sources,omitempty"`
	Items                []SessionItem          `json:"items,omitempty"`
	ActiveHistory        []string               `json:"active_history,omitempty"`
	Compactions          []CompactionCheckpoint `json:"compactions,omitempty"`
	LastSeq              int64                  `json:"last_seq,omitempty"`
	Context              contextwindow.Metadata `json:"context,omitempty"`
	SaveToolResults      bool                   `json:"save_tool_results"`
	metadataVersion      int64
}

func (s SessionV2) RootConfigPath() string {
	if strings.TrimSpace(s.ConfigPath) != "" {
		return s.ConfigPath
	}
	if strings.TrimSpace(s.ConfigDir) != "" {
		return filepath.Join(s.ConfigDir, "sai.yaml")
	}
	return ""
}

func (s SessionV2) MaterializeActiveHistory() ([]model.Message, error) {
	return materializeActiveHistory(s, nil)
}

func MaterializeActiveHistory(session SessionV2) ([]model.Message, error) {
	return session.MaterializeActiveHistory()
}

func materializeActiveHistory(session SessionV2, readBlob func(BlobRef) ([]byte, error)) ([]model.Message, error) {
	itemsByID := make(map[string]SessionItem, len(session.Items))
	for _, item := range session.Items {
		itemsByID[item.ID] = item
	}
	messages := make([]model.Message, 0, len(session.ActiveHistory))
	for _, id := range session.ActiveHistory {
		item, ok := itemsByID[id]
		if !ok {
			return nil, corruptedSessionError(session.ID, "active history references missing item %q", id)
		}
		if item.Message == nil {
			return nil, corruptedSessionError(session.ID, "active history references item %q without a message", id)
		}
		message := copyMessage(*item.Message)
		if shouldSynthesizeInterruptedToolResult(item, message) {
			message.Content = interruptedToolResultContent
			message.IsError = true
			messages = append(messages, message)
			continue
		}
		if message.Content == "" && item.Content != nil {
			content, ok, err := materializeStoredContent(session.ID, item.ID, item.Content, readBlob)
			if err != nil {
				return nil, err
			}
			if ok {
				message.Content = content
			}
		}
		if err := materializeMessageImages(&message, readBlob); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func materializeMessageImages(message *model.Message, readBlob func(BlobRef) ([]byte, error)) error {
	if message == nil {
		return nil
	}
	if err := model.ValidateImageInputBlocks(message.ContentBlocks, true); err != nil {
		return fmt.Errorf("materialize image attachment: %w", err)
	}
	for index := range message.ContentBlocks {
		block := &message.ContentBlocks[index]
		if block.ImageBlob == nil {
			continue
		}
		if readBlob == nil {
			return fmt.Errorf("image attachment %q requires a session blob reader", block.ImageBlob.Hash)
		}
		raw, err := readBlob(*block.ImageBlob)
		if err != nil {
			return err
		}
		mediaType, supported := model.NormalizeImageMediaType(block.ImageBlob.MediaType)
		if !supported {
			return fmt.Errorf("image attachment %q has unsupported media type %q", block.ImageBlob.Hash, block.ImageBlob.MediaType)
		}
		if !model.ImageBytesMatchMediaType(mediaType, raw) {
			return fmt.Errorf("image attachment %q data does not match media type %q", block.ImageBlob.Hash, mediaType)
		}
		block.ImageURL = model.ImageDataURL(mediaType, raw)
		block.ImageBlob = nil
	}
	return nil
}

func shouldSynthesizeInterruptedToolResult(item SessionItem, message model.Message) bool {
	return message.Role == model.MessageRoleTool && (item.Status == ItemStatusPending || item.Status == ItemStatusInterrupted)
}

type V2ListOptions struct {
	Archived bool
	All      bool
}

type HistoryPageOptions struct {
	BeforeSeq   int64
	AfterSeq    int64
	Limit       int
	AlignTurn   bool
	VisibleOnly bool
}

type HistoryPage struct {
	Items         []SessionItem
	OldestSeq     int64
	NewestSeq     int64
	HasMoreBefore bool
	HasMoreAfter  bool
}

type V2Store struct {
	root string
	now  func() time.Time
}

func NewV2Store(root string) *V2Store { return newV2StoreWithClock(root, time.Now) }

func newV2StoreWithClock(root string, now func() time.Time) *V2Store {
	if now == nil {
		now = time.Now
	}
	root = strings.TrimSpace(root)
	if root != "" {
		root = filepath.Clean(root)
	}
	return &V2Store{root: root, now: now}
}

func RootForHome(home string) (string, error) {
	home = strings.TrimSpace(home)
	if home == "" {
		return "", fmt.Errorf("home directory is required")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve home directory %q: %w", home, err)
	}
	return filepath.Join(filepath.Clean(abs), "data", "sessions"), nil
}

func RootForServerRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("server root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve server root %q: %w", root, err)
	}
	return filepath.Join(filepath.Clean(abs), "data", "sessions"), nil
}

const schema = `
CREATE TABLE IF NOT EXISTS state (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  session_id TEXT NOT NULL,
  state_json BLOB NOT NULL,
  last_seq INTEGER NOT NULL DEFAULT 0,
  metadata_version INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  previous_run_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  input_payload BLOB NOT NULL DEFAULT X'',
  started_at TEXT NOT NULL,
  settled_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runs_started_at ON runs(started_at);
CREATE TABLE IF NOT EXISTS turns (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL,
  status TEXT NOT NULL,
  started_at TEXT NOT NULL,
  settled_at TEXT NOT NULL DEFAULT '',
  UNIQUE(run_id, ordinal)
);
CREATE INDEX IF NOT EXISTS turns_run_ordinal ON turns(run_id, ordinal);
CREATE TABLE IF NOT EXISTS events (
  seq INTEGER PRIMARY KEY,
  type TEXT NOT NULL,
  turn_id TEXT NOT NULL DEFAULT '',
  item_id TEXT NOT NULL DEFAULT '',
  compaction_id TEXT NOT NULL DEFAULT '',
  payload BLOB NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_type_seq ON events(type, seq);
CREATE TABLE IF NOT EXISTS items (
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL,
  turn_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  payload BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS items_seq ON items(seq);
CREATE INDEX IF NOT EXISTS items_turn_seq ON items(turn_id, seq);
CREATE TABLE IF NOT EXISTS session_inbox (
  delivery_id TEXT PRIMARY KEY,
  child_session_id TEXT NOT NULL,
  child_run_id TEXT NOT NULL,
  parent_session_id TEXT NOT NULL,
  status TEXT NOT NULL,
  child_status TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  settled_at TEXT NOT NULL DEFAULT '',
  delivered_at TEXT NOT NULL DEFAULT '',
  consumed_at TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL DEFAULT 0,
  started_run_id TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  UNIQUE(child_session_id, child_run_id, parent_session_id)
);
CREATE INDEX IF NOT EXISTS session_inbox_status_created ON session_inbox(status, created_at);
CREATE INDEX IF NOT EXISTS session_inbox_child ON session_inbox(child_session_id, child_run_id);
`

func (s *V2Store) SaveMetadata(session SessionV2) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	now := s.now().UTC()
	expectedUpdatedAt := session.UpdatedAt
	if strings.TrimSpace(session.ID) == "" {
		id, err := newSessionID(now)
		if err != nil {
			return SessionV2{}, err
		}
		session.ID = id
	}
	if err := validateV2SessionID(session.ID); err != nil {
		return SessionV2{}, err
	}
	var err error
	session, err = normalizeSessionLineage(session)
	if err != nil {
		return SessionV2{}, err
	}
	if session.Version == 0 {
		session.Version = VersionV2
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	if session.LastUsedAt.IsZero() {
		session.LastUsedAt = sessionEffectiveLastUsedAt(session)
		if session.LastUsedAt.IsZero() {
			session.LastUsedAt = now
		}
	}
	if session.Archived {
		if session.ArchivedAt.IsZero() {
			session.ArchivedAt = now
		}
	} else {
		session.ArchivedAt = time.Time{}
	}
	session.UpdatedAt = now
	session = copySessionV2(session)

	db, err := s.openSessionDB(session.ID, true)
	if err != nil {
		return SessionV2{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return SessionV2{}, fmt.Errorf("begin save session metadata: %w", err)
	}
	defer tx.Rollback()
	var existing SessionV2
	var existingJSON []byte
	var existingLastSeq, existingMetadataVersion int64
	readErr := tx.QueryRow(`SELECT state_json, last_seq, metadata_version FROM state WHERE singleton = 1`).Scan(&existingJSON, &existingLastSeq, &existingMetadataVersion)
	if readErr == nil {
		if err := json.Unmarshal(existingJSON, &existing); err != nil {
			return SessionV2{}, corruptedSessionError(session.ID, "parse SQLite state: %v", err)
		}
		if existing.ID == "" {
			existing.ID = session.ID
		}
		if existing.CreatedBy != session.CreatedBy || existing.ParentSessionID != session.ParentSessionID || existing.RootSessionID != session.RootSessionID || existing.SpawnDepth != session.SpawnDepth {
			return SessionV2{}, fmt.Errorf("session %q lineage is immutable", session.ID)
		}
		if session.LastSeq != existingLastSeq {
			return SessionV2{}, fmt.Errorf("stale session metadata: got seq %d, current seq %d", session.LastSeq, existingLastSeq)
		}
		if session.metadataVersion != existingMetadataVersion {
			return SessionV2{}, fmt.Errorf("stale session metadata version: got %d, current %d", session.metadataVersion, existingMetadataVersion)
		}
		if !expectedUpdatedAt.IsZero() && !existing.UpdatedAt.Equal(expectedUpdatedAt) {
			return SessionV2{}, fmt.Errorf("stale session metadata timestamp for %q", session.ID)
		}
		session.LastSeq = existingLastSeq
		session.metadataVersion = existingMetadataVersion + 1
		// Metadata writes do not own the projected history. Always take these
		// values from the row read in this transaction so a caller holding an
		// older SessionV2 cannot overwrite a later event projection.
		session.ActiveHistory = copyStrings(existing.ActiveHistory)
		session.Compactions = copyCompactionCheckpoints(existing.Compactions)
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return SessionV2{}, fmt.Errorf("read session state: %w", readErr)
	} else {
		// A caller may pass a pre-populated in-memory value while creating a
		// session, but sequence zero is owned by the SQLite state row.
		session.LastSeq = 0
		session.metadataVersion = 0
	}
	if readErr == nil {
		for _, mutation := range lifecycleMutationsBetween(existing, session) {
			session.LastSeq++
			payload, marshalErr := json.Marshal(mutation)
			if marshalErr != nil {
				return SessionV2{}, fmt.Errorf("marshal lifecycle metadata mutation: %w", marshalErr)
			}
			if err := insertStoreEvent(tx, session.LastSeq, mutation.Type, mutation.TurnID, "", "", payload, now); err != nil {
				return SessionV2{}, err
			}
		}
	}
	data, err := marshalState(session)
	if err != nil {
		return SessionV2{}, err
	}
	if readErr == nil {
		_, err = tx.Exec(`UPDATE state SET session_id = ?, state_json = ?, last_seq = ?, metadata_version = ? WHERE singleton = 1`, session.ID, data, session.LastSeq, session.metadataVersion)
	} else {
		_, err = tx.Exec(`INSERT INTO state(singleton, session_id, state_json, last_seq, metadata_version) VALUES(1, ?, ?, ?, ?)`, session.ID, data, session.LastSeq, session.metadataVersion)
	}
	if err != nil {
		return SessionV2{}, fmt.Errorf("write session state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SessionV2{}, fmt.Errorf("commit session state: %w", err)
	}
	return copySessionV2(session), nil
}

func (s *V2Store) LoadState(id string) (SessionV2, error) {
	return s.loadState(id)
}

func (s *V2Store) loadState(id string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	if err := validateV2SessionID(id); err != nil {
		return SessionV2{}, err
	}
	db, err := s.openSessionDB(id, false)
	if err != nil {
		return SessionV2{}, err
	}
	defer db.Close()
	var data []byte
	var lastSeq, metadataVersion int64
	if err := db.QueryRow(`SELECT state_json, last_seq, metadata_version FROM state WHERE singleton = 1`).Scan(&data, &lastSeq, &metadataVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionV2{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return SessionV2{}, fmt.Errorf("read session state %q: %w", id, err)
	}
	var session SessionV2
	if err := json.Unmarshal(data, &session); err != nil {
		return SessionV2{}, corruptedSessionError(id, "parse SQLite state: %v", err)
	}
	if session.ID == "" {
		session.ID = id
	}
	if session.ID != id {
		return SessionV2{}, fmt.Errorf("session state %q contains id %q", id, session.ID)
	}
	session.Items = nil
	session.LastSeq = lastSeq
	session.metadataVersion = metadataVersion
	return copySessionV2(session), nil
}

// ListStates reads one compact state row from every session database. It never
// opens the events table and never materializes item history.
func (s *V2Store) ListStates(options V2ListOptions) ([]SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []SessionV2{}, nil
		}
		return nil, fmt.Errorf("read session store %q: %w", s.root, err)
	}
	states := make([]SessionV2, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !isSessionDirectory(entry.Name()) {
			continue
		}
		state, err := s.LoadState(entry.Name())
		if err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorruptedSession) {
				continue
			}
			return nil, err
		}
		if !options.All && state.Archived != options.Archived {
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool {
		li, lj := sessionEffectiveLastUsedAt(states[i]), sessionEffectiveLastUsedAt(states[j])
		if !li.Equal(lj) {
			return li.After(lj)
		}
		if !states[i].CreatedAt.Equal(states[j].CreatedAt) {
			return states[i].CreatedAt.After(states[j].CreatedAt)
		}
		return states[i].ID < states[j].ID
	})
	for i := range states {
		states[i].Items = nil
	}
	return states, nil
}

// LoadExecutionState is the explicit history-loading boundary. It reads the
// compact state row and the item projection; it does not replay immutable
// events.
func (s *V2Store) LoadExecutionState(id string) (SessionV2, error) {
	state, err := s.LoadState(id)
	if err != nil {
		return SessionV2{}, err
	}
	db, err := s.openSessionDB(id, false)
	if err != nil {
		return SessionV2{}, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT payload FROM items ORDER BY seq`)
	if err != nil {
		return SessionV2{}, fmt.Errorf("read execution items %q: %w", id, err)
	}
	defer rows.Close()
	items := make([]SessionItem, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return SessionV2{}, err
		}
		var item SessionItem
		if err := json.Unmarshal(payload, &item); err != nil {
			return SessionV2{}, corruptedSessionError(id, "parse item projection: %v", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return SessionV2{}, err
	}
	state.Items = items
	return copySessionV2(state), nil
}

func (s *V2Store) Latest() (SessionV2, error) {
	states, err := s.ListStates(V2ListOptions{})
	if err != nil {
		return SessionV2{}, err
	}
	if len(states) == 0 {
		return SessionV2{}, ErrNotFound
	}
	return s.LoadExecutionState(states[0].ID)
}

func (s *V2Store) Delete(id string) error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	if err := validateV2SessionID(id); err != nil {
		return err
	}
	if _, err := os.Stat(s.sessionDir(id)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return err
	}
	if err := os.RemoveAll(s.sessionDir(id)); err != nil {
		return fmt.Errorf("delete session %q: %w", id, err)
	}
	return nil
}

// MarkRead clears the durable result badge only when runID is still the
// session's latest run. A page that was opened for an older run can therefore
// never clear the badge belonging to a newer run. Repeating the operation for
// the matching run is an idempotent no-op.
func (s *V2Store) MarkRead(sessionID, runID string) (SessionV2, bool, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return SessionV2{}, false, fmt.Errorf("run id is required")
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return SessionV2{}, false, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return SessionV2{}, false, fmt.Errorf("begin mark read: %w", err)
	}
	defer tx.Rollback()
	state, err := readStateInTx(tx, sessionID)
	if err != nil {
		return SessionV2{}, false, err
	}
	if state.LatestRunID != runID || !state.HasUnreadResult {
		return state, false, nil
	}
	state.HasUnreadResult = false
	state.UpdatedAt = s.now().UTC()
	// Mark-read is a durable metadata mutation. The lifecycle event and state
	// row are committed together, making the resulting index change
	// replayable without inventing a sequence outside the session event log.
	if err := commitLifecycleTx(tx, &state, RecordTypeResultMarkedRead, "", struct {
		RunID string `json:"run_id"`
	}{RunID: runID}); err != nil {
		return SessionV2{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return SessionV2{}, false, fmt.Errorf("commit mark read: %w", err)
	}
	return state, true, nil
}

// CreateRun records the run and its active state in the same transaction as
// the run.started event. previousRunID is resolved by the execution layer for
// normal messages and is the interrupted run for Continue.
func (s *V2Store) CreateRun(sessionID, runID, previousRunID string, inputPayload []byte, startedAt time.Time) (RunRecord, error) {
	runID = strings.TrimSpace(runID)
	previousRunID = strings.TrimSpace(previousRunID)
	if runID == "" {
		return RunRecord{}, fmt.Errorf("run id is required")
	}
	if startedAt.IsZero() {
		startedAt = s.now().UTC()
	}
	if inputPayload == nil {
		inputPayload = []byte{}
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return RunRecord{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return RunRecord{}, fmt.Errorf("begin run creation: %w", err)
	}
	defer tx.Rollback()
	state, err := readStateInTx(tx, sessionID)
	if err != nil {
		return RunRecord{}, err
	}
	if state.RunningRunID != "" || state.RunningTurnID != "" {
		return RunRecord{}, fmt.Errorf("session %q already has active run %q turn %q", sessionID, state.RunningRunID, state.RunningTurnID)
	}
	if previousRunID != "" {
		var previousStatus string
		if err := tx.QueryRow(`SELECT status FROM runs WHERE id = ?`, previousRunID).Scan(&previousStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return RunRecord{}, fmt.Errorf("previous run %q was not found", previousRunID)
			}
			return RunRecord{}, err
		}
		if previousStatus == RunStatusRunning {
			return RunRecord{}, fmt.Errorf("previous run %q is still running", previousRunID)
		}
	}
	startedAt = startedAt.UTC()
	if _, err := tx.Exec(`INSERT INTO runs(id, previous_run_id, status, input_payload, started_at, settled_at) VALUES(?, ?, ?, ?, ?, '')`, runID, previousRunID, RunStatusRunning, inputPayload, startedAt.Format(time.RFC3339Nano)); err != nil {
		return RunRecord{}, fmt.Errorf("insert run %q: %w", runID, err)
	}
	state.CurrentRunID = runID
	state.RunningRunID = runID
	state.RunningStartedAt = startedAt
	state.LatestRunID = runID
	state.LastRunStatus = RunStatusRunning
	// A newly admitted run owns the current result slot. The previous run's
	// unread marker must not survive as the marker for a different run.
	state.HasUnreadResult = false
	// A new run consumes the previous Continue opportunity. Clear its
	// marker in the same transaction as the new run row and run.started event.
	interruptedRunID := state.InterruptedRunID
	interruptedTurnID := state.InterruptedTurnID
	interruptedAt := state.InterruptedAt
	state.InterruptedRunID = ""
	state.InterruptedTurnID = ""
	state.InterruptedAt = time.Time{}
	events := []lifecycleEvent{{
		Type:    RecordTypeRunStarted,
		Payload: map[string]any{"run_id": runID, "previous_run_id": previousRunID},
	}}
	if interruptedRunID != "" || interruptedTurnID != "" || !interruptedAt.IsZero() {
		events = append([]lifecycleEvent{{
			Type:    RecordTypeInterruptedCleared,
			TurnID:  interruptedTurnID,
			Payload: map[string]any{"run_id": interruptedRunID, "turn_id": interruptedTurnID},
		}}, events...)
	}
	if err := commitLifecycleEventsTx(tx, &state, events...); err != nil {
		return RunRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunRecord{}, fmt.Errorf("commit run creation: %w", err)
	}
	return RunRecord{ID: runID, PreviousRunID: previousRunID, Status: RunStatusRunning, InputPayload: append([]byte(nil), inputPayload...), StartedAt: startedAt}, nil
}

// StartTurn creates one model request/response turn under a durable run. An
// ordinal of zero asks the store to allocate the next ordinal atomically.
func (s *V2Store) StartTurn(sessionID, runID, turnID string, ordinal int, startedAt time.Time) (TurnRecord, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(turnID) == "" {
		return TurnRecord{}, fmt.Errorf("run id and turn id are required")
	}
	if startedAt.IsZero() {
		startedAt = s.now().UTC()
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return TurnRecord{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return TurnRecord{}, err
	}
	defer tx.Rollback()
	state, err := readStateInTx(tx, sessionID)
	if err != nil {
		return TurnRecord{}, err
	}
	if state.RunningRunID != runID {
		return TurnRecord{}, fmt.Errorf("run %q is not the running run", runID)
	}
	var runStatus string
	if err := tx.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&runStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TurnRecord{}, fmt.Errorf("run %q was not found", runID)
		}
		return TurnRecord{}, err
	}
	if runStatus != RunStatusRunning {
		return TurnRecord{}, fmt.Errorf("run %q is not running", runID)
	}
	if state.RunningTurnID != "" {
		return TurnRecord{}, fmt.Errorf("run %q already has running turn %q", runID, state.RunningTurnID)
	}
	if ordinal <= 0 {
		if err := tx.QueryRow(`SELECT COALESCE(MAX(ordinal), 0) + 1 FROM turns WHERE run_id = ?`, runID).Scan(&ordinal); err != nil {
			return TurnRecord{}, err
		}
	}
	startedAt = startedAt.UTC()
	if _, err := tx.Exec(`INSERT INTO turns(id, run_id, ordinal, status, started_at, settled_at) VALUES(?, ?, ?, ?, ?, '')`, turnID, runID, ordinal, TurnStatusRunning, startedAt.Format(time.RFC3339Nano)); err != nil {
		return TurnRecord{}, fmt.Errorf("insert turn %q: %w", turnID, err)
	}
	state.RunningTurnID = turnID
	state.RunningStartedAt = startedAt
	if err := commitLifecycleTx(tx, &state, RecordTypeTurnRunning, turnID, map[string]any{
		"run_id": runID, "turn_id": turnID, "ordinal": ordinal,
	}); err != nil {
		return TurnRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return TurnRecord{}, err
	}
	return TurnRecord{ID: turnID, RunID: runID, Ordinal: ordinal, Status: TurnStatusRunning, StartedAt: startedAt}, nil
}

// The following names are intentionally lifecycle-oriented adapters for the
// projector. They return compact state just like the pre-Run store methods,
// while also updating the explicit turns table.
func (s *V2Store) MarkTurnRunningForRun(sessionID, runID, turnID string) (SessionV2, error) {
	if _, err := s.StartTurn(sessionID, runID, turnID, 0, s.now().UTC()); err != nil {
		return SessionV2{}, err
	}
	return s.LoadState(sessionID)
}

func (s *V2Store) CompleteTurnForRun(sessionID, runID, turnID string) (SessionV2, error) {
	return s.SetTurnStatus(sessionID, runID, turnID, TurnStatusCommitted, s.now().UTC())
}

func (s *V2Store) InterruptTurnForRun(sessionID, runID, turnID string) (SessionV2, error) {
	return s.SetTurnStatus(sessionID, runID, turnID, TurnStatusInterrupted, s.now().UTC())
}

func (s *V2Store) SetTurnStatus(sessionID, runID, turnID, status string, settledAt time.Time) (SessionV2, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(turnID) == "" {
		return SessionV2{}, fmt.Errorf("run id and turn id are required")
	}
	if status != TurnStatusCommitted && status != TurnStatusFailed && status != TurnStatusInterrupted {
		return SessionV2{}, fmt.Errorf("invalid turn status %q", status)
	}
	if settledAt.IsZero() {
		settledAt = s.now().UTC()
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return SessionV2{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return SessionV2{}, err
	}
	defer tx.Rollback()
	state, err := readStateInTx(tx, sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	var runStatus string
	if err := tx.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&runStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionV2{}, fmt.Errorf("run %q was not found", runID)
		}
		return SessionV2{}, err
	}
	if runStatus != RunStatusRunning {
		return SessionV2{}, fmt.Errorf("run %q is already settled as %q", runID, runStatus)
	}
	var currentStatus string
	if err := tx.QueryRow(`SELECT status FROM turns WHERE id = ? AND run_id = ?`, turnID, runID).Scan(&currentStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionV2{}, fmt.Errorf("turn %q was not found for run %q", turnID, runID)
		}
		return SessionV2{}, err
	}
	if currentStatus != TurnStatusRunning {
		return SessionV2{}, fmt.Errorf("turn %q is already settled as %q", turnID, currentStatus)
	}
	result, err := tx.Exec(`UPDATE turns SET status = ?, settled_at = ? WHERE id = ? AND run_id = ? AND status = ?`, status, settledAt.UTC().Format(time.RFC3339Nano), turnID, runID, TurnStatusRunning)
	if err != nil {
		return SessionV2{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return SessionV2{}, fmt.Errorf("turn %q was not found for run %q", turnID, runID)
	}
	if state.RunningTurnID == turnID {
		state.RunningTurnID = ""
		state.RunningStartedAt = time.Time{}
	}
	if status == TurnStatusInterrupted || status == TurnStatusFailed {
		state.InterruptedRunID = runID
		state.InterruptedTurnID = turnID
		state.InterruptedAt = settledAt.UTC()
		state.LastRunStatus = status
	}
	eventType := RecordTypeTurnCleared
	if status == TurnStatusInterrupted || status == TurnStatusFailed {
		eventType = RecordTypeTurnInterrupted
	}
	if err := commitLifecycleTx(tx, &state, eventType, turnID, map[string]any{
		"run_id": runID, "turn_id": turnID, "status": status,
	}); err != nil {
		return SessionV2{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionV2{}, err
	}
	return state, nil
}

func (s *V2Store) SetRunStatus(sessionID, runID, status string, settledAt time.Time) (SessionV2, error) {
	if strings.TrimSpace(runID) == "" {
		return SessionV2{}, fmt.Errorf("run id is required")
	}
	if status != RunStatusCommitted && status != RunStatusFailed && status != RunStatusInterrupted && status != RunStatusCancelled {
		return SessionV2{}, fmt.Errorf("invalid run status %q", status)
	}
	if settledAt.IsZero() {
		settledAt = s.now().UTC()
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return SessionV2{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return SessionV2{}, err
	}
	defer tx.Rollback()
	state, err := readStateInTx(tx, sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	var currentStatus string
	if err := tx.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&currentStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionV2{}, fmt.Errorf("run %q was not found", runID)
		}
		return SessionV2{}, err
	}
	if currentStatus != RunStatusRunning {
		return SessionV2{}, fmt.Errorf("run %q is already settled as %q", runID, currentStatus)
	}

	// Settle still-running turns in this transaction. A terminal run row and
	// a running turn row must never be observable together, including on
	// cancellation and runner errors.
	turnSettlementStatus := TurnStatusInterrupted
	if status == RunStatusFailed {
		turnSettlementStatus = TurnStatusFailed
	}
	rows, err := tx.Query(`SELECT id FROM turns WHERE run_id = ? AND status = ? ORDER BY ordinal`, runID, TurnStatusRunning)
	if err != nil {
		return SessionV2{}, err
	}
	var runningTurnIDs []string
	for rows.Next() {
		var turnID string
		if err := rows.Scan(&turnID); err != nil {
			rows.Close()
			return SessionV2{}, err
		}
		runningTurnIDs = append(runningTurnIDs, turnID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SessionV2{}, err
	}
	rows.Close()
	if status == RunStatusCommitted && len(runningTurnIDs) > 0 {
		return SessionV2{}, fmt.Errorf("run %q still has running turn %q", runID, runningTurnIDs[0])
	}
	if status == RunStatusCommitted {
		var unsettled int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM turns WHERE run_id = ? AND status != ?`, runID, TurnStatusCommitted).Scan(&unsettled); err != nil {
			return SessionV2{}, err
		}
		if unsettled > 0 {
			return SessionV2{}, fmt.Errorf("run %q has unsettled turns", runID)
		}
	}
	for _, turnID := range runningTurnIDs {
		if _, err := tx.Exec(`UPDATE turns SET status = ?, settled_at = ? WHERE id = ? AND run_id = ? AND status = ?`, turnSettlementStatus, settledAt.UTC().Format(time.RFC3339Nano), turnID, runID, TurnStatusRunning); err != nil {
			return SessionV2{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE runs SET status = ?, settled_at = ? WHERE id = ? AND status = ?`, status, settledAt.UTC().Format(time.RFC3339Nano), runID, RunStatusRunning); err != nil {
		return SessionV2{}, err
	}
	if state.RunningRunID == runID || state.CurrentRunID == runID {
		state.RunningRunID = ""
		state.CurrentRunID = ""
		state.RunningTurnID = ""
		state.RunningStartedAt = time.Time{}
	} else if state.RunningTurnID != "" {
		var turnRunID string
		if err := tx.QueryRow(`SELECT run_id FROM turns WHERE id = ?`, state.RunningTurnID).Scan(&turnRunID); err == nil && turnRunID == runID {
			state.RunningTurnID = ""
			state.RunningStartedAt = time.Time{}
		}
	}
	state.LastRunID = runID
	state.LastRunStatus = status
	// A user-cancelled run is an intentional action, not a new result to
	// inspect. Committed, failed, and interrupted outcomes remain unread.
	if state.LatestRunID == runID && status != RunStatusCancelled {
		state.HasUnreadResult = true
	}
	if status == RunStatusInterrupted || status == RunStatusFailed {
		state.InterruptedRunID = runID
		state.InterruptedAt = settledAt.UTC()
		// If the run failed before its first model request there is no valid
		// Continue target. Otherwise use the turn settled above, or the latest
		// completed turn when the error happened between requests.
		var turnID string
		if len(runningTurnIDs) > 0 {
			turnID = runningTurnIDs[len(runningTurnIDs)-1]
		} else {
			_ = tx.QueryRow(`SELECT id FROM turns WHERE run_id = ? ORDER BY ordinal DESC LIMIT 1`, runID).Scan(&turnID)
		}
		state.InterruptedTurnID = turnID
		if turnID == "" {
			state.InterruptedRunID = ""
			state.InterruptedAt = time.Time{}
		}
	}
	if status == RunStatusCommitted || status == RunStatusCancelled {
		state.InterruptedRunID = ""
		state.InterruptedTurnID = ""
		state.InterruptedAt = time.Time{}
	}
	events := make([]lifecycleEvent, 0, len(runningTurnIDs)+1)
	for _, turnID := range runningTurnIDs {
		events = append(events, lifecycleEvent{
			Type:    RecordTypeTurnInterrupted,
			TurnID:  turnID,
			Payload: map[string]any{"run_id": runID, "turn_id": turnID, "status": turnSettlementStatus},
		})
	}
	events = append(events, lifecycleEvent{
		Type:    RecordTypeRunSettled,
		Payload: map[string]any{"run_id": runID, "status": status},
	})
	if err := commitLifecycleEventsTx(tx, &state, events...); err != nil {
		return SessionV2{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionV2{}, err
	}
	return state, nil
}

type lifecycleEvent struct {
	Type    string
	TurnID  string
	Payload any
}

func commitLifecycleTx(tx *sql.Tx, state *SessionV2, eventType, turnID string, payload any) error {
	return commitLifecycleEventsTx(tx, state, lifecycleEvent{Type: eventType, TurnID: turnID, Payload: payload})
}

// commitLifecycleEventsTx writes every lifecycle event and the compact state
// row as one SQLite transaction. Callers use this for transitions that touch
// more than one durable record (for example, settling an active turn while
// settling its run), so there is no committed prefix in the event stream.
func commitLifecycleEventsTx(tx *sql.Tx, state *SessionV2, events ...lifecycleEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("at least one lifecycle event is required")
	}
	updatedAt := time.Now().UTC()
	state.UpdatedAt = updatedAt
	state.metadataVersion++
	for _, event := range events {
		if strings.TrimSpace(event.Type) == "" {
			return fmt.Errorf("lifecycle event type is required")
		}
		data, err := json.Marshal(event.Payload)
		if err != nil {
			return err
		}
		state.LastSeq++
		if err := insertStoreEvent(tx, state.LastSeq, event.Type, event.TurnID, "", "", data, updatedAt); err != nil {
			return err
		}
	}
	stateData, err := marshalState(*state)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE state SET state_json = ?, last_seq = ?, metadata_version = ? WHERE singleton = 1`, stateData, state.LastSeq, state.metadataVersion)
	return err
}

func (s *V2Store) ListRuns(sessionID string) ([]RunRecord, error) {
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, previous_run_id, status, input_payload, started_at, settled_at FROM runs ORDER BY started_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRecord
	for rows.Next() {
		var r RunRecord
		var started, settled string
		if err := rows.Scan(&r.ID, &r.PreviousRunID, &r.Status, &r.InputPayload, &started, &settled); err != nil {
			return nil, err
		}
		r.StartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return nil, err
		}
		if settled != "" {
			r.SettledAt, err = time.Parse(time.RFC3339Nano, settled)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *V2Store) ListTurns(sessionID, runID string) ([]TurnRecord, error) {
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := `SELECT id, run_id, ordinal, status, started_at, settled_at FROM turns`
	args := []any{}
	if strings.TrimSpace(runID) != "" {
		query += ` WHERE run_id = ?`
		args = append(args, runID)
	}
	query += ` ORDER BY run_id, ordinal`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TurnRecord
	for rows.Next() {
		var t TurnRecord
		var started, settled string
		if err := rows.Scan(&t.ID, &t.RunID, &t.Ordinal, &t.Status, &started, &settled); err != nil {
			return nil, err
		}
		t.StartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return nil, err
		}
		if settled != "" {
			t.SettledAt, err = time.Parse(time.RFC3339Nano, settled)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *V2Store) MarkTurnRunning(sessionID, turnID string) (SessionV2, error) {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return SessionV2{}, fmt.Errorf("running turn id is required")
	}
	return s.mutateLifecycle(sessionID, func(state *SessionV2, now time.Time) (lifecycleMutation, bool, error) {
		state.RunningTurnID = turnID
		state.RunningStartedAt = now
		return lifecycleMutation{Type: RecordTypeTurnRunning, TurnID: turnID}, true, nil
	})
}

func (s *V2Store) ClearRunningTurn(sessionID, turnID string) (SessionV2, error) {
	turnID = strings.TrimSpace(turnID)
	return s.mutateLifecycle(sessionID, func(state *SessionV2, _ time.Time) (lifecycleMutation, bool, error) {
		if state.RunningTurnID == "" || (turnID != "" && state.RunningTurnID != turnID) {
			return lifecycleMutation{}, false, nil
		}
		cleared := state.RunningTurnID
		state.RunningTurnID = ""
		state.RunningStartedAt = time.Time{}
		return lifecycleMutation{Type: RecordTypeTurnCleared, TurnID: cleared}, true, nil
	})
}

func (s *V2Store) ClearInterruptedTurn(sessionID string) (SessionV2, error) {
	return s.mutateLifecycle(sessionID, func(state *SessionV2, _ time.Time) (lifecycleMutation, bool, error) {
		if state.InterruptedTurnID == "" && state.InterruptedAt.IsZero() {
			return lifecycleMutation{}, false, nil
		}
		cleared := state.InterruptedTurnID
		state.InterruptedTurnID = ""
		state.InterruptedAt = time.Time{}
		return lifecycleMutation{Type: RecordTypeInterruptedCleared, TurnID: cleared}, true, nil
	})
}

func (s *V2Store) MarkTurnInterrupted(sessionID, turnID string) (SessionV2, error) {
	turnID = strings.TrimSpace(turnID)
	return s.mutateLifecycle(sessionID, func(state *SessionV2, now time.Time) (lifecycleMutation, bool, error) {
		running := strings.TrimSpace(state.RunningTurnID)
		if running == "" {
			running = turnID
		}
		if running == "" || (turnID != "" && state.RunningTurnID != "" && state.RunningTurnID != turnID) {
			return lifecycleMutation{}, false, nil
		}
		state.RunningTurnID = ""
		state.RunningStartedAt = time.Time{}
		state.InterruptedTurnID = running
		state.InterruptedAt = now
		return lifecycleMutation{Type: RecordTypeTurnInterrupted, TurnID: running}, true, nil
	})
}

type lifecycleMutation struct {
	Type   string
	TurnID string
}

func lifecycleMutationsBetween(before, after SessionV2) []lifecycleMutation {
	mutations := make([]lifecycleMutation, 0, 2)
	if before.RunningTurnID != after.RunningTurnID || !before.RunningStartedAt.Equal(after.RunningStartedAt) {
		if after.RunningTurnID == "" {
			mutations = append(mutations, lifecycleMutation{Type: RecordTypeTurnCleared, TurnID: before.RunningTurnID})
		} else {
			mutations = append(mutations, lifecycleMutation{Type: RecordTypeTurnRunning, TurnID: after.RunningTurnID})
		}
	}
	if before.InterruptedTurnID != after.InterruptedTurnID || !before.InterruptedAt.Equal(after.InterruptedAt) {
		if after.InterruptedTurnID == "" {
			mutations = append(mutations, lifecycleMutation{Type: RecordTypeInterruptedCleared, TurnID: before.InterruptedTurnID})
		} else {
			mutations = append(mutations, lifecycleMutation{Type: RecordTypeTurnInterrupted, TurnID: after.InterruptedTurnID})
		}
	}
	return mutations
}

// mutateLifecycle performs the read/modify/write and its immutable lifecycle
// record in one immediate SQLite transaction. In particular, recovery must
// not read a state row, yield, and then save a stale copy of that row.
func (s *V2Store) mutateLifecycle(sessionID string, mutate func(*SessionV2, time.Time) (lifecycleMutation, bool, error)) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return SessionV2{}, err
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return SessionV2{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return SessionV2{}, fmt.Errorf("begin lifecycle mutation: %w", err)
	}
	defer tx.Rollback()
	state, err := readStateInTx(tx, sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	mutation, changed, err := mutate(&state, s.now().UTC())
	if err != nil {
		return SessionV2{}, err
	}
	if !changed {
		return state, nil
	}
	if mutation.Type == "" {
		return SessionV2{}, fmt.Errorf("lifecycle mutation event type is required")
	}
	state.UpdatedAt = s.now().UTC()
	state.LastSeq++
	state.metadataVersion++
	payload, err := json.Marshal(mutation)
	if err != nil {
		return SessionV2{}, fmt.Errorf("marshal lifecycle mutation: %w", err)
	}
	if err := insertStoreEvent(tx, state.LastSeq, mutation.Type, mutation.TurnID, "", "", payload, state.UpdatedAt); err != nil {
		return SessionV2{}, err
	}
	data, err := marshalState(state)
	if err != nil {
		return SessionV2{}, err
	}
	if _, err := tx.Exec(`UPDATE state SET state_json = ?, last_seq = ?, metadata_version = ? WHERE singleton = 1`, data, state.LastSeq, state.metadataVersion); err != nil {
		return SessionV2{}, fmt.Errorf("update lifecycle state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SessionV2{}, fmt.Errorf("commit lifecycle mutation: %w", err)
	}
	return state, nil
}

func readStateInTx(tx *sql.Tx, sessionID string) (SessionV2, error) {
	var data []byte
	var lastSeq, metadataVersion int64
	if err := tx.QueryRow(`SELECT state_json, last_seq, metadata_version FROM state WHERE singleton = 1`).Scan(&data, &lastSeq, &metadataVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionV2{}, fmt.Errorf("%w: %s", ErrNotFound, sessionID)
		}
		return SessionV2{}, fmt.Errorf("read session state %q: %w", sessionID, err)
	}
	var state SessionV2
	if err := json.Unmarshal(data, &state); err != nil {
		return SessionV2{}, corruptedSessionError(sessionID, "parse SQLite state: %v", err)
	}
	if state.ID == "" {
		state.ID = sessionID
	}
	if state.ID != sessionID {
		return SessionV2{}, fmt.Errorf("session state %q contains id %q", sessionID, state.ID)
	}
	state.Items = nil
	state.LastSeq = lastSeq
	state.metadataVersion = metadataVersion
	return state, nil
}

// MarkRunningTurnsInterrupted is the startup recovery operation. It only
// scans compact state rows, so startup cost is independent of event history.
func (s *V2Store) MarkRunningTurnsInterrupted() ([]SessionV2, error) {
	states, err := s.ListStates(V2ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	marked := make([]SessionV2, 0)
	for _, state := range states {
		if strings.TrimSpace(state.RunningTurnID) == "" && strings.TrimSpace(state.RunningRunID) == "" {
			continue
		}
		updated, err := s.recoverRunningSession(state.ID)
		if err != nil {
			return nil, err
		}
		marked = append(marked, updated)
	}
	return marked, nil
}

// recoverRunningSession closes a process-dead run, its active turn, and the
// compact state row atomically. It deliberately does not reconstruct anything
// from the event log: state and the lifecycle tables are the recovery source.
func (s *V2Store) recoverRunningSession(sessionID string) (SessionV2, error) {
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return SessionV2{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return SessionV2{}, err
	}
	defer tx.Rollback()
	state, err := readStateInTx(tx, sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	now := s.now().UTC()
	runID, turnID := state.RunningRunID, state.RunningTurnID
	if runID == "" && turnID == "" {
		// The compact-state scan happens before each per-session recovery
		// transaction. A concurrent terminal transition may have won between
		// those two operations; there is then nothing left to recover.
		return state, nil
	}
	var recoveredTurnIDs []string
	if runID != "" {
		if _, err := tx.Exec(`UPDATE runs SET status = ?, settled_at = ? WHERE id = ? AND status = ?`, RunStatusInterrupted, now.Format(time.RFC3339Nano), runID, RunStatusRunning); err != nil {
			return SessionV2{}, err
		}
		rows, err := tx.Query(`SELECT id FROM turns WHERE run_id = ? AND status = ? ORDER BY ordinal`, runID, TurnStatusRunning)
		if err != nil {
			return SessionV2{}, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return SessionV2{}, err
			}
			recoveredTurnIDs = append(recoveredTurnIDs, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return SessionV2{}, err
		}
		rows.Close()
		for _, id := range recoveredTurnIDs {
			if _, err := tx.Exec(`UPDATE turns SET status = ?, settled_at = ? WHERE id = ? AND run_id = ? AND status = ?`, TurnStatusInterrupted, now.Format(time.RFC3339Nano), id, runID, RunStatusRunning); err != nil {
				return SessionV2{}, err
			}
		}
	}
	if len(recoveredTurnIDs) > 0 {
		turnID = recoveredTurnIDs[len(recoveredTurnIDs)-1]
		for _, id := range recoveredTurnIDs {
			if id == state.RunningTurnID {
				turnID = id
				break
			}
		}
	} else if runID != "" {
		// Recovery can happen between model requests. The latest completed
		// turn is then the active-history checkpoint for Continue.
		_ = tx.QueryRow(`SELECT id FROM turns WHERE run_id = ? ORDER BY ordinal DESC LIMIT 1`, runID).Scan(&turnID)
	}
	if runID != "" {
		state.InterruptedRunID = runID
		state.LastRunStatus = RunStatusInterrupted
	}
	if turnID != "" {
		state.InterruptedTurnID = turnID
		state.InterruptedAt = now
	} else {
		state.InterruptedTurnID = ""
		state.InterruptedAt = time.Time{}
		if runID == "" {
			state.InterruptedRunID = ""
		}
	}
	state.RunningRunID = ""
	state.CurrentRunID = ""
	state.RunningTurnID = ""
	state.RunningStartedAt = time.Time{}
	if runID != "" {
		state.LastRunID = runID
	}
	events := make([]lifecycleEvent, 0, len(recoveredTurnIDs)+1)
	for _, id := range recoveredTurnIDs {
		events = append(events, lifecycleEvent{
			Type:    RecordTypeTurnInterrupted,
			TurnID:  id,
			Payload: map[string]any{"run_id": runID, "turn_id": id, "status": TurnStatusInterrupted, "recovered": true},
		})
	}
	if runID == "" && turnID != "" {
		events = append(events, lifecycleEvent{
			Type:    RecordTypeTurnInterrupted,
			TurnID:  turnID,
			Payload: map[string]any{"turn_id": turnID, "status": TurnStatusInterrupted, "recovered": true},
		})
	}
	if runID != "" {
		events = append(events, lifecycleEvent{
			Type:    RecordTypeRunSettled,
			Payload: map[string]any{"run_id": runID, "turn_id": turnID, "status": RunStatusInterrupted, "recovered": true},
		})
	}
	if err := commitLifecycleEventsTx(tx, &state, events...); err != nil {
		return SessionV2{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionV2{}, err
	}
	return state, nil
}

func (s *V2Store) MaterializeActiveHistory(session SessionV2) ([]model.Message, error) {
	if len(session.Items) == 0 && len(session.ActiveHistory) > 0 {
		items, err := s.loadActiveHistoryItems(session.ID, session.ActiveHistory)
		if err != nil {
			return nil, err
		}
		session.Items = items
	}
	return materializeActiveHistory(session, func(ref BlobRef) ([]byte, error) {
		return s.ReadBlobForSession(session.ID, ref)
	})
}

// loadActiveHistoryItems is the narrow hydration path used by materialization
// when the caller has a compact state row. It intentionally does not turn a
// request for the active prompt into a full historical item scan.
func (s *V2Store) loadActiveHistoryItems(sessionID string, itemIDs []string) ([]SessionItem, error) {
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return nil, err
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	items := make([]SessionItem, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		var payload []byte
		if err := db.QueryRow(`SELECT payload FROM items WHERE id = ?`, itemID).Scan(&payload); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		var item SessionItem
		if err := json.Unmarshal(payload, &item); err != nil {
			return nil, corruptedSessionError(sessionID, "parse active item projection: %v", err)
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *V2Store) AppendItem(sessionID string, item SessionItem) (SessionItem, error) {
	if item.CreatedAt.IsZero() {
		item.CreatedAt = s.now().UTC()
	}
	if strings.TrimSpace(item.ID) == "" {
		return SessionItem{}, fmt.Errorf("session item id is required")
	}
	item, err := s.blobifySessionItemContent(sessionID, item)
	if err != nil {
		return SessionItem{}, err
	}
	state, err := s.LoadState(sessionID)
	if err != nil {
		return SessionItem{}, err
	}
	item.Seq = state.LastSeq + 1
	_, err = s.appendEvents(sessionID, state, []storeEvent{{Type: RecordTypeItemAppended, Item: &item}}, false, false)
	if err != nil {
		return SessionItem{}, err
	}
	return item, nil
}

func (s *V2Store) UpdateItem(sessionID string, item SessionItem) (SessionItem, error) {
	state, err := s.LoadExecutionState(sessionID)
	if err != nil {
		return SessionItem{}, err
	}
	updated, _, err := s.UpdateItemFromState(sessionID, state, item)
	return updated, err
}

func (s *V2Store) UpdateItemFromState(sessionID string, state SessionV2, item SessionItem) (SessionItem, SessionV2, error) {
	if err := validateCachedWriteState(sessionID, state); err != nil {
		return SessionItem{}, SessionV2{}, err
	}
	if strings.TrimSpace(item.ID) == "" {
		return SessionItem{}, SessionV2{}, fmt.Errorf("session item id is required")
	}
	existing, ok := findSessionItemByID(state.Items, item.ID)
	if !ok {
		return SessionItem{}, SessionV2{}, corruptedSessionError(sessionID, "item.updated references missing item %q", item.ID)
	}
	updated := existing
	updated.Message = copyMessagePtr(item.Message)
	updated.Content = copyStoredContent(item.Content)
	updated.Status = item.Status
	updated, err := s.blobifySessionItemContent(sessionID, updated)
	if err != nil {
		return SessionItem{}, SessionV2{}, err
	}
	next, err := s.appendEvents(sessionID, state, []storeEvent{{Type: RecordTypeItemUpdated, Item: &updated}}, false, true)
	if err != nil {
		return SessionItem{}, SessionV2{}, err
	}
	return updated, next, nil
}

func (s *V2Store) ReplaceActiveHistory(sessionID string, itemIDs []string) (int64, error) {
	state, err := s.LoadState(sessionID)
	if err != nil {
		return 0, err
	}
	next, err := s.appendEvents(sessionID, state, []storeEvent{{Type: RecordTypeActiveHistoryReplaced, ItemIDs: copyStrings(itemIDs)}}, false, false)
	if err != nil {
		return 0, err
	}
	return next.LastSeq, nil
}

func (s *V2Store) AppendCompaction(sessionID string, checkpoint CompactionCheckpoint) (CompactionCheckpoint, error) {
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = s.now().UTC()
	}
	if strings.TrimSpace(checkpoint.ID) == "" {
		return CompactionCheckpoint{}, fmt.Errorf("compaction checkpoint id is required")
	}
	state, err := s.LoadState(sessionID)
	if err != nil {
		return CompactionCheckpoint{}, err
	}
	if _, err := s.appendEvents(sessionID, state, []storeEvent{{Type: RecordTypeCompactionCreated, Compaction: &checkpoint}}, false, false); err != nil {
		return CompactionCheckpoint{}, err
	}
	return checkpoint, nil
}

func (s *V2Store) AppendCompactionCheckpoint(sessionID string, summaryItem SessionItem, checkpoint CompactionCheckpoint) (SessionV2, error) {
	state, err := s.LoadExecutionState(sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	now := s.now().UTC()
	if summaryItem.CreatedAt.IsZero() {
		summaryItem.CreatedAt = now
	}
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = now
	}
	if err := validateCompactionCheckpointWrite(summaryItem, checkpoint, state); err != nil {
		return SessionV2{}, err
	}
	summaryItem, err = s.blobifySessionItemContent(sessionID, summaryItem)
	if err != nil {
		return SessionV2{}, err
	}
	next, err := s.appendEvents(sessionID, state, []storeEvent{
		{Type: RecordTypeItemAppended, Item: &summaryItem},
		{Type: RecordTypeCompactionCreated, Compaction: &checkpoint},
		{Type: RecordTypeActiveHistoryReplaced, ItemIDs: copyStrings(checkpoint.ReplacementHistory)},
	}, true, true)
	return next, err
}

func (s *V2Store) SaveTurn(session SessionV2, items []SessionItem, activeHistory []string) (SessionV2, error) {
	return s.saveTurn(session, nil, nil, items, activeHistory)
}

func (s *V2Store) SaveCompactedTurn(session SessionV2, summaryItem SessionItem, checkpoint CompactionCheckpoint, items []SessionItem, activeHistory []string) (SessionV2, error) {
	return s.saveTurn(session, &summaryItem, &checkpoint, items, activeHistory)
}

func (s *V2Store) saveTurn(session SessionV2, summaryItem *SessionItem, checkpoint *CompactionCheckpoint, items []SessionItem, activeHistory []string) (SessionV2, error) {
	now := s.now().UTC()
	isNew := strings.TrimSpace(session.ID) == ""
	if isNew {
		id, err := newSessionID(now)
		if err != nil {
			return SessionV2{}, err
		}
		session.ID = id
	}
	if session.Version == 0 {
		session.Version = VersionV2
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	if isNew {
		session.UpdatedAt = now
	}
	session.LastUsedAt = now
	if !isNew {
		if _, err := s.LoadState(session.ID); err != nil {
			if !errors.Is(err, ErrNotFound) {
				return SessionV2{}, err
			}
			isNew = true
		}
	}
	if isNew {
		if _, err := s.SaveMetadata(session); err != nil {
			return SessionV2{}, err
		}
	} else {
		if _, err := s.SaveMetadata(session); err != nil {
			return SessionV2{}, err
		}
	}
	state, err := s.LoadExecutionState(session.ID)
	if err != nil {
		return SessionV2{}, err
	}
	if summaryItem != nil && checkpoint != nil {
		if summaryItem.CreatedAt.IsZero() {
			summaryItem.CreatedAt = now
		}
		if checkpoint.CreatedAt.IsZero() {
			checkpoint.CreatedAt = now
		}
		if err := validateCompactionCheckpointWrite(*summaryItem, *checkpoint, state); err != nil {
			return SessionV2{}, err
		}
	}
	events := make([]storeEvent, 0, len(items)+3)
	if summaryItem != nil {
		copyItem := *summaryItem
		copyItem, err = s.blobifySessionItemContent(session.ID, copyItem)
		if err != nil {
			return SessionV2{}, err
		}
		events = append(events, storeEvent{Type: RecordTypeItemAppended, Item: &copyItem})
		events = append(events, storeEvent{Type: RecordTypeCompactionCreated, Compaction: checkpoint})
	}
	for i := range items {
		item := items[i]
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		if strings.TrimSpace(item.ID) == "" {
			return SessionV2{}, fmt.Errorf("session item id is required")
		}
		item, err = s.blobifySessionItemContent(session.ID, item)
		if err != nil {
			return SessionV2{}, err
		}
		events = append(events, storeEvent{Type: RecordTypeItemAppended, Item: &item})
	}
	events = append(events, storeEvent{Type: RecordTypeActiveHistoryReplaced, ItemIDs: copyStrings(activeHistory)})
	next, err := s.appendEvents(session.ID, state, events, true, true)
	if err != nil {
		return SessionV2{}, err
	}
	return next, nil
}

func (s *V2Store) AppendItemsAndReplaceActiveHistory(sessionID string, items []SessionItem, itemIDs []string) (SessionV2, error) {
	state, err := s.LoadExecutionState(sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	return s.AppendItemsAndReplaceActiveHistoryFromState(sessionID, state, items, itemIDs)
}

func (s *V2Store) AppendItemsAndReplaceActiveHistoryFromState(sessionID string, state SessionV2, items []SessionItem, itemIDs []string) (SessionV2, error) {
	if err := validateCachedWriteState(sessionID, state); err != nil {
		return SessionV2{}, err
	}
	now := s.now().UTC()
	events := make([]storeEvent, 0, len(items)+1)
	for i := range items {
		item := items[i]
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		if strings.TrimSpace(item.ID) == "" {
			return SessionV2{}, fmt.Errorf("session item id is required")
		}
		var err error
		item, err = s.blobifySessionItemContent(sessionID, item)
		if err != nil {
			return SessionV2{}, err
		}
		events = append(events, storeEvent{Type: RecordTypeItemAppended, Item: &item})
	}
	events = append(events, storeEvent{Type: RecordTypeActiveHistoryReplaced, ItemIDs: copyStrings(itemIDs)})
	return s.appendEvents(sessionID, state, events, true, true)
}

func (s *V2Store) PersistedEventsAfter(sessionID string, afterSeq int64) ([]PersistedEvent, error) {
	if afterSeq < 0 {
		return nil, fmt.Errorf("after seq must be non-negative")
	}
	if _, err := s.LoadState(sessionID); err != nil {
		return nil, err
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT seq, type, item_id, compaction_id, payload FROM events WHERE seq > ? ORDER BY seq`, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]PersistedEvent, 0)
	for rows.Next() {
		var event PersistedEvent
		var payload []byte
		if err := rows.Scan(&event.Seq, &event.Type, &event.ItemID, &event.CompactionID, &payload); err != nil {
			return nil, err
		}
		if event.Type == RecordTypeItemAppended || event.Type == RecordTypeItemUpdated {
			var item SessionItem
			if err := json.Unmarshal(payload, &item); err != nil {
				return nil, corruptedSessionError(sessionID, "parse persisted item event %d: %v", event.Seq, err)
			}
			event.Item = &item
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

type PersistedEvent struct {
	Seq          int64
	Type         string
	ItemID       string
	CompactionID string
	// Item is the committed event payload for item records. It is kept as an
	// internal persistence representation; callers must convert it through a
	// frontend-safe DTO before putting it on a wire.
	Item *SessionItem
}

// ReadItem reads the latest committed item projection without loading the
// complete execution state. Item notifications are emitted only after the
// event transaction commits, so this is the projection source for their
// frontend DTO rather than the transient event payload.
func (s *V2Store) ReadItem(sessionID, itemID string) (SessionItem, error) {
	if strings.TrimSpace(itemID) == "" {
		return SessionItem{}, fmt.Errorf("session item id is required")
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return SessionItem{}, err
	}
	defer db.Close()
	var payload []byte
	if err := db.QueryRow(`SELECT payload FROM items WHERE id = ?`, itemID).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionItem{}, fmt.Errorf("%w: item %s", ErrNotFound, itemID)
		}
		return SessionItem{}, fmt.Errorf("read item projection %q: %w", itemID, err)
	}
	var item SessionItem
	if err := json.Unmarshal(payload, &item); err != nil {
		return SessionItem{}, corruptedSessionError(sessionID, "parse item projection %q: %v", itemID, err)
	}
	return item, nil
}

// ReadHistoryPage performs bounded SQL reads from the item projection. It is
// intentionally separate from LoadExecutionState so list/detail paths cannot
// accidentally load the complete history.
func (s *V2Store) ReadHistoryPage(sessionID string, options HistoryPageOptions) (HistoryPage, error) {
	if options.BeforeSeq < 0 || options.AfterSeq < 0 {
		return HistoryPage{}, fmt.Errorf("history cursors must be non-negative")
	}
	if options.BeforeSeq > 0 && options.AfterSeq > 0 {
		return HistoryPage{}, fmt.Errorf("before and after cursors cannot be combined")
	}
	if options.Limit <= 0 {
		options.Limit = 50
	}
	if options.Limit > 1000 {
		return HistoryPage{}, fmt.Errorf("history page limit cannot exceed 1000")
	}
	if _, err := s.LoadState(sessionID); err != nil {
		return HistoryPage{}, err
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return HistoryPage{}, err
	}
	defer db.Close()
	where := ""
	args := []any{}
	if options.BeforeSeq > 0 {
		where = "seq < ?"
		args = append(args, options.BeforeSeq)
	} else if options.AfterSeq > 0 {
		where = "seq > ?"
		args = append(args, options.AfterSeq)
	}
	if options.VisibleOnly {
		if where != "" {
			where += " AND "
		}
		where += "json_extract(payload, '$.visibility') = 'visible'"
	}
	order := "seq ASC"
	if options.AfterSeq == 0 {
		order = "seq DESC"
	}
	query := `SELECT payload FROM items`
	if where != "" {
		query += " WHERE " + where
	}
	query += " ORDER BY " + order + " LIMIT ?"
	args = append(args, options.Limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return HistoryPage{}, fmt.Errorf("read history page: %w", err)
	}
	defer rows.Close()
	items := make([]SessionItem, 0, options.Limit)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return HistoryPage{}, err
		}
		var item SessionItem
		if err := json.Unmarshal(payload, &item); err != nil {
			return HistoryPage{}, corruptedSessionError(sessionID, "parse item projection: %v", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return HistoryPage{}, err
	}
	if options.AfterSeq == 0 {
		reverseItems(items)
	}
	if options.AlignTurn && options.AfterSeq == 0 && len(items) > 0 {
		items, err = s.extendHistoryPageBack(db, sessionID, items, options)
		if err != nil {
			return HistoryPage{}, err
		}
	}
	page := HistoryPage{Items: items}
	if len(items) > 0 {
		page.OldestSeq, page.NewestSeq = items[0].Seq, items[len(items)-1].Seq
		page.HasMoreBefore, err = s.historyExists(db, "seq < ?", page.OldestSeq, options.VisibleOnly)
		if err != nil {
			return HistoryPage{}, err
		}
		page.HasMoreAfter, err = s.historyExists(db, "seq > ?", page.NewestSeq, options.VisibleOnly)
		if err != nil {
			return HistoryPage{}, err
		}
	}
	return page, nil
}

func (s *V2Store) extendHistoryPageBack(db *sql.DB, sessionID string, items []SessionItem, options HistoryPageOptions) ([]SessionItem, error) {
	if len(items) == 0 {
		return items, nil
	}
	first := items[0]
	for first.TurnID != "" {
		var payload []byte
		err := db.QueryRow(`SELECT payload FROM items WHERE seq < ?`+visibleClause(options.VisibleOnly)+` ORDER BY seq DESC LIMIT 1`, first.Seq).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			return items, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read preceding history item: %w", err)
		}
		var previous SessionItem
		if err := json.Unmarshal(payload, &previous); err != nil {
			return nil, corruptedSessionError(sessionID, "parse preceding item projection: %v", err)
		}
		if previous.TurnID != first.TurnID {
			return items, nil
		}
		items = append([]SessionItem{previous}, items...)
		first = previous
	}
	return items, nil
}

func visibleClause(visible bool) string {
	if visible {
		return " AND json_extract(payload, '$.visibility') = 'visible'"
	}
	return ""
}

func (s *V2Store) historyExists(db *sql.DB, condition string, seq int64, visible bool) (bool, error) {
	query := `SELECT 1 FROM items WHERE ` + condition
	if visible {
		query += ` AND json_extract(payload, '$.visibility') = 'visible'`
	}
	query += ` LIMIT 1`
	var one int
	err := db.QueryRow(query, seq).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check history bounds: %w", err)
	}
	return true, nil
}

func (s *V2Store) WriteBlobForSession(sessionID string, raw []byte, encoding, mediaType string) (BlobRef, error) {
	if err := s.requireRoot(); err != nil {
		return BlobRef{}, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return BlobRef{}, err
	}
	ref := BlobRef{Hash: hashBytes(raw), SizeBytes: int64(len(raw)), Encoding: encoding, MediaType: mediaType}
	path, err := s.blobPath(sessionID, ref)
	if err != nil {
		return BlobRef{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return BlobRef{}, fmt.Errorf("create blob directory: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		if err := verifyBlobFile(path, ref); err != nil {
			return BlobRef{}, err
		}
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return BlobRef{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+ref.Hash+".*.tmp")
	if err != nil {
		return BlobRef{}, err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return BlobRef{}, err
	}
	if err := tmp.Close(); err != nil {
		return BlobRef{}, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			if verifyErr := verifyBlobFile(path, ref); verifyErr != nil {
				return BlobRef{}, verifyErr
			}
			return ref, nil
		}
		return BlobRef{}, err
	}
	cleanup = false
	return ref, nil
}

func (s *V2Store) ReadBlobForSession(sessionID string, ref BlobRef) ([]byte, error) {
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	path, err := s.blobPath(sessionID, ref)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("blob %q not found", ref.Hash)
		}
		return nil, err
	}
	if err := verifyBlobBytes(raw, ref); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *V2Store) blobifySessionItemContent(sessionID string, item SessionItem) (SessionItem, error) {
	if item.Message == nil {
		return item, nil
	}
	message := copyMessage(*item.Message)
	if err := model.ValidateImageInputBlocks(message.ContentBlocks, true); err != nil {
		return SessionItem{}, fmt.Errorf("persist image attachment: %w", err)
	}
	changed := false
	if len(message.Content) > largeContentBlobBytes {
		raw := []byte(message.Content)
		ref, err := s.WriteBlobForSession(sessionID, raw, "utf-8", "text/plain")
		if err != nil {
			return SessionItem{}, err
		}
		message.Content = ""
		item.Content = &StoredContent{Blob: &ref, Preview: previewStringByBytes(string(raw), storedContentPreviewBytes)}
		changed = true
	}
	for index := range message.ContentBlocks {
		block := &message.ContentBlocks[index]
		if block.Type != "input_image" || block.ImageBlob != nil {
			continue
		}
		mediaType, raw, err := model.ParseSupportedImageDataURL(block.ImageURL)
		if err != nil {
			return SessionItem{}, fmt.Errorf("persist image attachment: %w", err)
		}
		ref, err := s.WriteBlobForSession(sessionID, raw, "binary", mediaType)
		if err != nil {
			return SessionItem{}, err
		}
		block.ImageURL = ""
		block.ImageBlob = &ref
		changed = true
	}
	if changed {
		item.Message = &message
	}
	return item, nil
}

type storeEvent struct {
	Type       string
	Item       *SessionItem
	ItemIDs    []string
	Compaction *CompactionCheckpoint
}

// appendEvents is the only item/event write path. Immutable event rows, item
// projection updates, and state.last_seq are committed in one SQLite
// transaction. Any constraint or serialization failure rolls all of them back.
// hydratedItems says whether the caller supplied the item projection in state;
// compact callers deliberately keep Items nil and must not trigger a history
// read merely to construct the return value.
func (s *V2Store) appendEvents(sessionID string, state SessionV2, events []storeEvent, wrap, hydratedItems bool) (SessionV2, error) {
	if err := validateCachedWriteState(sessionID, state); err != nil {
		return SessionV2{}, err
	}
	if len(events) == 0 {
		return state, nil
	}
	db, err := s.openSessionDB(sessionID, false)
	if err != nil {
		return SessionV2{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return SessionV2{}, err
	}
	defer tx.Rollback()
	var currentLastSeq, currentMetadataVersion int64
	if err := tx.QueryRow(`SELECT last_seq, metadata_version FROM state WHERE singleton = 1`).Scan(&currentLastSeq, &currentMetadataVersion); err != nil {
		return SessionV2{}, fmt.Errorf("read write state: %w", err)
	}
	if currentLastSeq != state.LastSeq {
		return SessionV2{}, fmt.Errorf("stale cached session state: got seq %d, current seq %d", state.LastSeq, currentLastSeq)
	}
	if currentMetadataVersion != state.metadataVersion {
		return SessionV2{}, fmt.Errorf("stale cached session state: got metadata version %d, current version %d", state.metadataVersion, currentMetadataVersion)
	}
	state.LastSeq = currentLastSeq
	state.metadataVersion = currentMetadataVersion + 1
	if hydratedItems {
		// Keep projection updates local to the returned state. The caller's
		// cached state must not be changed before the transaction commits.
		state.Items = append([]SessionItem(nil), state.Items...)
	} else {
		state.Items = nil
	}
	state.ActiveHistory = copyStrings(state.ActiveHistory)
	state.Compactions = copyCompactionCheckpoints(state.Compactions)
	txID := ""
	if wrap {
		txID = fmt.Sprintf("tx-%d", state.LastSeq+1)
		beginSeq := state.LastSeq + 1
		if err := insertStoreEvent(tx, beginSeq, RecordTypeTransactionBegin, "", "", "", []byte(txID), s.now().UTC()); err != nil {
			return SessionV2{}, err
		}
		state.LastSeq = beginSeq
	}
	for _, event := range events {
		seq := state.LastSeq + 1
		payload, itemID, compactionID, err := marshalStoreEvent(event, seq)
		if err != nil {
			return SessionV2{}, err
		}
		if err := insertStoreEvent(tx, seq, event.Type, eventTurnID(event), itemID, compactionID, payload, s.now().UTC()); err != nil {
			return SessionV2{}, err
		}
		switch event.Type {
		case RecordTypeItemAppended:
			if event.Item == nil {
				return SessionV2{}, fmt.Errorf("item.appended event has no item")
			}
			item := copySessionItem(*event.Item)
			item.Seq = seq
			itemPayload, err := json.Marshal(item)
			if err != nil {
				return SessionV2{}, fmt.Errorf("marshal item projection %q: %w", item.ID, err)
			}
			if _, err := tx.Exec(`INSERT INTO items(id, seq, turn_id, created_at, payload) VALUES(?, ?, ?, ?, ?)`, item.ID, seq, item.TurnID, item.CreatedAt.UTC().Format(time.RFC3339Nano), itemPayload); err != nil {
				return SessionV2{}, fmt.Errorf("insert item projection %q: %w", item.ID, err)
			}
			if hydratedItems {
				state.Items = append(state.Items, item)
			}
		case RecordTypeItemUpdated:
			if event.Item == nil {
				return SessionV2{}, fmt.Errorf("item.updated event has no item")
			}
			item := copySessionItem(*event.Item)
			itemPayload, err := json.Marshal(item)
			if err != nil {
				return SessionV2{}, fmt.Errorf("marshal item projection %q: %w", item.ID, err)
			}
			result, err := tx.Exec(`UPDATE items SET turn_id = ?, created_at = ?, payload = ? WHERE id = ?`, item.TurnID, item.CreatedAt.UTC().Format(time.RFC3339Nano), itemPayload, item.ID)
			if err != nil {
				return SessionV2{}, err
			}
			count, _ := result.RowsAffected()
			if count != 1 {
				return SessionV2{}, corruptedSessionError(sessionID, "item.updated references missing item %q", item.ID)
			}
			if hydratedItems {
				found := false
				for i := range state.Items {
					if state.Items[i].ID == item.ID {
						state.Items[i] = item
						found = true
						break
					}
				}
				if !found {
					return SessionV2{}, corruptedSessionError(sessionID, "item.updated references missing cached item %q", item.ID)
				}
			}
		case RecordTypeActiveHistoryReplaced:
			state.ActiveHistory = copyStrings(event.ItemIDs)
		case RecordTypeCompactionCreated:
			if event.Compaction == nil {
				return SessionV2{}, fmt.Errorf("compaction.created event has no checkpoint")
			}
			state.Compactions = append(state.Compactions, copyCompactionCheckpoint(*event.Compaction))
		default:
			return SessionV2{}, fmt.Errorf("unknown session event type %q", event.Type)
		}
		state.LastSeq = seq
	}
	if wrap {
		commitSeq := state.LastSeq + 1
		if err := insertStoreEvent(tx, commitSeq, RecordTypeTransactionCommit, "", "", "", []byte(txID), s.now().UTC()); err != nil {
			return SessionV2{}, err
		}
		state.LastSeq = commitSeq
	}
	nextState := copySessionV2(state)
	newState, err := marshalState(nextState)
	if err != nil {
		return SessionV2{}, err
	}
	if _, err := tx.Exec(`UPDATE state SET state_json = ?, last_seq = ?, metadata_version = ? WHERE singleton = 1`, newState, nextState.LastSeq, nextState.metadataVersion); err != nil {
		return SessionV2{}, fmt.Errorf("update session state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SessionV2{}, fmt.Errorf("commit session event transaction: %w", err)
	}
	return nextState, nil
}

func insertStoreEvent(tx *sql.Tx, seq int64, eventType, turnID, itemID, compactionID string, payload []byte, createdAt time.Time) error {
	if _, err := tx.Exec(`INSERT INTO events(seq, type, turn_id, item_id, compaction_id, payload, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`, seq, eventType, turnID, itemID, compactionID, payload, createdAt.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("insert session event %d: %w", seq, err)
	}
	return nil
}

func marshalStoreEvent(event storeEvent, seq int64) ([]byte, string, string, error) {
	var payload any
	var itemID, compactionID string
	switch event.Type {
	case RecordTypeItemAppended, RecordTypeItemUpdated:
		if event.Item == nil {
			return nil, "", "", fmt.Errorf("%s event has no item", event.Type)
		}
		item := copySessionItem(*event.Item)
		item.Seq = seq
		payload = item
		itemID = item.ID
	case RecordTypeActiveHistoryReplaced:
		payload = event.ItemIDs
	case RecordTypeCompactionCreated:
		if event.Compaction == nil {
			return nil, "", "", fmt.Errorf("compaction.created event has no checkpoint")
		}
		payload = event.Compaction
		compactionID = event.Compaction.ID
	default:
		return nil, "", "", fmt.Errorf("unknown session event type %q", event.Type)
	}
	data, err := json.Marshal(payload)
	return data, itemID, compactionID, err
}

func eventTurnID(event storeEvent) string {
	if event.Item != nil {
		return event.Item.TurnID
	}
	return ""
}

func (s *V2Store) openSessionDB(id string, create bool) (*sql.DB, error) {
	if err := validateV2SessionID(id); err != nil {
		return nil, err
	}
	path := filepath.Join(s.sessionDir(id), "session.db")
	databaseExists := false
	info, statErr := os.Stat(path)
	if statErr == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("session database path %q is a directory", path)
		}
		databaseExists = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if !create {
		if !databaseExists {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
	} else if err := os.MkdirAll(s.sessionDir(id), 0o755); err != nil {
		return nil, fmt.Errorf("create session directory %q: %w", id, err)
	} else if err := os.MkdirAll(filepath.Join(s.sessionDir(id), blobsDirName), 0o755); err != nil {
		return nil, fmt.Errorf("create session blob directory %q: %w", id, err)
	}
	// modernc's _txlock=immediate makes every database/sql write transaction
	// start with BEGIN IMMEDIATE instead of the deferred default. mode=rw is
	// important for read paths: if the file disappears after os.Stat, opening
	// it must not silently recreate a database.
	mode := "rw"
	if create && !databaseExists {
		mode = "rwc"
	}
	// Put connection-local pragmas in the DSN so modernc applies them while
	// opening the physical connection (busy_timeout is applied first by the
	// driver). This avoids a burst of short-lived writers racing while each
	// separately configures the same database connection.
	dsn := "file:" + filepath.ToSlash(path) + "?_txlock=immediate&mode=" + mode +
		"&_pragma=busy_timeout%285000%29&_pragma=foreign_keys%28ON%29&_pragma=synchronous%28FULL%29"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open session database %q: %w", id, err)
	}
	db.SetMaxOpenConns(1)
	// synchronous, foreign_keys and busy_timeout are connection settings. A
	// read-only open may configure them, but it must not change journal mode or
	// run schema DDL. journal_mode is set only on the create/setup path and the
	// store deliberately uses rollback journaling rather than WAL.
	if create && !databaseExists {
		if _, err := db.Exec(`PRAGMA journal_mode = DELETE`); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure session journal %q: %w", id, err)
		}
	}
	if create && !databaseExists {
		if _, err := db.Exec(schema); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize session database %q: %w", id, err)
		}
	}
	return db, nil
}

func marshalState(session SessionV2) ([]byte, error) {
	session = copySessionV2(session)
	session.Items = nil
	data, err := json.Marshal(session)
	if err != nil {
		return nil, fmt.Errorf("marshal SQLite session state %q: %w", session.ID, err)
	}
	return data, nil
}

func (s *V2Store) requireRoot() error {
	if s == nil || strings.TrimSpace(s.root) == "" || s.root == "." {
		return fmt.Errorf("session store directory is required")
	}
	return nil
}

func (s *V2Store) sessionDir(id string) string { return filepath.Join(s.root, id) }

func isSessionDirectory(name string) bool {
	return validateV2SessionID(name) == nil && !strings.EqualFold(name, blobsDirName)
}

func validateV2SessionID(id string) error {
	if err := validateSessionID(id); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimRight(id, ". "), blobsDirName) {
		return fmt.Errorf("reserved session id %q", id)
	}
	return nil
}

func validateSessionID(id string) error {
	if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) || id == "." || id == ".." {
		return fmt.Errorf("invalid session id %q", id)
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("invalid session id %q", id)
	}
	return nil
}

func validateCachedWriteState(sessionID string, state SessionV2) error {
	if err := validateV2SessionID(sessionID); err != nil {
		return err
	}
	if strings.TrimSpace(state.ID) == "" {
		return fmt.Errorf("cached session state id is required")
	}
	if state.ID != sessionID {
		return fmt.Errorf("cached session state id %q does not match session id %q", state.ID, sessionID)
	}
	return nil
}

func findSessionItemByID(items []SessionItem, id string) (SessionItem, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return SessionItem{}, false
}

func normalizeSessionLineage(session SessionV2) (SessionV2, error) {
	session.CreatedBy = strings.TrimSpace(session.CreatedBy)
	session.ParentSessionID = strings.TrimSpace(session.ParentSessionID)
	session.RootSessionID = strings.TrimSpace(session.RootSessionID)
	if session.CreatedBy == "" {
		if session.ParentSessionID == "" {
			session.CreatedBy = SessionCreatedByUser
		} else {
			session.CreatedBy = SessionCreatedByAgent
		}
	}
	if session.CreatedBy != SessionCreatedByUser && session.CreatedBy != SessionCreatedByAgent {
		return SessionV2{}, fmt.Errorf("session created_by must be %q or %q", SessionCreatedByUser, SessionCreatedByAgent)
	}
	if session.SpawnDepth < 0 {
		return SessionV2{}, fmt.Errorf("session spawn_depth must be non-negative")
	}
	if session.ParentSessionID == "" {
		session.RootSessionID = session.ID
		session.SpawnDepth = 0
		return session, nil
	}
	if err := validateV2SessionID(session.ParentSessionID); err != nil {
		return SessionV2{}, fmt.Errorf("invalid parent session id: %w", err)
	}
	if session.ParentSessionID == session.ID {
		return SessionV2{}, fmt.Errorf("session cannot be its own parent")
	}
	if session.RootSessionID == "" {
		session.RootSessionID = session.ParentSessionID
	}
	if err := validateV2SessionID(session.RootSessionID); err != nil {
		return SessionV2{}, fmt.Errorf("invalid root session id: %w", err)
	}
	if session.SpawnDepth == 0 {
		session.SpawnDepth = 1
	}
	return session, nil
}

func sessionEffectiveLastUsedAt(session SessionV2) time.Time {
	if !session.LastUsedAt.IsZero() {
		return session.LastUsedAt
	}
	if !session.UpdatedAt.IsZero() {
		return session.UpdatedAt
	}
	return session.CreatedAt
}

func newSessionID(now time.Time) (string, error) {
	var randomBytes [4]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return now.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(randomBytes[:]), nil
}

func hashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *V2Store) blobPath(sessionID string, ref BlobRef) (string, error) {
	if err := validateV2SessionID(sessionID); err != nil {
		return "", err
	}
	if err := validateBlobRef(ref); err != nil {
		return "", err
	}
	return filepath.Join(s.sessionDir(sessionID), blobsDirName, "sha256", ref.Hash[:2], ref.Hash+".data"), nil
}

func validateBlobRef(ref BlobRef) error {
	if len(ref.Hash) != sha256.Size*2 {
		return fmt.Errorf("invalid blob hash %q", ref.Hash)
	}
	if _, err := hex.DecodeString(ref.Hash); err != nil {
		return fmt.Errorf("invalid blob hash %q: %w", ref.Hash, err)
	}
	if ref.SizeBytes < 0 {
		return fmt.Errorf("invalid blob size %d", ref.SizeBytes)
	}
	return nil
}

func verifyBlobFile(path string, ref BlobRef) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return verifyBlobBytes(raw, ref)
}

func verifyBlobBytes(raw []byte, ref BlobRef) error {
	if int64(len(raw)) != ref.SizeBytes {
		return fmt.Errorf("blob %q size mismatch: got %d, want %d", ref.Hash, len(raw), ref.SizeBytes)
	}
	if got := hashBytes(raw); !strings.EqualFold(got, ref.Hash) {
		return fmt.Errorf("blob %q hash mismatch: got %s", ref.Hash, got)
	}
	return nil
}

func materializeStoredContent(sessionID, itemID string, content *StoredContent, readBlob func(BlobRef) ([]byte, error)) (string, bool, error) {
	if content == nil {
		return "", false, nil
	}
	if content.Blob == nil {
		return content.Inline, content.Inline != "", nil
	}
	if readBlob == nil {
		return "", false, fmt.Errorf("item %q content blob requires a session blob reader", itemID)
	}
	raw, err := readBlob(*content.Blob)
	if err != nil {
		return "", false, fmt.Errorf("read session %q content blob: %w", sessionID, err)
	}
	return string(raw), true, nil
}

func previewStringByBytes(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	return value[:maxBytes]
}

func corruptedSessionError(sessionID, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("%w: %s", ErrCorruptedSession, message)
	}
	return fmt.Errorf("%w %q: %s", ErrCorruptedSession, sessionID, message)
}

func copyStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func copyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	copied := make(map[string]any, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func copyMessage(message model.Message) model.Message {
	copied := message
	copied.ContentBlocks = append([]model.InputContentBlock(nil), message.ContentBlocks...)
	for i := range copied.ContentBlocks {
		if message.ContentBlocks[i].ImageBlob != nil {
			ref := *message.ContentBlocks[i].ImageBlob
			copied.ContentBlocks[i].ImageBlob = &ref
		}
	}
	copied.ToolCalls = append([]model.ToolCall(nil), message.ToolCalls...)
	copied.ProviderItems = copyProviderItems(message.ProviderItems)
	if message.ResponseState != nil {
		state := *message.ResponseState
		state.ReasoningItems = append([]json.RawMessage(nil), message.ResponseState.ReasoningItems...)
		state.OutputItems = append([]json.RawMessage(nil), message.ResponseState.OutputItems...)
		copied.ResponseState = &state
	}
	return copied
}

func copyMessagePtr(message *model.Message) *model.Message {
	if message == nil {
		return nil
	}
	copied := copyMessage(*message)
	return &copied
}

func copyMessages(messages []model.Message) []model.Message {
	if messages == nil {
		return nil
	}
	copied := make([]model.Message, len(messages))
	for i := range messages {
		copied[i] = copyMessage(messages[i])
	}
	return copied
}

func copyInstructionSources(sources []InstructionSource) []InstructionSource {
	if sources == nil {
		return nil
	}
	return append([]InstructionSource(nil), sources...)
}

func copyProviderItems(items []model.ProviderItem) []model.ProviderItem {
	if items == nil {
		return nil
	}
	copied := make([]model.ProviderItem, len(items))
	for i := range items {
		copied[i] = items[i]
		copied[i].Data = append(json.RawMessage(nil), items[i].Data...)
	}
	return copied
}

func copyStoredContent(content *StoredContent) *StoredContent {
	if content == nil {
		return nil
	}
	copied := *content
	if content.Blob != nil {
		ref := *content.Blob
		copied.Blob = &ref
	}
	return &copied
}

func copySessionItem(item SessionItem) SessionItem {
	copied := item
	copied.Message = copyMessagePtr(item.Message)
	copied.Content = copyStoredContent(item.Content)
	return copied
}

func copySessionItems(items []SessionItem) []SessionItem {
	if items == nil {
		return nil
	}
	copied := make([]SessionItem, len(items))
	for i := range items {
		copied[i] = copySessionItem(items[i])
	}
	return copied
}

func copyCompactionCheckpoint(checkpoint CompactionCheckpoint) CompactionCheckpoint {
	checkpoint.PreviousActiveHistory = copyStrings(checkpoint.PreviousActiveHistory)
	checkpoint.ReplacementHistory = copyStrings(checkpoint.ReplacementHistory)
	return checkpoint
}

func copyCompactionCheckpoints(checkpoints []CompactionCheckpoint) []CompactionCheckpoint {
	if checkpoints == nil {
		return nil
	}
	copied := make([]CompactionCheckpoint, len(checkpoints))
	for i := range checkpoints {
		copied[i] = copyCompactionCheckpoint(checkpoints[i])
	}
	return copied
}

func copySessionV2(session SessionV2) SessionV2 {
	copied := session
	copied.ModelParameters = copyMap(session.ModelParameters)
	copied.EnabledTools = copyStrings(session.EnabledTools)
	copied.EnabledMCP = copyStrings(session.EnabledMCP)
	copied.EnabledSkills = copyStrings(session.EnabledSkills)
	copied.InstructionsSnapshot = copyMessages(session.InstructionsSnapshot)
	copied.InstructionSources = copyInstructionSources(session.InstructionSources)
	copied.Items = copySessionItems(session.Items)
	copied.ActiveHistory = copyStrings(session.ActiveHistory)
	copied.Compactions = copyCompactionCheckpoints(session.Compactions)
	if session.Pricing != nil {
		pricing := *session.Pricing
		if session.Pricing.LongContext != nil {
			longContext := *session.Pricing.LongContext
			pricing.LongContext = &longContext
		}
		copied.Pricing = &pricing
	}
	return copied
}

func validateCompactionCheckpointWrite(summaryItem SessionItem, checkpoint CompactionCheckpoint, state SessionV2) error {
	if strings.TrimSpace(summaryItem.ID) == "" {
		return fmt.Errorf("compaction summary item id is required")
	}
	if strings.TrimSpace(checkpoint.ID) == "" {
		return fmt.Errorf("compaction checkpoint id is required")
	}
	if checkpoint.SummaryItemID != "" && checkpoint.SummaryItemID != summaryItem.ID {
		return fmt.Errorf("compaction checkpoint summary item %q does not match summary item %q", checkpoint.SummaryItemID, summaryItem.ID)
	}
	for _, id := range checkpoint.ReplacementHistory {
		if id == summaryItem.ID {
			continue
		}
		if _, ok := findSessionItemByID(state.Items, id); !ok {
			return corruptedSessionError(state.ID, "compaction replacement references missing item %q", id)
		}
	}
	return nil
}

func nextItemID(existing map[string]struct{}, message model.Message) string {
	prefix := "msg"
	if message.Role == model.MessageRoleSystem || message.Role == model.MessageRoleDeveloper {
		prefix = "runtime"
	} else if message.Role == model.MessageRoleProvider {
		prefix = "compaction"
	}
	for i := len(existing) + 1; ; i++ {
		id := fmt.Sprintf("%s-%06d", prefix, i)
		if _, ok := existing[id]; !ok {
			return id
		}
	}
}

// Keep io imported in this file's error surface for callers that used the old
// short-write sentinel while the JSONL implementation was removed.
var _ = io.ErrShortWrite

func reverseItems(items []SessionItem) {
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
}
