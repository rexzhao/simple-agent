//go:build !windows

package sessions

import (
	"os"
	"syscall"
)

func tryLockSessionWriteFile(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return false, nil
	}
	return false, err
}

func unlockSessionWriteFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
