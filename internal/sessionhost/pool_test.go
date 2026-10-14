package sessionhost

// Pool-level lifecycle tests. The HTTP surface of the same semantics
// (status codes, the registration gate) lives in internal/web; these
// pin the pool's own contract: lazy open, eviction, deletion
// refusals, the closing window, and the D1/D2 hook behaviors.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

// testSessionMeta is the meta tests record in new sessions (the
// test provider/model names) - the literal replacement of the
// removed agent.SessionMetaForTest.
var testSessionMeta = storage.SessionMeta{Provider: "test", Model: "model-test"}

// gatedProvider holds the stream open so the test can keep a run
// active (Running() blocks eviction and deletion).
type gatedProvider struct {
	gate chan struct{}
}

func (p *gatedProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	events := make(chan model.Event, 4)
	go func() {
		defer close(events)
		events <- model.TextDeltaEvent{Text: "answering"}
		select {
		case <-p.gate:
		case <-ctx.Done():
			// Stop() must be able to settle the run: a harness that
			// ignores ctx would wedge Session.Close forever.
			return
		}
		events <- model.TextDeltaEvent{Text: "…"}
	}()
	return events, nil
}

func (p *gatedProvider) release() {
	select {
	case <-p.gate:
	default:
		close(p.gate)
	}
}

// testEnv is a pool over a real jsonl store with one registered
// project and one materialized session on disk.
type testEnv struct {
	pool       *Pool
	store      *jsonl.Store
	ns         string
	projectDir string
	id         string
}

// locateGate lets tests stretch the Locate call (the open's first IO)
// to hold an open in flight observably.
type locateGate struct {
	mu      sync.Mutex
	slow    bool
	entered chan struct{}
}

func (g *locateGate) maybeEnter() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.slow && g.entered != nil {
		select {
		case <-g.entered:
		default:
			close(g.entered)
		}
	}
}

