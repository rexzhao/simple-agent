package projectcontext

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReadsAgentsFromProjectDirectory(t *testing.T) {
	projectDir := t.TempDir()
	content := "Follow the project instructions.\n"
	writeContextFile(t, filepath.Join(projectDir, AgentsFileName), content)

	project, err := Load(projectDir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantDir, err := filepath.Abs(projectDir)
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	wantDir = filepath.Clean(wantDir)
	if project.Directory != wantDir {
		t.Fatalf("Directory = %q, want %q", project.Directory, wantDir)
	}
	if project.InstructionsPath != filepath.Join(wantDir, AgentsFileName) {
		t.Fatalf("InstructionsPath = %q, want AGENTS.md in project dir", project.InstructionsPath)
	}
	if !project.HasInstructions {
		t.Fatal("HasInstructions = false, want true")
	}
	if project.Instructions != content {
		t.Fatalf("Instructions = %q, want %q", project.Instructions, content)
	}
}

func TestLoadMissingAgentsReturnsEmptyProjectInstructions(t *testing.T) {
	projectDir := t.TempDir()

	project, err := Load(projectDir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if project.HasInstructions {
		t.Fatal("HasInstructions = true, want false")
	}
	if project.Instructions != "" {
		t.Fatalf("Instructions = %q, want empty", project.Instructions)
	}
}

func TestLoadDoesNotReadAgentsFromConfigDirectory(t *testing.T) {
	projectDir := t.TempDir()
	configDir := t.TempDir()
	writeContextFile(t, filepath.Join(configDir, AgentsFileName), "config instructions\n")

	project, err := Load(projectDir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if project.HasInstructions {
		t.Fatal("HasInstructions = true, want false")
	}
	if project.Instructions != "" {
		t.Fatalf("Instructions = %q, want empty", project.Instructions)
	}
}

func TestLoadWithOptionsExpandsInstructionFilePlaceholders(t *testing.T) {
	repoDir := t.TempDir()
	writeContextFile(t, filepath.Join(repoDir, ".git"), "gitdir\n")
	projectDir := filepath.Join(repoDir, "work", "nested")
	configDir := t.TempDir()
	userDir := t.TempDir()
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(projectDir) error = %v", err)
	}
	writeContextFile(t, filepath.Join(projectDir, "cwd.md"), "cwd\n")
	writeContextFile(t, filepath.Join(configDir, "config.md"), "config\n")
	writeContextFile(t, filepath.Join(userDir, "user.md"), "user\n")
	writeContextFile(t, filepath.Join(repoDir, "repo.md"), "repo\n")

	project, err := LoadWithOptions(LoadOptions{
		Directory: projectDir,
		ConfigDir: configDir,
		UserDir:   userDir,
		InstructionFiles: []string{
			"$CWD/cwd.md",
			"$CONFIG/config.md",
			"$USER/user.md",
			"$REPO/repo.md",
		},
	})
	if err != nil {
		t.Fatalf("LoadWithOptions() error = %v", err)
	}

	assertInstructionFiles(t, project.InstructionFiles, []InstructionFile{
		{Path: filepath.Join(projectDir, "cwd.md"), Content: "cwd\n"},
		{Path: filepath.Join(configDir, "config.md"), Content: "config\n"},
		{Path: filepath.Join(userDir, "user.md"), Content: "user\n"},
		{Path: filepath.Join(repoDir, "repo.md"), Content: "repo\n"},
	})
}

func TestLoadWithOptionsExplicitEmptyInstructionFilesLoadsNone(t *testing.T) {
	projectDir := t.TempDir()
	writeContextFile(t, filepath.Join(projectDir, AgentsFileName), "ignored\n")

	project, err := LoadWithOptions(LoadOptions{
		Directory:        projectDir,
		InstructionFiles: []string{},
	})
	if err != nil {
		t.Fatalf("LoadWithOptions() error = %v", err)
	}

	if len(project.InstructionFiles) != 0 {
		t.Fatalf("InstructionFiles = %#v, want none", project.InstructionFiles)
	}
	if project.HasInstructions {
		t.Fatal("HasInstructions = true, want false")
	}
}

func TestLoadWithOptionsSkipsMissingInstructionFiles(t *testing.T) {
	projectDir := t.TempDir()
	writeContextFile(t, filepath.Join(projectDir, "present.md"), "present\n")

	project, err := LoadWithOptions(LoadOptions{
		Directory: projectDir,
		InstructionFiles: []string{
			"$CWD/missing.md",
			"$CWD/present.md",
		},
	})
	if err != nil {
		t.Fatalf("LoadWithOptions() error = %v", err)
	}

	assertInstructionFiles(t, project.InstructionFiles, []InstructionFile{
		{Path: filepath.Join(projectDir, "present.md"), Content: "present\n"},
	})
}

func TestLoadWithOptionsWarnsAndSkipsUnresolvedRepo(t *testing.T) {
	projectDir := t.TempDir()
	var warnings bytes.Buffer

	project, err := LoadWithOptions(LoadOptions{
		Directory: projectDir,
		InstructionFiles: []string{
			"$REPO/secret.md",
		},
		WarningWriter: &warnings,
	})
	if err != nil {
		t.Fatalf("LoadWithOptions() error = %v", err)
	}

	if len(project.InstructionFiles) != 0 {
		t.Fatalf("InstructionFiles = %#v, want none", project.InstructionFiles)
	}
	if !strings.Contains(warnings.String(), "$REPO could not be resolved") {
		t.Fatalf("warning = %q, want unresolved $REPO warning", warnings.String())
	}
	instructions := ComposeInstructions("built-in", project, "user")
	if len(instructions) != 2 {
		t.Fatalf("len(ComposeInstructions()) = %d, want 2: %#v", len(instructions), instructions)
	}
	for _, instruction := range instructions {
		if strings.Contains(instruction.Content, warnings.String()) {
			t.Fatalf("warning entered instruction context: %#v", instruction)
		}
	}
}

func TestLoadWithOptionsExpandsGlobInStableSortOrder(t *testing.T) {
	projectDir := t.TempDir()
	writeContextFile(t, filepath.Join(projectDir, "b.md"), "b\n")
	writeContextFile(t, filepath.Join(projectDir, "a.md"), "a\n")
	writeContextFile(t, filepath.Join(projectDir, "notes.txt"), "ignored\n")

	project, err := LoadWithOptions(LoadOptions{
		Directory:        projectDir,
		InstructionFiles: []string{"$CWD/*.md"},
	})
	if err != nil {
		t.Fatalf("LoadWithOptions() error = %v", err)
	}

	assertInstructionFiles(t, project.InstructionFiles, []InstructionFile{
		{Path: filepath.Join(projectDir, "a.md"), Content: "a\n"},
		{Path: filepath.Join(projectDir, "b.md"), Content: "b\n"},
	})
}

func TestLoadWithOptionsExpandsRecursiveGlob(t *testing.T) {
	projectDir := t.TempDir()
	writeContextFile(t, filepath.Join(projectDir, "root.md"), "root\n")
	writeContextFile(t, filepath.Join(projectDir, "nested", "b.md"), "nested b\n")
	writeContextFile(t, filepath.Join(projectDir, "nested", "deeper", "a.md"), "nested a\n")
	writeContextFile(t, filepath.Join(projectDir, "nested", "ignored.txt"), "ignored\n")

	project, err := LoadWithOptions(LoadOptions{
		Directory:        projectDir,
		InstructionFiles: []string{"$CWD/**/*.md"},
	})
	if err != nil {
		t.Fatalf("LoadWithOptions() error = %v", err)
	}

	assertInstructionFiles(t, project.InstructionFiles, []InstructionFile{
		{Path: filepath.Join(projectDir, "nested", "b.md"), Content: "nested b\n"},
		{Path: filepath.Join(projectDir, "nested", "deeper", "a.md"), Content: "nested a\n"},
		{Path: filepath.Join(projectDir, "root.md"), Content: "root\n"},
	})
}

func TestComposeInstructionsKeepsSeparateProjectInstructionSources(t *testing.T) {
	project := Project{
		InstructionFiles: []InstructionFile{
			{Path: "first.md", Content: "first"},
			{Path: "second.md", Content: "second"},
		},
		HasInstructions: true,
	}

	got := ComposeInstructions("built-in", project, "user")
	want := []Instruction{
		{
			Source:   InstructionSourceBuiltIn,
			Priority: PriorityBuiltInBase,
			Content:  "built-in",
		},
		{
			Source:   InstructionSourceProject,
			Priority: PriorityProject,
			Content:  "first",
			Path:     "first.md",
		},
		{
			Source:   InstructionSourceProject,
			Priority: PriorityProject,
			Content:  "second",
			Path:     "second.md",
		},
		{
			Source:   InstructionSourceUser,
			Priority: PriorityUser,
			Content:  "user",
		},
	}

	if len(got) != len(want) {
		t.Fatalf("len(ComposeInstructions()) = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ComposeInstructions()[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestComposeInstructionsKeepsPriorityOrder(t *testing.T) {
	project := Project{
		Instructions:    "project",
		HasInstructions: true,
	}

	got := ComposeInstructions("built-in", project, "user")
	want := []Instruction{
		{
			Source:   InstructionSourceBuiltIn,
			Priority: PriorityBuiltInBase,
			Content:  "built-in",
		},
		{
			Source:   InstructionSourceProject,
			Priority: PriorityProject,
			Content:  "project",
		},
		{
			Source:   InstructionSourceUser,
			Priority: PriorityUser,
			Content:  "user",
		},
	}

	if len(got) != len(want) {
		t.Fatalf("len(ComposeInstructions()) = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ComposeInstructions()[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func assertInstructionFiles(t *testing.T, got, want []InstructionFile) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(InstructionFiles) = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		wantPath := filepath.Clean(want[i].Path)
		if got[i].Path != wantPath || got[i].Content != want[i].Content {
			t.Fatalf("InstructionFiles[%d] = %#v, want path %q content %q", i, got[i], wantPath, want[i].Content)
		}
	}
}

func writeContextFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
