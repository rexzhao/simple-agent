package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestModelsListUsesGlobalConfigDirFlag(t *testing.T) {
	dir := writeCLIFixture(t)
	writeCLIFile(t, filepath.Join(dir, "providers", "responses.yaml"), `name: openai
type: openai-responses
base_url: https://api.openai.com/v1
api_key: $OPENAI_API_KEY

models:
  default:
    id: gpt-5.1
`)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", dir, "models", "list"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"PROVIDER\tPROFILE\tMODEL ID",
		"openai\tdefault\tgpt-5.1",
		"paperhub\tglm-5.2\tglm-5.2",
		"paperhub\tglm-5.2-fast\tglm-5.2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("models list output missing %q:\n%s", want, out)
		}
	}
}

func TestModelsListAcceptsConfigDirAfterCommand(t *testing.T) {
	dir := writeCLIFixture(t)

	for _, args := range [][]string{
		{"models", "list", "--config-dir", dir},
		{"models", "--config-dir", dir, "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 0 {
				t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "paperhub\tglm-5.2\tglm-5.2") {
				t.Fatalf("models list output = %s", stdout.String())
			}
		})
	}
}

func TestModelsListMixedHelpDoesNotLoadConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", filepath.Join(t.TempDir(), "missing"), "models", "-h", "list"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "usage: sai models list") {
		t.Fatalf("stdout = %q, want models list usage", stdout.String())
	}
}

func TestModelsListDefaultsConfigDirToAgentsUnderCurrentWorkingDirectory(t *testing.T) {
	projectDir := t.TempDir()
	writeCLIFixtureInDir(t, filepath.Join(projectDir, ".agents"))

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"models", "list"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "paperhub\tglm-5.2\tglm-5.2") {
		t.Fatalf("models list output = %s", stdout.String())
	}
}

func TestMCPListShowsConfiguredServersAndEnabledState(t *testing.T) {
	dir := writeCLIFixture(t)
	writeCLIMCPFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", dir, "mcp", "list"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"ID\tENABLED",
		"local\ttrue",
		"remote\tfalse",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "COMMAND") || strings.Contains(out, "example-mcp-server") || strings.Contains(out, "remote-mcp-server") {
		t.Fatalf("mcp list output included command details:\n%s", out)
	}
}

func TestToolsListWritesBuiltInToolsWithoutConfig(t *testing.T) {
	for _, args := range [][]string{
		{"tools", "list"},
		{"tools", "list", "-h"},
		{"tools", "list", "extra", "-h", "--bad"},
		{"help", "tools", "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 0 {
				t.Fatalf("RunWithGetwd(%v) code = %d, stderr = %s", args, code, stderr.String())
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			out := stdout.String()
			if containsHelpArg(args) || args[0] == "help" {
				if !strings.Contains(out, "usage: sai tools list") {
					t.Fatalf("stdout = %q, want tools list usage", out)
				}
				return
			}
			if got, want := out, "list_files\nread_file\nwrite_file\nedit_file\nshell\n"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
		})
	}
}

func TestToolsListUnknownFlagAfterExtraArgIncludesHelpHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"tools", "list", "extra", "--bad"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined", `Run "sai help tools list" for usage.`)
}

func TestToolsListDelimiterTreatsDashArgAsExtraPositional(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"tools", "list", "--", "--bad"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "usage: sai tools list", `Run "sai help tools list" for usage.`)
	if strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("stderr treated delimiter positional as flag: %s", stderr.String())
	}
}

func TestMCPListEnableMCPOverridesConfiguredEnabledState(t *testing.T) {
	dir := writeCLIFixture(t)
	writeCLIMCPFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", dir, "mcp", "list", "--enable-mcp", "remote"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"local\tfalse",
		"remote\ttrue",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp list override output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "COMMAND") || strings.Contains(out, "example-mcp-server") || strings.Contains(out, "remote-mcp-server") {
		t.Fatalf("mcp list override output included command details:\n%s", out)
	}
}

func TestMCPListAcceptsConfigDirAfterCommandAndEnableMCP(t *testing.T) {
	dir := writeCLIFixture(t)
	writeCLIMCPFixture(t, dir)

	for _, args := range [][]string{
		{"mcp", "list", "--enable-mcp", "remote", "--config-dir", dir},
		{"mcp", "--enable-mcp", "remote", "--config-dir", dir, "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 0 {
				t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
			}
			out := stdout.String()
			for _, want := range []string{
				"local\tfalse",
				"remote\ttrue",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("mcp list output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestMCPListMixedFlagWithExtraArgIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"mcp", "list", "remote", "--enable-mcp", "local"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "usage: sai mcp list", `Run "sai help mcp list" for usage.`)
}

func TestMCPListRejectsUnknownEnableMCPServer(t *testing.T) {
	dir := writeCLIFixture(t)
	writeCLIMCPFixture(t, dir)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", dir, "mcp", "list", "--enable-mcp", "missing"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown MCP server "missing"`, "available MCP servers: local, remote")
}

func TestConfigShowDoesNotPrintAPIKeyValue(t *testing.T) {
	dir := writeCLIFixture(t)
	t.Setenv("PAPERHUB_API_KEY", "env-secret-value")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", dir, "config", "show"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, leaked := range []string{"env-secret-value", "direct-secret-value"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("config show leaked API key value %q:\n%s", leaked, out)
		}
	}
	if !strings.Contains(out, "PAPERHUB_API_KEY") {
		t.Fatalf("config show should include API key env var name:\n%s", out)
	}
	if !strings.Contains(out, "<redacted>") {
		t.Fatalf("config show should include redacted direct API key:\n%s", out)
	}
	if logPaths := sessionLogPaths(t, dir); len(logPaths) != 0 {
		t.Fatalf("config show created session log paths: %#v", logPaths)
	}
}

func TestConfigShowAcceptsConfigDirAfterCommand(t *testing.T) {
	dir := writeCLIFixture(t)

	for _, args := range [][]string{
		{"config", "show", "--config-dir", dir},
		{"config", "--config-dir", dir, "show"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})

			if code != 0 {
				t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), `"ConfigDir":`) && !strings.Contains(stdout.String(), `"config_dir":`) {
				t.Fatalf("config show output = %s", stdout.String())
			}
		})
	}
}

func TestConfigShowMixedHelpDoesNotLoadConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"config", "show", "--config-dir", filepath.Join(t.TempDir(), "missing"), "-h"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "usage: sai config show") {
		t.Fatalf("stdout = %q, want config show usage", stdout.String())
	}
}

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"version"}, &stdout, &stderr, func() (string, error) {
		return "unused", nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "sai dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestRootHelpWritesUsageWithoutConfig(t *testing.T) {
	for _, args := range [][]string{
		{"-h"},
		{"help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 0 {
				t.Fatalf("RunWithGetwd(%v) code = %d, stderr = %s", args, code, stderr.String())
			}
			out := stdout.String()
			for _, want := range []string{"usage: sai", `chat ["prompt"]`, "config show", "models list", "tools list", `Run "sai help <command>" for command usage.`} {
				if !strings.Contains(out, want) {
					t.Fatalf("stdout = %q, want contain %q", out, want)
				}
			}
			if strings.Contains(out, `run "prompt"`) || strings.Contains(out, "sai run") {
				t.Fatalf("root help still lists run:\n%s", out)
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRunHelpIsUnsupportedWithoutConfig(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"run", "-h"}, want: `unknown command "run"`},
		{args: []string{"help", "run"}, want: `unknown help topic "run"`},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunWithGetwd(tt.args, &stdout, &stderr, func() (string, error) {
				return "", errors.New("getwd should not be called")
			})
			if code != 1 {
				t.Fatalf("RunWithGetwd(%v) code = %d, want 1", tt.args, code)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			assertCLIErrorContains(t, stderr.String(), tt.want, `Run "sai help" for usage.`)
			if strings.Contains(stderr.String(), "usage: sai run") {
				t.Fatalf("stderr included run usage: %s", stderr.String())
			}
		})
	}
}

func TestChatHelpWritesUsageWithoutConfig(t *testing.T) {
	for _, args := range [][]string{
		{"chat", "-h"},
		{"chat", "--help"},
		{"help", "chat"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			assertCLIHelpWithoutConfig(t, args, "usage: sai chat", `[--quit] ["prompt"]`, "--provider name", "--enable-tools names")
		})
	}
}

func TestVersionHelpWritesUsageWithoutConfig(t *testing.T) {
	for _, args := range [][]string{
		{"version", "-h"},
		{"version", "--help"},
		{"help", "version"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			assertCLIHelpWithoutConfig(t, args, "usage: sai version", "Prints the sai version.")
		})
	}
}

func TestGroupHelpWritesUsageWithoutConfig(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{
			name:  "config flag short",
			args:  []string{"config", "-h"},
			wants: []string{"usage: sai config <command>", "config show"},
		},
		{
			name:  "config flag long",
			args:  []string{"config", "--help"},
			wants: []string{"usage: sai config <command>", "config show"},
		},
		{
			name:  "config help",
			args:  []string{"help", "config"},
			wants: []string{"usage: sai config <command>", "config show"},
		},
		{
			name:  "models flag short",
			args:  []string{"models", "-h"},
			wants: []string{"usage: sai models <command>", "models list"},
		},
		{
			name:  "models flag long",
			args:  []string{"models", "--help"},
			wants: []string{"usage: sai models <command>", "models list"},
		},
		{
			name:  "models help",
			args:  []string{"help", "models"},
			wants: []string{"usage: sai models <command>", "models list"},
		},
		{
			name:  "tools flag short",
			args:  []string{"tools", "-h"},
			wants: []string{"usage: sai tools <command>", "tools list"},
		},
		{
			name:  "tools flag long",
			args:  []string{"tools", "--help"},
			wants: []string{"usage: sai tools <command>", "tools list"},
		},
		{
			name:  "tools help",
			args:  []string{"help", "tools"},
			wants: []string{"usage: sai tools <command>", "tools list"},
		},
		{
			name:  "mcp flag short",
			args:  []string{"mcp", "-h"},
			wants: []string{"usage: sai mcp <command>", "mcp list"},
		},
		{
			name:  "mcp flag long",
			args:  []string{"mcp", "--help"},
			wants: []string{"usage: sai mcp <command>", "mcp list"},
		},
		{
			name:  "mcp help",
			args:  []string{"help", "mcp"},
			wants: []string{"usage: sai mcp <command>", "mcp list"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertCLIHelpWithoutConfig(t, tt.args, tt.wants...)
		})
	}
}

