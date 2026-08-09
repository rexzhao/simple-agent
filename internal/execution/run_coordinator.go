package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rexzhao/simple-agent/internal/sessions"
)

const defaultMaxConcurrentSessionRuns = 8

const (
	// Observer delivery is a source-owned boundary. These limits are separate
	// from the presentation/provider queues: a slow observer must not be able
	// to retain an unbounded copy of the execution event stream before its own
	// bounded adapter queue gets a chance to apply recovery semantics.
	defaultRunObserverQueueMessages = 256
	defaultRunObserverQueueBytes    = 2 * 1024 * 1024
	maxRunObserverLossBytes         = 256
	runObserverControlBytes         = 1
)

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

// SessionRunStarterWithIDAdmitted is used only after a durable command-owned
// run row has been committed. It prevents the asynchronous service path from
// attempting to create the same durable run a second time.
type SessionRunStarterWithIDAdmitted interface {
	StartSessionRunWithIDAdmitted(ctx context.Context, sessionID string, input SessionMessageInput, runID string, emit func(SessionStreamEvent)) *SessionRun
}

// DurableRunAdmission is the result of the durable run identity claim. A
// false Created result is an idempotent lookup of an already admitted or
// terminal run; the coordinator must not invoke a model starter for it.
type DurableRunAdmission struct {
	Created bool
	Status  SessionRunStatus
}

// SessionRunDurableAdmitter is the store-backed admission boundary for
// high-risk command starts. Implementations must make the identity claim and
// lifecycle record durable before returning Created=true.
type SessionRunDurableAdmitter interface {
	LookupSessionRun(ctx context.Context, sessionID string, input SessionMessageInput, runID, inputFingerprint string) (DurableRunAdmission, bool, error)
	AdmitSessionRun(ctx context.Context, sessionID string, input SessionMessageInput, runID, inputFingerprint string) (DurableRunAdmission, error)
	FailAdmittedSessionRun(ctx context.Context, sessionID, runID string) error
}

// SessionRunCoordinatorOptions controls application-wide run admission.
type SessionRunCoordinatorOptions struct {
	MaxConcurrentRuns int
	// ObserverQueueMessages and ObserverQueueBytes bound each registered
	// SessionRunEventObserver mailbox. Submission is always non-blocking; an
	// observer which cannot keep up receives one explicit loss notification for
	// each affected run. The mailbox remains alive for later runs after their
	// terminal cleanup has crossed the recovery boundary.
	ObserverQueueMessages int
	ObserverQueueBytes    int
	Now                   func() time.Time
	NewRunID              func() (string, error)
	// DurableAdmitter is intentionally opt-in. Existing REST/agent starts keep
	// their established asynchronous admission path; run.start supplies this
	// adapter and uses StartDurable below.
	DurableAdmitter SessionRunDurableAdmitter
	// OnRunAdmitted is called after the coordinator reserves the run and
	// before the starter is allowed to execute it. Presentation adapters use
	// this phase to register a run-local control owner before the first event
	// can be emitted.
	OnRunAdmitted func(*CoordinatedSessionRun) error
	// OnRunAdmissionFailed removes any adapter state created by OnRunAdmitted
	// when the starter cannot be invoked or returns a nil run.
	OnRunAdmissionFailed func(*CoordinatedSessionRun)
	// OnRunEvent observes every event from every admitted run, including runs
	// started by agent tools. It observes an already-admitted run; adapters
	// must not use this callback to perform first registration.
	OnRunEvent func(*CoordinatedSessionRun, SessionStreamEvent)
	// OnRunSettled observes the stable result after Wait returns and before the
	// run is removed from the coordinator's active indexes.
	OnRunSettled func(*CoordinatedSessionRun, SessionMessageResult, error)
}

// SessionRunEventObserver is the transport-neutral source used by execution
// projections such as the session-content provider. It observes the
// coordinator's single event production path and does not own a second event
// producer.
type SessionRunEventObserver interface {
	RunAdmitted(*CoordinatedSessionRun)
	RunAdmissionFailed(*CoordinatedSessionRun)
	RunEvent(*CoordinatedSessionRun, SessionStreamEvent)
	RunSettled(*CoordinatedSessionRun, SessionMessageResult, error)
}

