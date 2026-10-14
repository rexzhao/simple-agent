//go:build !windows

package model

import "syscall"

// retryableConnErrnos lists connection-level errnos treated as transient on
// Unix platforms.
var retryableConnErrnos = []syscall.Errno{
	syscall.ECONNRESET,
	syscall.ECONNABORTED,
	syscall.ECONNREFUSED,
	syscall.EPIPE,
}
