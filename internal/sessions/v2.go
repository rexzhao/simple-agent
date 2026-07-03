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
	ID            string                 `json:"id"`
	Version       int                    `json:"version"`
	Items         []SessionItem          `json:"items,omitempty"`
	ActiveHistory []string               `json:"active_history,omitempty"`
	Compactions   []CompactionCheckpoint `json:"compactions,omitempty"`
	LastSeq       int64                  `json:"last_seq,omitempty"`
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
	if err := validateSessionID(sessionID); err != nil {
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
	if err := validateSessionID(sessionID); err != nil {
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

func (s *V2Store) blobPath(ref BlobRef) (string, error) {
	if err := validateBlobRef(ref); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "blobs", "sha256", ref.Hash[:2], ref.Hash+".data"), nil
}

func (s *V2Store) requireRoot() error {
	if s == nil || strings.TrimSpace(s.root) == "" || s.root == "." {
		return fmt.Errorf("session store directory is required")
	}
	return nil
}

type v2Record struct {
	Seq        int64                 `json:"seq"`
	Type       string                `json:"type"`
	Item       *SessionItem          `json:"item,omitempty"`
	ItemIDs    []string              `json:"item_ids,omitempty"`
	Compaction *CompactionCheckpoint `json:"compaction,omitempty"`
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
