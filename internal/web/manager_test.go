package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
)

// newJSONLTestServer builds a manager over a REAL jsonl store (temp
// server root, registered project dir): eviction, locks, and listings
// behave exactly as in production.
func newJSONLTestServer(t *testing.T, provider model.Provider) (*Manager, *Server, *httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	projectDir := t.TempDir()
	registry := &projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
	}}
	if err := registry.Save(projects.FilePath(root)); err != nil {
		t.Fatal(err)
	}
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	factory := func(projectCwd string) (*agent.Agent, error) {
		return agent.New(agent.Options{
			Provider:     provider,
			ProviderName: "test",
			ModelID:      "model-test",
			Storage:      store,
			ProjectID:    jsonl.ProjectID(projectCwd),
			CWD:          projectCwd,
		})
	}
	manager, err := NewManager(ManagerOptions{
		ServerRoot: root,
		Store:      store,
		AgentFor:   factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := New(manager)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		server.Close()
	})
	return manager, server, httpServer, projectDir
}

// createJSONLSession makes a materialized session under the project
// through a factory-built agent, returning its id (the session is
// closed: the manager opens its own).
func createJSONLSession(t *testing.T, provider model.Provider, store *jsonl.Store, projectDir string) string {
	t.Helper()
	ag, err := agent.New(agent.Options{
		Provider:     provider,
		ProviderName: "test",
		ModelID:      "model-test",
		Storage:      store,
		ProjectID:    jsonl.ProjectID(projectDir),
		CWD:          projectDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	return session.ID()
}

// jsonlStoreOf digs the store out of the manager for session creation.
func jsonlStoreOf(t *testing.T, m *Manager) *jsonl.Store {
	store, ok := m.store.(*jsonl.Store)
	if !ok {
		t.Fatal("manager is not over a jsonl store")
	}
	return store
}

// The registration gate is per-route: a session living under an
// UNREGISTERED project is 404 on every session endpoint, even though
// the store can see its directory.
func TestRegistrationGatePerRoute(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)

	// A session under the REGISTERED project opens fine.
	registeredID := createJSONLSession(t, provider, store, projectDir)
	resp, err := http.Get(base.URL + "/api/s/" + registeredID + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("registered session state: %d", resp.StatusCode)
	}

	// A session under an UNREGISTERED project: created through a store
	// rooted at the same data dir but a different project.
	unregisteredDir := t.TempDir()
	unregisteredID := createJSONLSession(t, provider, store, unregisteredDir)

	paths := []struct {
		method, suffix string
	}{
		{"GET", "/stream"},
		{"GET", "/state"},
		{"GET", "/history?before=x"},
		{"POST", "/send"},
		{"POST", "/steer"},
		{"POST", "/compact"},
		{"POST", "/stop"},
	}
	for _, p := range paths {
		req, _ := http.NewRequest(p.method, base.URL+"/api/s/"+unregisteredID+p.suffix, strings.NewReader(`{}`)) //nolint:noctx // test
		if p.method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s on unregistered session: %d, want 404", p.method, p.suffix, resp.StatusCode)
		}
	}

	// An invalid session id is 404, not 500 (no path fragments reach
	// the directory scan).
	req, _ := http.NewRequest("GET", base.URL+"/api/s/../../etc/state", nil) //nolint:noctx // test
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("traversal id: %d, want 404", resp2.StatusCode)
	}
}

// The sweeper evicts an idle session: the OS lock is released (a fresh
// open succeeds), the entry is gone, and a later request reopens it
// transparently.
func TestSweeperEvictsIdleSession(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	// Shrink the timing BEFORE the manager starts its sweeper: the
	// ticker interval is captured at construction.
	oldEvict, oldInterval := SessionIdleEvict, sweeperInterval
	SessionIdleEvict, sweeperInterval = 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { SessionIdleEvict, sweeperInterval = oldEvict, oldInterval })
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)

	id := createJSONLSession(t, provider, store, projectDir)

	// Open it through a request; then let it go idle. Unlinking from
	// the map precedes teardown by design — wait for the CLOSING SET
	// to clear instead: that is the point where the lock is released
	// (waiting for the unlink alone races the closing window).
	resp, err := http.Get(base.URL + "/api/s/" + id + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first state: %d", resp.StatusCode)
	}
	waitFor(t, 2*time.Second, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		_, busy := manager.closing[id]
		_, open := manager.sessions[id]
		return !busy && !open
	})

	// The lock is really released: another session open (a second agent
	// holding the storage lock) succeeds now.
	holder, err := store.OpenIn(jsonl.ProjectID(projectDir), id)
	if err != nil {
		t.Fatalf("lock not released after eviction: %v", err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}

	// A later request transparently reopens.
	resp, err = http.Get(base.URL + "/api/s/" + id + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reopen after eviction: %d", resp.StatusCode)
	}
}