func newTestEnv(t *testing.T, opts func(*Options), provider model.Provider) *testEnv {
	t.Helper()
	root := t.TempDir()
	projectDir := t.TempDir()
	if err := (&projects.Registry{Projects: []projects.Entry{
		{Path: projectDir, Added: time.Now().UTC()},
	}}).Save(projects.FilePath(root)); err != nil {
		t.Fatal(err)
	}
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	ns := projects.ProjectID(projectDir)
	factory := func(projectCwd string) (*agent.Agent, error) {
		return agent.New(agent.Options{
			ModelResolver: agent.NewStaticResolver(provider, nil, 0, 0, ""),
			Storage:       store,
			CWD:           projectCwd,
		})
	}
	// One materialized session under the project.
	ag, err := factory(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	session, err := ag.NewSession(ns, testSessionMeta)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.MaterializeHeader(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	env := &testEnv{store: store, ns: ns, projectDir: projectDir, id: session.ID()}
	options := Options{
		Store:        store,
		RootLockPath: filepath.Join(root, "serve.lock"),
		Locate: func(id string) (string, string, error) {
			if id == env.id {
				if store.HasSession(ns, id) {
					return ns, projectDir, nil
				}
			}
			return "", "", ErrNotFound
		},
		AgentFor: factory,
	}
	if opts != nil {
		opts(&options)
	}
	pool, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	env.pool = pool
	t.Cleanup(pool.Close)
	return env
}

// plainEnv is the no-hooks default.
func newPlainEnv(t *testing.T) *testEnv {
	return newTestEnv(t, nil, &gatedProvider{gate: make(chan struct{})})
}

func TestPoolAcquireOpensAndReleases(t *testing.T) {
	env := newPlainEnv(t)
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Session() == nil {
		t.Fatal("lease has no session")
	}
	if got := len(env.pool.OpenIDs()); got != 1 {
		t.Fatalf("open ids = %d, want 1", got)
	}
	lease.Release()
	// The entry stays (lazy close is the sweeper's job); a second
	// acquire takes the fast path.
	lease2, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	lease2.Release()
}

// TestSweeperEvictsIdleSession pins the eviction contract: an idle
// session is torn down (the OS lock released) and a later acquire
// transparently reopens. The closing window is the synchronization
// point — the lock is released exactly when it clears.
func TestSweeperEvictsIdleSession(t *testing.T) {
	// Shrink the timing BEFORE the pool starts its sweeper: the
	// ticker interval is captured at construction.
	oldEvict, oldInterval := SessionIdleEvict, SweeperInterval
	SessionIdleEvict, SweeperInterval = 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { SessionIdleEvict, SweeperInterval = oldEvict, oldInterval })
	env := newPlainEnv(t)

	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	waitFor(t, 2*time.Second, func() bool {
		env.pool.mu.Lock()
		defer env.pool.mu.Unlock()
		_, busy := env.pool.closing[env.id]
		_, open := env.pool.sessions[env.id]
		return !busy && !open
	})

	// The lock is really released: another session open (a second
	// process-handle holding the storage lock) succeeds now.
	holder, err := env.store.Open(env.ns, env.id)
	if err != nil {
		t.Fatalf("lock not released after eviction: %v", err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}

	// A later acquire transparently reopens.
	lease2, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatalf("reopen after eviction: %v", err)
	}
	lease2.Release()
}

// TestSweeperNeverEvictsActiveSession: a running session and a held
// lease both block eviction; after they clear, the idle window
// reclaims the session.
func TestSweeperNeverEvictsActiveSession(t *testing.T) {
	provider := &gatedProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	oldEvict, oldInterval := SessionIdleEvict, SweeperInterval
	SessionIdleEvict, SweeperInterval = 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { SessionIdleEvict, SweeperInterval = oldEvict, oldInterval })
	env := newTestEnv(t, nil, provider)

	// A gated run: Running stays true; several sweep ticks pass.
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Session().Send("go"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	env.pool.mu.Lock()
	_, open := env.pool.sessions[env.id]
	env.pool.mu.Unlock()
	if !open {
		t.Fatal("sweeper evicted a session with an active run")
	}

	// The run settles but the lease is still held: refs block eviction.
	provider.release()
	waitFor(t, 2*time.Second, func() bool { return !lease.Session().Running() })
	time.Sleep(300 * time.Millisecond)
	env.pool.mu.Lock()
	_, open = env.pool.sessions[env.id]
	env.pool.mu.Unlock()
	if !open {
		t.Fatal("sweeper evicted a session with a held lease")
	}
	lease.Release()

	// Now idle: eviction reclaims it (same closing-set condition).
	waitFor(t, 2*time.Second, func() bool {
		env.pool.mu.Lock()
		defer env.pool.mu.Unlock()
		_, busy := env.pool.closing[env.id]
		_, open := env.pool.sessions[env.id]
		return !busy && !open
	})
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestOpenDuringCloseDiscards: a Close that lands while an open is
// mid-flight makes the open discard its result instead of publishing
// a doomed entry into a torn-down map. The discard must also release
// the session's OS lock — the jsonl store proves it by reopening the
// session through an independent path afterwards.
func TestOpenDuringCloseDiscards(t *testing.T) {
	gate := &locateGate{entered: make(chan struct{})}
	env := newTestEnv(t, func(o *Options) {
		inner := o.Locate
		o.Locate = func(id string) (string, string, error) {
			gate.maybeEnter()
			gate.mu.Lock()
			slow := gate.slow
			gate.mu.Unlock()
			if slow {
				time.Sleep(600 * time.Millisecond)
			}
			return inner(id)
		}
	}, &gatedProvider{gate: make(chan struct{})})
	gate.mu.Lock()
	gate.slow = true
	gate.mu.Unlock()

	openDone := make(chan error, 1)
	go func() {
		lease, err := env.pool.Acquire(env.id)
		if lease != nil {
			lease.Release()
		}
		openDone <- err
	}()
	<-gate.entered

	env.pool.Close()

	if err := <-openDone; !errors.Is(err, ErrClosing) {
		t.Fatalf("open during close: %v, want ErrClosing (discarded)", err)
	}

	// No zombie state: nothing published, no placeholder left.
	env.pool.mu.Lock()
	sessions, opening := len(env.pool.sessions), len(env.pool.opening)
	env.pool.mu.Unlock()
	if sessions != 0 || opening != 0 {
		t.Fatalf("post-close state: sessions=%d opening=%d, want 0/0", sessions, opening)
	}

	// The discarded session's OS lock was released: an independent
	// open through the store succeeds (it would conflict if the
	// discard had leaked the session open).
	entry, err := env.store.Open(env.ns, env.id)
	if err != nil {
		t.Fatalf("discarded open leaked the session lock: %v", err)
	}
	if err := entry.Close(); err != nil {
		t.Fatal(err)
	}
}

// --- D1: the Delete refusals ------------------------------------------

// TestDeleteRunningRefused: a delete of a session with an active run
// is ErrRunActive — the delete must not race a mid-flight turn.
func TestDeleteRunningRefused(t *testing.T) {
	provider := &gatedProvider{gate: make(chan struct{})}
	t.Cleanup(provider.release)
	env := newTestEnv(t, nil, provider)

	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Session().Send("go"); err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	err = env.pool.Delete(env.id)
	if !errors.Is(err, ErrRunActive) {
		t.Fatalf("delete of running session: %v, want ErrRunActive", err)
	}
	// Refused, not executed: the session is still on disk.
	if !env.store.HasSession(env.ns, env.id) {
		t.Fatal("the refused delete removed the session")
	}
}

// TestDeleteHeldLeaseRefused: a delete with an in-flight lease is
// ErrInUse.
func TestDeleteHeldLeaseRefused(t *testing.T) {
	env := newPlainEnv(t)
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	err = env.pool.Delete(env.id)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("delete with held lease: %v, want ErrInUse", err)
	}
	if !env.store.HasSession(env.ns, env.id) {
		t.Fatal("the refused delete removed the session")
	}
}

// TestDeleteSubscribedNoRefsRefused (D1's third condition): a session
// the HOST still holds — InUseGuard true, no lease refs — is refused
// with ErrInUse. This is the subscriber case after the split: SSE
// subscribers do not hold leases, the guard carries the refusal.
func TestDeleteSubscribedNoRefsRefused(t *testing.T) {
	var guardCalls atomic.Int64
	env := newTestEnv(t, func(o *Options) {
		o.InUseGuard = func(sessionID string) bool {
			guardCalls.Add(1)
			return true // "a subscriber is connected"
		}
	}, &gatedProvider{gate: make(chan struct{})})

	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release() // refs back to zero — only the guard holds it

	err = env.pool.Delete(env.id)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("delete of subscribed session without refs: %v, want ErrInUse", err)
	}
	if guardCalls.Load() == 0 {
		t.Fatal("the refusal never consulted InUseGuard — the guard is not wired")
	}
	if !env.store.HasSession(env.ns, env.id) {
		t.Fatal("the refused delete removed the session")
	}
}

