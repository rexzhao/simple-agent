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

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/storage"
	"github.com/rexzhao/simple-agent/internal/storage/jsonl"
)

// Manager is the serve-mode session host: registered projects, lazily
// opened sessions (one hub each), and the eviction sweeper. It replaces
// the single-session binding — the routes resolve the session per request
// (design: docs/multi-session.md §3).
//
// Locking: m.mu guards agents/sessions/closing/opening — MEMORY state
// only; all disk IO (registry read, agent assembly, session open)
// happens outside it. The registry cache has its own mutex (a slow disk
// read must not hold the sessions map); the opening set dedups
// concurrent opens of one id (waiters block on a channel, not on m.mu).
// The only other lock ever held under m.mu is a hub's (Subscribers) —
// hub observers run OUTSIDE the hub lock and take m.mu, so the ordering
// is strictly m.mu → hub.mu.
type Manager struct {
	root     string
	store    storage.Store
	agentFor func(projectCwd string) (*agent.Agent, error)
	rootLock *jsonl.PathLock

	mu       sync.Mutex
	agents   map[string]*agent.Agent // projectID → agent (never evicted; bounded by registrations)
	sessions map[string]*sessionEntry
	closing  map[string]struct{}
	// opening holds one placeholder per id with an open in flight: the
	// claimer does the IO lock-free; concurrent opens of the same id
	// wait on its done channel and re-enter the fast path.
	opening map[string]chan struct{}
	// closed makes a post-Close open claim refuse instead of racing
	// the shutdown's teardown.
	closed bool

	projectsMu     sync.Mutex
	projectsCache  []byte    // marshaled /api/projects body
	projectsCached time.Time // when it was built

	// regMu guards the registry cache: stat-gated (mtime+size), so an
	// unchanged projects.json costs one os.Stat, not a parse. Its own
	// mutex — a slow read must not hold m.mu. regEntries IS the last
	// good table (a failed runtime reload never clears it); the parsed
	// Registry itself is not retained — nothing reads it.
	regMu      sync.Mutex
	regEntries []projects.Entry // cached Deduped(); SHARED — callers must not mutate
	regErr     error
	regModTime time.Time
	regSize    int64
	regLoaded  bool

	sweeperStop chan struct{}
	sweeperDone chan struct{}
	closeOnce   sync.Once
}

// sessionEntry is one open session. refs counts in-flight requests;
// idleSince is the last moment the session was known active (a request,
// a subscriber transition, or a sweep tick during a run) — the sweeper
// evicts when nothing has held it for SessionIdleEvict.
type sessionEntry struct {
	session   *agent.Session
	hub       *Hub
	projectID string
	refs      int
	idleSince time.Time
}

var (
	// SessionIdleEvict is how long a session survives with no request,
	// no subscriber, and no run before the sweeper closes it (releasing
	// the OS lock for a CLI process). A var: tests shrink it.
	SessionIdleEvict = 30 * time.Second
	// sweeperInterval is the eviction scan period; it also refreshes
	// idleSince for running sessions, so measured idle time can exceed
	// the true idle by at most one interval (the safe direction —
	// eviction is only ever delayed by it).
	sweeperInterval = 10 * time.Second // a var: tests shrink it
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
}

// NewManager takes the serve-mode root lock (a second serve on the same
// root fails fast) and starts the sweeper. The registry must parse at
// startup — a corrupt projects.json exits here, not per request.
func NewManager(options ManagerOptions) (*Manager, error) {
	lock, err := jsonl.AcquirePathLock(filepath.Join(options.ServerRoot, "serve.lock"))
	if err != nil {
		var held *jsonl.LockHeldError
		if errors.As(err, &held) {
			return nil, fmt.Errorf("another serve already holds %s", options.ServerRoot)
		}
		return nil, err
	}
	registryPath := projects.FilePath(options.ServerRoot)
	statBefore, statBeforeErr := os.Stat(registryPath)
	registry, err := projects.Load(registryPath)
	if err != nil {
		lock.Release()
		return nil, err
	}
	// The cache's stat must bracket the Load: a write landing between
	// stat and Load (a startup-moment `sai project add`) would leave
	// "new mtime + old content" — invisible until the NEXT write. When
	// the two stats disagree, skip caching the stat; the first
	// registryEntries call reloads.
	statAfter, statAfterErr := os.Stat(registryPath)
	m := &Manager{
		root:        options.ServerRoot,
		store:       options.Store,
		agentFor:    options.AgentFor,
		rootLock:    lock,
		regEntries:  registry.Deduped(),
		regLoaded:   true,
		agents:      make(map[string]*agent.Agent),
		sessions:    make(map[string]*sessionEntry),
		closing:     make(map[string]struct{}),
		opening:     make(map[string]chan struct{}),
		sweeperStop: make(chan struct{}),
		sweeperDone: make(chan struct{}),
	}
	if statBeforeErr == nil && statAfterErr == nil &&
		statBefore.ModTime().Equal(statAfter.ModTime()) && statBefore.Size() == statAfter.Size() {
		m.regModTime, m.regSize = statAfter.ModTime(), statAfter.Size()
	} // else: the cached stat stays unset — registryEntries reloads.
	go m.sweepLoop()
	return m, nil
}

