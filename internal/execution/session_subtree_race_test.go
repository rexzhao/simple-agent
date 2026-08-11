package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateSessionKeepsIdleParentLockBelowActiveAncestor(t *testing.T) {
	service, _, root := newExecutionServiceWithSession(t, t.TempDir(), nil)
	child, err := service.CreateInheritedSession(root.ID, "child")
	if err != nil {
		t.Fatalf("CreateInheritedSession() error = %v", err)
	}
	if _, err := service.sessionStore.MarkTurnRunning(root.ID, "root-run"); err != nil {
		t.Fatalf("MarkTurnRunning(root) error = %v", err)
	}

	locks, err := service.acquireSessionParentMutationLocks(child.ID)
	if err != nil {
		t.Fatalf("acquireSessionParentMutationLocks() error = %v", err)
	}
	if len(locks) != 1 {
		t.Fatalf("parent mutation locks = %d, want the idle child lock", len(locks))
	}
	releaseSessionMutationLocks(locks)

	held, err := service.sessionStore.AcquireSessionWriteLock(context.Background(), child.ID)
	if err != nil {
		t.Fatalf("AcquireSessionWriteLock(child) error = %v", err)
	}
	defer func() { _ = held.Release() }()
	created := make(chan struct{})
	var grandchildErr error
	go func() {
		_, grandchildErr = service.CreateSession(child.ProjectID, SessionCreateMetadata{
			ParentSessionID: child.ID,
			CreatedCWD:      child.CreatedCWD,
			Provider:        child.Provider,
			ModelProfile:    child.ModelProfile,
			ModelID:         child.ModelID,
		})
		close(created)
	}()
	select {
	case <-created:
		t.Fatal("grandchild creation bypassed the idle child lock")
	case <-time.After(50 * time.Millisecond):
	}
	if err := held.Release(); err != nil {
		t.Fatalf("ReleaseSessionWriteLock(child) error = %v", err)
	}
	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("grandchild creation did not proceed after child lock release")
	}
	if grandchildErr != nil {
		t.Fatalf("grandchild creation error = %v", grandchildErr)
	}
}

func TestCreateSessionIdempotentLocksParentLineageForNewIdentity(t *testing.T) {
	service, _, root := newExecutionServiceWithSession(t, t.TempDir(), nil)
	parent, err := service.CreateInheritedSession(root.ID, "idle parent")
	if err != nil {
		t.Fatalf("CreateInheritedSession() error = %v", err)
	}
	if _, err := service.sessionStore.MarkTurnRunning(root.ID, "root-run"); err != nil {
		t.Fatalf("MarkTurnRunning(root) error = %v", err)
	}
	held, err := service.sessionStore.AcquireSessionWriteLock(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("AcquireSessionWriteLock(parent) error = %v", err)
	}
	created := make(chan struct{})
	var detail SessionDetail
	var wasCreated bool
	var createErr error
	go func() {
		detail, wasCreated, createErr = service.CreateSessionIdempotent(context.Background(), parent.ProjectID, "idempotent-grandchild", "idempotent-grandchild-fingerprint", SessionCreateMetadata{
			ParentSessionID: parent.ID,
			CreatedCWD:      parent.CreatedCWD,
			Provider:        parent.Provider,
			ModelProfile:    parent.ModelProfile,
			ModelID:         parent.ModelID,
		})
		close(created)
	}()
	select {
	case <-created:
		_ = held.Release()
		t.Fatal("idempotent grandchild creation bypassed the idle parent lock")
	case <-time.After(50 * time.Millisecond):
	}
	if err := held.Release(); err != nil {
		t.Fatalf("ReleaseSessionWriteLock(parent) error = %v", err)
	}
	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("idempotent grandchild creation did not proceed after parent lock release")
	}
	if createErr != nil || !wasCreated || detail.ParentSessionID != parent.ID {
		t.Fatalf("idempotent grandchild = %#v created=%v err=%v", detail, wasCreated, createErr)
	}
}

func TestCreateConfiguredSessionIdempotentLocksParentLineageForNewIdentity(t *testing.T) {
	service, projectID, projectRoot := newConfiguredIdempotentRaceService(t)
	parent, err := service.CreateSession(projectID, SessionCreateMetadata{CreatedCWD: projectRoot, Provider: "fake", ModelProfile: "fast", ModelID: "fake-model"})
	if err != nil {
		t.Fatalf("CreateSession(parent) error = %v", err)
	}
	held, err := service.sessionStore.AcquireSessionWriteLock(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("AcquireSessionWriteLock(parent) error = %v", err)
	}
	created := make(chan struct{})
	var detail SessionDetail
	var wasCreated bool
	var createErr error
	go func() {
		detail, wasCreated, createErr = service.CreateConfiguredSessionIdempotent(context.Background(), projectID, "configured-idempotent-child", "configured-idempotent-fingerprint", ConfiguredSessionOptions{
			CWD:             projectRoot,
			ParentSessionID: parent.ID,
			Provider:        "fake",
			ModelProfile:    "fast",
		})
		close(created)
	}()
	select {
	case <-created:
		_ = held.Release()
		t.Fatal("configured idempotent child creation bypassed the parent lock")
	case <-time.After(50 * time.Millisecond):
	}
	if err := held.Release(); err != nil {
		t.Fatalf("ReleaseSessionWriteLock(parent) error = %v", err)
	}
	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("configured idempotent child creation did not proceed after parent lock release")
	}
	if createErr != nil || !wasCreated || detail.ParentSessionID != parent.ID {
		t.Fatalf("configured idempotent child = %#v created=%v err=%v", detail, wasCreated, createErr)
	}
}