// TestDeleteOnClosedPanicDoesNotWedge: a panicking host callback must
// not leave the id wedged in the closing set. Delete runs host code
// (OnClosed, Locate) between claiming and clearing the closing
// window; net/http recovers handler panics, so a recovered panic must
// unwind the claim too — otherwise every later Acquire of this id is
// ErrClosing until process restart (the P1 lesson, §15.3).
func TestDeleteOnClosedPanicDoesNotWedge(t *testing.T) {
	env := newTestEnv(t, func(o *Options) {
		o.OnClosed = func(sessionID string, sess *agent.Session, hostData any) {
			panic("injected host teardown panic")
		}
	}, &gatedProvider{gate: make(chan struct{})})

	// Open it so the delete path actually reaches OnClosed.
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the injected OnClosed panic did not propagate (net/http, not the pool, recovers)")
			}
		}()
		_ = env.pool.Delete(env.id)
	}()

	// The panic unwound BEFORE the storage delete: the session still
	// exists — and the id is not wedged, a fresh Acquire succeeds (the
	// deferred session.Close released the OS lock despite the panic).
	lease2, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatalf("acquire after a recovered OnClosed panic: %v — the closing claim was not unwound", err)
	}
	lease2.Release()

	// Disarm the hook for the pool's own cleanup teardown: the Close
	// path does not panic-proof its callbacks (a panic there crashes
	// the process rather than wedging — the accepted trade-off this
	// step, see the Delete comment); the test only pins the Delete
	// contract.
	env.pool.opts.OnClosed = func(string, *agent.Session, any) {}
}

// TestDeleteLocatePanicDoesNotWedge: the same contract for the Locate
// gate — it is host code too (the web registry probe).
func TestDeleteLocatePanicDoesNotWedge(t *testing.T) {
	env := newPlainEnv(t)
	panicked := false
	env.pool.opts.Locate = func(id string) (string, string, error) {
		panicked = true
		panic("injected gate panic")
	}

	func() {
		defer func() { _ = recover() }()
		_ = env.pool.Delete(env.id)
	}()
	if !panicked {
		t.Fatal("the injected Locate panic did not fire")
	}

	// Restore a working gate: the id must not be wedged.
	env.pool.opts.Locate = func(id string) (string, string, error) {
		if id == env.id {
			return env.ns, env.projectDir, nil
		}
		return "", "", ErrNotFound
	}
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatalf("acquire after a recovered Locate panic: %v — the closing claim was not unwound", err)
	}
	lease.Release()
}

// --- D2: the OnOpened hook ---------------------------------------------

