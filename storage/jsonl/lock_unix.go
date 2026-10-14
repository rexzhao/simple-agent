//go:build !windows

package jsonl

import (
	"fmt"
	"os"
	"syscall"
)

// lockFileExclusive takes a non-blocking exclusive flock. The lock lives
// on the open file description: it is released by Close (and by process
// death, which is what makes crash recovery automatic).
func lockFileExclusive(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	return nil
}
