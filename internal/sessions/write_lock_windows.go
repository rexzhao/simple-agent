//go:build windows

package sessions

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	sessionLockFileExclusiveLock   = 0x00000002
	sessionLockFileFailImmediately = 0x00000001
	sessionErrorLockViolation      = syscall.Errno(33)
)

var (
	sessionKernel32Proc     = syscall.NewLazyDLL("kernel32.dll")
	sessionLockFileExProc   = sessionKernel32Proc.NewProc("LockFileEx")
	sessionUnlockFileExProc = sessionKernel32Proc.NewProc("UnlockFileEx")
)

func tryLockSessionWriteFile(file *os.File) (bool, error) {
	var overlapped syscall.Overlapped
	result, _, err := sessionLockFileExProc.Call(
		uintptr(syscall.Handle(file.Fd())),
		uintptr(sessionLockFileExclusiveLock|sessionLockFileFailImmediately),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if result != 0 {
		return true, nil
	}
	if err == sessionErrorLockViolation {
		return false, nil
	}
	if err == syscall.Errno(0) {
		return false, syscall.EINVAL
	}
	return false, err
}

func unlockSessionWriteFile(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, err := sessionUnlockFileExProc.Call(
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