// Close stops the sweeper, every hub, and every session, and releases
// the root lock. Idempotent: the serve shutdown has two entry points
// (the interrupt goroutine and the console loop's end/EOF) that can
// race, plus error paths. It ignores refs — in-flight requests are
// closed along with their sessions; that is the exit semantics, not
// the steady-state ones.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		close(m.sweeperStop)
		<-m.sweeperDone

		m.mu.Lock()
		m.closed = true
		// In-flight opens discard at publish (they see m.closed); wait
		// for the set to drain so nothing publishes into the teardown.
		for len(m.opening) > 0 {
			var inFlight []chan struct{}
			for _, ch := range m.opening {
				inFlight = append(inFlight, ch)
			}
			m.mu.Unlock()
			for _, ch := range inFlight {
				<-ch
			}
			m.mu.Lock()
		}
		entries := make([]*sessionEntry, 0, len(m.sessions))
		for id, entry := range m.sessions {
			entries = append(entries, entry)
			delete(m.sessions, id)
			m.closing[id] = struct{}{}
		}
		m.mu.Unlock()
		for _, entry := range entries {
			entry.hub.Stop()
			_ = entry.session.Close()
		}
		m.rootLock.Release()
	})
}

// StopRuns stops every active run across all open sessions (the serve
// interrupt path).
func (m *Manager) StopRuns() {
	m.mu.Lock()
	entries := make([]*sessionEntry, 0, len(m.sessions))
	for _, entry := range m.sessions {
		entries = append(entries, entry)
	}
	m.mu.Unlock()
	for _, entry := range entries {
		if entry.session.Running() {
			entry.session.Stop()
		}
	}
}

// HasRunning reports whether any open session has an active run.
func (m *Manager) HasRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.sessions {
		if entry.session.Running() {
			return true
		}
	}
	return false
}

// Preopen opens one session and holds it for the process lifetime — the
// serve-mode -session flag claims the lock up front (and the page
// initially selects it). It goes through the same registration gate as
// every route: an unregistered session errors here.
func (m *Manager) Preopen(sessionID string) error {
	entry, _, status, err := m.open(sessionID)
	if err != nil {
		return fmt.Errorf("preopen (status %d): %w", status, err)
	}
	// Held for the process lifetime: refs stays 1, so the sweeper never
	// evicts it; the lock is released at Close.
	_ = entry
	return nil
}

// Session opens a session lazily and returns the manager's object for
// it — through the same registration gate and error triage as the
// routes. The caller drives THAT object (sends on it notify its hub,
// snapshots reflect it). CAVEAT: the refs go back to zero here, so the
// entry stays subject to idle eviction — a caller holding the object
// across eviction drives a closed session (errors surface on use).
// Current callers are tests; a long-lived host (a future /tail tap)
// should take the release func instead — split the signature when that
// consumer exists. Preopen is the hold-forever form.
func (m *Manager) Session(id string) (*agent.Session, error) {
	entry, release, status, err := m.open(id)
	if err != nil {
		return nil, fmt.Errorf("open %s (status %d): %w", id, status, err)
	}
	release()
	return entry.session, nil
}

// resolve is the per-request session lookup: lazy open, refs counting,
// and the error triage. It writes the error response itself; the
// boolean reports whether the caller may proceed. The returned release
// decrements refs.
func (m *Manager) resolve(w http.ResponseWriter, r *http.Request) (*sessionEntry, func(), bool) {
	id := r.PathValue("id")
	entry, release, status, err := m.open(id)
	if err != nil {
		writeAPIError(w, status, err)
		return nil, nil, false
	}
	return entry, release, true
}

