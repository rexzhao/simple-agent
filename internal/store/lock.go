package store

import "fmt"

// LockedError 表示数据目录已被另一个进程占用。
type LockedError struct {
	Path string
	Err  error
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("数据目录已被另一个 simple-agent 实例占用（%s）：%v", e.Path, e.Err)
}

func (e *LockedError) Unwrap() error { return e.Err }
