//go:build !windows && !unix

package tools

import (
	"os"
	"os/exec"
)

type directShellCommandController struct{}

func newShellCommandController(cmd *exec.Cmd) shellCommandController {
	cmd.WaitDelay = shellCancelWaitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Kill()
	}
	return directShellCommandController{}
}

func (directShellCommandController) Run(cmd *exec.Cmd) error { return cmd.Run() }
func (directShellCommandController) Close()                  {}
