package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	BuiltinListFiles = "list_files"
	BuiltinReadFile  = "read_file"
	BuiltinShell     = "shell"
)

const shellCancelWaitDelay = 2 * time.Second

func RegisterBuiltins(registry *Registry, rootDir string) error {
	if registry == nil {
		return fmt.Errorf("registry must not be nil")
	}
	if strings.TrimSpace(rootDir) == "" {
		return fmt.Errorf("rootDir must not be blank")
	}

	root, err := filepath.Abs(rootDir)
	if err != nil {
		return fmt.Errorf("resolve rootDir: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat rootDir %q: %w", rootDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("rootDir %q must be a directory", rootDir)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve rootDir %q: %w", rootDir, err)
	}

	builtins := []Entry{
		{
			Definition: listFilesDefinition(),
			Executor:   newListFilesExecutor(canonicalRoot),
		},
		{
			Definition: readFileDefinition(),
			Executor:   newReadFileExecutor(canonicalRoot),
		},
		{
			Definition: shellDefinition(),
			Executor:   newShellExecutor(root),
		},
	}
	for _, builtin := range builtins {
		if err := registry.Register(builtin.Definition, builtin.Executor); err != nil {
			return err
		}
	}
	return nil
}

func listFilesDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinListFiles,
		Description: "List files and directories under the workspace.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path relative to the workspace. Defaults to the workspace root.",
				},
			},
			"additionalProperties": false,
		},
	}
}

func readFileDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinReadFile,
		Description: "Read a text file under the workspace.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the workspace.",
				},
			},
			"required":             []any{"path"},
			"additionalProperties": false,
		},
	}
}

func shellDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinShell,
		Description: "Run a shell command in the workspace.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Command to run.",
				},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	}
}

func newListFilesExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		path, err := optionalStringArgument(arguments, "path", ".")
		if err != nil {
			return model.ToolResult{}, err
		}
		resolved, err := resolveRootPath(rootDir, path)
		if err != nil {
			return model.ToolResult{}, err
		}

		entries, err := os.ReadDir(resolved)
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("list files %q: %w", path, err)
		}

		lines := make([]string, 0, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}
			lines = append(lines, name)
		}
		sort.Strings(lines)

		return model.ToolResult{
			Name:    BuiltinListFiles,
			Content: strings.Join(lines, "\n"),
		}, nil
	})
}

func newReadFileExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		path, err := requiredStringArgument(arguments, "path")
		if err != nil {
			return model.ToolResult{}, err
		}
		resolved, err := resolveRootPath(rootDir, path)
		if err != nil {
			return model.ToolResult{}, err
		}

		data, err := os.ReadFile(resolved)
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("read file %q: %w", path, err)
		}

		return model.ToolResult{
			Name:    BuiltinReadFile,
			Content: string(data),
		}, nil
	})
}

func newShellExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		command, err := requiredStringArgument(arguments, "command")
		if err != nil {
			return model.ToolResult{}, err
		}

		cmd := shellCommand(ctx, command)
		cmd.Dir = rootDir
		configureShellCommandCancel(cmd)

		output, err := cmd.CombinedOutput()
		result := model.ToolResult{
			Name:    BuiltinShell,
			Content: string(output),
		}
		if err != nil {
			result.IsError = true
			result.Content = shellErrorContent(ctx, result.Content, err)
		}
		return result, nil
	})
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	}
	return exec.CommandContext(ctx, "sh", "-c", command)
}

func configureShellCommandCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = shellCancelWaitDelay

	killProcessGroup := false
	if runtime.GOOS != "windows" {
		attr := &syscall.SysProcAttr{}
		killProcessGroup = setSysProcAttrBool(attr, "Setpgid", true)
		if killProcessGroup {
			cmd.SysProcAttr = attr
		}
	}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if killProcessGroup {
			if err := killShellProcessGroup(cmd.Process.Pid); err == nil || errors.Is(err, os.ErrProcessDone) {
				return err
			}
		}
		if runtime.GOOS == "windows" {
			if err := killWindowsProcessTree(cmd.Process.Pid); err == nil || errors.Is(err, os.ErrProcessDone) {
				return err
			}
		}
		return cmd.Process.Kill()
	}
}

func setSysProcAttrBool(attr *syscall.SysProcAttr, name string, value bool) bool {
	field := reflect.ValueOf(attr).Elem().FieldByName(name)
	if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.Bool {
		return false
	}
	field.SetBool(value)
	return true
}

func killShellProcessGroup(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	process, err := os.FindProcess(-pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

func killWindowsProcessTree(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	output, err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill process tree pid %d: %w: %s", pid, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func shellErrorContent(ctx context.Context, output string, err error) string {
	if ctxErr := ctx.Err(); ctxErr != nil {
		message := "shell command canceled"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			message = "shell command timed out"
		}
		message = fmt.Sprintf("%s: %v", message, ctxErr)
		if strings.TrimSpace(output) == "" {
			return message
		}
		return message + "\n" + output
	}
	if output == "" {
		return err.Error()
	}
	return output
}

func optionalStringArgument(arguments map[string]any, name string, defaultValue string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return defaultValue, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must not be blank", name)
	}
	return text, nil
}

func requiredStringArgument(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must not be blank", name)
	}
	return text, nil
}

func resolveRootPath(rootDir string, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path must not be blank")
	}

	root, err := filepath.Abs(rootDir)
	if err != nil {
		return "", fmt.Errorf("resolve rootDir: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve rootDir: %w", err)
	}

	var target string
	if filepath.IsAbs(path) {
		target = filepath.Clean(path)
	} else {
		target = filepath.Join(canonicalRoot, path)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}

	relative, err := filepath.Rel(canonicalRoot, canonicalTarget)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	if relative == "." {
		return canonicalTarget, nil
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path %q is outside rootDir", path)
	}
	return canonicalTarget, nil
}
