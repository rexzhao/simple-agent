package sessions

import (
	"fmt"

	"github.com/rexzhao/simple-agent/internal/model"
)

func SessionItemIDs(items []SessionItem) map[string]struct{} {
	ids := make(map[string]struct{}, len(items))
	for _, item := range items {
		ids[item.ID] = struct{}{}
	}
	return ids
}

func NextSessionItemID(existing map[string]struct{}, message model.Message) string {
	prefix := "msg"
	if message.Role == model.MessageRoleSystem || message.Role == model.MessageRoleDeveloper {
		prefix = "runtime"
	}
	for i := len(existing) + 1; ; i++ {
		id := fmt.Sprintf("%s-%06d", prefix, i)
		if _, ok := existing[id]; !ok {
			return id
		}
	}
}

func SessionItemFromMessage(id string, message model.Message) SessionItem {
	messageCopy := copyMessage(message)
	item := SessionItem{
		ID:         id,
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Message:    &messageCopy,
	}
	switch message.Role {
	case model.MessageRoleSystem, model.MessageRoleDeveloper:
		item.Kind = ItemKindRuntimeContext
		item.Visibility = ItemVisibilityHidden
	case model.MessageRoleUser:
		item.Audience = ItemAudienceUser
	}
	return item
}
