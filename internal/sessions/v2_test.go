package sessions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestV2StoreAppendItemsReplayBySeq(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	first, err := store.AppendItem("session-1", SessionItem{
		ID:         "item-1",
		TurnID:     "turn-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: "hello"},
	})
	if err != nil {
		t.Fatalf("AppendItem(first) error = %v", err)
	}
	second, err := store.AppendItem("session-1", SessionItem{
		ID:         "item-2",
		TurnID:     "turn-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "hi"},
	})
	if err != nil {
		t.Fatalf("AppendItem(second) error = %v", err)
	}

	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("seqs = %d, %d; want 1, 2", first.Seq, second.Seq)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if replayed.LastSeq != 2 {
		t.Fatalf("LastSeq = %d, want 2", replayed.LastSeq)
	}
	if got := []string{replayed.Items[0].ID, replayed.Items[1].ID}; !reflect.DeepEqual(got, []string{"item-1", "item-2"}) {
		t.Fatalf("replayed item order = %#v, want item-1,item-2", got)
	}
	if replayed.Items[0].Seq != 1 || replayed.Items[1].Seq != 2 {
		t.Fatalf("replayed seqs = %d, %d; want 1, 2", replayed.Items[0].Seq, replayed.Items[1].Seq)
	}
}

func TestV2StoreSegmentRolloverByMaxLineCount(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2StoreWithOptions(root, V2StoreOptions{MaxSegmentLines: 2})

	for i := 1; i <= 3; i++ {
		if _, err := store.AppendItem("session-1", SessionItem{
			ID:         "item-" + string(rune('0'+i)),
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "message"},
		}); err != nil {
			t.Fatalf("AppendItem(%d) error = %v", i, err)
		}
	}

	segmentsDir := filepath.Join(root, "session-1", "segments")
	first := filepath.Join(segmentsDir, "000001.jsonl")
	second := filepath.Join(segmentsDir, "000002.jsonl")
	if got := mustCountLines(t, first); got != 2 {
		t.Fatalf("000001.jsonl lines = %d, want 2", got)
	}
	if got := mustCountLines(t, second); got != 1 {
		t.Fatalf("000002.jsonl lines = %d, want 1", got)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if replayed.LastSeq != 3 {
		t.Fatalf("LastSeq = %d, want 3", replayed.LastSeq)
	}
}

func TestV2StoreRejectsRecordThatWouldReplayAsTooLarge(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)

	payload := recordPayloadForMarshaledSize(t, maxJSONLRecordBytes)
	_, err := store.appendRecord("session-1", v2Record{
		Type:    RecordTypeActiveHistoryReplaced,
		ItemIDs: []string{payload},
	})
	if err == nil {
		t.Fatal("appendRecord() error = nil, want record too large")
	}
	if !strings.Contains(err.Error(), "is too large") {
		t.Fatalf("appendRecord() error = %q, want too large detail", err)
	}

	segmentsDir := filepath.Join(root, "session-1", "segments")
	if _, statErr := os.Stat(segmentsDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("segments dir stat error = %v, want not exist", statErr)
	}
}

func TestV2StoreActiveHistoryReplacedReplayUsesLatest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)

	appendTestItem(t, store, "session-1", "item-1", "one")
	appendTestItem(t, store, "session-1", "item-2", "two")
	appendTestItem(t, store, "session-1", "item-3", "three")
	if _, err := store.ReplaceActiveHistory("session-1", []string{"item-1", "item-2"}); err != nil {
		t.Fatalf("ReplaceActiveHistory(first) error = %v", err)
	}
	if _, err := store.ReplaceActiveHistory("session-1", []string{"item-3"}); err != nil {
		t.Fatalf("ReplaceActiveHistory(second) error = %v", err)
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if !reflect.DeepEqual(replayed.ActiveHistory, []string{"item-3"}) {
		t.Fatalf("ActiveHistory = %#v, want latest replacement", replayed.ActiveHistory)
	}
	if replayed.LastSeq != 5 {
		t.Fatalf("LastSeq = %d, want 5", replayed.LastSeq)
	}
}

