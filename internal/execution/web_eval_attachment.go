package execution

import (
	"context"
	"errors"
	"sync"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

// WebEvalExecutor is the deliberately narrow execution boundary used by the
// Agent. The connection and browser-facing broker remain owned by webapp.
type WebEvalExecutor interface {
	Execute(context.Context, string, int) (protocol.DebugExecutionResultPayload, error)
}

type webEvalExecutorSwitch interface {
	WebEvalExecutor
	Enabled() bool
}

// ErrWebEvalExecutorUnavailable is intentionally not suitable for user
// output. The web.eval adapter translates it to a stable public error code.
var ErrWebEvalExecutorUnavailable = errors.New("web debug executor attachment is unavailable")

// WebEvalExecutorRegistration is an owner-scoped, compare-and-swap-safe
// attachment. A registration is never redirected to a replacement executor.
type WebEvalExecutorRegistration struct {
	service    *Service
	executor   WebEvalExecutor
	generation uint64
	once       sync.Once
}

// RegisterWebEvalExecutor installs the current executor and returns the
// exact registration token its owner must later unregister. Installing a new
// executor replaces the current one; an old token can only remove itself if
// it is still current.
func (s *Service) RegisterWebEvalExecutor(executor WebEvalExecutor) *WebEvalExecutorRegistration {
	if s == nil || executor == nil {
		return nil
	}
	if switched, ok := executor.(webEvalExecutorSwitch); ok && !switched.Enabled() {
		return nil
	}
	s.webEvalMu.Lock()
	defer s.webEvalMu.Unlock()
	s.webEvalNextID++
	registration := &WebEvalExecutorRegistration{
		service:    s,
		executor:   executor,
		generation: s.webEvalNextID,
	}
	s.webEvalAttachment = registration
	return registration
}

// CurrentWebEvalExecutor returns the authoritative current registration. The
// returned token is a snapshot: callers must check it again at execution
// time, because server close and owner replacement are concurrent events.
func (s *Service) CurrentWebEvalExecutor() *WebEvalExecutorRegistration {
	if s == nil {
		return nil
	}
	s.webEvalMu.RLock()
	registration := s.webEvalAttachment
	s.webEvalMu.RUnlock()
	return registration
}

// Generation identifies the registration captured by a prepared Agent
// runtime. It is useful for tests and diagnostics without exposing the
// underlying executor.
func (r *WebEvalExecutorRegistration) Generation() uint64 {
	if r == nil {
		return 0
	}
	return r.generation
}

func (r *WebEvalExecutorRegistration) current() bool {
	if r == nil || r.service == nil || r.executor == nil {
		return false
	}
	r.service.webEvalMu.RLock()
	current := r.service.webEvalAttachment == r
	r.service.webEvalMu.RUnlock()
	return current
}

// IsCurrent reports whether this exact owner/generation is still attached.
func (r *WebEvalExecutorRegistration) IsCurrent() bool {
	return r.current()
}

// Execute refuses stale registrations before delegating. The current() check
// is the linearization point: its lock is released before the delegate runs,
// so Close/replacement cannot be blocked by browser execution. If replacement
// races after this point, the delegate is still the originally captured
// executor and is never migrated or replayed onto the replacement.
func (r *WebEvalExecutorRegistration) Execute(ctx context.Context, code string, timeoutMS int) (protocol.DebugExecutionResultPayload, error) {
	if !r.current() {
		return protocol.DebugExecutionResultPayload{}, ErrWebEvalExecutorUnavailable
	}
	return r.executor.Execute(ctx, code, timeoutMS)
}

// Unregister performs an owner-scoped CAS. Closing an old Server therefore
// cannot detach a newer Server's registration.
func (r *WebEvalExecutorRegistration) Unregister() {
	if r == nil || r.service == nil {
		return
	}
	r.once.Do(func() {
		r.service.webEvalMu.Lock()
		if r.service.webEvalAttachment == r {
			r.service.webEvalAttachment = nil
		}
		r.service.webEvalMu.Unlock()
	})
}
