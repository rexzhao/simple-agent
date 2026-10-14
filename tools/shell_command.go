package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// shellCommandConfig describes how to start the Bash-compatible shell used by
// the shell tool. Legacy WSL's bash.exe only accepts a script through stdin;
// all other supported Bash implementations receive it as the argument to -c.
type shellCommandConfig struct {
	shell            string
	args             []string
	commandFromStdin bool
}

var legacyWSLBashPathPattern = regexp.MustCompile(`(?i)^[a-z]:\\windows\\(?:system32|sysnative)\\bash\.exe$`)

// newShellCommand creates a Pi-style Bash command. In particular, Windows
// never falls back to PowerShell or cmd.exe: commands have one portable Bash
// syntax on every platform.
func newShellCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	config, err := resolveShellCommandConfig()
	if err != nil {
		return nil, err
	}

	args := append([]string(nil), config.args...)
	if !config.commandFromStdin {
		args = append(args, command)
	}
	cmd := exec.CommandContext(ctx, config.shell, args...)
	if config.commandFromStdin {
		cmd.Stdin = strings.NewReader(command)
	}
	return cmd, nil
}

func resolveShellCommandConfig() (shellCommandConfig, error) {
	return resolveShellCommandConfigFor(runtime.GOOS, os.Getenv, shellPathExists, findBashOnPath)
}

// resolveShellCommandConfigFor contains the shell selection policy independently
// of process creation so it can be tested without relying on the host's PATH.
// Its search order matches pi: Git Bash locations then PATH on Windows; /bin/bash
// then PATH then sh elsewhere.
func resolveShellCommandConfigFor(
	goos string,
	getenv func(string) string,
	pathExists func(string) bool,
	findOnPath func() string,
) (shellCommandConfig, error) {
	bashConfig := func(shell string) shellCommandConfig {
		if isLegacyWSLBashPath(shell) {
			return shellCommandConfig{shell: shell, args: []string{"-s"}, commandFromStdin: true}
		}
		return shellCommandConfig{shell: shell, args: []string{"-c"}}
	}

	if goos == "windows" {
		candidates := make([]string, 0, 2)
		if programFiles := strings.TrimSpace(getenv("ProgramFiles")); programFiles != "" {
			candidates = append(candidates, filepath.Join(programFiles, "Git", "bin", "bash.exe"))
		}
		if programFilesX86 := strings.TrimSpace(getenv("ProgramFiles(x86)")); programFilesX86 != "" {
			candidates = append(candidates, filepath.Join(programFilesX86, "Git", "bin", "bash.exe"))
		}
		for _, candidate := range candidates {
			if pathExists(candidate) {
				return bashConfig(candidate), nil
			}
		}
		if bashOnPath := strings.TrimSpace(findOnPath()); bashOnPath != "" {
			return bashConfig(bashOnPath), nil
		}

		searched := "(no Program Files locations were configured)"
		if len(candidates) > 0 {
			searched = strings.Join(candidates, "\n")
		}
		return shellCommandConfig{}, fmt.Errorf(
			"no bash shell found; install Git for Windows, add bash.exe to PATH, or install another Bash-compatible shell. searched Git Bash locations:\n%s",
			searched,
		)
	}

	if pathExists("/bin/bash") {
		return bashConfig("/bin/bash"), nil
	}
	if bashOnPath := strings.TrimSpace(findOnPath()); bashOnPath != "" {
		return bashConfig(bashOnPath), nil
	}
	return shellCommandConfig{shell: "sh", args: []string{"-c"}}, nil
}

func shellPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func findBashOnPath() string {
	if runtime.GOOS == "windows" {
		// Match pi's Windows lookup. where.exe may print stale entries, so verify
		// the first reported candidate exists before using it.
		output, err := exec.Command("where", "bash.exe").Output()
		if err != nil {
			return ""
		}
		first := firstCommandPath(string(output))
		if first != "" && shellPathExists(first) {
			return first
		}
		return ""
	}

	// Pi uses which on Unix-like systems; use its first line as the executable.
	output, err := exec.Command("which", "bash").Output()
	if err != nil {
		return ""
	}
	return firstCommandPath(string(output))
}

func firstCommandPath(output string) string {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if candidate := strings.TrimSpace(line); candidate != "" {
			return candidate
		}
	}
	return ""
}

func isLegacyWSLBashPath(path string) bool {
	normalized := strings.ReplaceAll(path, "/", "\\")
	return legacyWSLBashPathPattern.MatchString(normalized)
}
