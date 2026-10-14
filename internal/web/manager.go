package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/sessionhost"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

// Manager is the web host: the HTTP adapter over the session pool,
// the project registry, and one hub per open session. The session
// lifecycle itself (lazy open, eviction, deletion, shutdown) lives in
// sessionhost; this file wires the web-specific pieces into
// it and keeps what is purely HTTP (routes, /api/projects, error
// bodies) — design: docs/multi-session.md §3, docs/design.md §15.3.
//
// The hub map mirrors the pool's session table: OnOpened creates the
// hub BEFORE the session is published (the mirror seeding must
// precede any client-visible state — the stage-A blank-view bug), and
// OnEvicted/OnClosed remove it after the pool unlinked the id. The
// guards (EvictGuard/InUseGuard) read the map: a session with SSE
// subscribers is neither evicted nor deletable even with no Lease
// refs — subscribers do not hold leases (§15.3).
type Manager struct {
	root  string
	store storage.Store
	pool  *sessionhost.Pool
	// agentFactory is the CLI assembly (kept for test-side session
	// creation — the pool consumes the same function).
	agentFactory func(projectCwd string) (*agent.Agent, error)

	// preopen is the serve-mode -session claim: a lease held for the
	// process lifetime so the sweeper never evicts the pinned session.
	preopen *sessionhost.Lease

	hubsMu sync.Mutex
	hubs   map[string]*Hub

	projectsMu     sync.Mutex
	projectsCache  []byte    // marshaled /api/projects body
	projectsCached time.Time // when it was built

	// regMu guards the registry cache: stat-gated (mtime+size), so an
	// unchanged projects.json costs one os.Stat, not a parse. Its own
	// mutex — a slow read must not hold the pool. regEntries IS the last
	// good table (a failed runtime reload never clears it); the parsed
	// Registry itself is not retained — nothing reads it.
	regMu      sync.Mutex
	regEntries []projects.Entry // cached Deduped(); SHARED — callers must not mutate
	regErr     error
	regModTime time.Time
	regSize    int64
	regLoaded  bool
}

var (
	// projectsCacheTTL amortizes the listing cost: one ListSessions per
	// registered project has a fixed disk cost (~70ms measured on the
	// reference machine), and the page polls this endpoint for `active`.
	projectsCacheTTL = 1 * time.Second
)

// ManagerOptions wires the manager to the CLI assembly.
type ManagerOptions struct {
	ServerRoot string
	Store      storage.Store
	// AgentFor assembles the agent for one project directory — the
	// factory from the CLI assembly (provider config shared, tool
	// executor and system prompt bound to the project cwd).
	AgentFor func(projectCwd string) (*agent.Agent, error)
	// IdleWindow / SweepInterval pass through to the session pool's
	// eviction timing; zero keeps the pool defaults. Tests shrink both
	// (the ticker captures the interval at construction).
	IdleWindow    time.Duration
	SweepInterval time.Duration
}

// NewManager builds the pool (which takes the serve-mode root lock —
// a second serve on the same root fails fast) and loads the registry
// once: a corrupt projects.json exits here, not per request.
func NewManager(options ManagerOptions) (*Manager, error) {
	m := &Manager{
		root:         options.ServerRoot,
		store:        options.Store,
		agentFactory: options.AgentFor,
		hubs:         make(map[string]*Hub),
	}
	pool, err := sessionhost.New(sessionhost.Options{
		Store:         options.Store,
		RootLockPath:  filepath.Join(options.ServerRoot, "serve.lock"),
		IdleWindow:    options.IdleWindow,
		SweepInterval: options.SweepInterval,
		Locate:        m.locate,
		AgentFor:      options.AgentFor,
		OnOpened:      m.onOpened,
		OnEvicted:     m.onTeardown,
		OnClosed:      m.onTeardown,
		EvictGuard:    m.subscribed,
		InUseGuard:    m.subscribed,
	})
	if err != nil {
		var held *jsonl.LockHeldError
		if errors.As(err, &held) {
			return nil, fmt.Errorf("another serve already holds %s", options.ServerRoot)
		}
		return nil, err
	}
	m.pool = pool

	registryPath := projects.FilePath(options.ServerRoot)
	// The cache's stat must bracket the Load: a write landing between
	// stat and Load (a startup-moment `sai project add`) would leave
	// "new mtime + old content" — invisible until the NEXT write. When
	// the two stats disagree, skip caching the stat; the first
	// registryEntries call reloads.
	statBefore, statBeforeErr := os.Stat(registryPath)
	registry, err := projects.Load(registryPath)
	if err != nil {
		pool.Close()
		return nil, err
	}
	statAfter, statAfterErr := os.Stat(registryPath)
	m.regEntries = registry.Deduped()
	m.regLoaded = true
	if statBeforeErr == nil && statAfterErr == nil &&
		statBefore.ModTime().Equal(statAfter.ModTime()) && statBefore.Size() == statAfter.Size() {
		m.regModTime, m.regSize = statAfter.ModTime(), statAfter.Size()
	} // else: the cached stat stays unset — registryEntries reloads.
	return m, nil
}

