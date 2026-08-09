package webapp

import (
	"context"
	"errors"
	"sync"

	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/codexlogin"
	"github.com/rexzhao/simple-agent/internal/execution"
)

// codexLoginRegistry is the execution-owned lifecycle coordinator used by
// the typed command adapter. The sync resource only receives identifier-only
// publications and reads the safe
// status projection through status; it never owns or copies the login state
// machine or token.
type codexLoginRegistry struct {
	ctx     context.Context
	service *execution.Service

	mu       sync.Mutex
	byTarget map[string]*codexLoginState
	sink     codexlogin.ChangeSink
	nextGen  uint64
	// Tests may supply codexauth's normal PendingDeviceLogin backed by a fake
	// HTTP server. Production leaves this nil and uses execution directly.
	startDeviceLogin func(context.Context, string) (codexauth.PendingDeviceLogin, error)
	// Tests may replace only the completion boundary to deterministically hold
	// an otherwise real pending device flow. Production always uses the
	// codexauth state machine directly.
	completeDeviceLogin func(context.Context, codexauth.PendingDeviceLogin) (codexauth.DeviceLoginResult, error)
}

type codexLoginState struct {
	generation uint64
	status     execution.CodexAuthStatus
	cancel     context.CancelFunc
}

type codexLoginFailure struct {
	code    string
	message string
	err     error
}

func (e *codexLoginFailure) Error() string {
	if e == nil || e.message == "" {
		return "Codex login failed"
	}
	return e.message
}

