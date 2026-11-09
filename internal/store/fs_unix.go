//go:build !windows

package store

import (
	"fmt"
	"os"
)

// renameWithRetry 在 POSIX 上 rename 是原子的，失败基本是真实错误。
func renameWithRetry(from, to string) error { return os.Rename(from, to) }

// syncDir 把目录项刷到磁盘：rename 之后不做这一步，断电可能丢掉改名结果（S1）。
func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("打开目录失败：%w", err)
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("同步目录失败：%w", err)
	}
	return nil
}