func TestNestedHelpWritesUsageWithoutConfig(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{
			name:  "config show flag",
			args:  []string{"config", "show", "-h"},
			wants: []string{"usage: sai config show", "sensitive values redacted"},
		},
		{
			name:  "config show help",
			args:  []string{"help", "config", "show"},
			wants: []string{"usage: sai config show", "sensitive values redacted"},
		},
		{
			name:  "models list flag",
			args:  []string{"models", "list", "-h"},
			wants: []string{"usage: sai models list", "provider model profiles"},
		},
		{
			name:  "models list help",
			args:  []string{"help", "models", "list"},
			wants: []string{"usage: sai models list", "provider model profiles"},
		},
		{
			name:  "tools list flag",
			args:  []string{"tools", "list", "-h"},
			wants: []string{"usage: sai tools list", "built-in tools"},
		},
		{
			name:  "tools list help",
			args:  []string{"help", "tools", "list"},
			wants: []string{"usage: sai tools list", "built-in tools"},
		},
		{
			name:  "mcp list flag",
			args:  []string{"mcp", "list", "-h"},
			wants: []string{"usage: sai mcp list", "--enable-mcp ids"},
		},
		{
			name:  "mcp list help",
			args:  []string{"help", "mcp", "list"},
			wants: []string{"usage: sai mcp list", "--enable-mcp ids"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertCLIHelpWithoutConfig(t, tt.args, tt.wants...)
		})
	}
}

func TestUnknownCommandIncludesHelpHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"nope"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown command "nope"`, `Run "sai help" for usage.`)
}

func TestUnknownRootFlagBeforeCommandIncludesHelpHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--bad", "chat"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined", `Run "sai help" for usage.`)
}

func TestRunCommandIsUnsupported(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"run", "Say hi"}, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown command "run"`, `Run "sai help" for usage.`)
	if strings.Contains(stderr.String(), "usage: sai run") {
		t.Fatalf("stderr included run usage: %s", stderr.String())
	}
}

func TestChatUnknownFlagIncludesHelpHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--bad"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined", `Run "sai help chat" for usage.`)
}

func TestChatMixedHelpDoesNotLoadConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "ignored", "-h", "--config-dir", filepath.Join(t.TempDir(), "missing")}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "usage: sai chat") {
		t.Fatalf("stdout = %q, want chat usage", stdout.String())
	}
}

func TestChatQuitWithoutPromptIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "--quit requires an initial prompt", "usage: sai chat", `Run "sai help chat" for usage.`)
}

