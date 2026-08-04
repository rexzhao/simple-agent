package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/contextwindow"
	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/model"
	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestServiceProjectLifecycleAndNearest(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	parent := mkdirProjectRoot(t, "parent")
	child := filepath.Join(parent, "child")
	leaf := filepath.Join(child, "leaf")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("MkdirAll(leaf) error = %v", err)
	}

	parentResult, err := service.CreateProject(parent, "Parent")
	if err != nil {
		t.Fatalf("CreateProject(parent) error = %v", err)
	}
	if !parentResult.Created {
		t.Fatal("CreateProject(parent) Created = false, want true")
	}
	childResult, err := service.CreateProject(child, "Child")
	if err != nil {
		t.Fatalf("CreateProject(child) error = %v", err)
	}

	nearest, ok, err := service.NearestProject(leaf, NearestProjectOptions{})
	if err != nil {
		t.Fatalf("NearestProject(active) error = %v", err)
	}
	if !ok || nearest.ID != childResult.Project.ID {
		t.Fatalf("NearestProject(active) = %#v/%t, want child", nearest, ok)
	}

	archived, err := service.ArchiveProject(childResult.Project.ID)
	if err != nil {
		t.Fatalf("ArchiveProject(child) error = %v", err)
	}
	if !archived.Archived {
		t.Fatalf("ArchiveProject(child) archived = false: %#v", archived)
	}
	nearest, ok, err = service.NearestProject(leaf, NearestProjectOptions{})
	if err != nil {
		t.Fatalf("NearestProject(active after archive) error = %v", err)
	}
	if !ok || nearest.ID != parentResult.Project.ID {
		t.Fatalf("NearestProject(active after archive) = %#v/%t, want parent", nearest, ok)
	}
	nearest, ok, err = service.NearestProject(leaf, NearestProjectOptions{IncludeArchived: true})
	if err != nil {
		t.Fatalf("NearestProject(include archived) error = %v", err)
	}
	if !ok || nearest.ID != childResult.Project.ID {
		t.Fatalf("NearestProject(include archived) = %#v/%t, want archived child", nearest, ok)
	}
}

func TestPublishCompactionUsagePublishesModelUsageEvent(t *testing.T) {
	bus := eventbus.NewBus(nil)
	defer bus.Close()
	events := bus.SubscribeLossless(1)
	usage := model.Usage{
		InputTokens: 100, OutputTokens: 80, TotalTokens: 1280,
		CachedTokens: 900, CacheWriteTokens: 200, ReasoningTokens: 64,
	}

	if err := publishCompactionUsage(bus, &usage); err != nil {
		t.Fatalf("publishCompactionUsage() error = %v", err)
	}
	event := <-events
	modelEvent, ok := event.(eventbus.ModelEvent)
	if !ok {
		t.Fatalf("event = %T, want eventbus.ModelEvent", event)
	}
	usageEvent, ok := modelEvent.Event.(model.UsageEvent)
	if !ok || usageEvent.Usage != usage {
		t.Fatalf("model event = %#v, want usage %#v", modelEvent.Event, usage)
	}
}

func TestServiceProjectLifecycleRules(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	root := mkdirProjectRoot(t, "repo")
	result, err := service.CreateProject(root, "Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}

	if _, err := service.RemoveProject(result.Project.ID); err == nil || !strings.Contains(err.Error(), "archive project before removing it") {
		t.Fatalf("RemoveProject(active) error = %v, want archive requirement", err)
	}

	if _, err := service.ArchiveProject(result.Project.ID); err != nil {
		t.Fatalf("ArchiveProject() error = %v", err)
	}
	if _, err := service.RenameProject(result.Project.ID, "Renamed"); err == nil || !strings.Contains(err.Error(), "archived project cannot be renamed") {
		t.Fatalf("RenameProject(archived) error = %v, want archived rejection", err)
	}
	restored, err := service.RestoreProject(result.Project.ID)
	if err != nil {
		t.Fatalf("RestoreProject() error = %v", err)
	}
	if restored.Archived {
		t.Fatalf("RestoreProject() archived = true: %#v", restored)
	}
	if _, err := service.RenameProject(result.Project.ID, "Restored"); err != nil {
		t.Fatalf("RenameProject(restored) error = %v", err)
	}
}

func TestServiceRemoveProjectDeletesProjectSessions(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "repo")
	otherRoot := mkdirProjectRoot(t, "other")
	project, err := service.CreateProject(projectRoot, "Repo")
	if err != nil {
		t.Fatalf("CreateProject(repo) error = %v", err)
	}
	other, err := service.CreateProject(otherRoot, "Other")
	if err != nil {
		t.Fatalf("CreateProject(other) error = %v", err)
	}
	sessionRoot, err := sessions.RootForHome(home)
	if err != nil {
		t.Fatalf("RootForHome(session) error = %v", err)
	}
	sessionStore := sessions.NewV2Store(sessionRoot)
	sessionA, err := sessionStore.SaveMetadata(sessions.SessionV2{ID: "session-a", ProjectID: project.Project.ID})
	if err != nil {
		t.Fatalf("SaveMetadata(session-a) error = %v", err)
	}
	sessionB, err := sessionStore.SaveMetadata(sessions.SessionV2{ID: "session-b", ProjectID: project.Project.ID})
	if err != nil {
		t.Fatalf("SaveMetadata(session-b) error = %v", err)
	}
	otherSession, err := sessionStore.SaveMetadata(sessions.SessionV2{ID: "session-other", ProjectID: other.Project.ID})
	if err != nil {
		t.Fatalf("SaveMetadata(session-other) error = %v", err)
	}

	if _, err := service.ArchiveProject(project.Project.ID); err != nil {
		t.Fatalf("ArchiveProject(repo) error = %v", err)
	}
	result, err := service.RemoveProject(project.Project.ID)
	if err != nil {
		t.Fatalf("RemoveProject(repo) error = %v", err)
	}
	if result.ID != project.Project.ID || result.Status != "removed" || result.RemovedSessions != 2 {
		t.Fatalf("RemoveProject(repo) = %#v, want removed 2 sessions", result)
	}
	for _, id := range []string{sessionA.ID, sessionB.ID} {
		if _, err := sessionStore.LoadExecutionState(id); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatalf("Load(%s) error = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := sessionStore.LoadExecutionState(otherSession.ID); err != nil {
		t.Fatalf("Load(other session) error = %v, want retained", err)
	}
	if _, err := service.GetProject(project.Project.ID); !errors.Is(err, projectstore.ErrNotFound) {
		t.Fatalf("GetProject(removed) error = %v, want ErrNotFound", err)
	}
}

func TestServiceProjectArchiveAndRemoveRejectRunningSession(t *testing.T) {
	service, err := NewService(t.TempDir())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	project, err := service.CreateProject(mkdirProjectRoot(t, "running-project"), "Running")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{CreatedCWD: project.Project.Root})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	stored, err := service.sessionStore.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("Load(session) error = %v", err)
	}
	stored.RunningTurnID = "turn-running"
	stored.RunningStartedAt = time.Now().UTC()
	if _, err := service.sessionStore.SaveMetadata(stored); err != nil {
		t.Fatalf("SaveMetadata(running session) error = %v", err)
	}
	if _, err := service.ArchiveProject(project.Project.ID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("ArchiveProject() error = %v, want ErrSessionBusy", err)
	}
	if _, err := service.projectStore.Archive(project.Project.ID); err != nil {
		t.Fatalf("projectStore.Archive() error = %v", err)
	}
	if _, err := service.RemoveProject(project.Project.ID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("RemoveProject() error = %v, want ErrSessionBusy", err)
	}
}

