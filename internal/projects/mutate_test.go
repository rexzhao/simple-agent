package projects

// Mutate's lock discipline (C8b, 待办 3): a held registry lock makes a
// Mutate call retry within its bounded budget and then fail with
// ErrRegistryLocked — deterministic, unlike the concurrent-writes test
// (which stays as the end-to-end integrity check).

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/storage/lockfile"
)

func TestMutateWaitsForLockThenRefuses(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()

	// No contention: the write goes through and the mutation saw the
	// document it loaded.
	if err := Mutate(root, func(r *Registry) error {
		r.Projects = append(r.Projects, Entry{Path: Canonical(dir), Added: time.Now().UTC()})
		return nil
	}); err != nil {
		t.Fatalf("uncontended Mutate: %v", err)
	}

	// A held lock: bounded retry, then the typed refusal — never a
	// write under the holder.
	lock, err := lockfile.AcquirePathLock(LockPath(root))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	other := t.TempDir()
	start := time.Now()
	err = Mutate(root, func(r *Registry) error {
		r.Projects = append(r.Projects, Entry{Path: Canonical(other), Added: time.Now().UTC()})
		return nil
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrRegistryLocked) {
		t.Fatalf("held-lock Mutate: %v, want ErrRegistryLocked", err)
	}
	// The budget is 100ms/2ms; the refusal must have retried (not
	// instant) and not blown far past the budget.
	if elapsed < 90*time.Millisecond {
		t.Fatalf("the refusal was instant (%v) — no retry", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the refusal took %v — the budget is not bounded", elapsed)
	}

	// Nothing was written under the holder.
	registry, err := Load(FilePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup(Canonical(other)); ok {
		t.Fatal("Mutate wrote while the lock was held")
	}

	// After release, the same write succeeds.
	lock.Release()
	if err := Mutate(root, func(r *Registry) error {
		r.Projects = append(r.Projects, Entry{Path: Canonical(other), Added: time.Now().UTC()})
		return nil
	}); err != nil {
		t.Fatalf("post-release Mutate: %v", err)
	}
	_ = os.RemoveAll(filepath.Join(root, "projects.json"))
}
