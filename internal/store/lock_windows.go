//go:build windows

package store

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

// DirLock 是数据目录的独占锁，防止两个进程同时写同一份日志。
//
// 用操作系统的文件锁（LockFileEx），**不用「锁文件是否存在」**判断：
// 崩溃后锁文件会残留，那样程序就再也打不开了。文件锁由内核在进程退出时释放。
type DirLock struct {
	file *os.File
}

// AcquireDirLock 抢占数据目录锁。第二个进程会失败（而不是阻塞）。
func AcquireDirLock(dir string) (*DirLock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败：%w", err)
	}
	path := dir + string(os.PathSeparator) + "simple-agent.lock"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件失败：%w", err)
	}

	handle := syscall.Handle(file.Fd())
	var overlapped syscall.Overlapped
	ret, _, callErr := procLockFileEx.Call(
		uintptr(handle),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0, 1, 0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if ret == 0 {
		_ = file.Close()
		return nil, &LockedError{Path: path, Err: callErr}
	}
	return &DirLock{file: file}, nil
}

// Release 释放锁。
func (l *DirLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	handle := syscall.Handle(l.file.Fd())
	var overlapped syscall.Overlapped
	_, _, _ = procUnlockFileEx.Call(
		uintptr(handle), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)),
	)
	err := l.file.Close()
	l.file = nil
	return err
}