func TestServiceSessionFullAccessLifecycle(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "repo")
	project, err := service.CreateProject(projectRoot, "Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}

	session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		CreatedCWD:  project.Project.Root,
		ConfigPath:  filepath.Join(project.Project.Root, ".agents", "sai.yaml"),
		FullAccess:  true,
		DisplayName: "Parent",
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if !session.FullAccess {
		t.Fatalf("CreateSession(FullAccess) FullAccess = false: %#v", session)
	}
	// The flag round-trips through the store and the list metadata.
	loaded, err := service.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if !loaded.FullAccess {
		t.Fatalf("GetSession() FullAccess = false, want persisted true")
	}
	listed, err := service.ListSessions(SessionListOptions{ProjectID: project.Project.ID})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(listed) != 1 || !listed[0].FullAccess {
		t.Fatalf("ListSessions() = %#v, want one full access session", listed)
	}

	// Agent children inherit the parent's mode.
	child, err := service.CreateInheritedSession(session.ID, "Child")
	if err != nil {
		t.Fatalf("CreateInheritedSession() error = %v", err)
	}
	if !child.FullAccess {
		t.Fatalf("CreateInheritedSession() FullAccess = false, want inherited true")
	}

	// Runtime toggle applies to parent and child independently and persists.
	toggled, err := service.SetSessionFullAccess(child.ID, false)
	if err != nil {
		t.Fatalf("SetSessionFullAccess(child, false) error = %v", err)
	}
	if toggled.FullAccess {
		t.Fatalf("SetSessionFullAccess(child, false) FullAccess = true: %#v", toggled)
	}
	reloaded, err := service.GetSession(child.ID)
	if err != nil {
		t.Fatalf("GetSession(child) error = %v", err)
	}
	if reloaded.FullAccess {
		t.Fatalf("GetSession(child) FullAccess = true, want persisted false")
	}
	parentAfter, err := service.GetSession(session.ID)
	if err != nil {
		t.Fatalf("GetSession(parent) error = %v", err)
	}
	if !parentAfter.FullAccess {
		t.Fatalf("GetSession(parent) FullAccess = false, want unaffected true")
	}

	if _, err := service.ArchiveSession(session.ID); err != nil {
		t.Fatalf("ArchiveSession() error = %v", err)
	}
	if _, err := service.SetSessionFullAccess(session.ID, false); err == nil || !strings.Contains(err.Error(), "archived session") {
		t.Fatalf("SetSessionFullAccess(archived) error = %v, want archived rejection", err)
	}
}

