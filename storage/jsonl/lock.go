package jsonl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/storage/lockfile"
)

// fileLock is an OS-level exclusive lock on one session directory. Two
// processes appending to the same jsonl log would corrupt it (the open
// mode has no O_APPEND; writes advance each process's own offset), so a
// session admits exactly one writer process. The lock is advisory but
// enforced at every open path of this store.
//
// The lock file itself is only the target of the OS primitive. Holder
// information for error messages lives in a sibling "holder" file: on
// Windows the locked byte range cannot be read by the loser, so the pid
// must sit outside the lock file. The holder file is informational —
// mutual exclusion never depends on it, and a crashed holder's stale
// entry does not block anyone (the OS releases the lock on death).
type fileLock struct {
	file   *os.File
	holder string
}

// lockSession acquires the session lock, creating the directory if it
// does not exist yet (a lazily created session materializes here).
func lockSession(dir string) (*fileLock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	path := filepath.Join(dir, "lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open session lock: %w", err)
	}
	if err := lockfile.LockExclusive(file); err != nil {
		file.Close()
		holder := readLockHolder(filepath.Join(dir, "holder"))
		return nil, &lockfile.LockHeldError{Target: "session " + filepath.Base(dir), Holder: holder}
	}
	lock := &fileLock{file: file, holder: filepath.Join(dir, "holder")}
	// Best-effort holder info for the next loser's error message.
	_ = os.WriteFile(lock.holder, []byte(fmt.Sprintf("pid %d\n", os.Getpid())), 0o644)
	return lock, nil
}

// Release drops the lock and the holder marker.
func (l *fileLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = os.Remove(l.holder)
	l.file.Close()
	l.file = nil
}

// readLockHolder reads the holder note for the loser's error message.
// The winner writes the note immediately AFTER taking the lock, and a
// loser can open inside that gap (or read a partially-written file
// under concurrent IO on Windows) — a bounded retry closes the window.
// The note is informational: a genuine absence (a winner deleted it on
// release while still holding the lock) still degrades to the plain
// message once the budget expires.
func readLockHolder(path string) string {
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		if data, err := os.ReadFile(path); err == nil {
			// The note is written with a trailing newline
			// ("pid %d" + LF; it must not leak into the error
			// message — a "holder: pid 123" line break inside the
			// "(holder: ...; may be stale)" tail is mojibake on a
			// terminal).
			holder := strings.TrimSpace(string(data))
			if len(holder) > 200 {
				holder = holder[:200]
			}
			if holder != "" {
				return holder
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(2 * time.Millisecond)
	}
}
