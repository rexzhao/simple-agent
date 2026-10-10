package jsonl

import (
	"fmt"
	"os"
	"path/filepath"
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
	if err := lockFileExclusive(file); err != nil {
		file.Close()
		holder := readLockHolder(filepath.Join(dir, "holder"))
		if holder == "" {
			return nil, fmt.Errorf("session %s is locked by another process", filepath.Base(dir))
		}
		return nil, fmt.Errorf("session %s is locked by another process (holder: %s; may be stale)", filepath.Base(dir), holder)
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

func readLockHolder(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	holder := string(data)
	if len(holder) > 200 {
		holder = holder[:200]
	}
	return holder
}
