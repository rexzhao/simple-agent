package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestCreateSessionIdempotentSurvivesServiceRebuild(t *testing.T) {
	home := t.TempDir()
	service, err := NewService(home)
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := mkdirProjectRoot(t, "durable-create")
	project, err := service.CreateProject(projectRoot, "Durable create")
	if err != nil {
		t.Fatal(err)
	}
	metadata := SessionCreateMetadata{DisplayName: "original", CreatedCWD: project.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model"}
	first, created, err := service.CreateSessionIdempotent(context.Background(), project.Project.ID, "session-service-restart", "fingerprint-service", metadata)
	if err != nil || !created {
		t.Fatalf("first service create=%#v created=%v err=%v", first, created, err)
	}

	restarted, err := NewService(home)
	if err != nil {
		t.Fatal(err)
	}
	retry, created, err := restarted.CreateSessionIdempotent(context.Background(), project.Project.ID, first.ID, "fingerprint-service", SessionCreateMetadata{
		DisplayName: "must not rebuild", CreatedCWD: project.Project.Root, Provider: "different", ModelProfile: "different", ModelID: "different",
	})
	if err != nil || created || retry.ID != first.ID || retry.DisplayName != first.DisplayName || retry.Provider != first.Provider {
		t.Fatalf("restart retry=%#v created=%v err=%v", retry, created, err)
	}
	if _, _, err := restarted.CreateSessionIdempotent(context.Background(), project.Project.ID, first.ID, "different-fingerprint", metadata); !errors.Is(err, sessions.ErrIdempotencyConflict) {
		t.Fatalf("different fingerprint error=%v, want conflict", err)
	}
}

func TestCreateSessionIdempotentRetryDoesNotPreflightDeletedParent(t *testing.T) {
	service, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(mkdirProjectRoot(t, "durable-parent"), "Durable parent")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{DisplayName: "parent", CreatedCWD: project.Project.Root})
	if err != nil {
		t.Fatal(err)
	}
	metadata := SessionCreateMetadata{
		DisplayName:     "durable child",
		CreatedCWD:      project.Project.Root,
		ParentSessionID: parent.ID,
		Provider:        "fake",
		ModelProfile:    "default",
		ModelID:         "model",
	}
	first, created, err := service.CreateSessionIdempotent(context.Background(), project.Project.ID, "session-durable-child", "fingerprint-child", metadata)
	if err != nil || !created {
		t.Fatalf("first child create=%#v created=%v err=%v", first, created, err)
	}

	if _, err := service.ArchiveSession(parent.ID); err != nil {
		t.Fatalf("ArchiveSession(parent) error = %v", err)
	}
	archivedRetry, created, err := service.CreateSessionIdempotent(context.Background(), project.Project.ID, first.ID, "fingerprint-child", metadata)
	if err != nil || created || archivedRetry.ID != first.ID || archivedRetry.DisplayName != first.DisplayName {
		t.Fatalf("retry after parent archive=%#v created=%v err=%v", archivedRetry, created, err)
	}

	if _, err := service.RemoveSession(parent.ID); err != nil {
		t.Fatalf("RemoveSession(parent) error = %v", err)
	}
	deletedRetry, created, err := service.CreateSessionIdempotent(context.Background(), project.Project.ID, first.ID, "fingerprint-child", metadata)
	if err != nil || created || deletedRetry.ID != first.ID || deletedRetry.ProjectID != project.Project.ID {
		t.Fatalf("retry after parent deletion=%#v created=%v err=%v", deletedRetry, created, err)
	}
}

func TestCreateConfiguredSessionIdempotentRetryDoesNotPreflightDeletedParent(t *testing.T) {
	home := t.TempDir()
	providersDir := filepath.Join(home, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sai.yaml"), []byte("default_provider: fake\ndefault_model: fast\nprovider_dir: providers\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(providersDir, "fake.yaml"), []byte("name: fake\nbase_url: http://127.0.0.1:1/v1\napi_key: test-key\nmodels:\n  fast:\n    id: fake-model\n    context_window: 64000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.CreateProject(mkdirProjectRoot(t, "configured-durable-parent"), "Configured durable parent")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{DisplayName: "parent", CreatedCWD: project.Project.Root})
	if err != nil {
		t.Fatal(err)
	}
	options := ConfiguredSessionOptions{
		CWD:             project.Project.Root,
		DisplayName:     "configured durable child",
		ParentSessionID: parent.ID,
		Provider:        "fake",
		ModelProfile:    "fast",
	}
	first, created, err := service.CreateConfiguredSessionIdempotent(context.Background(), project.Project.ID, "configured-durable-child", "configured-fingerprint", options)
	if err != nil || !created {
		t.Fatalf("first configured child create=%#v created=%v err=%v", first, created, err)
	}

	if err := os.Rename(filepath.Join(home, "sai.yaml"), filepath.Join(home, "sai.yaml.unavailable")); err != nil {
		t.Fatalf("make configured session config unavailable error = %v", err)
	}
	if _, err := service.ArchiveSession(parent.ID); err != nil {
		t.Fatalf("ArchiveSession(parent) error = %v", err)
	}
	archivedRetry, created, err := service.CreateConfiguredSessionIdempotent(context.Background(), project.Project.ID, first.ID, "configured-fingerprint", options)
	if err != nil || created || archivedRetry.ID != first.ID || archivedRetry.DisplayName != first.DisplayName {
		t.Fatalf("configured retry after parent archive=%#v created=%v err=%v", archivedRetry, created, err)
	}

	if _, err := service.RemoveSession(parent.ID); err != nil {
		t.Fatalf("RemoveSession(parent) error = %v", err)
	}
	deletedRetry, created, err := service.CreateConfiguredSessionIdempotent(context.Background(), project.Project.ID, first.ID, "configured-fingerprint", options)
	if err != nil || created || deletedRetry.ID != first.ID || deletedRetry.ProjectID != project.Project.ID {
		t.Fatalf("configured retry after parent deletion=%#v created=%v err=%v", deletedRetry, created, err)
	}
}
