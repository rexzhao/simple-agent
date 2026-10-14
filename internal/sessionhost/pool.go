// Package sessionhost owns the serve-mode session lifecycle: lazy
// opens with concurrent dedup, idle eviction, deletion, and shutdown.
// It holds no HTTP and no hub — the host (internal/web) builds its
// companion objects through the Options hooks (design §15.3).
package sessionhost

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

// Typed errors. The HTTP layer maps each to a stable status + code
// (docs/multi-session.md §4); sessionhost itself never sees HTTP.
var (
	// ErrNotFound: the id is not a session any registered project
	// holds (or not a valid id at all).
	ErrNotFound = errors.New("unknown session")
	// ErrConflict: the same id under multiple registered projects —
	// refuse instead of silently picking one.
	ErrConflict = errors.New("session conflict")
	// ErrLocked: the session's OS lock is held by another process.
	ErrLocked = errors.New("session locked by another process")
	// ErrClosing: the id is mid-teardown here (delete, eviction, or
	// pool shutdown) — retryable by contract.
	ErrClosing = errors.New("session closing")
	// ErrRunActive: Delete refused, the session has an active run.
	ErrRunActive = errors.New("run active")
	// ErrInUse: Delete refused, in-flight references or host-side
	// holders (subscribers, via Options.InUseGuard).
	ErrInUse = errors.New("session open")
	// ErrProjectUnavailable: the host's agent assembly for the owning
	// project failed (surfaced as a distinct code so the operator can
	// tell a config problem from a session problem).
	ErrProjectUnavailable = errors.New("project unavailable")
)

// Coded wraps a typed error with the wire message: the HTTP layer
// maps the kind to status+code and uses Error() as the message body,
// so the original human text survives the type upgrade (§15.3 N3).
func Coded(kind error, msg string) error {
	return &codedError{kind: kind, msg: msg}
}

type codedError struct {
	kind error
	msg  string
}

func (e *codedError) Error() string        { return e.msg }
func (e *codedError) Is(target error) bool { return target == e.kind }
func (e *codedError) Unwrap() error        { return e.kind }

// Options wires the pool to its host. Locate is the registration
// gate (the host's registry probe): it returns the owning namespace
// and the project path (for AgentFor), ErrNotFound for an unknown id,
// or a coded ErrConflict. AgentFor assembles the agent for one
// project directory. The hooks run on the teardown paths AFTER the
// id is unlinked from the pool's table — a callback's errors cannot
// affect the teardown itself.
type Options struct {
	Store storage.Store
	// RootLockPath is the serve root lock: acquired at construction,
	// released at Close — "one serve per root" follows the pool.
	RootLockPath string
	// Locate resolves a session id to (namespace, projectPath).
	Locate func(id string) (ns, projectPath string, err error)
	// AgentFor assembles the agent for one project directory.
	AgentFor func(projectPath string) (*agent.Agent, error)
	// IdleWindow: how long a session survives with no request, no
	// run, and no host-side holder. Zero means the package default
	// (SessionIdleEvict).
	IdleWindow time.Duration

	// OnOpened runs inside the open, BEFORE the session is published:
	// the host builds its companion object here (web creates the hub —
	// the mirror seeding must precede any client-visible state). The
	// returned value rides on Lease.HostData and is passed back to
	// OnEvicted/OnClosed verbatim. An error closes the session and
	// fails the Acquire — no half-open entry is left behind.
	OnOpened func(sessionID string, sess *agent.Session) (hostData any, err error)
	// OnEvicted: the sweeper tore the session down (idle + guards
	// allowed).
	OnEvicted func(sessionID string, sess *agent.Session, hostData any)
	// OnClosed: Delete or pool Close tore the session down (also the
	// discard path — an open whose result was thrown away because the
	// id entered teardown mid-open).
	OnClosed func(sessionID string, sess *agent.Session, hostData any)

	// EvictGuard vetoes an eviction (the host's "still watched"
	// signal — web reads its subscriber counts). A vetoed session
	// also refreshes its idle clock.
	EvictGuard func(sessionID string) bool
	// InUseGuard is the Delete-path twin of EvictGuard: "the host
	// still holds this session" even with no Lease refs (web: a
	// connected SSE subscriber).
	InUseGuard func(sessionID string) bool
}