func TestServiceSessionDebugLifecycle(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "repo")
	project, err := service.CreateProject(projectRoot, "Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	enabled := sessions.DebugSettings{RequestBodies: true}
	parent, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		CreatedCWD: project.Project.Root,
		ConfigPath: filepath.Join(project.Project.Root, ".agents", "sai.yaml"),
		Debug:      &enabled,
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if !parent.Debug.RequestBodies {
		t.Fatalf("CreateSession() Debug.RequestBodies = false, want true")
	}

	listed, err := service.ListSessions(SessionListOptions{ProjectID: project.Project.ID})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(listed) != 1 || !listed[0].Debug.RequestBodies {
		t.Fatalf("ListSessions() = %#v, want request-body capture enabled", listed)
	}
	reloaded, err := service.GetSession(parent.ID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if !reloaded.Debug.RequestBodies {
		t.Fatalf("GetSession() Debug.RequestBodies = false, want persisted true")
	}

	child, err := service.CreateInheritedSession(parent.ID, "Child")
	if err != nil {
		t.Fatalf("CreateInheritedSession() error = %v", err)
	}
	if !child.Debug.RequestBodies {
		t.Fatalf("CreateInheritedSession() Debug.RequestBodies = false, want inherited true")
	}
	toggled, err := service.SetSessionDebug(child.ID, sessions.DebugSettings{})
	if err != nil {
		t.Fatalf("SetSessionDebug(child, false) error = %v", err)
	}
	if toggled.Debug.RequestBodies {
		t.Fatalf("SetSessionDebug(child, false) Debug.RequestBodies = true")
	}
	parentAfter, err := service.GetSession(parent.ID)
	if err != nil {
		t.Fatalf("GetSession(parent) error = %v", err)
	}
	if !parentAfter.Debug.RequestBodies {
		t.Fatalf("SetSessionDebug(child, false) changed parent")
	}

	if _, err := service.ArchiveSession(parent.ID); err != nil {
		t.Fatalf("ArchiveSession() error = %v", err)
	}
	if _, err := service.SetSessionDebug(parent.ID, sessions.DebugSettings{}); err == nil || !strings.Contains(err.Error(), "archived session") {
		t.Fatalf("SetSessionDebug(archived) error = %v, want archived rejection", err)
	}
}

func TestServiceSessionLifecycle(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "repo")
	project, err := service.CreateProject(projectRoot, "Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	showReasoning := true
	saveToolResults := true
	contextMeta := contextwindow.Metadata{
		ContextWindow:           32000,
		ContextWindowSource:     string(contextwindow.WindowSourceEstimated),
		WarningThresholdPercent: contextwindow.WarningThresholdPercent,
	}

	session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		CreatedCWD:      project.Project.Root,
		ConfigPath:      filepath.Join(project.Project.Root, ".agents", "sai.yaml"),
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		ReasoningLevel:  "high",
		ModelParameters: map[string]any{"max_tokens": float64(128)},
		EnabledTools:    []string{"read_file"},
		EnabledMCP:      []string{"local"},
		EnabledSkills:   []string{"visible"},
		ShowReasoning:   &showReasoning,
		Context:         &contextMeta,
		SaveToolResults: &saveToolResults,
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if session.ID == "" || session.ProjectID != project.Project.ID || session.CreatedCWD != project.Project.Root {
		t.Fatalf("CreateSession() = %#v, want project metadata", session)
	}
	if session.Provider != "fake" || session.ModelProfile != "default" || session.ModelID != "model-default" {
		t.Fatalf("CreateSession() model metadata = %#v", session)
	}
	if session.ReasoningLevel != "high" {
		t.Fatalf("CreateSession() ReasoningLevel = %q, want high", session.ReasoningLevel)
	}
	if got := session.ModelParameters["max_tokens"]; got != float64(128) {
		t.Fatalf("CreateSession() model max_tokens = %#v, want 128", got)
	}
	if !session.ShowReasoning || !session.SaveToolResults || session.Context.ContextWindow != 32000 {
		t.Fatalf("CreateSession() runtime metadata = %#v", session)
	}

	renamed, err := service.RenameSession(session.ID, "Renamed Session")
	if err != nil {
		t.Fatalf("RenameSession() error = %v", err)
	}
	if renamed.DisplayName != "Renamed Session" {
		t.Fatalf("RenameSession() DisplayName = %q, want Renamed Session", renamed.DisplayName)
	}
	archived, err := service.ArchiveSession(session.ID)
	if err != nil {
		t.Fatalf("ArchiveSession() error = %v", err)
	}
	if !archived.Archived {
		t.Fatalf("ArchiveSession() archived = false: %#v", archived)
	}
	if _, err := service.RenameSession(session.ID, "Nope"); err == nil || !strings.Contains(err.Error(), "archived session cannot be renamed") {
		t.Fatalf("RenameSession(archived) error = %v, want archived rejection", err)
	}
	restored, err := service.RestoreSession(session.ID)
	if err != nil {
		t.Fatalf("RestoreSession() error = %v", err)
	}
	if restored.Archived {
		t.Fatalf("RestoreSession() archived = true: %#v", restored)
	}
	if _, err := service.RenameSession(session.ID, "Restored Session"); err != nil {
		t.Fatalf("RenameSession(restored) error = %v", err)
	}
	if _, err := service.ArchiveSession(session.ID); err != nil {
		t.Fatalf("ArchiveSession(restored) error = %v", err)
	}
	result, err := service.RemoveSession(session.ID)
	if err != nil {
		t.Fatalf("RemoveSession() error = %v", err)
	}
	if result.Status != "removed" || result.ID != session.ID {
		t.Fatalf("RemoveSession() = %#v, want removed id", result)
	}
	if _, err := service.GetSession(session.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("GetSession(removed) error = %v, want ErrNotFound", err)
	}
}

func TestServiceCreateConfiguredSessionResolvesServerRootConfig(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	root := mkdirProjectRoot(t, "configured")
	projectAgentsDir := filepath.Join(root, ".agents")
	if err := os.MkdirAll(projectAgentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(project .agents) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectAgentsDir, "sai.yaml"), []byte(": invalid project config\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(project config) error = %v", err)
	}
	providersDir := filepath.Join(home, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(providers) error = %v", err)
	}
	rootConfig := `default_provider: fake
default_model: fast
provider_dir: providers
agent:
  show_reasoning: true
tools:
  enabled: [read_file]
`
	providerConfig := `name: fake
base_url: http://127.0.0.1:1/v1
api_key: test-key
models:
  fast:
    id: fake-model
    context_window: 64000
    temperature: 0.2
  precise:
    id: fake-precise
    context_window: 128000
    reasoning_config:
      parameter: reasoning_effort
      default: high
      levels:
        low: low
        high: high
`
	if err := os.WriteFile(filepath.Join(home, "sai.yaml"), []byte(rootConfig), 0o600); err != nil {
		t.Fatalf("WriteFile(root config) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(providersDir, "fake.yaml"), []byte(providerConfig), 0o600); err != nil {
		t.Fatalf("WriteFile(provider config) error = %v", err)
	}
	project, err := service.CreateProject(root, "Configured")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	modelOptions, err := service.ConfiguredSessionModels(project.Project.ID)
	if err != nil {
		t.Fatalf("ConfiguredSessionModels() error = %v", err)
	}
	if modelOptions.DefaultProvider != "fake" || modelOptions.DefaultModel != "fast" || len(modelOptions.Models) != 2 {
		t.Fatalf("ConfiguredSessionModels() = %#v", modelOptions)
	}
	if modelOptions.Models[1].Provider != "fake" || modelOptions.Models[1].ModelProfile != "precise" || modelOptions.Models[1].ModelID != "fake-precise" {
		t.Fatalf("ConfiguredSessionModels()[1] = %#v", modelOptions.Models[1])
	}

	session, err := service.CreateConfiguredSession(project.Project.ID, ConfiguredSessionOptions{})
	if err != nil {
		t.Fatalf("CreateConfiguredSession() error = %v", err)
	}
	if session.ProjectID != project.Project.ID || session.CreatedCWD != project.Project.Root {
		t.Fatalf("session project/cwd = %q/%q, want %q/%q", session.ProjectID, session.CreatedCWD, project.Project.ID, project.Project.Root)
	}
	if session.ConfigPath != filepath.Join(home, "sai.yaml") {
		t.Fatalf("session config path = %q, want server-root config", session.ConfigPath)
	}
	if session.Provider != "fake" || session.ModelProfile != "fast" || session.ModelID != "fake-model" {
		t.Fatalf("session model = %q/%q/%q", session.Provider, session.ModelProfile, session.ModelID)
	}
	if !session.ShowReasoning || !session.SaveToolResults {
		t.Fatalf("session flags = reasoning %t save tools %t", session.ShowReasoning, session.SaveToolResults)
	}
	if len(session.EnabledTools) != 1 || session.EnabledTools[0] != "read_file" {
		t.Fatalf("EnabledTools = %#v", session.EnabledTools)
	}
	if session.Context.ContextWindow != 64000 || session.Context.ContextWindowSource != string(contextwindow.WindowSourceConfigured) {
		t.Fatalf("context metadata = %#v", session.Context)
	}
	if session.ReasoningLevel != "" {
		t.Fatalf("session reasoning level = %q, want empty for a model without reasoning config", session.ReasoningLevel)
	}
	selected, err := service.CreateConfiguredSession(project.Project.ID, ConfiguredSessionOptions{Provider: "fake", ModelProfile: "precise", ReasoningLevel: "low"})
	if err != nil {
		t.Fatalf("CreateConfiguredSession(selected model) error = %v", err)
	}
	if selected.Provider != "fake" || selected.ModelProfile != "precise" || selected.ModelID != "fake-precise" || selected.Context.ContextWindow != 128000 {
		t.Fatalf("selected session model = %#v", selected)
	}
	if selected.ReasoningLevel != "low" || selected.ModelParameters["reasoning_effort"] != "low" {
		t.Fatalf("selected session reasoning = level %q parameters %#v, want low", selected.ReasoningLevel, selected.ModelParameters)
	}
	defaulted, err := service.CreateConfiguredSession(project.Project.ID, ConfiguredSessionOptions{Provider: "fake", ModelProfile: "precise"})
	if err != nil {
		t.Fatalf("CreateConfiguredSession(default reasoning) error = %v", err)
	}
	if defaulted.ReasoningLevel != "high" || defaulted.ModelParameters["reasoning_effort"] != "high" {
		t.Fatalf("defaulted session reasoning = level %q parameters %#v, want high", defaulted.ReasoningLevel, defaulted.ModelParameters)
	}
	reloaded, err := service.GetSession(selected.ID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if reloaded.ReasoningLevel != "low" {
		t.Fatalf("reloaded ReasoningLevel = %q, want low", reloaded.ReasoningLevel)
	}
}

func TestServiceCreateConfiguredSessionRejectsOutsideProjectCWD(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	root := mkdirProjectRoot(t, "configured-boundary")
	project, err := service.CreateProject(root, "Configured")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	_, err = service.CreateConfiguredSession(project.Project.ID, ConfiguredSessionOptions{CWD: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Fatalf("CreateConfiguredSession(outside) error = %v", err)
	}
}

func TestServiceSessionListScopesAndArchivedFilter(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	firstRoot := mkdirProjectRoot(t, "first")
	secondRoot := mkdirProjectRoot(t, "second")
	first, err := service.CreateProject(firstRoot, "First")
	if err != nil {
		t.Fatalf("CreateProject(first) error = %v", err)
	}
	second, err := service.CreateProject(secondRoot, "Second")
	if err != nil {
		t.Fatalf("CreateProject(second) error = %v", err)
	}
	activeFirst, err := service.CreateSession(first.Project.ID, SessionCreateMetadata{CreatedCWD: first.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatalf("CreateSession(active first) error = %v", err)
	}
	archivedFirst, err := service.CreateSession(first.Project.ID, SessionCreateMetadata{CreatedCWD: first.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatalf("CreateSession(archived first) error = %v", err)
	}
	if _, err := service.ArchiveSession(archivedFirst.ID); err != nil {
		t.Fatalf("ArchiveSession(first) error = %v", err)
	}
	activeSecond, err := service.CreateSession(second.Project.ID, SessionCreateMetadata{CreatedCWD: second.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatalf("CreateSession(active second) error = %v", err)
	}

	firstActive, err := service.ListSessions(SessionListOptions{ProjectID: first.Project.ID})
	if err != nil {
		t.Fatalf("ListSessions(first active) error = %v", err)
	}
	if got := sessionMetadataIDs(firstActive); !sameStringSlice(got, []string{activeFirst.ID}) {
		t.Fatalf("ListSessions(first active) = %#v, want active first", got)
	}
	firstArchived, err := service.ListSessions(SessionListOptions{ProjectID: first.Project.ID, Archived: true})
	if err != nil {
		t.Fatalf("ListSessions(first archived) error = %v", err)
	}
	if got := sessionMetadataIDs(firstArchived); !sameStringSlice(got, []string{archivedFirst.ID}) {
		t.Fatalf("ListSessions(first archived) = %#v, want archived first", got)
	}
	allActive, err := service.ListSessions(SessionListOptions{AllProjects: true})
	if err != nil {
		t.Fatalf("ListSessions(all active) error = %v", err)
	}
	if got := sessionMetadataIDs(allActive); !sameStringSet(got, []string{activeFirst.ID, activeSecond.ID}) {
		t.Fatalf("ListSessions(all active) = %#v, want both active sessions", got)
	}
	allArchived, err := service.ListSessions(SessionListOptions{AllProjects: true, Archived: true})
	if err != nil {
		t.Fatalf("ListSessions(all archived) error = %v", err)
	}
	if got := sessionMetadataIDs(allArchived); !sameStringSlice(got, []string{archivedFirst.ID}) {
		t.Fatalf("ListSessions(all archived) = %#v, want only archived sessions", got)
	}
}

func TestServiceSessionListRejectsMissingOrArchivedProject(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "repo")
	project, err := service.CreateProject(projectRoot, "Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	if _, err := service.ArchiveProject(project.Project.ID); err != nil {
		t.Fatalf("ArchiveProject() error = %v", err)
	}
	if _, err := service.ListSessions(SessionListOptions{ProjectID: project.Project.ID}); err == nil || !strings.Contains(err.Error(), "project is archived") {
		t.Fatalf("ListSessions(archived project) error = %v, want archived rejection", err)
	}
	if _, err := service.ListSessions(SessionListOptions{ProjectID: "project-missing"}); !errors.Is(err, projectstore.ErrNotFound) {
		t.Fatalf("ListSessions(missing project) error = %v, want ErrNotFound", err)
	}
}

func TestServiceSessionStatusPrioritizesRunningTurn(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "repo")
	project, err := service.CreateProject(projectRoot, "Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	sessionStoreRoot, err := sessions.RootForHome(home)
	if err != nil {
		t.Fatalf("RootForHome(session) error = %v", err)
	}
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	sessionStore := sessions.NewV2Store(sessionStoreRoot)
	saved, err := sessionStore.SaveMetadata(sessions.SessionV2{
		ID:                "status-session",
		ProjectID:         project.Project.ID,
		RunningTurnID:     "stale-running-turn",
		InterruptedTurnID: "interrupted-turn",
		InterruptedAt:     now,
		LastUsedAt:        now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("SaveMetadata(status session) error = %v", err)
	}
	detail, err := service.GetSession(saved.ID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if detail.Status != "running" {
		t.Fatalf("GetSession() Status = %q, want running", detail.Status)
	}
	listed, err := service.ListSessions(SessionListOptions{ProjectID: project.Project.ID})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(listed) != 1 || listed[0].Status != "running" {
		t.Fatalf("ListSessions() = %#v, want one running session", listed)
	}
	if _, err := sessionStore.ClearRunningTurn(saved.ID, saved.RunningTurnID); err != nil {
		t.Fatalf("ClearRunningTurn() error = %v", err)
	}
	detail, err = service.GetSession(saved.ID)
	if err != nil {
		t.Fatalf("GetSession() after clear error = %v", err)
	}
	if detail.Status != "interrupted" {
		t.Fatalf("GetSession() Status after clear = %q, want interrupted", detail.Status)
	}
}

func TestServiceRejectsArchiveAndRemoveForRunningSession(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	project, err := service.CreateProject(mkdirProjectRoot(t, "repo"), "Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{CreatedCWD: project.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	stored, err := service.sessionStore.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	stored.RunningTurnID = "turn-000001"
	stored.Archived = true
	if _, err := service.sessionStore.SaveMetadata(stored); err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	if _, err := service.ArchiveSession(session.ID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("ArchiveSession() error = %v, want ErrSessionBusy", err)
	}
	if _, err := service.RemoveSession(session.ID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("RemoveSession() error = %v, want ErrSessionBusy", err)
	}
}

func TestServiceSendSessionMessagePersistsSuccessfulTurn(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			if request.Session.ID == "" || request.SessionStore == nil || request.TurnID != "turn-000001" {
				t.Fatalf("RunSessionTurn request = %#v, want session/store/turn", request)
			}
			if request.Content != "hello execution" {
				t.Fatalf("RunSessionTurn content = %q, want hello execution", request.Content)
			}
			if request.Publisher == nil {
				t.Fatal("RunSessionTurn Publisher = nil, want bus")
			}
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "execution answer")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, project, session := newExecutionServiceWithSession(t, home, runner)

	result, err := service.SendSessionMessage(context.Background(), session.ID, "hello execution")
	if err != nil {
		t.Fatalf("SendSessionMessage() error = %v", err)
	}
	if result.Status != "committed" || result.TurnID != "turn-000001" || result.LastSeq == 0 {
		t.Fatalf("SendSessionMessage() = %#v, want committed turn with last seq", result)
	}
	loaded, err := service.sessionStore.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("Load(session) error = %v", err)
	}
	if result.LastSeq != loaded.LastSeq {
		t.Fatalf("SendSessionMessage() LastSeq = %d, stored LastSeq = %d", result.LastSeq, loaded.LastSeq)
	}
	if loaded.ProjectID != project.Project.ID || loaded.RunningTurnID != "" {
		t.Fatalf("loaded session metadata = %#v, want project and no running turn", loaded)
	}
	messages, err := loaded.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !sameStringSlice(got, []string{"hello execution", "execution answer"}) {
		t.Fatalf("active messages = %#v, want prompt and answer", got)
	}
}

func TestServiceGetSessionChatItemsFiltersItemBackedVisibleMessages(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: true})
	blob, err := service.sessionStore.WriteBlobForSession(session.ID, []byte("full blob-backed assistant response"), "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob() error = %v", err)
	}
	_, err = service.sessionStore.AppendItemsAndReplaceActiveHistory(session.ID, []sessions.SessionItem{
		{
			ID:         "visible-user",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "hello"},
		},
		{
			ID:             "visible-assistant-preview",
			AgentIteration: 2,
			Kind:           sessions.ItemKindMessage,
			Visibility:     sessions.ItemVisibilityVisible,
			Audience:       sessions.ItemAudienceModel,
			Message: &model.Message{
				Role:      model.MessageRoleAssistant,
				ToolCalls: []model.ToolCall{{ID: "call-1", Name: "read_file", Arguments: `{"path":"notes.txt"}`}},
			},
			Content: &sessions.StoredContent{Preview: "long answer preview"},
		},
		{
			ID:         "visible-assistant-blob",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant},
			Content:    &sessions.StoredContent{Blob: &blob},
		},
		{
			ID:         "hidden-summary",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityHidden,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "hidden secret"},
		},
		{
			ID:         "debug-user",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityDebug,
			Audience:   sessions.ItemAudienceInternal,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "debug secret"},
		},
		{
			ID:             "tool-result",
			AgentIteration: 2,
			Kind:           sessions.ItemKindMessage,
			Visibility:     sessions.ItemVisibilityVisible,
			Audience:       sessions.ItemAudienceModel,
			Message:        &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: "tool secret"},
		},
	}, []string{"visible-user", "visible-assistant-preview", "visible-assistant-blob", "hidden-summary", "debug-user", "tool-result"})
	if err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory() error = %v", err)
	}

	page, err := service.GetSessionChatItems(session.ID)
	if err != nil {
		t.Fatalf("GetSessionChatItems() error = %v", err)
	}
	if got := executionSessionItemIDs(page.Items); !sameStringSlice(got, []string{"visible-user", "visible-assistant-preview", "visible-assistant-blob", "tool-result"}) {
		t.Fatalf("chat item IDs = %#v, want visible conversation and process items", got)
	}
	if page.Items[0].Message == nil || page.Items[0].Message.Content == nil || page.Items[0].Message.Content.Inline != "hello" {
		t.Fatalf("user item DTO = %#v, want inline content", page.Items[0])
	}
	if page.Items[1].Message == nil || page.Items[1].Message.Content == nil || page.Items[1].Message.Content.Preview != "long answer preview" {
		t.Fatalf("assistant item DTO = %#v, want preview content", page.Items[1])
	}
	if page.Items[2].Message == nil || page.Items[2].Message.Content == nil || page.Items[2].Message.Content.Inline != "full blob-backed assistant response" {
		t.Fatalf("blob-backed assistant item DTO = %#v, want full content for the authenticated chat view", page.Items[2])
	}
	if calls := page.Items[1].Message.ToolCalls; len(calls) != 1 || calls[0].Name != "read_file" || calls[0].Arguments != `{"path":"notes.txt"}` {
		t.Fatalf("assistant tool calls = %#v, want read_file arguments", calls)
	}
	if page.Items[1].AgentIteration != 2 || page.Items[3].AgentIteration != 2 {
		t.Fatalf("agent iterations = assistant %d tool %d, want 2", page.Items[1].AgentIteration, page.Items[3].AgentIteration)
	}
	if tool := page.Items[3]; tool.Message == nil || tool.Message.ToolCallID != "call-1" || tool.Message.Content == nil || tool.Message.Content.Inline != "tool secret" {
		t.Fatalf("tool result DTO = %#v, want call id and content", tool)
	}
	if page.OldestSeq == 0 || page.NewestSeq <= page.OldestSeq || page.HasMoreBefore || page.HasMoreAfter {
		t.Fatalf("page bounds = %#v, want bounded recent page without more flags", page)
	}
}