// The sweeper never evicts a session with an in-flight request (refs)
// or an active run.
func TestSweeperNeverEvictsActiveSession(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	// Shrink the timing BEFORE the manager starts its sweeper: the
	// ticker interval is captured at construction.
	oldEvict, oldInterval := SessionIdleEvict, sweeperInterval
	SessionIdleEvict, sweeperInterval = 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { SessionIdleEvict, sweeperInterval = oldEvict, oldInterval })
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)

	id := createJSONLSession(t, provider, store, projectDir)

	// A gated run: Running stays true; several sweep ticks pass.
	resp, err := http.Post(base.URL+"/api/s/"+id+"/send", "application/json", strings.NewReader(`{"text":"go"}`)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send: %d", resp.StatusCode)
	}
	time.Sleep(300 * time.Millisecond)
	manager.mu.Lock()
	_, open := manager.sessions[id]
	manager.mu.Unlock()
	if !open {
		t.Fatal("sweeper evicted a session with an active run")
	}

	// A held refs (in-flight request simulated through open+release
	// omission — resolve and never release) blocks eviction too.
	entry, release, _, err := manager.open(id)
	if err != nil {
		t.Fatal(err)
	}
	_ = entry
	time.Sleep(300 * time.Millisecond)
	manager.mu.Lock()
	_, open = manager.sessions[id]
	manager.mu.Unlock()
	if !open {
		t.Fatal("sweeper evicted a session with in-flight refs")
	}
	release()
	provider.release() // let the gated run settle: Running blocks eviction

	// Now idle: eviction reclaims it (same closing-set condition).
	waitFor(t, 2*time.Second, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		_, busy := manager.closing[id]
		_, open := manager.sessions[id]
		return !busy && !open
	})
}

// Concurrent first requests for the same session: every response is
// 200 or (transiently) 503, and the session opens exactly once. This
// is the guard for the "IO out of the critical section" upgrade path —
// a double open would surface as unexplained 503s, not corruption.
func TestConcurrentFirstRequestOpensOnce(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)

	ids := make([]string, 4)
	for i := range ids {
		ids[i] = createJSONLSession(t, provider, store, projectDir)
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				resp, err := http.Get(base.URL + "/api/s/" + id + "/state") //nolint:noctx // test
				if err != nil {
					t.Errorf("state: %v", err)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
					t.Errorf("state: %d, want 200 or 503", resp.StatusCode)
				}
			}(id)
		}
	}
	wg.Wait()

	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, id := range ids {
		if len(manager.sessions) != len(ids) {
			t.Fatalf("sessions map = %d entries, want %d (double open?)", len(manager.sessions), len(ids))
		}
		_ = id
	}
}

