//go:build windows

package projects

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	projectLockFileExclusiveLock   = 0x00000002
	projectLockFileFailImmediately = 0x00000001
	projectErrorLockViolation      = syscall.Errno(33)
)

var (
	projectKernel32Proc     = syscall.NewLazyDLL("kernel32.dll")
	projectLockFileExProc   = projectKernel32Proc.NewProc("LockFileEx")
	projectUnlockFileExProc = projectKernel32Proc.NewProc("UnlockFileEx")
)

func tryLockProjectCreateFile(file *os.File) (bool, error) {
	var overlapped syscall.Overlapped
	result, _, err := projectLockFileExProc.Call(
		uintptr(syscall.Handle(file.Fd())),
		uintptr(projectLockFileExclusiveLock|projectLockFileFailImmediately),
		0, 1, 0, uintptr(unsafe.Pointer(&overlapped)),
	)
	if result != 0 {
		return true, nil
	}
	if err == projectErrorLockViolation {
		return false, nil
	}
	if err == syscall.Errno(0) {
		return false, syscall.EINVAL
	}
	return false, err
}

func unlockProjectCreateFile(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, err := projectUnlockFileExProc.Call(
		uintptr(syscall.Handle(file.Fd())), 0, 1, 0,
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
