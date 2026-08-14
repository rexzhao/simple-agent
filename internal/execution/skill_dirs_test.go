package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/config"
	localskills "github.com/rexzhao/simple-agent/internal/skills"
)

func TestEnabledSkillsForRunResolvesRepoAndCWDPlaceholders(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	cwd := filepath.Join(repoDir, "work")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("MkdirAll(cwd) error = %v", err)
	}
	writeExecutionSkill(t, filepath.Join(repoDir, ".agents", "skills", "repo-skill"), "repo instructions")
	writeExecutionSkill(t, filepath.Join(cwd, ".agents", "skills", "cwd-skill"), "cwd instructions")

	cfg := &config.Config{
		ConfigPath: filepath.Join(root, "server", "sai.yaml"),
		SkillDirs: []string{
			"$REPO/.agents/skills",
			"$CWD/.agents/skills",
		},
	}
	got, err := enabledSkillsForRun(cfg, cwd)
	if err != nil {
		t.Fatalf("enabledSkillsForRun() error = %v", err)
	}
	if len(got) != 2 || got[0].ID != "repo-skill" || got[1].ID != "cwd-skill" {
		t.Fatalf("enabledSkillsForRun() = %#v, want repo-skill then cwd-skill", got)
	}
}

func TestEnabledSkillsForRunDeduplicatesRepoAndCWDAtRepoRoot(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	writeExecutionSkill(t, filepath.Join(repoDir, ".agents", "skills", "hello"), "hello instructions")

	cfg := &config.Config{
		ConfigPath: filepath.Join(t.TempDir(), "sai.yaml"),
		SkillDirs: []string{
			"$REPO/.agents/skills",
			"$CWD/.agents/skills",
		},
	}
	got, err := enabledSkillsForRun(cfg, repoDir)
	if err != nil {
		t.Fatalf("enabledSkillsForRun() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "hello" {
		t.Fatalf("enabledSkillsForRun() = %#v, want one hello skill", got)
	}
}

func TestBuiltInInstructionsListsRegisteredSkillFiles(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first", "SKILL.md")
	second := filepath.Join(t.TempDir(), "second", "SKILL.md")

	got := builtInInstructions([]localskills.Skill{
		{ID: "first", Path: first},
		{ID: "second", Path: second},
	})

	for _, want := range []string{first, second, "Do not scan the current working directory for skills"} {
		if !strings.Contains(got, want) {
			t.Fatalf("builtInInstructions() = %q, want contain %q", got, want)
		}
	}
}

func TestBuiltInInstructionsOmitsSkillDiscoveryWithoutSkills(t *testing.T) {
	if got := builtInInstructions(nil); got != builtInBaseInstructions {
		t.Fatalf("builtInInstructions(nil) = %q, want %q", got, builtInBaseInstructions)
	}
}

func writeExecutionSkill(t *testing.T, dir, instructions string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(instructions), 0o600); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}
}
