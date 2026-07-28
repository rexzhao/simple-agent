package execution

import (
	"errors"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestServiceCreatesAndReturnsSessionLineage(t *testing.T) {
	service, err := NewService(t.TempDir())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	project, err := service.CreateProject(mkdirProjectRoot(t, "lineage-project"), "Lineage")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	parent, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		DisplayName: "Parent",
		CreatedCWD:  project.Project.Root,
	})
	if err != nil {
		t.Fatalf("CreateSession(parent) error = %v", err)
	}
	if parent.CreatedBy != sessions.SessionCreatedByUser || parent.ParentSessionID != "" || parent.RootSessionID != parent.ID || parent.SpawnDepth != 0 {
		t.Fatalf("parent lineage = %#v, want user root", parent)
	}
	child, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		DisplayName:     "Child",
		ParentSessionID: parent.ID,
		CreatedCWD:      project.Project.Root,
	})
	if err != nil {
		t.Fatalf("CreateSession(child) error = %v", err)
	}
	grandchild, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		DisplayName:     "Grandchild",
		ParentSessionID: child.ID,
		CreatedCWD:      project.Project.Root,
	})
	if err != nil {
		t.Fatalf("CreateSession(grandchild) error = %v", err)
	}
	if child.CreatedBy != sessions.SessionCreatedByAgent || child.ParentSessionID != parent.ID || child.RootSessionID != parent.ID || child.SpawnDepth != 1 {
		t.Fatalf("child lineage = %#v", child)
	}
	if grandchild.ParentSessionID != child.ID || grandchild.RootSessionID != parent.ID || grandchild.SpawnDepth != 2 {
		t.Fatalf("grandchild lineage = %#v", grandchild)
	}

	listed, err := service.ListSessions(SessionListOptions{ProjectID: project.Project.ID})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	lineageByID := make(map[string]SessionMetadata, len(listed))
	for _, item := range listed {
		lineageByID[item.ID] = item
	}
	if got := lineageByID[grandchild.ID]; got.ParentSessionID != child.ID || got.RootSessionID != parent.ID || got.SpawnDepth != 2 {
		t.Fatalf("listed grandchild lineage = %#v", got)
	}
}

func TestServiceRejectsCrossProjectParentSession(t *testing.T) {
	service, err := NewService(t.TempDir())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	first, err := service.CreateProject(mkdirProjectRoot(t, "lineage-first"), "First")
	if err != nil {
		t.Fatalf("CreateProject(first) error = %v", err)
	}
	second, err := service.CreateProject(mkdirProjectRoot(t, "lineage-second"), "Second")
	if err != nil {
		t.Fatalf("CreateProject(second) error = %v", err)
	}
	parent, err := service.CreateSession(first.Project.ID, SessionCreateMetadata{CreatedCWD: first.Project.Root})
	if err != nil {
		t.Fatalf("CreateSession(parent) error = %v", err)
	}
	_, err = service.CreateSession(second.Project.ID, SessionCreateMetadata{
		ParentSessionID: parent.ID,
		CreatedCWD:      second.Project.Root,
	})
	if err == nil || !strings.Contains(err.Error(), "different project") {
		t.Fatalf("CreateSession(cross project parent) error = %v, want project rejection", err)
	}
}

func TestSearchSessionsUsesRE2CanonicalNamesAndProjectScope(t *testing.T) {
	service, err := NewService(t.TempDir())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	project, err := service.CreateProject(mkdirProjectRoot(t, "search-project"), "Search")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	other, err := service.CreateProject(mkdirProjectRoot(t, "search-other"), "Other")
	if err != nil {
		t.Fatalf("CreateProject(other) error = %v", err)
	}
	review, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{DisplayName: "Code Review", CreatedCWD: project.Project.Root})
	if err != nil {
		t.Fatalf("CreateSession(review) error = %v", err)
	}
	archived, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{DisplayName: "Archived Review", CreatedCWD: project.Project.Root})
	if err != nil {
		t.Fatalf("CreateSession(archived) error = %v", err)
	}
	if _, err := service.ArchiveSession(archived.ID); err != nil {
		t.Fatalf("ArchiveSession() error = %v", err)
	}
	if _, err := service.CreateSession(other.Project.ID, SessionCreateMetadata{DisplayName: "Other Review", CreatedCWD: other.Project.Root}); err != nil {
		t.Fatalf("CreateSession(other) error = %v", err)
	}

	result, err := service.SearchSessions(SessionSearchOptions{
		ProjectID: project.Project.ID,
		NameRegex: `(?i)review`,
	})
	if err != nil {
		t.Fatalf("SearchSessions(active) error = %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].ID != review.ID || result.Matches[0].Name != "Code Review" {
		t.Fatalf("active search = %#v, want only Code Review", result)
	}

	result, err = service.SearchSessions(SessionSearchOptions{
		ProjectID:       project.Project.ID,
		NameRegex:       `(?i)review`,
		IncludeArchived: true,
	})
	if err != nil {
		t.Fatalf("SearchSessions(all) error = %v", err)
	}
	if len(result.Matches) != 2 {
		t.Fatalf("all search matches = %#v, want active and archived in project", result.Matches)
	}
	if _, err := service.SearchSessions(SessionSearchOptions{ProjectID: project.Project.ID, NameRegex: `[`}); err == nil || !strings.Contains(err.Error(), "invalid session name regex") {
		t.Fatalf("SearchSessions(invalid regex) error = %v", err)
	}
	if _, err := service.SearchSessions(SessionSearchOptions{ProjectID: project.Project.ID, NameRegex: `.*`, Limit: maximumSessionSearchLimit + 1}); err == nil {
		t.Fatal("SearchSessions(over limit) error = nil")
	}
}

