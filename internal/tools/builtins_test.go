package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestRegisterBuiltinsRegistersExpectedTools(t *testing.T) {
	registry := NewRegistry()
	if err := RegisterBuiltins(registry, t.TempDir()); err != nil {
		t.Fatalf("RegisterBuiltins() error = %v", err)
	}

	for _, name := range []string{BuiltinListFiles, BuiltinReadFile, BuiltinShell} {
		entry, ok := registry.Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) ok = false, want true", name)
		}
		if entry.Definition.Name != name {
			t.Fatalf("Lookup(%q) definition name = %q", name, entry.Definition.Name)
		}
		if entry.Definition.Description == "" {
			t.Fatalf("Lookup(%q) description is empty", name)
		}
		if got := entry.Definition.InputSchema["type"]; got != "object" {
			t.Fatalf("Lookup(%q) schema type = %#v, want object", name, got)
		}
		if entry.Executor == nil {
			t.Fatalf("Lookup(%q) executor = nil", name)
		}
	}
}

func TestRegisterBuiltinsRejectsBlankRootDir(t *testing.T) {
	err := RegisterBuiltins(NewRegistry(), " \t\n")
	if err == nil {
		t.Fatal("RegisterBuiltins() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "rootDir must not be blank") {
		t.Fatalf("RegisterBuiltins() error = %q", err)
	}
}

func TestListFilesOutputsSortedEntries(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "b.txt"), "b")
	writeTestFile(t, filepath.Join(root, "a.txt"), "a")
	mkdirTestDir(t, filepath.Join(root, "sub"))

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinListFiles, map[string]any{"path": "."})
	if err != nil {
		t.Fatalf("Execute(list_files) error = %v", err)
	}

	want := "a.txt\nb.txt\nsub/"
	if result.Name != BuiltinListFiles || result.Content != want || result.IsError {
		t.Fatalf("Execute(list_files) result = %#v, want name %q content %q IsError false", result, BuiltinListFiles, want)
	}
}

func TestReadFileOutputsFileContent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "hello\nworld\n")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinReadFile, map[string]any{"path": "notes.txt"})
	if err != nil {
		t.Fatalf("Execute(read_file) error = %v", err)
	}

	want := "hello\nworld\n"
	if result.Name != BuiltinReadFile || result.Content != want || result.IsError {
		t.Fatalf("Execute(read_file) result = %#v, want name %q content %q IsError false", result, BuiltinReadFile, want)
	}
}

func TestShellRunsInRootDir(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "marker.txt"), "from-root")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinShell, map[string]any{"command": readMarkerCommand()})
	if err != nil {
		t.Fatalf("Execute(shell) error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute(shell) IsError = true, content = %q", result.Content)
	}

	got := strings.TrimSpace(result.Content)
	if got != "from-root" {
		t.Fatalf("Execute(shell) content = %q, want from-root", result.Content)
	}
}

