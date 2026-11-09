//go:build windows

package tsnetx

import "os/exec"

// openBrowser 用系统默认浏览器打开 URL（Windows）。
//
// 用 rundll32 而不是 cmd /c start：后者会把 URL 里的 & 当成命令分隔符，
// 登录链接里带查询参数时会被截断。
func openBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
