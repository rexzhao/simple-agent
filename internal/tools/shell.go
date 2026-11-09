package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

const (
	defaultTimeoutMs = 120_000
	maxTimeoutMs     = 600_000
	maxOutputBytes   = 100_000
)

// shellKind 决定解释器：bash 在 Windows 走 cmd /C，pwsh 走 Windows PowerShell。
//
// 名称与实现的差异是**有意保留 harness 的工具名**：描述里已写明实际解释器，
// 避免模型在 Windows 上写 bash 语法静默失败。
type shellKind int

const (
	shellBash shellKind = iota
	shellPwsh
)

func bashTool() *ToolDef {
	return &ToolDef{
		Name: "bash",
		Description: "Run a shell command and return its combined output and exit code. On Windows this " +
			"runs via cmd /C (use cmd syntax); elsewhere via sh -c.",
		Guidance: "Check the [exit code: N] marker on every bash result; " +
			"investigate failures before moving on. " +
			"Prefer the dedicated file and search tools over shell commands such as cat, find or grep.",
		Parameters: shellParams(),
		Run: func(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
			return runCommand(ctx, args, tc, shellBash)
		},
	}
}

func pwshTool() *ToolDef {
	return &ToolDef{
		Name: "pwsh",
		Description: "Run a PowerShell command via powershell.exe (Windows PowerShell 5.1) and return " +
			"its combined output and exit code.",
		Guidance: "Non-zero exits are reported as [exit code: N] markers; investigate failures before moving on. " +
			"On Windows a killed process settles as [exit code: 1] without a signal marker; " +
			"treat a bare exit 1 after an interruption as a termination, not a command failure.",
		Parameters: shellParams(),
		Run: func(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
			return runCommand(ctx, args, tc, shellPwsh)
		},
	}
}

func shellParams() map[string]any {
	return schema(map[string]any{
		"command": map[string]any{"type": "string",
			"description": "The shell command to execute (cmd syntax on Windows, sh syntax elsewhere)."},
		"timeoutMs": map[string]any{"type": "number",
			"description": fmt.Sprintf("Timeout in milliseconds. Defaults to %d.", defaultTimeoutMs)},
		"workdir": map[string]any{"type": "string",
			"description": "Working directory for this command. Defaults to the session working directory; a relative path is resolved against it."},
		"description": map[string]any{"type": "string",
			"description": "Clear, concise description of what this command does in active voice."},
	}, "command")
}

// ErrShellDisabled 表示命令执行被配置关闭。
//
// 文案里的文件名用**当前默认格式**（YAML）：写 simple-agent.toml 会让照着做的
// 用户新建一份旧格式文件，虽然还能读，但配置优先级和文档口径都会对不上。
var ErrShellDisabled = errors.New(
	"命令执行已禁用。启用方式（任选其一）：① 启动参数 --allow-shell；" +
		"② 环境变量 SIMPLE_AGENT_ALLOW_SHELL=1；③ 配置文件 simple-agent.yaml 中写 allow_shell: true")

func runCommand(ctx context.Context, args Args, tc *ToolContext, shell shellKind) (ToolReturn, error) {
	if !tc.AllowShell {
		return ToolReturn{}, ErrShellDisabled
	}
	command, err := args.Str("command")
	if err != nil {
		return ToolReturn{}, err
	}
	if command == "" {
		return ToolReturn{}, fmt.Errorf("command 不能为空")
	}

	workdir := tc.Cwd()
	if raw := args.OptStr("workdir"); raw != "" {
		if workdir, err = Resolve(tc, raw); err != nil {
			return ToolReturn{}, err
		}
	}
	info, statErr := os.Stat(workdir)
	if statErr != nil || !info.IsDir() {
		return ToolReturn{}, fmt.Errorf("workdir 不是目录：%s", workdir)
	}

	timeoutMs := int64(defaultTimeoutMs)
	if v, ok := args.Int("timeoutMs"); ok {
		timeoutMs = int64(v)
	}
	if timeoutMs < 1000 {
		timeoutMs = 1000
	}
	if timeoutMs > maxTimeoutMs {
		timeoutMs = maxTimeoutMs
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond

	// 输出重定向到临时文件：管道缓冲区写满会让子进程阻塞在写操作上（经典死锁）。
	outFile, err := os.CreateTemp("", "simple-agent-cmd-*.out")
	if err != nil {
		return ToolReturn{}, fmt.Errorf("创建临时文件失败：%w", err)
	}
	defer os.Remove(outFile.Name())
	defer outFile.Close()

	errFile, err := os.CreateTemp("", "simple-agent-cmd-*.err")
	if err != nil {
		return ToolReturn{}, fmt.Errorf("创建临时文件失败：%w", err)
	}
	defer os.Remove(errFile.Name())
	defer errFile.Close()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newShellCmd(runCtx, shell, command)
	cmd.Dir = workdir
	cmd.Stdout = outFile
	cmd.Stderr = errFile
	cmd.Stdin = nil // 交互式命令读到 EOF 直接退出，而不是挂住

	var killed atomic.Bool
	// CommandContext 的默认 Cancel 只杀直接子进程：cmd /C 起的孙进程（编译、ping）
	// 会变孤儿继续跑，并继续持有输出临时文件。这里改为杀整棵进程树。
	cmd.Cancel = func() error {
		killed.Store(true)
		if err := killProcessTree(cmd); err != nil {
			return err
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)

	output := readCapped(outFile.Name())
	stderr := readCapped(errFile.Name())
	if stderr != "" {
		if output != "" && !hasSuffixNewline(output) {
			output += "\n"
		}
		output += stderr
	}

	if ctx.Err() != nil {
		// 用户停止：不是工具本身的失败，但要给出可读原因
		return ToolReturn{}, context.Canceled
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return ToolReturn{}, fmt.Errorf("命令超时（%d ms，cwd=%s）：%s\n--- 已产生输出 ---\n%s",
			timeoutMs, workdir, command, output)
	}
	if runErr != nil && !killed.Load() {
		if _, ok := runErr.(*exec.ExitError); !ok {
			return ToolReturn{}, fmt.Errorf("无法启动进程：%w", runErr)
		}
	}

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	header := fmt.Sprintf("exit_code=%d elapsed=%dms cwd=%s\n", exitCode, elapsed.Milliseconds(), workdir)
	body := header + output
	if exitCode != 0 {
		// 非零退出码当错误：is_error=true 让模型知道要纠正，但输出要完整保留
		return ToolReturn{}, errors.New(body)
	}
	return OutputOf(body), nil
}

// readCapped 只读输出文件的前 maxOutputBytes 字节。
//
// 不能先 os.ReadFile 再截断：失控的命令（yes、死循环打印、cat 大文件）在 120 秒
// 内能写出好几个 GB，整个读进内存会 OOM。用 LimitReader 限流，再退回到最近的
// 完整 UTF-8 字符边界（中文被切一半会产生非法的替换字符）。
func readCapped(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	var size int64
	if stat, statErr := file.Stat(); statErr == nil {
		size = stat.Size()
	}

	data, err := io.ReadAll(io.LimitReader(file, maxOutputBytes))
	if err != nil && len(data) == 0 {
		return ""
	}
	text := decodeConsole(data)
	if size > int64(len(data)) {
		text += fmt.Sprintf("\n…（输出共 %d 字节，已截断到前 %d 字节）", size, len(data))
	}
	return text
}

func hasSuffixNewline(s string) bool {
	return len(s) > 0 && s[len(s)-1] == '\n'
}
