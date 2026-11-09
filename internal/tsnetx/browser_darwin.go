//go:build darwin

package tsnetx

import "os/exec"

// openBrowser 用系统默认浏览器打开 URL（macOS）。
func openBrowser(url string) error {
	return exec.Command("open", url).Start()
}
