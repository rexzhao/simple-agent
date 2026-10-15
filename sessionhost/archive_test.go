package sessionhost

// Pool.Archive/Unarchive tests (§17.3): the refusals mirror Delete
// (run active, in-use via lease and via the host guard), the teardown
// of an open entry, the closing window, the panic-wedge protection,
// the lock discipline, and the open refusal converging to ErrArchived.

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

// TestArchiveRunningRefused: archiving a session with an active run is
// ErrRunActive — the same refusal as Delete.
func TestArchiveRunningRefused(t *testing.T) {
	provider := &gatedProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	env := newPlainProviderEnv(t, provider)

	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Session().Send("go"); err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	err = env.pool.Archive(env.id)
	if !errors.Is(err, ErrRunActive) {
		t.Fatalf("archive of running session: %v, want ErrRunActive", err)
	}
	if archived, _ := env.store.IsArchived(env.ns, env.id); archived {
		t.Fatal("the refused archive wrote the marker")
	}
}

// TestArchiveHeldLeaseRefused: an in-flight lease refuses the archive.
func TestArchiveHeldLeaseRefused(t *testing.T) {
	env := newPlainEnv(t)
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	err = env.pool.Archive(env.id)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("archive with held lease: %v, want ErrInUse", err)
	}
	if archived, _ := env.store.IsArchived(env.ns, env.id); archived {
		t.Fatal("the refused archive wrote the marker")
	}
}

// TestArchiveSubscribedRefused: a session the HOST still holds
// (InUseGuard true, no refs) refuses the archive — the archive must
// not evict a session the user is looking at.
func TestArchiveSubscribedRefused(t *testing.T) {
	var guardCalls atomic.Int64
	env := newTestEnv(t, func(o *Options) {
		o.InUseGuard = func(sessionID string) bool {
			guardCalls.Add(1)
			return true // "a subscriber is connected"
		}
	}, &gatedProvider{gate: make(chan struct{})})

	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	err = env.pool.Archive(env.id)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("archive of subscribed session: %v, want ErrInUse", err)
	}
	if guardCalls.Load() == 0 {
		t.Fatal("the refusal never consulted InUseGuard")
	}
	if archived, _ := env.store.IsArchived(env.ns, env.id); archived {
		t.Fatal("the refused archive wrote the marker")
	}
}

// TestArchiveOpenIdleTearsDown: archiving an open, idle session tears
// the entry down (OnClosed runs, the pool forgets it) and the marker
// lands; a later Acquire converges to ErrArchived.
func TestArchiveOpenIdleTearsDown(t *testing.T) {
	var onClosedIDs []string
	env := newTestEnv(t, func(o *Options) {
		o.OnClosed = func(sessionID string, sess *agent.Session, hostData any) {
			onClosedIDs = append(onClosedIDs, sessionID)
		}
	}, &gatedProvider{gate: make(chan struct{})})

	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	if err := env.pool.Archive(env.id); err != nil {
		t.Fatalf("Archive open idle: %v", err)
	}
	if len(onClosedIDs) != 1 || onClosedIDs[0] != env.id {
		t.Fatalf("OnClosed calls = %v, want exactly [%s]", onClosedIDs, env.id)
	}
	if ids := env.pool.OpenIDs(); len(ids) != 0 {
		t.Fatalf("archived session still in the pool: %v", ids)
	}
	if archived, _ := env.store.IsArchived(env.ns, env.id); !archived {
		t.Fatal("marker missing after Archive")
	}
	_, err = env.pool.Acquire(env.id)
	if !errors.Is(err, ErrArchived) {
		t.Fatalf("acquire archived: %v, want ErrArchived", err)
	}
}

// TestArchiveClosedSession: archiving a session that is merely on disk
// (never open in this pool) writes the marker without any teardown.
func TestArchiveClosedSession(t *testing.T) {
	env := newPlainEnv(t)
	if err := env.pool.Archive(env.id); err != nil {
		t.Fatalf("Archive closed: %v", err)
	}
	if archived, _ := env.store.IsArchived(env.ns, env.id); !archived {
		t.Fatal("marker missing after Archive")
	}
	_, err := env.pool.Acquire(env.id)
	if !errors.Is(err, ErrArchived) {
		t.Fatalf("acquire archived: %v, want ErrArchived", err)
	}
}

