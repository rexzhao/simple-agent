//go:build !windows

package model

import (
	"net"
	"os"
	"syscall"
	"testing"
)

func TestIsRetryableConnErrorUnixErrnos(t *testing.T) {
	tests := []struct {
		name  string
		errno syscall.Errno
		want  bool
	}{
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"ECONNABORTED", syscall.ECONNABORTED, true},
		{"ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"EPIPE", syscall.EPIPE, true},
		{"EINVAL", syscall.EINVAL, false},
		{"ETIMEDOUT", syscall.ETIMEDOUT, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", test.errno)}
			if got := isRetryableConnError(err); got != test.want {
				t.Fatalf("isRetryableConnError(%v) = %v, want %v", err, got, test.want)
			}
		})
	}
}
