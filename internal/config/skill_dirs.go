package config

import (
	"fmt"
	"path/filepath"

	"github.com/rexzhao/simple-agent/internal/pathresolver"
)

// ResolveSkillDirs expands path placeholders for the current run and resolves
// relative entries from the root configuration directory.
func (c *Config) ResolveSkillDirs(cwd string) ([]string, error) {
	if c == nil {
		return nil, fmt.Errorf("resolve skill_dirs: config is nil")
	}
	configDir := ""
	if c.ConfigPath != "" {
		configDir = filepath.Dir(c.ConfigPath)
	}
	resolved, err := pathresolver.ResolveAll(c.SkillDirs, configDir, pathresolver.Variables{
		CWD:       cwd,
		ConfigDir: configDir,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve skill_dirs: %w", err)
	}
	return resolved, nil
}
