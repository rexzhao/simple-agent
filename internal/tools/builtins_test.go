package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestRegisterBuiltinsRegistersExpectedTools(t *testing.T) {
	registry := NewRegistry()
	if err := RegisterBuiltins(registry, t.TempDir(), false); err != nil {
		t.Fatalf("RegisterBuiltins() error = %v", err)
	}

	for _, name := range []string{BuiltinListFiles, BuiltinReadFile, BuiltinGlobFiles, BuiltinGrepFiles, BuiltinWriteFile, BuiltinEditFile, BuiltinApplyPatch, BuiltinShell} {
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

func TestShellDefinitionDescribesBashSyntax(t *testing.T) {
	definition := shellDefinition()
	for _, want := range []string{"Bash", "not PowerShell", "Git Bash"} {
		if !strings.Contains(definition.Description, want) {
			t.Fatalf("shell description = %q, want contain %q", definition.Description, want)
		}
	}
	properties := definition.InputSchema["properties"].(map[string]any)
	command := properties["command"].(map[string]any)
	if got := command["description"]; got != "Bash command to run." {
		t.Fatalf("shell command description = %#v, want Bash command description", got)
	}
}

func TestRegisterBuiltinsRejectsBlankRootDir(t *testing.T) {
	err := RegisterBuiltins(NewRegistry(), " \t\n", false)
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

func TestReadFileAcceptsConfiguredExternalReadRootOnly(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	skillDir := filepath.Join(parent, "skills")
	otherDir := filepath.Join(parent, "other")
	mkdirTestDir(t, workspace)
	mkdirTestDir(t, skillDir)
	mkdirTestDir(t, filepath.Join(skillDir, "review"))
	mkdirTestDir(t, otherDir)
	writeTestFile(t, filepath.Join(skillDir, "review", "SKILL.md"), "skill instructions")
	writeTestFile(t, filepath.Join(otherDir, "secret.txt"), "secret")

	registry := NewRegistry()
	if err := RegisterBuiltinsWithReadRoots(registry, workspace, false, []string{skillDir}); err != nil {
		t.Fatalf("RegisterBuiltinsWithReadRoots() error = %v", err)
	}
	result, err := registry.Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path": filepath.Join(skillDir, "review", "SKILL.md"),
	})
	if err != nil {
		t.Fatalf("Execute(read_file external skill) error = %v", err)
	}
	if result.Content != "skill instructions" {
		t.Fatalf("Execute(read_file external skill) content = %q", result.Content)
	}

	if _, err := registry.Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path": filepath.Join(otherDir, "secret.txt"),
	}); err == nil {
		t.Fatal("Execute(read_file unrelated external file) error = nil, want error")
	}
	if _, err := registry.Execute(context.Background(), BuiltinWriteFile, map[string]any{
		"path":    filepath.Join(skillDir, "review", "created.txt"),
		"content": "not allowed",
	}); err == nil {
		t.Fatal("Execute(write_file external skill) error = nil, want error")
	}
}

func TestReadFileLineRanges(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "one\ntwo\nthree\nfour\n")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path":       "notes.txt",
		"start_line": 2,
		"line_count": 2,
	})
	if err != nil {
		t.Fatalf("Execute(read_file range) error = %v", err)
	}
	for _, want := range []string{"path=notes.txt", "start_line=2", "lines_returned=2", "max_bytes=51200", "truncated=false", "\n\ntwo\nthree\n"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Execute(read_file range) content = %q, want contain %q", result.Content, want)
		}
	}

	result, err = registry.Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path":       "notes.txt",
		"start_line": 3,
	})
	if err != nil {
		t.Fatalf("Execute(read_file start_line) error = %v", err)
	}
	for _, want := range []string{"path=notes.txt", "start_line=3", "lines_returned=2", "max_bytes=51200", "truncated=false", "\n\nthree\nfour\n"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Execute(read_file start_line) content = %q, want contain %q", result.Content, want)
		}
	}

	result, err = registry.Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path":       "notes.txt",
		"line_count": 1,
	})
	if err != nil {
		t.Fatalf("Execute(read_file line_count) error = %v", err)
	}
	for _, want := range []string{"path=notes.txt", "start_line=1", "lines_returned=1", "max_bytes=51200", "truncated=false", "\n\none\n"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Execute(read_file line_count) content = %q, want contain %q", result.Content, want)
		}
	}
}

func TestReadFileMaxBytesTruncatesWithNextStep(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "one\ntwo\nthree\n")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path":      "notes.txt",
		"max_bytes": 8,
	})
	if err != nil {
		t.Fatalf("Execute(read_file max_bytes) error = %v", err)
	}
	for _, want := range []string{"path=notes.txt", "start_line=1", "lines_returned=2", "truncated=true", "max_bytes=8", "next_start_line=3", "next_step=Continue reading with start_line=3.", "\n\none\ntwo\n"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Execute(read_file max_bytes) content = %q, want contain %q", result.Content, want)
		}
	}
}

