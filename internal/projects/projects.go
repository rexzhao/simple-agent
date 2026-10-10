// Package projects is the serve-mode project registry: the list of
// workspace directories a serve process is authorized to serve. The
// registry is a plain JSON file — the interface between `sai project add`
// and a running serve is the file itself (serve re-reads on demand; a
// rename-atomic replace makes concurrent readers see either the old or the
// new whole document, never a torn one).
package projects

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
)

// Entry is one registered project. Path is the original string form —
// project IDs are derived on demand (never persisted), and "original"
// matters: the ID hash does no symlink resolution or drive-letter
// unification, so an equivalent spelling of the same directory hashes to a
// different project.
type Entry struct {
	Path  string    `json:"path"`
	Added time.Time `json:"added"`
}

// Registry is the parsed projects.json document.
type Registry struct {
	Projects []Entry `json:"projects"`
}

// FilePath returns the registry location for a server root.
func FilePath(serverRoot string) string {
	return filepath.Join(serverRoot, "projects.json")
}

// Canonical normalizes a path the same way ProjectID does before hashing
// (Abs + Clean + strings.ToLower on case-insensitive platforms — the same
// ToLower, not an approximation) — no symlink resolution, no drive-letter
// unification, by the same argument as Entry.
func Canonical(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		abs = strings.ToLower(abs)
	}
	return abs
}

// Load reads the registry. A missing file is an empty registry (the first
// add creates it); a corrupt file is an error — callers fail fast on it
// (serve startup exits, `sai project add` refuses to write: read-modify-
// rename over a corrupt file would silently drop every other project).
func Load(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{}, nil
		}
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var registry Registry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("registry %s is corrupt: %w", path, err)
	}
	return &registry, nil
}

// Save writes the registry atomically: temp file + rename, so a concurrent
// reader sees the old or the new document, never a torn one.
func (r *Registry) Save(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write registry staging: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return fmt.Errorf("activate registry: %w", err)
	}
	return nil
}

// Lookup finds the entry for a canonical path.
func (r *Registry) Lookup(canonical string) (Entry, bool) {
	for _, entry := range r.Projects {
		if Canonical(entry.Path) == canonical {
			return entry, true
		}
	}
	return Entry{}, false
}

// Remove drops every entry whose canonical path matches. It reports how
// many entries were removed.
func (r *Registry) Remove(canonical string) int {
	kept := r.Projects[:0]
	removed := 0
	for _, entry := range r.Projects {
		if Canonical(entry.Path) == canonical {
			removed++
			continue
		}
		kept = append(kept, entry)
	}
	r.Projects = kept
	return removed
}

// Deduped returns the entries with same-ID duplicates merged (the registry
// is hand-editable; the read side — serve — never trusts it to be
// canonical). Later entries win for display fields; the earliest Added is
// kept.
func (r *Registry) Deduped() []Entry {
	type slot struct {
		entry    Entry
		inserted int
	}
	byID := make(map[string]*slot)
	var order []string
	for i, entry := range r.Projects {
		id := projectID(entry.Path)
		if existing, ok := byID[id]; ok {
			if entry.Added.Before(existing.entry.Added) {
				added := entry.Added
				entry = existing.entry
				entry.Added = added
			}
			existing.entry = entry
			continue
		}
		byID[id] = &slot{entry: entry, inserted: i}
		order = append(order, id)
	}
	entries := make([]Entry, 0, len(order))
	for _, id := range order {
		entries = append(entries, byID[id].entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Added.Before(entries[j].Added) })
	return entries
}

// projectID derives the storage-layer project ID for a path via the
// canonical jsonl hash — one definition, no drift. Canonical(path) first
// so hand-entered relative or unclean paths hash identically to their
// registered forms.
func projectID(path string) string {
	return jsonl.ProjectID(Canonical(path))
}
