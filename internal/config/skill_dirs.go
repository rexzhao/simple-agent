package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rexzhao/simple-agent/internal/pathresolver"
)

// ResolveSkillDirs expands path placeholders for the current run and resolves
// relative entries from the root configuration directory. Entries that
// reference $REPO are skipped (not an error) when the current directory is not
// inside a repository, so the default layered skill dirs work everywhere.
func (c *Config) ResolveSkillDirs(cwd string) ([]string, error) {
	if c == nil {
		return nil, fmt.Errorf("resolve skill_dirs: config is nil")
	}
	configDir := ""
	if c.ConfigPath != "" {
		configDir = filepath.Dir(c.ConfigPath)
	}
	variables := pathresolver.Variables{
		CWD:       cwd,
		ConfigDir: configDir,
	}
	resolved := make([]string, 0, len(c.SkillDirs))
	for index, value := range c.SkillDirs {
		path, err := pathresolver.Resolve(value, configDir, variables)
		if err != nil {
			if errors.Is(err, pathresolver.ErrRepoRootNotFound) {
				continue
			}
			return nil, fmt.Errorf("resolve skill_dirs path[%d] %q: %w", index, value, err)
		}
		if strings.TrimSpace(path) == "" {
			continue
		}
		resolved = append(resolved, path)
	}
	return resolved, nil
}
