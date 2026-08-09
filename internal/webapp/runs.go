package webapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/rexzhao/simple-agent/internal/execution"
)

const (
	defaultMaxConcurrentRuns = 8
	// Typed prompt.move uses the same bounded coordinator authority that the
	// former HTTP adapter used; this is not an HTTP request limit.
	maxActivePromptMoveDelta = 64
)

var (
	// ErrRunRegistryCapacity means the local Web application is already running
	// the maximum number of concurrent session runs.
	ErrRunRegistryCapacity = execution.ErrSessionRunCoordinatorCapacity
	ErrRunRegistryClosed   = errors.New("web run registry is closed")
)

type runRegistryOptions struct {
	MaxConcurrentRuns int

	// beforeDurableAdmissionCheck is a deterministic in-package test seam for
	// the post-admission handoff. Production construction leaves it nil.
	beforeDurableAdmissionCheck func(*execution.CoordinatedSessionRun)
}

func (o runRegistryOptions) withDefaults() runRegistryOptions {
	if o.MaxConcurrentRuns <= 0 {
		o.MaxConcurrentRuns = defaultMaxConcurrentRuns
	}
	return o
}

type runRegistry struct {
	service     *execution.Service
	coordinator *execution.SessionRunCoordinator
	log         io.Writer
	logMu       sync.Mutex
	options     runRegistryOptions
	mu          sync.Mutex
	closed      bool
	byID        map[string]*managedRun
}

type managedRun struct {
	id        string
	sessionID string
	run       *execution.CoordinatedSessionRun
}

func newRunRegistry(ctx context.Context, service *execution.Service, logWriter io.Writer) *runRegistry {
	return newRunRegistryWithOptions(ctx, service, logWriter, runRegistryOptions{})
}

func newRunRegistryWithOptions(ctx context.Context, service *execution.Service, logWriter io.Writer, options runRegistryOptions) *runRegistry {
	if ctx == nil {
		ctx = context.Background()
	}
	resolvedOptions := options.withDefaults()
	registry := &runRegistry{
		service: service,
		log:     logWriter,
		options: resolvedOptions,
		byID:    make(map[string]*managedRun),
	}
	coordinatorOptions := execution.SessionRunCoordinatorOptions{
		MaxConcurrentRuns:    resolvedOptions.MaxConcurrentRuns,
		OnRunAdmitted:        registry.admitRun,
		OnRunAdmissionFailed: registry.rejectRun,
		OnRunSettled:         registry.settleRun,
	}
	if service != nil {
		coordinatorOptions.DurableAdmitter = service
	}
	coordinator := execution.NewSessionRunCoordinator(ctx, service, coordinatorOptions)
	registry.coordinator = coordinator
	if service != nil {
		service.SetSessionRunCoordinator(coordinator)
	}
	return registry
}

func newManagedRun(id, sessionID string) *managedRun {
	return &managedRun{
		id:        id,
		sessionID: sessionID,
	}
}

// startDurable is the Web command entry point. The coordinator still owns
// the single active-run path; this method only supplies the durable
// identity fingerprint and returns the compact status acknowledgement.
func (r *runRegistry) startDurable(sessionID, content, runID, inputFingerprint string) (string, error) {
	return r.startDurableWithInput(sessionID, execution.SessionMessageInput{Content: content}, runID, inputFingerprint)
}

// continueDurable is the Web command entry point for a durable continuation.
// It deliberately supplies no content: the execution service resolves the
// interrupted target from durable session state while holding the same
// admission lock used by run.start, and persists that target in PreviousRunID.
func (r *runRegistry) continueDurable(sessionID, runID, inputFingerprint string) (string, error) {
	return r.startDurableWithInput(sessionID, execution.SessionMessageInput{Continue: true}, runID, inputFingerprint)
}

func (r *runRegistry) startDurableWithInput(sessionID string, input execution.SessionMessageInput, runID, inputFingerprint string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	runID = strings.TrimSpace(runID)
	if sessionID == "" || runID == "" {
		return "", fmt.Errorf("session id and run id are required")
	}
	if input.Continue {
		if strings.TrimSpace(input.Content) != "" || len(input.ContentBlocks) != 0 {
			return "", fmt.Errorf("continue cannot include new message content")
		}
	} else if strings.TrimSpace(input.Content) == "" && len(input.ContentBlocks) == 0 {
		return "", fmt.Errorf("message content or image attachment is required")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", ErrRunRegistryClosed
	}
	r.mu.Unlock()
	coordinated, admission, err := r.coordinator.StartDurable(sessionID, input, runID, inputFingerprint, nil)
	if err != nil {
		return "", err
	}
	if coordinated != nil && !admission.Created {
		return string(admission.Status), nil
	}
	if !admission.Created {
		return string(admission.Status), nil
	}
	if coordinated == nil {
		return "", fmt.Errorf("durable run admission returned no active handle")
	}
	if hook := r.options.beforeDurableAdmissionCheck; hook != nil {
		hook(coordinated)
	}
	if r.isClosed() {
		coordinated.Cancel()
		return "", ErrRunRegistryClosed
	}
	return string(execution.SessionRunRunning), nil
}

func (r *runRegistry) isClosed() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// admitRun is the first presentation-adapter phase of coordinator admission.
// It runs before the execution starter, which means a synchronous starter and
// an agent/session-tool start have the same process-local control registration
// guarantee as a Web start.
func (r *runRegistry) admitRun(run *execution.CoordinatedSessionRun) error {
	if r == nil || run == nil || strings.TrimSpace(run.ID()) == "" {
		return ErrRunRegistryClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRunRegistryClosed
	}
	if existing := r.byID[run.ID()]; existing != nil {
		if existing.run == run {
			return nil
		}
		return fmt.Errorf("run %q is already registered", run.ID())
	}
	managed := newManagedRun(run.ID(), run.SessionID())
	managed.run = run
	r.byID[managed.id] = managed
	return nil
}

