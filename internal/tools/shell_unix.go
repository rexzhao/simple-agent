//go:build unix

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

func newShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}

type unixShellCommandController struct {
	cmd  *exec.Cmd
	once sync.Once
	err  error
}

func newShellCommandController(cmd *exec.Cmd) shellCommandController {
	controller := &unixShellCommandController{cmd: cmd}
	cmd.WaitDelay = shellCancelWaitDelay
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = controller.terminateProcessGroup
	return controller
}

func (c *unixShellCommandController) Run(cmd *exec.Cmd) error {
	err := cmd.Run()
	// A shell may exit successfully after spawning a background child. The
	// tool lifecycle owns that child as well, so do not let it outlive Run.
	c.Close()
	return err
}

func (c *unixShellCommandController) Close() {
	c.once.Do(func() {
		c.err = killUnixProcessGroup(c.cmd)
	})
}

func (c *unixShellCommandController) terminateProcessGroup() error {
	c.Close()
	return c.err
}

func killUnixProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return os.ErrProcessDone
	}
	process, err := os.FindProcess(-cmd.Process.Pid)
	if err != nil {
		return err
	}
	err = process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return os.ErrProcessDone
	}
	return err
}
