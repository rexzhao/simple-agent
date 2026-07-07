package execution

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func mkdirProjectRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", root, err)
	}
	return root
}
