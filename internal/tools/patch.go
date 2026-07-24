package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	maxApplyPatchBytes = 2 * 1024 * 1024
	openCodePatchBegin = "*** Begin Patch"
	openCodePatchEnd   = "*** End Patch"
	openCodeEndOfFile  = "*** End of File"
)

type openCodePatchFile struct {
	kind   patchOperationKind
	path   string
	moveTo string
	hunks  []openCodePatchHunk
}

type openCodePatchHunk struct {
	anchor    string
	endOfFile bool
	lines     []openCodePatchLine
}

type openCodePatchLine struct {
	kind byte
	text string
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
	patchMove
)

type patchOperation struct {
	kind           patchOperationKind
	workspaceRel   string
	resolved       string
	targetRel      string
	targetResolved string
	content        []byte
	mode           os.FileMode
}

func applyPatchDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinApplyPatch,
		Description: "Apply an OpenCode-format text patch to workspace-relative files. The patch must start with *** Begin Patch and end with *** End Patch. Use *** Add File:, *** Update File:, *** Delete File:, and optional *** Move to: sections; update hunks use @@ anchors with space, - and + lines. Supports create, update, delete, and move. All files and hunks are validated before any write; binary files and paths outside the workspace are rejected.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"patch": map[string]any{
					"type":        "string",
					"description": "OpenCode patch text. Begin with *** Begin Patch and end with *** End Patch. Use *** Add File: path (every content line starts with +), *** Update File: path followed by @@ hunks, *** Delete File: path, and optional *** Move to: destination after an update. Update hunk lines start with space, - or +.",
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

		files, err := parseOpenCodePatch(patch)
		if err != nil {
			return model.ToolResult{}, err
		}
		operations, err := prepareOpenCodePatchOperations(rootDir, files)
		if err != nil {
			return model.ToolResult{}, err
		}
		if err := applyPatchOperations(ctx, operations); err != nil {
			return model.ToolResult{}, err
		}

		added, updated, deleted, moved := 0, 0, 0, 0
		for _, operation := range operations {
			switch operation.kind {
			case patchCreate:
				added++
			case patchUpdate:
				updated++
			case patchDelete:
				deleted++
			case patchMove:
				moved++
			}
		}
		parts := make([]string, 0, 4)
		if added > 0 {
			parts = append(parts, fmt.Sprintf("%d added", added))
		}
		if updated > 0 {
			parts = append(parts, fmt.Sprintf("%d updated", updated))
		}
		if deleted > 0 {
			parts = append(parts, fmt.Sprintf("%d deleted", deleted))
		}
		if moved > 0 {
			parts = append(parts, fmt.Sprintf("%d moved", moved))
		}
		return model.ToolResult{
			Name:    BuiltinApplyPatch,
			Content: fmt.Sprintf("applied OpenCode patch (%s)", strings.Join(parts, ", ")),
		}, nil
	})
}