func TestServiceGetSessionTurnFinalAssistantOutputMaterializesLastVisibleAssistant(t *testing.T) {
	home := t.TempDir()
	service, _, session := newExecutionServiceWithSession(t, home, fakeExecutionTurnRunner{supports: true})
	fullAnswer := strings.Repeat("final answer body ", 400) + "FINAL-SUFFIX"
	blob, err := service.sessionStore.WriteBlobForSession(session.ID, []byte(fullAnswer), "utf-8", "text/plain")
	if err != nil {
		t.Fatalf("WriteBlob() error = %v", err)
	}
	_, err = service.sessionStore.AppendItemsAndReplaceActiveHistory(session.ID, []sessions.SessionItem{
		{
			ID:         "turn-user",
			TurnID:     "turn-1",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "review this"},
		},
		{
			ID:         "turn-prelude",
			TurnID:     "turn-1",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "checking the diff first"},
		},
		{
			ID:         "turn-tool",
			TurnID:     "turn-1",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: "tool result secret"},
		},
		{
			ID:         "turn-final",
			TurnID:     "turn-1",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant},
			Content:    &sessions.StoredContent{Blob: &blob, Preview: "truncated preview"},
		},
		{
			ID:         "turn-hidden",
			TurnID:     "turn-1",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityHidden,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "hidden assistant"},
		},
		{
			ID:         "other-turn",
			TurnID:     "turn-2",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "other turn"},
		},
	}, []string{"turn-user", "turn-prelude", "turn-tool", "turn-final", "turn-hidden", "other-turn"})
	if err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory() error = %v", err)
	}

	got, err := service.GetSessionTurnFinalAssistantOutput(session.ID, "turn-1")
	if err != nil {
		t.Fatalf("GetSessionTurnFinalAssistantOutput() error = %v", err)
	}
	if got != fullAnswer {
		t.Fatalf("final assistant output = %q, want full blob-backed answer", got)
	}
}

