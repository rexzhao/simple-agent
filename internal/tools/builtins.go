package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	BuiltinListFiles = "list_files"
	BuiltinReadFile  = "read_file"
	BuiltinGlobFiles = "glob_files"
	BuiltinGrepFiles = "grep_files"
	BuiltinWriteFile = "write_file"
	BuiltinEditFile  = "edit_file"
	BuiltinShell     = "shell"
)

const shellCancelWaitDelay = 2 * time.Second

// shellCommandController owns the platform-specific process-group or process-
// tree lifecycle for one shell command. Run starts and waits for the command;
// Close is idempotent and removes any remaining descendants after completion.
type shellCommandController interface {
	Run(*exec.Cmd) error
	Close()
}

const (
	defaultReadFileMaxBytes    = 50 * 1024
	defaultGlobFilesMaxResults = 200
	defaultGrepFilesMaxResults = 100
	defaultGrepSnippetMaxBytes = 200
)

func RegisterBuiltins(registry *Registry, rootDir string) error {
	if registry == nil {
		return fmt.Errorf("registry must not be nil")
	}
	if strings.TrimSpace(rootDir) == "" {
		return fmt.Errorf("rootDir must not be blank")
	}

	root, err := filepath.Abs(rootDir)
	if err != nil {
		return fmt.Errorf("resolve rootDir: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat rootDir %q: %w", rootDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("rootDir %q must be a directory", rootDir)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve rootDir %q: %w", rootDir, err)
	}

	builtins := []Entry{
		{
			Definition: listFilesDefinition(),
			Executor:   newListFilesExecutor(canonicalRoot),
		},
		{
			Definition: readFileDefinition(),
			Executor:   newReadFileExecutor(canonicalRoot),
		},
		{
			Definition: globFilesDefinition(),
			Executor:   newGlobFilesExecutor(canonicalRoot),
		},
		{
			Definition: grepFilesDefinition(),
			Executor:   newGrepFilesExecutor(canonicalRoot),
		},
		{
			Definition: writeFileDefinition(),
			Executor:   newWriteFileExecutor(canonicalRoot),
		},
		{
			Definition: editFileDefinition(),
			Executor:   newEditFileExecutor(canonicalRoot),
		},
		{
			Definition: shellDefinition(),
			Executor:   newShellExecutor(root),
		},
	}
	for _, builtin := range builtins {
		if err := registry.Register(builtin.Definition, builtin.Executor); err != nil {
			return err
		}
	}
	return nil
}

func listFilesDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinListFiles,
		Description: "List files and directories under the workspace.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path relative to the workspace. Defaults to the workspace root.",
				},
			},
			"additionalProperties": false,
		},
	}
}

func readFileDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinReadFile,
		Description: "Read a text file under the workspace.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the workspace.",
				},
				"start_line": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional 1-based line number to start reading from.",
				},
				"line_count": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional positive number of lines to return, still constrained by max_bytes.",
				},
				"max_bytes": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": fmt.Sprintf("Optional positive byte cap for returned content. Defaults to %d bytes.", defaultReadFileMaxBytes),
				},
			},
			"required":             []any{"path"},
			"additionalProperties": false,
		},
	}
}

func globFilesDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinGlobFiles,
		Description: "Find workspace files by slash-style glob pattern.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path relative to the workspace to search from. Defaults to the workspace root.",
				},
				"pattern": map[string]any{
					"type":        "string",
					"description": "Slash-style glob relative to path. Supports ** for recursive matching.",
				},
				"include_dirs": map[string]any{
					"type":        "boolean",
					"description": "Include matching directories as well as files. Defaults to false.",
				},
				"include_hidden": map[string]any{
					"type":        "boolean",
					"description": "Include paths with dot-prefixed segments. Defaults to false.",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": fmt.Sprintf("Maximum number of paths to return. Defaults to %d.", defaultGlobFilesMaxResults),
				},
			},
			"required":             []any{"pattern"},
			"additionalProperties": false,
		},
	}
}

func grepFilesDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinGrepFiles,
		Description: "Search workspace text files and return path:line:snippet matches.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path relative to the workspace to search from. Defaults to the workspace root.",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "Text or regular expression to search for.",
				},
				"include": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional slash-style glob patterns to include, relative to path.",
				},
				"exclude": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional slash-style glob patterns to exclude, relative to path.",
				},
				"regex": map[string]any{
					"type":        "boolean",
					"description": "Treat query as a Go regular expression. Defaults to false for literal search.",
				},
				"case_sensitive": map[string]any{
					"type":        "boolean",
					"description": "Use case-sensitive matching. Defaults to false.",
				},
				"context_lines": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "Number of lines before and after each match to include. Defaults to 0.",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": fmt.Sprintf("Maximum number of matching lines to return. Defaults to %d.", defaultGrepFilesMaxResults),
				},
				"max_snippet_bytes": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": fmt.Sprintf("Maximum bytes per returned snippet. Defaults to %d.", defaultGrepSnippetMaxBytes),
				},
			},
			"required":             []any{"query"},
			"additionalProperties": false,
		},
	}
}

func shellDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinShell,
		Description: "Run a shell command in the workspace.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Command to run.",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional positive command timeout in milliseconds.",
				},
				"max_output_bytes": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional positive cap for combined stdout/stderr bytes retained and returned.",
				},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	}
}

func writeFileDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinWriteFile,
		Description: "Write a text file under the workspace, creating parent directories if needed.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the workspace.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "File content to write.",
				},
			},
			"required":             []any{"path", "content"},
			"additionalProperties": false,
		},
	}
}

func editFileDefinition() model.Tool {
	return model.Tool{
		Name:        BuiltinEditFile,
		Description: "Edit a text file under the workspace by replacing one exact text match.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the workspace.",
				},
				"old": map[string]any{
					"type":        "string",
					"description": "Existing text to replace. It must appear exactly once.",
				},
				"new": map[string]any{
					"type":        "string",
					"description": "Replacement text.",
				},
			},
			"required":             []any{"path", "old", "new"},
			"additionalProperties": false,
		},
	}
}

func newListFilesExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		path, err := optionalStringArgument(arguments, "path", ".")
		if err != nil {
			return model.ToolResult{}, err
		}
		resolved, err := resolveRootPath(rootDir, path)
		if err != nil {
			return model.ToolResult{}, err
		}

		entries, err := os.ReadDir(resolved)
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("list files %q: %w", path, err)
		}

		lines := make([]string, 0, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}
			lines = append(lines, name)
		}
		sort.Strings(lines)

		return model.ToolResult{
			Name:    BuiltinListFiles,
			Content: strings.Join(lines, "\n"),
		}, nil
	})
}

func newReadFileExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		path, err := requiredStringArgument(arguments, "path")
		if err != nil {
			return model.ToolResult{}, err
		}
		startLine, startLineSet, err := optionalPositiveIntArgument(arguments, "start_line", 1)
		if err != nil {
			return model.ToolResult{}, err
		}
		lineCount, lineCountSet, err := optionalPositiveIntPointerArgument(arguments, "line_count")
		if err != nil {
			return model.ToolResult{}, err
		}
		maxBytes, err := optionalPositiveIntArgumentDefault(arguments, "max_bytes", defaultReadFileMaxBytes)
		if err != nil {
			return model.ToolResult{}, err
		}
		resolved, err := resolveRootPath(rootDir, path)
		if err != nil {
			return model.ToolResult{}, err
		}

		data, err := os.ReadFile(resolved)
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("read file %q: %w", path, err)
		}
		if bytes.Contains(data, []byte{0}) {
			return model.ToolResult{}, fmt.Errorf("read file %q: file appears to be binary", path)
		}

		if !startLineSet && !lineCountSet && len(data) <= maxBytes {
			return model.ToolResult{
				Name:    BuiltinReadFile,
				Content: string(data),
			}, nil
		}

		content := readFileContent(path, data, startLine, lineCount, maxBytes, startLineSet || lineCountSet)
		return model.ToolResult{
			Name:    BuiltinReadFile,
			Content: content,
		}, nil
	})
}

func newGlobFilesExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		searchPath, err := optionalStringArgument(arguments, "path", ".")
		if err != nil {
			return model.ToolResult{}, err
		}
		pattern, err := requiredStringArgument(arguments, "pattern")
		if err != nil {
			return model.ToolResult{}, err
		}
		includeDirs, err := optionalBoolArgument(arguments, "include_dirs", false)
		if err != nil {
			return model.ToolResult{}, err
		}
		includeHidden, err := optionalBoolArgument(arguments, "include_hidden", false)
		if err != nil {
			return model.ToolResult{}, err
		}
		maxResults, err := optionalPositiveIntArgumentDefault(arguments, "max_results", defaultGlobFilesMaxResults)
		if err != nil {
			return model.ToolResult{}, err
		}
		normalizedPattern, err := normalizeSlashPattern(pattern)
		if err != nil {
			return model.ToolResult{}, err
		}

		searchRoot, err := resolveRootPath(rootDir, searchPath)
		if err != nil {
			return model.ToolResult{}, err
		}
		info, err := os.Stat(searchRoot)
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("stat path %q: %w", searchPath, err)
		}
		if !info.IsDir() {
			return model.ToolResult{}, fmt.Errorf("path %q is not a directory", searchPath)
		}

		results := []string{}
		gitIgnore := newGitIgnoreMatcher(rootDir)
		err = filepath.WalkDir(searchRoot, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}

			workspaceRel, err := slashRel(rootDir, current)
			if err != nil {
				return err
			}
			if workspaceRel == "." {
				return nil
			}
			ignored, err := gitIgnore.ignores(workspaceRel, entry.IsDir())
			if err != nil {
				return err
			}
			if ignored {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !includeHidden && hasHiddenPathSegment(workspaceRel) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() && !includeDirs {
				return nil
			}

			searchRel, err := slashRel(searchRoot, current)
			if err != nil {
				return err
			}
			matched, err := matchSlashGlob(normalizedPattern, searchRel)
			if err != nil {
				return err
			}
			if matched {
				results = append(results, workspaceRel)
			}
			return nil
		})
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("glob files %q: %w", searchPath, err)
		}

		sort.Strings(results)
		truncated := false
		if len(results) > maxResults {
			results = results[:maxResults]
			truncated = true
		}
		content := strings.Join(results, "\n")
		if truncated {
			content = strings.Join([]string{
				"truncated=true",
				fmt.Sprintf("max_results=%d", maxResults),
				fmt.Sprintf("returned_results=%d", len(results)),
				"next_step=Use a narrower path or pattern, or increase max_results.",
				"",
				content,
			}, "\n")
		}
		return model.ToolResult{
			Name:    BuiltinGlobFiles,
			Content: content,
		}, nil
	})
}

func newGrepFilesExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		searchPath, err := optionalStringArgument(arguments, "path", ".")
		if err != nil {
			return model.ToolResult{}, err
		}
		query, err := requiredStringArgument(arguments, "query")
		if err != nil {
			return model.ToolResult{}, err
		}
		include, err := optionalStringListArgument(arguments, "include")
		if err != nil {
			return model.ToolResult{}, err
		}
		exclude, err := optionalStringListArgument(arguments, "exclude")
		if err != nil {
			return model.ToolResult{}, err
		}
		include, err = normalizeSlashPatterns(include)
		if err != nil {
			return model.ToolResult{}, err
		}
		exclude, err = normalizeSlashPatterns(exclude)
		if err != nil {
			return model.ToolResult{}, err
		}
		useRegex, err := optionalBoolArgument(arguments, "regex", false)
		if err != nil {
			return model.ToolResult{}, err
		}
		caseSensitive, err := optionalBoolArgument(arguments, "case_sensitive", false)
		if err != nil {
			return model.ToolResult{}, err
		}
		contextLines, err := optionalNonNegativeIntArgument(arguments, "context_lines", 0)
		if err != nil {
			return model.ToolResult{}, err
		}
		maxResults, err := optionalPositiveIntArgumentDefault(arguments, "max_results", defaultGrepFilesMaxResults)
		if err != nil {
			return model.ToolResult{}, err
		}
		maxSnippetBytes, err := optionalPositiveIntArgumentDefault(arguments, "max_snippet_bytes", defaultGrepSnippetMaxBytes)
		if err != nil {
			return model.ToolResult{}, err
		}
		matcher, err := newTextMatcher(query, useRegex, caseSensitive)
		if err != nil {
			return model.ToolResult{}, err
		}

		searchRoot, err := resolveRootPath(rootDir, searchPath)
		if err != nil {
			return model.ToolResult{}, err
		}
		info, err := os.Stat(searchRoot)
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("stat path %q: %w", searchPath, err)
		}
		if !info.IsDir() {
			return model.ToolResult{}, fmt.Errorf("path %q is not a directory", searchPath)
		}

		gitIgnore := newGitIgnoreMatcher(rootDir)
		files, err := grepCandidateFiles(ctx, rootDir, searchRoot, include, exclude, gitIgnore)
		if err != nil {
			return model.ToolResult{}, err
		}

		lines := []string{}
		resultCount := 0
		resultTruncated := false
		snippetTruncated := false
		for _, file := range files {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return model.ToolResult{}, ctxErr
			}
			data, err := os.ReadFile(file.absolute)
			if err != nil {
				return model.ToolResult{}, fmt.Errorf("read file %q: %w", file.workspaceRel, err)
			}
			if bytes.Contains(data, []byte{0}) {
				continue
			}
			fileLines := splitTextLines(string(data))
			for i, line := range fileLines {
				if !matcher.matches(line) {
					continue
				}
				if resultCount >= maxResults {
					resultTruncated = true
					break
				}
				start := i - contextLines
				if start < 0 {
					start = 0
				}
				end := i + contextLines
				if end >= len(fileLines) {
					end = len(fileLines) - 1
				}
				for j := start; j <= end; j++ {
					formatted, truncated := formatGrepResultLine(file.workspaceRel, j+1, fileLines[j], maxSnippetBytes)
					if truncated {
						snippetTruncated = true
					}
					lines = append(lines, formatted)
				}
				resultCount++
			}
			if resultTruncated {
				break
			}
		}

		content := strings.Join(lines, "\n")
		if resultTruncated || snippetTruncated {
			metadata := []string{}
			if resultTruncated {
				metadata = append(metadata,
					"truncated=true",
					fmt.Sprintf("max_results=%d", maxResults),
					fmt.Sprintf("returned_results=%d", resultCount),
					"next_step=Use a narrower path/include glob, or increase max_results.",
				)
			}
			if snippetTruncated {
				metadata = append(metadata,
					"snippet_truncated=true",
					fmt.Sprintf("max_snippet_bytes=%d", maxSnippetBytes),
					"snippet_next_step=Increase max_snippet_bytes for longer line snippets.",
				)
			}
			metadata = append(metadata, "")
			metadata = append(metadata, content)
			content = strings.Join(metadata, "\n")
		}
		return model.ToolResult{
			Name:    BuiltinGrepFiles,
			Content: content,
		}, nil
	})
}

func newWriteFileExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		path, err := requiredStringArgument(arguments, "path")
		if err != nil {
			return model.ToolResult{}, err
		}
		content, err := requiredStringArgumentAllowEmpty(arguments, "content")
		if err != nil {
			return model.ToolResult{}, err
		}
		resolved, err := resolveWritableFilePath(rootDir, path)
		if err != nil {
			return model.ToolResult{}, err
		}
		if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
			return model.ToolResult{}, fmt.Errorf("create parent directories for %q: %w", path, err)
		}
		data := []byte(content)
		if err := os.WriteFile(resolved, data, 0o644); err != nil {
			return model.ToolResult{}, fmt.Errorf("write file %q: %w", path, err)
		}
		return model.ToolResult{
			Name:    BuiltinWriteFile,
			Content: fmt.Sprintf("wrote %s (%d bytes)", path, len(data)),
		}, nil
	})
}

func newEditFileExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		path, err := requiredStringArgument(arguments, "path")
		if err != nil {
			return model.ToolResult{}, err
		}
		oldText, err := requiredNonEmptyStringArgument(arguments, "old")
		if err != nil {
			return model.ToolResult{}, err
		}
		newText, err := requiredStringArgumentAllowEmpty(arguments, "new")
		if err != nil {
			return model.ToolResult{}, err
		}
		resolved, err := resolveRootPath(rootDir, path)
		if err != nil {
			return model.ToolResult{}, err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("stat file %q: %w", path, err)
		}
		if info.IsDir() {
			return model.ToolResult{}, fmt.Errorf("path %q is a directory", path)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return model.ToolResult{}, fmt.Errorf("read file %q: %w", path, err)
		}
		content := string(data)
		matches := strings.Count(content, oldText)
		if matches == 0 {
			return model.ToolResult{}, fmt.Errorf("edit file %q: old text not found", path)
		}
		if matches > 1 {
			return model.ToolResult{}, fmt.Errorf("edit file %q: old text matched %d times", path, matches)
		}
		updated := strings.Replace(content, oldText, newText, 1)
		if err := os.WriteFile(resolved, []byte(updated), 0o644); err != nil {
			return model.ToolResult{}, fmt.Errorf("write file %q: %w", path, err)
		}
		return model.ToolResult{
			Name:    BuiltinEditFile,
			Content: fmt.Sprintf("edited %s (1 replacement)", path),
		}, nil
	})
}