func TestReadFileLongFirstLineMarksLineTruncated(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "long.txt"), "abcdefghij\nnext\n")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path":       "long.txt",
		"start_line": 1,
		"max_bytes":  4,
	})
	if err != nil {
		t.Fatalf("Execute(read_file long line) error = %v", err)
	}
	for _, want := range []string{"path=long.txt", "start_line=1", "lines_returned=1", "truncated=true", "line_truncated=true", "retry_from_line=1", "\n\nabcd"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Execute(read_file long line) content = %q, want contain %q", result.Content, want)
		}
	}
}

func TestReadFileRejectsBinaryContent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "binary.bin"), "abc\x00def\n")

	registry := registerBuiltinsForTest(t, root)
	tests := []struct {
		name string
		args map[string]any
	}{
		{
			name: "default",
			args: map[string]any{"path": "binary.bin"},
		},
		{
			name: "ranged",
			args: map[string]any{"path": "binary.bin", "start_line": 1, "line_count": 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registry.Execute(context.Background(), BuiltinReadFile, tt.args)
			if err == nil {
				t.Fatal("Execute(read_file binary) error = nil, want error")
			}
			if !strings.Contains(err.Error(), `read file "binary.bin": file appears to be binary`) {
				t.Fatalf("Execute(read_file binary) error = %q, want binary file error", err)
			}
		})
	}
}

func TestGlobFilesOutputsSortedWorkspaceRelativePaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "z.txt"), "z")
	writeTestFile(t, filepath.Join(root, "a.txt"), "a")
	mkdirTestDir(t, filepath.Join(root, "nested"))
	writeTestFile(t, filepath.Join(root, "nested", "b.txt"), "b")
	writeTestFile(t, filepath.Join(root, "nested", "ignore.md"), "ignore")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinGlobFiles, map[string]any{
		"pattern": "**/*.txt",
	})
	if err != nil {
		t.Fatalf("Execute(glob_files) error = %v", err)
	}
	if got, want := result.Content, "a.txt\nnested/b.txt\nz.txt"; got != want {
		t.Fatalf("Execute(glob_files) content = %q, want %q", got, want)
	}
}

func TestGlobFilesHiddenHandlingAndIncludeDirs(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "visible.txt"), "visible")
	writeTestFile(t, filepath.Join(root, ".hidden.txt"), "hidden")
	mkdirTestDir(t, filepath.Join(root, "dir"))
	mkdirTestDir(t, filepath.Join(root, ".hidden-dir"))
	writeTestFile(t, filepath.Join(root, ".hidden-dir", "secret.txt"), "secret")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinGlobFiles, map[string]any{
		"pattern":      "**",
		"include_dirs": true,
	})
	if err != nil {
		t.Fatalf("Execute(glob_files hidden default) error = %v", err)
	}
	if strings.Contains(result.Content, ".hidden") {
		t.Fatalf("Execute(glob_files hidden default) content = %q, want hidden paths omitted", result.Content)
	}
	for _, want := range []string{"dir", "visible.txt"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Execute(glob_files hidden default) content = %q, want contain %q", result.Content, want)
		}
	}

	result, err = registry.Execute(context.Background(), BuiltinGlobFiles, map[string]any{
		"pattern":        "**/*.txt",
		"include_hidden": true,
	})
	if err != nil {
		t.Fatalf("Execute(glob_files include_hidden) error = %v", err)
	}
	for _, want := range []string{".hidden-dir/secret.txt", ".hidden.txt", "visible.txt"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Execute(glob_files include_hidden) content = %q, want contain %q", result.Content, want)
		}
	}
}

func TestGlobFilesMaxResultsTruncates(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeTestFile(t, filepath.Join(root, name), name)
	}

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinGlobFiles, map[string]any{
		"pattern":     "*.txt",
		"max_results": 2,
	})
	if err != nil {
		t.Fatalf("Execute(glob_files max_results) error = %v", err)
	}
	for _, want := range []string{"truncated=true", "max_results=2", "returned_results=2", "\n\na.txt\nb.txt"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Execute(glob_files max_results) content = %q, want contain %q", result.Content, want)
		}
	}
	if strings.Contains(result.Content, "c.txt") {
		t.Fatalf("Execute(glob_files max_results) content = %q, want c.txt omitted", result.Content)
	}
}