func TestServiceSendSessionMessageWithEventsEmitsDirectStreamEvents(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			request.Emit(model.TextDeltaEvent{Text: "streamed"})
			request.Emit(model.ToolCallDoneEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "read_file", Arguments: `{"path":"notes.txt"}`}})
			request.Emit(model.ToolStartedEvent{ToolCall: model.ToolCall{ID: "call-1", Name: "read_file", Arguments: `{"path":"notes.txt"}`}})
			request.Emit(model.ToolResultEvent{Result: model.ToolResult{ToolCallID: "call-1", Name: "read_file", Content: "file contents"}})
			request.Emit(model.UsageEvent{Usage: model.Usage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18}})
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)
	var events []SessionStreamEvent

	result, err := service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", func(event SessionStreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("SendSessionMessageWithEvents() error = %v", err)
	}
	if result.Status != "committed" || result.TurnID != "turn-000001" || result.LastSeq == 0 {
		t.Fatalf("SendSessionMessageWithEvents() = %#v, want committed result", result)
	}
	types := sessionStreamEventTypes(events)
	if len(types) < 7 || types[0] != "turn.started" || types[len(types)-1] != "turn.committed" {
		t.Fatalf("event types = %#v, want turn.started first and turn.committed last", types)
	}
	for _, want := range []string{"item.appended", "text.delta", "tool.requested", "tool.started", "tool.finished", "usage.updated"} {
		if !stringSliceContains(types, want) {
			t.Fatalf("event types = %#v, want contain %q", types, want)
		}
	}
	if got := countString(types, "item.appended"); got != 2 {
		t.Fatalf("item.appended count = %d, want user and assistant items", got)
	}
	if !sessionStreamEventsContain(events, "text.delta", "text", "streamed") {
		t.Fatalf("events = %#v, want streamed text delta", events)
	}
	if !sessionStreamEventsContain(events, "usage.updated", "total_tokens", 18) {
		t.Fatalf("events = %#v, want usage.updated total_tokens", events)
	}
	if !sessionStreamEventsContain(events, "tool.requested", "arguments", `{"path":"notes.txt"}`) {
		t.Fatalf("events = %#v, want tool arguments", events)
	}
	if !sessionStreamEventsContain(events, "tool.finished", "content", "file contents") {
		t.Fatalf("events = %#v, want tool result content", events)
	}
}