// SessionRunEventObserverLoss is optional for compatibility with existing
// observers, but is implemented by D2 providers. It is delivered through the
// same observer mailbox after the queued suffix has been discarded, and means
// that the observer must not claim a continuous transient cursor from the loss
// point. If the discarded suffix contained multiple runs, the one bounded
// marker invokes the optional callback once for each affected run. The source
// fences those affected runs until their terminal cleanup, while later runs
// may continue through the same registration.
type SessionRunEventObserverLoss interface {
	RunEventObserverLoss(*CoordinatedSessionRun, string)
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
	if options.ObserverQueueMessages == 0 {
		options.ObserverQueueMessages = defaultRunObserverQueueMessages
	}
	if options.ObserverQueueBytes == 0 {
		options.ObserverQueueBytes = defaultRunObserverQueueBytes
	}
	if options.ObserverQueueMessages < 1 {
		options.ObserverQueueMessages = defaultRunObserverQueueMessages
	}
	if options.ObserverQueueBytes < maxRunObserverLossBytes {
		options.ObserverQueueBytes = defaultRunObserverQueueBytes
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
// projections and agent tools are adapters over this shared owner; they must
// not maintain competing active-run registries.
type SessionRunCoordinator struct {
	ctx     context.Context
	cancel  context.CancelFunc
	starter SessionRunStarter
	options SessionRunCoordinatorOptions

	mu              sync.Mutex
	startMu         sync.Mutex
	closed          bool
	closeDone       chan struct{}
	byID            map[string]*CoordinatedSessionRun
	activeBySession map[string]*CoordinatedSessionRun
	// transitionWG covers the interval after a provisional handle is reserved
	// and before its starter is either running or the handle is removed. It lets
	// Close release startMu while a durable admission callback is in flight,
	// without returning before that transition has finished.
	transitionWG sync.WaitGroup
	wg           sync.WaitGroup

	lifecycleMu  sync.RWMutex
	onRunStarted func(*CoordinatedSessionRun)
	onRunSettled func(*CoordinatedSessionRun, SessionMessageResult, error)
	onRunIdle    func(*CoordinatedSessionRun)
	observerNext uint64
	observers    map[uint64]*runObserverMailbox
	observerStop atomic.Bool
}

// CoordinatedSessionRun is a concurrency-safe handle to one admitted run.
type CoordinatedSessionRun struct {
	id          string
	sessionID   string
	startedAt   time.Time
	fingerprint string
	run         *SessionRun
	// admissionDone is present for durable starts only. It turns the
	// provisional coordinator handle into a single-flight owner: joiners wait
	// for the owner's durable admission/starter result instead of calling the
	// durable store themselves.
	admissionDone chan struct{}
	admissionMu   sync.Mutex
	admission     DurableRunAdmission
	admissionErr  error

	mu     sync.Mutex
	turnID string
}

func (run *CoordinatedSessionRun) completeAdmission(admission DurableRunAdmission, err error) {
	if run == nil || run.admissionDone == nil {
		return
	}
	run.admissionMu.Lock()
	if run.admissionDone != nil {
		run.admission = admission
		run.admissionErr = err
		done := run.admissionDone
		run.admissionDone = nil
		close(done)
	}
	run.admissionMu.Unlock()
}

func (run *CoordinatedSessionRun) waitAdmission() (DurableRunAdmission, error) {
	if run == nil {
		return DurableRunAdmission{}, fmt.Errorf("session run is not configured")
	}
	run.admissionMu.Lock()
	done := run.admissionDone
	if done == nil {
		admission, err := run.admission, run.admissionErr
		run.admissionMu.Unlock()
		return admission, err
	}
	run.admissionMu.Unlock()
	<-done
	run.admissionMu.Lock()
	defer run.admissionMu.Unlock()
	return run.admission, run.admissionErr
}

func (run *CoordinatedSessionRun) setSessionRun(sessionRun *SessionRun) {
	if run == nil {
		return
	}
	run.mu.Lock()
	run.run = sessionRun
	run.mu.Unlock()
}

func (run *CoordinatedSessionRun) sessionRun() *SessionRun {
	if run == nil {
		return nil
	}
	run.mu.Lock()
	sessionRun := run.run
	run.mu.Unlock()
	return sessionRun
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
		closeDone:       make(chan struct{}),
		byID:            make(map[string]*CoordinatedSessionRun),
		activeBySession: make(map[string]*CoordinatedSessionRun),
		observers:       make(map[uint64]*runObserverMailbox),
	}
}

// RegisterRunEventObserver attaches a source-owned bounded mailbox to the
// shared coordinator event source. The producer only snapshots/clones the
// event and performs a non-blocking mailbox submission; it never invokes
// observer code inline. A single mailbox has one delivery goroutine, so its
// admitted -> events -> settled order is FIFO. The returned function is
// idempotent and waits for the mailbox to finish, which proves that no
// callback remains in flight after an ordinary unregister.
func (coordinator *SessionRunCoordinator) RegisterRunEventObserver(observer SessionRunEventObserver) func() {
	if coordinator == nil || observer == nil || coordinator.observerStop.Load() {
		return func() {}
	}
	mailbox := newRunObserverMailbox(observer, coordinator.options.ObserverQueueMessages, coordinator.options.ObserverQueueBytes)
	coordinator.lifecycleMu.Lock()
	if coordinator.observerStop.Load() {
		coordinator.lifecycleMu.Unlock()
		mailbox.stop()
		return func() {}
	}
	coordinator.observerNext++
	id := coordinator.observerNext
	coordinator.observers[id] = mailbox
	coordinator.lifecycleMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			coordinator.lifecycleMu.Lock()
			delete(coordinator.observers, id)
			coordinator.lifecycleMu.Unlock()
			mailbox.stop()
		})
	}
}

func (coordinator *SessionRunCoordinator) runObservers() []*runObserverMailbox {
	coordinator.lifecycleMu.RLock()
	defer coordinator.lifecycleMu.RUnlock()
	observers := make([]*runObserverMailbox, 0, len(coordinator.observers))
	for _, observer := range coordinator.observers {
		observers = append(observers, observer)
	}
	return observers
}

func (coordinator *SessionRunCoordinator) notifyRunAdmittedObservers(run *CoordinatedSessionRun) {
	for _, observer := range coordinator.runObservers() {
		observer.enqueue(runObserverDelivery{kind: runObserverAdmitted, run: run})
	}
}

