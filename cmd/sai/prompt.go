package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/config"
)

// buildSystemPrompt assembles the system prompt: the built-in instructions
// plus the configured prompt template, rendered with placeholders.
func buildSystemPrompt(cfg *config.Config, cwd string) (string, error) {
	sections := make([]string, 0, 2)
	if base := strings.TrimSpace(builtInInstructions); base != "" {
		sections = append(sections, base)
	}
	if raw := strings.TrimSpace(cfg.Prompt.SystemPrompt); raw != "" {
		rendered, err := renderPrompt(raw, cwd, filepath.Dir(cfg.ConfigPath))
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(rendered) != "" {
			sections = append(sections, rendered)
		}
	}
	return strings.Join(sections, "\n\n"), nil
}

// renderPrompt expands the config prompt template's {{cwd}},
// {{config_dir}}, and {{time.now}} placeholders.
func renderPrompt(template, cwd, configDir string) (string, error) {
	replacements := map[string]string{
		"cwd":        cwd,
		"config_dir": configDir,
		"time.now":   time.Now().UTC().Format(time.RFC3339),
	}
	var rendered strings.Builder
	offset := 0
	for {
		start := strings.Index(template[offset:], "{{")
		if start < 0 {
			rendered.WriteString(template[offset:])
			return rendered.String(), nil
		}
		start += offset
		rendered.WriteString(template[offset:start])
		end := strings.Index(template[start+2:], "}}")
		if end < 0 {
			return "", fmt.Errorf("prompt placeholder starting at byte %d is missing closing }}", start)
		}
		end += start + 2
		name := strings.TrimSpace(template[start+2 : end])
		replacement, ok := replacements[name]
		if !ok {
			return "", fmt.Errorf("unknown prompt placeholder %q; supported placeholders: config_dir, cwd, time.now", name)
		}
		rendered.WriteString(replacement)
		offset = end + 2
	}
}

const builtInInstructions = `You are a capable software agent working in the user's project directory.

- Your only tool is the shell: a Bash environment whose working directory is the project. Read, search, and edit files, run builds, tests, and git through shell commands.
- Inspect before you change: read the relevant code first, verify your edits afterwards.
- Keep answers concise; show file paths when you change them.`