func TestServiceSendSessionMessageWithEventsReasoningFollowsSessionSetting(t *testing.T) {
	for _, tt := range []struct {
		name          string
		showReasoning bool
		wantReasoning bool
	}{
		{name: "hidden by default", showReasoning: false, wantReasoning: false},
		{name: "shown when enabled", showReasoning: true, wantReasoning: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			runner := fakeExecutionTurnRunner{
				supports: true,
				run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
					request.Emit(model.ReasoningDeltaEvent{Text: "thinking"})
					if err := request.Publisher.Publish(eventAssistant(request.TurnID, "answer")); err != nil {
						return SessionTurnResult{}, err
					}
					return SessionTurnResult{Incremental: true}, nil
				},
			}
			service, err := NewServiceWithOptions(home, ServiceOptions{TurnRunner: runner})
			if err != nil {
				t.Fatalf("NewServiceWithOptions() error = %v", err)
			}
			projectRoot := mkdirProjectRoot(t, "reasoning-repo")
			project, err := service.CreateProject(projectRoot, "Reasoning Repo")
			if err != nil {
				t.Fatalf("CreateProject() error = %v", err)
			}
			saveToolResults := true
			session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
				CreatedCWD:      project.Project.Root,
				ConfigPath:      filepath.Join(project.Project.Root, ".agents", "sai.yaml"),
				Provider:        "fake",
				ModelProfile:    "default",
				ModelID:         "model-default",
				ShowReasoning:   &tt.showReasoning,
				SaveToolResults: &saveToolResults,
			})
			if err != nil {
				t.Fatalf("CreateSession() error = %v", err)
			}

			var events []SessionStreamEvent
			_, err = service.SendSessionMessageWithEvents(context.Background(), session.ID, "hello", func(event SessionStreamEvent) {
				events = append(events, event)
			})
			if err != nil {
				t.Fatalf("SendSessionMessageWithEvents() error = %v", err)
			}
			gotReasoning := sessionStreamEventsContain(events, "reasoning.delta", "text", "thinking")
			if gotReasoning != tt.wantReasoning {
				t.Fatalf("reasoning.delta present = %t, want %t; events = %#v", gotReasoning, tt.wantReasoning, events)
			}
		})
	}
}