func newShellExecutor(rootDir string) Executor {
	return ExecutorFunc(func(ctx context.Context, arguments map[string]any) (model.ToolResult, error) {
		command, err := requiredStringArgument(arguments, "command")
		if err != nil {
			return model.ToolResult{}, err
		}
		timeoutMS, timeoutSet, err := optionalPositiveIntArgument(arguments, "timeout_ms", 0)
		if err != nil {
			return model.ToolResult{}, err
		}
		maxOutputBytes, maxOutputSet, err := optionalPositiveIntArgument(arguments, "max_output_bytes", 0)
		if err != nil {
			return model.ToolResult{}, err
		}

		commandCtx := ctx
		if timeoutSet {
			var cancel context.CancelFunc
			commandCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
			defer cancel()
		}

		cmd := newShellCommand(commandCtx, command)
		cmd.Dir = rootDir
		controller := newShellCommandController(cmd)
		defer controller.Close()

		var (
			content         string
			outputTruncated bool
		)
		if maxOutputSet {
			// Do not use CombinedOutput here: it buffers the complete command
			// output before we can truncate it. limitedOutput always reports a
			// successful full write to os/exec so both pipes keep draining, while
			// retaining at most maxOutputBytes in process memory.
			output := newLimitedOutput(maxOutputBytes)
			cmd.Stdout = output
			cmd.Stderr = output
			err = controller.Run(cmd)
			content = output.String()
			outputTruncated = output.Truncated()
		} else {
			// This matches exec.Cmd.CombinedOutput while allowing the platform
			// controller to attach process-group/process-tree cleanup after Start.
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			err = controller.Run(cmd)
			content = output.String()
		}
		result := model.ToolResult{
			Name:    BuiltinShell,
			Content: content,
		}
		if err != nil {
			result.IsError = true
			result.Content = shellErrorContent(commandCtx, result.Content, err)
		}
		if outputTruncated {
			result.Content = strings.Join([]string{
				"truncated=true",
				fmt.Sprintf("max_output_bytes=%d", maxOutputBytes),
				"next_step=Increase max_output_bytes and rerun the command if more output is needed.",
				"",
				result.Content,
			}, "\n")
		}
		return result, nil
	})
}

// limitedOutput is an io.Writer for command stdout/stderr. It acknowledges
// every write so os/exec continues draining child pipes, but retains only the
// configured prefix. This bounds the agent process memory used by a shell
// command even when that command produces unbounded output.
type limitedOutput struct {
	mu        sync.Mutex
	limit     int
	data      []byte
	truncated bool
}

func newLimitedOutput(limit int) *limitedOutput {
	return &limitedOutput{limit: limit}
}

func (o *limitedOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	remaining := o.limit - len(o.data)
	if remaining <= 0 {
		if len(data) > 0 {
			o.truncated = true
		}
		return len(data), nil
	}
	if len(data) > remaining {
		o.data = append(o.data, data[:remaining]...)
		o.truncated = true
		return len(data), nil
	}
	o.data = append(o.data, data...)
	return len(data), nil
}

func (o *limitedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.data)
}

func (o *limitedOutput) Truncated() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.truncated
}

func shellErrorContent(ctx context.Context, output string, err error) string {
	if ctxErr := ctx.Err(); ctxErr != nil {
		message := "shell command canceled"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			message = "shell command timed out"
		}
		message = fmt.Sprintf("%s: %v", message, ctxErr)
		if strings.TrimSpace(output) == "" {
			return message
		}
		return message + "\n" + output
	}
	if output == "" {
		return err.Error()
	}
	return output
}

