// Package lockfile is the OS-level exclusive file-lock primitive:
// one process may hold a path at a time, crash releases it, and a
// contender learns WHO holds it when it can.
//
// Two flavors over one primitive:
//
//   - AcquirePathLock: a dedicated lock file that is never renamed or
//     replaced — the serve-mode root lock, the projects registry
//     lock. Locking a file that atomic writers replace (rename) would
//     strand the lock on the old inode while the path points at a new
//     one; dedicated files have no such lifecycle.
//   - LockExclusive: lock an ALREADY-OPEN file — the caller owns the
//     file's lifecycle (the jsonl session lock: the lock file is the
//     target of the primitive, holder info sits in a sibling file).
//
// The lock is advisory but crash-safe: the OS releases it on process
// death, so a crashed holder never blocks anyone.
//
// Compatibility: v0 — NO stability guarantees. Breaking changes land
// without notice until v1 is declared. If you embed this package, pin
// an exact commit.
package lockfile

import (
	"fmt"
	"os"
)

// LockHeldError reports a lock held by another process. Holder is the
// best-effort note the winner left (informational only — mutual
// exclusion never depends on it, and a crashed holder's stale note
// does not block anyone).
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

// AcquirePathLock takes an exclusive OS lock on a dedicated file that
// is never renamed or replaced — the serve-mode root lock
// (<serverRoot>/serve.lock), the projects registry lock
// (projects.json.lock). Locking a file that atomic writers replace
// (rename) would leave the lock stranded on the old inode while the
// path points at a new one; dedicated files have no such lifecycle.
func AcquirePathLock(path string) (*PathLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := LockExclusive(file); err != nil {
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
