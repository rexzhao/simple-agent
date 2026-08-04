package sessions

import (
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestV2StoreRejectsNonDurableImageURL(t *testing.T) {
	store := NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(SessionV2{})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}

	_, err = store.AppendItem(session.ID, SessionItem{
		ID:         "remote-image",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceUser,
		Message: &model.Message{
			Role: model.MessageRoleUser,
			ContentBlocks: []model.InputContentBlock{{
				Type:     "input_image",
				ImageURL: "https://example.com/image.png",
			}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "base64 data URL") {
		t.Fatalf("AppendItem() error = %v, want base64 data URL rejection", err)
	}

	loaded, err := store.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.Items) != 0 {
		t.Fatalf("session items = %#v, want no persisted remote image", loaded.Items)
	}
}

func TestMaterializeActiveHistoryRejectsLegacyRemoteImageURL(t *testing.T) {
	session := SessionV2{
		Items: []SessionItem{{
			ID:         "remote-image",
			Kind:       ItemKindMessage,
			Visibility: ItemVisibilityVisible,
			Audience:   ItemAudienceUser,
			Message: &model.Message{
				Role: model.MessageRoleUser,
				ContentBlocks: []model.InputContentBlock{{
					Type:     "input_image",
					ImageURL: "https://example.com/image.png",
				}},
			},
		}},
		ActiveHistory: []string{"remote-image"},
	}

	_, err := session.MaterializeActiveHistory()
	if err == nil || !strings.Contains(err.Error(), "base64 data URL") {
		t.Fatalf("MaterializeActiveHistory() error = %v, want base64 data URL rejection", err)
	}
}