func TestGlobAndGrepRespectGitIgnore(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".gitignore"), "ignored/\n*.secret\n!keep.secret\n")
	writeTestFile(t, filepath.Join(root, "visible.txt"), "needle\n")
	writeTestFile(t, filepath.Join(root, "hidden.secret"), "needle\n")
	writeTestFile(t, filepath.Join(root, "keep.secret"), "needle\n")
	mkdirTestDir(t, filepath.Join(root, "ignored"))
	writeTestFile(t, filepath.Join(root, "ignored", "hidden.txt"), "needle\n")
	mkdirTestDir(t, filepath.Join(root, "nested"))
	writeTestFile(t, filepath.Join(root, "nested", ".gitignore"), "generated/\n")
	writeTestFile(t, filepath.Join(root, "nested", "visible.txt"), "needle\n")
	mkdirTestDir(t, filepath.Join(root, "nested", "generated"))
	writeTestFile(t, filepath.Join(root, "nested", "generated", "hidden.txt"), "needle\n")

	registry := registerBuiltinsForTest(t, root)
	globResult, err := registry.Execute(context.Background(), BuiltinGlobFiles, map[string]any{"pattern": "**"})
	if err != nil {
		t.Fatalf("Execute(glob_files) error = %v", err)
	}
	if got, want := globResult.Content, "keep.secret\nnested/visible.txt\nvisible.txt"; got != want {
		t.Fatalf("Execute(glob_files) content = %q, want %q", got, want)
	}

	grepResult, err := registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{"query": "needle"})
	if err != nil {
		t.Fatalf("Execute(grep_files) error = %v", err)
	}
	if got, want := grepResult.Content, "keep.secret:1:needle\nnested/visible.txt:1:needle\nvisible.txt:1:needle"; got != want {
		t.Fatalf("Execute(grep_files) content = %q, want %q", got, want)
	}
}

func TestGlobFilesRejectsOutsideSearchPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "secret.txt"), "secret")

	registry := registerBuiltinsForTest(t, root)
	_, err := registry.Execute(context.Background(), BuiltinGlobFiles, map[string]any{
		"path":    outside,
		"pattern": "*.txt",
	})
	if err == nil {
		t.Fatal("Execute(glob_files outside) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "outside rootDir") {
		t.Fatalf("Execute(glob_files outside) error = %q, want outside rootDir", err)
	}
}

func TestGrepFilesRejectsOutsideSearchPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "secret.txt"), "secret")

	registry := registerBuiltinsForTest(t, root)
	_, err := registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"path":  outside,
		"query": "secret",
	})
	if err == nil {
		t.Fatal("Execute(grep_files outside) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "outside rootDir") {
		t.Fatalf("Execute(grep_files outside) error = %q, want outside rootDir", err)
	}
}

func TestGrepFilesRegexDefaultLiteralAndCaseSensitivity(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "Needle here\nneedle there\nvalue[1]\nvalue1\n")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"query": "needle",
	})
	if err != nil {
		t.Fatalf("Execute(grep_files regex default) error = %v", err)
	}
	if got, want := result.Content, "notes.txt:2:needle there"; got != want {
		t.Fatalf("Execute(grep_files regex default) content = %q, want %q", got, want)
	}

	result, err = registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"query":          "needle",
		"case_sensitive": false,
	})
	if err != nil {
		t.Fatalf("Execute(grep_files case insensitive) error = %v", err)
	}
	if got, want := result.Content, "notes.txt:1:Needle here\nnotes.txt:2:needle there"; got != want {
		t.Fatalf("Execute(grep_files case insensitive) content = %q, want %q", got, want)
	}

	result, err = registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"query": `value[1]`,
	})
	if err != nil {
		t.Fatalf("Execute(grep_files regex metacharacter) error = %v", err)
	}
	if got, want := result.Content, "notes.txt:4:value1"; got != want {
		t.Fatalf("Execute(grep_files regex metacharacter) content = %q, want %q", got, want)
	}

	result, err = registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"query":   `value[1]`,
		"literal": true,
	})
	if err != nil {
		t.Fatalf("Execute(grep_files literal) error = %v", err)
	}
	if got, want := result.Content, "notes.txt:3:value[1]"; got != want {
		t.Fatalf("Execute(grep_files literal) content = %q, want %q", got, want)
	}
}

func TestGrepFilesSearchesSingleFilePath(t *testing.T) {
	root := t.TempDir()
	mkdirTestDir(t, filepath.Join(root, "nested"))
	writeTestFile(t, filepath.Join(root, "other.txt"), "needle outside target\n")
	writeTestFile(t, filepath.Join(root, "nested", "target.txt"), "needle inside target\n")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"path":  "nested/target.txt",
		"query": "needle",
	})
	if err != nil {
		t.Fatalf("Execute(grep_files file) error = %v", err)
	}
	if got, want := result.Content, "nested/target.txt:1:needle inside target"; got != want {
		t.Fatalf("Execute(grep_files file) content = %q, want %q", got, want)
	}

	result, err = registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"path":    "nested/target.txt",
		"query":   "needle",
		"exclude": []any{"nested/target.txt"},
	})
	if err != nil {
		t.Fatalf("Execute(grep_files excluded file) error = %v", err)
	}
	if result.Content != "" {
		t.Fatalf("Execute(grep_files excluded file) content = %q, want empty", result.Content)
	}
}