func parseOpenCodePatch(value string) ([]openCodePatchFile, error) {
	if strings.ContainsRune(value, 0) {
		return nil, fmt.Errorf("patch must be text")
	}
	lines := strings.Split(normalizePatchNewlines(value), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || lines[0] != openCodePatchBegin {
		return nil, fmt.Errorf("OpenCode patch must begin with %q", openCodePatchBegin)
	}

	files := make([]openCodePatchFile, 0)
	index := 1
	ended := false
	for index < len(lines) {
		line := lines[index]
		if line == openCodePatchEnd {
			ended = true
			index++
			break
		}
		if line == "" {
			return nil, fmt.Errorf("unexpected blank line at patch line %d", index+1)
		}
		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			path, err := parseOpenCodePatchPath(strings.TrimPrefix(line, "*** Add File: "))
			if err != nil {
				return nil, fmt.Errorf("invalid added file path at patch line %d: %w", index+1, err)
			}
			index++
			hunk := openCodePatchHunk{}
			for index < len(lines) && !isOpenCodePatchDirective(lines[index]) {
				if !strings.HasPrefix(lines[index], "+") {
					return nil, fmt.Errorf("added file %q has invalid content at patch line %d: every line must start with +", path, index+1)
				}
				hunk.lines = append(hunk.lines, openCodePatchLine{kind: '+', text: strings.TrimPrefix(lines[index], "+")})
				index++
			}
			files = append(files, openCodePatchFile{kind: patchCreate, path: path, hunks: []openCodePatchHunk{hunk}})
		case strings.HasPrefix(line, "*** Update File: "):
			path, err := parseOpenCodePatchPath(strings.TrimPrefix(line, "*** Update File: "))
			if err != nil {
				return nil, fmt.Errorf("invalid updated file path at patch line %d: %w", index+1, err)
			}
			index++
			file := openCodePatchFile{kind: patchUpdate, path: path}
			for index < len(lines) {
				line = lines[index]
				if isOpenCodeHunkHeader(line) {
					if file.moveTo != "" {
						return nil, fmt.Errorf("updated file %q has a hunk after *** Move to at patch line %d", path, index+1)
					}
					hunk, next, err := parseOpenCodePatchHunk(lines, index)
					if err != nil {
						return nil, err
					}
					file.hunks = append(file.hunks, hunk)
					index = next
					continue
				}
				if strings.HasPrefix(line, "*** Move to: ") {
					if file.moveTo != "" {
						return nil, fmt.Errorf("updated file %q has multiple move destinations", path)
					}
					moveTo, err := parseOpenCodePatchPath(strings.TrimPrefix(line, "*** Move to: "))
					if err != nil {
						return nil, fmt.Errorf("invalid move destination at patch line %d: %w", index+1, err)
					}
					if moveTo == path {
						return nil, fmt.Errorf("updated file %q cannot move to itself", path)
					}
					file.kind = patchMove
					file.moveTo = moveTo
					index++
					continue
				}
				if isOpenCodePatchDirective(line) {
					break
				}
				return nil, fmt.Errorf("updated file %q expects @@ hunk or *** Move to at patch line %d", path, index+1)
			}
			if len(file.hunks) == 0 && file.moveTo == "" {
				return nil, fmt.Errorf("updated file %q has no hunks", path)
			}
			files = append(files, file)
		case strings.HasPrefix(line, "*** Delete File: "):
			path, err := parseOpenCodePatchPath(strings.TrimPrefix(line, "*** Delete File: "))
			if err != nil {
				return nil, fmt.Errorf("invalid deleted file path at patch line %d: %w", index+1, err)
			}
			index++
			if index < len(lines) && !isOpenCodePatchDirective(lines[index]) {
				return nil, fmt.Errorf("deleted file %q must not contain hunks or content", path)
			}
			files = append(files, openCodePatchFile{kind: patchDelete, path: path})
		case strings.HasPrefix(line, "*** Move to: "):
			return nil, fmt.Errorf("*** Move to must follow an *** Update File section at patch line %d", index+1)
		case line == openCodeEndOfFile:
			return nil, fmt.Errorf("%q must appear inside an update hunk at patch line %d", openCodeEndOfFile, index+1)
		default:
			return nil, fmt.Errorf("invalid OpenCode patch directive at patch line %d: %q", index+1, line)
		}
	}
	if !ended {
		return nil, fmt.Errorf("OpenCode patch must end with %q", openCodePatchEnd)
	}
	for ; index < len(lines); index++ {
		if lines[index] != "" {
			return nil, fmt.Errorf("unexpected content after %q at patch line %d", openCodePatchEnd, index+1)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("patch contains no file changes")
	}
	return files, nil
}

func parseOpenCodePatchHunk(lines []string, index int) (openCodePatchHunk, int, error) {
	if !isOpenCodeHunkHeader(lines[index]) {
		return openCodePatchHunk{}, index, fmt.Errorf("invalid update hunk header at patch line %d", index+1)
	}
	hunk := openCodePatchHunk{anchor: strings.TrimSpace(strings.TrimPrefix(lines[index], "@@"))}
	index++
	for index < len(lines) {
		line := lines[index]
		if line == openCodeEndOfFile {
			hunk.endOfFile = true
			index++
			break
		}
		if isOpenCodeHunkHeader(line) || isOpenCodePatchDirective(line) {
			break
		}
		if line == "" || (line[0] != ' ' && line[0] != '+' && line[0] != '-') {
			return openCodePatchHunk{}, index, fmt.Errorf("invalid update hunk line at patch line %d", index+1)
		}
		hunk.lines = append(hunk.lines, openCodePatchLine{kind: line[0], text: line[1:]})
		index++
	}
	if len(hunk.lines) == 0 {
		return openCodePatchHunk{}, index, fmt.Errorf("update hunk at patch line %d has no changes or context", index+1)
	}
	return hunk, index, nil
}

func isOpenCodeHunkHeader(line string) bool {
	return line == "@@" || strings.HasPrefix(line, "@@ ")
}

func isOpenCodePatchDirective(line string) bool {
	return line == openCodePatchBegin ||
		line == openCodePatchEnd ||
		line == openCodeEndOfFile ||
		strings.HasPrefix(line, "*** Add File: ") ||
		strings.HasPrefix(line, "*** Update File: ") ||
		strings.HasPrefix(line, "*** Delete File: ") ||
		strings.HasPrefix(line, "*** Move to: ")
}

func parseOpenCodePatchPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || (len(value) >= 3 && value[1] == ':' && (value[2] == '/' || value[2] == '\\')) {
		return "", fmt.Errorf("path must be a workspace-relative slash path")
	}
	cleaned := pathpkg.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q is outside the workspace", value)
	}
	return cleaned, nil
}

