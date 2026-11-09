//go:build !windows

package tools

import (
	"strings"
	"unicode/utf8"
)

// decodeConsole 在非 Windows 上只需保证结果是合法 UTF-8。
func decodeConsole(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	for i := 0; i < 4 && len(data) > 0; i++ {
		r, size := utf8.DecodeLastRune(data)
		if r != utf8.RuneError || size > 1 {
			break
		}
		data = data[:len(data)-1]
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}
