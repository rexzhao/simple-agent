package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultMaxConcurrentSessionRuns = 8

var (
	// ErrSessionRunCoordinatorCapacity means the application-wide active-run
	// limit has been reached. Every presentation and agent entry point must use
	// the same coordinator so this limit cannot be bypassed.
	ErrSessionRunCoordinatorCapacity = errors.New("session run coordinator is at its concurrent run limit")
	ErrSessionRunCoordinatorClosed   = errors.New("session run coordinator is closed")
)

// SessionRunStarter is the low-level execution operation used by
// SessionRunCoordinator after it has atomically reserved a session/run slot.
// Service implements this interface.
type SessionRunStarter interface {
	StartSessionRunWithInput(ctx context.Context, sessionID string, input SessionMessageInput, emit func(SessionStreamEvent)) *SessionRun
}

type SessionRunStarterWithID interface {
	StartSessionRunWithID(ctx context.Context, sessionID string, input SessionMessageInput, runID string, emit func(SessionStreamEvent)) *SessionRun
}

// SessionRunCoordinatorOptions controls application-wide run admission.
type SessionRunCoordinatorOptions struct {
	MaxConcurrentRuns int
	Now               func() time.Time
	NewRunID          func() (string, error)
	// OnRunEvent observes every event from every admitted run, including runs
	// started by agent tools. It lets presentation adapters attach one shared
	// replay layer without supplying a per-Start callback.
	OnRunEvent func(*CoordinatedSessionRun, SessionStreamEvent)
	// OnRunSettled observes the stable result after Wait returns and before the
	// run is removed from the coordinator's active indexes.
	OnRunSettled func(*CoordinatedSessionRun, SessionMessageResult, error)
}