func TestGrepFilesRegexIncludeExcludeAndContext(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), "before\nabc123\nafter\n")
	writeTestFile(t, filepath.Join(root, "skip.txt"), "abc999\n")
	writeTestFile(t, filepath.Join(root, "b.log"), "abc456\n")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"query":          `abc\d+`,
		"case_sensitive": true,
		"include":        []any{"*.txt"},
		"exclude":        []any{"skip.txt"},
		"context_lines":  1,
	})
	if err != nil {
		t.Fatalf("Execute(grep_files regex) error = %v", err)
	}
	if got, want := result.Content, "a.txt:1:before\na.txt:2:abc123\na.txt:3:after"; got != want {
		t.Fatalf("Execute(grep_files regex) content = %q, want %q", got, want)
	}
}

func TestGrepFilesResultAndSnippetTruncation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "needle abcdefghij\nneedle second\n")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"query":             "needle",
		"max_results":       1,
		"max_snippet_bytes": 10,
	})
	if err != nil {
		t.Fatalf("Execute(grep_files truncation) error = %v", err)
	}
	for _, want := range []string{
		"truncated=true",
		"max_results=1",
		"returned_results=1",
		"snippet_truncated=true",
		"max_snippet_bytes=10",
		"\n\nnotes.txt:1:needle abc",
	} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Execute(grep_files truncation) content = %q, want contain %q", result.Content, want)
		}
	}
	if strings.Contains(result.Content, "second") {
		t.Fatalf("Execute(grep_files truncation) content = %q, want second result omitted", result.Content)
	}
}

func TestWriteFileOverwritesAndCreatesParentDirs(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "old")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinWriteFile, map[string]any{
		"path":    "notes.txt",
		"content": "secret-body",
	})
	if err != nil {
		t.Fatalf("Execute(write_file overwrite) error = %v", err)
	}
	if result.Name != BuiltinWriteFile || result.Content != "wrote notes.txt (11 bytes)" || result.IsError {
		t.Fatalf("Execute(write_file overwrite) result = %#v", result)
	}
	if strings.Contains(result.Content, "secret-body") {
		t.Fatalf("Execute(write_file overwrite) leaked content in result: %q", result.Content)
	}
	if got := readTestFile(t, filepath.Join(root, "notes.txt")); got != "secret-body" {
		t.Fatalf("written content = %q, want secret-body", got)
	}

	result, err = registry.Execute(context.Background(), BuiltinWriteFile, map[string]any{
		"path":    filepath.Join("nested", "dir", "created.txt"),
		"content": "created",
	})
	if err != nil {
		t.Fatalf("Execute(write_file create parents) error = %v", err)
	}
	if result.Name != BuiltinWriteFile || result.Content != "wrote "+filepath.Join("nested", "dir", "created.txt")+" (7 bytes)" || result.IsError {
		t.Fatalf("Execute(write_file create parents) result = %#v", result)
	}
	if got := readTestFile(t, filepath.Join(root, "nested", "dir", "created.txt")); got != "created" {
		t.Fatalf("created content = %q, want created", got)
	}
}

func TestEditFileReplacesSingleMatch(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "alpha SECRET_OLD omega")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinEditFile, map[string]any{
		"path": "notes.txt",
		"old":  "SECRET_OLD",
		"new":  "SECRET_NEW",
	})
	if err != nil {
		t.Fatalf("Execute(edit_file) error = %v", err)
	}
	if result.Name != BuiltinEditFile || result.Content != "edited notes.txt (1 replacement)" || result.IsError {
		t.Fatalf("Execute(edit_file) result = %#v", result)
	}
	if strings.Contains(result.Content, "SECRET_OLD") || strings.Contains(result.Content, "SECRET_NEW") {
		t.Fatalf("Execute(edit_file) leaked edit text in result: %q", result.Content)
	}
	if got := readTestFile(t, filepath.Join(root, "notes.txt")); got != "alpha SECRET_NEW omega" {
		t.Fatalf("edited content = %q, want replacement", got)
	}
}

func TestEditFileNormalizesLineEndingsAndPreservesBOM(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "\uFEFFfirst\r\nold\r\nlast\r\n")

	registry := registerBuiltinsForTest(t, root)
	result, err := registry.Execute(context.Background(), BuiltinEditFile, map[string]any{
		"path": "notes.txt",
		"old":  "first\nold\n",
		"new":  "first\nnew\nmiddle\n",
	})
	if err != nil {
		t.Fatalf("Execute(edit_file) error = %v", err)
	}
	if result.Name != BuiltinEditFile || result.IsError {
		t.Fatalf("Execute(edit_file) result = %#v", result)
	}
	if got := readTestFile(t, filepath.Join(root, "notes.txt")); got != "\uFEFFfirst\r\nnew\r\nmiddle\r\nlast\r\n" {
		t.Fatalf("edited content = %q, want CRLF content with BOM", got)
	}
}