// onOpened is the pool's pre-publish hook: the hub (the session's
// mirror) must exist before any concurrent request can observe the
// session (§15.3 D2).
func (m *Manager) onOpened(sessionID string, sess *agent.Session) (any, error) {
	hub := NewHub(sess)
	hub.SetClientObserver(func(int) {
		// Subscriber transitions restart the idle clock in BOTH
		// directions: attach and detach are activity by definition.
		m.pool.MarkActive(sessionID)
	})
	m.hubsMu.Lock()
	m.hubs[sessionID] = hub
	m.hubsMu.Unlock()
	return hub, nil
}

// onTeardown is the pool's post-unlink hook for eviction, deletion,
// and shutdown: stop the hub, drop it from the map.
func (m *Manager) onTeardown(sessionID string, sess *agent.Session, hostData any) {
	if hub, ok := hostData.(*Hub); ok {
		hub.Stop()
	}
	m.hubsMu.Lock()
	delete(m.hubs, sessionID)
	m.hubsMu.Unlock()
}

// hubOf returns the session's hub (nil if it is not open). Test and
// guard access path — the map is the web side of the pool's table.
func (m *Manager) hubOf(sessionID string) *Hub {
	m.hubsMu.Lock()
	defer m.hubsMu.Unlock()
	return m.hubs[sessionID]
}

// subscribed is the EvictGuard/InUseGuard predicate: a session with
// SSE subscribers is active (no eviction) and held (no deletion).
func (m *Manager) subscribed(sessionID string) bool {
	hub := m.hubOf(sessionID)
	return hub != nil && hub.Subscribers() > 0
}

// Close stops the pool (sweeper, every session, the root lock) and
// with it every hub. Idempotent: the serve shutdown has two entry
// points (the interrupt goroutine and the console loop's end/EOF)
// that can race, plus error paths. It ignores leases — in-flight
// requests are closed along with their sessions; that is the exit
// semantics, not the steady-state ones.
func (m *Manager) Close() {
	m.pool.Close()
}

// StopRuns stops every active run across all open sessions (the serve
// interrupt path).
func (m *Manager) StopRuns() { m.pool.StopRuns() }

// HasRunning reports whether any open session has an active run.
func (m *Manager) HasRunning() bool { return m.pool.HasRunning() }

// Preopen opens one session and holds it for the process lifetime — the
// serve-mode -session flag claims the lock up front (and the page
// initially selects it). It goes through the same registration gate as
// every route: an unregistered session errors here.
func (m *Manager) Preopen(sessionID string) error {
	lease, err := m.pool.Acquire(sessionID)
	if err != nil {
		return fmt.Errorf("preopen: %w", err)
	}
	// Held for the process lifetime: refs stays 1, so the sweeper never
	// evicts it; the lock is released at Close.
	m.preopen = lease
	return nil
}

// Session opens a session lazily and returns it — through the same
// registration gate and error triage as the routes. The caller drives
// THAT object (sends on it notify its hub, snapshots reflect it).
// CAVEAT: the lease goes back to zero here, so the session stays
// subject to idle eviction — a caller holding the object across
// eviction drives a closed session (errors surface on use). Current
// callers are tests; a long-lived host (a future /tail tap) should
// take the lease instead.
func (m *Manager) Session(id string) (*agent.Session, error) {
	lease, err := m.pool.Acquire(id)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", id, err)
	}
	lease.Release()
	return lease.Session(), nil
}