// open resolves a session id to an open entry, or a wire error. The
// registration gate is structural: only registered projects are probed,
// so an unregistered project's sessions are 404 even though the store
// can see their directories.
func (m *Manager) open(id string) (*sessionEntry, func(), int, error) {
	if !jsonl.ValidSessionID(id) {
		return nil, nil, http.StatusNotFound, fmt.Errorf("unknown session")
	}
	m.mu.Lock()
	if entry := m.sessions[id]; entry != nil {
		entry.refs++
		entry.idleSince = time.Now()
		m.mu.Unlock()
		return entry, m.releaseFunc(id), 0, nil
	}
	if _, busy := m.closing[id]; busy {
		// Our own teardown is mid-flight: the next request reopens
		// (lazily) once it finishes. Retryable by contract.
		m.mu.Unlock()
		return nil, nil, http.StatusServiceUnavailable, &apiError{code: "session_closing"}
	}
	if m.closed {
		m.mu.Unlock()
		return nil, nil, http.StatusServiceUnavailable, &apiError{code: "session_closing"}
	}
	if inFlight, busy := m.opening[id]; busy {
		// Someone is already opening this id: wait for their result
		// (on the channel, NOT under m.mu) and re-enter. The recursion
		// is bounded — the winner clears the placeholder before closing
		// its done channel, so each level corresponds to one completed
		// open attempt. COST NOTE: if the winner FAILED, every waiter
		// re-enters and becomes an opener in turn — N waiters, N open
		// attempts. Acceptable: failure is rare, and the alternative
		// (result propagation) needs a bigger structure.
		m.mu.Unlock()
		<-inFlight
		return m.open(id)
	}
	opened := make(chan struct{})
	m.opening[id] = opened
	m.mu.Unlock()

	// ALL the IO runs lock-free (openSlow); the placeholder dedups
	// concurrent opens of this id.
	entry, status, err := m.openSlow(id)

	m.mu.Lock()
	publish := err == nil
	if publish {
		if _, busy := m.closing[id]; busy || m.closed {
			// A delete claimed the id while we were opening (it is
			// waiting for us — its storage delete must never race our
			// replay), or the manager is closing: discard what we
			// opened, tell the client to retry.
			publish = false
			status, err = http.StatusServiceUnavailable, &apiError{code: "session_closing"}
		}
	}
	if publish {
		entry.refs = 1
		m.sessions[id] = entry
	}
	delete(m.opening, id)
	close(opened)
	m.mu.Unlock()
	if !publish && entry != nil {
		entry.hub.Stop()
		_ = entry.session.Close()
	}
	if !publish {
		return nil, nil, status, err
	}
	return entry, m.releaseFunc(id), 0, nil
}

// openSlow does every piece of open IO with NO manager lock held: the
// registry read (stat-gated cache), the per-project session probe, the
// agent assembly (deduped), and the session open itself (which takes
// the OS session lock and replays the active segment). One caller
// holds the opening placeholder for this id.
func (m *Manager) openSlow(id string) (*sessionEntry, int, error) {
	// Find the owning registered project — a lock-free probe, so the
	// agent assembly (which may fail) happens BEFORE the session lock:
	// a failed assembly must not leave a zombie locked entry.
	// The same id under two REGISTERED projects means hand-copied
	// directories: refuse instead of silently picking the first
	// (design §4).
	entries, _ := m.registryEntries()
	var ownerPath, ownerID string
	for _, entry := range entries {
		projectID := jsonl.ProjectID(entry.Path)
		if !m.store.HasSession(projectID, id) {
			continue
		}
		if ownerID != "" {
			return nil, http.StatusConflict, fmt.Errorf("multiple registered projects hold session %s", id)
		}
		ownerPath, ownerID = entry.Path, projectID
	}
	if ownerID == "" {
		return nil, http.StatusNotFound, fmt.Errorf("unknown session")
	}

	ag, err := m.agentForCached(ownerID, ownerPath)
	if err != nil {
		return nil, http.StatusInternalServerError, &apiError{code: "project_unavailable", cause: err}
	}

	session, err := ag.OpenSessionIn(ownerID, id)
	if err != nil {
		var held *jsonl.LockHeldError
		if errors.As(err, &held) {
			// Held by another process (a CLI, most likely): not
			// retryable from this serve — the page must not loop.
			return nil, http.StatusConflict, &apiError{code: "session_locked", cause: err}
		}
		if errors.Is(err, storage.ErrSessionNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("unknown session")
		}
		// Segment corruption, permissions: real errors, surfaced.
		return nil, http.StatusInternalServerError, err
	}

	entry := &sessionEntry{
		session:   session,
		hub:       NewHub(session),
		projectID: ownerID,
		idleSince: time.Now(),
	}
	entry.hub.SetClientObserver(func(int) {
		// Subscriber transitions restart the idle clock in BOTH
		// directions: attach and detach are activity by definition.
		m.mu.Lock()
		entry.idleSince = time.Now()
		m.mu.Unlock()
	})
	return entry, 0, nil
}