// TestArchiveUnarchiveRoundTrip: the full round-trip through the pool
// — archive → ErrArchived on open, unarchive → the session opens
// again.
func TestArchiveUnarchiveRoundTrip(t *testing.T) {
	env := newPlainEnv(t)
	if err := env.pool.Archive(env.id); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Acquire(env.id); !errors.Is(err, ErrArchived) {
		t.Fatalf("acquire archived: %v", err)
	}
	if err := env.pool.Unarchive(env.id); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatalf("acquire after unarchive: %v", err)
	}
	lease.Release()
}

// TestArchiveRefusesExternalLock: an external process (a second store
// instance playing a CLI) holding the session lock turns the archive
// into ErrLocked — cross-process mutual exclusion on the session's own
// OS lock.
func TestArchiveRefusesExternalLock(t *testing.T) {
	env := newPlainEnv(t)
	external, err := jsonl.OpenStore(env.store.Root())
	if err != nil {
		t.Fatal(err)
	}
	held, err := external.Open(env.ns, env.id)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	err = env.pool.Archive(env.id)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("archive under external lock: %v, want ErrLocked", err)
	}
	if archived, _ := env.store.IsArchived(env.ns, env.id); archived {
		t.Fatal("the refused archive wrote the marker")
	}
	// Unarchive hits the same lock.
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := env.pool.Archive(env.id); err != nil {
		t.Fatalf("archive after release: %v", err)
	}
	held2, err := external.Open(env.ns, env.id) // now refuses: archived
	if err == nil {
		held2.Close()
		t.Fatal("external open of archived session succeeded")
	}
	if err := env.pool.Unarchive(env.id); err != nil {
		t.Fatalf("unarchive after release: %v", err)
	}
}

// TestArchiveOnClosedPanicDoesNotWedge: the closing claim is cleared
// even when a host callback panics mid-teardown (Delete's panic-wedge
// shape, §15.3).
func TestArchiveOnClosedPanicDoesNotWedge(t *testing.T) {
	env := newTestEnv(t, func(o *Options) {
		o.OnClosed = func(sessionID string, sess *agent.Session, hostData any) {
			panic("injected host teardown panic")
		}
	}, &gatedProvider{gate: make(chan struct{})})

	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("the injected panic did not propagate")
			}
		}()
		_ = env.pool.Archive(env.id)
	}()

	// The id must not be wedged: the archive either landed or not,
	// but Acquire answers with a real verdict (ErrArchived or success
	// after a retry), never a permanent ErrClosing.
	if archived, _ := env.store.IsArchived(env.ns, env.id); archived {
		// The marker landed before... it cannot have (the panic was in
		// the teardown, before the marker write) — if it did, the open
		// converges to ErrArchived, still not a wedge.
		if _, err := env.pool.Acquire(env.id); !errors.Is(err, ErrArchived) {
			t.Fatalf("post-panic acquire: %v, want ErrArchived", err)
		}
		return
	}
	lease, err = env.pool.Acquire(env.id)
	if err != nil {
		t.Fatalf("post-panic acquire wedged: %v", err)
	}
	lease.Release()

	// Disarm the hook for the pool's own cleanup teardown (the Delete
	// panic-wedge test's pattern: Close does not panic-proof callbacks).
	env.pool.opts.OnClosed = func(string, *agent.Session, any) {}
}

// TestArchiveUnknownSession: an unregistered/unknown id is ErrNotFound
// (the Locate gate covers the archive surface as everywhere).
func TestArchiveUnknownSession(t *testing.T) {
	env := newPlainEnv(t)
	if err := env.pool.Archive("s00000000000000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archive unknown: %v, want ErrNotFound", err)
	}
	if err := env.pool.Unarchive("s00000000000000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unarchive unknown: %v, want ErrNotFound", err)
	}
}

