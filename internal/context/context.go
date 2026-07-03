package projectcontext

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const AgentsFileName = "AGENTS.md"

type Project struct {
	Directory        string
	InstructionFiles []InstructionFile
	Instructions     string
	InstructionsPath string
	HasInstructions  bool
}

type LoadOptions struct {
	Directory        string
	ConfigDir        string
	UserDir          string
	InstructionFiles []string
	WarningWriter    io.Writer
}

type InstructionFile struct {
	Path    string
	Content string
}

type InstructionSource string

const (
	InstructionSourceBuiltIn InstructionSource = "sai_builtin"
	InstructionSourceProject InstructionSource = "agents_md"
	InstructionSourceUser    InstructionSource = "user_prompt"
)

type InstructionPriority int

const (
	PriorityBuiltInBase InstructionPriority = iota
	PriorityProject
	PriorityUser
)

type Instruction struct {
	Source   InstructionSource
	Priority InstructionPriority
	Content  string
	Path     string
}

func Load(projectDir string) (Project, error) {
	return LoadWithOptions(LoadOptions{Directory: projectDir})
}

func LoadWithOptions(options LoadOptions) (Project, error) {
	if strings.TrimSpace(options.Directory) == "" {
		return Project{}, fmt.Errorf("project directory is required")
	}

	absProjectDir, err := filepath.Abs(options.Directory)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project directory: %w", err)
	}
	absProjectDir = filepath.Clean(absProjectDir)

	configDir := strings.TrimSpace(options.ConfigDir)
	if configDir == "" {
		configDir = absProjectDir
	}
	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		return Project{}, fmt.Errorf("resolve config directory: %w", err)
	}
	absConfigDir = filepath.Clean(absConfigDir)

	patterns := options.InstructionFiles
	if patterns == nil {
		patterns = []string{"$CWD/AGENTS.md"}
	}

	files, err := loadInstructionFiles(loadEnvironment{
		cwd:           absProjectDir,
		configDir:     absConfigDir,
		userDir:       options.UserDir,
		warningWriter: options.WarningWriter,
	}, patterns)
	if err != nil {
		return Project{}, err
	}

	project := Project{
		Directory:        absProjectDir,
		InstructionFiles: files,
		InstructionsPath: filepath.Join(absProjectDir, AgentsFileName),
	}
	if len(files) > 0 {
		project.Instructions = files[0].Content
		project.InstructionsPath = files[0].Path
		project.HasInstructions = true
	}
	return project, nil
}

type loadEnvironment struct {
	cwd           string
	configDir     string
	userDir       string
	repoRoot      string
	repoResolved  bool
	warningWriter io.Writer
}

func loadInstructionFiles(env loadEnvironment, patterns []string) ([]InstructionFile, error) {
	files := []InstructionFile{}
	seen := map[string]struct{}{}
	for _, entry := range patterns {
		expanded, skip, err := expandInstructionEntry(&env, entry)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}

		matches, err := instructionEntryMatches(expanded)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("stat instruction file %q: %w", match, err)
			}
			if info.IsDir() {
				continue
			}
			identity, err := instructionFileIdentity(match)
			if err != nil {
				return nil, err
			}
			if _, ok := seen[identity]; ok {
				continue
			}
			seen[identity] = struct{}{}
			data, err := os.ReadFile(match)
			if err != nil {
				return nil, fmt.Errorf("read instruction file %q: %w", match, err)
			}
			files = append(files, InstructionFile{
				Path:    filepath.Clean(match),
				Content: string(data),
			})
		}
	}
	return files, nil
}

func instructionFileIdentity(filePath string) (string, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return "", fmt.Errorf("resolve instruction file %q: %w", filePath, err)
	}
	absPath = filepath.Clean(absPath)
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return absPath, nil
	}
	resolvedAbsPath, err := filepath.Abs(resolvedPath)
	if err != nil {
		return filepath.Clean(resolvedPath), nil
	}
	return filepath.Clean(resolvedAbsPath), nil
}

func expandInstructionEntry(env *loadEnvironment, entry string) (string, bool, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", true, nil
	}

	if strings.Contains(entry, "$REPO") {
		repoRoot, ok := env.repo()
		if !ok {
			warnUnresolvedRepo(env.warningWriter, entry, env.cwd)
			return "", true, nil
		}
		entry = strings.ReplaceAll(entry, "$REPO", repoRoot)
	}
	if strings.Contains(entry, "$USER") {
		userDir, err := env.user()
		if err != nil {
			return "", false, err
		}
		entry = strings.ReplaceAll(entry, "$USER", userDir)
	}
	entry = strings.ReplaceAll(entry, "$CWD", env.cwd)
	entry = strings.ReplaceAll(entry, "$CONFIG", env.configDir)
	if !filepath.IsAbs(entry) {
		entry = filepath.Join(env.cwd, entry)
	}
	return filepath.Clean(entry), false, nil
}

