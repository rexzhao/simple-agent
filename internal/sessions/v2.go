package sessions

import (
	"bufio"
	"crypto/sha256"
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

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	VersionV2 = 2

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
	defaultV2MaxSegmentLines  = 1000
	maxJSONLRecordBytes       = 16 * 1024 * 1024
	largeContentBlobBytes     = 4 * 1024
	storedContentPreviewBytes = 240
	v2BlobsDirName            = "blobs"
)

var ErrCorruptedSession = errors.New("corrupted session")

const interruptedToolResultContent = "[tool execution interrupted]"

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

type BlobRef struct {
	Hash      string `json:"hash"`
	SizeBytes int64  `json:"size_bytes"`
	Encoding  string `json:"encoding"`
	MediaType string `json:"media_type,omitempty"`
}

type SessionV2 struct {
	ID                   string                 `json:"id"`
	Version              int                    `json:"version"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
	DisplayName          string                 `json:"display_name,omitempty"`
	Archived             bool                   `json:"-"`
	ArchivedAt           time.Time              `json:"-"`
	LastUsedAt           time.Time              `json:"last_used_at"`
	RunningTurnID        string                 `json:"running_turn_id,omitempty"`
	RunningStartedAt     time.Time              `json:"running_started_at,omitempty"`
	InterruptedTurnID    string                 `json:"interrupted_turn_id,omitempty"`
	InterruptedAt        time.Time              `json:"interrupted_at,omitempty"`
	Provider             string                 `json:"provider"`
	ModelProfile         string                 `json:"model_profile"`
	ModelID              string                 `json:"model_id"`
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
	InstructionsSnapshot []model.Message        `json:"instructions_snapshot,omitempty"`
	InstructionSources   []InstructionSource    `json:"instruction_sources,omitempty"`
	Items                []SessionItem          `json:"items,omitempty"`
	ActiveHistory        []string               `json:"active_history,omitempty"`
	Compactions          []CompactionCheckpoint `json:"compactions,omitempty"`
	LastSeq              int64                  `json:"last_seq,omitempty"`
	Context              contextwindow.Metadata `json:"context,omitempty"`
	SaveToolResults      bool                   `json:"save_tool_results"`
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
		messages = append(messages, message)
	}
	return messages, nil
}

func shouldSynthesizeInterruptedToolResult(item SessionItem, message model.Message) bool {
	if message.Role != model.MessageRoleTool {
		return false
	}
	return item.Status == ItemStatusPending || item.Status == ItemStatusInterrupted
}

func MaterializeActiveHistory(session SessionV2) ([]model.Message, error) {
	return session.MaterializeActiveHistory()
}

type V2StoreOptions struct {
	MaxSegmentLines int
}

type V2ListOptions struct {
	Archived bool
	All      bool
}

type V2Store struct {
	root            string
	maxSegmentLines int
	now             func() time.Time
}

func NewV2Store(root string) *V2Store {
	return NewV2StoreWithOptions(root, V2StoreOptions{})
}

func NewV2StoreWithOptions(root string, options V2StoreOptions) *V2Store {
	return newV2StoreWithClock(root, options, time.Now)
}

func newV2StoreWithClock(root string, options V2StoreOptions, now func() time.Time) *V2Store {
	if now == nil {
		now = time.Now
	}
	root = strings.TrimSpace(root)
	if root != "" {
		root = filepath.Clean(root)
	}
	maxSegmentLines := options.MaxSegmentLines
	if maxSegmentLines <= 0 {
		maxSegmentLines = defaultV2MaxSegmentLines
	}
	return &V2Store{
		root:            root,
		maxSegmentLines: maxSegmentLines,
		now:             now,
	}
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

func (s *V2Store) SaveMetadata(session SessionV2) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}

	now := s.now().UTC()
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
	session.UpdatedAt = now
	session = normalizeSessionLifecycle(session, now)
	session = copySessionV2(session)

	sessionDir := s.sessionDir(session.ID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return SessionV2{}, fmt.Errorf("create session directory %q: %w", sessionDir, err)
	}

	data, err := json.MarshalIndent(metadataFromSessionV2(session), "", "  ")
	if err != nil {
		return SessionV2{}, fmt.Errorf("marshal session metadata %q: %w", session.ID, err)
	}
	data = append(data, '\n')
	path := s.metadataPath(session.ID)
	if err := writeSessionMetadataAtomic(path, data); err != nil {
		return SessionV2{}, fmt.Errorf("write session metadata %q: %w", session.ID, err)
	}
	return session, nil
}

func (s *V2Store) MarkTurnRunning(sessionID, turnID string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return SessionV2{}, fmt.Errorf("running turn id is required")
	}
	session, err := s.loadMetadata(sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	session.RunningTurnID = turnID
	session.RunningStartedAt = s.now().UTC()
	return s.SaveMetadata(session)
}

func (s *V2Store) ClearRunningTurn(sessionID, turnID string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	session, err := s.loadMetadata(sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	if session.RunningTurnID == "" {
		return session, nil
	}
	if turnID = strings.TrimSpace(turnID); turnID != "" && session.RunningTurnID != turnID {
		return session, nil
	}
	session.RunningTurnID = ""
	session.RunningStartedAt = time.Time{}
	return s.SaveMetadata(session)
}

func (s *V2Store) MarkTurnInterrupted(sessionID, turnID string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	session, err := s.loadMetadata(sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	runningTurnID := strings.TrimSpace(session.RunningTurnID)
	turnID = strings.TrimSpace(turnID)
	if runningTurnID == "" {
		runningTurnID = turnID
	}
	if runningTurnID == "" {
		return session, nil
	}
	if turnID != "" && session.RunningTurnID != "" && session.RunningTurnID != turnID {
		return session, nil
	}
	session.RunningTurnID = ""
	session.RunningStartedAt = time.Time{}
	session.InterruptedTurnID = runningTurnID
	session.InterruptedAt = s.now().UTC()
	return s.SaveMetadata(session)
}

func (s *V2Store) MarkRunningTurnsInterrupted() ([]SessionV2, error) {
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
	marked := make([]SessionV2, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if err := validateV2SessionID(id); err != nil {
			continue
		}
		session, err := s.loadMetadata(id)
		if err != nil {
			continue
		}
		if strings.TrimSpace(session.RunningTurnID) == "" {
			continue
		}
		session, err = s.MarkTurnInterrupted(session.ID, session.RunningTurnID)
		if err != nil {
			return nil, err
		}
		marked = append(marked, session)
	}
	return marked, nil
}

func (s *V2Store) Load(id string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	if err := validateV2SessionID(id); err != nil {
		return SessionV2{}, err
	}

	session, err := s.loadMetadata(id)
	if err != nil {
		return SessionV2{}, err
	}
	replayed, err := s.Replay(id)
	if err != nil {
		return SessionV2{}, err
	}
	session.Items = copySessionItems(replayed.Items)
	session.ActiveHistory = copyStrings(replayed.ActiveHistory)
	session.Compactions = copyCompactionCheckpoints(replayed.Compactions)
	session.LastSeq = replayed.LastSeq
	return copySessionV2(session), nil
}

func (s *V2Store) List() ([]Info, error) {
	return s.ListWithOptions(V2ListOptions{})
}

func (s *V2Store) ListWithOptions(options V2ListOptions) ([]Info, error) {
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Info{}, nil
		}
		return nil, fmt.Errorf("read session store %q: %w", s.root, err)
	}

	sessions := make([]SessionV2, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if err := validateV2SessionID(id); err != nil {
			continue
		}
		session, err := s.loadMetadata(id)
		if err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorruptedSession) {
				continue
			}
			return nil, err
		}
		if !options.All && session.Archived != options.Archived {
			continue
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		leftLastUsed := sessionEffectiveLastUsedAt(sessions[i])
		rightLastUsed := sessionEffectiveLastUsedAt(sessions[j])
		if !leftLastUsed.Equal(rightLastUsed) {
			return leftLastUsed.After(rightLastUsed)
		}
		if !sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
		}
		return sessions[i].ID < sessions[j].ID
	})
	infos := make([]Info, 0, len(sessions))
	for _, session := range sessions {
		infos = append(infos, session.info())
	}
	return infos, nil
}

func (s *V2Store) Latest() (SessionV2, error) {
	infos, err := s.List()
	if err != nil {
		return SessionV2{}, err
	}
	if len(infos) == 0 {
		return SessionV2{}, ErrNotFound
	}
	return s.Load(infos[0].ID)
}

func (s *V2Store) Delete(id string) error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	if err := validateV2SessionID(id); err != nil {
		return err
	}

	sessionDir := s.sessionDir(id)
	info, err := os.Stat(sessionDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("stat session %q: %w", id, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("session %q is not a directory", id)
	}
	if err := os.RemoveAll(sessionDir); err != nil {
		return fmt.Errorf("delete session %q: %w", id, err)
	}
	return nil
}

func (s *V2Store) MaterializeActiveHistory(session SessionV2) ([]model.Message, error) {
	return materializeActiveHistory(session, s.ReadBlob)
}

func (s *V2Store) AppendItem(sessionID string, item SessionItem) (SessionItem, error) {
	if item.CreatedAt.IsZero() {
		item.CreatedAt = s.now().UTC()
	}
	if item.ID == "" {
		return SessionItem{}, fmt.Errorf("session item id is required")
	}
	item, err := s.blobifySessionItemContent(item)
	if err != nil {
		return SessionItem{}, err
	}
	seq, err := s.appendRecord(sessionID, v2Record{
		Type: RecordTypeItemAppended,
		Item: &item,
	})
	if err != nil {
		return SessionItem{}, err
	}
	item.Seq = seq
	return item, nil
}

func (s *V2Store) UpdateItem(sessionID string, item SessionItem) (SessionItem, error) {
	if err := s.requireRoot(); err != nil {
		return SessionItem{}, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return SessionItem{}, err
	}
	if item.ID == "" {
		return SessionItem{}, fmt.Errorf("session item id is required")
	}

	state, err := s.Replay(sessionID)
	if err != nil {
		return SessionItem{}, err
	}
	updated, _, err := s.UpdateItemFromState(sessionID, state, item)
	return updated, err
}

// UpdateItemFromState appends an item.updated record using the caller's cached
// session state and returns both the updated item and the advanced state.
// The caller must provide the latest state for this session and must be the
// session's single writer; stale state can write duplicate seqs and corrupt the
// log.
func (s *V2Store) UpdateItemFromState(sessionID string, state SessionV2, item SessionItem) (SessionItem, SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionItem{}, SessionV2{}, err
	}
	if err := validateCachedWriteState(sessionID, state); err != nil {
		return SessionItem{}, SessionV2{}, err
	}
	if item.ID == "" {
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
	updated, err := s.blobifySessionItemContent(updated)
	if err != nil {
		return SessionItem{}, SessionV2{}, err
	}

	record := v2Record{
		Seq:  state.LastSeq + 1,
		Type: RecordTypeItemUpdated,
		Item: &updated,
	}
	nextState, err := replayRecordOnState(state, record)
	if err != nil {
		return SessionItem{}, SessionV2{}, err
	}
	if err := s.appendRecords(sessionID, []v2Record{record}); err != nil {
		return SessionItem{}, SessionV2{}, err
	}
	return updated, nextState, nil
}

func (s *V2Store) ReplaceActiveHistory(sessionID string, itemIDs []string) (int64, error) {
	return s.appendRecord(sessionID, v2Record{
		Type:    RecordTypeActiveHistoryReplaced,
		ItemIDs: copyStrings(itemIDs),
	})
}

func (s *V2Store) AppendCompaction(sessionID string, checkpoint CompactionCheckpoint) (CompactionCheckpoint, error) {
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = s.now().UTC()
	}
	if checkpoint.ID == "" {
		return CompactionCheckpoint{}, fmt.Errorf("compaction checkpoint id is required")
	}
	_, err := s.appendRecord(sessionID, v2Record{
		Type:       RecordTypeCompactionCreated,
		Compaction: &checkpoint,
	})
	if err != nil {
		return CompactionCheckpoint{}, err
	}
	return checkpoint, nil
}

func (s *V2Store) AppendCompactionCheckpoint(sessionID string, summaryItem SessionItem, checkpoint CompactionCheckpoint) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return SessionV2{}, err
	}

	now := s.now().UTC()
	if summaryItem.CreatedAt.IsZero() {
		summaryItem.CreatedAt = now
	}
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = now
	}
	state, err := s.Replay(sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	if err := validateCompactionCheckpointWrite(summaryItem, checkpoint, state); err != nil {
		return SessionV2{}, err
	}
	summaryItem, err = s.blobifySessionItemContent(summaryItem)
	if err != nil {
		return SessionV2{}, err
	}

	txID := fmt.Sprintf("tx-%06d", state.LastSeq+1)
	records := make([]v2Record, 0, 5)
	nextSeq := state.LastSeq + 1
	records = append(records, v2Record{
		Seq:  nextSeq,
		Type: RecordTypeTransactionBegin,
		TxID: txID,
	})
	nextSeq++

	summaryItem.Seq = nextSeq
	summaryCopy := summaryItem
	records = append(records, v2Record{
		Seq:  nextSeq,
		Type: RecordTypeItemAppended,
		TxID: txID,
		Item: &summaryCopy,
	})
	nextSeq++

	checkpointCopy := checkpoint
	records = append(records, v2Record{
		Seq:        nextSeq,
		Type:       RecordTypeCompactionCreated,
		TxID:       txID,
		Compaction: &checkpointCopy,
	})
	nextSeq++

	records = append(records, v2Record{
		Seq:     nextSeq,
		Type:    RecordTypeActiveHistoryReplaced,
		TxID:    txID,
		ItemIDs: copyStrings(checkpoint.ReplacementHistory),
	})
	nextSeq++
	records = append(records, v2Record{
		Seq:  nextSeq,
		Type: RecordTypeTransactionCommit,
		TxID: txID,
	})

	if err := s.appendRecords(sessionID, records); err != nil {
		return SessionV2{}, err
	}
	return s.Replay(sessionID)
}

func (s *V2Store) SaveTurn(session SessionV2, items []SessionItem, activeHistory []string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}

	now := s.now().UTC()
	isNew := strings.TrimSpace(session.ID) == ""
	if isNew {
		id, err := newSessionID(now)
		if err != nil {
			return SessionV2{}, err
		}
		session.ID = id
	}
	if err := validateV2SessionID(session.ID); err != nil {
		return SessionV2{}, err
	}
	if session.Version == 0 {
		session.Version = VersionV2
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now
	session.LastUsedAt = now
	session = copySessionV2(session)

	if !isNew {
		if _, err := s.loadMetadata(session.ID); err != nil {
			if !errors.Is(err, ErrNotFound) {
				return SessionV2{}, err
			}
			isNew = true
		}
	}
	if !isNew {
		saved, err := s.SaveMetadata(session)
		if err != nil {
			return SessionV2{}, err
		}
		session = saved
	}

	if _, err := s.AppendItemsAndReplaceActiveHistory(session.ID, items, activeHistory); err != nil {
		return SessionV2{}, err
	}
	if isNew {
		if _, err := s.SaveMetadata(session); err != nil {
			_ = s.Delete(session.ID)
			return SessionV2{}, err
		}
	}

	return s.Load(session.ID)
}

func (s *V2Store) SaveCompactedTurn(session SessionV2, summaryItem SessionItem, checkpoint CompactionCheckpoint, items []SessionItem, activeHistory []string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}

	now := s.now().UTC()
	isNew := strings.TrimSpace(session.ID) == ""
	if isNew {
		id, err := newSessionID(now)
		if err != nil {
			return SessionV2{}, err
		}
		session.ID = id
	}
	if err := validateV2SessionID(session.ID); err != nil {
		return SessionV2{}, err
	}
	if session.Version == 0 {
		session.Version = VersionV2
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now
	session.LastUsedAt = now
	session = copySessionV2(session)

	if !isNew {
		if _, err := s.loadMetadata(session.ID); err != nil {
			if !errors.Is(err, ErrNotFound) {
				return SessionV2{}, err
			}
			isNew = true
		}
	}
	if !isNew {
		saved, err := s.SaveMetadata(session)
		if err != nil {
			return SessionV2{}, err
		}
		session = saved
	}

	if _, err := s.appendCompactionAndItemsReplaceActiveHistory(session.ID, summaryItem, checkpoint, items, activeHistory); err != nil {
		return SessionV2{}, err
	}
	if isNew {
		if _, err := s.SaveMetadata(session); err != nil {
			_ = s.Delete(session.ID)
			return SessionV2{}, err
		}
	}

	return s.Load(session.ID)
}

func (s *V2Store) AppendItemsAndReplaceActiveHistory(sessionID string, items []SessionItem, itemIDs []string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return SessionV2{}, err
	}

	state, err := s.Replay(sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	return s.AppendItemsAndReplaceActiveHistoryFromState(sessionID, state, items, itemIDs)
}

// AppendItemsAndReplaceActiveHistoryFromState appends a transaction using the
// caller's cached session state and returns the advanced state without replaying
// from disk. The caller must provide the latest state for this session and must
// be the session's single writer; stale state can write duplicate seqs and
// corrupt the log.
func (s *V2Store) AppendItemsAndReplaceActiveHistoryFromState(sessionID string, state SessionV2, items []SessionItem, itemIDs []string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	if err := validateCachedWriteState(sessionID, state); err != nil {
		return SessionV2{}, err
	}

	now := s.now().UTC()
	txID := fmt.Sprintf("tx-%06d", state.LastSeq+1)
	records := make([]v2Record, 0, len(items)+3)
	nextSeq := state.LastSeq + 1
	records = append(records, v2Record{
		Seq:  nextSeq,
		Type: RecordTypeTransactionBegin,
		TxID: txID,
	})
	nextSeq++
	for _, item := range items {
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		if item.ID == "" {
			return SessionV2{}, fmt.Errorf("session item id is required")
		}
		item, err := s.blobifySessionItemContent(item)
		if err != nil {
			return SessionV2{}, err
		}
		item.Seq = nextSeq
		itemCopy := item
		records = append(records, v2Record{
			Seq:  nextSeq,
			Type: RecordTypeItemAppended,
			TxID: txID,
			Item: &itemCopy,
		})
		nextSeq++
	}
	records = append(records, v2Record{
		Seq:     nextSeq,
		Type:    RecordTypeActiveHistoryReplaced,
		TxID:    txID,
		ItemIDs: copyStrings(itemIDs),
	})
	nextSeq++
	records = append(records, v2Record{
		Seq:  nextSeq,
		Type: RecordTypeTransactionCommit,
		TxID: txID,
	})

	nextState, err := replayTransactionOnState(state, records)
	if err != nil {
		return SessionV2{}, err
	}
	if err := s.appendRecords(sessionID, records); err != nil {
		return SessionV2{}, err
	}
	return nextState, nil
}

func (s *V2Store) appendCompactionAndItemsReplaceActiveHistory(sessionID string, summaryItem SessionItem, checkpoint CompactionCheckpoint, items []SessionItem, itemIDs []string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return SessionV2{}, err
	}

	state, err := s.Replay(sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	if err := validateCompactionCheckpointWrite(summaryItem, checkpoint, state); err != nil {
		return SessionV2{}, err
	}
	summaryItem, err = s.blobifySessionItemContent(summaryItem)
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
	txID := fmt.Sprintf("tx-%06d", state.LastSeq+1)
	records := make([]v2Record, 0, len(items)+5)
	nextSeq := state.LastSeq + 1
	records = append(records, v2Record{
		Seq:  nextSeq,
		Type: RecordTypeTransactionBegin,
		TxID: txID,
	})
	nextSeq++

	summaryItem.Seq = nextSeq
	summaryCopy := summaryItem
	records = append(records, v2Record{
		Seq:  nextSeq,
		Type: RecordTypeItemAppended,
		TxID: txID,
		Item: &summaryCopy,
	})
	nextSeq++

	checkpointCopy := checkpoint
	records = append(records, v2Record{
		Seq:        nextSeq,
		Type:       RecordTypeCompactionCreated,
		TxID:       txID,
		Compaction: &checkpointCopy,
	})
	nextSeq++

	for _, item := range items {
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		if item.ID == "" {
			return SessionV2{}, fmt.Errorf("session item id is required")
		}
		item, err = s.blobifySessionItemContent(item)
		if err != nil {
			return SessionV2{}, err
		}
		item.Seq = nextSeq
		itemCopy := item
		records = append(records, v2Record{
			Seq:  nextSeq,
			Type: RecordTypeItemAppended,
			TxID: txID,
			Item: &itemCopy,
		})
		nextSeq++
	}
	records = append(records, v2Record{
		Seq:     nextSeq,
		Type:    RecordTypeActiveHistoryReplaced,
		TxID:    txID,
		ItemIDs: copyStrings(itemIDs),
	})
	nextSeq++
	records = append(records, v2Record{
		Seq:  nextSeq,
		Type: RecordTypeTransactionCommit,
		TxID: txID,
	})

	if err := s.appendRecords(sessionID, records); err != nil {
		return SessionV2{}, err
	}
	return s.Replay(sessionID)
}

func (s *V2Store) Replay(sessionID string) (SessionV2, error) {
	if err := s.requireRoot(); err != nil {
		return SessionV2{}, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return SessionV2{}, err
	}

	state := SessionV2{
		ID:      sessionID,
		Version: VersionV2,
	}
	segments, err := s.segmentPaths(sessionID)
	if err != nil {
		return SessionV2{}, err
	}
	for _, path := range segments {
		if err := replaySegment(path, &state); err != nil {
			return SessionV2{}, err
		}
	}
	return state, nil
}

type PersistedEvent struct {
	Seq          int64
	Type         string
	ItemID       string
	CompactionID string
}

func (s *V2Store) PersistedEventsAfter(sessionID string, afterSeq int64) ([]PersistedEvent, error) {
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return nil, err
	}
	if afterSeq < 0 {
		return nil, fmt.Errorf("after seq must be non-negative")
	}

	state := SessionV2{
		ID:      sessionID,
		Version: VersionV2,
	}
	segments, err := s.segmentPaths(sessionID)
	if err != nil {
		return nil, err
	}
	events := make([]PersistedEvent, 0)
	for _, path := range segments {
		if err := replaySegmentPersistedEvents(path, &state, afterSeq, &events); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (s *V2Store) WriteBlob(raw []byte, encoding, mediaType string) (BlobRef, error) {
	if err := s.requireRoot(); err != nil {
		return BlobRef{}, err
	}

	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	ref := BlobRef{
		Hash:      hash,
		SizeBytes: int64(len(raw)),
		Encoding:  encoding,
		MediaType: mediaType,
	}
	path, err := s.blobPath(ref)
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
		return BlobRef{}, fmt.Errorf("stat blob %q: %w", ref.Hash, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+ref.Hash+".*.tmp")
	if err != nil {
		return BlobRef{}, fmt.Errorf("create temporary blob %q: %w", ref.Hash, err)
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
		return BlobRef{}, fmt.Errorf("write temporary blob %q: %w", ref.Hash, err)
	}
	if err := tmp.Close(); err != nil {
		return BlobRef{}, fmt.Errorf("close temporary blob %q: %w", ref.Hash, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			if verifyErr := verifyBlobFile(path, ref); verifyErr != nil {
				return BlobRef{}, verifyErr
			}
			return ref, nil
		}
		return BlobRef{}, fmt.Errorf("commit blob %q: %w", ref.Hash, err)
	}
	cleanup = false
	return ref, nil
}

func (s *V2Store) ReadBlob(ref BlobRef) ([]byte, error) {
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	path, err := s.blobPath(ref)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("blob %q not found", ref.Hash)
		}
		return nil, fmt.Errorf("read blob %q: %w", ref.Hash, err)
	}
	if err := verifyBlobBytes(raw, ref); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *V2Store) blobifySessionItemContent(item SessionItem) (SessionItem, error) {
	if item.Message == nil || len(item.Message.Content) <= largeContentBlobBytes {
		return item, nil
	}

	message := copyMessage(*item.Message)
	raw := []byte(message.Content)
	ref, err := s.WriteBlob(raw, "utf-8", "text/plain")
	if err != nil {
		return SessionItem{}, err
	}
	item.Message = &message
	item.Message.Content = ""
	item.Content = &StoredContent{
		Blob:    &ref,
		Preview: previewStringByBytes(string(raw), storedContentPreviewBytes),
	}
	return item, nil
}

func (s *V2Store) appendRecord(sessionID string, record v2Record) (int64, error) {
	if err := s.requireRoot(); err != nil {
		return 0, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return 0, err
	}
	state, err := s.Replay(sessionID)
	if err != nil {
		return 0, err
	}
	record.Seq = state.LastSeq + 1
	if record.Type == RecordTypeItemAppended && record.Item != nil {
		record.Item.Seq = record.Seq
	}

	if err := s.appendRecords(sessionID, []v2Record{record}); err != nil {
		return 0, err
	}
	return record.Seq, nil
}

func (s *V2Store) appendRecords(sessionID string, records []v2Record) error {
	if err := s.requireRoot(); err != nil {
		return err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return err
	}
	lines := make([][]byte, 0, len(records))
	for _, record := range records {
		line, err := marshalV2RecordLine(record)
		if err != nil {
			return err
		}
		lines = append(lines, line)
	}

	path, err := s.appendSegmentPathForLines(sessionID, len(lines))
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open segment %q: %w", path, err)
	}
	for _, line := range lines {
		n, err := file.Write(line)
		if err != nil {
			_ = file.Close()
			return fmt.Errorf("append segment %q: %w", path, err)
		}
		if n != len(line) {
			_ = file.Close()
			return fmt.Errorf("append segment %q: %w", path, io.ErrShortWrite)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close segment %q: %w", path, err)
	}
	return nil
}

func marshalV2RecordLine(record v2Record) ([]byte, error) {
	line, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("marshal session record %d: %w", record.Seq, err)
	}
	line = append(line, '\n')
	if len(line) > maxJSONLRecordBytes {
		return nil, fmt.Errorf("session record %d is too large: %d bytes", record.Seq, len(line))
	}
	return line, nil
}

func (s *V2Store) appendSegmentPath(sessionID string) (string, error) {
	return s.appendSegmentPathForLines(sessionID, 1)
}

func (s *V2Store) appendSegmentPathForLines(sessionID string, lineCount int) (string, error) {
	segmentsDir := s.segmentsDir(sessionID)
	if err := os.MkdirAll(segmentsDir, 0o755); err != nil {
		return "", fmt.Errorf("create segments directory %q: %w", segmentsDir, err)
	}

	segments, err := s.segmentPaths(sessionID)
	if err != nil {
		return "", err
	}
	if len(segments) == 0 {
		return filepath.Join(segmentsDir, "000001.jsonl"), nil
	}
	current := segments[len(segments)-1]
	complete, err := fileEndsWithNewline(current)
	if err != nil {
		return "", err
	}
	if !complete {
		next := segmentNumber(filepath.Base(current)) + 1
		return filepath.Join(segmentsDir, fmt.Sprintf("%06d.jsonl", next)), nil
	}
	lines, err := countLines(current)
	if err != nil {
		return "", err
	}
	if lines+lineCount <= s.maxSegmentLines {
		return current, nil
	}
	next := segmentNumber(filepath.Base(current)) + 1
	return filepath.Join(segmentsDir, fmt.Sprintf("%06d.jsonl", next)), nil
}

func (s *V2Store) segmentPaths(sessionID string) ([]string, error) {
	segmentsDir := s.segmentsDir(sessionID)
	entries, err := os.ReadDir(segmentsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read segments directory %q: %w", segmentsDir, err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") || segmentNumber(name) == 0 {
			continue
		}
		paths = append(paths, filepath.Join(segmentsDir, name))
	}
	sort.Strings(paths)
	return paths, nil
}

func (s *V2Store) segmentsDir(sessionID string) string {
	return filepath.Join(s.root, sessionID, "segments")
}

func (s *V2Store) sessionDir(sessionID string) string {
	return filepath.Join(s.root, sessionID)
}

func (s *V2Store) metadataPath(sessionID string) string {
	return filepath.Join(s.sessionDir(sessionID), "meta.json")
}

func (s *V2Store) blobPath(ref BlobRef) (string, error) {
	if err := validateBlobRef(ref); err != nil {
		return "", err
	}
	return filepath.Join(s.root, v2BlobsDirName, "sha256", ref.Hash[:2], ref.Hash+".data"), nil
}

func (s *V2Store) requireRoot() error {
	if s == nil || strings.TrimSpace(s.root) == "" || s.root == "." {
		return fmt.Errorf("session store directory is required")
	}
	return nil
}

func validateV2SessionID(id string) error {
	if err := validateSessionID(id); err != nil {
		return err
	}
	if strings.EqualFold(v2FilesystemAliasName(id), v2BlobsDirName) {
		return fmt.Errorf("reserved v2 session id %q", id)
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

func v2FilesystemAliasName(id string) string {
	return strings.TrimRight(id, ". ")
}

type v2Record struct {
	Seq        int64                 `json:"seq"`
	Type       string                `json:"type"`
	TxID       string                `json:"tx_id,omitempty"`
	Item       *SessionItem          `json:"item,omitempty"`
	ItemIDs    []string              `json:"item_ids,omitempty"`
	Compaction *CompactionCheckpoint `json:"compaction,omitempty"`
}

type sessionV2Metadata struct {
	ID                   string                 `json:"id"`
	Version              int                    `json:"version"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
	DisplayName          string                 `json:"display_name,omitempty"`
	ArchivedAt           *time.Time             `json:"archived_at"`
	LastUsedAt           time.Time              `json:"last_used_at"`
	RunningTurnID        string                 `json:"running_turn_id,omitempty"`
	RunningStartedAt     time.Time              `json:"running_started_at,omitempty"`
	InterruptedTurnID    string                 `json:"interrupted_turn_id,omitempty"`
	InterruptedAt        time.Time              `json:"interrupted_at,omitempty"`
	Provider             string                 `json:"provider"`
	ModelProfile         string                 `json:"model_profile"`
	ModelID              string                 `json:"model_id"`
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
	InstructionsSnapshot []model.Message        `json:"instructions_snapshot,omitempty"`
	InstructionSources   []InstructionSource    `json:"instruction_sources,omitempty"`
	Context              contextwindow.Metadata `json:"context,omitempty"`
	SaveToolResults      bool                   `json:"save_tool_results"`
}

