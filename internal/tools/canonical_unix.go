//go:build !windows

package tools

import "path/filepath"

// canonicalExisting 返回解析所有符号链接后的真实路径。
func canonicalExisting(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