func TestShellNonZeroExitReturnsErrorResult(t *testing.T) {
	registry := registerBuiltinsForTest(t, t.TempDir())
	result, err := registry.Execute(context.Background(), BuiltinShell, map[string]any{"command": failingCommand()})
	if err != nil {
		t.Fatalf("Execute(shell) error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("Execute(shell) IsError = false, want true; result = %#v", result)
	}
	if !strings.Contains(result.Content, "fail") {
		t.Fatalf("Execute(shell) content = %q, want fail output", result.Content)
	}
}

func TestBuiltinArgumentErrorsAreReadable(t *testing.T) {
	registry := registerBuiltinsForTest(t, t.TempDir())

	tests := []struct {
		name    string
		tool    string
		args    map[string]any
		wantErr string
	}{
		{
			name:    "list files path type",
			tool:    BuiltinListFiles,
			args:    map[string]any{"path": 42},
			wantErr: "path must be a string",
		},
		{
			name:    "list files blank path",
			tool:    BuiltinListFiles,
			args:    map[string]any{"path": ""},
			wantErr: "path must not be blank",
		},
		{
			name:    "read file missing path",
			tool:    BuiltinReadFile,
			args:    map[string]any{},
			wantErr: "path is required",
		},
		{
			name:    "read file path type",
			tool:    BuiltinReadFile,
			args:    map[string]any{"path": false},
			wantErr: "path must be a string",
		},
		{
			name:    "shell missing command",
			tool:    BuiltinShell,
			args:    map[string]any{},
			wantErr: "command is required",
		},
		{
			name:    "shell command type",
			tool:    BuiltinShell,
			args:    map[string]any{"command": []string{"echo"}},
			wantErr: "command must be a string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registry.Execute(context.Background(), tt.tool, tt.args)
			if err == nil {
				t.Fatal("Execute() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Execute() error = %q, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestReadAndListRejectPathsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "secret.txt"), "secret")

	registry := registerBuiltinsForTest(t, root)
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{
			name: "list outside",
			tool: BuiltinListFiles,
			args: map[string]any{"path": ".."},
		},
		{
			name: "read outside absolute",
			tool: BuiltinReadFile,
			args: map[string]any{"path": filepath.Join(outside, "secret.txt")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registry.Execute(context.Background(), tt.tool, tt.args)
			if err == nil {
				t.Fatal("Execute() error = nil, want error")
			}
			if !strings.Contains(err.Error(), "outside rootDir") {
				t.Fatalf("Execute() error = %q, want outside rootDir", err)
			}
		})
	}
}

func TestReadFileRejectsSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	writeTestFile(t, outsideFile, "secret")
	createFileSymlinkOrSkip(t, outsideFile, filepath.Join(root, "secret-link.txt"))

	registry := registerBuiltinsForTest(t, root)
	_, err := registry.Execute(context.Background(), BuiltinReadFile, map[string]any{"path": "secret-link.txt"})
	if err == nil {
		t.Fatal("Execute(read_file) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "outside rootDir") {
		t.Fatalf("Execute(read_file) error = %q, want outside rootDir", err)
	}
}

func TestListFilesRejectsSymlinkedDirectoryOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideDir := filepath.Join(outside, "secret-dir")
	mkdirTestDir(t, outsideDir)
	writeTestFile(t, filepath.Join(outsideDir, "secret.txt"), "secret")
	createDirSymlinkOrSkip(t, outsideDir, filepath.Join(root, "secret-dir-link"))

	registry := registerBuiltinsForTest(t, root)
	_, err := registry.Execute(context.Background(), BuiltinListFiles, map[string]any{"path": "secret-dir-link"})
	if err == nil {
		t.Fatal("Execute(list_files) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "outside rootDir") {
		t.Fatalf("Execute(list_files) error = %q, want outside rootDir", err)
	}
}

func TestListFilesDefaultsToRoot(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "root.txt"), "root")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinListFiles, nil)
	if err != nil {
		t.Fatalf("Execute(list_files) error = %v", err)
	}
	if result.Content != "root.txt" {
		t.Fatalf("Execute(list_files) content = %q, want root.txt", result.Content)
	}
}

func TestBuiltinDefinitionsHaveExpectedSchemas(t *testing.T) {
	registry := registerBuiltinsForTest(t, t.TempDir())

	tests := []struct {
		name string
		want map[string]any
	}{
		{
			name: BuiltinReadFile,
			want: map[string]any{
				"type":     "object",
				"required": []any{"path"},
			},
		},
		{
			name: BuiltinShell,
			want: map[string]any{
				"type":     "object",
				"required": []any{"command"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := registry.Lookup(tt.name)
			if !ok {
				t.Fatalf("Lookup(%q) ok = false, want true", tt.name)
			}
			for key, want := range tt.want {
				if got := entry.Definition.InputSchema[key]; !reflect.DeepEqual(got, want) {
					t.Fatalf("InputSchema[%q] = %#v, want %#v", key, got, want)
				}
			}
		})
	}
}

func registerBuiltinsForTest(t *testing.T, root string) *Registry {
	t.Helper()

	registry := NewRegistry()
	if err := RegisterBuiltins(registry, root); err != nil {
		t.Fatalf("RegisterBuiltins() error = %v", err)
	}
	return registry
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func mkdirTestDir(t *testing.T, path string) {
	t.Helper()

	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir(%q) error = %v", path, err)
	}
}

func createFileSymlinkOrSkip(t *testing.T, target string, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create file symlink: %v", err)
	}
}

func createDirSymlinkOrSkip(t *testing.T, target string, link string) {
	t.Helper()

	symlinkErr := os.Symlink(target, link)
	if symlinkErr == nil {
		return
	}
	if runtime.GOOS != "windows" {
		t.Skipf("cannot create directory symlink: %v", symlinkErr)
	}

	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot create directory symlink or junction: symlink failed: %v; junction failed: %v: %s", symlinkErr, err, output)
	}
}

func readMarkerCommand() string {
	if runtime.GOOS == "windows" {
		return "Get-Content marker.txt"
	}
	return "cat marker.txt"
}

func failingCommand() string {
	if runtime.GOOS == "windows" {
		return "Write-Output fail; exit 7"
	}
	return "printf fail; exit 7"
}