func TestInspectSessionReadsOnlyLatestPersistedAssistantItem(t *testing.T) {
	service, err := NewService(t.TempDir())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	project, err := service.CreateProject(mkdirProjectRoot(t, "inspect-project"), "Inspect")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	detail, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{CreatedCWD: project.Project.Root})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	if _, err := service.sessionStore.MarkTurnRunning(detail.ID, "turn-1"); err != nil {
		t.Fatalf("MarkTurnRunning() error = %v", err)
	}
	inspection, err := service.InspectSession(detail.ID, 0)
	if err != nil {
		t.Fatalf("InspectSession(no assistant) error = %v", err)
	}
	if inspection.Status != "running" || inspection.Output != nil {
		t.Fatalf("running inspection before assistant = %#v, want null output", inspection)
	}

	first := sessions.SessionItemFromMessage("assistant-first", model.Message{
		Role:             model.MessageRoleAssistant,
		Content:          "persisted answer",
		ReasoningContent: "hidden reasoning",
	})
	first.TurnID = "turn-1"
	first.AgentIteration = 1
	first, err = service.sessionStore.AppendItem(detail.ID, first)
	if err != nil {
		t.Fatalf("AppendItem(first assistant) error = %v", err)
	}
	tool := sessions.SessionItemFromMessage("tool-later", model.Message{Role: model.MessageRoleTool, Content: "secret tool result", ToolCallID: "call-1"})
	tool.TurnID = "turn-1"
	if _, err := service.sessionStore.AppendItem(detail.ID, tool); err != nil {
		t.Fatalf("AppendItem(tool) error = %v", err)
	}
	inspection, err = service.InspectSession(detail.ID, 0)
	if err != nil {
		t.Fatalf("InspectSession(intermediate) error = %v", err)
	}
	if inspection.Output == nil || inspection.Output.ItemID != first.ID || inspection.Output.Content != "persisted answer" || inspection.Output.Kind != SessionOutputIntermediate || inspection.Output.Complete {
		t.Fatalf("intermediate output = %#v", inspection.Output)
	}

	emptyToolCall := sessions.SessionItemFromMessage("assistant-tool-call", model.Message{
		Role:      model.MessageRoleAssistant,
		ToolCalls: []model.ToolCall{{ID: "call-2", Name: "read_file", Arguments: `{}`}},
	})
	emptyToolCall.TurnID = "turn-1"
	emptyToolCall.AgentIteration = 2
	emptyToolCall, err = service.sessionStore.AppendItem(detail.ID, emptyToolCall)
	if err != nil {
		t.Fatalf("AppendItem(tool-call assistant) error = %v", err)
	}
	inspection, err = service.InspectSession(detail.ID, 0)
	if err != nil {
		t.Fatalf("InspectSession(tool-call assistant) error = %v", err)
	}
	if inspection.Output == nil || inspection.Output.ItemID != emptyToolCall.ID || inspection.Output.Content != "" || !inspection.Output.HasToolCalls || inspection.Output.ToolCallCount != 1 {
		t.Fatalf("latest empty tool-call output = %#v", inspection.Output)
	}

	if _, err := service.sessionStore.ClearRunningTurn(detail.ID, "turn-1"); err != nil {
		t.Fatalf("ClearRunningTurn() error = %v", err)
	}
	inspection, err = service.InspectSession(detail.ID, 0)
	if err != nil {
		t.Fatalf("InspectSession(final) error = %v", err)
	}
	if inspection.Output == nil || inspection.Output.Kind != SessionOutputFinal || !inspection.Output.Complete || inspection.Output.ItemID != emptyToolCall.ID {
		t.Fatalf("final output = %#v", inspection.Output)
	}

	if _, err := service.sessionStore.MarkTurnRunning(detail.ID, "turn-2"); err != nil {
		t.Fatalf("MarkTurnRunning(turn-2) error = %v", err)
	}
	inspection, err = service.InspectSession(detail.ID, 0)
	if err != nil {
		t.Fatalf("InspectSession(new running turn) error = %v", err)
	}
	if inspection.Output != nil {
		t.Fatalf("new running turn output = %#v, want no fallback to prior final", inspection.Output)
	}
	partial := sessions.SessionItemFromMessage("assistant-partial", model.Message{Role: model.MessageRoleAssistant, Content: "abcdef"})
	partial.TurnID = "turn-2"
	partial, err = service.sessionStore.AppendItem(detail.ID, partial)
	if err != nil {
		t.Fatalf("AppendItem(partial) error = %v", err)
	}
	if _, err := service.sessionStore.MarkTurnInterrupted(detail.ID, "turn-2"); err != nil {
		t.Fatalf("MarkTurnInterrupted() error = %v", err)
	}
	inspection, err = service.InspectSession(detail.ID, 3)
	if err != nil {
		t.Fatalf("InspectSession(partial) error = %v", err)
	}
	if inspection.Status != "interrupted" || inspection.Output == nil || inspection.Output.Kind != SessionOutputPartial || inspection.Output.Complete || inspection.Output.Content != "abc" || !inspection.Output.Truncated {
		t.Fatalf("partial truncated output = %#v", inspection)
	}

	if _, err := service.InspectSession("missing", 0); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("InspectSession(missing) error = %v, want ErrNotFound", err)
	}
}