func (s *V2Store) loadMetadata(id string) (SessionV2, error) {
	session, err := readSessionV2MetadataFile(s.metadataPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionV2{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return SessionV2{}, err
	}
	if session.ID == "" {
		session.ID = id
	}
	if session.ID != id {
		return SessionV2{}, mismatchedSessionIDError(id, session.ID)
	}
	return copySessionV2(session), nil
}

func readSessionV2MetadataFile(path string) (SessionV2, error) {
	file, err := os.Open(path)
	if err != nil {
		return SessionV2{}, err
	}
	defer file.Close()

	var metadata sessionV2Metadata
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	if err := decoder.Decode(&metadata); err != nil {
		return SessionV2{}, fmt.Errorf("%w: parse session metadata %q: %v", ErrCorruptedSession, path, err)
	}
	return metadata.session(), nil
}

func writeSessionMetadataAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".meta-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
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
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := replaceFileAtomic(tempPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	cleanup = false
	return nil
}

func metadataFromSessionV2(session SessionV2) sessionV2Metadata {
	session = copySessionV2(session)
	return sessionV2Metadata{
		ID:                   session.ID,
		Version:              session.Version,
		CreatedAt:            session.CreatedAt,
		UpdatedAt:            session.UpdatedAt,
		DisplayName:          session.DisplayName,
		ArchivedAt:           sessionArchivedAtPtr(session),
		LastUsedAt:           session.LastUsedAt,
		RunningTurnID:        session.RunningTurnID,
		RunningStartedAt:     session.RunningStartedAt,
		InterruptedTurnID:    session.InterruptedTurnID,
		InterruptedAt:        session.InterruptedAt,
		Provider:             session.Provider,
		ModelProfile:         session.ModelProfile,
		ModelID:              session.ModelID,
		ModelParameters:      session.ModelParameters,
		CWD:                  session.CWD,
		ProjectID:            session.ProjectID,
		CreatedCWD:           session.CreatedCWD,
		ConfigPath:           session.ConfigPath,
		ConfigDir:            session.ConfigDir,
		EnabledTools:         session.EnabledTools,
		EnabledMCP:           session.EnabledMCP,
		EnabledSkills:        session.EnabledSkills,
		ShowReasoning:        session.ShowReasoning,
		InstructionsSnapshot: session.InstructionsSnapshot,
		InstructionSources:   session.InstructionSources,
		Context:              session.Context,
		SaveToolResults:      session.SaveToolResults,
	}
}

func (m sessionV2Metadata) session() SessionV2 {
	session := SessionV2{
		ID:                   m.ID,
		Version:              m.Version,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
		DisplayName:          m.DisplayName,
		LastUsedAt:           m.LastUsedAt,
		RunningTurnID:        m.RunningTurnID,
		RunningStartedAt:     m.RunningStartedAt,
		InterruptedTurnID:    m.InterruptedTurnID,
		InterruptedAt:        m.InterruptedAt,
		Provider:             m.Provider,
		ModelProfile:         m.ModelProfile,
		ModelID:              m.ModelID,
		ModelParameters:      copyMap(m.ModelParameters),
		CWD:                  m.CWD,
		ProjectID:            m.ProjectID,
		CreatedCWD:           m.CreatedCWD,
		ConfigPath:           m.ConfigPath,
		ConfigDir:            m.ConfigDir,
		EnabledTools:         copyStrings(m.EnabledTools),
		EnabledMCP:           copyStrings(m.EnabledMCP),
		EnabledSkills:        copyStrings(m.EnabledSkills),
		ShowReasoning:        m.ShowReasoning,
		InstructionsSnapshot: copyMessages(m.InstructionsSnapshot),
		InstructionSources:   copyInstructionSources(m.InstructionSources),
		Context:              m.Context,
		SaveToolResults:      m.SaveToolResults,
	}
	if session.LastUsedAt.IsZero() {
		session.LastUsedAt = sessionEffectiveLastUsedAt(session)
	}
	if m.ArchivedAt != nil {
		session.ArchivedAt = m.ArchivedAt.UTC()
	}
	session = normalizeSessionLifecycle(session, session.UpdatedAt)
	return session
}

func sessionArchivedAtPtr(session SessionV2) *time.Time {
	session = normalizeSessionLifecycle(session, time.Time{})
	if session.ArchivedAt.IsZero() {
		return nil
	}
	value := session.ArchivedAt.UTC()
	return &value
}

func normalizeSessionLifecycle(session SessionV2, fallback time.Time) SessionV2 {
	if !session.ArchivedAt.IsZero() {
		session.ArchivedAt = session.ArchivedAt.UTC()
		session.Archived = true
		return session
	}
	if session.Archived {
		if !fallback.IsZero() {
			session.ArchivedAt = fallback.UTC()
		} else if !session.UpdatedAt.IsZero() {
			session.ArchivedAt = session.UpdatedAt.UTC()
		} else if !session.CreatedAt.IsZero() {
			session.ArchivedAt = session.CreatedAt.UTC()
		}
		session.Archived = !session.ArchivedAt.IsZero()
		return session
	}
	session.Archived = false
	return session
}

type replayTransaction struct {
	txID    string
	records []v2Record
}

func replaySegment(path string, state *SessionV2) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open segment %q: %w", path, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0
	var pending *replayTransaction
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if errors.Is(err, io.EOF) && line[len(line)-1] != '\n' {
				break
			}
			lineNumber++
			if len(line) > maxJSONLRecordBytes {
				return corruptedSessionError(state.ID, "%s:%d record exceeds %d bytes", path, lineNumber, maxJSONLRecordBytes)
			}
			if err := replayRecord(path, lineNumber, line, state, &pending); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read segment %q: %w", path, err)
		}
	}
	return nil
}