func readFileContent(filePath string, data []byte, startLine int, lineCount *int, maxBytes int, includeMetadata bool) string {
	lines := splitFileLines(data)
	startIndex := startLine - 1
	if startIndex >= len(lines) {
		return formatReadFileContent(filePath, startLine, 0, maxBytes, false, false, 0, "", includeMetadata)
	}

	endIndex := len(lines)
	if lineCount != nil && startIndex+*lineCount < endIndex {
		endIndex = startIndex + *lineCount
	}

	var out []byte
	truncated := false
	lineTruncated := false
	nextLine := 0
	linesReturned := 0
	for i := startIndex; i < endIndex; i++ {
		line := lines[i]
		if len(out)+len(line) <= maxBytes {
			out = append(out, line...)
			linesReturned++
			continue
		}
		truncated = true
		nextLine = i + 1
		if len(out) == 0 {
			out = append(out, line[:maxBytes]...)
			lineTruncated = true
			linesReturned = 1
		}
		break
	}

	return formatReadFileContent(filePath, startLine, linesReturned, maxBytes, truncated, lineTruncated, nextLine, string(out), includeMetadata)
}

func formatReadFileContent(filePath string, startLine int, linesReturned int, maxBytes int, truncated bool, lineTruncated bool, nextLine int, content string, includeMetadata bool) string {
	if !includeMetadata && !truncated {
		return content
	}

	metadata := []string{
		fmt.Sprintf("path=%s", filePath),
		fmt.Sprintf("start_line=%d", startLine),
		fmt.Sprintf("lines_returned=%d", linesReturned),
		fmt.Sprintf("max_bytes=%d", maxBytes),
		fmt.Sprintf("truncated=%t", truncated),
	}
	if truncated {
		if lineTruncated {
			metadata = append(metadata,
				"line_truncated=true",
				fmt.Sprintf("retry_from_line=%d", nextLine),
				fmt.Sprintf("next_step=Increase max_bytes and retry from start_line=%d.", nextLine),
			)
		} else {
			metadata = append(metadata,
				fmt.Sprintf("next_start_line=%d", nextLine),
				fmt.Sprintf("next_step=Continue reading with start_line=%d.", nextLine),
			)
		}
	}
	metadata = append(metadata, "")
	metadata = append(metadata, content)
	return strings.Join(metadata, "\n")
}

func splitFileLines(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func normalizeSlashPattern(pattern string) (string, error) {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	pattern = strings.TrimPrefix(pattern, "./")
	pattern = strings.Trim(pattern, "/")
	if pattern == "" {
		return "", fmt.Errorf("pattern must not be blank")
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "**" {
			continue
		}
		if _, err := path.Match(segment, ""); err != nil {
			return "", fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
		}
	}
	return pattern, nil
}

func normalizeSlashPatterns(patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		next, err := normalizeSlashPattern(pattern)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, next)
	}
	return normalized, nil
}

func matchSlashGlob(pattern string, name string) (bool, error) {
	name = filepath.ToSlash(strings.Trim(name, "/"))
	if name == "." {
		name = ""
	}
	patternSegments := strings.Split(pattern, "/")
	nameSegments := []string{}
	if name != "" {
		nameSegments = strings.Split(name, "/")
	}

	type state struct {
		patternIndex int
		nameIndex    int
	}
	memo := map[state]bool{}
	var match func(int, int) (bool, error)
	match = func(patternIndex, nameIndex int) (bool, error) {
		key := state{patternIndex: patternIndex, nameIndex: nameIndex}
		if value, ok := memo[key]; ok {
			return value, nil
		}

		var result bool
		if patternIndex == len(patternSegments) {
			result = nameIndex == len(nameSegments)
			memo[key] = result
			return result, nil
		}
		segment := patternSegments[patternIndex]
		if segment == "**" {
			zero, err := match(patternIndex+1, nameIndex)
			if err != nil || zero {
				memo[key] = zero
				return zero, err
			}
			if nameIndex < len(nameSegments) {
				consumed, err := match(patternIndex, nameIndex+1)
				memo[key] = consumed
				return consumed, err
			}
			memo[key] = false
			return false, nil
		}
		if nameIndex >= len(nameSegments) {
			memo[key] = false
			return false, nil
		}
		matched, err := path.Match(segment, nameSegments[nameIndex])
		if err != nil || !matched {
			memo[key] = false
			return false, err
		}
		result, err = match(patternIndex+1, nameIndex+1)
		memo[key] = result
		return result, err
	}
	return match(0, 0)
}

func hasHiddenPathSegment(rel string) bool {
	for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.HasPrefix(segment, ".") && segment != "." && segment != ".." {
			return true
		}
	}
	return false
}