func (e *codexLoginFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func newCodexLoginRegistry(ctx context.Context, service *execution.Service) *codexLoginRegistry {
	if ctx == nil {
		ctx = context.Background()
	}
	return &codexLoginRegistry{ctx: ctx, service: service, byTarget: make(map[string]*codexLoginState)}
}

func (r *codexLoginRegistry) setSink(sink codexlogin.ChangeSink) {
	r.mu.Lock()
	r.sink = sink
	r.mu.Unlock()
}

func (r *codexLoginRegistry) publish(providerName string) {
	r.mu.Lock()
	sink := r.sink
	r.mu.Unlock()
	if sink != nil {
		// A publication is a bounded wake-up. The provider reloads the current
		// safe status and intentionally receives no error/token payload here.
		_ = sink.PublishCommitted(codexlogin.CommittedChange{Provider: providerName})
	}
}

func (r *codexLoginRegistry) validate(providerName string) error {
	if r == nil || r.service == nil {
		return &codexLoginFailure{code: "codex_provider_unavailable", message: "Codex provider is unavailable"}
	}
	if err := r.service.ValidateCodexProvider(providerName); err != nil {
		switch {
		case errors.Is(err, execution.ErrCodexProviderNotFound):
			return &codexLoginFailure{code: "codex_provider_not_found", message: "Codex provider was not found", err: err}
		case errors.Is(err, execution.ErrCodexProviderNotCodex), errors.Is(err, execution.ErrCodexProviderNoAuthFile):
			return &codexLoginFailure{code: "codex_provider_invalid", message: "Provider is not configured for Codex", err: err}
		default:
			return &codexLoginFailure{code: "codex_provider_unavailable", message: "Codex provider is unavailable", err: err}
		}
	}
	return nil
}

func (r *codexLoginRegistry) start(providerName string) (execution.CodexAuthStatus, error) {
	if err := r.validate(providerName); err != nil {
		return execution.CodexAuthStatus{}, err
	}
	key := codexLoginKey(providerName)

	r.mu.Lock()
	if current := r.byTarget[key]; current != nil && current.status.Status == "pending" {
		status := current.status
		r.mu.Unlock()
		return status, nil
	}
	r.nextGen++
	generation := r.nextGen
	r.mu.Unlock()

	loginID, err := randomID("login-")
	if err != nil {
		return execution.CodexAuthStatus{}, &codexLoginFailure{code: "codex_login_start_failed", message: "Codex login could not be started", err: err}
	}
	loginCtx, cancel := context.WithCancel(r.ctx)
	state := &codexLoginState{
		generation: generation,
		cancel:     cancel,
		status:     execution.CodexAuthStatus{Status: "pending", LoginID: loginID},
	}
	r.mu.Lock()
	// Reserve the target before the external request. A concurrent start joins
	// this state, and clear can invalidate it before the request returns.
	if current := r.byTarget[key]; current != nil && current.status.Status == "pending" {
		status := current.status
		r.mu.Unlock()
		cancel()
		return status, nil
	}
	r.byTarget[key] = state
	r.mu.Unlock()
	r.publish(providerName)

	startDeviceLogin := r.startDeviceLogin
	if startDeviceLogin == nil {
		startDeviceLogin = r.service.StartCodexDeviceLogin
	}
	pending, err := startDeviceLogin(loginCtx, providerName)
	if err != nil {
		cancel()
		r.mu.Lock()
		current := r.byTarget[key]
		if current == state {
			state.status = execution.CodexAuthStatus{Status: "error", LoginID: loginID}
		}
		r.mu.Unlock()
		r.publish(providerName)
		return execution.CodexAuthStatus{}, &codexLoginFailure{code: "codex_login_start_failed", message: "Codex login could not be started", err: err}
	}
	deviceStatus := execution.CodexAuthStatus{
		Status:    "pending",
		LoginID:   loginID,
		UserCode:  pending.UserCode(),
		VerifyURL: pending.VerificationURI(),
	}
	if err := codexlogin.ValidateDeviceCapabilities(providerName, deviceStatus); err != nil {
		cancel()
		r.mu.Lock()
		if r.byTarget[key] == state {
			state.status = execution.CodexAuthStatus{Status: "error", LoginID: loginID}
		}
		r.mu.Unlock()
		r.publish(providerName)
		return execution.CodexAuthStatus{}, &codexLoginFailure{code: "codex_login_start_failed", message: "Codex login could not be started", err: err}
	}

	r.mu.Lock()
	if r.byTarget[key] != state || state.generation != generation {
		r.mu.Unlock()
		cancel()
		return execution.CodexAuthStatus{}, &codexLoginFailure{code: "codex_login_superseded", message: "Codex login was superseded", err: context.Canceled}
	}
	state.status.UserCode = deviceStatus.UserCode
	state.status.VerifyURL = deviceStatus.VerifyURL
	status := state.status
	r.mu.Unlock()
	r.publish(providerName)

	go r.complete(loginCtx, key, providerName, state, pending)
	return status, nil
}

func (r *codexLoginRegistry) complete(ctx context.Context, key, providerName string, state *codexLoginState, pending codexauth.PendingDeviceLogin) {
	completeDeviceLogin := r.completeDeviceLogin
	var result codexauth.DeviceLoginResult
	var err error
	if completeDeviceLogin != nil {
		result, err = completeDeviceLogin(ctx, pending)
	} else {
		result, err = pending.Complete(ctx)
	}
	r.mu.Lock()
	if r.byTarget[key] != state {
		r.mu.Unlock()
		return
	}
	if err != nil || ctx.Err() != nil {
		state.status = execution.CodexAuthStatus{Status: "error", LoginID: state.status.LoginID}
		r.mu.Unlock()
		r.publish(providerName)
		return
	}
	// Serialize token persistence with clear. Once this lock is held, clear
	// cannot remove the file until SaveCodexAuth completes, and then clear
	// removes that exact result before allowing any stale completion to return.
	if err := r.service.SaveCodexAuth(providerName, result.Token); err != nil {
		state.status = execution.CodexAuthStatus{Status: "error", LoginID: state.status.LoginID}
		r.mu.Unlock()
		r.publish(providerName)
		return
	}
	status, err := r.service.CodexAuthStatus(providerName)
	if err != nil {
		state.status = execution.CodexAuthStatus{Status: "error", LoginID: state.status.LoginID}
		r.mu.Unlock()
		r.publish(providerName)
		return
	}
	status.LoginID = state.status.LoginID
	state.status = status
	r.mu.Unlock()
	r.publish(providerName)
}

func (r *codexLoginRegistry) status(providerName string) (execution.CodexAuthStatus, error) {
	key := codexLoginKey(providerName)
	r.mu.Lock()
	if current := r.byTarget[key]; current != nil {
		status := current.status
		r.mu.Unlock()
		return status, nil
	}
	r.mu.Unlock()
	if err := r.validate(providerName); err != nil {
		return execution.CodexAuthStatus{}, err
	}
	status, err := r.service.CodexAuthStatus(providerName)
	if err != nil {
		return execution.CodexAuthStatus{}, &codexLoginFailure{code: "codex_login_status_failed", message: "Codex login status is unavailable", err: err}
	}
	return status, nil
}

func (r *codexLoginRegistry) clear(providerName string) error {
	if err := r.validate(providerName); err != nil {
		return err
	}
	key := codexLoginKey(providerName)
	r.mu.Lock()
	if current := r.byTarget[key]; current != nil {
		if current.cancel != nil {
			current.cancel()
		}
		delete(r.byTarget, key)
	}
	// Keep the registry lock while clearing the durable file. This is the
	// linearization point that prevents a completed old login from writing
	// after clear.
	err := r.service.ClearCodexAuth(providerName)
	r.mu.Unlock()
	if err != nil {
		r.publish(providerName)
		return &codexLoginFailure{code: "codex_login_clear_failed", message: "Codex login could not be cleared", err: err}
	}
	// Signed-out clear is a target-state no-op: the resource owner compares
	// snapshots and does not append a duplicate journal entry.
	r.publish(providerName)
	return nil
}

func codexLoginKey(providerName string) string { return providerName }

func codexLoginFailureCode(err error) string {
	var failure *codexLoginFailure
	if errors.As(err, &failure) && failure.code != "" {
		return failure.code
	}
	return "codex_login_failed"
}

func codexLoginFailureMessage(err error) string {
	var failure *codexLoginFailure
	if errors.As(err, &failure) && failure.message != "" {
		return failure.message
	}
	return "Codex login failed"
}
