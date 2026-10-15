package memory

// Archive semantics tests (§17.3) — the memory mirror of the jsonl
// marker: round-trip, open refusal, listing views, and that the
// ownership gate applies to the archive operations as everywhere.

import (
	"errors"
	"testing"

	"github.com/rexzhao/simple-agent/storage"
)

func seedMemorySession(t *testing.T, store *Store, ns string) string {
	t.Helper()
	session, err := store.Create(ns)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(storage.Record{ItemID: "h1", Type: storage.TypeSessionCreated, Time: "2026-10-10T01:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Time: "2026-10-10T01:00:01Z", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	return session.ID()
}

func TestArchiveRoundTrip(t *testing.T) {
	store := OpenStore()
	ns := "test-ns"
	id := seedMemorySession(t, store, ns)

	if archived, err := store.IsArchived(ns, id); err != nil || archived {
		t.Fatalf("IsArchived before: %v %v", archived, err)
	}
	if err := store.Archive(ns, id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if archived, err := store.IsArchived(ns, id); err != nil || !archived {
		t.Fatalf("IsArchived after: %v %v", archived, err)
	}
	if _, err := store.Open(ns, id); !errors.Is(err, storage.ErrArchived) {
		t.Fatalf("open archived: %v, want ErrArchived", err)
	}
	// Idempotent.
	if err := store.Archive(ns, id); err != nil {
		t.Fatalf("double Archive: %v", err)
	}

	// Unarchive restores opens; idempotent.
	if err := store.Unarchive(ns, id); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if err := store.Unarchive(ns, id); err != nil {
		t.Fatalf("double Unarchive: %v", err)
	}
	if _, err := store.Open(ns, id); err != nil {
		t.Fatalf("open after unarchive: %v", err)
	}
}

func TestArchiveListingViews(t *testing.T) {
	store := OpenStore()
	ns := "test-ns"
	idA := seedMemorySession(t, store, ns)
	idB := seedMemorySession(t, store, ns)
	if err := store.Archive(ns, idA); err != nil {
		t.Fatal(err)
	}

	infos, err := store.ListSessions(ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != idB || infos[0].Archived {
		t.Fatalf("default view = %+v, want only live %s", infos, idB)
	}

	all, err := store.ListSessionsAll(ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all view = %d rows, want 2", len(all))
	}
	flags := map[string]bool{}
	for _, info := range all {
		flags[info.ID] = info.Archived
	}
	if !flags[idA] || flags[idB] {
		t.Fatalf("Archived flags = %v", flags)
	}
}

func TestArchiveOwnershipGate(t *testing.T) {
	store := OpenStore()
	id := seedMemorySession(t, store, "test-ns")
	// A wrong-ns archive is a miss, like a wrong-ns open.
	if err := store.Archive("other-ns", id); !errors.Is(err, storage.ErrSessionNotFound) {
		t.Fatalf("wrong-ns Archive: %v, want ErrSessionNotFound", err)
	}
	if err := store.Unarchive("other-ns", id); !errors.Is(err, storage.ErrSessionNotFound) {
		t.Fatalf("wrong-ns Unarchive: %v, want ErrSessionNotFound", err)
	}
	if _, err := store.IsArchived("other-ns", id); !errors.Is(err, storage.ErrSessionNotFound) {
		t.Fatalf("wrong-ns IsArchived: %v, want ErrSessionNotFound", err)
	}
}

func TestDeleteArchivedClearsFlag(t *testing.T) {
	store := OpenStore()
	ns := "test-ns"
	id := seedMemorySession(t, store, ns)
	if err := store.Archive(ns, id); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ns, id); err != nil {
		t.Fatalf("Delete archived: %v", err)
	}
	// The flag is bookkeeping about a session that no longer exists —
	// recreating an id must not inherit it (ids are unique today; the
	// invariant is cheap).
	if archived := store.archived[id]; archived {
		t.Fatal("archived flag survived Delete")
	}
}