func slashRel(base, target string) (string, error) {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

type grepCandidateFile struct {
	absolute     string
	workspaceRel string
}

func grepCandidateFiles(ctx context.Context, rootDir, searchRoot string, include, exclude []string, gitIgnore *gitIgnoreMatcher) ([]grepCandidateFile, error) {
	files := []grepCandidateFile{}
	err := filepath.WalkDir(searchRoot, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}

		workspaceRel, err := slashRel(rootDir, current)
		if err != nil {
			return err
		}
		ignored, err := gitIgnore.ignores(workspaceRel, entry.IsDir())
		if err != nil {
			return err
		}
		if ignored {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}

		searchRel, err := slashRel(searchRoot, current)
		if err != nil {
			return err
		}
		if len(include) > 0 {
			matched, err := matchAnySlashGlob(include, searchRel)
			if err != nil || !matched {
				return err
			}
		}
		if len(exclude) > 0 {
			matched, err := matchAnySlashGlob(exclude, searchRel)
			if err != nil || matched {
				return err
			}
		}

		files = append(files, grepCandidateFile{
			absolute:     current,
			workspaceRel: workspaceRel,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("grep files: %w", err)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].workspaceRel < files[j].workspaceRel
	})
	return files, nil
}

func matchAnySlashGlob(patterns []string, name string) (bool, error) {
	for _, pattern := range patterns {
		matched, err := matchSlashGlob(pattern, name)
		if err != nil || matched {
			return matched, err
		}
	}
	return false, nil
}

type textMatcher struct {
	query         string
	literalQuery  string
	caseSensitive bool
	regex         *regexp.Regexp
}

func newTextMatcher(query string, useRegex bool, caseSensitive bool) (textMatcher, error) {
	if useRegex {
		pattern := query
		if !caseSensitive {
			pattern = "(?i:" + query + ")"
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return textMatcher{}, fmt.Errorf("invalid regex query: %w", err)
		}
		return textMatcher{regex: compiled}, nil
	}
	literalQuery := query
	if !caseSensitive {
		literalQuery = strings.ToLower(query)
	}
	return textMatcher{
		query:         query,
		literalQuery:  literalQuery,
		caseSensitive: caseSensitive,
	}, nil
}

func (m textMatcher) matches(line string) bool {
	if m.regex != nil {
		return m.regex.MatchString(line)
	}
	if m.caseSensitive {
		return strings.Contains(line, m.query)
	}
	return strings.Contains(strings.ToLower(line), m.literalQuery)
}

func splitTextLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

func formatGrepResultLine(path string, lineNumber int, line string, maxSnippetBytes int) (string, bool) {
	snippet, truncated := truncateBytes(line, maxSnippetBytes)
	return fmt.Sprintf("%s:%d:%s", path, lineNumber, snippet), truncated
}

func truncateBytes(text string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len([]byte(text)) <= maxBytes {
		return text, false
	}
	return string([]byte(text)[:maxBytes]), true
}

func optionalStringArgument(arguments map[string]any, name string, defaultValue string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return defaultValue, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must not be blank", name)
	}
	return text, nil
}

func optionalStringListArgument(arguments map[string]any, name string) ([]string, error) {
	value, ok := arguments[name]
	if !ok {
		return nil, nil
	}
	switch value := value.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s must not contain blank patterns", name)
		}
		return []string{value}, nil
	case []string:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if strings.TrimSpace(item) == "" {
				return nil, fmt.Errorf("%s must not contain blank patterns", name)
			}
			out = append(out, item)
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a string or array of strings", name)
			}
			if strings.TrimSpace(text) == "" {
				return nil, fmt.Errorf("%s must not contain blank patterns", name)
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a string or array of strings", name)
	}
}

