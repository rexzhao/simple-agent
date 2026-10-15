package lockfile

// The package's own contract (C9a): contention produces the typed
// error with the holder-less message, Release is idempotent and
// nil-safe, and both message shapes render on one line.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquirePathLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dedicated.lock")

	first, err := AcquirePathLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()

	_, err = AcquirePathLock(path)
	var held *LockHeldError
	if !errors.As(err, &held) {
		t.Fatalf("second acquire: %v, want LockHeldError", err)
	}
	// A dedicated lock file has no holder note — the plain message.
	if held.Error() != path+" is locked by another process" {
		t.Fatalf("message = %q", held.Error())
	}

	first.Release()
	again, err := AcquirePathLock(path)
	if err != nil {
		t.Fatalf("post-release acquire: %v", err)
	}
	again.Release()
}

func TestReleaseIdempotentAndNilSafe(t *testing.T) {
	lock, err := AcquirePathLock(filepath.Join(t.TempDir(), "x.lock"))
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
	lock.Release() // idempotent

	var nilLock *PathLock
	nilLock.Release() // nil-safe
}

func TestHeldErrorMessages(t *testing.T) {
	plain := (&LockHeldError{Target: "serve.lock"}).Error()
	if plain != "serve.lock is locked by another process" {
		t.Fatalf("plain = %q", plain)
	}
	note := (&LockHeldError{Target: "serve.lock", Holder: "pid 42"}).Error()
	want := "serve.lock is locked by another process (holder: pid 42; may be stale)"
	if note != want {
		t.Fatalf("holder note = %q, want %q", note, want)
	}
	if strings.ContainsAny(note, "\n\r") {
		t.Fatalf("the message breaks a line: %q", note)
	}
}
