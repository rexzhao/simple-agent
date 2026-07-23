package tools

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveShellCommandConfigForWindowsPrefersGitBash(t *testing.T) {
	programFiles := filepath.Join("C:\\", "Program Files")
	wantShell := filepath.Join(programFiles, "Git", "bin", "bash.exe")
	config, err := resolveShellCommandConfigFor(
		"windows",
		func(key string) string {
			if key == "ProgramFiles" {
				return programFiles
			}
			return ""
		},
		func(path string) bool { return path == wantShell },
		func() string {
			t.Fatal("findOnPath must not run when Git Bash is installed")
			return ""
		},
	)
	if err != nil {
		t.Fatalf("resolveShellCommandConfigFor() error = %v", err)
	}
	want := shellCommandConfig{shell: wantShell, args: []string{"-c"}}
	if !reflect.DeepEqual(config, want) {
		t.Fatalf("resolveShellCommandConfigFor() = %#v, want %#v", config, want)
	}
}

func TestResolveShellCommandConfigForWindowsUsesBashOnPath(t *testing.T) {
	config, err := resolveShellCommandConfigFor(
		"windows",
		func(string) string { return "" },
		func(string) bool { return false },
		func() string { return `C:\msys64\usr\bin\bash.exe` },
	)
	if err != nil {
		t.Fatalf("resolveShellCommandConfigFor() error = %v", err)
	}
	want := shellCommandConfig{shell: `C:\msys64\usr\bin\bash.exe`, args: []string{"-c"}}
	if !reflect.DeepEqual(config, want) {
		t.Fatalf("resolveShellCommandConfigFor() = %#v, want %#v", config, want)
	}
}

func TestResolveShellCommandConfigForWindowsUsesLegacyWSLStdin(t *testing.T) {
	config, err := resolveShellCommandConfigFor(
		"windows",
		func(string) string { return "" },
		func(string) bool { return false },
		func() string { return `C:\Windows\System32\bash.exe` },
	)
	if err != nil {
		t.Fatalf("resolveShellCommandConfigFor() error = %v", err)
	}
	want := shellCommandConfig{
		shell:            `C:\Windows\System32\bash.exe`,
		args:             []string{"-s"},
		commandFromStdin: true,
	}
	if !reflect.DeepEqual(config, want) {
		t.Fatalf("resolveShellCommandConfigFor() = %#v, want %#v", config, want)
	}
}

func TestResolveShellCommandConfigForWindowsReportsMissingBash(t *testing.T) {
	_, err := resolveShellCommandConfigFor(
		"windows",
		func(key string) string {
			if key == "ProgramFiles" {
				return `C:\Program Files`
			}
			return ""
		},
		func(string) bool { return false },
		func() string { return "" },
	)
	if err == nil {
		t.Fatal("resolveShellCommandConfigFor() error = nil, want missing Bash error")
	}
	for _, want := range []string{"no bash shell found", "Git for Windows", "bash.exe"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Fatalf("resolveShellCommandConfigFor() error = %q, want contain %q", err, want)
		}
	}
}

func TestResolveShellCommandConfigForUnixFallbackOrder(t *testing.T) {
	tests := []struct {
		name      string
		binExists bool
		pathBash  string
		want      shellCommandConfig
	}{
		{
			name:      "bin bash",
			binExists: true,
			pathBash:  "/usr/bin/bash",
			want:      shellCommandConfig{shell: "/bin/bash", args: []string{"-c"}},
		},
		{
			name:     "path bash",
			pathBash: "/usr/local/bin/bash",
			want:     shellCommandConfig{shell: "/usr/local/bin/bash", args: []string{"-c"}},
		},
		{
			name: "sh fallback",
			want: shellCommandConfig{shell: "sh", args: []string{"-c"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := resolveShellCommandConfigFor(
				"linux",
				func(string) string { return "" },
				func(path string) bool { return path == "/bin/bash" && tt.binExists },
				func() string { return tt.pathBash },
			)
			if err != nil {
				t.Fatalf("resolveShellCommandConfigFor() error = %v", err)
			}
			if !reflect.DeepEqual(config, tt.want) {
				t.Fatalf("resolveShellCommandConfigFor() = %#v, want %#v", config, tt.want)
			}
		})
	}
}

func TestIsLegacyWSLBashPath(t *testing.T) {
	for _, tt := range []struct {
		path string
		want bool
	}{
		{`C:\Windows\System32\bash.exe`, true},
		{`d:/Windows/Sysnative/bash.exe`, true},
		{`C:\Program Files\Git\bin\bash.exe`, false},
		{`C:\msys64\usr\bin\bash.exe`, false},
	} {
		if got := isLegacyWSLBashPath(tt.path); got != tt.want {
			t.Errorf("isLegacyWSLBashPath(%q) = %t, want %t", tt.path, got, tt.want)
		}
	}
}