func optionalBoolArgument(arguments map[string]any, name string, defaultValue bool) (bool, error) {
	value, ok := arguments[name]
	if !ok {
		return defaultValue, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return flag, nil
}

func optionalPositiveIntArgumentDefault(arguments map[string]any, name string, defaultValue int) (int, error) {
	value, _, err := optionalPositiveIntArgument(arguments, name, defaultValue)
	return value, err
}

func optionalPositiveIntPointerArgument(arguments map[string]any, name string) (*int, bool, error) {
	value, ok, err := optionalPositiveIntArgument(arguments, name, 0)
	if err != nil || !ok {
		return nil, ok, err
	}
	return &value, true, nil
}

func optionalPositiveIntArgument(arguments map[string]any, name string, defaultValue int) (int, bool, error) {
	value, ok := arguments[name]
	if !ok {
		return defaultValue, false, nil
	}
	parsed, err := integerArgumentValue(value, name)
	if err != nil {
		return 0, true, err
	}
	if parsed <= 0 {
		return 0, true, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, true, nil
}

func optionalNonNegativeIntArgument(arguments map[string]any, name string, defaultValue int) (int, error) {
	value, ok := arguments[name]
	if !ok {
		return defaultValue, nil
	}
	parsed, err := integerArgumentValue(value, name)
	if err != nil {
		return 0, err
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}

func integerArgumentValue(value any, name string) (int, error) {
	var parsed int64
	switch value := value.(type) {
	case int:
		parsed = int64(value)
	case int64:
		parsed = value
	case float64:
		if math.Trunc(value) != value {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		maxInt, minInt := intBounds()
		if value > float64(maxInt) || value < float64(minInt) {
			return 0, fmt.Errorf("%s is outside integer range", name)
		}
		parsed = int64(value)
	case json.Number:
		next, err := value.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		parsed = next
	default:
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	maxInt, minInt := intBounds()
	if parsed > maxInt || parsed < minInt {
		return 0, fmt.Errorf("%s is outside integer range", name)
	}
	return int(parsed), nil
}

func intBounds() (int64, int64) {
	maxInt := int64(^uint(0) >> 1)
	return maxInt, -maxInt - 1
}

func requiredStringArgument(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must not be blank", name)
	}
	return text, nil
}

func requiredStringArgumentAllowEmpty(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return text, nil
}

func requiredNonEmptyStringArgument(arguments map[string]any, name string) (string, error) {
	text, err := requiredStringArgumentAllowEmpty(arguments, name)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", fmt.Errorf("%s must not be empty", name)
	}
	return text, nil
}

func resolveRootPath(rootDir string, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path must not be blank")
	}

	root, err := filepath.Abs(rootDir)
	if err != nil {
		return "", fmt.Errorf("resolve rootDir: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve rootDir: %w", err)
	}

	var target string
	if filepath.IsAbs(path) {
		target = filepath.Clean(path)
	} else {
		target = filepath.Join(canonicalRoot, path)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}

	relative, err := filepath.Rel(canonicalRoot, canonicalTarget)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	if relative == "." {
		return canonicalTarget, nil
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path %q is outside rootDir", path)
	}
	return canonicalTarget, nil
}

func resolveWritableFilePath(rootDir string, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path must not be blank")
	}

	root, err := filepath.Abs(rootDir)
	if err != nil {
		return "", fmt.Errorf("resolve rootDir: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve rootDir: %w", err)
	}

	var target string
	if filepath.IsAbs(path) {
		target = filepath.Clean(path)
	} else {
		target = filepath.Join(canonicalRoot, path)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	if err := ensurePathInsideRoot(canonicalRoot, target, path); err != nil {
		return "", err
	}

	if _, err := os.Lstat(target); err == nil {
		canonicalTarget, err := filepath.EvalSymlinks(target)
		if err != nil {
			return "", fmt.Errorf("resolve path %q: %w", path, err)
		}
		if err := ensurePathInsideRoot(canonicalRoot, canonicalTarget, path); err != nil {
			return "", err
		}
		info, err := os.Stat(canonicalTarget)
		if err != nil {
			return "", fmt.Errorf("stat file %q: %w", path, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("path %q is a directory", path)
		}
		return target, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat path %q: %w", path, err)
	}

	ancestor := filepath.Dir(target)
	for {
		info, err := os.Stat(ancestor)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("parent path %q is not a directory", ancestor)
			}
			canonicalAncestor, err := filepath.EvalSymlinks(ancestor)
			if err != nil {
				return "", fmt.Errorf("resolve parent path %q: %w", path, err)
			}
			if err := ensurePathInsideRoot(canonicalRoot, canonicalAncestor, path); err != nil {
				return "", err
			}
			return target, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat parent path %q: %w", path, err)
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return "", fmt.Errorf("resolve parent path %q: no existing parent directory", path)
		}
		ancestor = next
	}
}

func ensurePathInsideRoot(canonicalRoot, target, originalPath string) error {
	relative, err := filepath.Rel(canonicalRoot, target)
	if err != nil {
		return fmt.Errorf("resolve path %q: %w", originalPath, err)
	}
	if relative == "." {
		return nil
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("path %q is outside rootDir", originalPath)
	}
	return nil
}
