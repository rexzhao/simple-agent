package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func registerFullAccessBuiltinsForTest(t *testing.T, root string) *Registry {
	t.Helper()

	registry := NewRegistry()
	if err := RegisterBuiltins(registry, root, true); err != nil {
		t.Fatalf("RegisterBuiltins(full access) error = %v", err)
	}
	return registry
}

// outsideDir creates a sibling directory of root, so paths inside it are
// guaranteed to be outside the workspace.
func outsideDir(t *testing.T, root string) string {
	t.Helper()

	outside := filepath.Join(filepath.Dir(root), "outside")
	mkdirTestDir(t, outside)
	return outside
}

func TestFullAccessReadFileAcceptsOutsidePaths(t *testing.T) {
	root := t.TempDir()
	outside := outsideDir(t, root)
	writeTestFile(t, filepath.Join(outside, "notes.txt"), "outside\n")

	confined, err := registerBuiltinsForTest(t, root).Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path": filepath.Join(outside, "notes.txt"),
	})
	if err == nil || confined.IsError {
		t.Fatalf("confined read_file outside = %#v, %v; want error", confined, err)
	}

	result, err := registerFullAccessBuiltinsForTest(t, root).Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path": filepath.Join(outside, "notes.txt"),
	})
	if err != nil || result.IsError {
		t.Fatalf("full access read_file outside = %#v, %v; want success", result, err)
	}
	if result.Content != "outside\n" {
		t.Fatalf("full access read_file content = %q, want %q", result.Content, "outside\n")
	}
}

func TestFullAccessRelativePathsStayAnchoredAtWorkspace(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "inside.txt"), "inside\n")

	result, err := registerFullAccessBuiltinsForTest(t, root).Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path": "inside.txt",
	})
	if err != nil || result.IsError {
		t.Fatalf("full access read_file relative = %#v, %v; want success", result, err)
	}
	if result.Content != "inside\n" {
		t.Fatalf("full access relative content = %q, want %q", result.Content, "inside\n")
	}

	// A relative escape (../) is just a relative path that happens to leave
	// the workspace: allowed in full access, rejected when confined.
	_, err = registerBuiltinsForTest(t, root).Execute(context.Background(), BuiltinReadFile, map[string]any{
		"path": "../escape.txt",
	})
	if err == nil {
		t.Fatal("confined read_file ../ error = nil, want error")
	}
}

func TestFullAccessWriteAndEditFileAcceptOutsidePaths(t *testing.T) {
	root := t.TempDir()
	outside := outsideDir(t, root)
	target := filepath.Join(outside, "created.txt")

	if _, err := registerBuiltinsForTest(t, root).Execute(context.Background(), BuiltinWriteFile, map[string]any{
		"path": target, "content": "nope",
	}); err == nil {
		t.Fatal("confined write_file outside error = nil, want error")
	}

	registry := registerFullAccessBuiltinsForTest(t, root)
	if _, err := registry.Execute(context.Background(), BuiltinWriteFile, map[string]any{
		"path": target, "content": "one\ntwo\n",
	}); err != nil {
		t.Fatalf("full access write_file outside error = %v", err)
	}
	if got := readTestFile(t, target); got != "one\ntwo\n" {
		t.Fatalf("written content = %q, want %q", got, "one\ntwo\n")
	}

	if _, err := registry.Execute(context.Background(), BuiltinEditFile, map[string]any{
		"path": target, "old": "two", "new": "three",
	}); err != nil {
		t.Fatalf("full access edit_file outside error = %v", err)
	}
	if got := readTestFile(t, target); got != "one\nthree\n" {
		t.Fatalf("edited content = %q, want %q", got, "one\nthree\n")
	}
}