func newConfiguredIdempotentRaceService(t *testing.T) (*Service, string, string) {
	t.Helper()
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
	projectRoot := mkdirProjectRoot(t, "configured-idempotent-race")
	project, err := service.CreateProject(projectRoot, "Configured idempotent race")
	if err != nil {
		t.Fatal(err)
	}
	return service, project.Project.ID, project.Project.Root
}

func TestArchiveSessionRescansDescendantInsertedAfterInitialScan(t *testing.T) {
	service, _, root := newExecutionServiceWithSession(t, t.TempDir(), nil)
	var callbackErr error
	_, err := service.archiveSession(root.ID, func() {
		child, created, err := service.CreateSessionIdempotent(context.Background(), root.ProjectID, "idempotent-archive-race-child", "idempotent-archive-race-fingerprint", SessionCreateMetadata{
			ParentSessionID: root.ID,
			CreatedCWD:      root.CreatedCWD,
			Provider:        root.Provider,
			ModelProfile:    root.ModelProfile,
			ModelID:         root.ModelID,
		})
		if err != nil {
			callbackErr = err
			return
		}
		if !created {
			callbackErr = errors.New("idempotent archive-race child was not created")
			return
		}
		_, callbackErr = service.sessionStore.MarkTurnRunning(child.ID, "raced-child-run")
	})
	if callbackErr != nil {
		t.Fatalf("scan-window child insertion error = %v", callbackErr)
	}
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("ArchiveSession(scan-window child) error = %v, want ErrSessionBusy", err)
	}
	rootState, err := service.sessionStore.LoadState(root.ID)
	if err != nil {
		t.Fatalf("LoadState(root) error = %v", err)
	}
	if rootState.Archived {
		t.Fatal("archive changed root despite the inserted descendant run")
	}
}

func TestArchiveSessionRescansConfiguredIdempotentDescendantInsertedAfterInitialScan(t *testing.T) {
	service, projectID, projectRoot := newConfiguredIdempotentRaceService(t)
	root, err := service.CreateSession(projectID, SessionCreateMetadata{CreatedCWD: projectRoot, Provider: "fake", ModelProfile: "fast", ModelID: "fake-model"})
	if err != nil {
		t.Fatalf("CreateSession(root) error = %v", err)
	}
	var callbackErr error
	_, err = service.archiveSession(root.ID, func() {
		child, created, err := service.CreateConfiguredSessionIdempotent(context.Background(), projectID, "configured-archive-race-child", "configured-archive-race-fingerprint", ConfiguredSessionOptions{
			CWD:             projectRoot,
			ParentSessionID: root.ID,
			Provider:        "fake",
			ModelProfile:    "fast",
		})
		if err != nil {
			callbackErr = err
			return
		}
		if !created {
			callbackErr = errors.New("configured idempotent archive-race child was not created")
			return
		}
		_, callbackErr = service.sessionStore.MarkTurnRunning(child.ID, "configured-raced-child-run")
	})
	if callbackErr != nil {
		t.Fatalf("configured scan-window child insertion error = %v", callbackErr)
	}
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("ArchiveSession(configured scan-window child) error = %v, want ErrSessionBusy", err)
	}
	rootState, err := service.sessionStore.LoadState(root.ID)
	if err != nil {
		t.Fatalf("LoadState(configured root) error = %v", err)
	}
	if rootState.Archived {
		t.Fatal("configured archive changed root despite the inserted descendant run")
	}
}

func TestRemoveSessionRescansDescendantInsertedAfterInitialScan(t *testing.T) {
	service, project, root := newExecutionServiceWithSession(t, t.TempDir(), nil)
	racedChild, err := service.buildSession(project.Project.ID, SessionCreateMetadata{
		ParentSessionID: root.ID,
		CreatedCWD:      root.CreatedCWD,
		Provider:        root.Provider,
		ModelProfile:    root.ModelProfile,
		ModelID:         root.ModelID,
	}, "inserted-during-remove-scan")
	if err != nil {
		t.Fatalf("buildSession(raced child) error = %v", err)
	}
	if _, err := service.ArchiveSession(root.ID); err != nil {
		t.Fatalf("ArchiveSession(root) error = %v", err)
	}

	var callbackErr error
	_, err = service.removeSession(root.ID, func() {
		if _, callbackErr = service.sessionStore.SaveMetadata(racedChild); callbackErr != nil {
			return
		}
		_, callbackErr = service.sessionStore.MarkTurnRunning(racedChild.ID, "raced-remove-child-run")
	})
	if callbackErr != nil {
		t.Fatalf("scan-window child insertion error = %v", callbackErr)
	}
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("RemoveSession(scan-window child) error = %v, want ErrSessionBusy", err)
	}
	if _, err := service.sessionStore.LoadState(root.ID); err != nil {
		t.Fatalf("LoadState(root after failed remove) error = %v", err)
	}
	if _, err := service.sessionStore.LoadState(racedChild.ID); err != nil {
		t.Fatalf("LoadState(raced child after failed remove) error = %v", err)
	}
}
