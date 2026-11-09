//go:build !windows

package tools

import (
	"context"
	"os/exec"
	"syscall"
)

// newShellCmd 构造执行命令的 *exec.Cmd：Setpgid 让子进程成为新进程组的组长，
// 这样取消时 kill(-pgid) 能一次结束整棵进程树。
func newShellCmd(ctx context.Context, kind shellKind, command string) *exec.Cmd {
	var cmd *exec.Cmd
	if kind == shellPwsh {
		cmd = exec.CommandContext(ctx, "pwsh", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// killProcessTree 向进程组发送 SIGKILL（负 pid = 整个组）。
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