func (coordinator *SessionRunCoordinator) notifyRunAdmissionFailedObservers(run *CoordinatedSessionRun) {
	for _, observer := range coordinator.runObservers() {
		observer.enqueue(runObserverDelivery{kind: runObserverAdmissionFailed, run: run})
	}
}

func (coordinator *SessionRunCoordinator) notifyRunEventObservers(run *CoordinatedSessionRun, event SessionStreamEvent) {
	for _, observer := range coordinator.runObservers() {
		observer.enqueue(runObserverDelivery{kind: runObserverEvent, run: run, event: event})
	}
}

func (coordinator *SessionRunCoordinator) notifyRunSettledObservers(run *CoordinatedSessionRun, result SessionMessageResult, err error) {
	for _, observer := range coordinator.runObservers() {
		observer.enqueue(runObserverDelivery{kind: runObserverSettled, run: run, result: result, err: err})
	}
}

type runObserverDeliveryKind uint8

const (
	runObserverAdmitted runObserverDeliveryKind = iota
	runObserverAdmissionFailed
	runObserverEvent
	runObserverSettled
	runObserverLoss
)

type runObserverDelivery struct {
	kind   runObserverDeliveryKind
	run    *CoordinatedSessionRun
	runs   []*CoordinatedSessionRun // populated for a loss covering a discarded suffix
	event  SessionStreamEvent
	result SessionMessageResult
	err    error
	reason string
	bytes  int
}

type runObserverMailbox struct {
	observer    SessionRunEventObserver
	maxMessages int
	maxBytes    int
	queue       chan runObserverDelivery
	stopCh      chan struct{}
	done        chan struct{}

	mu          sync.Mutex
	queuedBytes int
	closed      bool
	// activeRuns covers the source-owned mailbox's whole admitted-but-not-yet
	// settled set, not only the entries currently sitting in queue. Events for
	// an active run which happened to be in flight (rather than queued) would
	// otherwise be dropped silently after the marker and that run would never
	// receive recovery.
	activeRuns map[*CoordinatedSessionRun]struct{}
	// poisonedRuns is the run-local recovery fence installed by an overflow.
	// Normal lifecycle/event callbacks for a poisoned run are rejected until
	// its terminal lifecycle callback is delivered; a later run can therefore
	// reuse this mailbox.
	poisonedRuns map[*CoordinatedSessionRun]struct{}
	// pendingTerminals retains at most one compact terminal delivery per run
	// while its loss marker is in flight. It is deliberately a sidecar to the
	// normal queue: loss is delivered first, then these bounded cleanups are
	// delivered before the worker resumes ordinary traffic.
	pendingTerminals map[*CoordinatedSessionRun]runObserverDelivery
	lossPending      bool
	stopOnce         sync.Once
}

func newRunObserverMailbox(observer SessionRunEventObserver, maxMessages, maxBytes int) *runObserverMailbox {
	if maxMessages <= 0 {
		maxMessages = defaultRunObserverQueueMessages
	}
	if maxBytes < maxRunObserverLossBytes {
		maxBytes = defaultRunObserverQueueBytes
	}
	mailbox := &runObserverMailbox{
		observer:         observer,
		maxMessages:      maxMessages,
		maxBytes:         maxBytes,
		queue:            make(chan runObserverDelivery, maxMessages),
		stopCh:           make(chan struct{}),
		done:             make(chan struct{}),
		activeRuns:       make(map[*CoordinatedSessionRun]struct{}),
		poisonedRuns:     make(map[*CoordinatedSessionRun]struct{}),
		pendingTerminals: make(map[*CoordinatedSessionRun]runObserverDelivery),
	}
	go mailbox.run()
	return mailbox
}

// enqueue is deliberately a try-send under the mailbox mutex. The mutex
// protects the closed/send race without ever being held across observer code;
// the channel is never closed, so there is no send-on-closed panic. A failed
// submission discards the pending mailbox contents and queues exactly one
// loss marker. The marker is the terminal recovery boundary for affected
// runs, not for the observer registration: after it and affected terminal
// cleanup are delivered, later runs may use the same bounded mailbox.
func (mailbox *runObserverMailbox) enqueue(delivery runObserverDelivery) {
	if mailbox == nil {
		return
	}
	delivery = mailbox.cloneDelivery(delivery)
	if delivery.bytes <= 0 {
		delivery.bytes = 128
	}
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.closed {
		return
	}
	if delivery.kind != runObserverSettled && delivery.kind != runObserverAdmissionFailed {
		if _, poisoned := mailbox.poisonedRuns[delivery.run]; poisoned {
			return
		}
	}
	if isRunObserverTerminal(delivery.kind) && mailbox.lossPending {
		mailbox.rememberTerminalLocked(delivery)
		return
	}
	mailbox.trackRunLocked(delivery)
	if delivery.bytes > mailbox.maxBytes || len(mailbox.queue) >= mailbox.maxMessages || mailbox.queuedBytes > mailbox.maxBytes-delivery.bytes {
		mailbox.overflowLocked(delivery.run, delivery)
		return
	}
	mailbox.queue <- delivery
	mailbox.queuedBytes += delivery.bytes
}