// A session lock held by another process-handle (here: a second
// in-process storage open, which conflicts on both flock and
// LockFileEx) surfaces as 409 session_locked, not a 500 or a string.
func TestOpenLockedSessionIs409(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)

	id := createJSONLSession(t, provider, store, projectDir)
	holder, err := store.OpenIn(jsonl.ProjectID(projectDir), id)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	resp, err := http.Get(base.URL + "/api/s/" + id + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("locked session: %d, want 409", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "session_locked" {
		t.Fatalf("code = %q, want session_locked", body["error"])
	}
}

// Two serves on one root: the second fails fast on the root lock.
func TestRootLockRejectsSecondServe(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, _, _ := newJSONLTestServer(t, provider)

	_, err := NewManager(ManagerOptions{
		ServerRoot: manager.root,
		Store:      manager.store,
		AgentFor:   func(string) (*agent.Agent, error) { return nil, errors.New("unused") },
	})
	if err == nil || !strings.Contains(err.Error(), "another serve") {
		t.Fatalf("second serve: %v, want root-lock failure", err)
	}
}

// /api/projects: registry entries with their storage-listed sessions,
// the `active` merge, and the runtime-corruption behavior (last good
// table survives, registry_ok=false).
func TestProjectsEndpoint(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)

	first := createJSONLSession(t, provider, store, projectDir)
	second := createJSONLSession(t, provider, store, projectDir)

	// Open one through a request so it shows active.
	resp, err := http.Get(base.URL + "/api/s/" + first + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	body := fetchProjects(t, base.URL)
	if len(body.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(body.Projects))
	}
	p := body.Projects[0]
	if p.ID != jsonl.ProjectID(projectDir) || p.Path != projectDir {
		t.Fatalf("project = %+v", p)
	}
	if len(p.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(p.Sessions))
	}
	active := map[string]bool{}
	for _, s := range p.Sessions {
		active[s.ID] = s.Active
	}
	if !active[first] || active[second] {
		t.Fatalf("active flags = %v (first opened, second not)", active)
	}
	if !body.RegistryOK {
		t.Fatal("registry_ok = false on a healthy registry")
	}

	// Corrupt the registry at runtime: the next listing keeps the last
	// good table and reports registry_ok=false — never an empty list.
	if err := os.WriteFile(projects.FilePath(manager.root), []byte(`{"projects":[{`), 0o644); err != nil {
		t.Fatal(err)
	}
	manager.projectsMu.Lock()
	manager.projectsCache, manager.projectsCached = nil, time.Time{}
	manager.projectsMu.Unlock()

	body = fetchProjects(t, base.URL)
	if body.RegistryOK {
		t.Fatal("registry_ok should flag the corrupt reload")
	}
	if len(body.Projects) != 1 || len(body.Projects[0].Sessions) != 2 {
		t.Fatalf("last good table lost: %+v", body.Projects)
	}
}

// Preopen goes through the registration gate: an unregistered session
// errors instead of opening.
func TestPreopenGate(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, _, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)

	if err := manager.Preopen("s0000doesnotexist00"); err == nil {
		t.Fatal("preopen of an unknown session succeeded")
	}
	unregistered := createJSONLSession(t, provider, store, t.TempDir())
	if err := manager.Preopen(unregistered); err == nil {
		t.Fatal("preopen of an unregistered project's session succeeded")
	}
	registered := createJSONLSession(t, provider, store, projectDir)
	if err := manager.Preopen(registered); err != nil {
		t.Fatalf("preopen of a registered session: %v", err)
	}
}

// waitFor polls a condition until the deadline.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before the deadline")
}

func fetchProjects(t *testing.T, base string) projectsBody {
	t.Helper()
	resp, err := http.Get(base + "/api/projects") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/projects: %d", resp.StatusCode)
	}
	var body projectsBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// Close is idempotent: the serve shutdown has two entry points (the
// interrupt goroutine and the console loop) that can race.
func TestManagerCloseIdempotent(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)
	id := createJSONLSession(t, provider, store, projectDir)
	resp, err := http.Get(base.URL + "/api/s/" + id + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	manager.Close()
	manager.Close() // must not panic (close of closed channel)
}

// The same session id under two REGISTERED projects (hand-copied
// directories) is refused, not silently resolved to the first.
func TestDuplicateSessionAcrossProjectsIs409(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)
	id := createJSONLSession(t, provider, store, projectDir)

	// Copy the session directory into a second registered project.
	secondDir := t.TempDir()
	registry := &projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
		{Path: secondDir, Added: time.Now().UTC()},
	}}
	if err := registry.Save(projects.FilePath(manager.root)); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(t, filepath.Join(store.Root(), jsonl.ProjectID(projectDir), id),
		filepath.Join(store.Root(), jsonl.ProjectID(secondDir), id)); err != nil {
		t.Fatal(err)
	}
	manager.projectsMu.Lock()
	manager.projectsCache, manager.projectsCached = nil, time.Time{}
	manager.projectsMu.Unlock()

	resp, err := http.Get(base.URL + "/api/s/" + id + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate session: %d, want 409", resp.StatusCode)
	}
}

// copyTree copies a session directory (files only).
func copyTree(t *testing.T, src, dst string) error {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