func normalizePatchNewlines(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

func prepareOpenCodePatchOperations(rootDir string, files []openCodePatchFile) ([]patchOperation, error) {
	seen := make(map[string]struct{}, len(files)*2)
	operations := make([]patchOperation, 0, len(files))
	for _, file := range files {
		paths := []string{file.path}
		if file.kind == patchMove {
			paths = append(paths, file.moveTo)
		}
		for _, workspaceRel := range paths {
			if _, exists := seen[workspaceRel]; exists {
				return nil, fmt.Errorf("patch changes %q more than once", workspaceRel)
			}
			seen[workspaceRel] = struct{}{}
		}

		switch file.kind {
		case patchCreate:
			resolved, err := resolveWritableFilePath(rootDir, file.path)
			if err != nil {
				return nil, err
			}
			if _, err := os.Lstat(resolved); err == nil {
				return nil, fmt.Errorf("add file %q: path already exists", file.path)
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("stat new file %q: %w", file.path, err)
			}
			content, err := applyOpenCodeHunks(nil, file.hunks)
			if err != nil {
				return nil, fmt.Errorf("apply patch to %q: %w", file.path, err)
			}
			operations = append(operations, patchOperation{kind: patchCreate, workspaceRel: file.path, resolved: resolved, content: content, mode: 0o644})
		case patchDelete:
			resolved, _, _, err := readPatchSource(rootDir, file.path)
			if err != nil {
				return nil, err
			}
			operations = append(operations, patchOperation{kind: patchDelete, workspaceRel: file.path, resolved: resolved})
		case patchUpdate, patchMove:
			resolved, info, source, err := readPatchSource(rootDir, file.path)
			if err != nil {
				return nil, err
			}
			content, err := applyOpenCodeHunks(source, file.hunks)
			if err != nil {
				return nil, fmt.Errorf("apply patch to %q: %w", file.path, err)
			}
			if file.kind == patchUpdate {
				operations = append(operations, patchOperation{kind: patchUpdate, workspaceRel: file.path, resolved: resolved, content: content, mode: info.Mode().Perm()})
				continue
			}
			targetResolved, err := resolveWritableFilePath(rootDir, file.moveTo)
			if err != nil {
				return nil, err
			}
			if _, err := os.Lstat(targetResolved); err == nil {
				return nil, fmt.Errorf("move destination %q already exists", file.moveTo)
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("stat move destination %q: %w", file.moveTo, err)
			}
			operations = append(operations, patchOperation{kind: patchMove, workspaceRel: file.path, resolved: resolved, targetRel: file.moveTo, targetResolved: targetResolved, content: content, mode: info.Mode().Perm()})
		default:
			return nil, fmt.Errorf("unsupported patch operation for %q", file.path)
		}
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
		case patchMove:
			if err := os.MkdirAll(filepath.Dir(operation.targetResolved), 0o755); err != nil {
				return fmt.Errorf("create parent directories for move destination %q: %w", operation.targetRel, err)
			}
			if err := os.WriteFile(operation.targetResolved, operation.content, operation.mode); err != nil {
				return fmt.Errorf("write move destination %q: %w", operation.targetRel, err)
			}
			if err := os.Remove(operation.resolved); err != nil {
				return fmt.Errorf("remove moved source %q: %w", operation.workspaceRel, err)
			}
		}
	}
	return nil
}

