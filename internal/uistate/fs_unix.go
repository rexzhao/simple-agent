//go:build !windows

package uistate

import "os"

// renameWithRetry 在 POSIX 上 rename 是原子的，失败基本是真实错误。
func renameWithRetry(from, to string) error { return os.Rename(from, to) }