func (env *loadEnvironment) repo() (string, bool) {
	if env.repoResolved {
		return env.repoRoot, env.repoRoot != ""
	}
	env.repoResolved = true
	repoRoot, ok := discoverRepoRoot(env.cwd)
	if ok {
		env.repoRoot = repoRoot
	}
	return env.repoRoot, ok
}

func (env *loadEnvironment) user() (string, error) {
	userDir := strings.TrimSpace(env.userDir)
	if userDir == "" {
		var err error
		userDir, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home directory: %w", err)
		}
	}
	absUserDir, err := filepath.Abs(userDir)
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Clean(absUserDir), nil
}

func warnUnresolvedRepo(w io.Writer, entry, cwd string) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "sai: warning: skipping instruction file entry %q because $REPO could not be resolved from %s\n", entry, cwd)
}

func discoverRepoRoot(start string) (string, bool) {
	dir := filepath.Clean(start)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		} else if !os.IsNotExist(err) {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func instructionEntryMatches(pattern string) ([]string, error) {
	if !hasGlob(pattern) {
		if _, err := os.Stat(pattern); err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("stat instruction file %q: %w", pattern, err)
		}
		return []string{filepath.Clean(pattern)}, nil
	}

	var (
		matches []string
		err     error
	)
	if hasRecursiveGlob(pattern) {
		matches, err = recursiveGlob(pattern)
	} else {
		matches, err = filepath.Glob(pattern)
	}
	if err != nil {
		return nil, fmt.Errorf("expand instruction file glob %q: %w", pattern, err)
	}
	for i := range matches {
		matches[i] = filepath.Clean(matches[i])
	}
	sort.Strings(matches)
	return matches, nil
}

func hasGlob(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

func hasRecursiveGlob(pattern string) bool {
	slashPattern := filepath.ToSlash(pattern)
	return strings.Contains(slashPattern, "/**/") || strings.HasSuffix(slashPattern, "/**")
}

func recursiveGlob(pattern string) ([]string, error) {
	slashPattern := filepath.ToSlash(filepath.Clean(pattern))
	parts := strings.SplitN(slashPattern, "/**/", 2)
	if len(parts) != 2 {
		if strings.HasSuffix(slashPattern, "/**") {
			return recursiveFiles(strings.TrimSuffix(slashPattern, "/**"), "")
		}
		return filepath.Glob(pattern)
	}
	return recursiveFiles(parts[0], parts[1])
}

func recursiveFiles(baseSlash, tailPattern string) ([]string, error) {
	base := filepath.FromSlash(baseSlash)
	tailPattern = strings.TrimPrefix(tailPattern, "/")

	matches := []string{}
	if _, err := os.Stat(base); err != nil {
		if os.IsNotExist(err) {
			return matches, nil
		}
		return nil, err
	}
	err := filepath.WalkDir(base, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if tailPattern == "" {
			matches = append(matches, filePath)
			return nil
		}
		rel, err := filepath.Rel(base, filePath)
		if err != nil {
			return err
		}
		if recursiveTailMatches(tailPattern, filepath.ToSlash(rel)) {
			matches = append(matches, filePath)
		}
		return nil
	})
	return matches, err
}

func recursiveTailMatches(pattern, rel string) bool {
	for {
		ok, err := path.Match(pattern, rel)
		if err != nil {
			return false
		}
		if ok {
			return true
		}
		next := strings.Index(rel, "/")
		if next < 0 {
			return false
		}
		rel = rel[next+1:]
	}
}

func ComposeInstructions(builtInBase string, project Project, userPrompt string) []Instruction {
	instructions := []Instruction{
		{
			Source:   InstructionSourceBuiltIn,
			Priority: PriorityBuiltInBase,
			Content:  builtInBase,
		},
	}

	for _, file := range project.InstructionFiles {
		instructions = append(instructions, Instruction{
			Source:   InstructionSourceProject,
			Priority: PriorityProject,
			Content:  file.Content,
			Path:     file.Path,
		})
	}
	if len(project.InstructionFiles) == 0 && project.HasInstructions {
		instructions = append(instructions, Instruction{
			Source:   InstructionSourceProject,
			Priority: PriorityProject,
			Content:  project.Instructions,
			Path:     project.InstructionsPath,
		})
	}

	instructions = append(instructions, Instruction{
		Source:   InstructionSourceUser,
		Priority: PriorityUser,
		Content:  userPrompt,
	})

	return instructions
}
