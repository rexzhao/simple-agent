package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func durableCreateSession(id, project string) SessionV2 {
	return SessionV2{ID: id, ProjectID: project, Provider: "codex", ModelProfile: "default", ModelID: "model"}
}

func TestCreateMetadataIdempotentIsDurableAndFingerprintStrict(t *testing.T) {
	root := t.TempDir()
	store := NewV2Store(root)
	var builds atomic.Int32
	build := func(context.Context) (SessionV2, error) {
		builds.Add(1)
		return durableCreateSession("session-durable-create", "project-a"), nil
	}
	first, created, err := store.CreateMetadataIdempotent(context.Background(), "session-durable-create", "fingerprint-a", build)
	if err != nil || !created {
		t.Fatalf("first create = %#v, created=%v, err=%v", first, created, err)
	}
	second, created, err := store.CreateMetadataIdempotent(context.Background(), "session-durable-create", "fingerprint-a", func(context.Context) (SessionV2, error) {
		builds.Add(1)
		return durableCreateSession("session-durable-create", "wrong-project"), nil
	})
	if err != nil || created || second.ProjectID != "project-a" {
		t.Fatalf("serial retry = %#v, created=%v, err=%v", second, created, err)
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("retry rebuilt business mutation %d times, want once", got)
	}
	if _, _, err := store.CreateMetadataIdempotent(context.Background(), "session-durable-create", "fingerprint-b", build); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different fingerprint error = %v, want ErrIdempotencyConflict", err)
	}

	restarted := NewV2Store(root)
	third, created, err := restarted.CreateMetadataIdempotent(context.Background(), "session-durable-create", "fingerprint-a", func(context.Context) (SessionV2, error) {
		builds.Add(1)
		return durableCreateSession("session-durable-create", "after-restart"), nil
	})
	if err != nil || created || third.ProjectID != "project-a" {
		t.Fatalf("restart retry = %#v, created=%v, err=%v", third, created, err)
	}
}

func TestCreateMetadataIdempotentConcurrentClaimsOnlyOnce(t *testing.T) {
	store := NewV2Store(t.TempDir())
	const id = "session-concurrent-create"
	var builds atomic.Int32
	build := func(context.Context) (SessionV2, error) {
		builds.Add(1)
		time.Sleep(10 * time.Millisecond)
		return durableCreateSession(id, "project-a"), nil
	}
	const callers = 12
	var wg sync.WaitGroup
	results := make(chan bool, callers)
	errorsCh := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := store.CreateMetadataIdempotent(context.Background(), id, "same-fingerprint", build)
			results <- created
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	createdCount := 0
	for created := range results {
		if created {
			createdCount++
		}
	}
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent create error = %v", err)
		}
	}
	if createdCount != 1 || builds.Load() != 1 {
		t.Fatalf("concurrent creates = created %d, builders %d; want 1/1", createdCount, builds.Load())
	}
}

