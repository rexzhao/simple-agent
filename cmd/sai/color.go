package main

import "os"

// ANSI color codes for the reasoning display. Gray (bright black) marks
// the model's thinking as visually secondary to the answer.
const (
	ansiGray  = "\x1b[90m"
	ansiReset = "\x1b[0m"
)

// colorEnabled gates every escape sequence: piped or redirected output
// must stay plain. Set once at startup from a TTY check.
var colorEnabled = false

// gray wraps text in the gray color when enabled. Every colored span is
// self-contained — the reset always follows in the same write — so the
// terminal state is restored even if a stream is interrupted mid-color
// (a lingering color would dye the stderr spinner and later output too).
func gray(text string) string {
	if !colorEnabled || text == "" {
		return text
	}
	return ansiGray + text + ansiReset
}

// stdoutIsTerminal reports whether stdout is an interactive terminal
// (a character device), as opposed to a pipe or a file.
func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
