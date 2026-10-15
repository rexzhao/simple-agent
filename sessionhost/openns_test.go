package sessionhost

// OpenInNamespace's counting rules (C8b): published entries count by
// their recorded namespace (including created-but-never-messaged
// sessions), an open still in flight does not, and the closing window
// does not — each rule pinned by its own observable state.

import (
	"testing"
)

// TestOpenInNamespace: the published table is the truth — an idle
// entry still counts, a wrong namespace does not, and the closing
// window (the entry already out of the table, teardown in flight)
// does not.
func TestOpenInNamespace(t *testing.T) {
	env := newPlainEnv(t)
	// The fixture's session is on disk but not open — acquire it.
	lease, err := env.pool.Acquire(env.id)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release() // idle, but the table entry remains
	if !env.pool.OpenInNamespace(env.ns) {
		t.Fatal("the open session's ns is not reported")
	}
	if env.pool.OpenInNamespace(env.ns + "x") {
		t.Fatal("a wrong ns is reported")
	}
	// Archive claims the closing window and removes the table entry.
	if err := env.pool.Archive(env.id); err != nil {
		t.Fatal(err)
	}
	if env.pool.OpenInNamespace(env.ns) {
		t.Fatal("the closing window still counts as open")
	}
}

// TestOpenInNamespaceOpeningNotCounted: an open held mid-flight in
// p.opening does NOT count — its ns is not settled until publish, and
// §17.4 accepts the in-flight window. The locate gate stretches the
// open's first IO so the in-flight state is observable.
func TestOpenInNamespaceOpeningNotCounted(t *testing.T) {
	gate := &locateGate{entered: make(chan struct{})}
	env := newTestEnv(t, func(o *Options) {
		inner := o.Locate
		o.Locate = func(id string) (string, string, error) {
			gate.maybeEnter()
			gate.mu.Lock()
			slow := gate.slow
			gate.mu.Unlock()
			if slow {
				<-gate.entered // held until released
			}
			return inner(id)
		}
	}, &gatedProvider{gate: make(chan struct{})})
	gate.mu.Lock()
	gate.slow = true
	gate.mu.Unlock()

	opened := make(chan error, 1)
	go func() {
		lease, err := env.pool.Acquire(env.id)
		if lease != nil {
			lease.Release()
		}
		opened <- err
	}()
	<-gate.entered // the open is mid-flight in p.opening

	if env.pool.OpenInNamespace(env.ns) {
		t.Fatal("an in-flight open counts as open — the ns is not settled until publish")
	}

	// Release: the open completes and publishes — now it counts.
	gate.mu.Lock()
	gate.slow = false
	gate.mu.Unlock()
	select {
	case <-gate.entered:
	default:
		close(gate.entered)
	}
	if err := <-opened; err != nil {
		t.Fatalf("the gated open failed: %v", err)
	}
	if !env.pool.OpenInNamespace(env.ns) {
		t.Fatal("the published entry is not reported")
	}
}
