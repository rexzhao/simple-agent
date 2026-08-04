package sessions

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestV2StorePersistsImageAttachmentsAsBlobsAndMaterializesThem(t *testing.T) {
	store := NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(SessionV2{})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}

	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	item, err := store.AppendItem(session.ID, SessionItem{
		ID:         "user-image",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message: &model.Message{
			Role: model.MessageRoleUser,
			ContentBlocks: []model.InputContentBlock{
				{Type: "input_image", ImageURL: model.ImageDataURL("image/png", raw), Detail: "auto"},
			},
		},
	})
	if err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}
	if item.Message == nil || len(item.Message.ContentBlocks) != 1 {
		t.Fatalf("stored item = %#v, want one image block", item)
	}
	storedBlock := item.Message.ContentBlocks[0]
	if storedBlock.ImageURL != "" || storedBlock.ImageBlob == nil {
		t.Fatalf("stored image block = %#v, want blob reference without data URL", storedBlock)
	}
	if storedBlock.ImageBlob.MediaType != "image/png" || storedBlock.ImageBlob.SizeBytes != int64(len(raw)) {
		t.Fatalf("stored image blob = %#v", storedBlock.ImageBlob)
	}

	if _, err := os.Stat(filepath.Join(store.root, session.ID, "session.db")); err != nil {
		t.Fatalf("session database stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, session.ID, "blobs")); err != nil {
		t.Fatalf("session blob directory stat error = %v", err)
	}

	if _, err := store.ReplaceActiveHistory(session.ID, []string{item.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	loaded, err := store.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	history, err := store.MaterializeActiveHistory(loaded)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if len(history) != 1 || len(history[0].ContentBlocks) != 1 {
		t.Fatalf("materialized history = %#v", history)
	}
	block := history[0].ContentBlocks[0]
	if block.ImageBlob != nil {
		t.Fatalf("materialized image block retained blob = %#v", block)
	}
	mediaType, got, err := model.ParseImageDataURL(block.ImageURL)
	if err != nil {
		t.Fatalf("ParseImageDataURL() error = %v", err)
	}
	if mediaType != "image/png" || !bytes.Equal(got, raw) {
		t.Fatalf("materialized image = %q %x, want image/png %x", mediaType, got, raw)
	}
}
