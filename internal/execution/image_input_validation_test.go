package execution

import (
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestValidateSessionMessageInputRejectsRemoteImageURL(t *testing.T) {
	service := &Service{}
	err := service.validateSessionMessageInput(sessions.SessionV2{}, SessionMessageInput{
		ContentBlocks: []model.InputContentBlock{{
			Type:     "input_image",
			ImageURL: "https://example.com/untrusted.png",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "base64 data URL") {
		t.Fatalf("validateSessionMessageInput() error = %v, want base64 data URL rejection", err)
	}
}