func (mailbox *runObserverMailbox) trackRunLocked(delivery runObserverDelivery) {
	if delivery.run == nil {
		return
	}
	// Keep terminal deliveries in the set until their callback is actually
	// delivered. If one is still queued when overflow happens, the loss marker
	// must include that run as well.
	mailbox.activeRuns[delivery.run] = struct{}{}
}

func (mailbox *runObserverMailbox) cloneDelivery(delivery runObserverDelivery) runObserverDelivery {
	if delivery.kind != runObserverEvent || delivery.event == nil {
		delivery.bytes = observerDeliveryBytes(delivery)
		return delivery
	}
	// SessionStreamEvent is a mutable map with potentially mutable nested
	// values. JSON round-tripping at this boundary creates an owned snapshot;
	// the serialized size is also the conservative retained-byte accounting
	// unit. This is intentionally before enqueue returns, so a producer may
	// safely reuse/mutate its original map after RunEvent returns.
	raw, err := json.Marshal(delivery.event)
	if err != nil {
		delivery.event = nil
		delivery.bytes = mailbox.maxBytes + 1
		return delivery
	}
	var snapshot map[string]any
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		delivery.event = nil
		delivery.bytes = mailbox.maxBytes + 1
		return delivery
	}
	delivery.event = SessionStreamEvent(snapshot)
	delivery.bytes = len(raw) + 256
	return delivery
}

func observerDeliveryBytes(delivery runObserverDelivery) int {
	bytes := 256
	if delivery.run != nil {
		bytes += len(delivery.run.ID()) + len(delivery.run.SessionID())
	}
	if delivery.event != nil {
		if raw, err := json.Marshal(delivery.event); err == nil {
			bytes += len(raw)
		} else {
			bytes += defaultRunObserverQueueBytes + 1
		}
	}
	if delivery.err != nil {
		bytes += len(delivery.err.Error())
	}
	if delivery.result.TurnID != "" || delivery.result.Status != "" || delivery.result.RunID != "" {
		if raw, err := json.Marshal(delivery.result); err == nil {
			bytes += len(raw)
		} else {
			bytes += defaultRunObserverQueueBytes + 1
		}
	}
	bytes += len(delivery.reason)
	return bytes
}

func isRunObserverTerminal(kind runObserverDeliveryKind) bool {
	return kind == runObserverSettled || kind == runObserverAdmissionFailed
}

func compactRunObserverTerminal(delivery runObserverDelivery) runObserverDelivery {
	// Recovery has already been signalled separately. Keep only the stable run
	// handle and the small result identity needed by cleanup; do not retain a
	// potentially unbounded error string in the sidecar.
	delivery.err = nil
	delivery.reason = ""
	delivery.bytes = runObserverControlBytes
	return delivery
}

func (mailbox *runObserverMailbox) rememberTerminalLocked(delivery runObserverDelivery) {
	if mailbox == nil || delivery.run == nil || !isRunObserverTerminal(delivery.kind) {
		return
	}
	if _, exists := mailbox.pendingTerminals[delivery.run]; exists {
		return
	}
	mailbox.pendingTerminals[delivery.run] = compactRunObserverTerminal(delivery)
}

func (mailbox *runObserverMailbox) overflowLocked(run *CoordinatedSessionRun, triggering runObserverDelivery) {
	if mailbox.closed {
		return
	}
	mailbox.lossPending = true
	runs := make([]*CoordinatedSessionRun, 0, len(mailbox.queue)+1)
	seen := make(map[*CoordinatedSessionRun]struct{}, len(mailbox.queue)+1)
	addRun := func(candidate *CoordinatedSessionRun) {
		if candidate == nil {
			return
		}
		if _, exists := seen[candidate]; exists {
			return
		}
		seen[candidate] = struct{}{}
		runs = append(runs, candidate)
	}
	for candidate := range mailbox.activeRuns {
		addRun(candidate)
	}
	for {
		select {
		case pending := <-mailbox.queue:
			addRun(pending.run)
			mailbox.queuedBytes -= pending.bytes
			if isRunObserverTerminal(pending.kind) {
				mailbox.rememberTerminalLocked(pending)
			}
		default:
			mailbox.queuedBytes = 0
			addRun(run)
			if isRunObserverTerminal(triggering.kind) {
				mailbox.rememberTerminalLocked(triggering)
			}
			for _, affected := range runs {
				mailbox.poisonedRuns[affected] = struct{}{}
			}
			loss := runObserverDelivery{kind: runObserverLoss, run: run, runs: runs, reason: "observer mailbox overflow", bytes: maxRunObserverLossBytes}
			mailbox.queue <- loss
			mailbox.queuedBytes = loss.bytes
			return
		}
	}
}

func (mailbox *runObserverMailbox) stop() {
	if mailbox == nil {
		return
	}
	mailbox.mu.Lock()
	if !mailbox.closed {
		mailbox.closed = true
		for {
			select {
			case pending := <-mailbox.queue:
				mailbox.queuedBytes -= pending.bytes
			default:
				mailbox.queuedBytes = 0
				clear(mailbox.activeRuns)
				clear(mailbox.poisonedRuns)
				clear(mailbox.pendingTerminals)
				mailbox.lossPending = false
				goto drained
			}
		}
	}
drained:
	mailbox.mu.Unlock()
	mailbox.stopOnce.Do(func() { close(mailbox.stopCh) })
	<-mailbox.done
}

