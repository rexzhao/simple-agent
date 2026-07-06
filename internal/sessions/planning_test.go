package sessions

import (
	"reflect"
	"testing"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/model"
)

func TestRefreshRuntimeMetadataCopiesRuntimeFields(t *testing.T) {
	contextMetadata := contextwindow.Metadata{
		ContextWindow:     128000,
		LastRequestTokens: 42,
	}
	update := RuntimeMetadataUpdate{
		Provider:        "paperhub",
		ModelProfile:    "glm-fast",
		ModelID:         "glm-5.2",
		ModelParameters: map[string]any{"temperature": 0.2},
		CWD:             `F:\work\simple-agent`,
		ConfigPath:      `F:\work\simple-agent\.agents\sai.yaml`,
		EnabledTools:    []string{"read_file"},
		EnabledMCP:      []string{"local"},
		EnabledSkills:   []string{"review"},
		ShowReasoning:   true,
		InstructionsSnapshot: []model.Message{
			{
				Role:    model.MessageRoleDeveloper,
				Content: "follow rules",
				ToolCalls: []model.ToolCall{
					{ID: "call-1", Name: "ignored", Arguments: "{}"},
				},
			},
		},
		InstructionSources: []InstructionSource{
			{Role: model.MessageRoleDeveloper, Source: "config", Path: "AGENTS.md"},
		},
		Context:         &contextMetadata,
		SaveToolResults: true,
	}

	session := RefreshRuntimeMetadata(SessionV2{ID: "session-1", ConfigDir: "legacy-config-dir"}, update)
	if session.Provider != "paperhub" || session.ModelProfile != "glm-fast" || session.ModelID != "glm-5.2" {
		t.Fatalf("model metadata = provider %q profile %q model %q", session.Provider, session.ModelProfile, session.ModelID)
	}
	if session.ConfigDir != "" || session.ConfigPath != update.ConfigPath || session.CWD != update.CWD {
		t.Fatalf("path metadata = cwd %q config_path %q config_dir %q", session.CWD, session.ConfigPath, session.ConfigDir)
	}
	if !session.ShowReasoning || !session.SaveToolResults {
		t.Fatalf("flags = show_reasoning %v save_tool_results %v, want true/true", session.ShowReasoning, session.SaveToolResults)
	}
	if !reflect.DeepEqual(session.Context, contextMetadata) {
		t.Fatalf("Context = %#v, want %#v", session.Context, contextMetadata)
	}

	update.ModelParameters["temperature"] = 0.9
	update.EnabledTools[0] = "shell"
	update.EnabledMCP[0] = "mutated"
	update.EnabledSkills[0] = "mutated"
	update.InstructionsSnapshot[0].ToolCalls[0].Name = "mutated"
	update.InstructionSources[0].Path = "mutated"
	contextMetadata.LastRequestTokens = 99

	if session.ModelParameters["temperature"] != 0.2 {
		t.Fatalf("ModelParameters aliased update map: %#v", session.ModelParameters)
	}
	if !reflect.DeepEqual(session.EnabledTools, []string{"read_file"}) || !reflect.DeepEqual(session.EnabledMCP, []string{"local"}) || !reflect.DeepEqual(session.EnabledSkills, []string{"review"}) {
		t.Fatalf("enabled lists aliased update slices: tools=%#v mcp=%#v skills=%#v", session.EnabledTools, session.EnabledMCP, session.EnabledSkills)
	}
	if session.InstructionsSnapshot[0].ToolCalls[0].Name != "ignored" {
		t.Fatalf("InstructionsSnapshot aliased update messages: %#v", session.InstructionsSnapshot)
	}
	if session.InstructionSources[0].Path != "AGENTS.md" {
		t.Fatalf("InstructionSources aliased update sources: %#v", session.InstructionSources)
	}
	if session.Context.LastRequestTokens != 42 {
		t.Fatalf("Context aliased update metadata: %#v", session.Context)
	}
}

func TestRefreshRuntimeMetadataPreservesExistingInstructionSnapshots(t *testing.T) {
	update := RuntimeMetadataUpdate{
		InstructionsSnapshot: []model.Message{{Role: model.MessageRoleDeveloper, Content: "new"}},
		InstructionSources:   []InstructionSource{{Role: model.MessageRoleDeveloper, Source: "new"}},
	}
	session := RefreshRuntimeMetadata(SessionV2{
		InstructionsSnapshot: []model.Message{{Role: model.MessageRoleDeveloper, Content: "existing"}},
		InstructionSources:   []InstructionSource{{Role: model.MessageRoleDeveloper, Source: "existing"}},
	}, update)

	if got := session.InstructionsSnapshot[0].Content; got != "existing" {
		t.Fatalf("InstructionsSnapshot = %q, want existing", got)
	}
	if got := session.InstructionSources[0].Source; got != "existing" {
		t.Fatalf("InstructionSources = %q, want existing", got)
	}
}

func TestAppendMessagesToActiveHistory(t *testing.T) {
	existingItems := []SessionItem{
		{ID: "runtime-000001"},
		{ID: "msg-000002"},
	}
	activeItemIDs := []string{"runtime-000001"}
	messages := []model.Message{
		{Role: model.MessageRoleSystem, Content: "system"},
		{Role: model.MessageRoleUser, Content: "ask"},
		{Role: model.MessageRoleAssistant, Content: "answer"},
	}

	newItems, nextActive, err := AppendMessagesToActiveHistory(existingItems, activeItemIDs, messages)
	if err != nil {
		t.Fatalf("AppendMessagesToActiveHistory() error = %v", err)
	}
	if got := itemIDs(newItems); !reflect.DeepEqual(got, []string{"msg-000003", "msg-000004"}) {
		t.Fatalf("new item IDs = %#v, want msg-000003,msg-000004", got)
	}
	if !reflect.DeepEqual(nextActive, []string{"runtime-000001", "msg-000003", "msg-000004"}) {
		t.Fatalf("next active history = %#v", nextActive)
	}
	if newItems[0].Audience != ItemAudienceUser || newItems[1].Audience != ItemAudienceModel {
		t.Fatalf("new item audiences = %q/%q, want user/model", newItems[0].Audience, newItems[1].Audience)
	}

	activeItemIDs[0] = "mutated"
	if nextActive[0] != "runtime-000001" {
		t.Fatalf("next active history aliased input: %#v", nextActive)
	}
}

func TestAppendMessagesToActiveHistoryRejectsShortHistory(t *testing.T) {
	_, _, err := AppendMessagesToActiveHistory(nil, []string{"item-1", "item-2"}, []model.Message{{Role: model.MessageRoleUser, Content: "one"}})
	if err == nil {
		t.Fatal("AppendMessagesToActiveHistory() error = nil, want short history error")
	}
	if got, want := err.Error(), "updated message history shorter than active history"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
