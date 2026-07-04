package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const registryStartupLockFileName = "startup.lock"

// StartupLock is the per-home namespace server startup lock.
type StartupLock struct {
	file     *os.File
	path     string
	released bool
}

// StartupLockPathForHome returns the lock file path for a home namespace.
func StartupLockPathForHome(home string) (string, error) {
	home, err := CanonicalPath(home)
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, registrySubdirName, registryStartupLockFileName), nil
}

// AcquireStartupLock acquires the per-home server startup lock.
func AcquireStartupLock(ctx context.Context, home string) (*StartupLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path, err := StartupLockPathForHome(home)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create server registry dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open server startup lock %q: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("chmod server startup lock %q: %w", path, err)
	}
	if err := lockStartupFile(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire server startup lock %q: %w", path, err)
	}
	return &StartupLock{file: file, path: path}, nil
}

// Path returns the lock file path.
func (l *StartupLock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Release releases the startup lock.
func (l *StartupLock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	return errors.Join(unlockStartupFile(l.file), l.file.Close())
}

func lockStartupFile(ctx context.Context, file *os.File) error {
	for {
		locked, err := tryLockStartupFile(file)
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
