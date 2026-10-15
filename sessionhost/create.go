package sessionhost

// Pool.Create (C4, §17.2): the session-creation entry. The meta is
// validated through the SHARED agent.ValidateSessionMeta (the same
// function the CLI's /new uses), then the session is created and
// published through the same sequence as Acquire's slow path.

import (
	"time"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/storage"
)

// Create validates the meta, creates a fresh session in the given
// namespace, and publishes it into the pool — the same sequence as
// Acquire's slow path (OnOpened before publish, defer cleans up on
// every exit). The host resolves the ns→projectPath mapping (the web
// reads its registry) and passes it here; sessionhost has no registry
// concept. The returned Lease holds the only reference (refs=1).
func (p *Pool) Create(ns, projectPath string, resolver agent.ModelResolver, meta storage.SessionMeta) (*Lease, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, Coded(ErrClosing, "")
	}
	p.mu.Unlock()

	if err := agent.ValidateSessionMeta(resolver, meta); err != nil {
		return nil, err
	}

	ag, err := p.agentForCached(ns, projectPath)
	if err != nil {
		return nil, Coded(ErrProjectUnavailable, err.Error())
	}

	session, err := ag.NewSession(ns, meta)
	if err != nil {
		return nil, err
	}
	id := session.ID()

	// OnOpened before publish — the same protection as Acquire's
	// slow path.
	var hostData any
	if p.opts.OnOpened != nil {
		hostData, err = p.opts.OnOpened(id, session)
		if err != nil {
			_ = session.Close()
			return nil, err
		}
	}

	p.mu.Lock()
	// Recheck closed under the publish lock: Pool.Close may have run
	// while we were assembling (agentForCached + NewSession + OnOpened
	// — the host callback can be slow). Acquire's slow path has the
	// same recheck; without it the entry publishes into a closed pool
	// and nothing will ever tear it down.
	if p.closed {
		p.mu.Unlock()
		if p.opts.OnClosed != nil {
			p.opts.OnClosed(id, session, hostData)
		}
		_ = session.Close()
		return nil, Coded(ErrClosing, "")
	}
	// ID guard: 26-char random makes collisions zero-probability, but
	// overwriting a live entry is worse than a cheap check. (ErrConflict
	// here is the dispatch-set collision, not the multi-project
	// meaning — zero-probability path.)
	if _, exists := p.sessions[id]; exists {
		p.mu.Unlock()
		if p.opts.OnClosed != nil {
			p.opts.OnClosed(id, session, hostData)
		}
		_ = session.Close()
		return nil, Coded(ErrConflict, "session id collision: "+id)
	}
	entry := &poolEntry{
		session:   session,
		hostData:  hostData,
		projectID: ns,
		refs:      1,
		idleSince: time.Now(),
	}
	p.sessions[id] = entry
	p.mu.Unlock()
	return p.lease(id, entry), nil
}
