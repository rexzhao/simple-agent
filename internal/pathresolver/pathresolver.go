package pathresolver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrRepoRootNotFound reports that a $REPO placeholder could not be resolved
// because no repository root (.git) was found from the current directory.
// Callers may treat it as a signal to skip the entry rather than fail.
var ErrRepoRootNotFound = errors.New("repository root not found")

// Variables supplies the roots used by path placeholders. Empty HomeDir and
// RepoDir values are resolved lazily when their placeholders are present.
type Variables struct {
	CWD       string
	ConfigDir string
	HomeDir   string
	RepoDir   string
}

// Expand replaces supported placeholders without otherwise changing the path.
// $USER remains an alias for $HOME for compatibility with instruction paths.
func Expand(value string, variables Variables) (string, error) {
	expanded := value

	if containsPlaceholder(expanded, "$HOME") || containsPlaceholder(expanded, "$USER") {
		home := strings.TrimSpace(variables.HomeDir)
		if home == "" {
			var err error
			home, err = os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve $HOME: %w", err)
			}
		}
		home, err := absoluteRoot("$HOME", home)
		if err != nil {
			return "", err
		}
		expanded = replacePlaceholder(expanded, "$HOME", home)
		expanded = replacePlaceholder(expanded, "$USER", home)
	}

	if containsPlaceholder(expanded, "$CWD") {
		cwd := strings.TrimSpace(variables.CWD)
		if cwd == "" {
			return "", fmt.Errorf("resolve $CWD: current directory is unavailable")
		}
		cwd, err := absoluteRoot("$CWD", cwd)
		if err != nil {
			return "", err
		}
		expanded = replacePlaceholder(expanded, "$CWD", cwd)
	}

	if containsPlaceholder(expanded, "$CONFIG") {
		configDir := strings.TrimSpace(variables.ConfigDir)
		if configDir == "" {
			return "", fmt.Errorf("resolve $CONFIG: config directory is unavailable")
		}
		configDir, err := absoluteRoot("$CONFIG", configDir)
		if err != nil {
			return "", err
		}
		expanded = replacePlaceholder(expanded, "$CONFIG", configDir)
	}

	if containsPlaceholder(expanded, "$REPO") {
		repo := strings.TrimSpace(variables.RepoDir)
		if repo == "" {
			cwd := strings.TrimSpace(variables.CWD)
			if cwd == "" {
				return "", fmt.Errorf("resolve $REPO: current directory is unavailable")
			}
			var ok bool
			var err error
			repo, ok, err = FindRepoRoot(cwd)
			if err != nil {
				return "", fmt.Errorf("resolve $REPO from %q: %w", cwd, err)
			}
			if !ok {
				return "", fmt.Errorf("resolve $REPO from %q: %w", cwd, ErrRepoRootNotFound)
			}
		}
		repo, err := absoluteRoot("$REPO", repo)
		if err != nil {
			return "", err
		}
		expanded = replacePlaceholder(expanded, "$REPO", repo)
	}

	return expanded, nil
}

// Resolve expands placeholders and makes a relative path absolute against
// baseDir. It does not require the resulting path to exist.
func Resolve(value, baseDir string, variables Variables) (string, error) {
	expanded, err := Expand(value, variables)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(expanded) == "" {
		return "", nil
	}
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded), nil
	}
	if strings.TrimSpace(baseDir) == "" {
		return "", fmt.Errorf("resolve relative path %q: base directory is unavailable", value)
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve base directory %q: %w", baseDir, err)
	}
	return filepath.Clean(filepath.Join(absBase, expanded)), nil
}

// ResolveAll resolves paths in their configured order.
func ResolveAll(values []string, baseDir string, variables Variables) ([]string, error) {
	if values == nil {
		return nil, nil
	}
	resolved := make([]string, 0, len(values))
	for index, value := range values {
		path, err := Resolve(value, baseDir, variables)
		if err != nil {
			return nil, fmt.Errorf("resolve path[%d] %q: %w", index, value, err)
		}
		resolved = append(resolved, path)
	}
	return resolved, nil
}

// FindRepoRoot walks upward from start until it finds a .git file or directory.
func FindRepoRoot(start string) (string, bool, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", false, err
	}
	dir := filepath.Clean(abs)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true, nil
		} else if !os.IsNotExist(err) {
			return "", false, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}

func absoluteRoot(name, value string) (string, error) {
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s value %q: %w", name, value, err)
	}
	return filepath.Clean(abs), nil
}

func containsPlaceholder(value, placeholder string) bool {
	return placeholderIndex(value, placeholder, 0) >= 0
}

func replacePlaceholder(value, placeholder, replacement string) string {
	var result strings.Builder
	start := 0
	for {
		index := placeholderIndex(value, placeholder, start)
		if index < 0 {
			result.WriteString(value[start:])
			return result.String()
		}
		result.WriteString(value[start:index])
		result.WriteString(replacement)
		start = index + len(placeholder)
	}
}

func placeholderIndex(value, placeholder string, start int) int {
	for start <= len(value)-len(placeholder) {
		relative := strings.Index(value[start:], placeholder)
		if relative < 0 {
			return -1
		}
		index := start + relative
		after := index + len(placeholder)
		if after == len(value) || !isPlaceholderNameByte(value[after]) {
			return index
		}
		start = after
	}
	return -1
}

func isPlaceholderNameByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}
