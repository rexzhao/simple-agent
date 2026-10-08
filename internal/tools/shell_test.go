package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestExecutor builds a shell executor over a temp directory.
func newTestExecutor(t *testing.T) Executor {
	t.Helper()
	_, executor, err := Shell(t.TempDir())
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	return executor
}

func TestShellExecutorRunsCommandAndReturnsCombinedOutput(t *testing.T) {
	executor := newTestExecutor(t)
	result, err := executor.Execute(context.Background(), map[string]any{
		"command": "echo out; echo err 1>&2",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %+v", result)
	}
	if !strings.Contains(result.Content, "out") || !strings.Contains(result.Content, "err") {
		t.Fatalf("combined output = %q, want stdout and stderr", result.Content)
	}
	if result.Name != BuiltinShell {
		t.Fatalf("result name = %q", result.Name)
	}
}

func TestShellExecutorFailingCommandIsErrorWithOutput(t *testing.T) {
	executor := newTestExecutor(t)
	result, err := executor.Execute(context.Background(), map[string]any{
		"command": "echo before-fail; exit 3",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.IsError {
		t.Fatalf("failing command reported success: %+v", result)
	}
	// With output present, the output itself is the content; the error
	// kind is carried by IsError, not by an appended message.
	if !strings.Contains(result.Content, "before-fail") {
		t.Fatalf("output before failure lost: %q", result.Content)
	}
}

func TestShellExecutorMissingCommandArgument(t *testing.T) {
	executor := newTestExecutor(t)
	if _, err := executor.Execute(context.Background(), map[string]any{}); err == nil {
		t.Fatal("missing command argument accepted")
	}
}

func TestShellExecutorInvalidArguments(t *testing.T) {
	executor := newTestExecutor(t)
	for name, value := range map[string]any{
		"command":          123,
		"timeout_ms":       -1,
		"max_output_bytes": 0,
	} {
		arguments := map[string]any{"command": "echo hi", name: value}
		if name == "command" {
			arguments = map[string]any{name: value}
		}
		if _, err := executor.Execute(context.Background(), arguments); err == nil {
			t.Fatalf("invalid %s=%v accepted", name, value)
		}
	}
}

func TestShellExecutorTimeout(t *testing.T) {
	executor := newTestExecutor(t)
	start := time.Now()
	result, err := executor.Execute(context.Background(), map[string]any{
		"command":    "echo started; sleep 30",
		"timeout_ms": 500,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.IsError {
		t.Fatalf("timed-out command reported success: %+v", result)
	}
	if !strings.Contains(result.Content, "shell command timed out") {
		t.Fatalf("timeout message missing: %q", result.Content)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout not enforced, took %v", elapsed)
	}
}

func TestShellExecutorContextCancel(t *testing.T) {
	executor := newTestExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	result, err := executor.Execute(ctx, map[string]any{"command": "sleep 30"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "shell command canceled") {
		t.Fatalf("cancel result = %+v", result)
	}
}

func TestShellExecutorOutputTruncation(t *testing.T) {
	executor := newTestExecutor(t)
	result, err := executor.Execute(context.Background(), map[string]any{
		"command":          "seq 1 100000",
		"max_output_bytes": 1024,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Content, "truncated=true") {
		t.Fatalf("truncation notice missing: %q", result.Content[:min(120, len(result.Content))])
	}
	if !strings.Contains(result.Content, "max_output_bytes=1024") {
		t.Fatalf("truncation metadata missing: %q", result.Content[:min(120, len(result.Content))])
	}
	if len(result.Content) > 2048 {
		t.Fatalf("retained output far over the cap: %d bytes", len(result.Content))
	}
}

func TestLimitedOutput(t *testing.T) {
	output := newLimitedOutput(10)
	if n, err := output.Write([]byte("12345")); n != 5 || err != nil {
		t.Fatalf("write = %d, %v", n, err)
	}
	// The next write exceeds the cap: reported fully written, retained
	// partially, flagged truncated.
	if n, err := output.Write([]byte("67890ABCDEF")); n != 11 || err != nil {
		t.Fatalf("over-cap write = %d, %v", n, err)
	}
	if got := output.String(); got != "1234567890" {
		t.Fatalf("retained = %q, want the first 10 bytes", got)
	}
	if !output.Truncated() {
		t.Fatal("truncation not flagged")
	}
	// Writes past the cap keep draining successfully (os/exec must not
	// block) but retain nothing.
	if n, err := output.Write([]byte("more")); n != 4 || err != nil {
		t.Fatalf("post-cap write = %d, %v", n, err)
	}
	if got := output.String(); got != "1234567890" {
		t.Fatalf("retained after cap = %q", got)
	}
}

func TestShellExecutorWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	// A marker file avoids comparing Windows and MSYS path spellings.
	marker := filepath.Join(root, "sai-cwd-marker.txt")
	if err := os.WriteFile(marker, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	_, executor, err := Shell(root)
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	result, err := executor.Execute(context.Background(), map[string]any{
		"command": "test -f sai-cwd-marker.txt && echo in-root || echo elsewhere",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Content, "in-root") {
		t.Fatalf("command did not run in the tool root: %q", result.Content)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