// sessionHandle is what a resolved request works on: the session and
// its hub (the web companion of the pool's entry).
type sessionHandle struct {
	session *agent.Session
	hub     *Hub
}

// resolve is the per-request session lookup: lazy open, lease
// counting, and the error triage. It writes the error response
// itself; the boolean reports whether the caller may proceed. The
// returned release drops the lease.
func (m *Manager) resolve(w http.ResponseWriter, r *http.Request) (*sessionHandle, func(), bool) {
	id := r.PathValue("id")
	lease, err := m.pool.Acquire(id)
	if err != nil {
		writePoolError(w, err)
		return nil, nil, false
	}
	hub, _ := lease.HostData().(*Hub)
	return &sessionHandle{session: lease.Session(), hub: hub}, lease.Release, true
}

// locate is the pool's registration gate: only registered projects
// are probed, so an unregistered project's sessions are not found
// even though the store can see their directories. The same id under
// two REGISTERED projects means hand-copied directories: refuse
// instead of silently picking the first (design §4).
func (m *Manager) locate(id string) (string, string, error) {
	prober, canProbe := m.store.(storage.Prober)
	entries, _ := m.registryEntries()
	var ownerPath, ownerID string
	for _, entry := range entries {
		projectID := projects.ProjectID(entry.Path)
		if !canProbe || !prober.HasSession(projectID, id) {
			continue
		}
		if ownerID != "" {
			return "", "", sessionhost.Coded(sessionhost.ErrConflict,
				fmt.Sprintf("multiple registered projects hold session %s", id))
		}
		ownerPath, ownerID = entry.Path, projectID
	}
	if ownerID == "" {
		return "", "", sessionhost.ErrNotFound
	}
	return ownerID, ownerPath, nil
}

// registryEntries returns the deduped registry entries through a
// stat-gated cache (mtime+size): an unchanged projects.json costs one
// os.Stat, not a parse — the read happens with NO pool lock held, so
// a slow disk never blocks session traffic. A failed reload keeps
// the last good table (never an empty one) and records the error for
// /api/projects to expose as registry_ok=false. (A MISSING file at
// startup is different — NewManager fails on it — but a file deleted
// at runtime reads as an empty table here, matching §1's semantics.)
//
// The returned slice is SHARED cache state: callers must not mutate
// elements or order (a sort here would silently corrupt every later
// reader).
func (m *Manager) registryEntries() ([]projects.Entry, error) {
	path := projects.FilePath(m.root)
	info, statErr := os.Stat(path)
	m.regMu.Lock()
	defer m.regMu.Unlock()
	if statErr == nil && m.regLoaded && info.ModTime().Equal(m.regModTime) && info.Size() == m.regSize {
		return m.regEntries, m.regErr
	}
	registry, loadErr := projects.Load(path)
	if loadErr != nil {
		if m.regErr == nil {
			log.Printf("sai: serve: registry reload failed, keeping last good table: %v", loadErr)
		}
		m.regErr = loadErr
		return m.regEntries, m.regErr
	}
	m.regEntries = registry.Deduped()
	m.regErr = nil
	if statErr == nil {
		m.regModTime, m.regSize, m.regLoaded = info.ModTime(), info.Size(), true
	}
	return m.regEntries, nil
}

// projectsSessionWire and projectsProjectWire are the /api/projects DTOs.
type projectsSessionWire struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Created time.Time `json:"created"`
	Active  bool      `json:"active"`
}

type projectsProjectWire struct {
	ID       string                `json:"id"`
	Path     string                `json:"path"`
	Sessions []projectsSessionWire `json:"sessions"`
}

type projectsBody struct {
	RegistryOK bool                  `json:"registry_ok"`
	Projects   []projectsProjectWire `json:"projects"`
}

