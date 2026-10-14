package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/storage"
	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
	"github.com/rexzhao/simple-agent/internal/storage/memory"
)

// gateStore wraps a store with per-session gates and counters so the
// concurrency tests can observe (and stretch) the open path's IO.
type gateStore struct {
	storage.Store
	slowID    string // HasSession for this id sleeps
	slowDelay time.Duration
	entered   chan struct{} // closed when the slow path is first entered
	failOpens atomic.Bool   // injected OpenIn failures
	openCalls atomic.Int64
}

func (s *gateStore) HasSession(projectID, sessionID string) bool {
	if sessionID == s.slowID && s.slowDelay > 0 {
		if s.entered != nil {
			select {
			case <-s.entered:
			default:
				close(s.entered)
			}
		}
		time.Sleep(s.slowDelay)
	}
	p, ok := s.Store.(storage.Prober)
	if !ok {
		return false
	}
	return p.HasSession(projectID, sessionID)
}

func (s *gateStore) Open(projectID, sessionID string) (storage.Session, error) {
	s.openCalls.Add(1)
	if s.failOpens.Load() {
		return nil, fmt.Errorf("injected open failure")
	}
	return s.Store.Open(projectID, sessionID)
}

func (s *gateStore) ListSessions(ns string) ([]storage.SessionInfo, error) {
	l, ok := s.Store.(storage.Lister)
	if !ok {
		return nil, fmt.Errorf("wrapped store cannot list")
	}
	return l.ListSessions(ns)
}

func (s *gateStore) Delete(ns, id string) error {
	d, ok := s.Store.(storage.Deleter)
	if !ok {
		return fmt.Errorf("wrapped store cannot delete")
	}
	return d.Delete(ns, id)
}

// newOpenTestServer builds a manager over one registered project with
// two sessions on disk; the returned ids are [slowCandidate, warm].
func newOpenTestServer(t *testing.T, store storage.Store) (*Manager, *httptest.Server, [2]string) {
	t.Helper()
	root := t.TempDir()
	projectDir := t.TempDir()
	registry := &projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
	}}
	if err := registry.Save(projects.FilePath(root)); err != nil {
		t.Fatal(err)
	}
	factory := func(projectCwd string) (*agent.Agent, error) {
		return agent.New(agent.Options{
			ModelResolver: agent.NewStaticResolver(&webFakeProvider{}, nil, 0, 0, ""),
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
	var ids [2]string
	for i := range ids {
		ag, err := factory(projectDir)
		if err != nil {
			t.Fatal(err)
		}
		session, err := ag.NewSession(projects.ProjectID(projectDir), agent.SessionMetaForTest)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.MaterializeHeader(); err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		ids[i] = session.ID()
	}
	server := New(manager)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		server.Close()
	})
	return manager, httpServer, ids
}

func getState(t *testing.T, base, id string) int {
	t.Helper()
	res, err := http.Get(base + "/api/s/" + id + "/state") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// TestSlowOpenDoesNotBlockOtherSessions pins the lock-scope contract:
// the IO of opening one session (registry probe, assembly, replay)
// must not hold the lock other sessions' requests need. The slow
// session's open is stretched inside HasSession; the warm session's
// /state must complete well inside that window.
func TestSlowOpenDoesNotBlockOtherSessions(t *testing.T) {
	store := &gateStore{
		Store:     memory.OpenStore(),
		slowDelay: 800 * time.Millisecond,
		entered:   make(chan struct{}),
	}
	manager, base, ids := newOpenTestServer(t, store)
	store.slowID = ids[0]
	_ = manager

	// Warm the OTHER session (its open passes the gate fast — not the
	// slow id).
	if code := getState(t, base.URL, ids[1]); code != http.StatusOK {
		t.Fatalf("warm open: %d", code)
	}

	slowDone := make(chan int, 1)
	go func() { slowDone <- getState(t, base.URL, ids[0]) }()
	<-store.entered // the slow open is provably in flight

	// The fast path on the warm session, measured while the slow open
	// holds its IO window open.
	start := time.Now()
	code := getState(t, base.URL, ids[1])
	elapsed := time.Since(start)
	if code != http.StatusOK {
		t.Fatalf("warm /state during slow open: %d", code)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("warm /state took %v while another session's open was in flight — lock held across IO", elapsed)
	}
	if code := <-slowDone; code != http.StatusOK {
		t.Fatalf("slow open finished: %d", code)
	}
}

// TestConcurrentOpenOpensOnce: N requests for the same unopened id,
// stretched so they overlap — the placeholder lets exactly one open
// through; everyone succeeds.
func TestConcurrentOpenOpensOnce(t *testing.T) {
	store := &gateStore{Store: memory.OpenStore(), slowDelay: 300 * time.Millisecond}
	_, base, ids := newOpenTestServer(t, store)
	store.slowID = ids[0]

	const n = 8
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() { codes <- getState(t, base.URL, ids[0]) }()
	}
	for i := 0; i < n; i++ {
		if code := <-codes; code != http.StatusOK {
			t.Fatalf("concurrent open: %d", code)
		}
	}
	if got := store.openCalls.Load(); got != 1 {
		t.Fatalf("OpenIn called %d times for one id, want 1", got)
	}
}