func TestV2StoreCompactionCreatedReplay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	clock := &fakeClock{current: time.Date(2026, 7, 3, 4, 5, 6, 0, time.UTC)}
	store := newV2StoreWithClock(root, V2StoreOptions{}, clock.Now)

	checkpoint, err := store.AppendCompaction("session-1", CompactionCheckpoint{
		ID:                    "compact-1",
		Reason:                "user_requested",
		Phase:                 "manual",
		Trigger:               "manual",
		SummaryItemID:         "summary-1",
		PreviousActiveHistory: []string{"item-1", "item-2"},
		ReplacementHistory:    []string{"summary-1"},
		SummaryProvider:       "paperhub",
		SummaryModel:          "glm-5.2",
	})
	if err != nil {
		t.Fatalf("AppendCompaction() error = %v", err)
	}
	if checkpoint.CreatedAt.IsZero() {
		t.Fatal("checkpoint.CreatedAt is zero")
	}

	replayed, err := store.Replay("session-1")
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got, want := len(replayed.Compactions), 1; got != want {
		t.Fatalf("len(Compactions) = %d, want %d", got, want)
	}
	if !reflect.DeepEqual(replayed.Compactions[0], checkpoint) {
		t.Fatalf("Compactions[0] = %#v, want %#v", replayed.Compactions[0], checkpoint)
	}
}

func TestV2StoreBlobWriteDedupeMetadataAndReadByRef(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	raw := []byte("large tool result body")

	first, err := store.WriteBlob(raw, "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob(first) error = %v", err)
	}
	second, err := store.WriteBlob(raw, "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob(second) error = %v", err)
	}
	if first != second {
		t.Fatalf("second ref = %#v, want same as first %#v", second, first)
	}

	sum := sha256.Sum256(raw)
	wantHash := hex.EncodeToString(sum[:])
	if first.Hash != wantHash || first.SizeBytes != int64(len(raw)) || first.Encoding != "utf-8" || first.MediaType != "text/plain" {
		t.Fatalf("BlobRef = %#v, want hash/size/encoding/media metadata", first)
	}

	matches, err := filepath.Glob(filepath.Join(root, "blobs", "sha256", first.Hash[:2], "*.data"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("blob file count = %d, want 1: %#v", len(matches), matches)
	}

	read, err := store.ReadBlob(first)
	if err != nil {
		t.Fatalf("ReadBlob() error = %v", err)
	}
	if !bytes.Equal(read, raw) {
		t.Fatalf("ReadBlob() = %q, want %q", read, raw)
	}
}

func TestV2StoreWriteBlobRejectsExistingCorruptBlob(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store := NewV2Store(root)
	raw := []byte("large tool result body")

	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(root, "blobs", "sha256", hash[:2], hash+".data")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("truncated"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt blob) error = %v", err)
	}

	_, err := store.WriteBlob(raw, "utf-8", "text/plain")
	if err == nil {
		t.Fatal("WriteBlob() error = nil, want corrupt existing blob error")
	}
	if !strings.Contains(err.Error(), "size mismatch") && !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("WriteBlob() error = %q, want integrity detail", err)
	}

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(corrupt blob) error = %v", err)
	}
	if string(stored) != "truncated" {
		t.Fatalf("existing corrupt blob was overwritten: %q", stored)
	}
}