func TestChatExitReturnsWithoutModelRequest(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config-dir", configDir, "chat"}, strings.NewReader("/exit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "> "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if logPaths := sessionLogPaths(t, configDir); len(logPaths) != 0 {
		t.Fatalf("session log paths = %#v, want none", logPaths)
	}
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatInitialPromptWithQuitRunsOneTurnAndExits(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config-dir", configDir, "chat", "--quit", "first"}, strings.NewReader("second\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	request := <-requests
	messages := requestMessages(t, request.Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatAcceptsConfigDirAfterCommand(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--config-dir", configDir, "--quit", "first"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertMessage(t, requestMessages(t, (<-requests).Body), 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatDelimiterTreatsHelpAsPrompt(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	projectDir := t.TempDir()
	configDir := filepath.Join(projectDir, ".agents")
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--quit", "--", "--help"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertMessage(t, requestMessages(t, (<-requests).Body), 1, "user", "--help")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatDelimiterAfterConfigDirTreatsHelpAsPrompt(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--config-dir", configDir, "--quit", "--", "--help"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertMessage(t, requestMessages(t, (<-requests).Body), 1, "user", "--help")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatDelimiterTreatsConfigDirAsPrompt(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "--config-dir", configDir, "--quit", "--", "--config-dir"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertMessage(t, requestMessages(t, (<-requests).Body), 1, "user", "--config-dir")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatInitialPromptAllowsQuitAfterPrompt(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config-dir", configDir, "chat", "first", "--quit"}, strings.NewReader("second\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	messages := requestMessages(t, (<-requests).Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatInitialPromptAllowsModelFlagAfterPrompt(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config-dir", configDir, "chat", "first", "--model", "fast", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	request := <-requests
	if got := request.Body["model"]; got != "model-fast" {
		t.Fatalf("model = %#v, want model-fast", got)
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatAllowsModelFlagBeforeCommand(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--model", "fast", "chat", "--config-dir", configDir, "first", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	request := <-requests
	if got := request.Body["model"]; got != "model-fast" {
		t.Fatalf("model = %#v, want model-fast", got)
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatInitialPromptAllowsEnableToolsFlagAfterPrompt(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config-dir", configDir, "chat", "first", "--enable-tools", "read_file", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"read_file"})
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatAllowsEnableToolsFlagBeforeCommand(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--enable-tools", "read_file", "chat", "--config-dir", configDir, "first", "--quit"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"read_file"})
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatInterspersedArgsRejectMultiplePrompts(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "first", "--quit", "second"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "expected at most one prompt", "usage: sai chat", `Run "sai help chat" for usage.`)
}

func TestChatUnknownFlagAfterPromptIncludesHelpHint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"chat", "first", "--bad"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "flag provided but not defined", `Run "sai help chat" for usage.`)
}

func TestChatInitialPromptContinuesIntoREPLWithHistory(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"one"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config-dir", configDir, "chat", "first"}, strings.NewReader("second\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one\ntwo\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "> > "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	firstRequest := <-requests
	firstMessages := requestMessages(t, firstRequest.Body)
	assertMessage(t, firstMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, firstMessages, 1, "user", "first")

	secondRequest := <-requests
	secondMessages := requestMessages(t, secondRequest.Body)
	if len(secondMessages) != 4 {
		t.Fatalf("len(second request messages) = %d, want 4: %#v", len(secondMessages), secondMessages)
	}
	assertMessage(t, secondMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, secondMessages, 1, "user", "first")
	assertMessage(t, secondMessages, 2, "assistant", "one")
	assertMessage(t, secondMessages, 3, "user", "second")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatTwoTurnsCarryForwardUserAndAssistantHistory(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"one"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config-dir", configDir, "chat"}, strings.NewReader("first\n\nsecond\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "one\ntwo\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stdout.String(), "> ") {
		t.Fatalf("stdout contains prompt: %q", stdout.String())
	}
	if got, want := stderr.String(), "> > > > "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	firstRequest := <-requests
	firstMessages := requestMessages(t, firstRequest.Body)
	assertMessage(t, firstMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, firstMessages, 1, "user", "first")

	secondRequest := <-requests
	secondMessages := requestMessages(t, secondRequest.Body)
	if len(secondMessages) != 4 {
		t.Fatalf("len(second request messages) = %d, want 4: %#v", len(secondMessages), secondMessages)
	}
	assertMessage(t, secondMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, secondMessages, 1, "user", "first")
	assertMessage(t, secondMessages, 2, "assistant", "one")
	assertMessage(t, secondMessages, 3, "user", "second")
	assertNoAdditionalCLIRunRequest(t, requests)

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one chat session log", logPaths)
	}
	data, err := os.ReadFile(logPaths[0])
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logPaths[0], err)
	}
	records := readJSONLRecords(t, data)
	assertCLILogBaseFields(t, records)
	if !hasCLILogRecord(records, "text_delta", "event", "text_delta") {
		t.Fatalf("chat log records missing text_delta: %#v", records)
	}
}

func TestChatREPLRecoverableErrorContinuesWithoutFailedHistory(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`not-json`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config-dir", configDir, "chat"}, strings.NewReader("failed\nsecond\n/quit\n"), &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "two\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	errOut := stderr.String()
	if !strings.HasPrefix(errOut, "> sai: ") || !strings.Contains(errOut, "parse OpenAI chat stream") || !strings.HasSuffix(errOut, "\n> > ") {
		t.Fatalf("stderr = %q, want recoverable error followed by next prompts", errOut)
	}

	firstRequest := <-requests
	firstMessages := requestMessages(t, firstRequest.Body)
	assertMessage(t, firstMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, firstMessages, 1, "user", "failed")

	secondRequest := <-requests
	secondMessages := requestMessages(t, secondRequest.Body)
	if len(secondMessages) != 2 {
		t.Fatalf("len(second request messages) = %d, want 2: %#v", len(secondMessages), secondMessages)
	}
	assertMessage(t, secondMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, secondMessages, 1, "user", "second")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatREPLStdoutWriteErrorExitsWithoutNextTurn(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"delta":{"content":"one"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"two"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	stdoutErr := errors.New("stdout write failed")
	var stderr bytes.Buffer
	code := RunWithIO([]string{"--config-dir", configDir, "chat"}, strings.NewReader("first\nsecond\n/quit\n"), failingWriter{err: stdoutErr}, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithIO() code = %d, want 1", code)
	}
	assertCLIErrorContains(t, stderr.String(), "> sai: stdout write failed")
	firstRequest := <-requests
	firstMessages := requestMessages(t, firstRequest.Body)
	assertMessage(t, firstMessages, 0, "system", builtInBaseInstructions)
	assertMessage(t, firstMessages, 1, "user", "first")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatToolCallHistoryCarriesIntoNextTurn(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"done"}}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"next"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"--config-dir", configDir, "chat", "--enable-tools", "read_file"}, strings.NewReader("Read note\nNext\n/exit\n"), &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithIO() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "done\nnext\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "> \ntool: read_file note.txt\n> > "; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	assertCLIToolStatus(t, stdout.String(), stderr.String(), "read_file note.txt", "tool output")

	<-requests
	<-requests
	thirdRequest := <-requests
	messages := requestMessages(t, thirdRequest.Body)
	if len(messages) != 6 {
		t.Fatalf("len(third request messages) = %d, want 6: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "Read note")
	assertAssistantToolCallMessage(t, messages, 2, "call_1", "read_file", `{"path":"note.txt"}`)
	assertToolMessage(t, messages, 3, "call_1", "tool output")
	assertMessage(t, messages, 4, "assistant", "done")
	assertMessage(t, messages, 5, "user", "Next")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunWithContextCancelReachesProviderRequest(t *testing.T) {
	requestStarted := make(chan capturedCLIRunRequest, 1)
	requestContextDone := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requestStarted <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}

		<-r.Context().Done()
		close(requestContextDone)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- RunWithContext(ctx, []string{"--config-dir", configDir, "chat", "--quit", "hello"}, strings.NewReader(""), &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
	}()

	var request capturedCLIRunRequest
	select {
	case request = <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider request")
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "hello")

	cancel()
	select {
	case <-requestContextDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider request context cancellation")
	}

	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("RunWithContext() code = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunWithContext to return")
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "context canceled")
}

func TestRunWithContextCancelFlushesLoggerAfterStreamEvent(t *testing.T) {
	requestStarted := make(chan capturedCLIRunRequest, 1)
	requestContextDone := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requestStarted <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer does not support flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"streamed response secret\"}}]}\n\n")
		flusher.Flush()

		<-r.Context().Done()
		close(requestContextDone)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	stdout := newSignalingWriter("streamed response secret")
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- RunWithContext(ctx, []string{"--config-dir", configDir, "chat", "--quit", "user prompt secret"}, strings.NewReader(""), stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
	}()

	var request capturedCLIRunRequest
	select {
	case request = <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider request")
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "user prompt secret")

	select {
	case <-stdout.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for streamed text")
	}
	cancel()

	select {
	case <-requestContextDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider request context cancellation")
	}
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("RunWithContext() code = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunWithContext to return")
	}
	if got, want := stdout.String(), "streamed response secret"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIErrorContains(t, stderr.String(), "context canceled")

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one canceled session log", logPaths)
	}
	logData, err := os.ReadFile(logPaths[0])
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logPaths[0], err)
	}
	logText := string(logData)
	for _, leaked := range []string{"user prompt secret", "streamed response secret", "direct-secret-value"} {
		if strings.Contains(logText, leaked) {
			t.Fatalf("log leaked %q:\n%s", leaked, logText)
		}
	}
	records := readJSONLRecords(t, logData)
	assertCLILogBaseFields(t, records)
	if !hasCLILogRecord(records, "text_delta", "event", "text_delta") {
		t.Fatalf("log records missing flushed text_delta: %#v", records)
	}
	if !hasCLILogRecord(records, "error", "message", "read OpenAI chat stream") {
		t.Fatalf("log records missing cancel error: %#v", records)
	}
}

func TestRunUsesDefaultProviderModelAndOutputsTextDelta(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"hello"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL+"/v1", "$SAI_TEST_API_KEY", "openai-chat")
	t.Setenv("SAI_TEST_API_KEY", "env-secret-value")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Say hi"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "hello"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	request := <-requests
	if request.Path != "/v1/chat/completions" {
		t.Fatalf("request path = %q, want /v1/chat/completions", request.Path)
	}
	if request.Authorization != "Bearer env-secret-value" {
		t.Fatalf("Authorization = %q, want env API key", request.Authorization)
	}
	if got := request.Body["model"]; got != "model-default" {
		t.Fatalf("model = %#v, want model-default", got)
	}
	messages := requestMessages(t, request.Body)
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "Say hi")
	assertCLIRequestOmitsKey(t, request.Body, "tools")
}

func TestRunOpenAIResponsesProviderOutputsTextDelta(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"type":"response.output_text.delta","delta":"hello "}`,
		`{"type":"response.output_text.delta","delta":"responses"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "responses-secret-value", "openai-responses")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Say hi"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "hello responses"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	request := <-requests
	if request.Path != "/responses" {
		t.Fatalf("request path = %q, want /responses", request.Path)
	}
	if request.Authorization != "Bearer responses-secret-value" {
		t.Fatalf("Authorization = %q, want Responses API key", request.Authorization)
	}
	if request.ContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", request.ContentType)
	}
	if got := request.Body["model"]; got != "model-default" {
		t.Fatalf("model = %#v, want model-default", got)
	}
	assertJSONNumber(t, request.Body["temperature"], "0.6")
	assertJSONNumber(t, request.Body["max_output_tokens"], "128")
	assertCLIRequestOmitsKey(t, request.Body, "max_tokens")
	input := requestInput(t, request.Body)
	assertMessage(t, input, 0, "system", builtInBaseInstructions)
	assertMessage(t, input, 1, "user", "Say hi")
	assertCLIRequestOmitsKey(t, request.Body, "tools")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunOpenAIResponsesExecutesFunctionCallAndContinuesToFinalText(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"path\":\"note.txt\"}"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"path\":\"note.txt\"}"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"note.txt\"}"}}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`,
		},
		[]string{
			`{"type":"response.output_text.delta","delta":"done"}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":2,"total_tokens":13}}}`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "responses-secret-value", "openai-responses")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-tools", "read_file", "Read note"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "done"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIToolStatus(t, stdout.String(), stderr.String(), "read_file note.txt", "tool output")

	firstRequest := <-requests
	if firstRequest.Path != "/responses" {
		t.Fatalf("first request path = %q, want /responses", firstRequest.Path)
	}
	assertCLIResponsesToolNames(t, firstRequest.Body, []string{"read_file"})

	secondRequest := <-requests
	if secondRequest.Path != "/responses" {
		t.Fatalf("second request path = %q, want /responses", secondRequest.Path)
	}
	assertCLIResponsesToolNames(t, secondRequest.Body, []string{"read_file"})
	input := requestInput(t, secondRequest.Body)
	if len(input) != 4 {
		t.Fatalf("len(second request input) = %d, want 4: %#v", len(input), input)
	}
	assertMessage(t, input, 0, "system", builtInBaseInstructions)
	assertMessage(t, input, 1, "user", "Read note")
	assertResponseFunctionCallInput(t, input, 2, "call_1", "read_file", `{"path":"note.txt"}`)
	assertResponseFunctionCallOutput(t, input, 3, "call_1", "tool output")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunAnthropicMessagesProviderOutputsTextDelta(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello anthropic"}}`,
		`{"type":"message_stop"}`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "anthropic-secret-value", "anthropic-messages")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Say hi"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "hello anthropic"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	request := <-requests
	if request.Path != "/messages" {
		t.Fatalf("request path = %q, want /messages", request.Path)
	}
	if request.XAPIKey != "anthropic-secret-value" {
		t.Fatalf("x-api-key = %q, want Anthropic API key", request.XAPIKey)
	}
	if request.AnthropicVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want 2023-06-01", request.AnthropicVersion)
	}
	if request.ContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", request.ContentType)
	}
	if got := request.Body["model"]; got != "model-default" {
		t.Fatalf("model = %#v, want model-default", got)
	}
	if got := request.Body["system"]; got != builtInBaseInstructions {
		t.Fatalf("system = %#v, want built-in instructions", got)
	}
	assertJSONNumber(t, request.Body["temperature"], "0.6")
	assertJSONNumber(t, request.Body["max_tokens"], "128")
	messages := requestMessages(t, request.Body)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "user", "Say hi")
	assertCLIRequestOmitsKey(t, request.Body, "tools")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunAnthropicMessagesProviderExecutesToolUseAndReturnsToolResult(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_read","name":"read_file","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"note.txt\"}"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_stop"}`,
		},
		[]string{
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
			`{"type":"message_stop"}`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "anthropic-secret-value", "anthropic-messages")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-tools", "read_file", "Read note"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "done"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIToolStatus(t, stdout.String(), stderr.String(), "read_file note.txt", "tool output")

	firstRequest := <-requests
	if firstRequest.Path != "/messages" {
		t.Fatalf("first request path = %q, want /messages", firstRequest.Path)
	}
	assertCLIAnthropicToolNames(t, firstRequest.Body, []string{"read_file"})

	secondRequest := <-requests
	if secondRequest.Path != "/messages" {
		t.Fatalf("second request path = %q, want /messages", secondRequest.Path)
	}
	messages := requestMessages(t, secondRequest.Body)
	if len(messages) != 3 {
		t.Fatalf("len(second request messages) = %d, want 3: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "user", "Read note")
	assertAnthropicAssistantToolUseMessage(t, messages, 1, "call_read", "read_file", "path", "note.txt")
	assertAnthropicToolResultMessage(t, messages, 2, "call_read", "tool output")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunExplicitProviderModelSelectsProfileParameters(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--provider", "fake", "--model", "fast", "Use fast"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	request := <-requests
	if request.Authorization != "Bearer direct-secret-value" {
		t.Fatalf("Authorization = %q, want direct API key", request.Authorization)
	}
	if got := request.Body["model"]; got != "model-fast" {
		t.Fatalf("model = %#v, want model-fast", got)
	}
	assertJSONNumber(t, request.Body["temperature"], "0.2")
	assertJSONNumber(t, request.Body["max_tokens"], "64")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunUsesConfiguredEnabledTools(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"read_file", "write_file", "edit_file", "shell"})

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Use configured tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"read_file", "write_file", "edit_file", "shell"})
}

func TestRunDoesNotExposeEditingToolsWhenOnlyNonEditingToolsAreEnabled(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"read_file", "shell"})

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Do not expose tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"read_file", "shell"})
}

func TestRunEnableToolsOverridesConfiguredTools(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithTools(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"read_file", "shell"})

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-tools", "list_files,write_file,edit_file", "Use CLI tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	assertCLIToolNames(t, (<-requests).Body, []string{"list_files", "write_file", "edit_file"})
}

