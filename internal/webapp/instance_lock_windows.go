//go:build windows

package webapp

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	instanceLockFileExclusiveLock   = 0x00000002
	instanceLockFileFailImmediately = 0x00000001
	instanceErrorLockViolation      = syscall.Errno(33)
)

var (
	instanceKernel32Proc     = syscall.NewLazyDLL("kernel32.dll")
	instanceLockFileExProc   = instanceKernel32Proc.NewProc("LockFileEx")
	instanceUnlockFileExProc = instanceKernel32Proc.NewProc("UnlockFileEx")
)

func tryLockInstanceFile(file *os.File) (bool, error) {
	var overlapped syscall.Overlapped
	result, _, err := instanceLockFileExProc.Call(
		uintptr(syscall.Handle(file.Fd())),
		uintptr(instanceLockFileExclusiveLock|instanceLockFileFailImmediately),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if result != 0 {
		return true, nil
	}
	if err == instanceErrorLockViolation {
		return false, nil
	}
	if err == syscall.Errno(0) {
		return false, syscall.EINVAL
	}
	return false, err
}

func unlockInstanceFile(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, err := instanceUnlockFileExProc.Call(
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
