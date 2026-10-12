package main

import (
	"fmt"
	"os"
	"time"

	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
)

// projectCommand implements the `sai project` subcommands — the management
// side of the serve-mode registry (projects.json). It runs before any
// agent assembly: registry work needs only the server root.
//
//	sai project add <path>     register (canonicalized, deduplicated)
//	sai project remove <path>  unregister
//	sai project list           list registered projects
func projectCommand(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	// The registry path needs the same server-root resolution main()
	// performs; this runs after that resolution set up the root.
	switch args[0] {
	case "add":
		if len(args) != 2 {
			usage()
			return 2
		}
		return projectAdd(args[1])
	case "remove":
		if len(args) != 2 {
			usage()
			return 2
		}
		return projectRemove(args[1])
	case "list":
		if len(args) != 1 {
			usage()
			return 2
		}
		return projectList()
	default:
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "sai: usage: sai project add <path> | sai project remove <path> | sai project list")
}

// registryPath resolves the registry location from the already-resolved
// server root (main resolves the flag/env before dispatching the
// subcommand).
func registryPath() string {
	return projects.FilePath(resolvedServerRoot)
}

func projectAdd(path string) int {
	registry, err := projects.Load(registryPath())
	if err != nil {
		// A corrupt registry must refuse the write: read-modify-rename
		// over it would silently drop every other project.
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		return 1
	}
	canonical := projects.Canonical(path)
	if _, exists := registry.Lookup(canonical); exists {
		fmt.Fprintf(os.Stderr, "sai: already registered: %s\n", canonical)
		return 0
	}
	registry.Projects = append(registry.Projects, projects.Entry{
		Path:  canonical,
		Added: time.Now().UTC(),
	})
	if err := registry.Save(registryPath()); err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "sai: registered %s (%s)\n", canonical, jsonl.ProjectID(canonical))
	return 0
}

func projectRemove(path string) int {
	registry, err := projects.Load(registryPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		return 1
	}
	canonical := projects.Canonical(path)
	if registry.Remove(canonical) == 0 {
		fmt.Fprintf(os.Stderr, "sai: not registered: %s\n", canonical)
		return 1
	}
	if err := registry.Save(registryPath()); err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "sai: unregistered %s\n", canonical)
	return 0
}

func projectList() int {
	registry, err := projects.Load(registryPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		return 1
	}
	entries := registry.Deduped()
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "sai: no registered projects")
		return 0
	}
	for _, entry := range entries {
		fmt.Printf("%s  %s  added %s\n", jsonl.ProjectID(entry.Path), entry.Path, entry.Added.Format("2006-01-02"))
	}
	return 0
}