func TestRunUsesConfiguredEnabledSkillsInMessageOrder(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithSkills(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"zeta", "alpha"})
	alphaInstructions := "Alpha instructions.\n"
	zetaInstructions := "Zeta instructions.\n"
	writeCLISkill(t, configDir, "alpha", "---\nname: Alpha Skill\ndescription: alpha desc\n---\n"+alphaInstructions)
	writeCLISkill(t, configDir, "zeta", "---\nname: Zeta Skill\ndescription: zeta desc\n---\n"+zetaInstructions)
	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "AGENTS.md"), "Project instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Use configured skills"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	messages := requestMessages(t, (<-requests).Body)
	if len(messages) != 5 {
		t.Fatalf("len(messages) = %d, want 5: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "Project instructions\n")
	assertMessage(t, messages, 2, "developer", "Skill zeta (Zeta Skill):\n"+zetaInstructions)
	assertMessage(t, messages, 3, "developer", "Skill alpha (Alpha Skill):\n"+alphaInstructions)
	assertMessage(t, messages, 4, "user", "Use configured skills")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunEnableSkillsOverridesConfiguredSkills(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithSkills(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"alpha"})
	writeCLISkill(t, configDir, "alpha", "Alpha config instructions\n")
	writeCLISkill(t, configDir, "beta", "Beta CLI instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-skills", "beta", "Use CLI skills"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}

	messages := requestMessages(t, (<-requests).Body)
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "Skill beta (beta):\nBeta CLI instructions\n")
	assertMessage(t, messages, 2, "user", "Use CLI skills")
}

func TestRunConfiguredSkillIgnoresDisabledMalformedSkill(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithSkills(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"valid"})
	writeCLISkill(t, configDir, "valid", "Valid instructions\n")
	writeCLISkill(t, configDir, "bad", "---\nname: [bad\n---\nBad instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Use configured skill"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	messages := requestMessages(t, (<-requests).Body)
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "Skill valid (valid):\nValid instructions\n")
	assertMessage(t, messages, 2, "user", "Use configured skill")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunDisableSkillsSkipsConfiguredSkillLoading(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDirWithSkills(t, configDir, server.URL, "direct-secret-value", "openai-chat", []string{"bad"})
	writeCLISkill(t, configDir, "bad", "---\nname: [bad\n---\nBad instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--disable-skills", "Do not use skills"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	messages := requestMessages(t, (<-requests).Body)
	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "Do not use skills")
}

