//go:build !windows

package main

// enableVirtualTerminal is a no-op outside Windows: ANSI escapes are the
// terminal default there.
func enableVirtualTerminal() bool { return true }
