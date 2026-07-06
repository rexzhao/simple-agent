package sessions

import (
	"fmt"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
)

type RuntimeMetadataUpdate struct {
	Provider             string
	ModelProfile         string
	ModelID              string
	ModelParameters      map[string]any
	CWD                  string
	ConfigPath           string
	EnabledTools         []string
	EnabledMCP           []string
	EnabledSkills        []string
	ShowReasoning        bool
	InstructionsSnapshot []model.Message
	InstructionSources   []InstructionSource
	Context              *contextwindow.Metadata
	SaveToolResults      bool
}

func RefreshRuntimeMetadata(session SessionV2, update RuntimeMetadataUpdate) SessionV2 {
	session.Provider = update.Provider
	session.ModelProfile = update.ModelProfile
	session.ModelID = update.ModelID
	session.ModelParameters = copyMap(update.ModelParameters)
	session.CWD = update.CWD
	session.ConfigPath = update.ConfigPath
	session.ConfigDir = ""
	session.EnabledTools = copyStrings(update.EnabledTools)
	session.EnabledMCP = copyStrings(update.EnabledMCP)
	session.EnabledSkills = copyStrings(update.EnabledSkills)
	session.ShowReasoning = update.ShowReasoning
	if len(session.InstructionsSnapshot) == 0 {
		session.InstructionsSnapshot = copyMessages(update.InstructionsSnapshot)
	}
	if len(session.InstructionSources) == 0 {
		session.InstructionSources = copyInstructionSources(update.InstructionSources)
	}
	if update.Context != nil {
		session.Context = *update.Context
	}
	session.SaveToolResults = update.SaveToolResults
	return session
}

func AppendMessagesToActiveHistory(existingItems []SessionItem, activeItemIDs []string, messages []model.Message) ([]SessionItem, []string, error) {
	if len(messages) < len(activeItemIDs) {
		return nil, nil, fmt.Errorf("updated message history shorter than active history")
	}

	existingIDs := SessionItemIDs(existingItems)
	nextActiveItemIDs := copyStrings(activeItemIDs)
	newMessages := messages[len(activeItemIDs):]
	newItems := make([]SessionItem, 0, len(newMessages))
	for _, message := range newMessages {
		itemID := NextSessionItemID(existingIDs, message)
		item := SessionItemFromMessage(itemID, message)
		existingIDs[itemID] = struct{}{}
		newItems = append(newItems, item)
		nextActiveItemIDs = append(nextActiveItemIDs, itemID)
	}

	return newItems, nextActiveItemIDs, nil
}