func (mailbox *runObserverMailbox) run() {
	defer close(mailbox.done)
	for {
		select {
		case delivery := <-mailbox.queue:
			mailbox.mu.Lock()
			if mailbox.queuedBytes >= delivery.bytes {
				mailbox.queuedBytes -= delivery.bytes
			} else {
				mailbox.queuedBytes = 0
			}
			mailbox.mu.Unlock()
			mailbox.deliver(delivery)
			if isRunObserverTerminal(delivery.kind) {
				mailbox.completeTerminal(delivery)
			}
			if delivery.kind == runObserverLoss {
				// The loss marker is a run-local recovery boundary, not a
				// registration-lifetime boundary. Deliver terminal cleanup only
				// after recovery is visible, then keep this worker alive so a
				// subsequent admitted run can use the same registration.
				mailbox.deliverPendingTerminals()
			}
		case <-mailbox.stopCh:
			return
		}
	}
}

func (mailbox *runObserverMailbox) deliverPendingTerminals() {
	mailbox.mu.Lock()
	terminals := make([]runObserverDelivery, 0, len(mailbox.pendingTerminals))
	for _, terminal := range mailbox.pendingTerminals {
		terminals = append(terminals, terminal)
	}
	clear(mailbox.pendingTerminals)
	mailbox.lossPending = false
	mailbox.mu.Unlock()
	for _, terminal := range terminals {
		mailbox.deliver(terminal)
		mailbox.completeTerminal(terminal)
	}
}

func (mailbox *runObserverMailbox) completeTerminal(delivery runObserverDelivery) {
	if mailbox == nil || !isRunObserverTerminal(delivery.kind) {
		return
	}
	mailbox.mu.Lock()
	delete(mailbox.activeRuns, delivery.run)
	delete(mailbox.poisonedRuns, delivery.run)
	mailbox.mu.Unlock()
}

