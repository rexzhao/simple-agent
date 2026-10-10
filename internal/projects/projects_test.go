package projects

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
)

func TestLoadMissingIsEmpty(t *testing.T) {
	registry, err := Load(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatalf("missing registry: %v", err)
	}
	if len(registry.Projects) != 0 {
		t.Fatalf("projects = %d, want 0", len(registry.Projects))
	}
}

func TestLoadCorruptIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	if err := os.WriteFile(path, []byte(`{"projects":[{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("corrupt registry loaded without error")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	registry := &Registry{Projects: []Entry{
		{Path: "F:/work/a", Added: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{Path: "F:/work/b", Added: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)},
	}}
	if err := registry.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Projects) != 2 || loaded.Projects[0].Path != "F:/work/a" {
		t.Fatalf("round trip = %+v", loaded.Projects)
	}
}

func TestLookupRemoveByCanonical(t *testing.T) {
	registry := &Registry{Projects: []Entry{
		{Path: "F:/Work/ProjectA", Added: time.Now()},
		{Path: "F:/work/projectb", Added: time.Now()},
	}}
	// Canonical is case-folded on Windows/darwin: the lookup matches
	// regardless of the stored spelling's case.
	if _, ok := registry.Lookup(Canonical("f:/work/projecta")); !ok {
		t.Fatal("lookup missed case-folded path")
	}
	if _, ok := registry.Lookup(Canonical("f:/work/missing")); ok {
		t.Fatal("lookup hit unknown path")
	}
	if got := registry.Remove(Canonical("F:/work/PROJECTA")); got != 1 {
		t.Fatalf("removed = %d, want 1", got)
	}
	if len(registry.Projects) != 1 || registry.Projects[0].Path != "F:/work/projectb" {
		t.Fatalf("remaining = %+v", registry.Projects)
	}
}

// Same-ID duplicates (hand-edited registry, equivalent path spellings)
// merge on the read side; the earliest Added survives.
func TestDedupedMergesSameID(t *testing.T) {
	base := t.TempDir()
	registry := &Registry{Projects: []Entry{
		{Path: filepath.Join(base, "x"), Added: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)},
		{Path: filepath.Join(base, "y"), Added: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
		{Path: filepath.Join(base, "x") + string(filepath.Separator), Added: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
	}}
	entries := registry.Deduped()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (duplicate merged)", len(entries))
	}
	found := false
	xID := jsonl.ProjectID(filepath.Join(base, "x"))
	for _, entry := range entries {
		if jsonl.ProjectID(entry.Path) == xID {
			found = true
			if !entry.Added.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
				t.Fatalf("merged Added = %v, want the earliest", entry.Added)
			}
		}
	}
	if !found {
		t.Fatalf("merged entry not found in %+v", entries)
	}
}

// Canonical and jsonl.ProjectID must agree on normalization: the registry
// dedupes by ID, and a Canonical that hashes differently from the stored
// spelling would split one project in two.
func TestCanonicalMatchesProjectIDNormalization(t *testing.T) {
	dir := t.TempDir()
	variants := []string{dir, dir + string(filepath.Separator), filepath.Join(dir, ".")}
	ids := make(map[string]bool)
	for _, variant := range variants {
		ids[jsonl.ProjectID(variant)] = true
	}
	if len(ids) != 1 {
		t.Fatalf("path variants hash to %d ids, want 1: %v", len(ids), ids)
	}
}