// TestConcurrentAcquireTriggersOnOpenedOnce: concurrent acquires of
// one id dedup to a single open — OnOpened runs exactly once, every
// lease carries the same host data.
func TestConcurrentAcquireTriggersOnOpenedOnce(t *testing.T) {
	gate := &locateGate{entered: make(chan struct{})}
	var opened atomic.Int64
	env := newTestEnv(t, func(o *Options) {
		inner := o.Locate
		o.Locate = func(id string) (string, string, error) {
			gate.maybeEnter()
			time.Sleep(300 * time.Millisecond)
			return inner(id)
		}
		o.OnOpened = func(sessionID string, sess *agent.Session) (any, error) {
			opened.Add(1)
			return "host-data", nil
		}
	}, &gatedProvider{gate: make(chan struct{})})

	const n = 8
	errs := make(chan error, n)
	data := make(chan any, n)
	for i := 0; i < n; i++ {
		go func() {
			lease, err := env.pool.Acquire(env.id)
			if err != nil {
				errs <- err
				return
			}
			data <- lease.HostData()
			lease.Release()
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatalf("concurrent acquire: %v", err)
		case d := <-data:
			if d != "host-data" {
				t.Fatalf("host data = %v, want %q", d, "host-data")
			}
		}
	}
	if got := opened.Load(); got != 1 {
		t.Fatalf("OnOpened ran %d times, want 1", got)
	}
}

// TestOnOpenedErrorLeavesNoPlaceholderOrLock: a failing OnOpened
// closes the session, clears the placeholder, and fails the Acquire —
// no half-open entry, no leaked OS lock.
func TestOnOpenedErrorLeavesNoPlaceholderOrLock(t *testing.T) {
	var failOpened atomic.Bool
	env := newTestEnv(t, func(o *Options) {
		o.OnOpened = func(sessionID string, sess *agent.Session) (any, error) {
			if failOpened.Load() {
				return nil, errors.New("injected host failure")
			}
			return "host-data", nil
		}
	}, &gatedProvider{gate: make(chan struct{})})

	failOpened.Store(true)
	sentinel := errors.New("injected host failure")
	_, err := env.pool.Acquire(env.id)
	if err == nil || err.Error() != sentinel.Error() {
		t.Fatalf("acquire with failing OnOpened: %v, want the hook's error", err)
	}

	// No placeholder left: the next acquire retries the open for real.
	env.pool.mu.Lock()
	left := len(env.pool.opening)
	env.pool.mu.Unlock()
	if left != 0 {
		t.Fatalf("placeholder left behind: %d", left)
	}

	// The failed open released the OS lock: an independent store open
	// succeeds (it would conflict with a leaked session).
	entry, err := env.store.Open(env.ns, env.id)
	if err != nil {
		t.Fatalf("failed OnOpened leaked the session lock: %v", err)
	}
	if err := entry.Close(); err != nil {
		t.Fatal(err)
	}

	// The failure is gone: the next acquire succeeds.
	failOpened.Store(false)
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatalf("acquire after the failure cleared: %v", err)
	}
	if lease.HostData() != "host-data" {
		t.Fatalf("host data = %v", lease.HostData())
	}
	lease.Release()
}

