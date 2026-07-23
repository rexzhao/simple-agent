package skills

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDiscoverFindsDirectSkillsSorted(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "zeta"), "Zeta instructions")
	writeSkill(t, filepath.Join(dir, "alpha"), "Alpha instructions")
	writeSkill(t, filepath.Join(dir, "middle"), "Middle instructions")

	got, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	wantIDs := []string{"alpha", "middle", "zeta"}
	if len(got) != len(wantIDs) {
		t.Fatalf("len(Discover()) = %d, want %d: %#v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("Discover()[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}
}

func TestDiscoverIgnoresEntriesWithoutDirectSkillMD(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "found"), "Found instructions")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "root file is ignored")
	writeFile(t, filepath.Join(dir, "plain-file"), "ignored")
	writeFile(t, filepath.Join(dir, "missing", "README.md"), "ignored")
	writeSkill(t, filepath.Join(dir, "nested", "child"), "nested child is ignored")

	got, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("len(Discover()) = %d, want 1: %#v", len(got), got)
	}
	if got[0].ID != "found" {
		t.Fatalf("Discover()[0].ID = %q, want found", got[0].ID)
	}
}

func TestDiscoverRefsDoesNotParseSkillFiles(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "valid"), "Valid instructions")
	writeSkill(t, filepath.Join(dir, "malformed"), "---\nname: [bad\n---\nBad instructions\n")

	got, err := DiscoverRefs(dir)
	if err != nil {
		t.Fatalf("DiscoverRefs() error = %v", err)
	}

	wantIDs := []string{"malformed", "valid"}
	if len(got) != len(wantIDs) {
		t.Fatalf("len(DiscoverRefs()) = %d, want %d: %#v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("DiscoverRefs()[%d].ID = %q, want %q", i, got[i].ID, want)
		}
		if wantPath := filepath.Join(dir, want); got[i].Path != wantPath {
			t.Fatalf("DiscoverRefs()[%d].Path = %q, want %q", i, got[i].Path, wantPath)
		}
	}
}

func TestDiscoverDirsPreservesDirectoryOrder(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	writeSkill(t, filepath.Join(first, "zeta"), "Zeta instructions")
	writeSkill(t, filepath.Join(first, "alpha"), "Alpha instructions")
	writeSkill(t, filepath.Join(second, "beta"), "Beta instructions")
	writeSkill(t, filepath.Join(second, "aardvark"), "Aardvark instructions")

	got, err := DiscoverDirs([]string{first, second})
	if err != nil {
		t.Fatalf("DiscoverDirs() error = %v", err)
	}

	wantIDs := []string{"alpha", "zeta", "aardvark", "beta"}
	if len(got) != len(wantIDs) {
		t.Fatalf("len(DiscoverDirs()) = %d, want %d: %#v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("DiscoverDirs()[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}
}

func TestDiscoverDirsDeduplicatesEquivalentDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills")
	writeSkill(t, filepath.Join(dir, "shared"), "Shared instructions")

	got, err := DiscoverDirs([]string{dir, filepath.Join(dir, ".")})
	if err != nil {
		t.Fatalf("DiscoverDirs() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "shared" {
		t.Fatalf("DiscoverDirs() = %#v, want one shared skill", got)
	}
}

func TestDiscoverDirsUsesLastDirectoryForDuplicateIDs(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	writeSkill(t, filepath.Join(first, "shared"), "First instructions")
	writeSkill(t, filepath.Join(second, "shared"), "Second instructions")

	got, err := DiscoverDirs([]string{first, second})
	if err != nil {
		t.Fatalf("DiscoverDirs() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("DiscoverDirs() = %#v, want one shared skill", got)
	}
	if got[0].Instructions != "Second instructions" {
		t.Fatalf("shared instructions = %q, want last directory contents", got[0].Instructions)
	}
	if wantPath := filepath.Join(second, "shared", skillFileName); got[0].Path != wantPath {
		t.Fatalf("shared path = %q, want %q", got[0].Path, wantPath)
	}
}

func TestLoadReadsFrontmatter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sample")
	writeSkill(t, dir, `---
name: my-skill
description: short text
---
body line one
body line two
`)

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got.ID != "sample" {
		t.Fatalf("ID = %q, want sample", got.ID)
	}
	if got.Name != "my-skill" {
		t.Fatalf("Name = %q, want my-skill", got.Name)
	}
	if got.Description != "short text" {
		t.Fatalf("Description = %q, want short text", got.Description)
	}
	if got.DisableModelInvocation {
		t.Fatal("DisableModelInvocation = true, want false")
	}
	if want := "body line one\nbody line two\n"; got.Instructions != want {
		t.Fatalf("Instructions = %q, want %q", got.Instructions, want)
	}
	if want := filepath.Join(dir, skillFileName); got.Path != want {
		t.Fatalf("Path = %q, want %q", got.Path, want)
	}
}

func TestLoadReadsDisableModelInvocationFrontmatter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sample")
	writeSkill(t, dir, `---
name: hidden-skill
disable-model-invocation: true
---
hidden body
`)

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !got.DisableModelInvocation {
		t.Fatal("DisableModelInvocation = false, want true")
	}
	if got.Name != "hidden-skill" {
		t.Fatalf("Name = %q, want hidden-skill", got.Name)
	}
	if got.Instructions != "hidden body\n" {
		t.Fatalf("Instructions = %q, want hidden body", got.Instructions)
	}
}

func TestLoadWithoutFrontmatterFallsBackToID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plain")
	instructions := "Use concise local instructions.\n"
	writeSkill(t, dir, instructions)

	got, err := Load(filepath.Join(dir, skillFileName))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got.ID != "plain" {
		t.Fatalf("ID = %q, want plain", got.ID)
	}
	if got.Name != "plain" {
		t.Fatalf("Name = %q, want plain", got.Name)
	}
	if got.Description != "" {
		t.Fatalf("Description = %q, want empty", got.Description)
	}
	if got.Instructions != instructions {
		t.Fatalf("Instructions = %q, want %q", got.Instructions, instructions)
	}
}

func TestLoadRejectsUnterminatedFrontmatter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bad")
	writeSkill(t, dir, "---\nname: bad\nmissing closing marker\n")

	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load() error = nil, want unterminated frontmatter error")
	}
	got := err.Error()
	for _, want := range []string{"parse skill frontmatter", "unterminated frontmatter", strconv.Quote(filepath.Join(dir, skillFileName))} {
		if !strings.Contains(got, want) {
			t.Fatalf("Load() error = %q, want contain %q", got, want)
		}
	}
}

func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, skillFileName), content)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
