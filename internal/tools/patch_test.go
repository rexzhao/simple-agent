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
		"patch": `*** Begin Patch
*** Update File: notes.txt
@@
 first
-old
+new
+middle
 last
*** End Patch`,
	})
	if err != nil {
		t.Fatalf("Execute(apply_patch) error = %v", err)
	}
	if result.Name != BuiltinApplyPatch || result.Content != "applied OpenCode patch (1 updated)" || result.IsError {
		t.Fatalf("Execute(apply_patch) result = %#v", result)
	}
	if got := readTestFile(t, filepath.Join(root, "notes.txt")); got != "first\r\nnew\r\nmiddle\r\nlast\r\n" {
		t.Fatalf("patched content = %q", got)
	}
}

func TestApplyPatchAddsDeletesAndMovesFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "move.txt"), "old\n")
	writeTestFile(t, filepath.Join(root, "remove.txt"), "remove\n")
	registry := registerBuiltinsForTest(t, root)

	result, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": `*** Begin Patch
*** Add File: nested/new.txt
+new
+file
*** Update File: move.txt
@@
-old
+changed
*** Move to: nested/moved.txt
*** Delete File: remove.txt
*** End Patch`,
	})
	if err != nil {
		t.Fatalf("Execute(apply_patch) error = %v", err)
	}
	if result.Content != "applied OpenCode patch (1 added, 1 deleted, 1 moved)" {
		t.Fatalf("Execute(apply_patch) result = %#v", result)
	}
	if got := readTestFile(t, filepath.Join(root, "nested", "new.txt")); got != "new\nfile\n" {
		t.Fatalf("created content = %q", got)
	}
	if got := readTestFile(t, filepath.Join(root, "nested", "moved.txt")); got != "changed\n" {
		t.Fatalf("moved content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "move.txt")); !os.IsNotExist(err) {
		t.Fatalf("moved source stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(root, "remove.txt")); !os.IsNotExist(err) {
		t.Fatalf("removed file stat error = %v, want not exist", err)
	}
}

func TestApplyPatchSupportsAnchorAndEndOfFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "anchored.txt"), "first\nlast\n")
	writeTestFile(t, filepath.Join(root, "eof.txt"), "first\nlast\n")
	registry := registerBuiltinsForTest(t, root)

	_, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": `*** Begin Patch
*** Update File: anchored.txt
@@ last
+after-anchor
*** Update File: eof.txt
@@
 last
+at-end
*** End of File
*** End Patch`,
	})
	if err != nil {
		t.Fatalf("Execute(apply_patch) error = %v", err)
	}
	if got := readTestFile(t, filepath.Join(root, "anchored.txt")); got != "first\nlast\nafter-anchor\n" {
		t.Fatalf("anchored content = %q", got)
	}
	if got := readTestFile(t, filepath.Join(root, "eof.txt")); got != "first\nlast\nat-end\n" {
		t.Fatalf("end-of-file content = %q", got)
	}
}

func TestApplyPatchValidatesAllFilesBeforeWriting(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.txt"), "actual\n")
	registry := registerBuiltinsForTest(t, root)

	_, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": `*** Begin Patch
*** Add File: new.txt
+should not be written
*** Update File: notes.txt
@@
-expected
+changed
*** End Patch`,
	})
	if err == nil || !strings.Contains(err.Error(), "expected hunk content") {
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
		"patch": `*** Begin Patch
*** Update File: secret-link.txt
@@
-secret
+changed
*** End Patch`,
	})
	if err == nil || !strings.Contains(err.Error(), "outside rootDir") {
		t.Fatalf("Execute(apply_patch) error = %v, want workspace path rejection", err)
	}
	if got := readTestFile(t, outsideFile); got != "secret\n" {
		t.Fatalf("outside content = %q, want unchanged", got)
	}
}

func TestApplyPatchRejectsInvalidPaths(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		match string
	}{
		{name: "Windows absolute", path: "C:/outside.txt", match: "workspace-relative"},
		{name: "outside workspace", path: "../outside.txt", match: "outside the workspace"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := registerBuiltinsForTest(t, t.TempDir())
			_, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
				"patch": "*** Begin Patch\n*** Add File: " + test.path + "\n+secret\n*** End Patch",
			})
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("Execute(apply_patch) error = %v, want %q", err, test.match)
			}
		})
	}
}

func TestApplyPatchRejectsStandardUnifiedDiff(t *testing.T) {
	registry := registerBuiltinsForTest(t, t.TempDir())
	_, err := registry.Execute(context.Background(), BuiltinApplyPatch, map[string]any{
		"patch": "--- a/notes.txt\n+++ b/notes.txt\n@@ -1 +1 @@\n-old\n+new\n",
	})
	if err == nil || !strings.Contains(err.Error(), openCodePatchBegin) {
		t.Fatalf("Execute(apply_patch) error = %v, want OpenCode format rejection", err)
	}
}

func TestApplyPatchDefinitionDescribesOpenCodeRequirements(t *testing.T) {
	definition := applyPatchDefinition(false)
	for _, want := range []string{
		"OpenCode-format",
		"*** Begin Patch",
		"*** End Patch",
		"*** Add File:",
		"*** Move to:",
		"validated before any write",
	} {
		if !strings.Contains(definition.Description, want) {
			t.Fatalf("description = %q, want contain %q", definition.Description, want)
		}
	}
	properties := definition.InputSchema["properties"].(map[string]any)
	patch := properties["patch"].(map[string]any)
	for _, want := range []string{"*** Begin Patch", "*** Update File: path", "*** Delete File: path", "*** Move to:"} {
		if !strings.Contains(patch["description"].(string), want) {
			t.Fatalf("patch description = %q, want contain %q", patch["description"], want)
		}
	}
}
