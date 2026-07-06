package sessions

import (
	"reflect"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestSessionItemHelpers(t *testing.T) {
	existing := SessionItemIDs([]SessionItem{
		{ID: "msg-000001"},
		{ID: "msg-000002"},
		{ID: "msg-000003"},
	})
	if got := NextSessionItemID(existing, model.Message{Role: model.MessageRoleUser}); got != "msg-000004" {
		t.Fatalf("NextSessionItemID(user) = %q, want msg-000004", got)
	}
	if got := NextSessionItemID(existing, model.Message{Role: model.MessageRoleDeveloper}); got != "runtime-000004" {
		t.Fatalf("NextSessionItemID(developer) = %q, want runtime-000004", got)
	}

	message := model.Message{
		Role:    model.MessageRoleAssistant,
		Content: "answer",
		ToolCalls: []model.ToolCall{
			{ID: "call-1", Name: "read_file", Arguments: `{"path":"README.md"}`},
		},
	}
	item := SessionItemFromMessage("assistant-1", message)
	if item.Kind != ItemKindMessage || item.Visibility != ItemVisibilityVisible || item.Audience != ItemAudienceModel {
		t.Fatalf("assistant item metadata = kind %q visibility %q audience %q", item.Kind, item.Visibility, item.Audience)
	}
	if item.Message == nil || !reflect.DeepEqual(*item.Message, message) {
		t.Fatalf("assistant item message = %#v, want %#v", item.Message, message)
	}
	message.ToolCalls[0].Name = "mutated"
	if item.Message.ToolCalls[0].Name != "read_file" {
		t.Fatalf("SessionItemFromMessage aliased ToolCalls: %#v", item.Message.ToolCalls)
	}

	runtimeItem := SessionItemFromMessage("runtime-1", model.Message{Role: model.MessageRoleSystem, Content: "system"})
	if runtimeItem.Kind != ItemKindRuntimeContext || runtimeItem.Visibility != ItemVisibilityHidden || runtimeItem.Audience != ItemAudienceModel {
		t.Fatalf("runtime item metadata = kind %q visibility %q audience %q", runtimeItem.Kind, runtimeItem.Visibility, runtimeItem.Audience)
	}
	userItem := SessionItemFromMessage("user-1", model.Message{Role: model.MessageRoleUser, Content: "ask"})
	if userItem.Kind != ItemKindMessage || userItem.Visibility != ItemVisibilityVisible || userItem.Audience != ItemAudienceUser {
		t.Fatalf("user item metadata = kind %q visibility %q audience %q", userItem.Kind, userItem.Visibility, userItem.Audience)
	}
}