func replayRecord(path string, lineNumber int, line []byte, state *SessionV2, pending **replayTransaction) error {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return nil
	}
	var record v2Record
	if err := json.Unmarshal(line, &record); err != nil {
		return corruptedSessionError(state.ID, "%s:%d invalid JSONL record: %v", path, lineNumber, err)
	}
	if record.Type == RecordTypeTransactionBegin {
		if record.TxID == "" {
			return corruptedSessionError(state.ID, "%s:%d transaction.begin missing tx_id", path, lineNumber)
		}
		if record.Seq != state.LastSeq+1 {
			return corruptedSessionError(state.ID, "%s:%d record seq %d follows seq %d", path, lineNumber, record.Seq, state.LastSeq)
		}
		*pending = &replayTransaction{txID: record.TxID, records: []v2Record{record}}
		return nil
	}
	if record.TxID != "" {
		if *pending == nil || (*pending).txID != record.TxID {
			*pending = nil
			return nil
		}
		expectedSeq := state.LastSeq + int64(len((*pending).records)) + 1
		if record.Seq != expectedSeq {
			*pending = nil
			return nil
		}
		if record.Type == RecordTypeTransactionCommit {
			if err := replayCommittedTransaction(*pending, record, state); err != nil {
				return err
			}
			*pending = nil
			return nil
		}
		(*pending).records = append((*pending).records, record)
		return nil
	}
	*pending = nil
	return replayCommittedRecord(path, lineNumber, record, state)
}

