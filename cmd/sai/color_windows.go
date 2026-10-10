//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// enableVirtualTerminal turns on ANSI escape processing for the legacy
// Windows console (conhost). Windows Terminal already processes them;
// without this call, escape sequences would print literally on older
// consoles.
func enableVirtualTerminal() bool {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getStdHandle := kernel32.NewProc("GetStdHandle")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	const stdOutputHandle = ^uintptr(10) // -11
	handle, _, _ := getStdHandle.Call(stdOutputHandle)
	if handle == 0 || syscall.Handle(handle) == syscall.InvalidHandle {
		return false
	}
	var mode uint32
	if ok, _, _ := getConsoleMode.Call(handle, uintptr(unsafe.Pointer(&mode))); ok == 0 {
		return false
	}
	const enableProcessedVirtualTerminal = 0x0004
	if mode&enableProcessedVirtualTerminal != 0 {
		return true
	}
	ok, _, _ := setConsoleMode.Call(handle, uintptr(mode|enableProcessedVirtualTerminal))
	return ok != 0
}
