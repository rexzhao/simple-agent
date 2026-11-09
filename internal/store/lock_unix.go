//go:build !windows

package store

import (
	"fmt"
	"os"
	"syscall"
)

// DirLock 是数据目录的独占锁，防止两个进程同时写同一份日志。
//
// 用 flock，**不用「锁文件是否存在」**判断：崩溃后锁文件会残留，
// 那样程序就再也打不开了。flock 的锁随进程退出自动释放。
type DirLock struct {
	file *os.File
}

// AcquireDirLock 抢占数据目录锁。第二个进程会失败（而不是阻塞）。
func AcquireDirLock(dir string) (*DirLock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败：%w", err)
	}
	path := dir + "/simple-agent.lock"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件失败：%w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, &LockedError{Path: path, Err: err}
	}
	return &DirLock{file: file}, nil
}

// Release 释放锁。
func (l *DirLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
