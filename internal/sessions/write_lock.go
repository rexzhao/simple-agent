package sessions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const sessionWriteLockFileName = "write.lock"

// SessionWriteLock is a per-session cross-process writer lock.
type SessionWriteLock struct {
	file     *os.File
	path     string
	released bool
}

// AcquireSessionWriteLock acquires the per-session writer lock.
func (s *V2Store) AcquireSessionWriteLock(ctx context.Context, sessionID string) (*SessionWriteLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	if err := validateV2SessionID(sessionID); err != nil {
		return nil, err
	}

	path := s.sessionWriteLockPath(sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create session directory %q: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session write lock %q: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("chmod session write lock %q: %w", path, err)
	}
	if err := lockSessionWriteFile(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire session write lock %q: %w", path, err)
	}
	return &SessionWriteLock{file: file, path: path}, nil
}

// Path returns the lock file path.
func (l *SessionWriteLock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Release releases the session writer lock.
func (l *SessionWriteLock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	return errors.Join(unlockSessionWriteFile(l.file), l.file.Close())
}

func (s *V2Store) sessionWriteLockPath(sessionID string) string {
	return filepath.Join(s.sessionDir(sessionID), sessionWriteLockFileName)
}

func lockSessionWriteFile(ctx context.Context, file *os.File) error {
	for {
		locked, err := tryLockSessionWriteFile(file)
		if err != nil {
			return err
		}
		if locked {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
