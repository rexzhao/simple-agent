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
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/sessionhost"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage/jsonl"
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
			ModelResolver: agent.NewStaticResolver(provider, nil, 0, 0, ""),
			Storage:       store,
			CWD:           projectCwd,
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
	ns := projects.ProjectID(projectDir)
	ag, err := agent.New(agent.Options{
		ModelResolver: agent.NewStaticResolver(provider, nil, 0, 0, ""),
		Storage:       store,
		CWD:           projectDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession(ns, agent.SessionMetaForTest)
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
		// N3 (§15.3): the unknown session carries its code now —
		// error has a value, the message keeps the original text.
		body := map[string]string{}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s on unregistered session: %d, want 404", p.method, p.suffix, resp.StatusCode)
		}
		if body["error"] != "unknown_session" {
			t.Fatalf("%s %s code: %q, want unknown_session", p.method, p.suffix, body["error"])
		}
	}

	// An invalid session id is 404, not 500 (no path fragments reach
	// the directory scan). The traversal form that actually arrives at
	// the API layer is the percent-encoded one: the client normalizes
	// literal ../ sequences away before the wire (they resolve against
	// the base URL), and the mux redirects whatever is left — the point
	// here is the encoded fragment stays on the API and never 500s.
	req, _ := http.NewRequest("GET", base.URL+"/api/s/..%2f..%2fetc/state", nil) //nolint:noctx // test
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("traversal id: %d, want 404", resp2.StatusCode)
	}
	// And the literal form, normalized client-side to a non-API path,
	// lands on the page redirect — never storage.
	req, _ = http.NewRequest("GET", base.URL+"/api/s/../../etc/state", nil) //nolint:noctx // test
	resp3, err := noRedirect().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode >= 500 || strings.HasPrefix(resp3.Header.Get("Location"), "/api") {
		t.Fatalf("normalized traversal: %d %q", resp3.StatusCode, resp3.Header.Get("Location"))
	}
}

