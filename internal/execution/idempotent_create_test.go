package execution

import (
	"context"
	"errors"
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