// Projects serves /api/projects: the registered projects, their
// sessions (id, title, created, active), and the registry health.
func (m *Manager) Projects(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, false) {
		return
	}
	if cache, ok := m.projectsCachedBody(); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(cache)
		return
	}

	entries, regErr := m.registryEntries()
	openIDs := make(map[string]bool) // open in this process → `active`
	for _, id := range m.pool.OpenIDs() {
		openIDs[id] = true
	}

	body := projectsBody{RegistryOK: regErr == nil}
	body.Projects = make([]projectsProjectWire, len(entries))
	// Identity goes in BEFORE the listings: a failed listing then
	// leaves a project with no sessions rather than an empty shell
	// with no id (which the page could neither render nor debug).
	for i, entry := range entries {
		body.Projects[i] = projectsProjectWire{ID: projects.ProjectID(entry.Path), Path: entry.Path}
	}
	lister, canList := m.store.(storage.Lister)
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		go func(i int, projectID string) {
			defer wg.Done()
			if !canList {
				return
			}
			infos, err := lister.ListSessions(projectID)
			if err != nil {
				log.Printf("sai: serve: list %s: %v", projectID, err)
				return
			}
			for _, info := range infos {
				body.Projects[i].Sessions = append(body.Projects[i].Sessions, projectsSessionWire{
					ID:      info.ID,
					Title:   info.Title,
					Created: info.Created,
					Active:  openIDs[info.ID],
				})
			}
		}(i, projects.ProjectID(entry.Path))
	}
	wg.Wait()

	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.projectsMu.Lock()
	m.projectsCache, m.projectsCached = encoded, time.Now()
	m.projectsMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (m *Manager) projectsCachedBody() ([]byte, bool) {
	m.projectsMu.Lock()
	defer m.projectsMu.Unlock()
	if m.projectsCache == nil || time.Since(m.projectsCached) > projectsCacheTTL {
		return nil, false
	}
	return m.projectsCache, true
}

// apiError carries the stable code for the JSON error body.
type apiError struct {
	code  string
	cause error
}

func (e *apiError) Error() string {
	if e.cause != nil {
		return e.code + ": " + e.cause.Error()
	}
	return e.code
}

func (e *apiError) Unwrap() error { return e.cause }

// writeAPIError renders the stable-code JSON errors (the contract table
// in docs/multi-session.md §4). Errors without a code keep the generic
// body shape with the underlying message.
func writeAPIError(w http.ResponseWriter, status int, err error) {
	var api *apiError
	if errors.As(err, &api) && api.code != "" {
		message := ""
		if api.cause != nil {
			message = api.cause.Error()
		}
		writeJSON(w, status, map[string]string{"error": api.code, "message": message})
		return
	}
	writeJSON(w, status, map[string]string{"error": "", "message": err.Error()})
}

// writePoolError maps a sessionhost typed error to its HTTP status and
// stable code (§15.3's table); the message body is the error's own
// text — for coded errors that is the original human message, for
// sentinels the sentinel text.
func writePoolError(w http.ResponseWriter, err error) {
	var status int
	var code string
	switch {
	case errors.Is(err, sessionhost.ErrNotFound):
		status, code = http.StatusNotFound, "unknown_session"
	case errors.Is(err, sessionhost.ErrConflict):
		status, code = http.StatusConflict, "session_conflict"
	case errors.Is(err, sessionhost.ErrLocked):
		status, code = http.StatusConflict, "session_locked"
	case errors.Is(err, sessionhost.ErrClosing):
		status, code = http.StatusServiceUnavailable, "session_closing"
	case errors.Is(err, sessionhost.ErrRunActive):
		status, code = http.StatusConflict, "run_active"
	case errors.Is(err, sessionhost.ErrInUse):
		status, code = http.StatusConflict, "session_open"
	case errors.Is(err, sessionhost.ErrProjectUnavailable):
		status, code = http.StatusInternalServerError, "project_unavailable"
	case errors.Is(err, agent.ErrModelUnavailable):
		// The session's recorded model no longer resolves against the
		// configuration (§15.2): a stable conflict — the operator must
		// restore the model or migrate the session; a silent fallback
		// would change behavior without a trace.
		status, code = http.StatusConflict, "model_unavailable"
	default:
		// Segment corruption, permissions: real errors, surfaced.
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	writeAPIError(w, status, &apiError{code: code, cause: err})
}

// DeleteSession removes a session permanently (the web delete). The
// registration gate applies as everywhere; the refusals (running,
// held, locked) and the closing-window convergence are the pool's
// contract (§15.3) — this handler is the HTTP surface.
func (m *Manager) DeleteSession(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, true) {
		return
	}
	id := r.PathValue("id")
	if err := m.pool.Delete(id); err != nil {
		writePoolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