// agentForCached returns the project's agent, assembling it at most
// effectively once: concurrent first-opens of one project may both
// assemble (the factory is pure), but only one object is kept — the
// loser is stateless garbage.
func (m *Manager) agentForCached(ownerID, ownerPath string) (*agent.Agent, error) {
	m.mu.Lock()
	ag := m.agents[ownerID]
	m.mu.Unlock()
	if ag != nil {
		return ag, nil
	}
	assembled, err := m.agentFor(ownerPath)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if existing := m.agents[ownerID]; existing != nil {
		m.mu.Unlock()
		return existing, nil
	}
	m.agents[ownerID] = assembled
	m.mu.Unlock()
	return assembled, nil
}

// releaseFunc returns the refs decrement for one resolved request.
func (m *Manager) releaseFunc(id string) func() {
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if entry := m.sessions[id]; entry != nil && entry.refs > 0 {
			entry.refs--
		}
	}
}

// registryEntries returns the deduped registry entries through a
// stat-gated cache (mtime+size): an unchanged projects.json costs one
// os.Stat, not a parse — the read happens with NO manager lock held,
// so a slow disk never blocks session traffic. A failed reload keeps
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

// sweepLoop evicts idle sessions: no in-flight request, no subscriber,
// no run, and nothing of the kind for SessionIdleEvict. Unlinking from
// the map happens INSIDE the critical section and BEFORE teardown —
// after the unlink, a concurrent open goes to the lazy path (503 while
// the closing set holds the id, then a fresh open once teardown is done).
func (m *Manager) sweepLoop() {
	defer close(m.sweeperDone)
	ticker := time.NewTicker(sweeperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.sweeperStop:
			return
		case <-ticker.C:
			m.sweep()
		}
	}
}

type evictJob struct {
	id    string
	entry *sessionEntry
}

func (m *Manager) sweep() {
	now := time.Now()
	m.mu.Lock()
	var evict []evictJob
	for id, entry := range m.sessions {
		if entry.refs > 0 || entry.hub.Subscribers() > 0 || entry.session.Running() {
			// Known active: restart the clock. For a running session
			// this refresh is the run-end transition (up to one
			// interval late — the safe direction, eviction delayed).
			// A COMPACTING session is covered here too: Compact
			// occupies the run slot for the whole summary — if that
			// ever changes, the sweeper must learn the Compacting
			// transient explicitly (Close mid-summary would lose the
			// failure notice with the hub).
			entry.idleSince = now
			continue
		}
		if now.Sub(entry.idleSince) >= SessionIdleEvict {
			delete(m.sessions, id)
			m.closing[id] = struct{}{}
			evict = append(evict, evictJob{id: id, entry: entry})
		}
	}
	m.mu.Unlock()
	for _, job := range evict {
		job.entry.hub.Stop()
		_ = job.entry.session.Close()
		m.mu.Lock()
		delete(m.closing, job.id)
		m.mu.Unlock()
	}
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
	Projects   []projectsProjectWire `json:"projects"`
	RegistryOK bool                  `json:"registry_ok"`
}

// Projects serves GET /api/projects: the registered projects with their
// stored sessions (from the storage listing) and the `active` merge
// (open in THIS serve process — not "running"; the page shows run state
// from the session stream, not from this flag). One listing per project
// has a fixed disk cost, so the response is cached for projectsCacheTTL
// and the per-project listings run concurrently (each writes a distinct
// output slot). A failed runtime registry reload keeps the last good
// table and surfaces registry_ok=false — never an empty project list.
func (m *Manager) Projects(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, false) {
		return
	}
	m.projectsMu.Lock()
	cached, age := m.projectsCache, time.Since(m.projectsCached)
	m.projectsMu.Unlock()
	if cached != nil && age < projectsCacheTTL {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(cached)
		return
	}

	entries, regErr := m.registryEntries()
	m.mu.Lock()
	openIDs := make(map[string]bool) // open in this process → `active`
	for id := range m.sessions {
		openIDs[id] = true
	}
	m.mu.Unlock()

	body := projectsBody{RegistryOK: regErr == nil}
	body.Projects = make([]projectsProjectWire, len(entries))
	// Identity goes in BEFORE the listings: a failed listing then
	// leaves a project with no sessions rather than an empty shell
	// with no id (which the page could neither render nor debug).
	for i, entry := range entries {
		body.Projects[i] = projectsProjectWire{ID: jsonl.ProjectID(entry.Path), Path: entry.Path}
	}
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		go func(i int, projectID string) {
			defer wg.Done()
			infos, err := m.store.ListSessions(projectID)
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
		}(i, jsonl.ProjectID(entry.Path))
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