// The sweeper tests (eviction of idle sessions, the run/refs/lease
// refusals) migrated to internal/sessionhost with the lifecycle
// itself: TestSweeperEvictsIdleSession and
// TestSweeperNeverEvictsActiveSession live there now.

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

	if got := len(manager.pool.OpenIDs()); got != len(ids) {
		t.Fatalf("open sessions = %d, want %d (double open?)", got, len(ids))
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
	holder, err := store.Open(projects.ProjectID(projectDir), id)
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
	if p.ID != projects.ProjectID(projectDir) || p.Path != projectDir {
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

// TestSubscriberBlocksEviction pins the EVICTION side of the guard
// wiring (§15.3): after the split an SSE subscriber holds no lease, so
// "watched ⇒ not evicted" rides entirely on EvictGuard. Unwiring it
// would let the sweeper tear the session (and its hub) down under the
// viewer — a silent semantic change. Both sides are asserted: watched
// survives many sweep periods; disconnected gets evicted (the store's
// OS lock released proves the teardown was real, not a map quirk).
func TestSubscriberBlocksEviction(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	// Shrink BEFORE construction: the pool's ticker captures the
	// interval at startup.
	oldEvict, oldInterval := sessionhost.SessionIdleEvict, sessionhost.SweeperInterval
	sessionhost.SessionIdleEvict, sessionhost.SweeperInterval = 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { sessionhost.SessionIdleEvict, sessionhost.SweeperInterval = oldEvict, oldInterval })
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)

	id := createJSONLSession(t, provider, store, projectDir)

	// Watch it: an SSE subscriber, no lease, no run.
	stream, err := http.Get(base.URL + "/api/s/" + id + "/stream") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	waitFor(t, 2*time.Second, func() bool {
		hub := manager.hubOf(id)
		return hub != nil && hub.Subscribers() > 0
	})

	// Many sweep periods pass while watched (15 intervals ≥ 3× the
	// idle window): the session must survive.
	time.Sleep(300 * time.Millisecond)
	if manager.hubOf(id) == nil {
		t.Fatal("the sweeper evicted a watched session — EvictGuard is not wired")
	}

	// Disconnect: eviction reclaims it (waitFor — the teardown is
	// asynchronous; hubOf turns nil only after the pool unlinked AND
	// the teardown hook ran).
	stream.Body.Close()
	waitFor(t, 2*time.Second, func() bool { return manager.hubOf(id) == nil })

	// The teardown was real: the OS lock is back. BOUNDED RETRY, not a
	// single Open — the pool closes the session AFTER the teardown
	// hook removed the hub (hubOf==nil is 2-4ms early on the reference
	// machine); a single Open would flake under load. The pool-level
	// twin observes the closing set instead (it can); the web test
	// retries the observable it has.
	waitFor(t, 2*time.Second, func() bool {
		holder, err := store.Open(projects.ProjectID(projectDir), id)
		if err != nil {
			return false
		}
		return holder.Close() == nil
	})
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
	if err := copyTree(t, filepath.Join(store.Root(), projects.ProjectID(projectDir), id),
		filepath.Join(store.Root(), projects.ProjectID(secondDir), id)); err != nil {
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
	// N3 (§15.3): the refusal carries the session_conflict code with
	// the original message — and nothing was opened on either side.
	body := map[string]string{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "session_conflict" {
		t.Fatalf("duplicate session code: %q, want session_conflict", body["error"])
	}
	if want := "multiple registered projects hold session " + id; body["message"] != want {
		t.Fatalf("duplicate session message: %q, want %q", body["message"], want)
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

// The web delete: happy path (idle or never-opened), the refusals
// (running, watched, locked, unregistered), and the convergence after
// deletion (404).
func TestDeleteSession(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)

	deleteSession := func(id string) (*http.Response, map[string]string) {
		req, _ := http.NewRequest("DELETE", base.URL+"/api/s/"+id, nil) //nolint:noctx // test
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := map[string]string{}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		return resp, body
	}

	// Unregistered: 404 (the gate covers deletion too).
	unregistered := createJSONLSession(t, provider, store, t.TempDir())
	resp, body := deleteSession(unregistered)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unregistered delete: %d %v", resp.StatusCode, body)
	}

	// Never-opened registered session: deleted.
	victim := createJSONLSession(t, provider, store, projectDir)
	resp, _ = deleteSession(victim)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("idle delete: %d", resp.StatusCode)
	}
	if store.HasSession(projects.ProjectID(projectDir), victim) {
		t.Fatal("session still exists after delete")
	}

	// Watched (an SSE subscriber — no lease refs after the split, the
	// InUseGuard carries the refusal): session_open.
	watched := createJSONLSession(t, provider, store, projectDir)
	stream, err := http.Get(base.URL + "/api/s/" + watched + "/stream") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	waitFor(t, 2*time.Second, func() bool {
		hub := manager.hubOf(watched)
		return hub != nil && hub.Subscribers() > 0
	})
	resp, body = deleteSession(watched)
	if resp.StatusCode != http.StatusConflict || body["error"] != "session_open" {
		t.Fatalf("watched delete: %d %v", resp.StatusCode, body)
	}
	stream.Body.Close()

	// Running: run_active.
	running := createJSONLSession(t, provider, store, projectDir)
	if resp, err := http.Post(base.URL+"/api/s/"+running+"/send", "application/json", strings.NewReader(`{"text":"go"}`)); err != nil || resp.StatusCode != 200 { //nolint:noctx // test
		t.Fatalf("send: %v %d", err, resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp, body = deleteSession(running)
	if resp.StatusCode != http.StatusConflict || body["error"] != "run_active" {
		t.Fatalf("running delete: %d %v", resp.StatusCode, body)
	}
	provider.release()

	// Externally locked: session_locked.
	locked := createJSONLSession(t, provider, store, projectDir)
	holder, err := store.Open(projects.ProjectID(projectDir), locked)
	if err != nil {
		t.Fatal(err)
	}
	resp, body = deleteSession(locked)
	if resp.StatusCode != http.StatusConflict || body["error"] != "session_locked" {
		t.Fatalf("locked delete: %d %v", resp.StatusCode, body)
	}
	holder.Close()

	// After the refusals clear, deletion succeeds and later requests
	// converge to 404.
	resp, _ = deleteSession(locked)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-refusal delete: %d", resp.StatusCode)
	}
	resp, err = http.Get(base.URL + "/api/s/" + locked + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("state after delete: %d, want 404", resp.StatusCode)
	}
}
