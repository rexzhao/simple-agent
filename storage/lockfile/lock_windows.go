//go:build windows

package lockfile

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	lockFileExProc = kernel32.NewProc("LockFileEx")
)

const (
	lockfileExclusiveLock   = 0x0002
	lockfileFailImmediately = 0x0001
)

// LockExclusive takes a non-blocking exclusive byte-range lock via
// LockFileEx. The lock lives on the file handle: it is released by
// CloseHandle (and by process death, which is what makes crash recovery
// automatic).
func LockExclusive(file *os.File) error {
	// Lock the whole file: offset 0, length max (0 wraps to the maximum
	// range on NTFS).
	var overlapped syscall.Overlapped
	r1, _, err := lockFileExProc.Call(
		file.Fd(),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		uintptr(^uint32(0)), uintptr(^uint32(0)), // length: 64-bit max
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 == 0 {
		return fmt.Errorf("LockFileEx: %w", err)
	}
	return nil
}
