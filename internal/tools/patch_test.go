package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchUpdatesTextAndPreservesLineEnding(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "first\r\nold\r\nlast\r\n")
	registry := registerBuiltinsForTest(t, root)

	result, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": `--- a/notes.txt
+++ b/notes.txt
@@ -1,3 +1,4 @@
 first
-old
+new
+middle
 last
`,
	})
	if err != nil {
		t.Fatalf("Execute(apply_patch) error = %v", err)
	}
	if result.Name != BuiltinApplyPatch || result.Content != "applied unified patch (1 updated)" || result.IsError {
		t.Fatalf("Execute(apply_patch) result = %#v", result)
	}
	if got := readTestFile(t, filepath.Join(root, "notes.txt")); got != "first\r\nnew\r\nmiddle\r\nlast\r\n" {
		t.Fatalf("patched content = %q", got)
	}
}

func TestApplyPatchPreservesMissingTrailingNewline(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "old")
	registry := registerBuiltinsForTest(t, root)

	_, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": `--- a/notes.txt
+++ b/notes.txt
@@ -1 +1 @@
-old
\ No newline at end of file
+new
\ No newline at end of file
`,
	})
	if err != nil {
		t.Fatalf("Execute(apply_patch) error = %v", err)
	}
	if got := readTestFile(t, filepath.Join(root, "notes.txt")); got != "new" {
		t.Fatalf("patched content = %q, want no trailing newline", got)
	}
}

func TestApplyPatchCreatesAndDeletesFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "remove.txt"), "remove\n")
	registry := registerBuiltinsForTest(t, root)

	result, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": `--- /dev/null
+++ b/nested/new.txt
@@ -0,0 +1,2 @@
+new
+file
--- a/remove.txt
+++ /dev/null
@@ -1 +0,0 @@
-remove
`,
	})
	if err != nil {
		t.Fatalf("Execute(apply_patch) error = %v", err)
	}
	if result.Content != "applied unified patch (1 added, 1 deleted)" {
		t.Fatalf("Execute(apply_patch) result = %#v", result)
	}
	if got := readTestFile(t, filepath.Join(root, "nested", "new.txt")); got != "new\nfile\n" {
		t.Fatalf("created content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "remove.txt")); !os.IsNotExist(err) {
		t.Fatalf("removed file stat error = %v, want not exist", err)
	}
}

func TestApplyPatchValidatesAllHunksBeforeWriting(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "actual\n")
	registry := registerBuiltinsForTest(t, root)

	_, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": `--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+should not be written
--- a/notes.txt
+++ b/notes.txt
@@ -1 +1 @@
-expected
+changed
`,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Execute(apply_patch) error = %v, want hunk mismatch", err)
	}
	if got := readTestFile(t, filepath.Join(root, "notes.txt")); got != "actual\n" {
		t.Fatalf("existing content = %q, want unchanged", got)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new file stat error = %v, want not exist", err)
	}
}

func TestApplyPatchRejectsSymlinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	writeTestFile(t, outsideFile, "secret\n")
	createFileSymlinkOrSkip(t, outsideFile, filepath.Join(root, "secret-link.txt"))
	registry := registerBuiltinsForTest(t, root)

	_, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": `--- a/secret-link.txt
+++ b/secret-link.txt
@@ -1 +1 @@
-secret
+changed
`,
	})
	if err == nil || !strings.Contains(err.Error(), "outside rootDir") {
		t.Fatalf("Execute(apply_patch) error = %v, want workspace path rejection", err)
	}
	if got := readTestFile(t, outsideFile); got != "secret\n" {
		t.Fatalf("outside content = %q, want unchanged", got)
	}
}

func TestApplyPatchRejectsWindowsAbsolutePath(t *testing.T) {
	registry := registerBuiltinsForTest(t, t.TempDir())
	_, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": `--- /dev/null
+++ b/C:/outside.txt
@@ -0,0 +1 @@
+secret
`,
	})
	if err == nil || !strings.Contains(err.Error(), "workspace-relative") {
		t.Fatalf("Execute(apply_patch) error = %v, want Windows absolute path rejection", err)
	}
}

func TestApplyPatchRejectsPathsOutsideWorkspace(t *testing.T) {
	registry := registerBuiltinsForTest(t, t.TempDir())
	_, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": `--- /dev/null
+++ b/../outside.txt
@@ -0,0 +1 @@
+secret
`,
	})
	if err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("Execute(apply_patch) error = %v, want workspace path rejection", err)
	}
}
