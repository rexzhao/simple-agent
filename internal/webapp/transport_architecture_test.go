package webapp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

// TestProductionHTTPRouteAllowlist makes the HTTP clean break executable. A
// new product route must be added deliberately to this test and to the
// allowlist below; provider SDK HTTP/SSE traffic and the typed WebSocket
// command/resource transport are intentionally outside this scan.
func TestProductionHTTPRouteAllowlist(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	allowed := map[string]bool{
		"HandleFunc GET /api/bootstrap":                          true,
		"HandleFunc POST /api/ws-ticket":                         true,
		"HandleFunc GET /api/ws":                                 true,
		"HandleFunc GET /api/blobs/{blobID}":                     true,
		"HandleFunc POST /api/sessions/{sessionID}/images":       true,
		"HandleFunc GET /api/sessions/{sessionID}/images/{hash}": true,
		"Handle /": true,
	}
	seen := make(map[string]int)
	webappDir := filepath.Join(root, "internal", "webapp")
	if err := filepath.WalkDir(webappDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), "_test.go") || !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "HandleFunc" && selector.Sel.Name != "Handle") {
				return true
			}
			// syscall.Handle is an unrelated Windows API call, not a mux
			// registration. Any other Handle/HandleFunc call is intentionally
			// treated as a possible production HTTP registration.
			if selector.Sel.Name == "Handle" {
				if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "syscall" {
					return true
				}
			}
			if len(call.Args) == 0 {
				t.Errorf("%s: %s registration has no route pattern", filepath.ToSlash(path), selector.Sel.Name)
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Errorf("%s: %s registration must use a literal route pattern", filepath.ToSlash(path), selector.Sel.Name)
				return true
			}
			pattern, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Errorf("%s: invalid route pattern %q: %v", filepath.ToSlash(path), literal.Value, err)
				return true
			}
			key := selector.Sel.Name + " " + pattern
			seen[key]++
			if !allowed[key] {
				t.Errorf("%s: production HTTP route %q is outside the clean-break allowlist", filepath.ToSlash(path), key)
			}
			return true
		})
		return nil
	}); err != nil {
		t.Fatalf("scan production HTTP registrations: %v", err)
	}
	for route := range allowed {
		if seen[route] != 1 {
			t.Errorf("production HTTP route %q registered %d times, want exactly once", route, seen[route])
		}
	}

	forbiddenProductionSymbols := []string{
		"handleListProjects", "handleCreateProject", "handleRenameProject",
		"handleArchiveProject", "handleRestoreProject", "handleRemoveProject",
		"handleSessionModels", "handleProviderSettings", "handleCreateProvider",
		"handleUpdateProvider", "handleUpdateDefaultProviderModel", "handleDiscoverProviderModels",
		"handleStartCodexLogin", "handleCodexLoginStatus", "handleClearCodexLogin", "handleCodexUsage",
		"handleListSessions", "handleCreateSession", "handleGetSession", "handleGetSessionSnapshot",
		"handleRenameSession", "handleSetSessionFullAccess", "handleSetSessionDebug",
		"handleArchiveSession", "handleRestoreSession", "handleRemoveSession", "handleSessionItems",
		"handleCompactSession", "handleStartRun", "handleContinueRun", "handleListActiveRuns",
		"handleCancelRun", "handleCancelToolCall", "handleAppendActive", "handleRemoveActivePrompt",
		"handleSteerActivePrompt", "handleMoveActivePrompt", "activeRunSnapshot", "startRunRequest",
	}
	if err := filepath.WalkDir(webappDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), "_test.go") || !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, marker := range forbiddenProductionSymbols {
			if strings.Contains(string(content), marker) {
				t.Errorf("%s contains removed REST-only production symbol %q", filepath.ToSlash(path), marker)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("scan production webapp symbols: %v", err)
	}

	apiSource, err := os.ReadFile(filepath.Join(root, "web", "src", "api.ts"))
	if err != nil {
		t.Fatal(err)
	}
	apiText := string(apiSource)
	for _, marker := range []string{
		"/api/projects", "/api/provider-settings", "/api/providers", "/api/provider-default",
		"/api/runs", "/api/ws-ticket", "/api/blobs/",
	} {
		if strings.Contains(apiText, marker) {
			t.Errorf("web/src/api.ts references removed or independent HTTP surface %q", marker)
		}
	}
	if !strings.Contains(apiText, "request<Bootstrap>('/api/bootstrap')") || !strings.Contains(apiText, "/api/sessions/${encodeURIComponent(sessionID)}/images/") {
		t.Error("web/src/api.ts must retain only bootstrap and session-image HTTP product clients")
	}
}
