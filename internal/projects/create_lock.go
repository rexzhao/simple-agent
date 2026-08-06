package projects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type projectCreateLock struct {
	file     *os.File
	path     string
	released bool
}

func (s *Store) acquireProjectOperationLock(ctx context.Context, operationID string) (*projectCreateLock, error) {
	return s.acquireProjectLock(ctx, "operation-"+projectOperationHash(operationID)+".lock")
}

func (s *Store) acquireProjectCreateLock(ctx context.Context, projectID string) (*projectCreateLock, error) {
	return s.acquireProjectLock(ctx, "root-"+projectID+".lock")
}

func (s *Store) acquireProjectLock(ctx context.Context, name string) (*projectCreateLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.requireRoot(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.projectOperationLocksDir(), 0o700); err != nil {
		return nil, fmt.Errorf("create project operation lock directory: %w", err)
	}
	path := filepath.Join(s.projectOperationLocksDir(), name)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open project operation lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("chmod project operation lock: %w", err)
	}
	if err := lockProjectCreateFile(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire project operation lock: %w", err)
	}
	return &projectCreateLock{file: file, path: path}, nil
}

func (l *projectCreateLock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	return errors.Join(unlockProjectCreateFile(l.file), l.file.Close())
}

func lockProjectCreateFile(ctx context.Context, file *os.File) error {
	for {
		locked, err := tryLockProjectCreateFile(file)
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