func TestRunEnableSkillsAndDisableSkillsConflict(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"chat", "--quit", "--enable-skills", "alpha", "--disable-skills", "Use skills"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "cannot use --enable-skills with --disable-skills", `Run "sai help chat" for usage.`)
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunEnableSkillsRejectsUnknownSkill(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	writeCLISkill(t, configDir, "beta", "Beta instructions\n")
	writeCLISkill(t, configDir, "alpha", "Alpha instructions\n")
	writeCLISkill(t, configDir, "bad", "---\nname: [bad\n---\nBad instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-skills", "missing", "Use skills"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown skill "missing"`, "available skills: alpha, bad, beta")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunEnableSkillsReportsMalformedFrontmatter(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	writeCLISkill(t, configDir, "bad", "---\nname: [bad\n---\nBad instructions\n")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-skills", "bad", "Use skills"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), "load skills: parse skill frontmatter", "bad", "SKILL.md")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunEnableMCPExposesOnlyEnabledMCPSchemas(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	exitFile := filepath.Join(t.TempDir(), "mcp-exited")
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	writeCLIRunMCPFixture(t, configDir, exitFile)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-mcp", "local", "--enable-tools", "list_files,mcp.local.search", "Use mixed tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	request := <-requests
	assertCLIToolNames(t, request.Body, []string{"list_files", "mcp.local.search"})
	assertNoAdditionalCLIRunRequest(t, requests)
	assertCLIFileEventuallyContains(t, exitFile, "closed")
}

func TestRunRoutesMCPToolCallAndReturnsResultToModel(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_mcp","function":{"name":"mcp.local.search","arguments":"{\"query\":\"needle\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"final answer"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	configDir := t.TempDir()
	exitFile := filepath.Join(t.TempDir(), "mcp-exited")
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")
	writeCLIRunMCPFixture(t, configDir, exitFile)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-mcp", "local", "--enable-tools", "mcp.local.search", "Use MCP search"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "final answer"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	firstRequest := <-requests
	assertCLIToolNames(t, firstRequest.Body, []string{"mcp.local.search"})

	secondRequest := <-requests
	messages := requestMessages(t, secondRequest.Body)
	if len(messages) != 4 {
		t.Fatalf("len(second request messages) = %d, want 4: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "Use MCP search")
	assertAssistantToolCallMessage(t, messages, 2, "call_mcp", "mcp.local.search", `{"query":"needle"}`)
	assertToolMessage(t, messages, 3, "call_mcp", "mcp result one\nmcp result two")
	assertNoAdditionalCLIRunRequest(t, requests)
	assertCLIFileEventuallyContains(t, exitFile, "closed")
}

func TestRunVerboseWritesDiagnosticsWithoutSensitiveContent(t *testing.T) {
	t.Run("direct API key", func(t *testing.T) {
		server, requests := newCLIRunServer(t,
			`{"choices":[{"delta":{"content":"model response secret"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--verbose", "--enable-tools", "list_files,read_file", "user prompt secret"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "model response secret"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
		assertCLIVerboseContains(t, stderr.String(),
			"config_dir: "+filepath.Clean(configDir),
			"provider: fake",
			"model_profile: default",
			"model_id: model-default",
			"max_turns: 8",
			"enabled_tools: list_files,read_file",
			"show_reasoning: false",
		)
		logPaths := sessionLogPaths(t, configDir)
		if len(logPaths) != 1 {
			t.Fatalf("session log paths = %#v, want one", logPaths)
		}
		assertCLIVerboseContains(t, stderr.String(), "log_path: "+logPaths[0])
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value", "user prompt secret", "model response secret")
		<-requests
		assertNoAdditionalCLIRunRequest(t, requests)
	})

	t.Run("env API key", func(t *testing.T) {
		server, requests := newCLIRunServer(t,
			`{"choices":[{"delta":{"content":"env response secret"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "$SAI_VERBOSE_API_KEY", "openai-chat")
		t.Setenv("SAI_VERBOSE_API_KEY", "env-secret-value")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--verbose", "env user prompt secret"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "env response secret"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
		assertCLIVerboseContains(t, stderr.String(),
			"provider: fake",
			"model_id: model-default",
			"enabled_tools: (none)",
		)
		assertCLIErrorOmits(t, stderr.String(), "env-secret-value", "env user prompt secret", "env response secret")
		<-requests
		assertNoAdditionalCLIRunRequest(t, requests)
	})
}

func TestRunVerboseReportsFutureSessionLogPathBeforeFirstEvent(t *testing.T) {
	releaseResponse := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseResponse)
		})
	}

	requests := make(chan capturedCLIRunRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requests <- capturedCLIRunRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			RawBody:       body,
			Body:          decodeCLIJSON(t, body),
		}

		<-releaseResponse
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	defer release()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--verbose", "hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
	}()

	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first model request")
	}

	futurePath := verboseLogPath(t, stderr.String())
	if futurePath == "(disabled)" {
		t.Fatalf("verbose log_path = %q, want future session path", futurePath)
	}
	if filepath.Base(futurePath) != "sai.jsonl" {
		t.Fatalf("verbose log_path = %q, want sai.jsonl file", futurePath)
	}
	if logPaths := sessionLogPaths(t, configDir); len(logPaths) != 0 {
		t.Fatalf("session log paths before first event = %#v, want none", logPaths)
	}

	release()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for run to finish")
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one", logPaths)
	}
	if logPaths[0] != futurePath {
		t.Fatalf("session log path = %q, want verbose future path %q", logPaths[0], futurePath)
	}
}

func TestRunExecutesToolCallAndContinuesToFinalText(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"done"}}]}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-tools", "read_file", "Read note"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "done"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIToolStatus(t, stdout.String(), stderr.String(), "read_file note.txt", "tool output")

	firstRequest := <-requests
	if got := firstRequest.Body["model"]; got != "model-default" {
		t.Fatalf("first request model = %#v, want model-default", got)
	}
	assertCLIToolNames(t, firstRequest.Body, []string{"read_file"})

	secondRequest := <-requests
	if got := secondRequest.Body["model"]; got != "model-default" {
		t.Fatalf("second request model = %#v, want model-default", got)
	}
	messages := requestMessages(t, secondRequest.Body)
	if len(messages) != 4 {
		t.Fatalf("len(second request messages) = %d, want 4: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "user", "Read note")
	assertAssistantToolCallMessage(t, messages, 2, "call_1", "read_file", `{"path":"note.txt"}`)
	assertToolMessage(t, messages, 3, "call_1", "tool output")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunWritesJSONLLogForToolCallWithoutSensitiveContent(t *testing.T) {
	server, requests := newSequentialCLIRunServer(t,
		[]string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		},
		[]string{
			`{"choices":[{"delta":{"content":"final response secret"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":13,"total_tokens":24}}`,
			`[DONE]`,
		},
	)
	defer server.Close()

	projectDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "note.txt"), "tool output secret")
	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "secret-api-key", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-tools", "read_file", "user prompt secret"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "final response secret"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIToolStatus(t, stdout.String(), stderr.String(), "read_file note.txt", "tool output secret")
	<-requests
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 1 {
		t.Fatalf("session log paths = %#v, want one", logPaths)
	}
	logPath := logPaths[0]
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", logPath, err)
	}
	logText := string(logData)
	for _, leaked := range []string{"user prompt secret", "final response secret", "tool output secret", "secret-api-key"} {
		if strings.Contains(logText, leaked) {
			t.Fatalf("log leaked %q:\n%s", leaked, logText)
		}
	}

	records := readJSONLRecords(t, logData)
	assertCLILogBaseFields(t, records)
	if !hasCLILogRecord(records, "tool_call_done", "tool_name", "read_file") {
		t.Fatalf("log records missing read_file tool_call_done: %#v", records)
	}
	if !hasCLILogRecord(records, "tool_result", "tool_name", "read_file") || !hasCLILogRecord(records, "tool_result", "is_error", false) {
		t.Fatalf("log records missing successful tool_result metadata: %#v", records)
	}
	usage := firstCLILogRecord(t, records, "usage")["usage"]
	usageMap, ok := usage.(map[string]any)
	if !ok {
		t.Fatalf("usage field = %T(%#v), want object", usage, usage)
	}
	assertJSONNumber(t, usageMap["input_tokens"], "11")
	assertJSONNumber(t, usageMap["output_tokens"], "13")
	assertJSONNumber(t, usageMap["total_tokens"], "24")
}

func TestRunCreatesSeparateSessionLogs(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "secret-api-key", "openai-chat")

	for _, prompt := range []string{"first", "second"} {
		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", prompt}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})
		if code != 0 {
			t.Fatalf("RunWithGetwd(%q) code = %d, stderr = %s", prompt, code, stderr.String())
		}
		if got, want := stdout.String(), "ok"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
		<-requests
	}
	assertNoAdditionalCLIRunRequest(t, requests)

	logPaths := sessionLogPaths(t, configDir)
	if len(logPaths) != 2 {
		t.Fatalf("session log paths = %#v, want two", logPaths)
	}
	if logPaths[0] == logPaths[1] {
		t.Fatalf("two runs wrote the same log path: %q", logPaths[0])
	}
	for _, logPath := range logPaths {
		if filepath.Base(logPath) != "sai.jsonl" {
			t.Fatalf("session log path = %q, want sai.jsonl file", logPath)
		}
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", logPath, err)
		}
		records := readJSONLRecords(t, data)
		assertCLILogBaseFields(t, records)
		firstCLILogRecord(t, records, "usage")
	}
}

func TestRunWithLoggingDisabledDoesNotCreateSessionLog(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "secret-api-key", "openai-chat")
	setCLILoggingPath(t, configDir, `""`)

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--verbose", "hello"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertCLIVerboseContains(t, stderr.String(), "log_path: (disabled)")
	if logPaths := sessionLogPaths(t, configDir); len(logPaths) != 0 {
		t.Fatalf("session log paths = %#v, want none", logPaths)
	}
	<-requests
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunEnableToolsRejectsUnknownTool(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-tools", "missing", "Use tools"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `enabled tool "missing" is not registered`)
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunEnableMCPRejectsUnknownServer(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"unexpected"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--enable-mcp", "missing", "Use MCP"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 1 {
		t.Fatalf("RunWithGetwd() code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	assertCLIErrorContains(t, stderr.String(), `unknown MCP server "missing"`, "available MCP servers: (none)")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestChatAllowsPostPromptModelFlag(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	configDir := t.TempDir()
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--model", "fast", "Use fast", "--model", "default"}, &stdout, &stderr, func() (string, error) {
		return t.TempDir(), nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), "ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	request := <-requests
	if got := request.Body["model"]; got != "model-default" {
		t.Fatalf("model = %#v, want model-default", got)
	}
	assertMessage(t, requestMessages(t, request.Body), 1, "user", "Use fast")
	assertNoAdditionalCLIRunRequest(t, requests)
}

func TestRunIncludesStartupAgentsAndConfigDirDoesNotChangeLookup(t *testing.T) {
	server, requests := newCLIRunServer(t,
		`{"choices":[{"delta":{"content":"done"}}]}`,
		`[DONE]`,
	)
	defer server.Close()

	projectDir := t.TempDir()
	configDir := t.TempDir()
	writeCLIFile(t, filepath.Join(projectDir, "AGENTS.md"), "project instructions\n")
	writeCLIFile(t, filepath.Join(configDir, "AGENTS.md"), "config instructions should not be used\n")
	writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Hello"}, &stdout, &stderr, func() (string, error) {
		return projectDir, nil
	})

	if code != 0 {
		t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
	}

	request := <-requests
	messages := requestMessages(t, request.Body)
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3: %#v", len(messages), messages)
	}
	assertMessage(t, messages, 0, "system", builtInBaseInstructions)
	assertMessage(t, messages, 1, "developer", "project instructions\n")
	assertMessage(t, messages, 2, "user", "Hello")
	if strings.Contains(string(request.RawBody), "config instructions should not be used") {
		t.Fatalf("request included AGENTS.md from config dir: %s", request.RawBody)
	}
}

func TestRunReasoningIsHiddenUnlessShowReasoningIsSet(t *testing.T) {
	t.Run("default hidden", func(t *testing.T) {
		server, _ := newCLIRunServer(t,
			`{"choices":[{"delta":{"reasoning_content":"hidden"}}]}`,
			`{"choices":[{"delta":{"content":"visible"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "visible"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("flag shows reasoning", func(t *testing.T) {
		server, _ := newCLIRunServer(t,
			`{"choices":[{"delta":{"reasoning_content":"shown"}}]}`,
			`{"choices":[{"delta":{"content":"visible"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--show-reasoning", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "shown\nvisible"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("flag does not duplicate reasoning newline", func(t *testing.T) {
		server, _ := newCLIRunServer(t,
			`{"choices":[{"delta":{"reasoning_content":"shown\n"}}]}`,
			`{"choices":[{"delta":{"content":"visible"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--show-reasoning", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "shown\nvisible"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("flag without reasoning does not add newline", func(t *testing.T) {
		server, _ := newCLIRunServer(t,
			`{"choices":[{"delta":{"content":"visible"}}]}`,
			`[DONE]`,
		)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--show-reasoning", "Think"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 0 {
			t.Fatalf("RunWithGetwd() code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "visible"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})
}

func TestWriteStreamDoesNotColorReasoningForBufferOutput(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStream(&stdout, &stderr, cliEventStream(
		model.ReasoningDeltaEvent{Text: "shown"},
		model.TextDeltaEvent{Text: "visible"},
	), true, nil)

	if err != nil {
		t.Fatalf("writeStream() error = %v", err)
	}
	if got, want := stdout.String(), "shown\nvisible"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stdout.String(), "\x1b[") {
		t.Fatalf("stdout contains ANSI escape sequence: %q", stdout.String())
	}
}

func TestWriteStreamWithOptionsColorsReasoningAndResetsBeforeFinalText(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStreamWithOptions(&stdout, &stderr, cliEventStream(
		model.ReasoningDeltaEvent{Text: "thinking"},
		model.TextDeltaEvent{Text: "final"},
	), true, nil, streamOutputOptions{colorReasoning: true})

	if err != nil {
		t.Fatalf("writeStreamWithOptions() error = %v", err)
	}
	if got, want := stdout.String(), reasoningColorDarkGray+"thinking"+ansiReset+"\nfinal"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestWriteStreamStartsReasoningOnIndependentLine(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStream(&stdout, &stderr, cliEventStream(
		model.TextDeltaEvent{Text: "prefix"},
		model.ReasoningDeltaEvent{Text: "thinking"},
		model.TextDeltaEvent{Text: "final"},
	), true, nil)

	if err != nil {
		t.Fatalf("writeStream() error = %v", err)
	}
	if got, want := stdout.String(), "prefix\nthinking\nfinal"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestWriteStreamWithOptionsResetsAfterReasoningOnly(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStreamWithOptions(&stdout, &stderr, cliEventStream(
		model.ReasoningDeltaEvent{Text: "thinking"},
	), true, nil, streamOutputOptions{colorReasoning: true})

	if err != nil {
		t.Fatalf("writeStreamWithOptions() error = %v", err)
	}
	if got, want := stdout.String(), reasoningColorDarkGray+"thinking"+ansiReset; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestWriteStreamWithOptionsResetsReasoningBeforeToolAndReentersCleanly(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStreamWithOptions(&stdout, &stderr, cliEventStream(
		model.ReasoningDeltaEvent{Text: "thinking"},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "read_file", Arguments: `{"path":"note.txt"}`}},
		model.ReasoningDeltaEvent{Text: "again"},
		model.TextDeltaEvent{Text: "final"},
	), true, nil, streamOutputOptions{colorReasoning: true, colorToolStatus: true})

	if err != nil {
		t.Fatalf("writeStreamWithOptions() error = %v", err)
	}
	if got, want := stdout.String(), reasoningColorDarkGray+"thinking"+ansiReset+"\n"+reasoningColorDarkGray+"again"+ansiReset+"\nfinal"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "\n"+reasoningColorDarkGray+"tool: read_file note.txt"+ansiReset+"\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestWriteStreamWritesToolStatusWithSafeDetailsOnly(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStream(&stdout, &stderr, cliEventStream(
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "read_file", Arguments: `{"path":"docs/checklist.md"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "list_files", Arguments: `{}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "list_files", Arguments: ``}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "list_files", Arguments: " \n\t "}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "read_file", Arguments: `[`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "list_files", Arguments: `null`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "write_file", Arguments: `{"path":"draft.txt","content":"secret-write-content"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "edit_file", Arguments: `{"path":"draft.txt","old":"secret-old","new":"secret-new"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "shell", Arguments: `{"command":"echo secret-command"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "mcp.local.search", Arguments: `{"query":"secret-query"}`}},
		model.ToolResultEvent{Result: model.ToolResult{Name: "read_file", Content: "secret result body"}},
		model.ToolResultEvent{Result: model.ToolResult{Name: "write_file", Content: "wrote draft.txt (20 bytes)"}},
		model.ToolResultEvent{Result: model.ToolResult{Name: "edit_file", Content: "edited draft.txt (1 replacement)"}},
	), false, nil)

	if err != nil {
		t.Fatalf("writeStream() error = %v", err)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "tool: read_file docs/checklist.md\ntool: list_files .\ntool: list_files .\ntool: list_files .\ntool: read_file\ntool: list_files\ntool: write_file draft.txt\ntool: edit_file draft.txt\ntool: shell\ntool: mcp.local.search\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	assertCLIErrorOmits(t, stderr.String(), "secret-write-content", "secret-old", "secret-new", "secret-command", "secret-query", "secret result body", "wrote draft.txt", "edited draft.txt")
}

func TestWriteStreamPutsConsecutiveToolStatusesOnIndependentLinesAfterOutputAndPrompt(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := writeStreamWithOptions(&stdout, &stderr, cliEventStream(
		model.TextDeltaEvent{Text: "partial"},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "shell", Arguments: `{"command":"echo hidden"}`}},
		model.ToolCallDoneEvent{ToolCall: model.ToolCall{Name: "read_file", Arguments: `{"path":"note.txt"}`}},
	), false, nil, streamOutputOptions{stderrNeedsLeadingBreak: true})

	if err != nil {
		t.Fatalf("writeStreamWithOptions() error = %v", err)
	}
	if got, want := stdout.String(), "partial"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "\ntool: shell\ntool: read_file note.txt\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	assertCLIErrorOmits(t, stderr.String(), "echo hidden")
}

func TestShouldColorizeWriterRequiresTerminalAndHonorsNOColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buffer bytes.Buffer
	if shouldColorizeWriter(&buffer) {
		t.Fatal("shouldColorizeWriter(bytes.Buffer) = true, want false")
	}

	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	defer file.Close()
	if shouldColorizeWriter(file) {
		t.Fatal("shouldColorizeWriter(regular file) = true, want false")
	}

	t.Setenv("NO_COLOR", "1")
	if shouldColorizeWriter(os.Stdout) {
		t.Fatal("shouldColorizeWriter(os.Stdout) = true with NO_COLOR set, want false")
	}
}

func TestRunErrorsDoNotLeakAPIKeyValues(t *testing.T) {
	t.Run("missing API key", func(t *testing.T) {
		server, requests := newCLIRunServer(t, `[DONE]`)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "$SAI_MISSING_API_KEY", "openai-chat")
		t.Setenv("SAI_MISSING_API_KEY", "")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stdout.String() != "" {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		assertCLIErrorContains(t, stderr.String(), `API key environment variable "SAI_MISSING_API_KEY" is not set`)
		assertCLIErrorOmits(t, stderr.String(), "Bearer ")
		assertNoAdditionalCLIRunRequest(t, requests)
	})

	t.Run("HTTP failure", func(t *testing.T) {
		requests := make(chan capturedCLIRunRequest, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("ReadAll() error = %v", err)
			}
			requests <- capturedCLIRunRequest{
				Path:          r.URL.Path,
				Authorization: r.Header.Get("Authorization"),
				RawBody:       body,
				Body:          decodeCLIJSON(t, body),
			}
			http.Error(w, "bad request for Authorization: Bearer direct-secret-value", http.StatusBadRequest)
		}))
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stdout.String() != "" {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		assertCLIErrorContains(t, stderr.String(), "request model", "OpenAI chat request failed", "400 Bad Request", "bad request")
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value", "Bearer direct-secret-value")

		request := <-requests
		if request.Authorization != "Bearer direct-secret-value" {
			t.Fatalf("Authorization = %q, want direct API key", request.Authorization)
		}
		logPaths := sessionLogPaths(t, configDir)
		if len(logPaths) != 1 {
			t.Fatalf("session log paths = %#v, want one error log", logPaths)
		}
		data, err := os.ReadFile(logPaths[0])
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", logPaths[0], err)
		}
		records := readJSONLRecords(t, data)
		assertCLILogBaseFields(t, records)
		errorRecord := firstCLILogRecord(t, records, "error")
		if errorRecord["level"] != "error" || errorRecord["message"] != "request model" {
			t.Fatalf("error log record = %#v, want level error with request model message", errorRecord)
		}
		assertNoAdditionalCLIRunRequest(t, requests)
	})

	t.Run("invalid SSE chunk", func(t *testing.T) {
		server, requests := newCLIRunServer(t, `{not-json`)
		defer server.Close()

		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, server.URL, "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		if stdout.String() != "" {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		assertCLIErrorContains(t, stderr.String(), "parse OpenAI chat stream", "parse chat completion chunk")
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value", "Bearer direct-secret-value")
		<-requests
		assertNoAdditionalCLIRunRequest(t, requests)
	})

	t.Run("unknown model", func(t *testing.T) {
		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "openai-chat")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "--model", "missing", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		assertCLIErrorContains(t, stderr.String(), `unknown model "missing" for provider "fake"`)
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value")
	})

	t.Run("unsupported provider type", func(t *testing.T) {
		configDir := t.TempDir()
		writeCLIRunFixtureInDir(t, configDir, "http://127.0.0.1:1", "direct-secret-value", "not-openai")

		var stdout, stderr bytes.Buffer
		code := RunWithGetwd([]string{"--config-dir", configDir, "chat", "--quit", "Hello"}, &stdout, &stderr, func() (string, error) {
			return t.TempDir(), nil
		})

		if code != 1 {
			t.Fatalf("RunWithGetwd() code = %d, want 1", code)
		}
		assertCLIErrorContains(t, stderr.String(), `unknown provider type "not-openai"`, "supported provider types: anthropic-messages, openai-chat, openai-responses")
		assertCLIErrorOmits(t, stderr.String(), "direct-secret-value")
	})
}

func writeCLIFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	writeCLIFixtureInDir(t, dir)
	return dir
}

func writeCLIFixtureInDir(t *testing.T, dir string) {
	t.Helper()

	providersDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeCLIFile(t, filepath.Join(dir, "sai.yaml"), `default_provider: paperhub
default_model: glm-5.2
provider_dir: providers

agent:
  max_turns: 8
  stream: true
  show_reasoning: false

tools:
  enabled: []

logging:
  path: logs/sai.jsonl
  level: info
`)

	writeCLIFile(t, filepath.Join(providersDir, "paperhub.yaml"), `name: paperhub
type: openai-chat
base_url: https://tc-paperhub.diezhi.net/v1
api_key: $PAPERHUB_API_KEY

models:
  glm-5.2:
    id: glm-5.2
    temperature: 0.6
    max_tokens: 4096
  glm-5.2-fast:
    id: glm-5.2
    temperature: 0.2
    max_tokens: 2048
`)

	writeCLIFile(t, filepath.Join(providersDir, "local.yaml"), `name: local
type: openai-chat
base_url: http://localhost:8080/v1
api_key: direct-secret-value

models:
  small:
    id: local-small
`)
}

func writeCLIMCPFixture(t *testing.T, dir string) {
	t.Helper()

	mcpDir := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeCLIFile(t, filepath.Join(mcpDir, "local.yaml"), `id: local
enabled: true
command: example-mcp-server
args: []
env: {}
`)

	writeCLIFile(t, filepath.Join(mcpDir, "remote.yaml"), `id: remote
enabled: false
command: remote-mcp-server
args: []
env: {}
`)
}

func writeCLIRunMCPFixture(t *testing.T, dir, exitFile string) {
	t.Helper()

	mcpDir := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeCLIFile(t, filepath.Join(mcpDir, "local.yaml"), fmt.Sprintf(`id: local
enabled: false
command: %q
args:
  - "-test.run=TestCLIMCPHelperProcess"
  - "--"
  - "fake-mcp"
env:
  SAI_CLI_MCP_HELPER_PROCESS: "1"
  SAI_CLI_MCP_EXIT_FILE: %q
`, os.Args[0], exitFile))
}

func writeCLIFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func setCLILoggingPath(t *testing.T, configDir, path string) {
	t.Helper()

	configPath := filepath.Join(configDir, "sai.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", configPath, err)
	}
	updated := strings.Replace(string(data), "  path: logs/sai.jsonl", "  path: "+path, 1)
	if updated == string(data) {
		t.Fatalf("sai.yaml did not contain logging path to replace:\n%s", data)
	}
	writeCLIFile(t, configPath, updated)
}

func writeCLISkill(t *testing.T, configDir, id, content string) {
	t.Helper()
	skillDir := filepath.Join(configDir, "skills", id)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", skillDir, err)
	}
	writeCLIFile(t, filepath.Join(skillDir, "SKILL.md"), content)
}