// DeleteSession removes a session permanently (the web delete). The
// registration gate applies as everywhere. A session this serve holds
// open refuses while running (run_active) or watched / mid-request
// (session_open — an SSE subscriber holds refs for its whole stream);
// an otherwise-idle entry is torn down first. The closing set holds the
// id THROUGH the storage deletion: a concurrent lazy open in that
// window gets 503 (retryable), and after the deletion it converges to
// 404 — a reopen can never race the removal. Every exit after the
// claim clears it again.
func (m *Manager) DeleteSession(w http.ResponseWriter, r *http.Request) {
	if !guardAPI(w, r, true) {
		return
	}
	id := r.PathValue("id")
	if !jsonl.ValidSessionID(id) {
		writeAPIError(w, http.StatusNotFound, fmt.Errorf("unknown session"))
		return
	}

	// Pre-checks and the unlink happen under one critical section; the
	// teardown and the storage deletion run outside it.
	m.mu.Lock()
	var entry *sessionEntry
	if e := m.sessions[id]; e != nil {
		if e.session.Running() {
			m.mu.Unlock()
			writeAPIError(w, http.StatusConflict, &apiError{code: "run_active"})
			return
		}
		if e.refs > 0 {
			m.mu.Unlock()
			writeAPIError(w, http.StatusConflict, &apiError{code: "session_open"})
			return
		}
		delete(m.sessions, id)
		entry = e
	}
	if _, busy := m.closing[id]; busy {
		m.mu.Unlock()
		writeAPIError(w, http.StatusServiceUnavailable, &apiError{code: "session_closing"})
		return
	}
	m.closing[id] = struct{}{}
	m.mu.Unlock()

	// Every path from here clears the closing claim. INVARIANT: the
	// teardown between claim and clear must stay fast — hub.Stop is a
	// channel close and session.Close was made cheap by the Running
	// pre-check — a blocking Close here would hold every concurrent
	// open of this id at 503 for its whole duration.
	clearClosing := func() {
		m.mu.Lock()
		delete(m.closing, id)
		m.mu.Unlock()
	}

	if entry != nil {
		entry.hub.Stop()
		_ = entry.session.Close()
	}

	// A lazy open may be mid-flight on this id: the closing claim makes
	// it discard its result at publish — wait for that here so the
	// storage delete never races the open's replay.
	for {
		m.mu.Lock()
		inFlight := m.opening[id]
		m.mu.Unlock()
		if inFlight == nil {
			break
		}
		<-inFlight
	}

	// The owning project through the registration gate (a lock-free
	// probe, mirroring open). Unregistered sessions are 404 even for
	// deletion: the gate covers every surface.
	entries, _ := m.registryEntries()
	var ownerID string
	var duplicate bool
	for _, e := range entries {
		projectID := jsonl.ProjectID(e.Path)
		if m.store.HasSession(projectID, id) {
			if ownerID != "" {
				duplicate = true
				break
			}
			ownerID = projectID
		}
	}
	if duplicate {
		clearClosing()
		writeAPIError(w, http.StatusConflict, fmt.Errorf("multiple registered projects hold session %s", id))
		return
	}
	if ownerID == "" {
		clearClosing()
		writeAPIError(w, http.StatusNotFound, fmt.Errorf("unknown session"))
		return
	}

	if err := m.store.Delete(ownerID, id); err != nil {
		clearClosing()
		var held *jsonl.LockHeldError
		switch {
		case errors.As(err, &held):
			writeAPIError(w, http.StatusConflict, &apiError{code: "session_locked", cause: err})
		case errors.Is(err, storage.ErrSessionNotFound):
			writeAPIError(w, http.StatusNotFound, fmt.Errorf("unknown session"))
		default:
			writeAPIError(w, http.StatusInternalServerError, err)
		}
		return
	}

	clearClosing()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
