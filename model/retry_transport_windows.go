//go:build windows

package model

import "syscall"

// wsaECONNREFUSED is not exported by the standard library's Windows syscall
// package; define the WSA value here. Note that the BSD-style constants such
// as syscall.ECONNRESET exist on Windows only as invented values
// (APPLICATION_ERROR + iota) and never match real WSA errnos.
const wsaECONNREFUSED syscall.Errno = 10061

// retryableConnErrnos lists connection-level errnos treated as transient on
// Windows. Real socket errors surface as WSA errno values.
var retryableConnErrnos = []syscall.Errno{
	syscall.WSAECONNRESET,
	syscall.WSAECONNABORTED,
	syscall.ERROR_BROKEN_PIPE,
	wsaECONNREFUSED,
}
