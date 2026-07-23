package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

const maxApplyPatchBytes = 2 * 1024 * 1024

var unifiedHunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?: .*)?$`)

type unifiedPatchFile struct {
	oldPath string
	newPath string
	hunks   []unifiedPatchHunk
}

type unifiedPatchHunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []unifiedPatchLine
}

type unifiedPatchLine struct {
	kind      byte
	text      string
	noNewline bool
}

type patchTextLine struct {
	text      string
	noNewline bool
}

type patchOperationKind int

const (
	patchCreate patchOperationKind = iota
	patchUpdate
	patchDelete
)

type patchOperation struct {
	kind         patchOperationKind
	workspaceRel string
	resolved     string
	content      []byte
	mode         os.FileMode
}

func applyPatchDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinApplyPatch,
		Description: "Apply a unified diff patch to text files under the workspace. The patch must use ---/+++ file headers and @@ hunks; paths must be workspace-relative. It supports file creation, updates, and deletion, and validates every hunk before writing.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"patch": map[string]any{
					"type":        "string",
					"description": "Unified diff patch. Use --- a/path and +++ b/path headers; use /dev/null for added or deleted files.",
				},
			},
			"required":             []any{"patch"},
			"additionalProperties": false,
		},
	}
}

func newApplyPatchExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		patch, err := requiredNonEmptyStringArgument(arguments, "patch")
		if err != nil {
			return model.ToolResult{}, err
		}
		if len(patch) > maxApplyPatchBytes {
			return model.ToolResult{}, fmt.Errorf("patch exceeds the %d byte limit", maxApplyPatchBytes)
		}
		if err := ctx.Err(); err != nil {
			return model.ToolResult{}, err
		}

		files, err := parseUnifiedPatch(patch)
		if err != nil {
			return model.ToolResult{}, err
		}
		operations, err := preparePatchOperations(rootDir, files)
		if err != nil {
			return model.ToolResult{}, err
		}
		if err := applyPatchOperations(ctx, operations); err != nil {
			return model.ToolResult{}, err
		}

		added, updated, deleted := 0, 0, 0
		for _, operation := range operations {
			switch operation.kind {
			case patchCreate:
				added++
			case patchUpdate:
				updated++
			case patchDelete:
				deleted++
			}
		}
		parts := make([]string, 0, 3)
		if added > 0 {
			parts = append(parts, fmt.Sprintf("%d added", added))
		}
		if updated > 0 {
			parts = append(parts, fmt.Sprintf("%d updated", updated))
		}
		if deleted > 0 {
			parts = append(parts, fmt.Sprintf("%d deleted", deleted))
		}
		return model.ToolResult{
			Name:    BuiltinApplyPatch,
			Content: fmt.Sprintf("applied unified patch (%s)", strings.Join(parts, ", ")),
		}, nil
	})
}

func parseUnifiedPatch(value string) ([]unifiedPatchFile, error) {
	if strings.ContainsRune(value, 0) {
		return nil, fmt.Errorf("patch must be text")
	}
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	files := make([]unifiedPatchFile, 0)
	for index := 0; index < len(lines); {
		line := lines[index]
		if line == "" || isUnifiedPatchMetadata(line) {
			index++
			continue
		}
		if !strings.HasPrefix(line, "--- ") {
			return nil, fmt.Errorf("invalid unified patch line %d: expected --- file header", index+1)
		}
		oldPath, err := parseUnifiedPatchPath(strings.TrimPrefix(line, "--- "))
		if err != nil {
			return nil, fmt.Errorf("invalid old path at patch line %d: %w", index+1, err)
		}
		index++
		if index >= len(lines) || !strings.HasPrefix(lines[index], "+++ ") {
			return nil, fmt.Errorf("invalid unified patch after %q: expected +++ file header", oldPath)
		}
		newPath, err := parseUnifiedPatchPath(strings.TrimPrefix(lines[index], "+++ "))
		if err != nil {
			return nil, fmt.Errorf("invalid new path at patch line %d: %w", index+1, err)
		}
		if oldPath == "" && newPath == "" {
			return nil, fmt.Errorf("invalid unified patch: both file paths are /dev/null")
		}
		if oldPath != "" && newPath != "" && oldPath != newPath {
			return nil, fmt.Errorf("renaming files is not supported (%q to %q)", oldPath, newPath)
		}
		index++

		file := unifiedPatchFile{oldPath: oldPath, newPath: newPath}
		for index < len(lines) && strings.HasPrefix(lines[index], "@@ ") {
			hunk, next, err := parseUnifiedPatchHunk(lines, index)
			if err != nil {
				return nil, err
			}
			file.hunks = append(file.hunks, hunk)
			index = next
		}
		if len(file.hunks) == 0 {
			return nil, fmt.Errorf("patch for %q has no hunks", firstPatchPath(file))
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("patch contains no file changes")
	}
	return files, nil
}

func isUnifiedPatchMetadata(line string) bool {
	return strings.HasPrefix(line, "diff --git ") ||
		strings.HasPrefix(line, "index ") ||
		strings.HasPrefix(line, "new file mode ") ||
		strings.HasPrefix(line, "deleted file mode ") ||
		strings.HasPrefix(line, "similarity index ") ||
		strings.HasPrefix(line, "dissimilarity index ")
}

func firstPatchPath(file unifiedPatchFile) string {
	if file.newPath != "" {
		return file.newPath
	}
	return file.oldPath
}

func parseUnifiedPatchPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if tab := strings.IndexByte(value, '\t'); tab >= 0 {
		value = value[:tab]
	} else if fields := strings.Fields(value); len(fields) > 0 {
		value = fields[0]
	}
	if value == "/dev/null" {
		return "", nil
	}
	if strings.HasPrefix(value, "a/") || strings.HasPrefix(value, "b/") {
		value = value[2:]
	}
	if value == "" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || (len(value) >= 3 && value[1] == ':' && value[2] == '/') {
		return "", fmt.Errorf("path must be a workspace-relative slash path")
	}
	cleaned := pathpkg.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q is outside the workspace", value)
	}
	return cleaned, nil
}

func parseUnifiedPatchHunk(lines []string, index int) (unifiedPatchHunk, int, error) {
	matches := unifiedHunkHeader.FindStringSubmatch(lines[index])
	if matches == nil {
		return unifiedPatchHunk{}, index, fmt.Errorf("invalid hunk header at patch line %d", index+1)
	}
	oldStart, oldCount, err := parseUnifiedHunkRange(matches[1], matches[2])
	if err != nil {
		return unifiedPatchHunk{}, index, fmt.Errorf("invalid old hunk range at patch line %d: %w", index+1, err)
	}
	newStart, newCount, err := parseUnifiedHunkRange(matches[3], matches[4])
	if err != nil {
		return unifiedPatchHunk{}, index, fmt.Errorf("invalid new hunk range at patch line %d: %w", index+1, err)
	}
	if oldStart == 0 && oldCount != 0 || newStart == 0 && newCount != 0 {
		return unifiedPatchHunk{}, index, fmt.Errorf("invalid zero line number at patch line %d", index+1)
	}

	hunk := unifiedPatchHunk{oldStart: oldStart, oldCount: oldCount, newStart: newStart, newCount: newCount}
	index++
	oldLines, newLines := 0, 0
	for oldLines < oldCount || newLines < newCount {
		if index >= len(lines) {
			return unifiedPatchHunk{}, index, fmt.Errorf("truncated hunk starting at patch line %d", index-len(hunk.lines))
		}
		line := lines[index]
		if line == `\ No newline at end of file` {
			if len(hunk.lines) == 0 {
				return unifiedPatchHunk{}, index, fmt.Errorf("newline marker without a hunk line at patch line %d", index+1)
			}
			hunk.lines[len(hunk.lines)-1].noNewline = true
			index++
			continue
		}
		if line == "" || (line[0] != ' ' && line[0] != '+' && line[0] != '-') {
			return unifiedPatchHunk{}, index, fmt.Errorf("invalid hunk line at patch line %d", index+1)
		}
		patchLine := unifiedPatchLine{kind: line[0], text: line[1:]}
		switch patchLine.kind {
		case ' ':
			oldLines++
			newLines++
		case '-':
			oldLines++
		case '+':
			newLines++
		}
		if oldLines > oldCount || newLines > newCount {
			return unifiedPatchHunk{}, index, fmt.Errorf("hunk line counts exceed header at patch line %d", index+1)
		}
		hunk.lines = append(hunk.lines, patchLine)
		index++
	}
	for index < len(lines) && lines[index] == `\ No newline at end of file` {
		if len(hunk.lines) == 0 {
			return unifiedPatchHunk{}, index, fmt.Errorf("newline marker without a hunk line at patch line %d", index+1)
		}
		hunk.lines[len(hunk.lines)-1].noNewline = true
		index++
	}
	return hunk, index, nil
}

func parseUnifiedHunkRange(startText, countText string) (int, int, error) {
	start, err := strconv.Atoi(startText)
	if err != nil {
		return 0, 0, err
	}
	count := 1
	if countText != "" {
		count, err = strconv.Atoi(countText)
		if err != nil {
			return 0, 0, err
		}
	}
	if start < 0 || count < 0 {
		return 0, 0, fmt.Errorf("line range must not be negative")
	}
	return start, count, nil
}

func preparePatchOperations(rootDir string, files []unifiedPatchFile) ([]patchOperation, error) {
	seen := make(map[string]struct{}, len(files))
	operations := make([]patchOperation, 0, len(files))
	for _, file := range files {
		patchPaths := []string{file.oldPath}
		if file.newPath != file.oldPath {
			patchPaths = append(patchPaths, file.newPath)
		}
		for _, workspaceRel := range patchPaths {
			if workspaceRel == "" {
				continue
			}
			if _, exists := seen[workspaceRel]; exists {
				return nil, fmt.Errorf("patch changes %q more than once", workspaceRel)
			}
			seen[workspaceRel] = struct{}{}
		}

		workspaceRel := firstPatchPath(file)
		if file.oldPath == "" {
			resolved, err := resolveWritableFilePath(rootDir, workspaceRel)
			if err != nil {
				return nil, err
			}
			if _, err := os.Lstat(resolved); err == nil {
				return nil, fmt.Errorf("add file %q: path already exists", workspaceRel)
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("stat new file %q: %w", workspaceRel, err)
			}
			content, err := applyUnifiedHunks(nil, file.hunks)
			if err != nil {
				return nil, fmt.Errorf("apply patch to %q: %w", workspaceRel, err)
			}
			operations = append(operations, patchOperation{kind: patchCreate, workspaceRel: workspaceRel, resolved: resolved, content: content, mode: 0o644})
			continue
		}

		resolved, info, source, err := readPatchSource(rootDir, workspaceRel)
		if err != nil {
			return nil, err
		}
		content, err := applyUnifiedHunks(source, file.hunks)
		if err != nil {
			return nil, fmt.Errorf("apply patch to %q: %w", workspaceRel, err)
		}
		if file.newPath == "" {
			if len(content) != 0 {
				return nil, fmt.Errorf("delete patch for %q leaves content behind", workspaceRel)
			}
			operations = append(operations, patchOperation{kind: patchDelete, workspaceRel: workspaceRel, resolved: resolved})
			continue
		}
		operations = append(operations, patchOperation{kind: patchUpdate, workspaceRel: workspaceRel, resolved: resolved, content: content, mode: info.Mode().Perm()})
	}
	return operations, nil
}

func readPatchSource(rootDir, workspaceRel string) (string, os.FileInfo, []byte, error) {
	resolved, err := resolveRootPath(rootDir, filepath.FromSlash(workspaceRel))
	if err != nil {
		return "", nil, nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, nil, fmt.Errorf("stat file %q: %w", workspaceRel, err)
	}
	if info.IsDir() {
		return "", nil, nil, fmt.Errorf("path %q is a directory", workspaceRel)
	}
	source, err := os.ReadFile(resolved)
	if err != nil {
		return "", nil, nil, fmt.Errorf("read file %q: %w", workspaceRel, err)
	}
	if bytes.Contains(source, []byte{0}) {
		return "", nil, nil, fmt.Errorf("patch file %q: file appears to be binary", workspaceRel)
	}
	return resolved, info, source, nil
}

func applyPatchOperations(ctx context.Context, operations []patchOperation) error {
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch operation.kind {
		case patchCreate:
			if err := os.MkdirAll(filepath.Dir(operation.resolved), 0o755); err != nil {
				return fmt.Errorf("create parent directories for %q: %w", operation.workspaceRel, err)
			}
			if err := os.WriteFile(operation.resolved, operation.content, operation.mode); err != nil {
				return fmt.Errorf("write file %q: %w", operation.workspaceRel, err)
			}
		case patchUpdate:
			if err := os.WriteFile(operation.resolved, operation.content, operation.mode); err != nil {
				return fmt.Errorf("write file %q: %w", operation.workspaceRel, err)
			}
		case patchDelete:
			if err := os.Remove(operation.resolved); err != nil {
				return fmt.Errorf("remove file %q: %w", operation.workspaceRel, err)
			}
		}
	}
	return nil
}

func applyUnifiedHunks(source []byte, hunks []unifiedPatchHunk) ([]byte, error) {
	original, lineEnding, err := splitPatchText(source)
	if err != nil {
		return nil, err
	}
	result := make([]patchTextLine, 0, len(original))
	cursor := 0
	for hunkIndex, hunk := range hunks {
		start := hunk.oldStart - 1
		if hunk.oldStart == 0 {
			start = 0
		}
		if start < cursor || start > len(original) {
			return nil, fmt.Errorf("hunk %d starts at unavailable source line %d", hunkIndex+1, hunk.oldStart)
		}
		var appendErr error
		result, appendErr = appendPatchTextLines(result, original[cursor:start])
		if appendErr != nil {
			return nil, appendErr
		}
		cursor = start
		for _, line := range hunk.lines {
			switch line.kind {
			case ' ':
				if cursor >= len(original) || original[cursor].text != line.text {
					return nil, fmt.Errorf("hunk %d context does not match source line %d", hunkIndex+1, cursor+1)
				}
				if original[cursor].noNewline != line.noNewline {
					return nil, fmt.Errorf("hunk %d newline marker does not match source line %d", hunkIndex+1, cursor+1)
				}
				result, appendErr = appendPatchTextLines(result, []patchTextLine{original[cursor]})
				if appendErr != nil {
					return nil, appendErr
				}
				cursor++
			case '-':
				if cursor >= len(original) || original[cursor].text != line.text {
					return nil, fmt.Errorf("hunk %d removal does not match source line %d", hunkIndex+1, cursor+1)
				}
				if original[cursor].noNewline != line.noNewline {
					return nil, fmt.Errorf("hunk %d newline marker does not match source line %d", hunkIndex+1, cursor+1)
				}
				cursor++
			case '+':
				result, appendErr = appendPatchTextLines(result, []patchTextLine{{text: line.text, noNewline: line.noNewline}})
				if appendErr != nil {
					return nil, appendErr
				}
			}
		}
	}
	result, err = appendPatchTextLines(result, original[cursor:])
	if err != nil {
		return nil, err
	}
	return joinPatchText(result, lineEnding)
}

func splitPatchText(source []byte) ([]patchTextLine, string, error) {
	if bytes.Contains(source, []byte{0}) {
		return nil, "", fmt.Errorf("file appears to be binary")
	}
	lineEnding := "\n"
	if bytes.Contains(source, []byte("\r\n")) {
		lineEnding = "\r\n"
	}
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	endsWithNewline := strings.HasSuffix(text, "\n")
	if endsWithNewline {
		text = strings.TrimSuffix(text, "\n")
	}
	if text == "" && !endsWithNewline {
		return nil, lineEnding, nil
	}
	parts := strings.Split(text, "\n")
	lines := make([]patchTextLine, len(parts))
	for index, part := range parts {
		lines[index] = patchTextLine{text: part, noNewline: !endsWithNewline && index == len(parts)-1}
	}
	return lines, lineEnding, nil
}

func appendPatchTextLines(result, next []patchTextLine) ([]patchTextLine, error) {
	for _, line := range next {
		if len(result) > 0 && result[len(result)-1].noNewline {
			return nil, fmt.Errorf("patch adds content after a line without a trailing newline")
		}
		result = append(result, line)
	}
	return result, nil
}

func joinPatchText(lines []patchTextLine, lineEnding string) ([]byte, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	var output strings.Builder
	for index, line := range lines {
		if index > 0 {
			output.WriteString(lineEnding)
		}
		output.WriteString(line.text)
		if line.noNewline && index != len(lines)-1 {
			return nil, fmt.Errorf("line without a trailing newline is not final")
		}
	}
	if !lines[len(lines)-1].noNewline {
		output.WriteString(lineEnding)
	}
	return []byte(output.String()), nil
}
