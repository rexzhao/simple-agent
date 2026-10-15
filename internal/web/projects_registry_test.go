package web

// Project register/unregister endpoint tests (C8, §17.4): the path
// checks, canonicalization, idempotency, the lock discipline, the
// open-session refusal, the data-preserving unregister, and the
// concurrent-write integrity.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/storage/lockfile"
)

func registerPost(t *testing.T, base *httptest.Server, path string) (*http.Response, map[string]string) {
	t.Helper()
	resp, err := http.Post(base.URL+"/api/projects", "application/json",
		strings.NewReader(fmt.Sprintf(`{"path": %q}`, path))) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]string{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	return resp, body
}

func unregisterDelete(t *testing.T, base *httptest.Server, pid string) (*http.Response, map[string]string) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", base.URL+"/api/projects/"+pid, nil) //nolint:noctx // test
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]string{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	return resp, body
}

// projectsOf lists the registered project ids through the endpoint.
func projectsOf(t *testing.T, base *httptest.Server) map[string]bool {
	t.Helper()
	resp, err := http.Get(base.URL + "/api/projects") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, p := range body.Projects {
		ids[p.ID] = true
	}
	return ids
}

func TestRegisterProjectEndpoint(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	_, _, base, _ := newJSONLTestServer(t, provider)

	dir := t.TempDir()

	// A new directory registers: 201 with the id and canonical path.
	resp, body := registerPost(t, base, dir)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %v", resp.StatusCode, body)
	}
	if body["id"] != projects.ProjectID(dir) || body["path"] != projects.Canonical(dir) {
		t.Fatalf("register body = %v", body)
	}

	// The same path again: the idempotent 200.
	resp, body = registerPost(t, base, dir)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-register: %d %v", resp.StatusCode, body)
	}

	// A SECOND SPELLING of the same path is the same project (the
	// canonicalization kill's target): no second entry.
	spelling := dir + string([]byte{'/', '.', '/'})
	if Canonical := projects.Canonical(spelling); Canonical == projects.Canonical(dir) {
		resp, body = registerPost(t, base, spelling)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("re-spelling: %d %v", resp.StatusCode, body)
		}
	} else {
		t.Fatalf("test spelling does not canonicalize to the dir: %q vs %q", Canonical, projects.Canonical(dir))
	}

	// A nonexistent path and a plain file refuse with invalid_path.
	resp, body = registerPost(t, base, dir+"/no-such-dir")
	if resp.StatusCode != http.StatusBadRequest || body["error"] != "invalid_path" {
		t.Fatalf("nonexistent: %d %v", resp.StatusCode, body)
	}
	file := dir + "/plain-file"
	if err := writeFile(file, "x"); err != nil {
		t.Fatal(err)
	}
	resp, body = registerPost(t, base, file)
	if resp.StatusCode != http.StatusBadRequest || body["error"] != "invalid_path" {
		t.Fatalf("file: %d %v", resp.StatusCode, body)
	}

	// The listing carries the new project (the stat-gated registry
	// cache reloads on the renamed file; the 1s body cache bounds the
	// delay — poll for it).
	waitFor(t, projectsCacheTTL*4, func() bool {
		return projectsOf(t, base)[projects.ProjectID(dir)]
	})
}

// writeFile registers a plain file (the not-a-directory refusal).
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestUnregisterProjectEndpoint(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, projectDir := newJSONLTestServer(t, provider)
	store := jsonlStoreOf(t, manager)
	ns := projects.ProjectID(projectDir)

	// Unknown pid: 404 unknown_project.
	resp, body := unregisterDelete(t, base, "p0000000000000000")
	if resp.StatusCode != http.StatusNotFound || body["error"] != "unknown_project" {
		t.Fatalf("unknown: %d %v", resp.StatusCode, body)
	}

	// An OPEN session refuses: 409 project_in_use.
	id := createJSONLSession(t, provider, store, projectDir)
	lease, err := manager.pool.Acquire(id)
	if err != nil {
		t.Fatal(err)
	}
	resp, body = unregisterDelete(t, base, ns)
	if resp.StatusCode != http.StatusConflict || body["error"] != "project_in_use" {
		t.Fatalf("in-use: %d %v", resp.StatusCode, body)
	}
	lease.Release()

	lease.Release()
	// The pooled entry outlives its lease (it holds the session's OS
	// lock until eviction) — an IDLE-OPEN session still counts as in
	// use; that is the design's reading of "打开会话".
	resp, body = unregisterDelete(t, base, ns)
	if resp.StatusCode != http.StatusConflict || body["error"] != "project_in_use" {
		t.Fatalf("idle-open: %d %v", resp.StatusCode, body)
	}

	// A DIFFERENT project with disk sessions but NOTHING open:
	// allowed, the data stays (the user's ruling).
	other := t.TempDir()
	resp, body = registerPost(t, base, other)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register other: %d %v", resp.StatusCode, body)
	}
	otherNS := projects.ProjectID(other)
	diskID := createJSONLSession(t, provider, store, other)
	resp, body = unregisterDelete(t, base, otherNS)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unregister disk-only: %d %v", resp.StatusCode, body)
	}
	if !store.HasSession(otherNS, diskID) {
		t.Fatal("the unregister deleted disk data")
	}

	// The listing drops the unregistered project and the Locate gate
	// refuses new opens of its sessions (unknown_session).
	waitFor(t, projectsCacheTTL*4, func() bool {
		return !projectsOf(t, base)[otherNS]
	})
	stateResp, err := http.Get(base.URL + "/api/s/" + diskID + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusNotFound {
		t.Fatalf("state after unregister: %d, want 404", stateResp.StatusCode)
	}
}

