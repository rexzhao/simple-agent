package projectcontext

import (
	"os"
	"path/filepath"
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

func writeContextFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
