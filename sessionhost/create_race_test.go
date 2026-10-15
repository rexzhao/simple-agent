package sessionhost

// Pool.Create race and lifecycle tests (C4 review): the Close race
// (Create must not publish into a closed pool), the OnOpened failure
// cleanup, and the evictability of a created-then-released session.

import (
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/storage"
	"github.com/rexzhao/simple-agent/storage/jsonl"
)

// TestCreateDuringPoolClose pins the Close race: Create's publish
// rechecks p.closed under the lock — a Close that runs while the
// OnOpened callback is blocked must cause the Create to discard (not
// publish an orphan into the torn-down pool).
func TestCreateDuringPoolClose(t *testing.T) {
	blockOnOpened := make(chan struct{})
	releaseOnOpened := make(chan struct{})
	var onClosedIDs []string
	// Build a fresh pool with a blocking OnOpened + OnClosed counter.
	root := t.TempDir()
	store2, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	factory := func(projectCwd string) (*agent.Agent, error) {
		return agent.New(agent.Options{
			ModelResolver: agent.NewStaticResolver(&gatedProvider{gate: make(chan struct{})}, nil, 0, 0, ""),
			Storage:       store2,
			CWD:           projectCwd,
		})
	}
	pool, err := New(Options{
		Store:        store2,
		RootLockPath: filepath.Join(root, "serve.lock"),
		AgentFor:     factory,
		OnOpened: func(sessionID string, sess *agent.Session) (any, error) {
			close(blockOnOpened)
			<-releaseOnOpened
			return nil, nil
		},
		OnClosed: func(sessionID string, sess *agent.Session, hostData any) {
			onClosedIDs = append(onClosedIDs, sessionID)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	ns := "p" + strings.Repeat("0", 16)
	projectDir := t.TempDir()
	meta := storage.SessionMeta{Provider: "p", Model: "m"}
	done := make(chan error, 1)
	go func() {
		lease, err := pool.Create(ns, projectDir, agent.NewStaticResolver(&gatedProvider{gate: make(chan struct{})}, nil, 0, 0, ""), meta)
		if lease != nil {
			lease.Release()
		}
		done <- err
	}()

	// Wait for OnOpened to be in flight (Create is past the initial
	// closed check), then Close the pool.
	<-blockOnOpened
	pool.Close()
	close(releaseOnOpened)

	if err := <-done; !errors.Is(err, ErrClosing) {
		t.Fatalf("Create during Close: err = %v, want ErrClosing", err)
	}
	if len(onClosedIDs) != 1 {
		t.Fatalf("OnClosed calls = %d, want exactly 1 (the discarded entry)", len(onClosedIDs))
	}
	if ids := pool.OpenIDs(); len(ids) != 0 {
		t.Fatalf("the closed pool still has open sessions: %v", ids)
	}
}

// TestCreateOnOpenedFailureLeavesNoEntry: a failing OnOpened closes
// the session and leaves no pool entry (the same shape as Acquire's
// OnOpened failure — OnClosed is NOT called: the host's companion
// object was never built).
func TestCreateOnOpenedFailureLeavesNoEntry(t *testing.T) {
	var onClosedCount int
	root := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := New(Options{
		Store:        store,
		RootLockPath: filepath.Join(root, "serve.lock"),
		AgentFor: func(projectCwd string) (*agent.Agent, error) {
			return agent.New(agent.Options{
				ModelResolver: agent.NewStaticResolver(&gatedProvider{gate: make(chan struct{})}, nil, 0, 0, ""),
				Storage:       store,
				CWD:           projectCwd,
			})
		},
		OnOpened: func(sessionID string, sess *agent.Session) (any, error) {
			return nil, errors.New("injected host failure")
		},
		OnClosed: func(sessionID string, sess *agent.Session, hostData any) {
			onClosedCount++
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	ns := "p" + strings.Repeat("0", 16)
	meta := storage.SessionMeta{Provider: "p", Model: "m"}
	_, err = pool.Create(ns, t.TempDir(), agent.NewStaticResolver(&gatedProvider{gate: make(chan struct{})}, nil, 0, 0, ""), meta)
	if err == nil || err.Error() != "injected host failure" {
		t.Fatalf("Create with failing OnOpened: err = %v, want the hook's error", err)
	}
	if ids := pool.OpenIDs(); len(ids) != 0 {
		t.Fatalf("failed OnOpened left entries: %v", ids)
	}
	if onClosedCount != 0 {
		t.Fatalf("OnClosed ran %d times for a failed OnOpened — should not run (no companion object)", onClosedCount)
	}
}

// TestCreateThenReleaseEvictable: a created-then-released session is
// evictable (the sweeper tears it down; nothing on disk since the
// header is lazy). This proves the lease was truly released.
func TestCreateThenReleaseEvictable(t *testing.T) {
	root := t.TempDir()
	store, err := jsonl.OpenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	var evictedCount atomic.Int32
	pool, err := New(Options{
		Store:         store,
		RootLockPath:  filepath.Join(root, "serve.lock"),
		IdleWindow:    50 * time.Millisecond,
		SweepInterval: 20 * time.Millisecond,
		AgentFor: func(projectCwd string) (*agent.Agent, error) {
			return agent.New(agent.Options{
				ModelResolver: agent.NewStaticResolver(&gatedProvider{gate: make(chan struct{})}, nil, 0, 0, ""),
				Storage:       store,
				CWD:           projectCwd,
			})
		},
		OnEvicted: func(sessionID string, sess *agent.Session, hostData any) {
			evictedCount.Add(1)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	ns := "p" + strings.Repeat("0", 16)
	meta := storage.SessionMeta{Provider: "p", Model: "m"}
	lease, err := pool.Create(ns, t.TempDir(), agent.NewStaticResolver(&gatedProvider{gate: make(chan struct{})}, nil, 0, 0, ""), meta)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if evictedCount.Load() > 0 {
			return // pass
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the released created session was never evicted — the lease is still held")
}