func TestCreateMetadataIdempotentFailureLeavesNoSuccessReceipt(t *testing.T) {
	root := t.TempDir()
	store := NewV2Store(root)
	const id = "session-failed-create"
	wantErr := errors.New("configuration failed")
	if _, created, err := store.CreateMetadataIdempotent(context.Background(), id, "fingerprint-a", func(context.Context) (SessionV2, error) {
		return SessionV2{}, wantErr
	}); !errors.Is(err, wantErr) || created {
		t.Fatalf("failed create = created=%v err=%v", created, err)
	}
	if _, err := store.LoadState(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed create LoadState error = %v, want not found", err)
	}
	if _, err := os.Stat(filepath.Join(root, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed create session directory stat error = %v, want absent", err)
	}
	restarted := NewV2Store(root)
	if _, created, err := restarted.CreateMetadataIdempotent(context.Background(), id, "fingerprint-a", func(context.Context) (SessionV2, error) {
		return durableCreateSession(id, "project-a"), nil
	}); err != nil || !created {
		t.Fatalf("retry after failed create = created=%v err=%v", created, err)
	}
}

func TestCreateMetadataIdempotentReservesDeletedIdentity(t *testing.T) {
	root := t.TempDir()
	store := NewV2Store(root)
	const id = "session-delete-claim"
	if _, created, err := store.CreateMetadataIdempotent(context.Background(), id, "fingerprint-a", func(context.Context) (SessionV2, error) {
		return durableCreateSession(id, "project-a"), nil
	}); err != nil || !created {
		t.Fatalf("create = created=%v err=%v", created, err)
	}
	if err := store.Delete(id); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.LoadState(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted LoadState() error = %v, want ErrNotFound", err)
	}

	// A retry after deletion returns only the minimal historical identity. It
	// must not invoke a builder or recreate the private session directory.
	result, created, err := store.CreateMetadataIdempotent(context.Background(), id, "fingerprint-a", func(context.Context) (SessionV2, error) {
		t.Fatal("deleted retry unexpectedly rebuilt the session")
		return SessionV2{}, nil
	})
	if err != nil || created || result.ID != id || result.ProjectID != "project-a" {
		t.Fatalf("deleted same-fingerprint retry = %#v created=%v err=%v", result, created, err)
	}
	if _, _, err := store.CreateMetadataIdempotent(context.Background(), id, "fingerprint-b", func(context.Context) (SessionV2, error) {
		return durableCreateSession(id, "project-b"), nil
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("deleted different-fingerprint retry error = %v, want conflict", err)
	}

	// The claim is independent of the store object and survives a process
	// reconstruction.
	restarted := NewV2Store(root)
	result, created, err = restarted.CreateMetadataIdempotent(context.Background(), id, "fingerprint-a", func(context.Context) (SessionV2, error) {
		t.Fatal("restart retry unexpectedly rebuilt the deleted session")
		return SessionV2{}, nil
	})
	if err != nil || created || result.ProjectID != "project-a" {
		t.Fatalf("restart deleted retry = %#v created=%v err=%v", result, created, err)
	}
}

func TestCreateMetadataIdempotentLegacyDeleteCreatesPermanentEmptyFingerprintClaim(t *testing.T) {
	store := NewV2Store(t.TempDir())
	const id = "legacy-session-deleted"
	if _, err := store.SaveMetadata(SessionV2{ID: id, ProjectID: "legacy-project"}); err != nil {
		t.Fatalf("legacy SaveMetadata() error = %v", err)
	}
	if err := store.Delete(id); err != nil {
		t.Fatalf("legacy Delete() error = %v", err)
	}
	if _, _, err := store.CreateMetadataIdempotent(context.Background(), id, "new-create-fingerprint", func(context.Context) (SessionV2, error) {
		return durableCreateSession(id, "new-project"), nil
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("legacy deleted create error = %v, want conflict", err)
	}
}

func TestCreateMetadataIdempotentTombstoneBeforePhysicalDeleteIsSafe(t *testing.T) {
	store := NewV2Store(t.TempDir())
	const id = "session-delete-window"
	if _, err := store.SaveMetadata(SessionV2{ID: id, ProjectID: "project-a"}); err != nil {
		t.Fatal(err)
	}
	lock, err := store.AcquireSessionWriteLock(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.writeSessionClaim(id, "", "project-a"); err != nil {
		_ = lock.Release()
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	// This simulates a crash between the durable claim rename and the
	// removable-directory deletion. The state is still present, but the ID is
	// already permanently unavailable to the command create primitive.
	if _, _, err := store.CreateMetadataIdempotent(context.Background(), id, "any-fingerprint", func(context.Context) (SessionV2, error) {
		return durableCreateSession(id, "wrong-project"), nil
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("claim-window create error = %v, want conflict", err)
	}
	if _, err := store.LoadState(id); err != nil {
		t.Fatalf("claim-window state disappeared too early: %v", err)
	}
}

func TestCreateMetadataIdempotentCreationBaselineIgnoresBuilderProjection(t *testing.T) {
	store := NewV2Store(t.TempDir())
	const id = "session-create-baseline"
	built := SessionV2{
		ID:                id,
		ProjectID:         "project-a",
		LastSeq:           42,
		metadataVersion:   17,
		Items:             []SessionItem{{ID: "stale-item"}},
		ActiveHistory:     []string{"stale-item"},
		Compactions:       []CompactionCheckpoint{{ID: "stale-compaction"}},
		CurrentRunID:      "stale-current",
		RunningRunID:      "stale-running",
		RunningTurnID:     "stale-turn",
		InterruptedRunID:  "stale-interrupted",
		InterruptedTurnID: "stale-interrupted-turn",
		LatestRunID:       "stale-latest",
		LastRunID:         "stale-last",
		LastRunStatus:     "running",
		HasUnreadResult:   true,
	}
	if _, created, err := store.CreateMetadataIdempotent(context.Background(), id, "fingerprint-a", func(context.Context) (SessionV2, error) {
		return built, nil
	}); err != nil || !created {
		t.Fatalf("baseline create = created=%v err=%v", created, err)
	}
	loaded, err := store.LoadState(id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastSeq != 0 || loaded.metadataVersion != 0 || len(loaded.Items) != 0 || len(loaded.ActiveHistory) != 0 || len(loaded.Compactions) != 0 || loaded.CurrentRunID != "" || loaded.RunningRunID != "" || loaded.RunningTurnID != "" || loaded.InterruptedRunID != "" || loaded.InterruptedTurnID != "" || loaded.LatestRunID != "" || loaded.LastRunID != "" || loaded.LastRunStatus != "" || loaded.HasUnreadResult {
		t.Fatalf("creation baseline retained builder projection: %#v", loaded)
	}
}

func TestLegacyStateSchemaMigratesAndLegacyLengthRemainsCompatible(t *testing.T) {
	root := t.TempDir()
	// This deliberately exceeds the command-layer 128-byte limit. Existing
	// durable IDs must remain loadable and removable even though new command
	// IDs are more tightly bounded.
	id := strings.Repeat("legacy-id-", 20)
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	session, err := prepareNewSessionMetadata(durableCreateSession(id, "legacy-project"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	data, err := marshalState(session)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "session.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE state (
        singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
        session_id TEXT NOT NULL,
        state_json BLOB NOT NULL,
        last_seq INTEGER NOT NULL DEFAULT 0,
        metadata_version INTEGER NOT NULL DEFAULT 0
    )`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO state(singleton, session_id, state_json, last_seq, metadata_version) VALUES(1, ?, ?, 0, 0)`, id, data); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store := NewV2Store(root)
	loaded, err := store.LoadState(id)
	if err != nil || loaded.ID != id {
		t.Fatalf("legacy LoadState() = %#v err=%v", loaded, err)
	}
	if _, err := store.SaveMetadata(loaded); err != nil {
		t.Fatalf("legacy SaveMetadata() migration error = %v", err)
	}
	checkDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var columnCount int
	if err := checkDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('state') WHERE name = 'create_fingerprint'`).Scan(&columnCount); err != nil {
		_ = checkDB.Close()
		t.Fatal(err)
	}
	_ = checkDB.Close()
	if columnCount != 1 {
		t.Fatalf("legacy schema create_fingerprint columns = %d, want 1", columnCount)
	}
	if _, _, err := store.CreateMetadataIdempotent(context.Background(), id, "fingerprint-a", func(context.Context) (SessionV2, error) {
		return durableCreateSession(id, "new-project"), nil
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("legacy existing create error = %v, want conflict", err)
	}
	if err := store.Delete(id); err != nil {
		t.Fatalf("legacy Delete() after migration error = %v", err)
	}
	restarted := NewV2Store(root)
	if _, _, err := restarted.CreateMetadataIdempotent(context.Background(), id, "fingerprint-a", func(context.Context) (SessionV2, error) {
		return durableCreateSession(id, "new-project"), nil
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("legacy deleted restart create error = %v, want conflict", err)
	}
}

func TestValidateSessionIDDoesNotRejectExistingLongIDs(t *testing.T) {
	id := strings.Repeat("long-", 30)
	if err := ValidateSessionID(id); err != nil {
		t.Fatalf("ValidateSessionID(%d bytes) error = %v", len(id), err)
	}
	if err := ValidateSessionCreateID(id); err == nil {
		t.Fatal("ValidateSessionCreateID accepted an over-limit new ID")
	}
	if got := fmt.Sprint(ValidateSessionCreateID(id)); !strings.Contains(got, "too long") {
		t.Fatalf("create ID error = %q, want the command length boundary", got)
	}
	for _, reserved := range []string{".session-claims", ".SESSION-CLAIMS."} {
		if err := ValidateSessionID(reserved); err == nil {
			t.Fatalf("ValidateSessionID(%q) accepted the durable claims root", reserved)
		}
	}
}

func TestCreateMetadataIdempotentPublishesOneMutation(t *testing.T) {
	store := NewV2Store(t.TempDir())
	mutations := make(chan Mutation, 4)
	registration := store.RegisterMutationSinkWithOptions(mutationSinkFunc(func(mutation Mutation) error {
		mutations <- mutation
		return nil
	}), MutationSinkOptions{QueueCapacity: 4})
	if registration == nil {
		t.Fatal("RegisterMutationSink returned nil")
	}
	defer registration.Unregister()
	const id = "session-one-mutation"
	build := func(context.Context) (SessionV2, error) { return durableCreateSession(id, "project-a"), nil }
	if _, _, err := store.CreateMetadataIdempotent(context.Background(), id, "fingerprint-a", build); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateMetadataIdempotent(context.Background(), id, "fingerprint-a", build); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mutations:
	case <-time.After(2 * time.Second):
		t.Fatal("missing create mutation")
	}
	select {
	case mutation := <-mutations:
		t.Fatalf("duplicate create mutation = %#v", mutation)
	case <-time.After(100 * time.Millisecond):
	}
}

type mutationSinkFunc func(Mutation) error

func (f mutationSinkFunc) PublishMutation(mutation Mutation) error { return f(mutation) }