// TestArchiveThenDelete: an archived session deletes through the pool
// (archive ≠ delete), leaving nothing behind.
func TestArchiveThenDelete(t *testing.T) {
	env := newPlainEnv(t)
	if err := env.pool.Archive(env.id); err != nil {
		t.Fatal(err)
	}
	if err := env.pool.Delete(env.id); err != nil {
		t.Fatalf("delete archived: %v", err)
	}
	if env.store.HasSession(env.ns, env.id) {
		t.Fatal("archived session survived Delete")
	}
}

// newPlainProviderEnv is newTestEnv with an explicit provider (for the
// gated run tests).
func newPlainProviderEnv(t *testing.T, provider *gatedProvider) *testEnv {
	return newTestEnv(t, nil, provider)
}

// TestArchiveClosingWindow pins the closing claim: an Acquire that
// arrives while Archive is mid-teardown gets ErrClosing (retryable),
// and once the marker lands the verdict converges to ErrArchived.
// The window is stretched by a blocking OnClosed (Delete's closing-
// window shape).
func TestArchiveClosingWindow(t *testing.T) {
	onClosedEntered := make(chan struct{})
	releaseOnClosed := make(chan struct{})
	env := newTestEnv(t, func(o *Options) {
		o.OnClosed = func(sessionID string, sess *agent.Session, hostData any) {
			close(onClosedEntered)
			<-releaseOnClosed
		}
	}, &gatedProvider{gate: make(chan struct{})})

	// Open the entry so the archive actually tears something down.
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	archiveDone := make(chan error, 1)
	go func() { archiveDone <- env.pool.Archive(env.id) }()
	<-onClosedEntered // the closing claim is held, teardown is paused

	if _, err := env.pool.Acquire(env.id); !errors.Is(err, ErrClosing) {
		t.Fatalf("acquire during archive: %v, want ErrClosing", err)
	}

	close(releaseOnClosed)
	if err := <-archiveDone; err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// Converged: the marker landed, the verdict is final.
	if _, err := env.pool.Acquire(env.id); !errors.Is(err, ErrArchived) {
		t.Fatalf("acquire after archive: %v, want ErrArchived", err)
	}
}

// TestUnarchiveOpenSessionRefusedByLock pins the real semantics the
// Unarchive comment now states: a session held open by THIS pool (the
// entry keeps the session's OS lock) refuses with ErrLocked — whether
// a lease is held or the entry is merely idle in the table — and the
// marker removal only succeeds after the entry is torn down.
func TestUnarchiveOpenSessionRefusedByLock(t *testing.T) {
	env := newTestEnv(t, fastSweep, &gatedProvider{gate: make(chan struct{})})

	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.pool.Unarchive(env.id); !errors.Is(err, ErrLocked) {
		t.Fatalf("unarchive open (lease held): %v, want ErrLocked", err)
	}
	lease.Release()

	// Idle in the table, lock still held by the pool entry.
	if err := env.pool.Unarchive(env.id); !errors.Is(err, ErrLocked) {
		t.Fatalf("unarchive open (idle in pool): %v, want ErrLocked", err)
	}
	if archived, _ := env.store.IsArchived(env.ns, env.id); archived {
		t.Fatal("the refused unarchive touched the marker")
	}

	// After the sweeper tears the entry down (the OS lock releases
	// with it), the marker removal succeeds. The wait is
	// TestSweeperEvictsIdleSession's shape: under the pool mutex, the
	// entry gone AND the closing placeholder gone. OpenIDs() alone is
	// NOT enough — eviction unlinks from the table BEFORE closing the
	// session (the closing set covers that window), and an unarchive
	// racing the pre-Close gap legitimately gets ErrLocked.
	deadline := time.Now().Add(3 * time.Second)
	for {
		env.pool.mu.Lock()
		_, busy := env.pool.closing[env.id]
		_, open := env.pool.sessions[env.id]
		env.pool.mu.Unlock()
		if !busy && !open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session was never evicted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := env.pool.Unarchive(env.id); err != nil {
		t.Fatalf("unarchive after teardown: %v", err)
	}
}