func TestEditFileErrorsForNotFoundAndMultipleMatches(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "not-found.txt"), "alpha beta")
	writeTestFile(t, filepath.Join(root, "multiple.txt"), "old middle old")

	registry := registerBuiltinsForTest(t, root)
	tests := []struct {
		name    string
		path    string
		old     string
		wantErr string
	}{
		{
			name:    "not found",
			path:    "not-found.txt",
			old:     "missing",
			wantErr: "old text not found",
		},
		{
			name:    "multiple matches",
			path:    "multiple.txt",
			old:     "old",
			wantErr: "old text matched 2 times",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registry.Execute(context.Background(), BuiltinEditFile, map[string]any{
				"path": tt.path,
				"old":  tt.old,
				"new":  "new",
			})
			if err == nil {
				t.Fatal("Execute(edit_file) error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Execute(edit_file) error = %q, want %q", err, tt.wantErr)
			}
		})
	}
	if got := readTestFile(t, filepath.Join(root, "multiple.txt")); got != "old middle old" {
		t.Fatalf("multiple match file changed to %q", got)
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

func TestShellPreservesANSIOutput(t *testing.T) {
	registry := registerBuiltinsForTest(t, t.TempDir())
	result, err := registry.Execute(context.Background(), BuiltinShell, map[string]any{
		"command": "printf '\\033[31mstdout\\033[0m\\n'; printf '\\033[32mstderr\\033[0m\\n' >&2",
	})
	if err != nil {
		t.Fatalf("Execute(shell) error = %v", err)
	}
	want := "\x1b[31mstdout\x1b[0m\n\x1b[32mstderr\x1b[0m\n"
	if result.IsError || result.Content != want {
		t.Fatalf("Execute(shell) result = %#v, want raw ANSI content %q", result, want)
	}
}

func TestShellRunsBashSyntaxWhenBashIsAvailable(t *testing.T) {
	config, err := resolveShellCommandConfig()
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Bash is unavailable on this Windows test host: %v", err)
		}
		t.Fatalf("resolveShellCommandConfig() error = %v", err)
	}
	if config.shell == "sh" {
		t.Skip("Bash is unavailable; pi-compatible fallback is sh")
	}

	registry := registerBuiltinsForTest(t, t.TempDir())
	result, err := registry.Execute(context.Background(), BuiltinShell, map[string]any{
		"command": "[[ -n \"${BASH_VERSION:-}\" ]] && printf bash",
	})
	if err != nil {
		t.Fatalf("Execute(shell) error = %v", err)
	}
	if result.IsError || result.Content != "bash" {
		t.Fatalf("Execute(shell) result = %#v, want Bash-specific syntax to succeed", result)
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

func TestShellTimeoutMSReturnsErrorResult(t *testing.T) {
	registry := registerBuiltinsForTest(t, t.TempDir())
	start := time.Now()
	result, err := registry.Execute(context.Background(), BuiltinShell, map[string]any{
		"command":    sleepCommand(),
		"timeout_ms": 100,
	})
	if err != nil {
		t.Fatalf("Execute(shell timeout_ms) error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Execute(shell timeout_ms) elapsed = %v, want under 3s", elapsed)
	}
	assertShellContextErrorResult(t, result, context.DeadlineExceeded.Error())
	if !strings.Contains(strings.ToLower(result.Content), "timed out") {
		t.Fatalf("Execute(shell timeout_ms) content = %q, want timed out", result.Content)
	}
}

func TestShellMaxOutputBytesTruncatesOutput(t *testing.T) {
	registry := registerBuiltinsForTest(t, t.TempDir())
	result, err := registry.Execute(context.Background(), BuiltinShell, map[string]any{
		"command":          printTextCommand("abcdef"),
		"max_output_bytes": 3,
	})
	if err != nil {
		t.Fatalf("Execute(shell max_output_bytes) error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute(shell max_output_bytes) IsError = true, content = %q", result.Content)
	}
	for _, want := range []string{"truncated=true", "max_output_bytes=3", "\n\nabc"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("Execute(shell max_output_bytes) content = %q, want contain %q", result.Content, want)
		}
	}
	if strings.Contains(result.Content, "def") {
		t.Fatalf("Execute(shell max_output_bytes) content = %q, want output capped", result.Content)
	}
}

func TestLimitedOutputCapsRetainedMemoryWhileDraining(t *testing.T) {
	output := newLimitedOutput(3)
	for _, chunk := range [][]byte{
		[]byte("ab"),
		[]byte("cdef"),
		[]byte(strings.Repeat("x", 1<<20)),
	} {
		written, err := output.Write(chunk)
		if err != nil {
			t.Fatalf("limitedOutput.Write() error = %v", err)
		}
		if written != len(chunk) {
			t.Fatalf("limitedOutput.Write() = %d, want %d", written, len(chunk))
		}
	}
	if got := output.String(); got != "abc" {
		t.Fatalf("limitedOutput.String() = %q, want abc", got)
	}
	if !output.Truncated() {
		t.Fatal("limitedOutput.Truncated() = false, want true")
	}
}