type capturedCLIRunRequest struct {
	Path             string
	Authorization    string
	XAPIKey          string
	AnthropicVersion string
	ContentType      string
	RawBody          []byte
	Body             map[string]any
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

type signalingWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	want  string
	wrote chan struct{}
	once  sync.Once
}

func newSignalingWriter(want string) *signalingWriter {
	return &signalingWriter{
		want:  want,
		wrote: make(chan struct{}),
	}
}

func (w *signalingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.wrote) })
	}
	return n, err
}

func (w *signalingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func newCLIRunServer(t *testing.T, chunks ...string) (*httptest.Server, <-chan capturedCLIRunRequest) {
	t.Helper()

	requests := make(chan capturedCLIRunRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requests <- capturedCLIRunRequest{
			Path:             r.URL.Path,
			Authorization:    r.Header.Get("Authorization"),
			XAPIKey:          r.Header.Get("x-api-key"),
			AnthropicVersion: r.Header.Get("anthropic-version"),
			ContentType:      r.Header.Get("Content-Type"),
			RawBody:          body,
			Body:             decodeCLIJSON(t, body),
		}

		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	return server, requests
}

func newSequentialCLIRunServer(t *testing.T, responses ...[]string) (*httptest.Server, <-chan capturedCLIRunRequest) {
	t.Helper()

	requests := make(chan capturedCLIRunRequest, len(responses))
	var mu sync.Mutex
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requests <- capturedCLIRunRequest{
			Path:             r.URL.Path,
			Authorization:    r.Header.Get("Authorization"),
			XAPIKey:          r.Header.Get("x-api-key"),
			AnthropicVersion: r.Header.Get("anthropic-version"),
			ContentType:      r.Header.Get("Content-Type"),
			RawBody:          body,
			Body:             decodeCLIJSON(t, body),
		}

		mu.Lock()
		index := requestCount
		requestCount++
		mu.Unlock()
		if index >= len(responses) {
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range responses[index] {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	return server, requests
}

func writeCLIRunFixtureInDir(t *testing.T, dir, baseURL, apiKey, providerType string) {
	t.Helper()
	writeCLIRunFixtureInDirWithTools(t, dir, baseURL, apiKey, providerType, nil)
}

func writeCLIRunFixtureInDirWithTools(t *testing.T, dir, baseURL, apiKey, providerType string, enabledTools []string) {
	t.Helper()
	writeCLIRunFixtureInDirWithToolsAndSkills(t, dir, baseURL, apiKey, providerType, enabledTools, nil)
}

func writeCLIRunFixtureInDirWithSkills(t *testing.T, dir, baseURL, apiKey, providerType string, enabledSkills []string) {
	t.Helper()
	writeCLIRunFixtureInDirWithToolsAndSkills(t, dir, baseURL, apiKey, providerType, nil, enabledSkills)
}

func writeCLIRunFixtureInDirWithToolsAndSkills(t *testing.T, dir, baseURL, apiKey, providerType string, enabledTools []string, enabledSkills []string) {
	t.Helper()

	providersDir := filepath.Join(dir, "providers")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	writeCLIFile(t, filepath.Join(dir, "sai.yaml"), fmt.Sprintf(`default_provider: fake
default_model: default
provider_dir: providers

agent:
  max_turns: 8
  stream: true
  show_reasoning: false

tools:
  enabled: %s

skills:
  enabled: %s

logging:
  path: logs/sai.jsonl
  level: info
`, formatEnabledToolsYAML(enabledTools), formatEnabledToolsYAML(enabledSkills)))

	writeCLIFile(t, filepath.Join(providersDir, "fake.yaml"), fmt.Sprintf(`name: fake
type: %s
base_url: %s
api_key: %s

models:
  default:
    id: model-default
    temperature: 0.6
    max_tokens: 128
  fast:
    id: model-fast
    temperature: 0.2
    max_tokens: 64
`, providerType, baseURL, apiKey))
}

func formatEnabledToolsYAML(enabledTools []string) string {
	if len(enabledTools) == 0 {
		return "[]"
	}

	var out strings.Builder
	out.WriteByte('\n')
	for _, name := range enabledTools {
		fmt.Fprintf(&out, "    - %s\n", name)
	}
	return strings.TrimRight(out.String(), "\n")
}

func decodeCLIJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()

	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode request JSON %q: %v", data, err)
	}
	return value
}

func readJSONLRecords(t *testing.T, data []byte) []map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode JSONL line %q: %v", line, err)
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		t.Fatal("JSONL log has no records")
	}
	return records
}

