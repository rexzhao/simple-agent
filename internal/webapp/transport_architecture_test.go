package webapp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// G1 is a repository-wide clean break, not just a Go handler deletion. Keep
// the production source, browser source, Playwright fixtures, and generated
// browser payloads unable to grow a second event transport again.
func TestNoLegacySSETransportMarkers(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	forbidden := []string{
		"text/" + "event-stream",
		"Event" + "Source",
		"stream" + "Lifecycle",
		"stream" + "Run",
		"/api/" + "events",
		"/api/runs/" + "events",
		"/api/runs/{runID}/" + "events",
	}

	checkTree := func(relative string, include func(string) bool) {
		directory := filepath.Join(root, relative)
		err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			name := filepath.Base(path)
			if !include(name) {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(content)
			for _, marker := range forbidden {
				if strings.Contains(text, marker) {
					t.Errorf("%s contains removed legacy transport marker %q", filepath.ToSlash(filepath.Join(relative, name)), marker)
				}
			}
			// The run identifier is dynamic in the route, so a literal marker
			// cannot cover every spelling of the removed per-run endpoint.
			if strings.Contains(text, "/api/runs/") && strings.Contains(text, "events") {
				t.Errorf("%s contains a removed per-run events route marker", filepath.ToSlash(filepath.Join(relative, name)))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", relative, err)
		}
	}

	checkTree("internal/webapp", func(name string) bool {
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	})
	checkTree("web/src", func(name string) bool {
		return strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".tsx")
	})
	checkTree("web/e2e", func(name string) bool {
		return strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".tsx")
	})
	checkTree("internal/webapp/assets", func(name string) bool {
		return strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".css") || strings.HasSuffix(name, ".html")
	})
}
