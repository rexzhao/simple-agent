package main

// The CLI's registry writes go through the file lock (C8, §17.4):
// `sai project add` racing a running serve's register endpoint
// serializes on <root>/projects.json.lock — while the lock is held
// (here: by the test, playing the serve), the add retries briefly
// and then refuses with the registry-locked error instead of writing
// under the holder.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

func TestProjectAddWaitsForRegistryLock(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	oldRoot := resolvedServerRoot
	resolvedServerRoot = root
	t.Cleanup(func() { resolvedServerRoot = oldRoot })

	// A plain add (no contention) succeeds and canonicalizes.
	if code := projectAdd(dir); code != 0 {
		t.Fatalf("plain add: exit %d", code)
	}
	registry, err := projects.Load(projects.FilePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup(projects.Canonical(dir)); !ok {
		t.Fatal("the plain add did not register")
	}

	// While the lock is HELD (the test plays a concurrent serve's
	// register), the add retries briefly, then refuses — it must NOT
	// write under the holder.
	lock, err := jsonl.AcquirePathLock(projects.LockPath(root))
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()

	stderr := captureStderr(t, func() {
		if code := projectAdd(other); code != 1 {
			t.Fatalf("locked add: exit %d, want 1", code)
		}
	})
	if !strings.Contains(stderr, "registry locked") {
		t.Fatalf("locked add stderr = %q, want the registry-locked refusal", stderr)
	}

	// Nothing was written under the holder.
	lock.Release()
	registry, err = projects.Load(projects.FilePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup(projects.Canonical(other)); ok {
		t.Fatal("the add wrote while the lock was held")
	}

	// After release, the add goes through.
	if code := projectAdd(other); code != 0 {
		t.Fatalf("post-release add: exit %d", code)
	}
	_ = os.RemoveAll(filepath.Join(root, "data"))
}