func replaySegmentPersistedEvents(path string, state *SessionV2, afterSeq int64, events *[]PersistedEvent) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open segment %q: %w", path, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0
	var pending *replayTransaction
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if errors.Is(err, io.EOF) && line[len(line)-1] != '\n' {
				break
			}
			lineNumber++
			if len(line) > maxJSONLRecordBytes {
				return corruptedSessionError(state.ID, "%s:%d record exceeds %d bytes", path, lineNumber, maxJSONLRecordBytes)
			}
			if err := replayRecordPersistedEvents(path, lineNumber, line, state, &pending, afterSeq, events); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read segment %q: %w", path, err)
		}
	}
	return nil
}

func replayRecordPersistedEvents(path string, lineNumber int, line []byte, state *SessionV2, pending **replayTransaction, afterSeq int64, events *[]PersistedEvent) error {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return nil
	}
	var record v2Record
	if err := json.Unmarshal(line, &record); err != nil {
		return corruptedSessionError(state.ID, "%s:%d invalid JSONL record: %v", path, lineNumber, err)
	}
	if record.Type == RecordTypeTransactionBegin {
		if record.TxID == "" {
			return corruptedSessionError(state.ID, "%s:%d transaction.begin missing tx_id", path, lineNumber)
		}
		if record.Seq != state.LastSeq+1 {
			return corruptedSessionError(state.ID, "%s:%d record seq %d follows seq %d", path, lineNumber, record.Seq, state.LastSeq)
		}
		*pending = &replayTransaction{txID: record.TxID, records: []v2Record{record}}
		return nil
	}
	if record.TxID != "" {
		if *pending == nil || (*pending).txID != record.TxID {
			*pending = nil
			return nil
		}
		expectedSeq := state.LastSeq + int64(len((*pending).records)) + 1
		if record.Seq != expectedSeq {
			*pending = nil
			return nil
		}
		if record.Type == RecordTypeTransactionCommit {
			if err := replayCommittedTransactionPersistedEvents(*pending, record, state, afterSeq, events); err != nil {
				return err
			}
			*pending = nil
			return nil
		}
		(*pending).records = append((*pending).records, record)
		return nil
	}
	*pending = nil
	if err := replayCommittedRecord(path, lineNumber, record, state); err != nil {
		return err
	}
	if record.Seq > afterSeq {
		if event, ok := persistedEventFromRecord(record, false); ok {
			*events = append(*events, event)
		}
	}
	return nil
}

