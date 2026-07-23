package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const skillFileName = "SKILL.md"

type Skill struct {
	ID                     string
	Name                   string
	Description            string
	Path                   string
	Instructions           string
	DisableModelInvocation bool
}

type SkillRef struct {
	ID   string
	Path string
}

type skillFrontmatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

func Discover(dir string) ([]Skill, error) {
	refs, err := DiscoverRefs(dir)
	if err != nil {
		return nil, err
	}

	found := make([]Skill, 0, len(refs))
	for _, ref := range refs {
		skill, err := Load(ref.Path)
		if err != nil {
			return nil, err
		}
		found = append(found, skill)
	}
	return found, nil
}

func DiscoverDirs(dirs []string) ([]Skill, error) {
	refs, err := DiscoverRefsDirs(dirs)
	if err != nil {
		return nil, err
	}

	found := make([]Skill, 0, len(refs))
	for _, ref := range refs {
		skill, err := Load(ref.Path)
		if err != nil {
			return nil, err
		}
		found = append(found, skill)
	}
	return found, nil
}

func DiscoverRefs(dir string) ([]SkillRef, error) {
	if strings.TrimSpace(dir) == "" {
		return []SkillRef{}, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []SkillRef{}, nil
		}
		return nil, fmt.Errorf("read skill directory %q: %w", dir, err)
	}

	found := make([]SkillRef, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillPath := filepath.Join(dir, entry.Name(), skillFileName)
		info, err := os.Stat(skillPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat skill file %q: %w", skillPath, err)
		}
		if info.IsDir() {
			continue
		}

		found = append(found, SkillRef{
			ID:   entry.Name(),
			Path: filepath.Clean(filepath.Join(dir, entry.Name())),
		})
	}

	sort.Slice(found, func(i, j int) bool {
		return found[i].ID < found[j].ID
	})
	return found, nil
}

func DiscoverRefsDirs(dirs []string) ([]SkillRef, error) {
	found := []SkillRef{}
	seen := map[string]int{}
	seenDirs := map[string]struct{}{}
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		key := skillDirKey(dir)
		if _, duplicate := seenDirs[key]; duplicate {
			continue
		}
		seenDirs[key] = struct{}{}
		refs, err := DiscoverRefs(dir)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if previous, ok := seen[ref.ID]; ok {
				found[previous] = ref
				continue
			}
			seen[ref.ID] = len(found)
			found = append(found, ref)
		}
	}
	return found, nil
}

func skillDirKey(dir string) string {
	key := filepath.Clean(dir)
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}
	if evaluated, err := filepath.EvalSymlinks(key); err == nil {
		key = evaluated
	}
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}

func Load(path string) (Skill, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Skill{}, fmt.Errorf("stat skill path %q: %w", path, err)
	}

	skillPath := path
	idDir := filepath.Dir(path)
	if info.IsDir() {
		idDir = path
		skillPath = filepath.Join(path, skillFileName)
	}

	id := filepath.Base(filepath.Clean(idDir))
	data, err := os.ReadFile(skillPath)
	if err != nil {
		return Skill{}, fmt.Errorf("read skill file %q: %w", skillPath, err)
	}

	return parseSkill(id, filepath.Clean(skillPath), string(data))
}

func parseSkill(id, path, text string) (Skill, error) {
	skill := Skill{
		ID:           id,
		Name:         id,
		Path:         path,
		Instructions: text,
	}

	frontmatter, body, ok, err := splitFrontmatter(text)
	if err != nil {
		return Skill{}, fmt.Errorf("parse skill frontmatter %q: %w", path, err)
	}
	if !ok {
		return skill, nil
	}

	var meta skillFrontmatter
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return Skill{}, fmt.Errorf("parse skill frontmatter %q: %w", path, err)
	}

	if name := strings.TrimSpace(meta.Name); name != "" {
		skill.Name = name
	}
	skill.Description = strings.TrimSpace(meta.Description)
	skill.DisableModelInvocation = meta.DisableModelInvocation
	skill.Instructions = body
	return skill, nil
}

func splitFrontmatter(text string) (string, string, bool, error) {
	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r\n") != "---" {
		return "", "", false, nil
	}

	frontmatterStart := len(lines[0])
	offset := frontmatterStart
	for _, line := range lines[1:] {
		if strings.TrimRight(line, "\r\n") == "---" {
			bodyStart := offset + len(line)
			return text[frontmatterStart:offset], text[bodyStart:], true, nil
		}
		offset += len(line)
	}
	return "", "", false, fmt.Errorf("unterminated frontmatter")
}