func TestV2SessionMaterializeActiveHistory(t *testing.T) {
	session := SessionV2{
		ID: "session-1",
		Items: []SessionItem{
			{
				ID:      "old-visible",
				Kind:    ItemKindMessage,
				Message: &model.Message{Role: model.MessageRoleUser, Content: "not active"},
			},
			{
				ID:   "active-user",
				Kind: ItemKindMessage,
				Message: &model.Message{
					Role:    model.MessageRoleUser,
					Content: "continue",
				},
			},
			{
				ID:   "active-assistant",
				Kind: ItemKindMessage,
				Message: &model.Message{
					Role:    model.MessageRoleAssistant,
					Content: "ok",
					ToolCalls: []model.ToolCall{
						{ID: "call-1", Name: "read_file", Arguments: `{"path":"README.md"}`},
					},
				},
			},
		},
		ActiveHistory: []string{"active-user", "active-assistant"},
	}

	messages, err := session.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	want := []model.Message{
		{Role: model.MessageRoleUser, Content: "continue"},
		{
			Role:    model.MessageRoleAssistant,
			Content: "ok",
			ToolCalls: []model.ToolCall{
				{ID: "call-1", Name: "read_file", Arguments: `{"path":"README.md"}`},
			},
		},
	}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("messages = %#v, want %#v", messages, want)
	}

	messages[1].ToolCalls[0].Name = "mutated"
	if session.Items[2].Message.ToolCalls[0].Name != "read_file" {
		t.Fatalf("MaterializeActiveHistory returned aliased ToolCalls: %#v", session.Items[2].Message.ToolCalls)
	}
}

func TestV2SessionMaterializeActiveHistoryCorruptedMissingRef(t *testing.T) {
	session := SessionV2{
		ID:            "session-1",
		ActiveHistory: []string{"missing"},
	}

	_, err := session.MaterializeActiveHistory()
	if !errors.Is(err, ErrCorruptedSession) {
		t.Fatalf("MaterializeActiveHistory() error = %v, want ErrCorruptedSession", err)
	}
	if !strings.Contains(err.Error(), `active history references missing item "missing"`) {
		t.Fatalf("MaterializeActiveHistory() error = %q, want missing ref detail", err)
	}
}

func TestV2SessionMaterializeActiveHistoryCorruptedNonMessageRef(t *testing.T) {
	session := SessionV2{
		ID: "session-1",
		Items: []SessionItem{
			{ID: "runtime-1", Kind: ItemKindRuntimeContext},
		},
		ActiveHistory: []string{"runtime-1"},
	}

	_, err := session.MaterializeActiveHistory()
	if !errors.Is(err, ErrCorruptedSession) {
		t.Fatalf("MaterializeActiveHistory() error = %v, want ErrCorruptedSession", err)
	}
	if !strings.Contains(err.Error(), `active history references item "runtime-1" without a message`) {
		t.Fatalf("MaterializeActiveHistory() error = %q, want no-message detail", err)
	}
}

func appendTestItem(t *testing.T, store *V2Store, sessionID, itemID, content string) {
	t.Helper()

	if _, err := store.AppendItem(sessionID, SessionItem{
		ID:         itemID,
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: content},
	}); err != nil {
		t.Fatalf("AppendItem(%q) error = %v", itemID, err)
	}
}

func mustCountLines(t *testing.T, path string) int {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return strings.Count(string(raw), "\n")
}

func recordPayloadForMarshaledSize(t *testing.T, target int) string {
	t.Helper()

	empty := v2Record{
		Seq:     1,
		Type:    RecordTypeActiveHistoryReplaced,
		ItemIDs: []string{""},
	}
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("Marshal(empty record) error = %v", err)
	}
	payloadLen := target - len(raw)
	if payloadLen < 0 {
		t.Fatalf("record overhead %d exceeds target %d", len(raw), target)
	}
	payload := strings.Repeat("a", payloadLen)
	withPayload := v2Record{
		Seq:     1,
		Type:    RecordTypeActiveHistoryReplaced,
		ItemIDs: []string{payload},
	}
	raw, err = json.Marshal(withPayload)
	if err != nil {
		t.Fatalf("Marshal(payload record) error = %v", err)
	}
	if len(raw) != target {
		t.Fatalf("payload record marshaled size = %d, want %d", len(raw), target)
	}
	return payload
}