func replayCommittedTransaction(pending *replayTransaction, commit v2Record, state *SessionV2) error {
	if pending == nil || len(pending.records) == 0 {
		return nil
	}
	temp := copySessionV2(*state)
	temp.LastSeq = pending.records[0].Seq
	for _, record := range pending.records[1:] {
		if err := replayCommittedRecord("", 0, record, &temp); err != nil {
			return err
		}
	}
	if commit.Seq != temp.LastSeq+1 {
		return corruptedSessionError(state.ID, "transaction %q commit seq %d follows seq %d", commit.TxID, commit.Seq, temp.LastSeq)
	}
	temp.LastSeq = commit.Seq
	*state = temp
	return nil
}

func replayTransactionOnState(state SessionV2, records []v2Record) (SessionV2, error) {
	if len(records) < 2 {
		return SessionV2{}, fmt.Errorf("transaction records are required")
	}
	begin := records[0]
	commit := records[len(records)-1]
	if begin.Type != RecordTypeTransactionBegin {
		return SessionV2{}, fmt.Errorf("transaction must start with %s", RecordTypeTransactionBegin)
	}
	if commit.Type != RecordTypeTransactionCommit {
		return SessionV2{}, fmt.Errorf("transaction must end with %s", RecordTypeTransactionCommit)
	}
	if begin.TxID == "" || begin.TxID != commit.TxID {
		return SessionV2{}, fmt.Errorf("transaction tx_id mismatch")
	}
	next := copySessionV2(state)
	pending := &replayTransaction{
		txID:    begin.TxID,
		records: append([]v2Record(nil), records[:len(records)-1]...),
	}
	if err := replayCommittedTransaction(pending, commit, &next); err != nil {
		return SessionV2{}, err
	}
	return next, nil
}

