//go:build windows

package store

import (
	"os"
	"time"
)

// renameWithRetry 带重试地改名。
//
// Windows 上杀毒软件/搜索索引可能短暂占用文件，导致 rename 失败（D7）；
// 重试几次即可，不需要用户干预。
func renameWithRetry(from, to string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// syncDir 在 Windows 上没有对应操作（目录不能 fsync）。
func syncDir(string) error { return nil }
