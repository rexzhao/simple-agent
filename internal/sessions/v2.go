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
	RecordTypeActiveHistoryReplaced = "active_history.replaced"
	RecordTypeCompactionCreated     = "compaction.created"
)

const (
	defaultV2MaxSegmentLines = 1000
	maxJSONLRecordBytes      = 16 * 1024 * 1024
	v2BlobsDirName           = "blobs"
)

var ErrCorruptedSession = errors.New("corrupted session")

type SessionItem struct {
	ID         string         `json:"id"`
	TurnID     string         `json:"turn_id,omitempty"`
	Seq        int64          `json:"seq,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	Kind       string         `json:"kind"`
	Visibility string         `json:"visibility"`
	Audience   string         `json:"audience"`
	Message    *model.Message `json:"message,omitempty"`
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
	Provider             string                 `json:"provider"`
	ModelProfile         string                 `json:"model_profile"`
	ModelID              string                 `json:"model_id"`
	ModelParameters      map[string]any         `json:"model_parameters,omitempty"`
	CWD                  string                 `json:"cwd"`
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
	itemsByID := make(map[string]SessionItem, len(s.Items))
	for _, item := range s.Items {
		itemsByID[item.ID] = item
	}

	messages := make([]model.Message, 0, len(s.ActiveHistory))
	for _, id := range s.ActiveHistory {
		item, ok := itemsByID[id]
		if !ok {
			return nil, corruptedSessionError(s.ID, "active history references missing item %q", id)
		}
		if item.Message == nil {
			return nil, corruptedSessionError(s.ID, "active history references item %q without a message", id)
		}
		messages = append(messages, copyMessage(*item.Message))
	}
	return messages, nil
}

func MaterializeActiveHistory(session SessionV2) ([]model.Message, error) {
	return session.MaterializeActiveHistory()
}

type V2StoreOptions struct {
	MaxSegmentLines int
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
	session.UpdatedAt = now
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
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return SessionV2{}, fmt.Errorf("write session metadata %q: %w", session.ID, err)
	}
	return session, nil
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

	infos := make([]Info, 0, len(entries))
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
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		infos = append(infos, session.info())
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].UpdatedAt.Equal(infos[j].UpdatedAt) {
			return infos[i].ID < infos[j].ID
		}
		return infos[i].UpdatedAt.After(infos[j].UpdatedAt)
	})
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

func (s *V2Store) AppendItem(sessionID string, item SessionItem) (SessionItem, error) {
	if item.CreatedAt.IsZero() {
		item.CreatedAt = s.now().UTC()
	}
	if item.ID == "" {
		return SessionItem{}, fmt.Errorf("session item id is required")
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

	line, err := json.Marshal(record)
	if err != nil {
		return 0, fmt.Errorf("marshal session record %d: %w", record.Seq, err)
	}
	line = append(line, '\n')
	if len(line) > maxJSONLRecordBytes {
		return 0, fmt.Errorf("session record %d is too large: %d bytes", record.Seq, len(line))
	}

	path, err := s.appendSegmentPath(sessionID)
	if err != nil {
		return 0, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open segment %q: %w", path, err)
	}
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		return 0, fmt.Errorf("append segment %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return 0, fmt.Errorf("close segment %q: %w", path, err)
	}
	return record.Seq, nil
}

func (s *V2Store) appendSegmentPath(sessionID string) (string, error) {
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
	lines, err := countLines(current)
	if err != nil {
		return "", err
	}
	if lines < s.maxSegmentLines {
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

func v2FilesystemAliasName(id string) string {
	return strings.TrimRight(id, ". ")
}

type v2Record struct {
	Seq        int64                 `json:"seq"`
	Type       string                `json:"type"`
	Item       *SessionItem          `json:"item,omitempty"`
	ItemIDs    []string              `json:"item_ids,omitempty"`
	Compaction *CompactionCheckpoint `json:"compaction,omitempty"`
}

type sessionV2Metadata struct {
	ID                   string                 `json:"id"`
	Version              int                    `json:"version"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
	Provider             string                 `json:"provider"`
	ModelProfile         string                 `json:"model_profile"`
	ModelID              string                 `json:"model_id"`
	ModelParameters      map[string]any         `json:"model_parameters,omitempty"`
	CWD                  string                 `json:"cwd"`
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
		return SessionV2{}, fmt.Errorf("parse session metadata %q: %w", path, err)
	}
	return metadata.session(), nil
}

func metadataFromSessionV2(session SessionV2) sessionV2Metadata {
	session = copySessionV2(session)
	return sessionV2Metadata{
		ID:                   session.ID,
		Version:              session.Version,
		CreatedAt:            session.CreatedAt,
		UpdatedAt:            session.UpdatedAt,
		Provider:             session.Provider,
		ModelProfile:         session.ModelProfile,
		ModelID:              session.ModelID,
		ModelParameters:      session.ModelParameters,
		CWD:                  session.CWD,
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
	return SessionV2{
		ID:                   m.ID,
		Version:              m.Version,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
		Provider:             m.Provider,
		ModelProfile:         m.ModelProfile,
		ModelID:              m.ModelID,
		ModelParameters:      copyMap(m.ModelParameters),
		CWD:                  m.CWD,
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
}

func replaySegment(path string, state *SessionV2) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open segment %q: %w", path, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNumber++
			if len(line) > maxJSONLRecordBytes {
				return corruptedSessionError(state.ID, "%s:%d record exceeds %d bytes", path, lineNumber, maxJSONLRecordBytes)
			}
			if err := replayRecord(path, lineNumber, line, state); err != nil {
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

func replayRecord(path string, lineNumber int, line []byte, state *SessionV2) error {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return nil
	}
	var record v2Record
	if err := json.Unmarshal(line, &record); err != nil {
		return corruptedSessionError(state.ID, "%s:%d invalid JSONL record: %v", path, lineNumber, err)
	}
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

func copySessionV2(session SessionV2) SessionV2 {
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
	}
	return copied
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
		ID:              s.ID,
		CreatedAt:       s.CreatedAt,
		UpdatedAt:       s.UpdatedAt,
		Version:         s.Version,
		Provider:        s.Provider,
		ModelProfile:    s.ModelProfile,
		ModelID:         s.ModelID,
		ContextWindow:   s.Context.ContextWindow,
		ContextSource:   s.Context.ContextWindowSource,
		SaveToolResults: s.SaveToolResults,
	}
}