// SessionIdleEvict is how long a session survives with no request, no
// subscriber, and no run before the sweeper closes it (releasing the
// OS lock for a CLI process). A var: tests shrink it.
var SessionIdleEvict = 30 * time.Second

// SweeperInterval is the eviction scan period; it also refreshes
// idleSince for active sessions, so measured idle time can exceed the
// true idle by at most one interval (the safe direction — eviction is
// only ever delayed by it). Exported like SessionIdleEvict: host-level
// tests (web) shrink both BEFORE constructing the pool.
var SweeperInterval = 10 * time.Second

// Pool is the session lifecycle host. Locking: mu guards
// agents/sessions/closing/opening — MEMORY state only; all disk IO
// (Locate, agent assembly, session open) happens outside it. The
// opening set dedups concurrent opens of one id (waiters block on a
// channel, not on mu).
type Pool struct {
	opts  Options
	store storage.Store
	idle  time.Duration

	rootLock *jsonl.PathLock

	mu       sync.Mutex
	agents   map[string]*agent.Agent // projectID → agent (never evicted; bounded by registrations)
	sessions map[string]*poolEntry
	closing  map[string]struct{}
	// opening holds one placeholder per id with an open in flight: the
	// claimer does the IO lock-free; concurrent opens of the same id
	// wait on its done channel and re-enter the fast path.
	opening map[string]chan struct{}
	// closed makes a post-Close open claim refuse instead of racing
	// the shutdown's teardown.
	closed    bool
	sweepStop chan struct{}
	sweepDone chan struct{}
	closeOnce sync.Once
}

// poolEntry is one open session. refs counts in-flight Leases;
// idleSince is the last moment the session was known active (a
// request, a subscriber transition, or a sweep tick during a run).
type poolEntry struct {
	session   *agent.Session
	hostData  any
	projectID string
	refs      int
	idleSince time.Time
}

// New acquires the root lock (a second pool on the same root fails
// fast) and starts the sweeper. The caller owns triaging the lock
// error (jsonl.LockHeldError distinguishes "another process holds
// it").
func New(opts Options) (*Pool, error) {
	lock, err := jsonl.AcquirePathLock(opts.RootLockPath)
	if err != nil {
		return nil, err
	}
	idle := opts.IdleWindow
	if idle <= 0 {
		idle = SessionIdleEvict
	}
	p := &Pool{
		opts:      opts,
		store:     opts.Store,
		idle:      idle,
		rootLock:  lock,
		agents:    make(map[string]*agent.Agent),
		sessions:  make(map[string]*poolEntry),
		closing:   make(map[string]struct{}),
		opening:   make(map[string]chan struct{}),
		sweepStop: make(chan struct{}),
		sweepDone: make(chan struct{}),
	}
	go p.sweepLoop()
	return p, nil
}

// Lease is one acquired reference to an open session. Release drops
// the reference; the session enters the idle window afterwards.
type Lease struct {
	pool     *Pool
	id       string
	entry    *poolEntry
	session  *agent.Session
	hostData any
}

// Session returns the open session.
func (l *Lease) Session() *agent.Session { return l.session }

// HostData returns the OnOpened result (the host's companion object).
func (l *Lease) HostData() any { return l.hostData }

// Release drops the lease's reference. CONTRACT: each lease is
// released EXACTLY once — a second call would over-decrement the
// entry's refs (there is no idempotence guard; callers structure
// their exit paths accordingly, cf. the web stream handler). It is
// the entry the lease was taken on that counts — a stale lease (the
// entry was torn down and the id reopened) must not decrement the NEW
// entry's refs.
func (l *Lease) Release() {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if cur := l.pool.sessions[l.id]; cur == l.entry && cur.refs > 0 {
		cur.refs--
	}
}