func TestFullAccessApplyPatchAcceptsOutsidePaths(t *testing.T) {
	root := t.TempDir()
	outside := outsideDir(t, root)
	writeTestFile(t, filepath.Join(outside, "update.txt"), "before\n")

	patch := "*** Begin Patch\n*** Add File: " + filepath.Join(outside, "added.txt") + "\n+added\n*** Update File: " + filepath.Join(outside, "update.txt") + "\n@@\n-before\n+after\n*** End Patch"

	if _, err := registerBuiltinsForTest(t, root).Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": patch,
	}); err == nil {
		t.Fatal("confined apply_patch outside error = nil, want error")
	}

	if _, err := registerFullAccessBuiltinsForTest(t, root).Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": patch,
	}); err != nil {
		t.Fatalf("full access apply_patch outside error = %v", err)
	}
	if got := readTestFile(t, filepath.Join(outside, "added.txt")); got != "added\n" {
		t.Fatalf("added content = %q, want %q", got, "added\n")
	}
	if got := readTestFile(t, filepath.Join(outside, "update.txt")); got != "after\n" {
		t.Fatalf("updated content = %q, want %q", got, "after\n")
	}
}

func TestFullAccessListAndGlobReportOutsidePaths(t *testing.T) {
	root := t.TempDir()
	outside := outsideDir(t, root)
	writeTestFile(t, filepath.Join(outside, "match.go"), "package outside\n")

	registry := registerFullAccessBuiltinsForTest(t, root)

	listResult, err := registry.Execute(context.Background(), BuiltinListFiles, map[string]any{"path": outside})
	if err != nil || listResult.IsError {
		t.Fatalf("full access list_files outside = %#v, %v; want success", listResult, err)
	}
	if listResult.Content != "match.go" {
		t.Fatalf("list_files outside content = %q, want %q", listResult.Content, "match.go")
	}

	globResult, err := registry.Execute(context.Background(), BuiltinGlobFiles, map[string]any{
		"path": outside, "pattern": "*.go",
	})
	if err != nil || globResult.IsError {
		t.Fatalf("full access glob_files outside = %#v, %v; want success", globResult, err)
	}
	want := filepath.ToSlash(filepath.Join(outside, "match.go"))
	if globResult.Content != want {
		t.Fatalf("glob_files outside content = %q, want absolute %q", globResult.Content, want)
	}
}

func TestFullAccessGrepReportsOutsidePaths(t *testing.T) {
	root := t.TempDir()
	outside := outsideDir(t, root)
	writeTestFile(t, filepath.Join(outside, "match.txt"), "needle in outside\n")

	registry := registerFullAccessBuiltinsForTest(t, root)

	dirResult, err := registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"path": outside, "query": "needle", "literal": true,
	})
	if err != nil || dirResult.IsError {
		t.Fatalf("full access grep_files outside dir = %#v, %v; want success", dirResult, err)
	}
	wantPrefix := filepath.ToSlash(filepath.Join(outside, "match.txt")) + ":1:"
	if !strings.HasPrefix(dirResult.Content, wantPrefix) {
		t.Fatalf("grep_files outside dir content = %q, want prefix %q", dirResult.Content, wantPrefix)
	}

	fileResult, err := registry.Execute(context.Background(), BuiltinGrepFiles, map[string]any{
		"path": filepath.Join(outside, "match.txt"), "query": "needle", "literal": true,
	})
	if err != nil || fileResult.IsError {
		t.Fatalf("full access grep_files outside file = %#v, %v; want success", fileResult, err)
	}
	if !strings.HasPrefix(fileResult.Content, wantPrefix) {
		t.Fatalf("grep_files outside file content = %q, want prefix %q", fileResult.Content, wantPrefix)
	}
}

func TestFullAccessDefinitionsAdvertiseAbsolutePaths(t *testing.T) {
	registry := registerFullAccessBuiltinsForTest(t, t.TempDir())
	for _, name := range []string{BuiltinListFiles, BuiltinReadFile, BuiltinGlobFiles, BuiltinGrepFiles, BuiltinWriteFile, BuiltinEditFile, BuiltinApplyPatch} {
		entry, ok := registry.Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) ok = false", name)
		}
		if !strings.Contains(entry.Definition.Description, "absolute") {
			t.Fatalf("%q full access description = %q, want it to mention absolute paths", name, entry.Definition.Description)
		}
	}

	confined := registerBuiltinsForTest(t, t.TempDir())
	entry, _ := confined.Lookup(BuiltinReadFile)
	if strings.Contains(entry.Definition.Description, "absolute") {
		t.Fatalf("confined read_file description = %q, want no absolute path mention", entry.Definition.Description)
	}
}