func replayRecordOnState(state SessionV2, record v2Record) (SessionV2, error) {
	next := copySessionV2(state)
	if err := replayCommittedRecord("", 0, record, &next); err != nil {
		return SessionV2{}, err
	}
	return next, nil
}

func replayCommittedTransactionPersistedEvents(pending *replayTransaction, commit v2Record, state *SessionV2, afterSeq int64, events *[]PersistedEvent) error {
	if pending == nil || len(pending.records) == 0 {
		return nil
	}
	temp := copySessionV2(*state)
	temp.LastSeq = pending.records[0].Seq
	hasCompaction := false
	for _, record := range pending.records[1:] {
		if record.Type == RecordTypeCompactionCreated {
			hasCompaction = true
			break
		}
	}
	txEvents := make([]PersistedEvent, 0, len(pending.records))
	for _, record := range pending.records[1:] {
		if err := replayCommittedRecord("", 0, record, &temp); err != nil {
			return err
		}
		if record.Seq <= afterSeq {
			continue
		}
		if event, ok := persistedEventFromRecord(record, hasCompaction); ok {
			txEvents = append(txEvents, event)
		}
	}
	if commit.Seq != temp.LastSeq+1 {
		return corruptedSessionError(state.ID, "transaction %q commit seq %d follows seq %d", commit.TxID, commit.Seq, temp.LastSeq)
	}
	temp.LastSeq = commit.Seq
	*state = temp
	*events = append(*events, txEvents...)
	return nil
}