// rejectRun rolls back adapter admission when the coordinator cannot start a
// SessionRun after reserving the handle (for example, a nil starter result).
func (r *runRegistry) rejectRun(run *execution.CoordinatedSessionRun) {
	if r == nil || run == nil {
		return
	}
	r.mu.Lock()
	if managed := r.byID[run.ID()]; managed != nil && managed.run == run {
		delete(r.byID, run.ID())
	}
	r.mu.Unlock()
}

// settleRun closes the process-local control lifetime for every coordinated
// run. Durable run/session state remains the source of terminal status; this
// registry only retains handles while they can still be controlled.
func (r *runRegistry) settleRun(run *execution.CoordinatedSessionRun, _ execution.SessionMessageResult, err error) {
	managed, ok := r.lookupManaged(run)
	if !ok {
		return
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logRunFailure(managed, err)
		}
	}
	r.mu.Lock()
	if !r.closed && r.byID[managed.id] == managed {
		delete(r.byID, managed.id)
	}
	r.mu.Unlock()
}

func (r *runRegistry) lookupManaged(run *execution.CoordinatedSessionRun) (*managedRun, bool) {
	if r == nil || run == nil || strings.TrimSpace(run.ID()) == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	managed, ok := r.byID[run.ID()]
	if !ok || managed.run != run {
		return nil, false
	}
	return managed, true
}

// Close makes the registry reject new runs and cancels active SessionRuns.
func (r *runRegistry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.byID = make(map[string]*managedRun)
	coordinator := r.coordinator
	service := r.service
	r.mu.Unlock()
	if service != nil {
		service.ClearSessionRunCoordinator(coordinator)
	}
	coordinator.Close()
}

func (r *runRegistry) logRunFailure(managed *managedRun, err error) {
	if r == nil || r.log == nil || managed == nil || err == nil {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	fmt.Fprintf(r.log, "sai: run %s for session %s failed: %v\n", managed.id, managed.sessionID, err)
}

func (r *runRegistry) get(id string) (*managedRun, bool) {
	id = strings.TrimSpace(id)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, false
	}
	managed, ok := r.byID[id]
	r.mu.Unlock()
	return managed, ok
}

// activeRunControl resolves a process-local control target. The lookup first
// checks the Web adapter's active identity so a run ID cannot be used to search
// another session, then confirms that the same handle is still owned by the
// shared coordinator. No durable row or operation claim is consulted.
func (r *runRegistry) activeRunControl(sessionID, runID string) (*execution.CoordinatedSessionRun, error) {
	sessionID = strings.TrimSpace(sessionID)
	runID = strings.TrimSpace(runID)
	if r == nil {
		return nil, execution.ErrRunControlNotFound
	}
	managed, ok := r.get(runID)
	if !ok || managed == nil {
		return nil, execution.ErrRunControlNotFound
	}
	if managed.sessionID != sessionID {
		return nil, execution.ErrRunControlWrongSession
	}
	if managed.run == nil {
		return nil, execution.ErrRunControlNotActive
	}
	if managed.run.Status() != execution.SessionRunRunning {
		return nil, execution.ErrRunControlRunSettled
	}
	if r.coordinator == nil {
		return nil, execution.ErrRunControlNotActive
	}
	active, ok := r.coordinator.ActiveForSession(sessionID)
	if !ok || active.ID() != runID || !active.ActiveControlReady() {
		return nil, execution.ErrRunControlNotActive
	}
	return active, nil
}

func (r *runRegistry) removeActivePrompt(sessionID, runID, promptID string) error {
	active, err := r.activeRunControl(sessionID, runID)
	if err != nil {
		return err
	}
	if !active.ActivePromptReady() {
		return execution.ErrRunControlNotActive
	}
	if !active.RemoveActive(promptID) {
		return execution.ErrRunControlPromptNotFound
	}
	return nil
}

func (r *runRegistry) steerActivePrompt(sessionID, runID, promptID string, steer bool) error {
	active, err := r.activeRunControl(sessionID, runID)
	if err != nil {
		return err
	}
	if !active.ActivePromptReady() {
		return execution.ErrRunControlNotActive
	}
	if !active.SetActivePromptSteer(promptID, steer) {
		return execution.ErrRunControlPromptNotFound
	}
	return nil
}

func (r *runRegistry) moveActivePrompt(sessionID, runID, promptID string, delta int) (bool, error) {
	active, err := r.activeRunControl(sessionID, runID)
	if err != nil {
		return false, err
	}
	if !active.ActivePromptReady() {
		return false, execution.ErrRunControlNotActive
	}
	found, moved := active.MoveActivePromptResult(promptID, delta)
	if !found {
		return false, execution.ErrRunControlPromptNotFound
	}
	return moved, nil
}

func (r *runRegistry) cancelToolCall(sessionID, runID, toolCallID string) error {
	active, err := r.activeRunControl(sessionID, runID)
	if err != nil {
		return err
	}
	if !active.CancelToolCall(toolCallID) {
		return execution.ErrRunControlToolNotActive
	}
	return nil
}

func (r *runRegistry) cancel(id string) (*managedRun, bool) {
	managed, ok := r.get(id)
	if ok {
		managed.run.Cancel()
	}
	return managed, ok
}

func randomID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + hex.EncodeToString(raw), nil
}
