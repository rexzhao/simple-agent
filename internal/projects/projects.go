// Package projects is the serve-mode project registry: the list of
// workspace directories a serve process is authorized to serve. The
// registry is a plain JSON file — the interface between `sai project add`
// and a running serve is the file itself (serve re-reads on demand; a
// rename-atomic replace makes concurrent readers see either the old or the
// new whole document, never a torn one).
package projects

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
)

// Entry is one registered project. Path is stored canonicalized (what
// `sai project add` writes — the same normalization ProjectID hashes), so
// the ID is always derivable; the "original spelling" caveat of the
// design docs is about what the USER must re-enter to recover a moved
// project, not about what the file holds.
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
// add creates it); so is a blank one (touch or an emptied file must not
// lock every entry point); a corrupt document is an error — callers fail
// fast on it (serve startup exits, `sai project add` refuses to write:
// read-modify-rename over a corrupt file would silently drop every other
// project).
//
// A concurrent Save's rename can make the file briefly unopenable on
// Windows (no FILE_SHARE_DELETE): transient sharing conflicts are retried
// — the file is the interface, and its replacements are atomic elsewhere.
func Load(path string) (*Registry, error) {
	var data []byte
	err := readFileWithRetry(path, func() (err error) {
		data, err = os.ReadFile(path)
		return
	})
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{}, nil
		}
		return nil, fmt.Errorf("read registry: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return &Registry{}, nil
	}
	var registry Registry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("registry %s is corrupt: %w", path, err)
	}
	return &registry, nil
}

// Save writes the registry atomically: a unique temp file in the same
// directory + rename, so a concurrent reader sees the old or the new
// document, never a torn one — and two concurrent Saves never share a
// staging path (a fixed sibling name would let one writer publish the
// other writer's half-written document; last writer wins whole).
func (r *Registry) Save(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("stage registry: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName) // no-op after a successful rename
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return fmt.Errorf("write registry staging: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close registry staging: %w", err)
	}
	if err := renameWithRetry(tempName, path); err != nil {
		return fmt.Errorf("activate registry: %w", err)
	}
	return nil
}

// renameRetry is the retry budget for Windows sharing conflicts: a
// concurrent reader holding the target without FILE_SHARE_DELETE makes
// MoveFileEx fail; the conflict window is a reader's open-close span, so a
// bounded short-backoff retry converges in practice.
var renameRetry = 25

// sharingConflict reports whether an error is a transient Windows
// sharing violation (rename-vs-open races on the registry file).
func sharingConflict(err error) bool {
	if err == nil {
		return false
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		err = linkErr.Err
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	message := err.Error()
	return strings.Contains(message, "Access is denied") ||
		strings.Contains(message, "being used by another process") ||
		strings.Contains(message, "used by another process")
}

// renameWithRetry renames, retrying transient sharing conflicts.
func renameWithRetry(oldPath, newPath string) error {
	var err error
	for attempt := 0; attempt < renameRetry; attempt++ {
		if err = os.Rename(oldPath, newPath); err == nil {
			return nil
		}
		if !sharingConflict(err) {
			return err
		}
		time.Sleep(2 * time.Millisecond)
	}
	return err
}

// readFileWithRetry runs read once, retrying transient sharing conflicts.
func readFileWithRetry(path string, read func() error) error {
	var err error
	for attempt := 0; attempt < renameRetry; attempt++ {
		if err = read(); err == nil {
			return nil
		}
		if !sharingConflict(err) {
			return err
		}
		time.Sleep(2 * time.Millisecond)
	}
	return err
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
// canonical). The later entry wins display fields; the earliest Added is
// kept — "when was this project first registered" is the question the
// field answers.
func (r *Registry) Deduped() []Entry {
	index := make(map[string]int) // project ID → position in out
	var out []Entry
	for _, entry := range r.Projects {
		id := projectID(entry.Path)
		if idx, ok := index[id]; ok {
			merged := out[idx]
			merged.Path = entry.Path
			if entry.Added.Before(merged.Added) {
				merged.Added = entry.Added
			}
			out[idx] = merged
			continue
		}
		index[id] = len(out)
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Added.Before(out[j].Added) })
	return out
}

// projectID derives the storage-layer project ID for a path via the
// canonical jsonl hash — one definition, no drift. Canonical(path) first
// so hand-entered relative or unclean paths hash identically to their
// registered forms.
func projectID(path string) string {
	return jsonl.ProjectID(Canonical(path))
}
