package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 搜索返回上限。
const (
	maxGlobFiles  = 500
	maxGrepHits   = 200
	maxGrepLineCh = 300
)

func globTool() *ToolDef {
	return &ToolDef{
		Name: "glob",
		Description: "Find files, not directories, whose paths match a glob pattern, including hidden " +
			"and ignored files. Returns paths relative to the search root.",
		Guidance: "Use the glob tool — not shell find — to discover files by path pattern.",
		Parameters: schema(map[string]any{
			"pattern": map[string]any{"type": "string",
				"description": `Glob pattern to match file paths against (e.g. "**/*.ts", "src/**/*.test.js").`},
			"path": map[string]any{"type": "string",
				"description": "Directory to search in. Defaults to the session workspace; a relative path resolves against it."},
		}, "pattern"),
		Run: globRun,
	}
}

func globRun(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	pattern, err := args.Str("pattern")
	if err != nil {
		return ToolReturn{}, err
	}
	matcher, err := compileGlob(pattern)
	if err != nil {
		return ToolReturn{}, err
	}

	root := tc.Cwd()
	if raw := args.OptStr("path"); raw != "" {
		if root, err = Resolve(tc, raw); err != nil {
			return ToolReturn{}, err
		}
	}
	info, err := os.Stat(root)
	if err != nil {
		return ToolReturn{}, fmt.Errorf("路径不存在：%s", displayPath(tc, root))
	}

	var matches []string
	if !info.IsDir() {
		// path 直接指向单个文件
		if matcher.MatchString(filepath.Base(root)) {
			matches = append(matches, displayPath(tc, root))
		}
	} else {
		walkFiles(root, func(file string) bool {
			rel, relErr := filepath.Rel(root, file)
			if relErr != nil {
				rel = file
			}
			if matcher.MatchString(filepath.ToSlash(rel)) {
				matches = append(matches, displayPath(tc, file))
			}
			return len(matches) < maxGlobFiles && ctx.Err() == nil
		})
	}

	if err := ctx.Err(); err != nil {
		return ToolReturn{}, err
	}
	if len(matches) == 0 {
		return OutputOf(fmt.Sprintf("没有文件匹配 %q", pattern)), nil
	}
	sortStrings(matches)
	out := strings.Join(matches, "\n")
	if len(matches) >= maxGlobFiles {
		out += fmt.Sprintf("\n…（已达 %d 个文件上限）", maxGlobFiles)
	}
	return OutputOf(out), nil
}

// compileGlob 编译 glob 模式。
//
// 支持 `**`（跨目录）、`*`、`?`、`[...]`；匹配对象是相对路径（用 `/` 分隔）。
// 裸文件名模式（`*.md`）同时匹配根目录下的文件——这与 Rust 版行为一致。
func compileGlob(pattern string) (*regexp.Regexp, error) {
	pattern = filepath.ToSlash(pattern)
	expr, err := globToRegexp(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob 模式非法：%w", err)
	}
	return regexp.Compile(expr)
}

// globToRegexp 把 glob 翻译成正则。
func globToRegexp(pattern string) (string, error) {
	var out strings.Builder
	out.WriteString(`^(?:`)
	i := 0
	for i < len(pattern) {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// `**/` 匹配零个或多个目录；单独的 `**` 匹配任意字符
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					out.WriteString(`(?:[^/]*/)*`)
					i += 3
					continue
				}
				out.WriteString(`.*`)
				i += 2
				continue
			}
			out.WriteString(`[^/]*`)
			i++
		case '?':
			out.WriteString(`[^/]`)
			i++
		case '[':
			end := strings.IndexByte(pattern[i:], ']')
			if end < 0 {
				return "", fmt.Errorf("字符类未闭合：%s", pattern)
			}
			class := pattern[i : i+end+1]
			// `[!abc]` → `[^abc]`
			class = strings.Replace(class, "[!", "[^", 1)
			out.WriteString(class)
			i += end + 1
		default:
			out.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	out.WriteString(`)$`)
	return out.String(), nil
}

func grepTool() *ToolDef {
	return &ToolDef{
		Name:        "grep",
		Description: "Search file contents with a regular expression. Returns matching lines with line numbers, grouped by file.",
		Guidance: "Use the grep tool — not shell grep or rg — to search file contents. " +
			"Use read on a matched file when you need surrounding context.",
		Parameters: schema(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Regular expression to search for."},
			"path": map[string]any{"type": "string",
				"description": "File or directory to search. Defaults to the session workspace; a relative path resolves against it."},
			"include": map[string]any{"type": "string",
				"description": `One glob filter for which files to search (e.g. "*.rs"). Not a list; negation is not supported.`},
		}, "pattern"),
		Run: grepRun,
	}
}

func grepRun(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	pattern, err := args.Str("pattern")
	if err != nil {
		return ToolReturn{}, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ToolReturn{}, fmt.Errorf("正则非法：%w", err)
	}

	root := tc.Cwd()
	if raw := args.OptStr("path"); raw != "" {
		if root, err = Resolve(tc, raw); err != nil {
			return ToolReturn{}, err
		}
	}
	info, err := os.Stat(root)
	if err != nil {
		return ToolReturn{}, fmt.Errorf("路径不存在：%s", displayPath(tc, root))
	}

	var include *regexp.Regexp
	if filter := args.OptStr("include"); filter != "" {
		if include, err = compileGlob(filter); err != nil {
			return ToolReturn{}, err
		}
	}

	total := 0
	var groups []string

	searchFile := func(file string) bool {
		if include != nil && !include.MatchString(filepath.Base(file)) {
			return true
		}
		stat, statErr := os.Stat(file)
		if statErr != nil || !isTextFile(file, stat) {
			return true // 二进制/无权限：跳过
		}
		data, readErr := os.ReadFile(file)
		if readErr != nil {
			return true
		}
		var hits []string
		for index, line := range splitLines(decodeConsole(data)) {
			if !re.MatchString(line) {
				continue
			}
			line = strings.TrimRight(line, " \t\r")
			if runes := []rune(line); len(runes) > maxGrepLineCh {
				line = string(runes[:maxGrepLineCh]) + "…"
			}
			hits = append(hits, fmt.Sprintf("%6d: %s", index+1, line))
			total++
			if total >= maxGrepHits {
				break
			}
		}
		if len(hits) > 0 {
			groups = append(groups, displayPath(tc, file)+"\n"+strings.Join(hits, "\n"))
		}
		return total < maxGrepHits && ctx.Err() == nil
	}

	if info.IsDir() {
		walkFiles(root, searchFile)
	} else {
		searchFile(root)
	}

	if err := ctx.Err(); err != nil {
		return ToolReturn{}, err
	}
	if len(groups) == 0 {
		return OutputOf(fmt.Sprintf("没有匹配 %q 的内容", pattern)), nil
	}
	out := strings.Join(groups, "\n\n")
	if total >= maxGrepHits {
		out += fmt.Sprintf("\n…（已达 %d 条命中上限）", maxGrepHits)
	}
	return OutputOf(out), nil
}

func sortStrings(list []string) { sort.Strings(list) }