func (mailbox *runObserverMailbox) deliver(delivery runObserverDelivery) {
	if mailbox == nil || mailbox.observer == nil {
		return
	}
	switch delivery.kind {
	case runObserverAdmitted:
		mailbox.observer.RunAdmitted(delivery.run)
	case runObserverAdmissionFailed:
		mailbox.observer.RunAdmissionFailed(delivery.run)
	case runObserverEvent:
		mailbox.observer.RunEvent(delivery.run, delivery.event)
	case runObserverSettled:
		mailbox.observer.RunSettled(delivery.run, delivery.result, delivery.err)
	case runObserverLoss:
		if lossObserver, ok := mailbox.observer.(SessionRunEventObserverLoss); ok {
			if len(delivery.runs) == 0 {
				lossObserver.RunEventObserverLoss(delivery.run, delivery.reason)
				return
			}
			for _, run := range delivery.runs {
				lossObserver.RunEventObserverLoss(run, delivery.reason)
			}
		}
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
// cannot replace the existing lifecycle callbacks (or the presentation
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

func (coordinator *SessionRunCoordinator) notifyRunAdmissionFailed(run *CoordinatedSessionRun) {
	if coordinator == nil || coordinator.options.OnRunAdmissionFailed == nil {
		return
	}
	coordinator.options.OnRunAdmissionFailed(run)
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

// StartDurable reserves the normal in-memory session slot, then commits the
// durable identity/lifecycle admission before invoking the execution starter.
// This ordering leaves no business-start window in which a retry can run a
// second model request without seeing the original durable claim.
func (coordinator *SessionRunCoordinator) StartDurable(sessionID string, input SessionMessageInput, runID, inputFingerprint string, emit func(SessionStreamEvent)) (*CoordinatedSessionRun, DurableRunAdmission, error) {
	if coordinator == nil {
		return nil, DurableRunAdmission{}, fmt.Errorf("session run coordinator is not configured")
	}
	if coordinator.options.DurableAdmitter == nil {
		return nil, DurableRunAdmission{}, fmt.Errorf("durable run admission is not configured")
	}
	if strings.TrimSpace(runID) == "" {
		return nil, DurableRunAdmission{}, fmt.Errorf("run id is required")
	}
	if _, ok := coordinator.starter.(SessionRunStarterWithIDAdmitted); !ok {
		return nil, DurableRunAdmission{}, fmt.Errorf("session run starter does not support durable admitted run ids")
	}
	// Resolve an already durable identity before applying process-local
	// capacity/busy gates. A retry of a settled run must return its status even
	// when unrelated work currently fills the coordinator.
	admission, found, err := coordinator.options.DurableAdmitter.LookupSessionRun(coordinator.ctx, strings.TrimSpace(sessionID), input, strings.TrimSpace(runID), inputFingerprint)
	if err != nil {
		return nil, DurableRunAdmission{}, err
	}
	if found {
		if active, ok := coordinator.Get(runID); ok && active.SessionID() == strings.TrimSpace(sessionID) {
			return active, admission, nil
		}
		return nil, admission, nil
	}
	return coordinator.startWithIDDurable(sessionID, input, runID, inputFingerprint, emit)
}

func (coordinator *SessionRunCoordinator) startWithID(sessionID string, input SessionMessageInput, runID string, emit func(SessionStreamEvent)) (*CoordinatedSessionRun, error) {
	run, _, err := coordinator.startWithIDInternal(sessionID, input, runID, "", emit, false)
	return run, err
}

func (coordinator *SessionRunCoordinator) startWithIDDurable(sessionID string, input SessionMessageInput, runID, inputFingerprint string, emit func(SessionStreamEvent)) (*CoordinatedSessionRun, DurableRunAdmission, error) {
	return coordinator.startWithIDInternal(sessionID, input, runID, inputFingerprint, emit, true)
}

func (coordinator *SessionRunCoordinator) startWithIDInternal(sessionID string, input SessionMessageInput, runID, inputFingerprint string, emit func(SessionStreamEvent), durable bool) (*CoordinatedSessionRun, DurableRunAdmission, error) {
	if coordinator == nil {
		return nil, DurableRunAdmission{}, fmt.Errorf("session run coordinator is not configured")
	}
	if coordinator.starter == nil {
		return nil, DurableRunAdmission{}, fmt.Errorf("session run starter is not configured")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, DurableRunAdmission{}, fmt.Errorf("session id is required")
	}

	handle := &CoordinatedSessionRun{
		id:          runID,
		sessionID:   sessionID,
		startedAt:   coordinator.options.Now().UTC(),
		fingerprint: inputFingerprint,
	}
	if durable {
		handle.admissionDone = make(chan struct{})
	}

	// Serialize only the provisional reservation. The coordinator mutex is
	// deliberately released while admission callbacks and the starter run so
	// a same-identity caller can join the provisional owner instead of invoking
	// the durable admitter a second time. Close uses transitionWG to wait for
	// these callbacks after cancelling the coordinator context.
	coordinator.startMu.Lock()
	coordinator.mu.Lock()
	if coordinator.closed {
		coordinator.mu.Unlock()
		coordinator.startMu.Unlock()
		return nil, DurableRunAdmission{}, ErrSessionRunCoordinatorClosed
	}
	// Keep the admission slot occupied until await has removed the handle from
	// activeBySession. A run may have already changed its terminal status while
	// its lifecycle goroutine is still settling; admitting here would overlap
	// the old run with a queued durable continuation.
	if active := coordinator.activeBySession[sessionID]; active != nil {
		if durable && active.ID() == runID {
			if active.fingerprint != inputFingerprint {
				coordinator.mu.Unlock()
				coordinator.startMu.Unlock()
				return nil, DurableRunAdmission{}, fmt.Errorf("%w: run %q", sessions.ErrIdempotencyConflict, runID)
			}
			coordinator.mu.Unlock()
			coordinator.startMu.Unlock()
			admission, err := active.waitAdmission()
			if err != nil {
				return nil, DurableRunAdmission{}, err
			}
			return active, DurableRunAdmission{Created: false, Status: admission.Status}, nil
		}
		coordinator.mu.Unlock()
		coordinator.startMu.Unlock()
		return nil, DurableRunAdmission{}, ErrSessionBusy
	}
	if len(coordinator.activeBySession) >= coordinator.options.MaxConcurrentRuns {
		coordinator.mu.Unlock()
		coordinator.startMu.Unlock()
		return nil, DurableRunAdmission{}, ErrSessionRunCoordinatorCapacity
	}
	if _, exists := coordinator.byID[runID]; exists {
		existing := coordinator.byID[runID]
		coordinator.mu.Unlock()
		if durable {
			if existing.SessionID() != sessionID || existing.fingerprint != inputFingerprint {
				coordinator.startMu.Unlock()
				return nil, DurableRunAdmission{}, fmt.Errorf("%w: run %q", sessions.ErrIdempotencyConflict, runID)
			}
			coordinator.startMu.Unlock()
			admission, err := existing.waitAdmission()
			if err != nil {
				return nil, DurableRunAdmission{}, err
			}
			return existing, DurableRunAdmission{Created: false, Status: admission.Status}, nil
		}
		coordinator.startMu.Unlock()
		return nil, DurableRunAdmission{}, fmt.Errorf("generated run id %q is already active", runID)
	}
	// Reserve the coordinator indexes before invoking either the adapter
	// admission callback or the execution starter. This makes the handle
	// discoverable during the entire admission transition.
	coordinator.byID[runID] = handle
	coordinator.activeBySession[sessionID] = handle
	coordinator.transitionWG.Add(1)
	coordinator.mu.Unlock()
	coordinator.startMu.Unlock()
	defer coordinator.transitionWG.Done()

	admission := DurableRunAdmission{Created: true, Status: SessionRunRunning}
	if durable {
		var err error
		admission, err = coordinator.options.DurableAdmitter.AdmitSessionRun(coordinator.ctx, sessionID, input, runID, inputFingerprint)
		if err != nil {
			handle.completeAdmission(DurableRunAdmission{}, err)
			coordinator.removeAdmittedRun(handle)
			coordinator.notifyRunAdmissionFailed(handle)
			coordinator.notifyRunAdmissionFailedObservers(handle)
			return nil, DurableRunAdmission{}, err
		}
		if !admission.Created {
			handle.completeAdmission(admission, nil)
			coordinator.removeAdmittedRun(handle)
			return nil, admission, nil
		}
	}
	if coordinator.options.OnRunAdmitted != nil {
		if err := coordinator.options.OnRunAdmitted(handle); err != nil {
			if durable {
				if failErr := coordinator.options.DurableAdmitter.FailAdmittedSessionRun(coordinator.ctx, sessionID, runID); failErr == nil {
					// The durable identity now has an authoritative failed outcome.
					// Return that compact result to both the owner and joiners rather
					// than exposing a presentation-adapter error which would hide the
					// already-admitted identity from the command caller.
					handle.completeAdmission(DurableRunAdmission{Created: false, Status: SessionRunFailed}, nil)
					coordinator.removeAdmittedRun(handle)
					coordinator.notifyRunAdmissionFailed(handle)
					coordinator.notifyRunAdmissionFailedObservers(handle)
					return nil, DurableRunAdmission{Created: false, Status: SessionRunFailed}, nil
				}
				handle.completeAdmission(DurableRunAdmission{Created: false, Status: SessionRunFailed}, err)
			}
			coordinator.removeAdmittedRun(handle)
			coordinator.notifyRunAdmissionFailed(handle)
			coordinator.notifyRunAdmissionFailedObservers(handle)
			return nil, DurableRunAdmission{}, err
		}
	}
	coordinator.notifyRunAdmittedObservers(handle)

	forward := func(event SessionStreamEvent) {
		handle.observe(event)
		if coordinator.options.OnRunEvent != nil {
			coordinator.options.OnRunEvent(handle, event)
		}
		coordinator.notifyRunEventObservers(handle, event)
		if emit != nil {
			emit(event)
		}
	}
	if durable {
		starter := coordinator.starter.(SessionRunStarterWithIDAdmitted)
		handle.setSessionRun(starter.StartSessionRunWithIDAdmitted(coordinator.ctx, sessionID, input, runID, forward))
	} else if starter, ok := coordinator.starter.(SessionRunStarterWithID); ok {
		handle.setSessionRun(starter.StartSessionRunWithID(coordinator.ctx, sessionID, input, runID, forward))
	} else {
		handle.setSessionRun(coordinator.starter.StartSessionRunWithInput(coordinator.ctx, sessionID, input, forward))
	}
	if handle.sessionRun() == nil {
		if durable {
			starterErr := fmt.Errorf("session run starter returned a nil run")
			if failErr := coordinator.options.DurableAdmitter.FailAdmittedSessionRun(coordinator.ctx, sessionID, runID); failErr == nil {
				handle.completeAdmission(DurableRunAdmission{Created: false, Status: SessionRunFailed}, nil)
				coordinator.removeAdmittedRun(handle)
				coordinator.notifyRunAdmissionFailed(handle)
				coordinator.notifyRunAdmissionFailedObservers(handle)
				return nil, DurableRunAdmission{Created: false, Status: SessionRunFailed}, nil
			}
			handle.completeAdmission(DurableRunAdmission{Created: false, Status: SessionRunFailed}, starterErr)
		}
		coordinator.removeAdmittedRun(handle)
		coordinator.notifyRunAdmissionFailed(handle)
		coordinator.notifyRunAdmissionFailedObservers(handle)
		return nil, admission, fmt.Errorf("session run starter returned a nil run")
	}
	handle.completeAdmission(admission, nil)
	coordinator.notifyRunStarted(handle)

	coordinator.wg.Add(1)
	go coordinator.await(handle)
	return handle, admission, nil
}

func (coordinator *SessionRunCoordinator) removeAdmittedRun(handle *CoordinatedSessionRun) {
	if coordinator == nil || handle == nil {
		return
	}
	coordinator.mu.Lock()
	if coordinator.byID[handle.id] == handle {
		delete(coordinator.byID, handle.id)
	}
	if coordinator.activeBySession[handle.sessionID] == handle {
		delete(coordinator.activeBySession, handle.sessionID)
	}
	coordinator.mu.Unlock()
}

func (coordinator *SessionRunCoordinator) await(handle *CoordinatedSessionRun) {
	defer coordinator.wg.Done()
	result, err := handle.Wait()
	coordinator.notifyRunSettled(handle, result, err)
	coordinator.notifyRunSettledObservers(handle, result, err)
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

// Close rejects new runs and cancels the coordinator context before waiting
// for an in-flight admission/starter transition. This ordering is important:
// a starter may synchronously wait for ctx.Done() before returning. Once that
// transition has completed, Close cancels any returned run handles and waits
// for their lifecycle goroutines to release coordinator entries.
func (coordinator *SessionRunCoordinator) Close() {
	if coordinator == nil {
		return
	}
	coordinator.mu.Lock()
	if coordinator.closed {
		closeDone := coordinator.closeDone
		coordinator.mu.Unlock()
		if closeDone != nil {
			<-closeDone
		}
		return
	}
	coordinator.closed = true
	coordinator.mu.Unlock()
	coordinator.observerStop.Store(true)

	// Cancel before taking startMu. The starter is invoked while startMu is
	// held only for the reservation and may be waiting on this context after
	// that reservation, so taking startMu first would deadlock Close against a
	// synchronous reservation path.
	coordinator.cancel()
	coordinator.startMu.Lock()
	coordinator.startMu.Unlock()
	// No new transition can be added after startMu has been observed here;
	// existing transitions may still be inside the durable admitter/starter.
	coordinator.transitionWG.Wait()
	coordinator.mu.Lock()
	runs := make([]*CoordinatedSessionRun, 0, len(coordinator.byID))
	for _, run := range coordinator.byID {
		runs = append(runs, run)
	}
	coordinator.mu.Unlock()

	for _, run := range runs {
		run.Cancel()
	}
	coordinator.wg.Wait()
	coordinator.lifecycleMu.Lock()
	observers := make([]*runObserverMailbox, 0, len(coordinator.observers))
	for id, observer := range coordinator.observers {
		observers = append(observers, observer)
		delete(coordinator.observers, id)
	}
	coordinator.lifecycleMu.Unlock()
	for _, observer := range observers {
		observer.stop()
	}
	if coordinator.closeDone != nil {
		close(coordinator.closeDone)
	}
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
	if run == nil {
		return SessionRunFailed
	}
	sessionRun := run.sessionRun()
	if sessionRun == nil {
		// A handle is visible to observers during the short admission phase,
		// before the starter has returned its SessionRun. It is already a
		// reserved running slot, not a failed execution.
		return SessionRunRunning
	}
	return sessionRun.Status()
}

func (run *CoordinatedSessionRun) Wait() (SessionMessageResult, error) {
	if run == nil {
		return SessionMessageResult{}, fmt.Errorf("session run is not configured")
	}
	sessionRun := run.sessionRun()
	if sessionRun == nil {
		return SessionMessageResult{}, fmt.Errorf("session run is not configured")
	}
	return sessionRun.Wait()
}

// Done is closed when the coordinated run settles. It is suitable for
// context- and timeout-aware waiting without spawning a goroutine around Wait.
func (run *CoordinatedSessionRun) Done() <-chan struct{} {
	if run == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	sessionRun := run.sessionRun()
	if sessionRun == nil {
		return nil
	}
	return sessionRun.Done()
}

func (run *CoordinatedSessionRun) Cancel() {
	if sessionRun := run.sessionRun(); sessionRun != nil {
		sessionRun.Cancel()
	}
}

// CancelToolCall cancels a single in-flight tool call without aborting the run.
func (run *CoordinatedSessionRun) CancelToolCall(toolCallID string) bool {
	sessionRun := run.sessionRun()
	if sessionRun == nil {
		return false
	}
	return sessionRun.CancelToolCall(toolCallID)
}

func (run *CoordinatedSessionRun) AppendActive(content string) error {
	sessionRun := run.sessionRun()
	if sessionRun == nil {
		return ErrSessionRunSettled
	}
	return sessionRun.AppendActive(content)
}

// ActivePromptReady reports whether the starter has installed a SessionRun
// and that run still accepts active prompts. A provisional durable handle is
// intentionally not ready: its Status is running for coordinator capacity,
// but there is no queue owner to mutate until the starter returns.
func (run *CoordinatedSessionRun) ActivePromptReady() bool {
	if run == nil {
		return false
	}
	sessionRun := run.sessionRun()
	return sessionRun != nil && sessionRun.acceptsActivePrompt()
}

// ActiveControlReady reports whether the coordinator has installed the
// process-local SessionRun owner and it is still running. Unlike
// ActivePromptReady it does not require the enqueue/follow-up acceptance gate;
// an in-flight tool may still be cancellable while that gate is closing.
func (run *CoordinatedSessionRun) ActiveControlReady() bool {
	if run == nil {
		return false
	}
	sessionRun := run.sessionRun()
	return sessionRun != nil && sessionRun.Status() == SessionRunRunning
}

func (run *CoordinatedSessionRun) TrySteer(content string) error {
	sessionRun := run.sessionRun()
	if sessionRun == nil {
		return ErrSessionNotSteerable
	}
	return sessionRun.TrySteer(content)
}

func (run *CoordinatedSessionRun) RemoveActive(promptID string) bool {
	sessionRun := run.sessionRun()
	return sessionRun != nil && sessionRun.RemoveActive(promptID)
}

// SetActivePromptSteer marks or unmarks a queued prompt as a steer prompt;
// steer prompts sort ahead of plain queued prompts and drain first.
func (run *CoordinatedSessionRun) SetActivePromptSteer(promptID string, steer bool) bool {
	sessionRun := run.sessionRun()
	return sessionRun != nil && sessionRun.SetActivePromptSteer(promptID, steer)
}

// MoveActivePrompt reorders a queued prompt within its priority group.
func (run *CoordinatedSessionRun) MoveActivePrompt(promptID string, delta int) bool {
	found, _ := run.MoveActivePromptResult(promptID, delta)
	return found
}

// MoveActivePromptResult delegates the richer queue-owner result without
// moving queue policy into the coordinator or a presentation adapter.
func (run *CoordinatedSessionRun) MoveActivePromptResult(promptID string, delta int) (found, moved bool) {
	sessionRun := run.sessionRun()
	if sessionRun == nil {
		return false, false
	}
	return sessionRun.MoveActivePromptResult(promptID, delta)
}

func (run *CoordinatedSessionRun) Enqueue(event PromptEvent) (*PromptReceipt, error) {
	sessionRun := run.sessionRun()
	if sessionRun == nil {
		return nil, ErrSessionRunSettled
	}
	return sessionRun.Enqueue(event)
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
