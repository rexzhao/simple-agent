package execution

import (
	"errors"
	"testing"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

// cascadeTestTree holds parent → child → grandchild plus an unrelated
// sibling root in the same project.
type cascadeTestTree struct {
	project    Project
	parent     SessionDetail
	child      SessionDetail
	grandchild SessionDetail
	sibling    SessionDetail
}

func newCascadeTestTree(t *testing.T, home string) (*Service, cascadeTestTree) {
	t.Helper()
	service, err := NewService(home)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	project, err := service.CreateProject(mkdirProjectRoot(t, "cascade-repo"), "Cascade")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	create := func(name, parentID string) SessionDetail {
		t.Helper()
		session, err := service.CreateSession(project.Project.ID, SessionCreateMetadata{
			DisplayName:     name,
			ParentSessionID: parentID,
			CreatedCWD:      project.Project.Root,
			Provider:        "fake",
			ModelProfile:    "default",
			ModelID:         "model",
		})
		if err != nil {
			t.Fatalf("CreateSession(%s) error = %v", name, err)
		}
		return session
	}
	parent := create("Parent", "")
	child := create("Child", parent.ID)
	grandchild := create("Grandchild", child.ID)
	sibling := create("Sibling", "")
	return service, cascadeTestTree{project: project.Project, parent: parent, child: child, grandchild: grandchild, sibling: sibling}
}

func archivedFlag(t *testing.T, service *Service, sessionID string) bool {
	t.Helper()
	session, err := service.GetSession(sessionID)
	if err != nil {
		t.Fatalf("GetSession(%s) error = %v", sessionID, err)
	}
	return session.Archived
}

func markSessionRunning(t *testing.T, service *Service, sessionID string) {
	t.Helper()
	stored, err := service.sessionStore.LoadExecutionState(sessionID)
	if err != nil {
		t.Fatalf("Load(%s) error = %v", sessionID, err)
	}
	stored.RunningTurnID = "turn-running"
	if _, err := service.sessionStore.SaveMetadata(stored); err != nil {
		t.Fatalf("SaveMetadata(%s) error = %v", sessionID, err)
	}
}

func TestArchiveSessionCascadesToDescendants(t *testing.T) {
	service, tree := newCascadeTestTree(t, t.TempDir())

	archived, err := service.ArchiveSession(tree.parent.ID)
	if err != nil {
		t.Fatalf("ArchiveSession() error = %v", err)
	}
	if !archived.Archived || archived.ID != tree.parent.ID {
		t.Fatalf("ArchiveSession() = %#v, want archived target", archived)
	}
	for id, want := range map[string]bool{
		tree.parent.ID:     true,
		tree.child.ID:      true,
		tree.grandchild.ID: true,
		tree.sibling.ID:    false,
	} {
		if got := archivedFlag(t, service, id); got != want {
			t.Errorf("archived(%s) = %v, want %v", id, got, want)
		}
	}

	// Archiving again is an idempotent no-op.
	if _, err := service.ArchiveSession(tree.parent.ID); err != nil {
		t.Fatalf("ArchiveSession(again) error = %v", err)
	}
}

func TestArchiveSessionSubtreeBusyIsAtomic(t *testing.T) {
	service, tree := newCascadeTestTree(t, t.TempDir())
	markSessionRunning(t, service, tree.child.ID)

	if _, err := service.ArchiveSession(tree.parent.ID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("ArchiveSession() error = %v, want ErrSessionBusy", err)
	}
	// Nothing in the subtree may change when one session is busy.
	for _, id := range []string{tree.parent.ID, tree.child.ID, tree.grandchild.ID} {
		if archivedFlag(t, service, id) {
			t.Errorf("archived(%s) = true after rejected cascade, want unchanged", id)
		}
	}
}

func TestRemoveSessionCascadesToDescendants(t *testing.T) {
	service, tree := newCascadeTestTree(t, t.TempDir())

	// Archive only the target directly in the store: the removal cascade must
	// archive the still-active descendants itself before deleting them.
	stored, err := service.sessionStore.LoadExecutionState(tree.parent.ID)
	if err != nil {
		t.Fatalf("Load(parent) error = %v", err)
	}
	stored.Archived = true
	if _, err := service.sessionStore.SaveMetadata(stored); err != nil {
		t.Fatalf("SaveMetadata(parent) error = %v", err)
	}

	result, err := service.RemoveSession(tree.parent.ID)
	if err != nil {
		t.Fatalf("RemoveSession() error = %v", err)
	}
	if result.Status != "removed" || result.ID != tree.parent.ID {
		t.Fatalf("RemoveSession() = %#v, want removed target", result)
	}
	for _, id := range []string{tree.parent.ID, tree.child.ID, tree.grandchild.ID} {
		if _, err := service.GetSession(id); !errors.Is(err, sessions.ErrNotFound) {
			t.Errorf("GetSession(%s) error = %v, want ErrNotFound", id, err)
		}
	}
	if archivedFlag(t, service, tree.sibling.ID) {
		t.Error("archived(sibling) = true, want untouched")
	}
}

func TestRemoveSessionSubtreeBusyRejects(t *testing.T) {
	service, tree := newCascadeTestTree(t, t.TempDir())
	if _, err := service.ArchiveSession(tree.parent.ID); err != nil {
		t.Fatalf("ArchiveSession() error = %v", err)
	}
	markSessionRunning(t, service, tree.grandchild.ID)

	if _, err := service.RemoveSession(tree.parent.ID); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("RemoveSession() error = %v, want ErrSessionBusy", err)
	}
	// Everything stays in place when any session in the subtree is busy.
	for _, id := range []string{tree.parent.ID, tree.child.ID, tree.grandchild.ID} {
		if _, err := service.GetSession(id); err != nil {
			t.Errorf("GetSession(%s) error = %v, want present after rejected removal", id, err)
		}
	}
}

func TestRemoveSessionRequiresArchivedTarget(t *testing.T) {
	service, tree := newCascadeTestTree(t, t.TempDir())
	if _, err := service.RemoveSession(tree.parent.ID); err == nil || err.Error() != "archive session before removing it" {
		t.Fatalf("RemoveSession(unarchived) error = %v, want archive-first rejection", err)
	}
	for _, id := range []string{tree.parent.ID, tree.child.ID, tree.grandchild.ID} {
		if _, err := service.GetSession(id); err != nil {
			t.Errorf("GetSession(%s) error = %v, want present after rejected removal", id, err)
		}
	}
}