// Acquire resolves a session id to an open lease, or a typed error.
// Lazy open, concurrent dedup (one open per id), closing window
// semantics — the full contract is §15.3.
func (p *Pool) Acquire(id string) (*Lease, error) {
	if !jsonl.ValidSessionID(id) {
		return nil, ErrNotFound
	}
	p.mu.Lock()
	if entry := p.sessions[id]; entry != nil {
		entry.refs++
		entry.idleSince = time.Now()
		p.mu.Unlock()
		return p.lease(id, entry), nil
	}
	if _, busy := p.closing[id]; busy {
		// Our own teardown is mid-flight: the next request reopens
		// (lazily) once it finishes. Retryable by contract.
		p.mu.Unlock()
		return nil, Coded(ErrClosing, "")
	}
	if p.closed {
		p.mu.Unlock()
		return nil, Coded(ErrClosing, "")
	}
	if inFlight, busy := p.opening[id]; busy {
		// Someone is already opening this id: wait for their result
		// (on the channel, NOT under mu) and re-enter. The recursion
		// is bounded — the winner clears the placeholder before
		// closing its done channel. COST NOTE: if the winner FAILED,
		// every waiter re-enters and becomes an opener in turn — N
		// waiters, N open attempts. Acceptable: failure is rare, and
		// the alternative (result propagation) needs a bigger
		// structure.
		p.mu.Unlock()
		<-inFlight
		return p.Acquire(id)
	}
	opened := make(chan struct{})
	p.opening[id] = opened
	p.mu.Unlock()
	// The placeholder is cleaned on EVERY exit path — panics included:
	// openSlow reads disk data (segments a user can hand-edit), and a
	// panic must not leave this id permanently blocked on an unclosed
	// channel.
	defer func() {
		p.mu.Lock()
		delete(p.opening, id)
		close(opened)
		p.mu.Unlock()
	}()

	// ALL the IO runs lock-free (openSlow); the placeholder dedups
	// concurrent opens of this id.
	entry, err := p.openSlow(id)

	p.mu.Lock()
	publish := err == nil
	if publish {
		if _, busy := p.closing[id]; busy || p.closed {
			// A delete claimed the id while we were opening (it is
			// waiting for us — its storage delete must never race our
			// replay), or the pool is closing: discard what we opened,
			// tell the client to retry.
			publish = false
			err = Coded(ErrClosing, "")
		}
	}
	if publish {
		entry.refs = 1
		p.sessions[id] = entry
	}
	p.mu.Unlock()
	if !publish && entry != nil {
		// The host's companion object (if OnOpened ran) must be torn
		// down with the discarded session.
		if p.opts.OnClosed != nil {
			p.opts.OnClosed(id, entry.session, entry.hostData)
		}
		_ = entry.session.Close()
	}
	if !publish {
		return nil, err
	}
	return p.lease(id, entry), nil
}

func (p *Pool) lease(id string, entry *poolEntry) *Lease {
	return &Lease{pool: p, id: id, entry: entry, session: entry.session, hostData: entry.hostData}
}

// openSlow does every piece of open IO with NO pool lock held: the
// Locate probe (the host's registration gate), the agent assembly
// (deduped), the session open itself (which takes the OS session lock
// and replays the active segment), and OnOpened. One caller holds the
// opening placeholder for this id.
func (p *Pool) openSlow(id string) (*poolEntry, error) {
	ns, projectPath, err := p.opts.Locate(id)
	if err != nil {
		return nil, err
	}

	ag, err := p.agentForCached(ns, projectPath)
	if err != nil {
		return nil, Coded(ErrProjectUnavailable, err.Error())
	}

	session, err := ag.OpenSession(ns, id)
	if err != nil {
		var held *jsonl.LockHeldError
		if errors.As(err, &held) {
			// Held by another process (a CLI, most likely): not
			// retryable from this serve — the page must not loop.
			return nil, Coded(ErrLocked, held.Error())
		}
		if errors.Is(err, storage.ErrSessionNotFound) {
			return nil, ErrNotFound
		}
		// agent.ErrModelUnavailable passes through unchanged (the
		// host maps it); segment corruption, permissions: real
		// errors, surfaced.
		return nil, err
	}

	// OnOpened runs BEFORE publish, still inside the placeholder's
	// defer protection: the host's companion object must exist before
	// any concurrent Acquire can observe the entry. An error closes
	// the session and fails the open — no half-open entry.
	var hostData any
	if p.opts.OnOpened != nil {
		hostData, err = p.opts.OnOpened(id, session)
		if err != nil {
			_ = session.Close()
			return nil, err
		}
	}
	return &poolEntry{
		session:   session,
		hostData:  hostData,
		projectID: ns,
		idleSince: time.Now(),
	}, nil
}