func TestServiceCompactSessionUsesConfiguredPlanner(t *testing.T) {
	home := t.TempDir()
	called := false
	planner := fakeExecutionCompactPlanner{
		plan: func(ctx context.Context, request SessionCompactionRequest) (SessionCompactionResult, error) {
			called = true
			if request.SessionStore == nil || !sameStringSlice(request.Session.ActiveHistory, []string{"old-user", "old-assistant"}) {
				t.Fatalf("PlanSessionCompaction request = %#v, want active history and store", request)
			}
			return SessionCompactionResult{Compaction: SessionCompactionPlan{
				SummaryItem: sessions.SessionItem{
					ID:         "manual-summary",
					Kind:       sessions.ItemKindMessage,
					Visibility: sessions.ItemVisibilityHidden,
					Audience:   sessions.ItemAudienceModel,
					Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "manual summary"},
				},
				Checkpoint: sessions.CompactionCheckpoint{
					ID:                    "manual-checkpoint",
					Reason:                "manual",
					Phase:                 "manual",
					Trigger:               "manual",
					SummaryItemID:         "manual-summary",
					PreviousActiveHistory: request.Session.ActiveHistory,
					ReplacementHistory:    []string{"manual-summary"},
				},
			}}, nil
		},
	}
	service, err := NewServiceWithOptions(home, ServiceOptions{
		TurnRunner:     fakeExecutionTurnRunner{supports: true},
		CompactPlanner: planner,
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "compact-repo")
	project, err := service.CreateProject(projectRoot, "Compact Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{CreatedCWD: project.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model-default"})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if _, err := service.sessionStore.AppendItemsAndReplaceActiveHistory(session.ID, []sessions.SessionItem{
		{
			ID:         "old-user",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "old prompt"},
		},
		{
			ID:         "old-assistant",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "old answer"},
		},
	}, []string{"old-user", "old-assistant"}); err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory(seed) error = %v", err)
	}

	result, err := service.CompactSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("CompactSession() error = %v", err)
	}
	if !called {
		t.Fatal("PlanSessionCompaction was not called")
	}
	if result.Status != "committed" || result.CompactionID != "manual-checkpoint" || result.SummaryItemID != "manual-summary" || result.LastSeq == 0 {
		t.Fatalf("CompactSession() = %#v, want manual compaction result", result)
	}
	loaded, err := service.sessionStore.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("Load(session) error = %v", err)
	}
	if len(loaded.Compactions) != 1 || loaded.Compactions[0].ID != "manual-checkpoint" {
		t.Fatalf("Compactions = %#v, want manual checkpoint", loaded.Compactions)
	}
	messages, err := loaded.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !sameStringSlice(got, []string{"manual summary"}) {
		t.Fatalf("active messages = %#v, want compacted summary", got)
	}

	// The durable compaction record shows in the chat timeline as the divider;
	// the hidden summary itself stays out of the chat.
	page, err := service.GetSessionChatItems(session.ID)
	if err != nil {
		t.Fatalf("GetSessionChatItems() error = %v", err)
	}
	if got := executionSessionItemIDs(page.Items); !sameStringSlice(got, []string{"old-user", "old-assistant", "manual-summary-record"}) {
		t.Fatalf("chat item IDs = %#v, want conversation plus compaction record", got)
	}
	for _, item := range page.Items {
		if item.ID != "manual-summary-record" {
			continue
		}
		if item.Kind != sessions.ItemKindCompaction || item.Visibility != sessions.ItemVisibilityVisible || item.Audience != sessions.ItemAudienceUser {
			t.Fatalf("record kind/visibility/audience = %q/%q/%q, want compaction/visible/user", item.Kind, item.Visibility, item.Audience)
		}
		if item.Message == nil || item.Message.Content == nil || item.Message.Content.Inline != "Context compacted" {
			t.Fatalf("record content = %#v, want manual divider text", item.Message)
		}
	}
}

func TestServiceSendSessionMessageRunsAutoCompactionBeforeTurn(t *testing.T) {
	home := t.TempDir()
	var planned bool
	runner := fakeExecutionTurnRunner{
		supports: true,
		plan: func(ctx context.Context, request SessionTurnRequest) (SessionCompactionResult, error) {
			planned = true
			if strings.TrimSpace(request.TurnID) == "" {
				t.Fatal("PlanSessionTurnCompaction TurnID is empty")
			}
			return SessionCompactionResult{Compaction: SessionCompactionPlan{
				SummaryItem: sessions.SessionItem{
					ID:         "summary-1",
					Kind:       sessions.ItemKindMessage,
					Visibility: sessions.ItemVisibilityHidden,
					Audience:   sessions.ItemAudienceModel,
					Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "summary"},
				},
				Checkpoint: sessions.CompactionCheckpoint{
					ID:                    "checkpoint-1",
					Reason:                "context_limit",
					Phase:                 "pre_turn",
					Trigger:               "auto",
					SummaryItemID:         "summary-1",
					PreviousActiveHistory: request.Session.ActiveHistory,
					ReplacementHistory:    []string{"summary-1"},
				},
			}}, nil
		},
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			if got := sessionItemIDs(request.Session.Items); !sameStringSet(got, []string{"seed-user", "seed-assistant", "summary-1", "summary-1-record"}) {
				t.Fatalf("RunSessionTurn session items = %#v, want compacted summary and record before turn", got)
			}
			if err := request.Publisher.Publish(eventAssistant(request.TurnID, "after compaction")); err != nil {
				return SessionTurnResult{}, err
			}
			return SessionTurnResult{Incremental: true}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)
	if _, err := service.sessionStore.AppendItemsAndReplaceActiveHistory(session.ID, []sessions.SessionItem{
		{
			ID:         "seed-user",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceUser,
			Message:    &model.Message{Role: model.MessageRoleUser, Content: "old prompt"},
		},
		{
			ID:         "seed-assistant",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityVisible,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleAssistant, Content: "old answer"},
		},
	}, []string{"seed-user", "seed-assistant"}); err != nil {
		t.Fatalf("AppendItemsAndReplaceActiveHistory(seed) error = %v", err)
	}

	result, err := service.SendSessionMessage(context.Background(), session.ID, "new prompt")
	if err != nil {
		t.Fatalf("SendSessionMessage() error = %v", err)
	}
	if !planned {
		t.Fatal("PlanSessionTurnCompaction was not called")
	}
	if !strings.HasPrefix(result.TurnID, "turn-") || result.LastSeq == 0 {
		t.Fatalf("SendSessionMessage() = %#v, want compacted turn result", result)
	}
	loaded, err := service.sessionStore.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("Load(session) error = %v", err)
	}
	if len(loaded.Compactions) != 1 || loaded.Compactions[0].ID != "checkpoint-1" {
		t.Fatalf("Compactions = %#v, want checkpoint-1", loaded.Compactions)
	}
	messages, err := loaded.MaterializeActiveHistory()
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if got := messageContents(messages); !sameStringSlice(got, []string{"summary", "new prompt", "after compaction"}) {
		t.Fatalf("active messages = %#v, want summary plus new turn", got)
	}
	for _, item := range loaded.Items {
		if item.ID == "summary-1-record" {
			if item.Message == nil || item.Message.Content != "Context compacted automatically" {
				t.Fatalf("auto compaction record content = %#v, want automatic divider text", item.Message)
			}
			if item.TurnID != result.TurnID {
				t.Fatalf("auto compaction record turn_id = %q, want %q", item.TurnID, result.TurnID)
			}
		}
	}
}

func TestServiceSendSessionMessageSanitizesRunnerErrorAndMarksInterrupted(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			return SessionTurnResult{}, errors.New("provider leaked prompt secret")
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)

	_, err := service.SendSessionMessage(context.Background(), session.ID, "prompt secret")
	if !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("SendSessionMessage() error = %v, want ErrTurnFailed", err)
	}
	if strings.Contains(err.Error(), "prompt secret") || strings.Contains(err.Error(), "provider leaked") {
		t.Fatalf("SendSessionMessage() leaked runner error: %v", err)
	}
	loaded, err := service.sessionStore.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("Load(session) error = %v", err)
	}
	if loaded.RunningTurnID != "" || loaded.InterruptedTurnID != "turn-000001" || loaded.InterruptedAt.IsZero() {
		t.Fatalf("turn metadata after failure = running %q interrupted %q at %s", loaded.RunningTurnID, loaded.InterruptedTurnID, loaded.InterruptedAt)
	}
}

