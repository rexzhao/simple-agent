package projects

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// The earliest Added survives in BOTH orders — ascending (the duplicate
// appears later with a later Added) and descending.
func TestDedupedKeepsEarliestAddedBothOrders(t *testing.T) {
	base := t.TempDir()
	early := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	x := filepath.Join(base, "x")
	y := filepath.Join(base, "y")

	ascending := &Registry{Projects: []Entry{
		{Path: x, Added: early},
		{Path: y, Added: early},
		{Path: x + string(filepath.Separator), Added: late},
	}}
	entries := ascending.Deduped()
	if len(entries) != 2 {
		t.Fatalf("ascending: entries = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		if jsonl.ProjectID(entry.Path) == jsonl.ProjectID(x) && !entry.Added.Equal(early) {
			t.Fatalf("ascending: merged Added = %v, want the earliest %v", entry.Added, early)
		}
	}

	descending := &Registry{Projects: []Entry{
		{Path: x, Added: late},
		{Path: y, Added: early},
		{Path: x + string(filepath.Separator), Added: early},
	}}
	entries = descending.Deduped()
	if len(entries) != 2 {
		t.Fatalf("descending: entries = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		if jsonl.ProjectID(entry.Path) == jsonl.ProjectID(x) && !entry.Added.Equal(early) {
			t.Fatalf("descending: merged Added = %v, want the earliest %v", entry.Added, early)
		}
	}
}

// A blank registry file (touch, or an emptied file) is an empty registry,
// not corruption — it must not lock add and serve startup.
func TestLoadBlankIsEmpty(t *testing.T) {
	for _, content := range []string{"", "\n", "   \n"} {
		path := filepath.Join(t.TempDir(), "projects.json")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		registry, err := Load(path)
		if err != nil {
			t.Fatalf("blank %q: %v", content, err)
		}
		if len(registry.Projects) != 0 {
			t.Fatalf("blank %q: projects = %d", content, len(registry.Projects))
		}
	}
}

// Concurrent Saves never share a staging path: every intermediate Load
// must parse, and every Save must succeed (a fixed sibling temp name
// would let one writer publish another's half-written document).
func TestSaveConcurrentNoSharedStaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	// A payload large enough to make the write window non-trivial.
	big := &Registry{}
	for i := 0; i < 2000; i++ {
		big.Projects = append(big.Projects, Entry{
			Path:  fmt.Sprintf("F:/work/project-%04d", i),
			Added: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	if err := big.Save(path); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		defer close(readErr)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := Load(path); err != nil {
				readErr <- err
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := big.Save(path); err != nil {
				t.Errorf("concurrent save: %v", err)
			}
		}()
	}
	wg.Wait()
	close(stop)
	if err := <-readErr; err != nil {
		t.Fatalf("reader saw a torn document: %v", err)
	}
	// No staging leftovers.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "projects.json-") {
			t.Fatalf("staging leftover: %s", entry.Name())
		}
	}
}
