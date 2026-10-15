package jsonl

// Archive marker tests (§17.3): the round-trip (archive → open refused
// → unarchive → open works), the listing views (default filters,
// All includes with Archived filled), the lock discipline (an external
// lock holder refuses the mutation), and that deleting an archived
// session just works (archive ≠ delete).

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rexzhao/simple-agent/storage"
)

// seedArchivable creates one materialized session and returns its id.
func seedArchivable(t *testing.T, store *Store, project string) string {
	t.Helper()
	session, err := store.Create(project)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, session,
		headerRecord("2026-10-10T01:00:00Z"),
		userInput("u1", "2026-10-10T01:00:01Z", "fix the login bug"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	return session.ID()
}

func TestArchiveRoundTrip(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	id := seedArchivable(t, store, project)

	// Not archived yet: IsArchived false, open works (and CLOSES —
	// an open session holds the lock the archive later needs).
	if archived, err := store.IsArchived(project, id); err != nil || archived {
		t.Fatalf("IsArchived before: %v %v", archived, err)
	}
	pre, err := store.Open(project, id)
	if err != nil {
		t.Fatalf("open before archive: %v", err)
	}
	if err := pre.Close(); err != nil {
		t.Fatal(err)
	}

	// Archive → marker on disk, open refused with the typed sentinel.
	if err := store.Archive(project, id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, project, id, "archived")); err != nil {
		t.Fatalf("marker missing after Archive: %v", err)
	}
	if archived, err := store.IsArchived(project, id); err != nil || !archived {
		t.Fatalf("IsArchived after: %v %v", archived, err)
	}
	_, err = store.Open(project, id)
	if !errors.Is(err, storage.ErrArchived) {
		t.Fatalf("open archived: err = %v, want ErrArchived", err)
	}

	// Idempotent double archive.
	if err := store.Archive(project, id); err != nil {
		t.Fatalf("double Archive: %v", err)
	}

	// Unarchive → marker gone, open works again; idempotent too.
	if err := store.Unarchive(project, id); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, project, id, "archived")); !os.IsNotExist(err) {
		t.Fatal("marker still present after Unarchive")
	}
	if err := store.Unarchive(project, id); err != nil {
		t.Fatalf("double Unarchive: %v", err)
	}
	post, err := store.Open(project, id)
	if err != nil {
		t.Fatalf("open after unarchive: %v", err)
	}
	_ = post.Close()
}

func TestArchiveUnknownSession(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	// A valid-format id with no directory: not-found, not a format error.
	if err := store.Archive(project, "s00000000000000000000000000"); !errors.Is(err, storage.ErrSessionNotFound) {
		t.Fatalf("Archive unknown: %v, want ErrSessionNotFound", err)
	}
}

func TestArchiveListingViews(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	idA := seedArchivable(t, store, project)
	idB := seedArchivable(t, store, project)
	if err := store.Archive(project, idA); err != nil {
		t.Fatal(err)
	}

	// Default view: archived hidden, the live one remains.
	infos, err := store.ListSessions(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != idB {
		t.Fatalf("default view = %+v, want only %s", infos, idB)
	}
	if infos[0].Archived {
		t.Fatal("live session reported Archived")
	}

	// All view: both rows, the archived one flagged.
	all, err := store.ListSessionsAll(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all view = %d rows, want 2", len(all))
	}
	archived := map[string]bool{}
	for _, info := range all {
		archived[info.ID] = info.Archived
	}
	if !archived[idA] || archived[idB] {
		t.Fatalf("Archived flags = %v, want %s=true %s=false", archived, idA, idB)
	}
}

func TestArchiveRefusesExternalLock(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	id := seedArchivable(t, store, project)

	// A second store instance plays the external process: an Open
	// session holds the OS lock through its lifetime.
	external, err := OpenStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	held, err := external.Open(project, id)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	if err := store.Archive(project, id); err == nil {
		t.Fatal("Archive under an external lock holder succeeded")
	} else {
		var lockErr *LockHeldError
		if !errors.As(err, &lockErr) {
			t.Fatalf("Archive under lock: err = %v, want LockHeldError", err)
		}
	}
	if archived, _ := store.IsArchived(project, id); archived {
		t.Fatal("marker written despite the lock refusal")
	}

	// The marker check inside openDir serializes on the same lock:
	// once WE hold the session, an external archive cannot sneak in —
	// covered by the same LockHeldError above; here the flip side: our
	// open keeps working while unlocked state is untouched.
	_ = held.Close()
	if err := store.Archive(project, id); err != nil {
		t.Fatalf("Archive after release: %v", err)
	}
}

func TestDeleteArchivedSession(t *testing.T) {
	store, _ := testStore(t)
	project := testNS(t)
	id := seedArchivable(t, store, project)
	if err := store.Archive(project, id); err != nil {
		t.Fatal(err)
	}

	// Deleting a parked session succeeds and leaves no directory —
	// the content sweep removes the marker along with the segments.
	if err := store.Delete(project, id); err != nil {
		t.Fatalf("Delete archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, project, id)); !os.IsNotExist(err) {
		t.Fatal("session directory still present after Delete")
	}
	if _, err := store.Open(project, id); !errors.Is(err, storage.ErrSessionNotFound) {
		t.Fatalf("open after delete: %v, want ErrSessionNotFound", err)
	}
}
