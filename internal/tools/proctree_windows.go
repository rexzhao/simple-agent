//go:build windows

package tools

import (
	"context"
	"encoding/base64"
	"os/exec"
	"strconv"
	"syscall"
	"unicode/utf16"
)

// createNoWindow：不分配控制台窗口。GUI 程序（我们的应用没有控制台）调用
// cmd/taskkill 时，若不加这个标志会闪一下黑窗；HideWindow 只隐藏窗口，
// 控制台照样会被分配。
const createNoWindow = 0x08000000

// newShellCmd 构造执行命令的 *exec.Cmd。
//
// **不能用 exec.CommandContext(ctx, "cmd", "/C", command)**：Go 按
// CommandLineToArgvW 规则转义参数（把 `"` 变成 `\"`），而 cmd.exe 不认这套规则
// （golang/go#17149）。结果是 `echo "a b"` 变成 `echo \"a b\"`，输出里带反斜杠。
// 这会让带空格的路径、`git commit -m "msg"` 这类最常见用法全部出错。
// 所以直接给完整命令行（SysProcAttr.CmdLine），绕开 Go 的拼参数逻辑。
func newShellCmd(ctx context.Context, kind shellKind, command string) *exec.Cmd {
	attr := &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}

	var cmd *exec.Cmd
	if kind == shellPwsh {
		cmd = exec.CommandContext(ctx, "powershell")
		// -EncodedCommand 传 UTF-16LE base64：绕开所有引号与转义问题。
		// 同时把控制台输出编码强制成 UTF-8，否则中文输出是 GBK 字节。
		script := "[Console]::OutputEncoding = [Text.Encoding]::UTF8; " + command
		attr.CmdLine = "powershell -NoProfile -NonInteractive -EncodedCommand " + encodePowerShell(script)
	} else {
		cmd = exec.CommandContext(ctx, "cmd")
		// /S 让 cmd 只剥掉最外层的一对引号，内部引号原样保留。
		//
		// 不拼 chcp 65001：它在没有控制台的 GUI 进程里不生效（实测），是死代码；
		// 而且会给模型发来的命令多叠一层 & 语义。编码问题交给 decodeConsole。
		attr.CmdLine = `cmd /S /C "` + command + `"`
	}
	cmd.SysProcAttr = attr
	return cmd
}

// encodePowerShell 把命令编码成 powershell -EncodedCommand 需要的 base64(UTF-16LE)。
func encodePowerShell(command string) string {
	units := utf16.Encode([]rune(command))
	buf := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		buf = append(buf, byte(unit), byte(unit>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// killProcessTree 用 taskkill /T /F 结束整棵进程树。
//
// 直接 Process.Kill() 只结束 cmd.exe，孙进程（编译器、ping 等）会变孤儿继续
// 运行，并继续持有输出临时文件。
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	taskkill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	// 同样不能闪窗：这是从 GUI 进程里启动的
	taskkill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := taskkill.Run(); err != nil {
		// taskkill 失败（进程已退出/无权限）时退回单进程 kill
		return cmd.Process.Kill()
	}
	return nil
}