func persistedEventFromRecord(record v2Record, includeActiveHistory bool) (PersistedEvent, bool) {
	switch record.Type {
	case RecordTypeItemAppended:
		if record.Item == nil {
			return PersistedEvent{}, false
		}
		return PersistedEvent{
			Seq:    record.Seq,
			Type:   record.Type,
			ItemID: record.Item.ID,
		}, true
	case RecordTypeItemUpdated:
		if record.Item == nil {
			return PersistedEvent{}, false
		}
		return PersistedEvent{
			Seq:    record.Seq,
			Type:   record.Type,
			ItemID: record.Item.ID,
		}, true
	case RecordTypeCompactionCreated:
		if record.Compaction == nil {
			return PersistedEvent{}, false
		}
		return PersistedEvent{
			Seq:          record.Seq,
			Type:         record.Type,
			CompactionID: record.Compaction.ID,
		}, true
	case RecordTypeActiveHistoryReplaced:
		if !includeActiveHistory {
			return PersistedEvent{}, false
		}
		return PersistedEvent{
			Seq:  record.Seq,
			Type: record.Type,
		}, true
	default:
		return PersistedEvent{}, false
	}
}

func replayCommittedRecord(path string, lineNumber int, record v2Record, state *SessionV2) error {
	if record.Seq != state.LastSeq+1 {
		return corruptedSessionError(state.ID, "%s:%d record seq %d follows seq %d", path, lineNumber, record.Seq, state.LastSeq)
	}
	switch record.Type {
	case RecordTypeItemAppended:
		if record.Item == nil {
			return corruptedSessionError(state.ID, "%s:%d item.appended missing item", path, lineNumber)
		}
		if record.Item.Seq != 0 && record.Item.Seq != record.Seq {
			return corruptedSessionError(state.ID, "%s:%d item seq %d does not match record seq %d", path, lineNumber, record.Item.Seq, record.Seq)
		}
		record.Item.Seq = record.Seq
		state.Items = append(state.Items, *record.Item)
	case RecordTypeItemUpdated:
		if record.Item == nil {
			return corruptedSessionError(state.ID, "%s:%d item.updated missing item", path, lineNumber)
		}
		updated := false
		for i := range state.Items {
			if state.Items[i].ID != record.Item.ID {
				continue
			}
			existing := state.Items[i]
			existing.Message = copyMessagePtr(record.Item.Message)
			existing.Content = copyStoredContent(record.Item.Content)
			existing.Status = record.Item.Status
			state.Items[i] = existing
			updated = true
			break
		}
		if !updated {
			return corruptedSessionError(state.ID, "%s:%d item.updated references missing item %q", path, lineNumber, record.Item.ID)
		}
	case RecordTypeActiveHistoryReplaced:
		state.ActiveHistory = copyStrings(record.ItemIDs)
	case RecordTypeCompactionCreated:
		if record.Compaction == nil {
			return corruptedSessionError(state.ID, "%s:%d compaction.created missing compaction", path, lineNumber)
		}
		state.Compactions = append(state.Compactions, *record.Compaction)
	default:
		return corruptedSessionError(state.ID, "%s:%d unknown session record type %q", path, lineNumber, record.Type)
	}
	state.LastSeq = record.Seq
	return nil
}

func countLines(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open segment %q: %w", path, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lines := 0
	for {
		_, err := reader.ReadBytes('\n')
		if err == nil {
			lines++
			continue
		}
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		return 0, fmt.Errorf("read segment %q: %w", path, err)
	}
}

func fileEndsWithNewline(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open segment %q: %w", path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat segment %q: %w", path, err)
	}
	if info.Size() == 0 {
		return true, nil
	}
	if _, err := file.Seek(-1, io.SeekEnd); err != nil {
		return false, fmt.Errorf("seek segment %q: %w", path, err)
	}
	var last [1]byte
	if _, err := io.ReadFull(file, last[:]); err != nil {
		return false, fmt.Errorf("read segment %q: %w", path, err)
	}
	return last[0] == '\n', nil
}

func segmentNumber(name string) int {
	if len(name) != len("000001.jsonl") || !strings.HasSuffix(name, ".jsonl") {
		return 0
	}
	var number int
	if _, err := fmt.Sscanf(strings.TrimSuffix(name, ".jsonl"), "%06d", &number); err != nil {
		return 0
	}
	return number
}