func TestServiceSendSessionMessageReturnsBusyForHeldWriteLock(t *testing.T) {
	home := t.TempDir()
	runner := fakeExecutionTurnRunner{
		supports: true,
		run: func(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
			t.Fatal("RunSessionTurn should not be called while write lock is held")
			return SessionTurnResult{}, nil
		},
	}
	service, _, session := newExecutionServiceWithSession(t, home, runner)
	service.sessionWriteLockTimeout = 20 * time.Millisecond
	lock, err := service.sessionStore.AcquireSessionWriteLock(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("AcquireSessionWriteLock() error = %v", err)
	}
	defer lock.Release()

	_, err = service.SendSessionMessage(context.Background(), session.ID, "busy")
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("SendSessionMessage(locked) error = %v, want ErrSessionBusy", err)
	}
}

func TestServiceGetSessionSnapshot(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "repo")
	project, err := service.CreateProject(projectRoot, "Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		CreatedCWD:   project.Project.Root,
		Provider:     "fake",
		ModelProfile: "default",
		ModelID:      "model",
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	// Snapshot of an empty session: revision = "0".
	snapshot, err := service.GetSessionSnapshot(session.ID)
	if err != nil {
		t.Fatalf("GetSessionSnapshot() error = %v", err)
	}
	if snapshot.SessionID != session.ID {
		t.Fatalf("snapshot SessionID = %q, want %q", snapshot.SessionID, session.ID)
	}
	if snapshot.Revision != "0" {
		t.Fatalf("empty session revision = %q, want 0", snapshot.Revision)
	}
	if snapshot.Session.ID != session.ID {
		t.Fatalf("snapshot Session.ID = %q, want %q", snapshot.Session.ID, session.ID)
	}
	if len(snapshot.History.Items) != 0 {
		t.Fatalf("empty session history items = %d, want 0", len(snapshot.History.Items))
	}

	// Add a turn with visible items via SaveTurn.
	stored, err := service.sessionStore.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	userItem := sessions.SessionItemFromMessage("item-user-1", model.Message{Role: model.MessageRoleUser, Content: "hello"})
	userItem.TurnID = "turn-1"
	asstItem := sessions.SessionItemFromMessage("item-asst-1", model.Message{Role: model.MessageRoleAssistant, Content: "hi there"})
	asstItem.TurnID = "turn-1"
	saved, err := service.sessionStore.SaveTurn(stored, []sessions.SessionItem{userItem, asstItem}, nil)
	if err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}

	// Snapshot after items: revision should match LastSeq (as string), history should have items.
	snapshot, err = service.GetSessionSnapshot(session.ID)
	if err != nil {
		t.Fatalf("GetSessionSnapshot() after SaveTurn error = %v", err)
	}
	wantRevision := strconv.FormatInt(saved.LastSeq, 10)
	if snapshot.Revision != wantRevision {
		t.Fatalf("snapshot revision after SaveTurn = %q, want %q (LastSeq=%d)", snapshot.Revision, wantRevision, saved.LastSeq)
	}
	if snapshot.Session.LastSeq != saved.LastSeq {
		t.Fatalf("snapshot Session.LastSeq = %d, want %d", snapshot.Session.LastSeq, saved.LastSeq)
	}
	if len(snapshot.History.Items) != 2 {
		t.Fatalf("snapshot history items = %d, want 2", len(snapshot.History.Items))
	}
	if snapshot.History.NewestSeq < 1 {
		t.Fatalf("snapshot history NewestSeq = %d, expected > 0", snapshot.History.NewestSeq)
	}
	// NewestSeq is the seq of the last visible chat item; LastSeq includes
	// transaction records (begin/commit/active_history_replaced), so
	// NewestSeq < LastSeq. This is the key distinction that makes
	// session.last_seq the correct settlement watermark, not history.newest_seq.
	if snapshot.History.NewestSeq >= saved.LastSeq {
		t.Fatalf("snapshot history NewestSeq = %d should be < LastSeq = %d", snapshot.History.NewestSeq, saved.LastSeq)
	}
}

func mkdirProjectRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", root, err)
	}
	return root
}

type fakeExecutionTurnRunner struct {
	supports bool
	run      func(context.Context, SessionTurnRequest) (SessionTurnResult, error)
	plan     func(context.Context, SessionTurnRequest) (SessionCompactionResult, error)
}

func (r fakeExecutionTurnRunner) SupportsIncrementalSessionTurn(ctx context.Context, request SessionTurnRequest) (bool, error) {
	return r.supports, nil
}

func (r fakeExecutionTurnRunner) RunSessionTurn(ctx context.Context, request SessionTurnRequest) (SessionTurnResult, error) {
	if r.run == nil {
		return SessionTurnResult{Incremental: true}, nil
	}
	return r.run(ctx, request)
}

func (r fakeExecutionTurnRunner) PlanSessionTurnCompaction(ctx context.Context, request SessionTurnRequest) (SessionCompactionResult, error) {
	if r.plan == nil {
		return SessionCompactionResult{}, nil
	}
	return r.plan(ctx, request)
}

type fakeExecutionCompactPlanner struct {
	plan func(context.Context, SessionCompactionRequest) (SessionCompactionResult, error)
}

func (p fakeExecutionCompactPlanner) PlanSessionCompaction(ctx context.Context, request SessionCompactionRequest) (SessionCompactionResult, error) {
	if p.plan == nil {
		return SessionCompactionResult{}, nil
	}
	return p.plan(ctx, request)
}

func newExecutionServiceWithSession(t *testing.T, home string, runner SessionTurnRunner) (*Service, ProjectCreateResult, SessionDetail) {
	t.Helper()

	service, err := NewServiceWithOptions(home, ServiceOptions{TurnRunner: runner})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	projectRoot := mkdirProjectRoot(t, "send-repo")
	project, err := service.CreateProject(projectRoot, "Send Repo")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	saveToolResults := true
	session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
		CreatedCWD:      project.Project.Root,
		ConfigPath:      filepath.Join(project.Project.Root, ".agents", "sai.yaml"),
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model-default",
		SaveToolResults: &saveToolResults,
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	return service, project, session
}

func eventAssistant(turnID, content string) eventbus.AssistantReady {
	return eventbus.AssistantReady{
		TurnID:  turnID,
		Message: model.Message{Role: model.MessageRoleAssistant, Content: content},
	}
}

func sessionMetadataIDs(items []SessionMetadata) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	return true
}

func messageContents(messages []model.Message) []string {
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return contents
}

func sessionItemIDs(items []sessions.SessionItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func executionSessionItemIDs(items []SessionItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func sessionStreamEventTypes(events []SessionStreamEvent) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		eventType, _ := event["type"].(string)
		types = append(types, eventType)
	}
	return types
}

func sessionStreamEventsContain(events []SessionStreamEvent, eventType, key string, value any) bool {
	for _, event := range events {
		if event["type"] == eventType && event[key] == value {
			return true
		}
	}
	return false
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}
