package webapp

import (
	"context"
	"fmt"
	"sync"

	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/execution"
)

type codexLoginRegistry struct {
	ctx     context.Context
	service *execution.Service

	mu       sync.Mutex
	byTarget map[string]*codexLoginState
}

type codexLoginState struct {
	status execution.CodexAuthStatus
	cancel context.CancelFunc
}

func newCodexLoginRegistry(ctx context.Context, service *execution.Service) *codexLoginRegistry {
	return &codexLoginRegistry{ctx: ctx, service: service, byTarget: make(map[string]*codexLoginState)}
}

func (r *codexLoginRegistry) start(projectID, providerName string) (execution.CodexAuthStatus, error) {
	key := codexLoginKey(projectID, providerName)
	r.mu.Lock()
	if current := r.byTarget[key]; current != nil && current.status.Status == "pending" {
		status := current.status
		r.mu.Unlock()
		return status, nil
	}
	r.mu.Unlock()

	loginID, err := randomID("login-")
	if err != nil {
		return execution.CodexAuthStatus{}, err
	}
	loginCtx, cancel := context.WithCancel(r.ctx)
	pending, err := codexauth.StartDeviceLogin(loginCtx, codexauth.DeviceLoginOptions{})
	if err != nil {
		cancel()
		return execution.CodexAuthStatus{}, err
	}
	state := &codexLoginState{
		cancel: cancel,
		status: execution.CodexAuthStatus{
			Status:    "pending",
			LoginID:   loginID,
			UserCode:  pending.UserCode(),
			VerifyURL: pending.VerificationURI(),
		},
	}
	r.mu.Lock()
	if old := r.byTarget[key]; old != nil && old.cancel != nil {
		old.cancel()
	}
	r.byTarget[key] = state
	r.mu.Unlock()

	go r.complete(loginCtx, key, projectID, providerName, state, pending)
	return state.status, nil
}

func (r *codexLoginRegistry) complete(ctx context.Context, key, projectID, providerName string, state *codexLoginState, pending codexauth.PendingDeviceLogin) {
	result, err := pending.Complete(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byTarget[key] != state {
		return
	}
	if err != nil {
		state.status = execution.CodexAuthStatus{Status: "error", LoginID: state.status.LoginID, Message: err.Error()}
		return
	}
	if err := r.service.SaveCodexAuth(projectID, providerName, result.Token); err != nil {
		state.status = execution.CodexAuthStatus{Status: "error", LoginID: state.status.LoginID, Message: err.Error()}
		return
	}
	status, err := r.service.CodexAuthStatus(projectID, providerName)
	if err != nil {
		state.status = execution.CodexAuthStatus{Status: "error", LoginID: state.status.LoginID, Message: err.Error()}
		return
	}
	status.LoginID = state.status.LoginID
	state.status = status
}

func (r *codexLoginRegistry) status(projectID, providerName string) (execution.CodexAuthStatus, error) {
	key := codexLoginKey(projectID, providerName)
	r.mu.Lock()
	if current := r.byTarget[key]; current != nil {
		status := current.status
		r.mu.Unlock()
		return status, nil
	}
	r.mu.Unlock()
	return r.service.CodexAuthStatus(projectID, providerName)
}

func (r *codexLoginRegistry) clear(projectID, providerName string) error {
	key := codexLoginKey(projectID, providerName)
	r.mu.Lock()
	if current := r.byTarget[key]; current != nil {
		if current.cancel != nil {
			current.cancel()
		}
		delete(r.byTarget, key)
	}
	defer r.mu.Unlock()
	if err := r.service.ClearCodexAuth(projectID, providerName); err != nil {
		return fmt.Errorf("clear Codex login: %w", err)
	}
	return nil
}

func codexLoginKey(projectID, providerName string) string {
	return projectID + "\x00" + providerName
}
