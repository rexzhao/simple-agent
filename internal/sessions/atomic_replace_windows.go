//go:build windows

package sessions

import (
	"errors"
	"syscall"
)

const (
	windowsErrorAccessDenied     syscall.Errno = 5
	windowsErrorSharingViolation syscall.Errno = 32
)

func isRetryableAtomicReplaceError(err error) bool {
	return errors.Is(err, windowsErrorSharingViolation) || errors.Is(err, windowsErrorAccessDenied)
}