func cliEventStream(events ...model.Event) <-chan model.Event {
	ch := make(chan model.Event, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch
}

func requestMessages(t *testing.T, body map[string]any) []any {
	t.Helper()

	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %T, want []any", body["messages"])
	}
	return messages
}

func requestInput(t *testing.T, body map[string]any) []any {
	t.Helper()

	input, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("input = %T, want []any", body["input"])
	}
	return input
}

func assertMessage(t *testing.T, messages []any, index int, role, content string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing message %d in %#v", index, messages)
	}
	message, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("message[%d] = %T, want object", index, messages[index])
	}
	if got := message["role"]; got != role {
		t.Fatalf("message[%d].role = %#v, want %q", index, got, role)
	}
	if got := message["content"]; got != content {
		t.Fatalf("message[%d].content = %#v, want %q", index, got, content)
	}
}

func assertAssistantToolCallMessage(t *testing.T, messages []any, index int, id, name, arguments string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing message %d in %#v", index, messages)
	}
	message, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("message[%d] = %T, want object", index, messages[index])
	}
	if got := message["role"]; got != "assistant" {
		t.Fatalf("message[%d].role = %#v, want assistant", index, got)
	}
	if got := message["content"]; got != "" {
		t.Fatalf("message[%d].content = %#v, want empty string", index, got)
	}
	toolCalls, ok := message["tool_calls"].([]any)
	if !ok {
		t.Fatalf("message[%d].tool_calls = %T, want []any", index, message["tool_calls"])
	}
	if len(toolCalls) != 1 {
		t.Fatalf("len(message[%d].tool_calls) = %d, want 1", index, len(toolCalls))
	}
	toolCall, ok := toolCalls[0].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0] = %T, want object", toolCalls[0])
	}
	if got := toolCall["id"]; got != id {
		t.Fatalf("tool_calls[0].id = %#v, want %q", got, id)
	}
	if got := toolCall["type"]; got != "function" {
		t.Fatalf("tool_calls[0].type = %#v, want function", got)
	}
	function, ok := toolCall["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0].function = %T, want object", toolCall["function"])
	}
	if got := function["name"]; got != name {
		t.Fatalf("tool_calls[0].function.name = %#v, want %q", got, name)
	}
	if got := function["arguments"]; got != arguments {
		t.Fatalf("tool_calls[0].function.arguments = %#v, want %q", got, arguments)
	}
}

func assertToolMessage(t *testing.T, messages []any, index int, toolCallID, content string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing message %d in %#v", index, messages)
	}
	message, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("message[%d] = %T, want object", index, messages[index])
	}
	if got := message["role"]; got != "tool" {
		t.Fatalf("message[%d].role = %#v, want tool", index, got)
	}
	if got := message["tool_call_id"]; got != toolCallID {
		t.Fatalf("message[%d].tool_call_id = %#v, want %q", index, got, toolCallID)
	}
	if got := message["content"]; got != content {
		t.Fatalf("message[%d].content = %#v, want %q", index, got, content)
	}
}

func assertAnthropicAssistantToolUseMessage(t *testing.T, messages []any, index int, id, name, inputKey, inputValue string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing message %d in %#v", index, messages)
	}
	message, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("message[%d] = %T, want object", index, messages[index])
	}
	if got := message["role"]; got != "assistant" {
		t.Fatalf("message[%d].role = %#v, want assistant", index, got)
	}
	blocks, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("message[%d].content = %T, want []any", index, message["content"])
	}
	if len(blocks) != 1 {
		t.Fatalf("len(message[%d].content) = %d, want 1: %#v", index, len(blocks), blocks)
	}
	block, ok := blocks[0].(map[string]any)
	if !ok {
		t.Fatalf("message[%d].content[0] = %T, want object", index, blocks[0])
	}
	if got := block["type"]; got != "tool_use" {
		t.Fatalf("message[%d].content[0].type = %#v, want tool_use", index, got)
	}
	if got := block["id"]; got != id {
		t.Fatalf("message[%d].content[0].id = %#v, want %q", index, got, id)
	}
	if got := block["name"]; got != name {
		t.Fatalf("message[%d].content[0].name = %#v, want %q", index, got, name)
	}
	input, ok := block["input"].(map[string]any)
	if !ok {
		t.Fatalf("message[%d].content[0].input = %T, want object", index, block["input"])
	}
	if got := input[inputKey]; got != inputValue {
		t.Fatalf("message[%d].content[0].input[%q] = %#v, want %q", index, inputKey, got, inputValue)
	}
}

func assertAnthropicToolResultMessage(t *testing.T, messages []any, index int, toolUseID, content string) {
	t.Helper()

	if index >= len(messages) {
		t.Fatalf("missing message %d in %#v", index, messages)
	}
	message, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("message[%d] = %T, want object", index, messages[index])
	}
	if got := message["role"]; got != "user" {
		t.Fatalf("message[%d].role = %#v, want user", index, got)
	}
	blocks, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("message[%d].content = %T, want []any", index, message["content"])
	}
	if len(blocks) != 1 {
		t.Fatalf("len(message[%d].content) = %d, want 1: %#v", index, len(blocks), blocks)
	}
	block, ok := blocks[0].(map[string]any)
	if !ok {
		t.Fatalf("message[%d].content[0] = %T, want object", index, blocks[0])
	}
	if got := block["type"]; got != "tool_result" {
		t.Fatalf("message[%d].content[0].type = %#v, want tool_result", index, got)
	}
	if got := block["tool_use_id"]; got != toolUseID {
		t.Fatalf("message[%d].content[0].tool_use_id = %#v, want %q", index, got, toolUseID)
	}
	if got := block["content"]; got != content {
		t.Fatalf("message[%d].content[0].content = %#v, want %q", index, got, content)
	}
	if got, ok := block["is_error"]; ok && got != false {
		t.Fatalf("message[%d].content[0].is_error = %#v, want absent or false", index, got)
	}
}

func assertCLIRequestOmitsKey(t *testing.T, body map[string]any, key string) {
	t.Helper()

	if _, ok := body[key]; ok {
		t.Fatalf("request body contains unexpected key %q: %#v", key, body[key])
	}
}

func assertCLIAnthropicToolNames(t *testing.T, body map[string]any, want []string) {
	t.Helper()

	toolsValue, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %T(%#v), want []any", body["tools"], body["tools"])
	}
	if len(toolsValue) != len(want) {
		t.Fatalf("len(tools) = %d, want %d: %#v", len(toolsValue), len(want), toolsValue)
	}
	for i, wantName := range want {
		tool, ok := toolsValue[i].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d] = %T, want object", i, toolsValue[i])
		}
		if got := tool["name"]; got != wantName {
			t.Fatalf("tools[%d].name = %#v, want %q", i, got, wantName)
		}
		if got := tool["description"]; got == "" {
			t.Fatalf("tools[%d].description is empty", i)
		}
		inputSchema, ok := tool["input_schema"].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d].input_schema = %T, want object", i, tool["input_schema"])
		}
		if got := inputSchema["type"]; got != "object" {
			t.Fatalf("tools[%d].input_schema.type = %#v, want object", i, got)
		}
	}
}

func assertCLIToolNames(t *testing.T, body map[string]any, want []string) {
	t.Helper()

	toolsValue, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %T(%#v), want []any", body["tools"], body["tools"])
	}
	if len(toolsValue) != len(want) {
		t.Fatalf("len(tools) = %d, want %d: %#v", len(toolsValue), len(want), toolsValue)
	}
	for i, wantName := range want {
		tool, ok := toolsValue[i].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d] = %T, want object", i, toolsValue[i])
		}
		if got := tool["type"]; got != "function" {
			t.Fatalf("tools[%d].type = %#v, want function", i, got)
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d].function = %T, want object", i, tool["function"])
		}
		if got := function["name"]; got != wantName {
			t.Fatalf("tools[%d].function.name = %#v, want %q", i, got, wantName)
		}
		if got := function["description"]; got == "" {
			t.Fatalf("tools[%d].function.description is empty", i)
		}
		parameters, ok := function["parameters"].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d].function.parameters = %T, want object", i, function["parameters"])
		}
		if got := parameters["type"]; got != "object" {
			t.Fatalf("tools[%d].function.parameters.type = %#v, want object", i, got)
		}
	}
}