// agentForCached returns the project's agent, assembling it at most
// effectively once: concurrent first-opens of one project may both
// assemble (the factory is pure), but only one object is kept — the
// loser is stateless garbage.
func (p *Pool) agentForCached(ownerID, ownerPath string) (*agent.Agent, error) {
	p.mu.Lock()
	ag := p.agents[ownerID]
	p.mu.Unlock()
	if ag != nil {
		return ag, nil
	}
	assembled, err := p.opts.AgentFor(ownerPath)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if existing := p.agents[ownerID]; existing != nil {
		p.mu.Unlock()
		return existing, nil
	}
	p.agents[ownerID] = assembled
	p.mu.Unlock()
	return assembled, nil
}

// MarkActive restarts the session's idle clock (the host calls it on
// its own activity signals — web does it on subscriber transitions).
func (p *Pool) MarkActive(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry := p.sessions[id]; entry != nil {
		entry.idleSince = time.Now()
	}
}

// OpenIDs snapshots the ids of the open sessions (the host's project
// listing marks them active).
func (p *Pool) OpenIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.sessions))
	for id := range p.sessions {
		ids = append(ids, id)
	}
	return ids
}

// StopRuns stops every active run across all open sessions.
func (p *Pool) StopRuns() {
	p.mu.Lock()
	entries := make([]*poolEntry, 0, len(p.sessions))
	for _, entry := range p.sessions {
		entries = append(entries, entry)
	}
	p.mu.Unlock()
	for _, entry := range entries {
		if entry.session.Running() {
			entry.session.Stop()
		}
	}
}

// HasRunning reports whether any open session has an active run.
func (p *Pool) HasRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.sessions {
		if entry.session.Running() {
			return true
		}
	}
	return false
}

// Delete removes a session permanently. Refusals: running
// (ErrRunActive), in-flight or host-held references (ErrInUse — the
// host's holders ride on Options.InUseGuard). Otherwise: claim the
// closing window, tear down the open entry, drain in-flight opens,
// then delete the storage. The closing set holds the id THROUGH the
// storage deletion: a concurrent lazy open in that window gets
// ErrClosing (retryable), and after the deletion it converges to
// ErrNotFound — a reopen can never race the removal.
func (p *Pool) Delete(id string) error {
	if !jsonl.ValidSessionID(id) {
		return ErrNotFound
	}
	// Pre-checks and the unlink happen under one critical section; the
	// teardown and the storage deletion run outside it.
	p.mu.Lock()
	var entry *poolEntry
	if e := p.sessions[id]; e != nil {
		if e.session.Running() {
			p.mu.Unlock()
			return Coded(ErrRunActive, "")
		}
		if e.refs > 0 || (p.opts.InUseGuard != nil && p.opts.InUseGuard(id)) {
			p.mu.Unlock()
			return Coded(ErrInUse, "")
		}
		delete(p.sessions, id)
		entry = e
	}
	if _, busy := p.closing[id]; busy {
		p.mu.Unlock()
		return Coded(ErrClosing, "")
	}
	p.closing[id] = struct{}{}
	p.mu.Unlock()

	// The claim is cleared on EVERY exit path — panics included: the
	// code below runs HOST CALLBACKS (OnClosed, Locate) in this
	// goroutine, and a callback panic recovered by net/http must not
	// leave the id wedged in ErrClosing until process restart (the P1
	// lesson, §15.3 — "回调里的任何错误不影响拆除本身"). The defer
	// replaces every explicit branch clear: delete is idempotent, the
	// ordering is identical (the claim holds through the storage
	// deletion).
	// INVARIANT: the teardown between claim and clear must stay fast —
	// OnClosed is the host's hub stop (a channel close) and
	// session.Close was made cheap by the Running pre-check — a
	// blocking Close here would hold every concurrent open of this id
	// at ErrClosing for its whole duration.
	defer func() {
		p.mu.Lock()
		delete(p.closing, id)
		p.mu.Unlock()
	}()

	if entry != nil {
		// The session closes even when the host callback panics
		// ("回调里的任何错误不影响拆除本身", §15.3): a recovered panic
		// must not leak the OS lock — the id reopens cleanly after.
		// The panic itself still propagates to the caller.
		func() {
			defer func() { _ = entry.session.Close() }()
			if p.opts.OnClosed != nil {
				p.opts.OnClosed(id, entry.session, entry.hostData)
			}
		}()
	}

	// A lazy open may be mid-flight on this id: the closing claim makes
	// it discard its result at publish — wait for that here so the
	// storage delete never races the open's replay.
	for {
		p.mu.Lock()
		inFlight := p.opening[id]
		p.mu.Unlock()
		if inFlight == nil {
			break
		}
		<-inFlight
	}

	// The owning project through the host's registration gate
	// (mirroring Acquire). Unregistered sessions are not found even
	// for deletion: the gate covers every surface.
	ns, _, err := p.opts.Locate(id)
	if err != nil {
		return err
	}

	deleter, canDelete := p.store.(storage.Deleter)
	if !canDelete {
		return fmt.Errorf("store cannot delete sessions")
	}
	if err := deleter.Delete(ns, id); err != nil {
		var held *jsonl.LockHeldError
		switch {
		case errors.As(err, &held):
			return Coded(ErrLocked, held.Error())
		case errors.Is(err, storage.ErrSessionNotFound):
			return ErrNotFound
		default:
			return err
		}
	}

	return nil
}