func validateCompactionCheckpointWrite(summaryItem SessionItem, checkpoint CompactionCheckpoint, state SessionV2) error {
	if strings.TrimSpace(summaryItem.ID) == "" {
		return fmt.Errorf("compaction summary item id is required")
	}
	if summaryItem.Kind != ItemKindMessage {
		return fmt.Errorf("compaction summary item kind must be %q", ItemKindMessage)
	}
	if summaryItem.Visibility != ItemVisibilityHidden {
		return fmt.Errorf("compaction summary item visibility must be %q", ItemVisibilityHidden)
	}
	if summaryItem.Audience != ItemAudienceModel {
		return fmt.Errorf("compaction summary item audience must be %q", ItemAudienceModel)
	}
	if summaryItem.Message == nil {
		return fmt.Errorf("compaction summary item message is required")
	}
	if strings.TrimSpace(summaryItem.Message.Content) == "" {
		return fmt.Errorf("compaction summary message content is required")
	}
	if strings.TrimSpace(checkpoint.ID) == "" {
		return fmt.Errorf("compaction checkpoint id is required")
	}
	if strings.TrimSpace(checkpoint.SummaryItemID) == "" {
		return fmt.Errorf("compaction checkpoint summary item id is required")
	}
	if checkpoint.SummaryItemID != summaryItem.ID {
		return fmt.Errorf("compaction checkpoint summary item id %q does not match summary item id %q", checkpoint.SummaryItemID, summaryItem.ID)
	}
	if len(checkpoint.ReplacementHistory) == 0 {
		return fmt.Errorf("compaction replacement history is required")
	}
	itemsByID := make(map[string]SessionItem, len(state.Items))
	for _, item := range state.Items {
		if item.ID == summaryItem.ID {
			return fmt.Errorf("compaction summary item id %q already exists", summaryItem.ID)
		}
		itemsByID[item.ID] = item
	}

	includesSummary := false
	for i, id := range checkpoint.ReplacementHistory {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("compaction replacement history contains empty item id at index %d", i)
		}
		if id == summaryItem.ID {
			includesSummary = true
		}
	}
	if !includesSummary {
		return fmt.Errorf("compaction replacement history must include summary item id %q", summaryItem.ID)
	}
	for _, id := range checkpoint.ReplacementHistory {
		if id == summaryItem.ID {
			continue
		}
		item, ok := itemsByID[id]
		if !ok {
			return fmt.Errorf("compaction replacement history references missing item id %q", id)
		}
		if item.Message == nil {
			return fmt.Errorf("compaction replacement history references item id %q without a message", id)
		}
	}
	return nil
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
		return fmt.Errorf("read blob %q: %w", ref.Hash, err)
	}
	return verifyBlobBytes(raw, ref)
}

func verifyBlobBytes(raw []byte, ref BlobRef) error {
	if int64(len(raw)) != ref.SizeBytes {
		return fmt.Errorf("blob %q size mismatch: got %d bytes, want %d", ref.Hash, len(raw), ref.SizeBytes)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != ref.Hash {
		return fmt.Errorf("blob %q hash mismatch: got %s", ref.Hash, got)
	}
	return nil
}

func materializeStoredContent(sessionID, itemID string, content *StoredContent, readBlob func(BlobRef) ([]byte, error)) (string, bool, error) {
	if content == nil {
		return "", false, nil
	}
	if content.Inline != "" {
		return content.Inline, true, nil
	}
	if content.Blob == nil {
		return "", false, nil
	}
	if readBlob == nil {
		return "", false, corruptedSessionError(sessionID, "active history references blob-backed item %q without store-backed materialization", itemID)
	}
	raw, err := readBlob(*content.Blob)
	if err != nil {
		return "", false, corruptedSessionError(sessionID, "active history item %q blob content is unavailable: %v", itemID, err)
	}
	return string(raw), true, nil
}

func previewStringByBytes(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	cut := 0
	for i := range value {
		if i > maxBytes {
			break
		}
		cut = i
	}
	if cut == 0 {
		return ""
	}
	return value[:cut]
}

func corruptedSessionError(sessionID, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if sessionID == "" {
		return fmt.Errorf("%w: %s", ErrCorruptedSession, message)
	}
	return fmt.Errorf("%w %q: %s", ErrCorruptedSession, sessionID, message)
}

func copyMessage(message model.Message) model.Message {
	message.ToolCalls = append([]model.ToolCall(nil), message.ToolCalls...)
	return message
}

func copyMessagePtr(message *model.Message) *model.Message {
	if message == nil {
		return nil
	}
	copied := copyMessage(*message)
	return &copied
}

func copySessionV2(session SessionV2) SessionV2 {
	session = normalizeSessionLifecycle(session, time.Time{})
	session.ModelParameters = copyMap(session.ModelParameters)
	session.EnabledTools = copyStrings(session.EnabledTools)
	session.EnabledMCP = copyStrings(session.EnabledMCP)
	session.EnabledSkills = copyStrings(session.EnabledSkills)
	session.InstructionsSnapshot = copyMessages(session.InstructionsSnapshot)
	session.InstructionSources = copyInstructionSources(session.InstructionSources)
	session.Items = copySessionItems(session.Items)
	session.ActiveHistory = copyStrings(session.ActiveHistory)
	session.Compactions = copyCompactionCheckpoints(session.Compactions)
	return session
}

func copySessionItems(items []SessionItem) []SessionItem {
	if items == nil {
		return nil
	}
	copied := append([]SessionItem(nil), items...)
	for i := range copied {
		if items[i].Message != nil {
			message := copyMessage(*items[i].Message)
			copied[i].Message = &message
		}
		copied[i].Content = copyStoredContent(items[i].Content)
	}
	return copied
}

func copyStoredContent(content *StoredContent) *StoredContent {
	if content == nil {
		return nil
	}
	copied := *content
	if content.Blob != nil {
		blob := *content.Blob
		copied.Blob = &blob
	}
	return &copied
}

func copyCompactionCheckpoints(checkpoints []CompactionCheckpoint) []CompactionCheckpoint {
	if checkpoints == nil {
		return nil
	}
	copied := append([]CompactionCheckpoint(nil), checkpoints...)
	for i := range copied {
		copied[i].PreviousActiveHistory = copyStrings(checkpoints[i].PreviousActiveHistory)
		copied[i].ReplacementHistory = copyStrings(checkpoints[i].ReplacementHistory)
	}
	return copied
}

func (s SessionV2) info() Info {
	return Info{
		ID:                s.ID,
		CreatedAt:         s.CreatedAt,
		UpdatedAt:         s.UpdatedAt,
		Version:           s.Version,
		Provider:          s.Provider,
		ModelProfile:      s.ModelProfile,
		ModelID:           s.ModelID,
		RunningTurnID:     s.RunningTurnID,
		RunningStartedAt:  s.RunningStartedAt,
		InterruptedTurnID: s.InterruptedTurnID,
		InterruptedAt:     s.InterruptedAt,
		ProjectID:         s.ProjectID,
		CreatedCWD:        s.CreatedCWD,
		ContextWindow:     s.Context.ContextWindow,
		ContextSource:     s.Context.ContextWindowSource,
		SaveToolResults:   s.SaveToolResults,
	}
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