func assertCLIResponsesToolNames(t *testing.T, body map[string]any, want []string) {
	t.Helper()

	toolsValue, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %T(%#v), want []any", body["tools"], body["tools"])
	}
	if len(toolsValue) != len(want) {
		t.Fatalf("len(tools) = %d, want %d: %#v", len(toolsValue), len(want), toolsValue)
	}
	for i, wantName := range want {
		tool, ok := toolsValue[i].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d] = %T, want object", i, toolsValue[i])
		}
		if got := tool["type"]; got != "function" {
			t.Fatalf("tools[%d].type = %#v, want function", i, got)
		}
		if got := tool["name"]; got != wantName {
			t.Fatalf("tools[%d].name = %#v, want %q", i, got, wantName)
		}
		if got := tool["description"]; got == "" {
			t.Fatalf("tools[%d].description is empty", i)
		}
		parameters, ok := tool["parameters"].(map[string]any)
		if !ok {
			t.Fatalf("tools[%d].parameters = %T, want object", i, tool["parameters"])
		}
		if got := parameters["type"]; got != "object" {
			t.Fatalf("tools[%d].parameters.type = %#v, want object", i, got)
		}
	}
}

func assertResponseFunctionCallInput(t *testing.T, input []any, index int, callID, name, arguments string) {
	t.Helper()

	if index >= len(input) {
		t.Fatalf("missing input %d in %#v", index, input)
	}
	item, ok := input[index].(map[string]any)
	if !ok {
		t.Fatalf("input[%d] = %T, want object", index, input[index])
	}
	if got := item["type"]; got != "function_call" {
		t.Fatalf("input[%d].type = %#v, want function_call", index, got)
	}
	if got := item["call_id"]; got != callID {
		t.Fatalf("input[%d].call_id = %#v, want %q", index, got, callID)
	}
	if got := item["name"]; got != name {
		t.Fatalf("input[%d].name = %#v, want %q", index, got, name)
	}
	if got := item["arguments"]; got != arguments {
		t.Fatalf("input[%d].arguments = %#v, want %q", index, got, arguments)
	}
}

func assertResponseFunctionCallOutput(t *testing.T, input []any, index int, callID, output string) {
	t.Helper()

	if index >= len(input) {
		t.Fatalf("missing input %d in %#v", index, input)
	}
	item, ok := input[index].(map[string]any)
	if !ok {
		t.Fatalf("input[%d] = %T, want object", index, input[index])
	}
	if got := item["type"]; got != "function_call_output" {
		t.Fatalf("input[%d].type = %#v, want function_call_output", index, got)
	}
	if got := item["call_id"]; got != callID {
		t.Fatalf("input[%d].call_id = %#v, want %q", index, got, callID)
	}
	if got := item["output"]; got != output {
		t.Fatalf("input[%d].output = %#v, want %q", index, got, output)
	}
}

func assertJSONNumber(t *testing.T, value any, want string) {
	t.Helper()

	number, ok := value.(json.Number)
	if !ok {
		t.Fatalf("value = %T(%#v), want json.Number", value, value)
	}
	if got := number.String(); got != want {
		t.Fatalf("number = %q, want %q", got, want)
	}
}

func assertCLIErrorContains(t *testing.T, got string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("stderr = %q, want contain %q", got, want)
		}
	}
}

func assertCLIHelpWithoutConfig(t *testing.T, args []string, wants ...string) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	code := RunWithGetwd(args, &stdout, &stderr, func() (string, error) {
		return "", errors.New("getwd should not be called")
	})
	if code != 0 {
		t.Fatalf("RunWithGetwd(%v) code = %d, stderr = %s", args, code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	if out == "" {
		t.Fatal("stdout is empty")
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want contain %q", out, want)
		}
	}
}

func assertCLIVerboseContains(t *testing.T, got string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose stderr = %q, want contain %q", got, want)
		}
	}
}

func verboseLogPath(t *testing.T, got string) string {
	t.Helper()

	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "log_path: ") {
			return strings.TrimPrefix(line, "log_path: ")
		}
	}
	t.Fatalf("verbose stderr = %q, want log_path line", got)
	return ""
}

func sessionLogPaths(t *testing.T, configDir string) []string {
	t.Helper()

	logRoot := filepath.Join(configDir, "logs")
	entries, err := os.ReadDir(logRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir(%q) error = %v", logRoot, err)
	}

	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(logRoot, entry.Name(), "sai.jsonl")
		if _, err := os.Stat(path); err == nil {
			paths = append(paths, path)
		} else if !os.IsNotExist(err) {
			t.Fatalf("Stat(%q) error = %v", path, err)
		}
	}
	sort.Strings(paths)
	return paths
}

func assertCLIToolStatus(t *testing.T, stdout, stderr, statusDetail string, hiddenValues ...string) {
	t.Helper()

	if strings.Contains(stdout, "tool:") || strings.Contains(stdout, statusDetail) {
		t.Fatalf("stdout contains tool status: %q", stdout)
	}
	if !strings.Contains(stderr, "tool: "+statusDetail+"\n") {
		t.Fatalf("stderr = %q, want tool status for %q", stderr, statusDetail)
	}
	for _, value := range hiddenValues {
		if strings.Contains(stderr, value) {
			t.Fatalf("stderr leaked tool detail %q: %s", value, stderr)
		}
	}
}

func assertCLIErrorOmits(t *testing.T, got string, values ...string) {
	t.Helper()

	for _, value := range values {
		if strings.Contains(got, value) {
			t.Fatalf("stderr leaked %q: %s", value, got)
		}
	}
}

func assertCLILogBaseFields(t *testing.T, records []map[string]any) {
	t.Helper()

	for _, record := range records {
		for _, key := range []string{"time", "level", "event", "provider", "model"} {
			if value, ok := record[key].(string); !ok || value == "" {
				t.Fatalf("log record %v missing string field %q", record, key)
			}
		}
		if record["provider"] != "fake" {
			t.Fatalf("log provider = %#v, want fake", record["provider"])
		}
		if record["model"] != "model-default" {
			t.Fatalf("log model = %#v, want model-default", record["model"])
		}
	}
}

func firstCLILogRecord(t *testing.T, records []map[string]any, event string) map[string]any {
	t.Helper()

	for _, record := range records {
		if record["event"] == event {
			return record
		}
	}
	t.Fatalf("missing log event %q in %#v", event, records)
	return nil
}

func hasCLILogRecord(records []map[string]any, event, key string, value any) bool {
	for _, record := range records {
		if record["event"] == event && record[key] == value {
			return true
		}
	}
	return false
}

func assertNoAdditionalCLIRunRequest(t *testing.T, requests <-chan capturedCLIRunRequest) {
	t.Helper()

	select {
	case request := <-requests:
		t.Fatalf("unexpected additional model request: path=%s body=%s", request.Path, request.RawBody)
	default:
	}
}

func assertCLIFileEventuallyContains(t *testing.T, path, want string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %q did not contain %q before timeout; last read error = %v, data = %q", path, want, err, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCLIMCPHelperProcess(t *testing.T) {
	if os.Getenv("SAI_CLI_MCP_HELPER_PROCESS") != "1" {
		return
	}

	code := runCLIFakeMCPServer()
	if code == 0 {
		if exitFile := os.Getenv("SAI_CLI_MCP_EXIT_FILE"); exitFile != "" {
			_ = os.WriteFile(exitFile, []byte("closed"), 0o644)
		}
	}
	os.Exit(code)
}

func runCLIFakeMCPServer() int {
	reader := bufio.NewReader(os.Stdin)

	request, err := readCLIMCPRequest(reader)
	if err != nil {
		return 2
	}
	if request.JSONRPC != "2.0" || request.Method != "initialize" || request.Params.ClientInfo.Name != "sai" {
		return 3
	}
	if err := writeCLIMCPMessage(os.Stdout, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result": map[string]any{
			"protocolVersion": request.Params.ProtocolVersion,
			"capabilities":    map[string]any{},
			"serverInfo": map[string]any{
				"name":    "cli-fake-mcp",
				"version": "test",
			},
		},
	}); err != nil {
		return 4
	}

	notification, err := readCLIMCPRequest(reader)
	if err != nil {
		return 5
	}
	if notification.JSONRPC != "2.0" || notification.Method != "notifications/initialized" {
		return 6
	}

	for {
		request, err := readCLIMCPRequest(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			return 7
		}
		if request.JSONRPC != "2.0" {
			return 8
		}

		switch request.Method {
		case "tools/list":
			if err := writeCLIMCPMessage(os.Stdout, map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{
							"name":        "search",
							"description": "search local fixture data",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"query": map[string]any{"type": "string"},
								},
							},
						},
						{
							"name":        "ignored",
							"description": "disabled fixture tool",
							"inputSchema": map[string]any{"type": "object"},
						},
					},
				},
			}); err != nil {
				return 9
			}
		case "tools/call":
			if request.Params.Name != "search" || request.Params.Arguments["query"] != "needle" {
				return 10
			}
			if err := writeCLIMCPMessage(os.Stdout, map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "mcp result one"},
						{"type": "text", "text": "mcp result two"},
					},
					"isError": false,
				},
			}); err != nil {
				return 11
			}
		default:
			return 12
		}
	}
}

type cliMCPRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
}

func readCLIMCPRequest(reader *bufio.Reader) (cliMCPRequest, error) {
	payload, err := reader.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return cliMCPRequest{}, io.EOF
		}
		return cliMCPRequest{}, err
	}

	var request cliMCPRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return cliMCPRequest{}, err
	}
	return request, nil
}

func writeCLIMCPMessage(w io.Writer, message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	_, err = w.Write(payload)
	return err
}