func TestShellContextCancelReturnsErrorResult(t *testing.T) {
	root := t.TempDir()
	registry := registerBuiltinsForTest(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer cleanupChildProcessFromPIDFile(t, filepath.Join(root, "child.pid"))

	type executeResult struct {
		result model.ToolResult
		err    error
	}
	resultCh := make(chan executeResult, 1)
	go func() {
		result, err := registry.Execute(ctx, BuiltinShell, map[string]any{"command": childProcessTreeCommand()})
		resultCh <- executeResult{result: result, err: err}
	}()

	waitForFile(t, filepath.Join(root, "started.txt"), 5*time.Second)
	waitForFileSize(t, filepath.Join(root, "child.txt"), 1, 5*time.Second)
	cancelAt := time.Now()
	cancel()

	select {
	case got := <-resultCh:
		if elapsed := time.Since(cancelAt); elapsed > 5*time.Second {
			t.Fatalf("Execute(shell) returned after %v, want under 5s", elapsed)
		}
		if got.err != nil {
			t.Fatalf("Execute(shell) error = %v", got.err)
		}
		assertShellContextErrorResult(t, got.result, context.Canceled.Error())
		assertFileStopsGrowing(t, filepath.Join(root, "child.txt"))
	case <-time.After(5 * time.Second):
		t.Fatal("Execute(shell) did not return promptly after context cancellation")
	}
}

func TestShellContextDeadlineReturnsErrorResult(t *testing.T) {
	root := t.TempDir()
	registry := registerBuiltinsForTest(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer cleanupChildProcessFromPIDFile(t, filepath.Join(root, "child.pid"))

	resultCh := make(chan struct {
		result model.ToolResult
		err    error
	}, 1)
	go func() {
		result, err := registry.Execute(ctx, BuiltinShell, map[string]any{"command": childProcessTreeCommand()})
		resultCh <- struct {
			result model.ToolResult
			err    error
		}{result: result, err: err}
	}()

	waitForFile(t, filepath.Join(root, "started.txt"), 2*time.Second)
	waitForFileSize(t, filepath.Join(root, "child.txt"), 1, 2*time.Second)

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("Execute(shell) error = %v", got.err)
		}
		assertShellContextErrorResult(t, got.result, context.DeadlineExceeded.Error())
		assertFileStopsGrowing(t, filepath.Join(root, "child.txt"))
	case <-time.After(5 * time.Second):
		t.Fatal("Execute(shell) did not return promptly after context deadline")
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
			name:    "read file start line positive",
			tool:    BuiltinReadFile,
			args:    map[string]any{"path": "notes.txt", "start_line": 0},
			wantErr: "start_line must be a positive integer",
		},
		{
			name:    "read file line count positive",
			tool:    BuiltinReadFile,
			args:    map[string]any{"path": "notes.txt", "line_count": 0},
			wantErr: "line_count must be a positive integer",
		},
		{
			name:    "read file max bytes positive",
			tool:    BuiltinReadFile,
			args:    map[string]any{"path": "notes.txt", "max_bytes": 0},
			wantErr: "max_bytes must be a positive integer",
		},
		{
			name:    "glob files missing pattern",
			tool:    BuiltinGlobFiles,
			args:    map[string]any{},
			wantErr: "pattern is required",
		},
		{
			name:    "glob files max results positive",
			tool:    BuiltinGlobFiles,
			args:    map[string]any{"pattern": "*", "max_results": 0},
			wantErr: "max_results must be a positive integer",
		},
		{
			name:    "grep files missing query",
			tool:    BuiltinGrepFiles,
			args:    map[string]any{},
			wantErr: "query is required",
		},
		{
			name:    "grep files literal type",
			tool:    BuiltinGrepFiles,
			args:    map[string]any{"query": "needle", "literal": "false"},
			wantErr: "literal must be a boolean",
		},
		{
			name:    "grep files invalid regex",
			tool:    BuiltinGrepFiles,
			args:    map[string]any{"query": "["},
			wantErr: "invalid regex query",
		},
		{
			name:    "grep files context non-negative",
			tool:    BuiltinGrepFiles,
			args:    map[string]any{"query": "needle", "context_lines": -1},
			wantErr: "context_lines must be a non-negative integer",
		},
		{
			name:    "grep files snippet bytes positive",
			tool:    BuiltinGrepFiles,
			args:    map[string]any{"query": "needle", "max_snippet_bytes": 0},
			wantErr: "max_snippet_bytes must be a positive integer",
		},
		{
			name:    "write file missing content",
			tool:    BuiltinWriteFile,
			args:    map[string]any{"path": "notes.txt"},
			wantErr: "content is required",
		},
		{
			name:    "write file content type",
			tool:    BuiltinWriteFile,
			args:    map[string]any{"path": "notes.txt", "content": 42},
			wantErr: "content must be a string",
		},
		{
			name:    "edit file missing old",
			tool:    BuiltinEditFile,
			args:    map[string]any{"path": "notes.txt", "new": "new"},
			wantErr: "old is required",
		},
		{
			name:    "edit file empty old",
			tool:    BuiltinEditFile,
			args:    map[string]any{"path": "notes.txt", "old": "", "new": "new"},
			wantErr: "old must not be empty",
		},
		{
			name:    "edit file new type",
			tool:    BuiltinEditFile,
			args:    map[string]any{"path": "notes.txt", "old": "old", "new": true},
			wantErr: "new must be a string",
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
		{
			name:    "shell timeout positive",
			tool:    BuiltinShell,
			args:    map[string]any{"command": "echo ok", "timeout_ms": 0},
			wantErr: "timeout_ms must be a positive integer",
		},
		{
			name:    "shell max output positive",
			tool:    BuiltinShell,
			args:    map[string]any{"command": "echo ok", "max_output_bytes": 0},
			wantErr: "max_output_bytes must be a positive integer",
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

func TestWriteAndEditRejectPathErrors(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "old")
	mkdirTestDir(t, filepath.Join(root, "dir"))
	writeTestFile(t, filepath.Join(outside, "secret.txt"), "secret")

	registry := registerBuiltinsForTest(t, root)
	tests := []struct {
		name    string
		tool    string
		args    map[string]any
		wantErr string
	}{
		{
			name: "write outside root",
			tool: BuiltinWriteFile,
			args: map[string]any{
				"path":    filepath.Join(outside, "new-secret.txt"),
				"content": "secret",
			},
			wantErr: "outside rootDir",
		},
		{
			name: "edit outside root",
			tool: BuiltinEditFile,
			args: map[string]any{
				"path": filepath.Join(outside, "secret.txt"),
				"old":  "secret",
				"new":  "changed",
			},
			wantErr: "outside rootDir",
		},
		{
			name: "write directory",
			tool: BuiltinWriteFile,
			args: map[string]any{
				"path":    "dir",
				"content": "secret",
			},
			wantErr: "is a directory",
		},
		{
			name: "edit directory",
			tool: BuiltinEditFile,
			args: map[string]any{
				"path": "dir",
				"old":  "old",
				"new":  "new",
			},
			wantErr: "is a directory",
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
	if _, err := os.Stat(filepath.Join(outside, "new-secret.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file creation error = %v, want not exist", err)
	}
	if got := readTestFile(t, filepath.Join(outside, "secret.txt")); got != "secret" {
		t.Fatalf("outside file changed to %q", got)
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

func TestWriteAndEditRejectFileSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	writeTestFile(t, outsideFile, "secret")
	createFileSymlinkOrSkip(t, outsideFile, filepath.Join(root, "secret-link.txt"))

	registry := registerBuiltinsForTest(t, root)
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{
			name: "write file symlink",
			tool: BuiltinWriteFile,
			args: map[string]any{
				"path":    "secret-link.txt",
				"content": "changed",
			},
		},
		{
			name: "edit file symlink",
			tool: BuiltinEditFile,
			args: map[string]any{
				"path": "secret-link.txt",
				"old":  "secret",
				"new":  "changed",
			},
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
	if got := readTestFile(t, outsideFile); got != "secret" {
		t.Fatalf("outside file changed to %q", got)
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

func TestWriteFileRejectsSymlinkedParentDirectoryOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideDir := filepath.Join(outside, "secret-dir")
	mkdirTestDir(t, outsideDir)
	createDirSymlinkOrSkip(t, outsideDir, filepath.Join(root, "secret-dir-link"))

	registry := registerBuiltinsForTest(t, root)
	_, err := registry.Execute(context.Background(), BuiltinWriteFile, map[string]any{
		"path":    filepath.Join("secret-dir-link", "created.txt"),
		"content": "secret",
	})
	if err == nil {
		t.Fatal("Execute(write_file) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "outside rootDir") {
		t.Fatalf("Execute(write_file) error = %q, want outside rootDir", err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file creation error = %v, want not exist", err)
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

func TestWorkspaceSearchToolsTreatBlankPathAsRoot(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "root.txt"), "find me")
	registry := registerBuiltinsForTest(t, root)

	tests := []struct {
		name      string
		arguments map[string]any
		want      string
	}{
		{BuiltinListFiles, map[string]any{"path": ""}, "root.txt"},
		{BuiltinGlobFiles, map[string]any{"path": " ", "pattern": "*.txt"}, "root.txt"},
		{BuiltinGrepFiles, map[string]any{"path": "\t", "query": "find me"}, "root.txt:1:find me"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := registry.Execute(context.Background(), tt.name, tt.arguments)
			if err != nil {
				t.Fatalf("Execute(%s) error = %v", tt.name, err)
			}
			if result.Content != tt.want {
				t.Fatalf("Execute(%s) content = %q, want %q", tt.name, result.Content, tt.want)
			}
		})
	}
}

func TestBuiltinDefinitionsHaveExpectedSchemas(t *testing.T) {
	registry := registerBuiltinsForTest(t, t.TempDir())

	tests := []struct {
		name      string
		want      map[string]any
		wantProps []string
	}{
		{
			name: BuiltinReadFile,
			want: map[string]any{
				"type":     "object",
				"required": []any{"path"},
			},
			wantProps: []string{"path", "start_line", "line_count", "max_bytes"},
		},
		{
			name: BuiltinGlobFiles,
			want: map[string]any{
				"type":     "object",
				"required": []any{"pattern"},
			},
			wantProps: []string{"path", "pattern", "include_dirs", "include_hidden", "max_results"},
		},
		{
			name: BuiltinGrepFiles,
			want: map[string]any{
				"type":     "object",
				"required": []any{"query"},
			},
			wantProps: []string{"path", "query", "include", "exclude", "literal", "case_sensitive", "context_lines", "max_results", "max_snippet_bytes"},
		},
		{
			name: BuiltinShell,
			want: map[string]any{
				"type":     "object",
				"required": []any{"command"},
			},
			wantProps: []string{"command", "timeout_ms", "max_output_bytes"},
		},
		{
			name: BuiltinWriteFile,
			want: map[string]any{
				"type":     "object",
				"required": []any{"path", "content"},
			},
			wantProps: []string{"path", "content"},
		},
		{
			name: BuiltinEditFile,
			want: map[string]any{
				"type":     "object",
				"required": []any{"path", "old", "new"},
			},
			wantProps: []string{"path", "old", "new"},
		},
		{
			name: BuiltinApplyPatch,
			want: map[string]any{
				"type":     "object",
				"required": []any{"patch"},
			},
			wantProps: []string{"patch"},
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
			properties, ok := entry.Definition.InputSchema["properties"].(map[string]any)
			if !ok {
				t.Fatalf("InputSchema[properties] = %T, want map[string]any", entry.Definition.InputSchema["properties"])
			}
			for _, prop := range tt.wantProps {
				if _, ok := properties[prop]; !ok {
					t.Fatalf("InputSchema properties missing %q in %#v", prop, properties)
				}
			}
		})
	}
}

func registerBuiltinsForTest(t *testing.T, root string) *Registry {
	t.Helper()

	registry := NewRegistry()
	if err := RegisterBuiltins(registry, root, false); err != nil {
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

func readTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return string(data)
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
	return "cat marker.txt"
}

func failingCommand() string {
	return "printf fail >&2; exit 7"
}

func sleepCommand() string {
	return "sleep 2"
}

func printTextCommand(text string) string {
	return "printf '" + text + "'"
}

func childProcessTreeCommand() string {
	return "sh -c 'printf \"%s\" \"$$\" > child.pid; printf started > started.txt; while :; do printf tick >> child.txt; sleep 0.2; done'"
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("Stat(%q) error = %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file %q did not appear within %v", path, timeout)
}

func waitForFileSize(t *testing.T, path string, minSize int64, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := os.Stat(path)
		if err == nil && info.Size() >= minSize {
			return
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("Stat(%q) error = %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file %q did not reach size %d within %v", path, minSize, timeout)
}

func assertFileStopsGrowing(t *testing.T, path string) {
	t.Helper()

	before := fileSize(t, path)
	time.Sleep(800 * time.Millisecond)
	after := fileSize(t, path)
	if after != before {
		t.Fatalf("file %q kept growing after shell cancellation: size before = %d, after = %d", path, before, after)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	return info.Size()
}

func cleanupChildProcessFromPIDFile(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Logf("cannot read child pid file %q: %v", path, err)
		return
	}
	pidText := strings.TrimSpace(string(data))
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", pidText).Run()
		return
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Logf("cannot parse child pid %q: %v", pidText, err)
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Logf("cannot find child pid %d: %v", pid, err)
		return
	}
	_ = process.Kill()
}

func assertShellContextErrorResult(t *testing.T, result model.ToolResult, wantMessage string) {
	t.Helper()

	if result.Name != BuiltinShell {
		t.Fatalf("Execute(shell) result name = %q, want %q", result.Name, BuiltinShell)
	}
	if !result.IsError {
		t.Fatalf("Execute(shell) IsError = false, want true; result = %#v", result)
	}
	if strings.TrimSpace(result.Content) == "" {
		t.Fatal("Execute(shell) content is empty, want context error message")
	}
	lower := strings.ToLower(result.Content)
	if !strings.Contains(lower, wantMessage) {
		t.Fatalf("Execute(shell) content = %q, want message containing %q", result.Content, wantMessage)
	}
}