func applyOpenCodeHunks(source []byte, hunks []openCodePatchHunk) ([]byte, error) {
	original, lineEnding, err := splitPatchText(source)
	if err != nil {
		return nil, err
	}
	result := make([]patchTextLine, 0, len(original))
	cursor := 0
	for hunkIndex, hunk := range hunks {
		start, err := findOpenCodeHunkStart(original, hunk, cursor)
		if err != nil {
			return nil, fmt.Errorf("hunk %d: %w", hunkIndex+1, err)
		}
		result, err = appendPatchTextLines(result, original[cursor:start])
		if err != nil {
			return nil, err
		}
		cursor = start
		for _, line := range hunk.lines {
			switch line.kind {
			case ' ':
				if cursor >= len(original) || original[cursor].text != line.text {
					return nil, fmt.Errorf("hunk context does not match source line %d", cursor+1)
				}
				result, err = appendPatchTextLines(result, []patchTextLine{original[cursor]})
				if err != nil {
					return nil, err
				}
				cursor++
			case '-':
				if cursor >= len(original) || original[cursor].text != line.text {
					return nil, fmt.Errorf("hunk removal does not match source line %d", cursor+1)
				}
				cursor++
			case '+':
				result, err = appendPatchTextLines(result, []patchTextLine{{text: line.text}})
				if err != nil {
					return nil, err
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

func findOpenCodeHunkStart(source []patchTextLine, hunk openCodePatchHunk, cursor int) (int, error) {
	oldLineCount := 0
	for _, line := range hunk.lines {
		if line.kind != '+' {
			oldLineCount++
		}
	}
	if hunk.endOfFile {
		start := len(source) - oldLineCount
		if start < cursor || start < 0 || !openCodeHunkMatchesAt(source, hunk, start) {
			return 0, fmt.Errorf("expected hunk content at end of file")
		}
		return start, nil
	}

	searchStart := cursor
	if hunk.anchor != "" {
		anchorIndex := findOpenCodeAnchor(source, hunk.anchor, cursor)
		if anchorIndex < 0 {
			return 0, fmt.Errorf("anchor %q was not found", hunk.anchor)
		}
		searchStart = anchorIndex + 1
	}
	if oldLineCount == 0 {
		if len(source) == 0 && searchStart == 0 {
			return 0, nil
		}
		if hunk.anchor == "" {
			return 0, fmt.Errorf("addition-only hunk needs an @@ anchor or %q", openCodeEndOfFile)
		}
		return searchStart, nil
	}
	for start := searchStart; start+oldLineCount <= len(source); start++ {
		if openCodeHunkMatchesAt(source, hunk, start) {
			return start, nil
		}
	}
	return 0, fmt.Errorf("expected hunk content was not found")
}

func findOpenCodeAnchor(source []patchTextLine, anchor string, start int) int {
	for index := start; index < len(source); index++ {
		if source[index].text == anchor {
			return index
		}
	}
	for index := start; index < len(source); index++ {
		if strings.Contains(source[index].text, anchor) {
			return index
		}
	}
	return -1
}

func openCodeHunkMatchesAt(source []patchTextLine, hunk openCodePatchHunk, start int) bool {
	cursor := start
	for _, line := range hunk.lines {
		if line.kind == '+' {
			continue
		}
		if cursor >= len(source) || source[cursor].text != line.text {
			return false
		}
		cursor++
	}
	return true
}

func splitPatchText(source []byte) ([]patchTextLine, string, error) {
	if bytes.Contains(source, []byte{0}) {
		return nil, "", fmt.Errorf("file appears to be binary")
	}
	lineEnding := "\n"
	if bytes.Contains(source, []byte("\r\n")) {
		lineEnding = "\r\n"
	}
	text := normalizePatchNewlines(string(source))
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