// TestRegistryLockedRefuses: a held registry lock turns the register
// endpoint into 409 registry_locked after the bounded retry.
func TestRegistryLockedRefuses(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, _ := newJSONLTestServer(t, provider)

	lock, err := lockfile.AcquirePathLock(projects.LockPath(manager.root))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	dir := t.TempDir()
	start := time.Now()
	resp, body := registerPost(t, base, dir)
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusConflict || body["error"] != "registry_locked" {
		t.Fatalf("locked: %d %v", resp.StatusCode, body)
	}
	// The bounded retry actually retried (not an instant refusal),
	// and did not exceed the budget by much.
	if elapsed < 90*time.Millisecond {
		t.Fatalf("the lock refusal was instant (%v) — no retry", elapsed)
	}
}

// TestConcurrentRegistrations: parallel registers of different paths
// all land (the lock keeps the read-modify-write from losing updates),
// and parallel registers of the SAME path produce exactly one entry.
func TestConcurrentRegistrations(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	manager, _, base, _ := newJSONLTestServer(t, provider)

	// Four parallel writers fit the design's 100ms lock budget (each
	// Mutate cycle is a load + temp-file + rename; more than that
	// correctly surfaces registry_locked, the retryable code).
	const n = 4
	dirs := make([]string, n)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}
	var wg sync.WaitGroup
	for _, dir := range dirs {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			resp, _ := registerPost(t, base, dir)
			if resp.StatusCode != http.StatusCreated {
				t.Errorf("concurrent register %s: %d", dir, resp.StatusCode)
			}
		}(dir)
	}
	wg.Wait()

	registry, err := projects.Load(projects.FilePath(manager.root))
	if err != nil {
		t.Fatalf("the registry is corrupt after concurrent writes: %v", err)
	}
	for _, dir := range dirs {
		if _, ok := registry.Lookup(projects.Canonical(dir)); !ok {
			t.Fatalf("the concurrent register of %s was lost", dir)
		}
	}

	// Same path, concurrently: one entry, one 201, the rest 200.
	var wg2 sync.WaitGroup
	var created int
	var mu sync.Mutex
	for i := 0; i < 6; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			resp, _ := registerPost(t, base, dirs[0])
			mu.Lock()
			defer mu.Unlock()
			if resp.StatusCode == http.StatusCreated {
				created++
			} else if resp.StatusCode != http.StatusOK {
				t.Errorf("same-path concurrent register: %d", resp.StatusCode)
			}
		}()
	}
	wg2.Wait()
	registry, err = projects.Load(projects.FilePath(manager.root))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range registry.Deduped() {
		if entry.Path == projects.Canonical(dirs[0]) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("same-path concurrent registers left %d entries, want 1", count)
	}
}

// TestRegisterProjectBadBodyCodes: the 400 codes follow the C4-v4
// contract — unknown keys are unexpected_field, malformed JSON and
// type errors are invalid_input.
func TestRegisterProjectBadBodyCodes(t *testing.T) {
	provider := &steerFlowProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	_, _, base, _ := newJSONLTestServer(t, provider)
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		// A plain relative path: the unknown-key error fires before any
		// path validation, and Windows tempdir backslashes would break
		// the JSON escape first.
		{"unknown field", `{"path":"x","bogus":1}`, "unexpected_field"},
		{"malformed json", `{`, "invalid_input"},
		{"type mismatch", `{"path":1}`, "invalid_input"},
		{"missing path", `{}`, "invalid_input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(base.URL+"/api/projects", "application/json",
				strings.NewReader(tc.body)) //nolint:noctx // test
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]string{}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if body["error"] != tc.code {
				t.Fatalf("code = %q, want %q", body["error"], tc.code)
			}
		})
	}
}

// TestUnregisterRefusesCreatedNotMessaged (C8b): a session created
// through the endpoint but with NO first message has nothing on disk
// — the disk-probing locate could not see it, the pool's namespace
// record can. The unregister must refuse; after the sweeper evicts
// the empty entry, it must succeed.
func TestUnregisterRefusesCreatedNotMessaged(t *testing.T) {
	// createTestServer (the C4 fixture) wires the ModelList the create
	// endpoint's shared validation needs, with a 50ms idle window so
	// the empty entry's eviction is observable.
	base, ns := createTestServer(t)

	// Create a session through the endpoint; the lease is released by
	// the handler (C4), leaving a table entry with no disk presence.
	resp, err := http.Post(base.URL+"/api/projects/"+ns+"/sessions", "application/json",
		strings.NewReader(`{"provider":"fake","model":"fake-model"}`)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.ID == "" {
		t.Fatalf("create: %d %+v", resp.StatusCode, created)
	}

	// The never-messaged session blocks the unregister: 409.
	resp2, body := unregisterDelete(t, base, ns)
	if resp2.StatusCode != http.StatusConflict || body["error"] != "project_in_use" {
		t.Fatalf("created-not-messaged: %d %v", resp2.StatusCode, body)
	}

	// After the idle sweeper tears the empty entry down (its lease is
	// gone — the fixture's 50ms window), the unregister succeeds. Poll
	// the endpoint itself: every refusal until then must be the SAME
	// project_in_use (an unrelated 409 would hide here otherwise).
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp3, body := unregisterDelete(t, base, ns)
		if resp3.StatusCode == http.StatusOK {
			return
		}
		if resp3.StatusCode != http.StatusConflict || body["error"] != "project_in_use" {
			t.Fatalf("unregister during eviction wait: %d %v", resp3.StatusCode, body)
		}
		if time.Now().After(deadline) {
			t.Fatal("the empty created session was never evicted")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