func (options SessionRunCoordinatorOptions) withDefaults() SessionRunCoordinatorOptions {
	if options.MaxConcurrentRuns <= 0 {
		options.MaxConcurrentRuns = defaultMaxConcurrentSessionRuns
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewRunID == nil {
		options.NewRunID = newSessionRunID
	}
	return options
}

// SessionRunDescriptor is the stable, presentation-neutral description of an
// active coordinated run.
type SessionRunDescriptor struct {
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Status    string    `json:"status"`
}

// SessionRunCoordinator is the application-wide owner of active SessionRuns.
// It enforces one active run per session and a global concurrency limit. Web
// replay buffers and future agent tools are adapters over this shared owner;
// they must not maintain competing active-run registries.
type SessionRunCoordinator struct {
	ctx     context.Context
	cancel  context.CancelFunc
	starter SessionRunStarter
	options SessionRunCoordinatorOptions

	mu              sync.Mutex
	closed          bool
	byID            map[string]*CoordinatedSessionRun
	activeBySession map[string]*CoordinatedSessionRun
	wg              sync.WaitGroup

	lifecycleMu  sync.RWMutex
	onRunStarted func(*CoordinatedSessionRun)
	onRunSettled func(*CoordinatedSessionRun, SessionMessageResult, error)
	onRunIdle    func(*CoordinatedSessionRun)
}

// CoordinatedSessionRun is a concurrency-safe handle to one admitted run.
type CoordinatedSessionRun struct {
	id        string
	sessionID string
	startedAt time.Time
	run       *SessionRun

	mu     sync.Mutex
	turnID string
}

func NewSessionRunCoordinator(ctx context.Context, starter SessionRunStarter, options SessionRunCoordinatorOptions) *SessionRunCoordinator {
	if ctx == nil {
		ctx = context.Background()
	}
	rootCtx, cancel := context.WithCancel(ctx)
	return &SessionRunCoordinator{
		ctx:             rootCtx,
		cancel:          cancel,
		starter:         starter,
		options:         options.withDefaults(),
		byID:            make(map[string]*CoordinatedSessionRun),
		activeBySession: make(map[string]*CoordinatedSessionRun),
	}
}

// SetLifecycleCallbacks installs the service-owned lifecycle observer. The
// observer is separate from the presentation callbacks in
// SessionRunCoordinatorOptions so Web and agent-tool starts use the same
// publication path even when no Web request supplied an emitter.
func (coordinator *SessionRunCoordinator) SetLifecycleCallbacks(
	onRunStarted func(*CoordinatedSessionRun),
	onRunSettled func(*CoordinatedSessionRun, SessionMessageResult, error),
) {
	if coordinator == nil {
		return
	}
	coordinator.lifecycleMu.Lock()
	coordinator.onRunStarted = onRunStarted
	coordinator.onRunSettled = onRunSettled
	coordinator.lifecycleMu.Unlock()
}

// SetRunIdleCallback installs an observer which is called after a run has been
// removed from the coordinator's active indexes. It is deliberately separate
// from SetLifecycleCallbacks so installing the durable completion dispatcher
// cannot replace the existing lifecycle/SSE callbacks (or the presentation
// callbacks supplied in SessionRunCoordinatorOptions).
func (coordinator *SessionRunCoordinator) SetRunIdleCallback(callback func(*CoordinatedSessionRun)) {
	if coordinator == nil {
		return
	}
	coordinator.lifecycleMu.Lock()
	coordinator.onRunIdle = callback
	coordinator.lifecycleMu.Unlock()
}

func (coordinator *SessionRunCoordinator) notifyRunStarted(run *CoordinatedSessionRun) {
	if coordinator == nil {
		return
	}
	coordinator.lifecycleMu.RLock()
	callback := coordinator.onRunStarted
	coordinator.lifecycleMu.RUnlock()
	if callback != nil {
		callback(run)
	}
}

func (coordinator *SessionRunCoordinator) notifyRunSettled(run *CoordinatedSessionRun, result SessionMessageResult, err error) {
	if coordinator == nil {
		return
	}
	coordinator.lifecycleMu.RLock()
	callback := coordinator.onRunSettled
	coordinator.lifecycleMu.RUnlock()
	if callback != nil {
		callback(run, result, err)
	}
}

func (coordinator *SessionRunCoordinator) notifyRunIdle(run *CoordinatedSessionRun) {
	if coordinator == nil {
		return
	}
	coordinator.lifecycleMu.RLock()
	callback := coordinator.onRunIdle
	coordinator.lifecycleMu.RUnlock()
	if callback != nil {
		callback(run)
	}
}

// Start atomically admits and starts a run. The run is rooted in the
// coordinator context rather than an HTTP request or tool-call context, so an
// asynchronous run outlives the request that created it.
func (coordinator *SessionRunCoordinator) Start(sessionID string, input SessionMessageInput, emit func(SessionStreamEvent)) (*CoordinatedSessionRun, error) {
	if coordinator == nil {
		return nil, fmt.Errorf("session run coordinator is not configured")
	}
	if coordinator.starter == nil {
		return nil, fmt.Errorf("session run starter is not configured")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	runID, err := coordinator.options.NewRunID()
	if err != nil {
		return nil, err
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, fmt.Errorf("generated run id is blank")
	}
	return coordinator.startWithID(sessionID, input, runID, emit)
}

// StartWithID admits a run using a caller-provided stable id. Durable retry
// paths use this for at-least-once notifications: a crash after admission but
// before the inbox row is marked consumed can discover the same run instead of
// creating a second durable run.
func (coordinator *SessionRunCoordinator) StartWithID(sessionID string, input SessionMessageInput, runID string, emit func(SessionStreamEvent)) (*CoordinatedSessionRun, error) {
	if coordinator == nil {
		return nil, fmt.Errorf("session run coordinator is not configured")
	}
	if coordinator.starter == nil {
		return nil, fmt.Errorf("session run starter is not configured")
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, fmt.Errorf("run id is required")
	}
	if _, ok := coordinator.starter.(SessionRunStarterWithID); !ok {
		return nil, fmt.Errorf("session run starter does not support stable run ids")
	}
	return coordinator.startWithID(sessionID, input, runID, emit)
}

func (coordinator *SessionRunCoordinator) startWithID(sessionID string, input SessionMessageInput, runID string, emit func(SessionStreamEvent)) (*CoordinatedSessionRun, error) {
	if coordinator == nil {
		return nil, fmt.Errorf("session run coordinator is not configured")
	}
	if coordinator.starter == nil {
		return nil, fmt.Errorf("session run starter is not configured")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}

	handle := &CoordinatedSessionRun{
		id:        runID,
		sessionID: sessionID,
		startedAt: coordinator.options.Now().UTC(),
	}

	coordinator.mu.Lock()
	if coordinator.closed {
		coordinator.mu.Unlock()
		return nil, ErrSessionRunCoordinatorClosed
	}
	// Keep the admission slot occupied until await has removed the handle from
	// activeBySession. A run may have already changed its terminal status while
	// its lifecycle goroutine is still settling; admitting here would overlap
	// the old run with a queued durable continuation.
	if active := coordinator.activeBySession[sessionID]; active != nil {
		coordinator.mu.Unlock()
		return nil, ErrSessionBusy
	}
	if len(coordinator.activeBySession) >= coordinator.options.MaxConcurrentRuns {
		coordinator.mu.Unlock()
		return nil, ErrSessionRunCoordinatorCapacity
	}
	if _, exists := coordinator.byID[runID]; exists {
		coordinator.mu.Unlock()
		return nil, fmt.Errorf("generated run id %q is already active", runID)
	}
	forward := func(event SessionStreamEvent) {
		handle.observe(event)
		if coordinator.options.OnRunEvent != nil {
			coordinator.options.OnRunEvent(handle, event)
		}
		if emit != nil {
			emit(event)
		}
	}
	if starter, ok := coordinator.starter.(SessionRunStarterWithID); ok {
		handle.run = starter.StartSessionRunWithID(coordinator.ctx, sessionID, input, runID, forward)
	} else {
		handle.run = coordinator.starter.StartSessionRunWithInput(coordinator.ctx, sessionID, input, forward)
	}
	if handle.run == nil {
		coordinator.mu.Unlock()
		return nil, fmt.Errorf("session run starter returned a nil run")
	}
	coordinator.byID[runID] = handle
	coordinator.activeBySession[sessionID] = handle
	coordinator.mu.Unlock()
	coordinator.notifyRunStarted(handle)

	coordinator.wg.Add(1)
	go coordinator.await(handle)
	return handle, nil
}

func (coordinator *SessionRunCoordinator) await(handle *CoordinatedSessionRun) {
	defer coordinator.wg.Done()
	result, err := handle.Wait()
	coordinator.notifyRunSettled(handle, result, err)
	if coordinator.options.OnRunSettled != nil {
		coordinator.options.OnRunSettled(handle, result, err)
	}

	coordinator.mu.Lock()
	if coordinator.byID[handle.id] == handle {
		delete(coordinator.byID, handle.id)
	}
	if coordinator.activeBySession[handle.sessionID] == handle {
		delete(coordinator.activeBySession, handle.sessionID)
	}
	coordinator.mu.Unlock()
	coordinator.notifyRunIdle(handle)
}

func (coordinator *SessionRunCoordinator) Get(runID string) (*CoordinatedSessionRun, bool) {
	if coordinator == nil {
		return nil, false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.closed {
		return nil, false
	}
	run, ok := coordinator.byID[strings.TrimSpace(runID)]
	return run, ok
}

func (coordinator *SessionRunCoordinator) ActiveForSession(sessionID string) (*CoordinatedSessionRun, bool) {
	if coordinator == nil {
		return nil, false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.closed {
		return nil, false
	}
	run, ok := coordinator.activeBySession[strings.TrimSpace(sessionID)]
	if !ok || run.Status() != SessionRunRunning {
		return nil, false
	}
	return run, ok
}

func (coordinator *SessionRunCoordinator) ActiveRuns() []SessionRunDescriptor {
	if coordinator == nil {
		return []SessionRunDescriptor{}
	}
	coordinator.mu.Lock()
	if coordinator.closed {
		coordinator.mu.Unlock()
		return []SessionRunDescriptor{}
	}
	runs := make([]*CoordinatedSessionRun, 0, len(coordinator.activeBySession))
	for _, run := range coordinator.activeBySession {
		runs = append(runs, run)
	}
	coordinator.mu.Unlock()

	descriptors := make([]SessionRunDescriptor, 0, len(runs))
	for _, run := range runs {
		if descriptor, ok := run.ActiveDescriptor(); ok {
			descriptors = append(descriptors, descriptor)
		}
	}
	sort.Slice(descriptors, func(i, j int) bool {
		if descriptors[i].StartedAt.Equal(descriptors[j].StartedAt) {
			return descriptors[i].RunID < descriptors[j].RunID
		}
		return descriptors[i].StartedAt.Before(descriptors[j].StartedAt)
	})
	return descriptors
}

// Close rejects new runs, cancels all active runs, and waits for their
// lifecycle goroutines to release coordinator entries.
func (coordinator *SessionRunCoordinator) Close() {
	if coordinator == nil {
		return
	}
	coordinator.mu.Lock()
	if coordinator.closed {
		coordinator.mu.Unlock()
		return
	}
	coordinator.closed = true
	runs := make([]*CoordinatedSessionRun, 0, len(coordinator.byID))
	for _, run := range coordinator.byID {
		runs = append(runs, run)
	}
	coordinator.mu.Unlock()

	coordinator.cancel()
	for _, run := range runs {
		run.Cancel()
	}
	coordinator.wg.Wait()
}

func (run *CoordinatedSessionRun) ID() string {
	if run == nil {
		return ""
	}
	return run.id
}

func (run *CoordinatedSessionRun) SessionID() string {
	if run == nil {
		return ""
	}
	return run.sessionID
}

func (run *CoordinatedSessionRun) StartedAt() time.Time {
	if run == nil {
		return time.Time{}
	}
	return run.startedAt
}

func (run *CoordinatedSessionRun) Status() SessionRunStatus {
	if run == nil || run.run == nil {
		return SessionRunFailed
	}
	return run.run.Status()
}

func (run *CoordinatedSessionRun) Wait() (SessionMessageResult, error) {
	if run == nil || run.run == nil {
		return SessionMessageResult{}, fmt.Errorf("session run is not configured")
	}
	return run.run.Wait()
}

// Done is closed when the coordinated run settles. It is suitable for
// context- and timeout-aware waiting without spawning a goroutine around Wait.
func (run *CoordinatedSessionRun) Done() <-chan struct{} {
	if run == nil || run.run == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return run.run.Done()
}

func (run *CoordinatedSessionRun) Cancel() {
	if run != nil && run.run != nil {
		run.run.Cancel()
	}
}

// CancelToolCall cancels a single in-flight tool call without aborting the run.
func (run *CoordinatedSessionRun) CancelToolCall(toolCallID string) bool {
	if run == nil || run.run == nil {
		return false
	}
	return run.run.CancelToolCall(toolCallID)
}

func (run *CoordinatedSessionRun) AppendActive(content string) error {
	if run == nil || run.run == nil {
		return ErrSessionRunSettled
	}
	return run.run.AppendActive(content)
}

func (run *CoordinatedSessionRun) TrySteer(content string) error {
	if run == nil || run.run == nil {
		return ErrSessionNotSteerable
	}
	return run.run.TrySteer(content)
}

func (run *CoordinatedSessionRun) RemoveActive(promptID string) bool {
	return run != nil && run.run != nil && run.run.RemoveActive(promptID)
}

// SetActivePromptSteer marks or unmarks a queued prompt as a steer prompt;
// steer prompts sort ahead of plain queued prompts and drain first.
func (run *CoordinatedSessionRun) SetActivePromptSteer(promptID string, steer bool) bool {
	return run != nil && run.run != nil && run.run.SetActivePromptSteer(promptID, steer)
}

// MoveActivePrompt reorders a queued prompt within its priority group.
func (run *CoordinatedSessionRun) MoveActivePrompt(promptID string, delta int) bool {
	return run != nil && run.run != nil && run.run.MoveActivePrompt(promptID, delta)
}

func (run *CoordinatedSessionRun) Enqueue(event PromptEvent) (*PromptReceipt, error) {
	if run == nil || run.run == nil {
		return nil, ErrSessionRunSettled
	}
	return run.run.Enqueue(event)
}

func (run *CoordinatedSessionRun) ActiveDescriptor() (SessionRunDescriptor, bool) {
	if run == nil || run.Status() != SessionRunRunning {
		return SessionRunDescriptor{}, false
	}
	run.mu.Lock()
	turnID := run.turnID
	run.mu.Unlock()
	return SessionRunDescriptor{
		RunID:     run.id,
		SessionID: run.sessionID,
		TurnID:    turnID,
		StartedAt: run.startedAt,
		Status:    string(SessionRunRunning),
	}, true
}

func (run *CoordinatedSessionRun) observe(event SessionStreamEvent) {
	if run == nil || event == nil {
		return
	}
	if eventType, _ := event["type"].(string); eventType == "turn.started" {
		if turnID, _ := event["turn_id"].(string); strings.TrimSpace(turnID) != "" {
			run.mu.Lock()
			run.turnID = turnID
			run.mu.Unlock()
		}
	}
}

func newSessionRunID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session run id: %w", err)
	}
	return "run-" + hex.EncodeToString(raw), nil
}
