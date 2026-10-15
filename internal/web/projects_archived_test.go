package web

// /api/projects?include_archived=1 endpoint tests (C6b, §17.3): the
// archived rows appear flagged in the all view, stay hidden in the
// default one, and the two variants cache independently.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rexzhao/simple-agent/internal/projects"
)

func projectsBodyQuery(t *testing.T, base *httptest.Server, query string) map[string]any {
	t.Helper()
	resp, err := http.Get(base.URL + "/api/projects" + query) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// rowsOf extracts the (id, archived) pairs of one project's sessions.
func rowsOf(t *testing.T, body map[string]any) map[string]bool {
	t.Helper()
	projs, _ := body["projects"].([]any)
	if len(projs) != 1 {
		t.Fatalf("projects = %d, want 1", len(projs))
	}
	sessions, _ := projs[0].(map[string]any)["sessions"].([]any)
	rows := map[string]bool{}
	for _, s := range sessions {
		m, _ := s.(map[string]any)
		archived, _ := m["archived"].(bool)
		rows[m["id"].(string)] = archived
	}
	return rows
}

func TestProjectsIncludeArchived(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)
	live := createJSONLSession(t, provider, store, projectDir)
	parked := createJSONLSession(t, provider, store, projectDir)
	ns := projects.ProjectID(projectDir)
	if err := store.Archive(ns, parked); err != nil {
		t.Fatal(err)
	}

	// Default view: the parked row is hidden.
	rows := rowsOf(t, projectsBodyQuery(t, base, ""))
	if len(rows) != 1 {
		t.Fatalf("default rows = %v, want only the live one", rows)
	}
	if _, ok := rows[live]; !ok {
		t.Fatalf("default rows = %v, want %s", rows, live)
	}

	// All view: both rows, the parked one flagged.
	rows = rowsOf(t, projectsBodyQuery(t, base, "?include_archived=1"))
	if len(rows) != 2 {
		t.Fatalf("all rows = %v, want both", rows)
	}
	if rows[live] {
		t.Fatal("the live session is flagged archived")
	}
	if !rows[parked] {
		t.Fatal("the parked session is not flagged archived")
	}

	// The two variants cache independently: after the archive, the
	// cached DEFAULT body must not leak the parked row through the all
	// view (and vice versa) — hit both again within the TTL.
	rows = rowsOf(t, projectsBodyQuery(t, base, ""))
	if _, ok := rows[parked]; ok {
		t.Fatal("the default view leaked the archived row after the all view")
	}
	rows = rowsOf(t, projectsBodyQuery(t, base, "?include_archived=1"))
	if !rows[parked] {
		t.Fatal("the all view lost the archived row after the default view")
	}

	// Unarchive: both views converge back (after the 1s cache TTL —
	// poll for it rather than sleeping blind).
	if err := store.Unarchive(ns, parked); err != nil {
		t.Fatal(err)
	}
	waitFor(t, projectsCacheTTL*4, func() bool {
		rows := rowsOf(t, projectsBodyQuery(t, base, "?include_archived=1"))
		return len(rows) == 2 && !rows[parked]
	})
}
