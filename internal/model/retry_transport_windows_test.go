//go:build windows

package model

import (
	"net"
	"os"
	"syscall"
	"testing"
)

func TestIsRetryableConnErrorWindowsErrnos(t *testing.T) {
	tests := []struct {
		name  string
		errno syscall.Errno
		want  bool
	}{
		{"WSAECONNRESET", syscall.Errno(10054), true},
		{"WSAECONNABORTED", syscall.Errno(10053), true},
		{"ERROR_BROKEN_PIPE", syscall.Errno(109), true},
		{"WSAECONNREFUSED", syscall.Errno(10061), true},
		{"WSAEINVAL", syscall.Errno(10022), false},
		{"WSAETIMEDOUT", syscall.Errno(10060), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", test.errno)}
			if got := isRetryableConnError(err); got != test.want {
				t.Fatalf("isRetryableConnError(%v) = %v, want %v", err, got, test.want)
			}
		})
	}
}

// TestInventedErrnoDoesNotMatchRealWSA guards the platform split: the
// BSD-style constants on Windows are invented values and must never be used
// for matching real WSA errnos.
func TestInventedErrnoDoesNotMatchRealWSA(t *testing.T) {
	realReset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", syscall.Errno(10054))}
	if !isRetryableConnError(realReset) {
		t.Fatal("real WSAECONNRESET must be retryable")
	}
	if syscall.ECONNRESET == syscall.WSAECONNRESET {
		t.Skip("platform maps ECONNRESET to WSA value")
	}
	if isRetryableConnError(syscall.ECONNRESET) {
		t.Fatal("invented ECONNRESET value must not be treated as a real connection reset")
	}
}
