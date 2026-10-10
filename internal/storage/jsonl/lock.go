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
		return nil, &LockHeldError{Target: "session " + filepath.Base(dir), Holder: holder}
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

// LockHeldError reports a session (or path) lock held by another process.
// Callers classify it with errors.As — never by parsing the message. The
// holder string is informational (a pid note for humans); mutual exclusion
// never depends on it.
type LockHeldError struct {
	Target string
	Holder string
}

func (e *LockHeldError) Error() string {
	if e.Holder == "" {
		return fmt.Sprintf("%s is locked by another process", e.Target)
	}
	return fmt.Sprintf("%s is locked by another process (holder: %s; may be stale)", e.Target, e.Holder)
}

// AcquirePathLock takes an exclusive OS lock on a dedicated file that is
// never renamed or replaced — the serve-mode root lock (<serverRoot>/
// serve.lock). Locking a file that atomic writers replace (rename) would
// leave the lock stranded on the old inode while the path points at a new
// one; dedicated files have no such lifecycle.
func AcquirePathLock(path string) (*PathLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockFileExclusive(file); err != nil {
		file.Close()
		return nil, &LockHeldError{Target: path}
	}
	return &PathLock{file: file}, nil
}

// PathLock is a held dedicated-file lock.
type PathLock struct {
	file *os.File
}

// Release drops the lock.
func (p *PathLock) Release() {
	if p == nil || p.file == nil {
		return
	}
	p.file.Close()
	p.file = nil
}
