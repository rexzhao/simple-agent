package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
		if _, err := sessionStore.Load(id); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatalf("Load(%s) error = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := sessionStore.Load(otherSession.ID); err != nil {
		t.Fatalf("Load(other session) error = %v, want retained", err)
	}
	if _, err := service.GetProject(project.Project.ID); !errors.Is(err, projectstore.ErrNotFound) {
		t.Fatalf("GetProject(removed) error = %v, want ErrNotFound", err)
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

func TestServiceSessionStatusUsesInterruptedMetadataOnly(t *testing.T) {
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
	if detail.Status != "interrupted" {
		t.Fatalf("GetSession() Status = %q, want interrupted", detail.Status)
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
	loaded, err := service.sessionStore.Load(session.ID)
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
			if got := sessionItemIDs(request.Session.Items); !sameStringSet(got, []string{"seed-user", "seed-assistant", "summary-1"}) {
				t.Fatalf("RunSessionTurn session items = %#v, want compacted summary before turn", got)
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
	loaded, err := service.sessionStore.Load(session.ID)
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
	loaded, err := service.sessionStore.Load(session.ID)
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
