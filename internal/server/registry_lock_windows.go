//go:build windows

package server

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	lockFileExclusiveLock   = 0x00000002
	lockFileFailImmediately = 0x00000001
	errorLockViolation      = syscall.Errno(33)
)

var (
	kernel32Proc     = syscall.NewLazyDLL("kernel32.dll")
	lockFileExProc   = kernel32Proc.NewProc("LockFileEx")
	unlockFileExProc = kernel32Proc.NewProc("UnlockFileEx")
)

func tryLockStartupFile(file *os.File) (bool, error) {
	var overlapped syscall.Overlapped
	result, _, err := lockFileExProc.Call(
		uintptr(syscall.Handle(file.Fd())),
		uintptr(lockFileExclusiveLock|lockFileFailImmediately),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if result != 0 {
		return true, nil
	}
	if err == errorLockViolation {
		return false, nil
	}
	if err == syscall.Errno(0) {
		return false, syscall.EINVAL
	}
	return false, err
}

func unlockStartupFile(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, err := unlockFileExProc.Call(
		uintptr(syscall.Handle(file.Fd())),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if result != 0 {
		return nil
	}
	if err == syscall.Errno(0) {
		return syscall.EINVAL
	}
	return err
}
