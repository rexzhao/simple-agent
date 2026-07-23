package tools

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type gitIgnoreMatcher struct {
	rootDir    string
	loadedDirs map[string]bool
	rulesByDir map[string][]gitIgnoreRule
}

type gitIgnoreRule struct {
	pattern       string
	negated       bool
	directoryOnly bool
	anchored      bool
	hasSlash      bool
}

func newGitIgnoreMatcher(rootDir string) *gitIgnoreMatcher {
	return &gitIgnoreMatcher{
		rootDir:    rootDir,
		loadedDirs: make(map[string]bool),
		rulesByDir: make(map[string][]gitIgnoreRule),
	}
}

func (m *gitIgnoreMatcher) ignores(workspaceRel string, isDir bool) (bool, error) {
	workspaceRel = filepath.ToSlash(strings.Trim(workspaceRel, "/"))
	if workspaceRel == "" || workspaceRel == "." {
		return false, nil
	}

	ignored := false
	for _, dir := range gitIgnoreRuleDirectories(workspaceRel) {
		rules, err := m.rulesFor(dir)
		if err != nil {
			return false, err
		}
		pathFromRule, err := slashRel(filepath.Join(m.rootDir, filepath.FromSlash(dir)), filepath.Join(m.rootDir, filepath.FromSlash(workspaceRel)))
		if err != nil {
			return false, err
		}
		for _, rule := range rules {
			matched, err := rule.matches(pathFromRule, isDir)
			if err != nil {
				return false, err
			}
			if matched {
				ignored = !rule.negated
			}
		}
	}
	return ignored, nil
}

func gitIgnoreRuleDirectories(workspaceRel string) []string {
	segments := strings.Split(workspaceRel, "/")
	dirs := []string{""}
	for index := 1; index < len(segments); index++ {
		dirs = append(dirs, strings.Join(segments[:index], "/"))
	}
	return dirs
}

func (m *gitIgnoreMatcher) rulesFor(dir string) ([]gitIgnoreRule, error) {
	dir = filepath.ToSlash(strings.Trim(dir, "/"))
	if m.loadedDirs[dir] {
		return m.rulesByDir[dir], nil
	}
	m.loadedDirs[dir] = true

	ignorePath := filepath.Join(m.rootDir, filepath.FromSlash(dir), ".gitignore")
	data, err := os.ReadFile(ignorePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read gitignore %q: %w", ignorePath, err)
	}
	rules, err := parseGitIgnoreRules(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse gitignore %q: %w", ignorePath, err)
	}
	m.rulesByDir[dir] = rules
	return rules, nil
}

func parseGitIgnoreRules(content string) ([]gitIgnoreRule, error) {
	rules := []gitIgnoreRule{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, `\#`) {
			line = line[1:]
		} else if strings.HasPrefix(line, "#") {
			continue
		}

		negated := false
		if strings.HasPrefix(line, `\!`) {
			line = line[1:]
		} else if strings.HasPrefix(line, "!") {
			negated = true
			line = line[1:]
		}
		if line == "" {
			continue
		}

		directoryOnly := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		anchored := strings.HasPrefix(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		line = filepath.ToSlash(line)
		rule := gitIgnoreRule{
			pattern:       line,
			negated:       negated,
			directoryOnly: directoryOnly,
			anchored:      anchored,
			hasSlash:      strings.Contains(line, "/"),
		}
		if err := rule.validate(); err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func (r gitIgnoreRule) validate() error {
	if r.hasSlash {
		_, err := matchSlashGlob(r.pattern, "")
		return err
	}
	_, err := path.Match(r.pattern, "")
	return err
}

func (r gitIgnoreRule) matches(relative string, isDir bool) (bool, error) {
	relative = filepath.ToSlash(strings.Trim(relative, "/"))
	if relative == "" || relative == "." {
		return false, nil
	}
	segments := strings.Split(relative, "/")
	for end := 1; end <= len(segments); end++ {
		if r.anchored && !r.hasSlash && end != 1 {
			continue
		}
		candidateIsDir := end < len(segments) || isDir
		if r.directoryOnly && !candidateIsDir {
			continue
		}

		var (
			matched bool
			err     error
		)
		if r.hasSlash {
			matched, err = matchSlashGlob(r.pattern, strings.Join(segments[:end], "/"))
		} else {
			matched, err = path.Match(r.pattern, segments[end-1])
		}
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}