// TestFailedOpenClearsPlaceholder: an open that fails must clear its
// placeholder — the next request retries for real (and succeeds once
// the injected failure is gone), never hangs on a zombie claim.
func TestFailedOpenClearsPlaceholder(t *testing.T) {
	store := &gateStore{Store: memory.OpenStore()}
	store.failOpens.Store(true)
	_, base, ids := newOpenTestServer(t, store)

	if code := getState(t, base.URL, ids[0]); code != http.StatusInternalServerError {
		t.Fatalf("first open: %d, want 500", code)
	}
	// Retryable: the placeholder is gone, a new attempt runs (and
	// fails the same way — proving no hang and no zombie claim).
	if code := getState(t, base.URL, ids[0]); code != http.StatusInternalServerError {
		t.Fatalf("second open: %d, want 500 (placeholder cleared)", code)
	}
	if got := store.openCalls.Load(); got != 2 {
		t.Fatalf("OpenIn called %d times, want 2", got)
	}
	// The failure is gone: the next request succeeds.
	store.failOpens.Store(false)
	if code := getState(t, base.URL, ids[0]); code != http.StatusOK {
		t.Fatalf("open after failure cleared: %d", code)
	}
}

// TestDeleteDuringInFlightOpen: a delete that claims the id while an
// open is mid-flight wins — the open discards (503), the delete
// proceeds, and the id converges to 404.
func TestDeleteDuringInFlightOpen(t *testing.T) {
	store := &gateStore{
		Store:     memory.OpenStore(),
		slowDelay: 600 * time.Millisecond,
		entered:   make(chan struct{}),
	}
	_, base, ids := newOpenTestServer(t, store)
	store.slowID = ids[0]

	openDone := make(chan int, 1)
	go func() { openDone <- getState(t, base.URL, ids[0]) }()
	<-store.entered

	res, err := http.NewRequest(http.MethodDelete, base.URL+"/api/s/"+ids[0], nil) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	del := make(chan int, 1)
	go func() {
		r, err := http.DefaultClient.Do(res)
		if err != nil {
			del <- -1
			return
		}
		r.Body.Close()
		del <- r.StatusCode
	}()

	if code := <-openDone; code != http.StatusServiceUnavailable {
		t.Fatalf("open during delete: %d, want 503 (discarded)", code)
	}
	if code := <-del; code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if code := getState(t, base.URL, ids[0]); code != http.StatusNotFound {
		t.Fatalf("after delete: %d, want 404", code)
	}
}

// TestRegistryCacheRefreshesOnWrite: a runtime edit of projects.json
// is visible to the next registry read (the stat gate must see it) —
// both the entries and the /api/projects surface.
func TestRegistryCacheRefreshesOnWrite(t *testing.T) {
	store := &gateStore{Store: memory.OpenStore()}
	manager, base, _ := newOpenTestServer(t, store)
	_ = base

	entries, err := manager.registryEntries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("initial entries: %v %d", err, len(entries))
	}
	// Same content, unchanged stat: served from the cache (no error,
	// same slice semantics is not asserted — identity is).
	again, _ := manager.registryEntries()
	if len(again) != 1 {
		t.Fatalf("cached entries: %d", len(again))
	}

	// A runtime edit: add a second project.
	path := projects.FilePath(manager.root)
	registry, err := projects.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	registry.Projects = append(registry.Projects, projects.Entry{Path: t.TempDir(), Added: time.Now().UTC()})
	if err := registry.Save(path); err != nil {
		t.Fatal(err)
	}
	// mtime granularity: force observability by waiting out the coarse
	// bound if the write landed in the same tick.
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err = manager.registryEntries()
		if err == nil && len(entries) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry edit not visible: %v %d", err, len(entries))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Corrupt the file: the last good table survives, the error is
	// recorded (registry_ok=false).
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, regErr := manager.registryEntries()
	if regErr == nil || len(entries) != 2 {
		t.Fatalf("corrupt registry: err=%v entries=%d, want last-good 2 + error", regErr, len(entries))
	}
}

// TestOpenDuringCloseDiscards: a Close that lands while an open is
// mid-flight makes the open discard its result (503) instead of
// publishing a doomed entry into a torn-down map — the publish check
// covers m.closed, matching the Close comment. The discard must also
// release the session's OS lock: the jsonl store below proves it by
// reopening the session through an independent path afterwards.
func TestOpenDuringCloseDiscards(t *testing.T) {
	jsonlStore, err := jsonl.OpenStore(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	store := &gateStore{
		Store:     jsonlStore,
		slowDelay: 600 * time.Millisecond,
		entered:   make(chan struct{}),
	}
	manager, base, ids := newOpenTestServer(t, store)
	store.slowID = ids[0]

	openDone := make(chan int, 1)
	go func() { openDone <- getState(t, base.URL, ids[0]) }()
	<-store.entered

	closed := make(chan struct{})
	go func() {
		manager.Close()
		close(closed)
	}()

	if code := <-openDone; code != http.StatusServiceUnavailable {
		t.Fatalf("open during close: %d, want 503 (discarded)", code)
	}
	<-closed

	// No zombie state: nothing published, no placeholder left.
	manager.mu.Lock()
	sessions, opening := len(manager.sessions), len(manager.opening)
	manager.mu.Unlock()
	if sessions != 0 || opening != 0 {
		t.Fatalf("post-close state: sessions=%d opening=%d, want 0/0", sessions, opening)
	}

	// The discarded session's OS lock was released: an independent
	// open through the store succeeds (it would conflict if the
	// discard had leaked the session open).
	entries, _ := manager.registryEntries()
	projectID := projects.ProjectID(entries[0].Path)
	entry, err := jsonlStore.Open(projectID, ids[0])
	_ = entry
	if err != nil {
		t.Fatalf("independent reopen after discard: %v (OS lock leaked?)", err)
	}
	if entry != nil {
		_ = entry.Close()
	}
}