// TestOnOpenedRunsBeforePublish pins the ordering (§15.3 D2, the
// stage-A blank-view bug): OnOpened must complete BEFORE the session
// is published. The observation is ORDERED, not raced: a second
// Acquire that takes the fast path on a published entry may return
// before OnOpened has produced the host data — so while OnOpened is
// gated, a concurrent Acquire must NOT be able to complete at all
// (the opening placeholder covers the whole open, OnOpened included).
// If OnOpened ran after publish, the fast path would let the second
// acquire finish while the hook is still blocked.
func TestOnOpenedRunsBeforePublish(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	// The failure path must not hang: t.Fatal fires BEFORE the
	// explicit close, and a blocked OnOpened would wedge the cleanup's
	// pool.Close until the package timeout. The once-guarded close is
	// both the fast-fail exit and the normal completion.
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()
	env := newTestEnv(t, func(o *Options) {
		o.OnOpened = func(sessionID string, sess *agent.Session) (any, error) {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release // hold the pre-publish window open
			return "host-data", nil
		}
	}, &gatedProvider{gate: make(chan struct{})})

	first := make(chan error, 1)
	go func() {
		lease, err := env.pool.Acquire(env.id)
		if err == nil {
			if lease.HostData() != "host-data" {
				err = errors.New("first acquire: nil host data")
			}
			lease.Release()
		}
		first <- err
	}()
	<-started // OnOpened is in flight; the entry must NOT be usable yet

	var secondDone atomic.Bool
	second := make(chan error, 1)
	go func() {
		lease, err := env.pool.Acquire(env.id)
		secondDone.Store(true)
		if err == nil {
			if lease.HostData() != "host-data" {
				err = errors.New("second acquire saw a published entry with nil host data — OnOpened ran after publish")
			}
			lease.Release()
		}
		second <- err
	}()

	// While the hook is gated the second acquire must NOT complete: in
	// the correct implementation it waits on the opening placeholder.
	// (Deterministic: the hook cannot progress until release closes.)
	deadline := time.Now().Add(300 * time.Millisecond)
	for !secondDone.Load() {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if secondDone.Load() {
		t.Fatal("a concurrent acquire completed while OnOpened was still in flight — the entry was published before the hook ran")
	}

	// Let the open complete; then collect both results.
	closeRelease()
	for i := 0; i < 2; i++ {
		select {
		case err := <-first:
			if err != nil {
				t.Fatal(err)
			}
		case err := <-second:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("acquire never returned — deadlock in the open path")
		}
	}
}

// TestDeleteDuringInFlightOpenAtPool: a delete that claims the id
// while an open is mid-flight wins — the open discards (ErrClosing),
// the delete proceeds, and the id converges to not-found. (The HTTP
// twin of this contract is TestDeleteDuringInFlightOpen in
// internal/web.)
func TestDeleteDuringInFlightOpenAtPool(t *testing.T) {
	gate := &locateGate{entered: make(chan struct{})}
	env := newTestEnv(t, func(o *Options) {
		inner := o.Locate
		o.Locate = func(id string) (string, string, error) {
			gate.maybeEnter()
			gate.mu.Lock()
			slow := gate.slow
			gate.mu.Unlock()
			if slow {
				time.Sleep(600 * time.Millisecond)
			}
			return inner(id)
		}
	}, &gatedProvider{gate: make(chan struct{})})
	gate.mu.Lock()
	gate.slow = true
	gate.mu.Unlock()

	openDone := make(chan error, 1)
	go func() {
		lease, err := env.pool.Acquire(env.id)
		if lease != nil {
			lease.Release()
		}
		openDone <- err
	}()
	<-gate.entered

	delDone := make(chan error, 1)
	go func() { delDone <- env.pool.Delete(env.id) }()

	if err := <-openDone; !errors.Is(err, ErrClosing) {
		t.Fatalf("open during delete: %v, want ErrClosing (discarded)", err)
	}
	if err := <-delDone; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := env.pool.Acquire(env.id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v, want ErrNotFound", err)
	}
}

// TestPoolCloseIsIdempotent: a double Close must not panic (close of
// a closed channel).
func TestPoolCloseIsIdempotent(t *testing.T) {
	env := newPlainEnv(t)
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	env.pool.Close()
	env.pool.Close()
}

// TestRootLockRejectsSecondPool: the serve root lock follows the pool
// — a second pool on the same root fails fast.
func TestRootLockRejectsSecondPool(t *testing.T) {
	env := newPlainEnv(t)
	_, err := New(Options{
		Store:        env.store,
		RootLockPath: filepath.Join(filepath.Dir(env.store.Root()), "serve.lock"),
		Locate:       func(string) (string, string, error) { return "", "", ErrNotFound },
	})
	if err == nil {
		t.Fatal("second pool on the same root succeeded")
	}
	var held *jsonl.LockHeldError
	if !errors.As(err, &held) {
		t.Fatalf("second pool error = %v, want a LockHeldError", err)
	}
}

// TestLeaseReleaseAfterReopenOnlyCountsItsEntry: a stale lease (its
// entry was evicted and the id reopened) must not decrement the NEW
// entry's refs.
func TestLeaseReleaseAfterReopenOnlyCountsItsEntry(t *testing.T) {
	oldEvict, oldInterval := SessionIdleEvict, SweeperInterval
	SessionIdleEvict, SweeperInterval = 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { SessionIdleEvict, SweeperInterval = oldEvict, oldInterval })
	env := newPlainEnv(t)

	stale, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	stale.Release()
	waitFor(t, 2*time.Second, func() bool {
		env.pool.mu.Lock()
		defer env.pool.mu.Unlock()
		_, busy := env.pool.closing[env.id]
		_, open := env.pool.sessions[env.id]
		return !busy && !open
	})

	fresh, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	env.pool.mu.Lock()
	refs := env.pool.sessions[env.id].refs
	env.pool.mu.Unlock()
	if refs != 1 {
		t.Fatalf("fresh entry refs = %d, want 1", refs)
	}
	stale.Release() // must not touch the fresh entry
	env.pool.mu.Lock()
	refs = env.pool.sessions[env.id].refs
	env.pool.mu.Unlock()
	if refs != 1 {
		t.Fatalf("stale release decremented the fresh entry: refs = %d, want 1", refs)
	}
	fresh.Release()
}