// Close stops the sweeper, tears down every open session (OnClosed,
// then session.Close), and releases the root lock. Idempotent: the
// serve shutdown has two entry points that can race, plus error
// paths. It ignores refs — in-flight requests are closed along with
// their sessions; that is the exit semantics, not the steady-state
// ones.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		close(p.sweepStop)
		<-p.sweepDone

		p.mu.Lock()
		p.closed = true
		// In-flight opens discard at publish (they see p.closed); wait
		// for the set to drain so nothing publishes into the teardown.
		for len(p.opening) > 0 {
			var inFlight []chan struct{}
			for _, ch := range p.opening {
				inFlight = append(inFlight, ch)
			}
			p.mu.Unlock()
			for _, ch := range inFlight {
				<-ch
			}
			p.mu.Lock()
		}
		type closed struct {
			id    string
			entry *poolEntry
		}
		entries := make([]closed, 0, len(p.sessions))
		for id, entry := range p.sessions {
			entries = append(entries, closed{id: id, entry: entry})
			delete(p.sessions, id)
			p.closing[id] = struct{}{}
		}
		p.mu.Unlock()
		for _, c := range entries {
			if p.opts.OnClosed != nil {
				p.opts.OnClosed(c.id, c.entry.session, c.entry.hostData)
			}
			_ = c.entry.session.Close()
		}
		p.rootLock.Release()
	})
}

// sweepLoop evicts idle sessions: no in-flight lease, no run, no
// host-side holder (EvictGuard), and nothing of the kind for the idle
// window. Unlinking from the map happens INSIDE the critical section
// and BEFORE teardown — after the unlink, a concurrent open goes to
// the lazy path (ErrClosing while the closing set holds the id, then
// a fresh open once teardown is done).
func (p *Pool) sweepLoop() {
	defer close(p.sweepDone)
	ticker := time.NewTicker(SweeperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.sweepStop:
			return
		case <-ticker.C:
			p.sweep()
		}
	}
}

type evictJob struct {
	id    string
	entry *poolEntry
}

func (p *Pool) sweep() {
	now := time.Now()
	p.mu.Lock()
	var evict []evictJob
	for id, entry := range p.sessions {
		if entry.refs > 0 || entry.session.Running() ||
			(p.opts.EvictGuard != nil && p.opts.EvictGuard(id)) {
			// Known active: restart the clock. For a running session
			// this refresh is the run-end transition (up to one
			// interval late — the safe direction, eviction delayed).
			// A COMPACTING session is covered here too: Compact
			// occupies the run slot for the whole summary.
			entry.idleSince = now
			continue
		}
		if now.Sub(entry.idleSince) >= p.idle {
			delete(p.sessions, id)
			p.closing[id] = struct{}{}
			evict = append(evict, evictJob{id: id, entry: entry})
		}
	}
	p.mu.Unlock()
	for _, job := range evict {
		if p.opts.OnEvicted != nil {
			p.opts.OnEvicted(job.id, job.entry.session, job.entry.hostData)
		}
		_ = job.entry.session.Close()
		p.mu.Lock()
		delete(p.closing, job.id)
		p.mu.Unlock()
	}
}
