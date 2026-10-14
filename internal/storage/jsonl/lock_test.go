package jsonl

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/storage"
)

// TestSessionLockSecondOpenFails verifies the single-writer invariant: a
// second Open of a session held by another process fails and names the
// holder, instead of silently letting two writers interleave offsets.
func TestSessionLockSecondOpenFails(t *testing.T) {
	if os.Getenv("SAI_LOCK_CHILD") != "" {
		// Holder child: open the session (retrying briefly against the
		// parent's probe loop) and idle until killed.
		store, err := OpenStore(os.Getenv("SAI_LOCK_ROOT"))
		if err != nil {
			os.Exit(2)
		}
		var session storage.Session
		for attempt := 0; attempt < 500; attempt++ {
			session, err = store.Open(os.Getenv("SAI_LOCK_NS"), os.Getenv("SAI_LOCK_SESSION"))
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			os.Exit(3)
		}
		defer session.Close()
		// Sleep, not select{}: a bare select{} trips the runtime deadlock
		// detector and exits immediately, releasing the lock within
		// milliseconds. The parent kills this process when done.
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}

	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create("p" + strings.Repeat("0", 16))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	id := session.ID()
	ns := "p" + strings.Repeat("0", 16)
	// The parent must let go of the lock before the child can take it.
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	// A helper process holds the session lock on the same root.
	cmd := exec.Command(os.Args[0], "-test.run", "^TestSessionLockSecondOpenFails$")
	cmd.Env = append(os.Environ(),
		"SAI_LOCK_CHILD=1",
		"SAI_LOCK_ROOT="+root,
		"SAI_LOCK_NS="+ns,
		"SAI_LOCK_SESSION="+id,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	// Wait until the child actually holds the lock. Every probe that
	// succeeds is closed immediately — a leaked open would hold the
	// lock ourselves and defeat the whole point.
	for i := 0; i < 500; i++ {
		probe, err := store.Open(ns, id)
		if err != nil {
			break
		}
		probe.Close()
		time.Sleep(10 * time.Millisecond)
	}
	second, err := store.Open(ns, id)
	if err == nil {
		second.Close()
		t.Fatal("second Open of a locked session succeeded")
	}
	if !strings.Contains(err.Error(), "locked by another process") {
		t.Fatalf("error does not name the lock: %v", err)
	}
	if !strings.Contains(err.Error(), "pid") {
		t.Fatalf("error does not carry holder info: %v", err)
	}

	// Kill the holder: the OS releases the lock, and the session
	// reopens (the stale pid file must not block it).
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	reopened, err := store.Open(ns, id)
	if err != nil {
		t.Fatalf("Open after holder death failed: %v", err)
	}
	reopened.Close()
}

// TestSessionLockReleaseOnClose verifies the lock is released by Close in
// the same process.
func TestSessionLockReleaseOnClose(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ns := "p" + strings.Repeat("0", 16)
	session, err := store.Create(ns)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(storage.Record{ItemID: "u1", Type: storage.TypeUserInput, Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ns, id)
	if err != nil {
		t.Fatalf("Open after Close failed: %v", err)
	}
	reopened.Close()
}

// TestReadLockHolderRetriesThroughWriteGap pins the loser-side retry:
// the winner writes the holder note microseconds AFTER taking the lock,
// so a loser opening in that gap (or reading a partial file under
// Windows concurrent IO) must retry briefly rather than report the lock
// without holder info.
func TestReadLockHolderRetriesThroughWriteGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "holder")
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(path, []byte("pid 123\n"), 0o644)
	}()
	start := time.Now()
	holder := readLockHolder(path)
	if holder != "pid 123\n" {
		t.Fatalf("holder = %q, want the late-written content", holder)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("returned in %v without retrying through the write gap", elapsed)
	}
}
