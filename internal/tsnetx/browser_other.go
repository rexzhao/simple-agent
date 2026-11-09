//go:build !windows && !darwin

package tsnetx

import (
	"errors"
	"os/exec"
)

// openBrowser 用系统默认浏览器打开 URL（Linux 等）。
//
// 没有 xdg-open 的环境（容器、无桌面发行版）直接返回错误，
// 调用方会提示用户手动访问日志里的 URL。
func openBrowser(url string) error {
	path, err := exec.LookPath("xdg-open")
	if err != nil {
		return errors.New("系统里没有 xdg-open")
	}
	return exec.Command(path, url).Start()
}
