package sessioncontent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

const (
	DefaultJournalEntries         = 4096
	DefaultJournalBytes           = 8 * 1024 * 1024
	DefaultLiveCapacity           = 256
	DefaultProjectorQueue         = 256
	DefaultInlineSnapshot         = 64 * 1024
	DefaultHistoryLimit           = 50
	DefaultMaxCompactionRecords   = 64
	DefaultMaxItemContentBytes    = 64 * 1024
	DefaultMaxItemBlobs           = 256
	DefaultMaxOwners              = 1024
	DefaultBlobRefreshSkew        = time.Minute
	DefaultTransientReplayEntries = 2048
	DefaultTransientReplayBytes   = 4 * 1024 * 1024
	DefaultTransientLiveCapacity  = 256
	DefaultTransientLiveBytes     = 1 * 1024 * 1024
	DefaultTransientQueueCapacity = 256
	DefaultTransientQueueBytes    = 2 * 1024 * 1024
)

var (
	ErrProviderClosed  = errors.New("session content provider is closed")
	ErrProviderInvalid = errors.New("session content provider requires resync")
	ErrQueueFull       = errors.New("session content projector queue is full")
	ErrSnapshotRace    = errors.New("durable session changed while snapshot was read")
)

type BlobWriter interface {
	Put(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error)
}

// FreshBlobWriter is an optional extension used when a cached descriptor is
// near expiry. Put is allowed to deduplicate and return an existing
// descriptor; PutFresh guarantees a new immutable descriptor instead. A
// writer which does not implement this extension must cause projection to
// fail rather than put an expired descriptor on the wire.
type FreshBlobWriter interface {
	PutFresh(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error)
}

type ProviderOptions struct {
	JournalEntries         int
	JournalBytes           int
	LiveCapacity           int
	ProjectorQueueCapacity int
	InlineSnapshotBytes    int
	HistoryLimit           int
	MaxCompactionRecords   int
	MaxItemContentBytes    int
	MaxItemBlobs           int
	MaxOwners              int
	MaxChangeMessageBytes  int
	TransientReplayEntries int
	TransientReplayBytes   int
	TransientLiveCapacity  int
	TransientLiveBytes     int
	TransientQueueCapacity int
	TransientQueueBytes    int
	BlobRefreshSkew        time.Duration
	StreamEpoch            string
	OwnerContext           context.Context
	BlobWriter             BlobWriter
	Now                    func() time.Time
	// BeforeSnapshot is a deterministic test hook. It runs after the first
	// compact state read and before the bounded history read; production callers
	// leave it nil.
	BeforeSnapshot func(string)
}

func (o ProviderOptions) withDefaults() (ProviderOptions, error) {
	if o.JournalEntries == 0 {
		o.JournalEntries = DefaultJournalEntries
	}
	if o.JournalBytes == 0 {
		o.JournalBytes = DefaultJournalBytes
	}
	if o.LiveCapacity == 0 {
		o.LiveCapacity = DefaultLiveCapacity
	}
	if o.ProjectorQueueCapacity == 0 {
		o.ProjectorQueueCapacity = DefaultProjectorQueue
	}
	if o.InlineSnapshotBytes == 0 {
		o.InlineSnapshotBytes = DefaultInlineSnapshot
	}
	if o.HistoryLimit == 0 {
		o.HistoryLimit = DefaultHistoryLimit
	}
	if o.MaxCompactionRecords == 0 {
		o.MaxCompactionRecords = DefaultMaxCompactionRecords
	}
	if o.MaxItemContentBytes == 0 {
		o.MaxItemContentBytes = DefaultMaxItemContentBytes
	}
	if o.MaxItemBlobs == 0 {
		o.MaxItemBlobs = DefaultMaxItemBlobs
	}
	if o.MaxOwners == 0 {
		o.MaxOwners = DefaultMaxOwners
	}
	if o.MaxChangeMessageBytes == 0 {
		o.MaxChangeMessageBytes = protocol.DefaultMaxMessageBytes
		// A deliberately smaller custom journal remains useful in tests and
		// for embedded callers. The default frame bound must still never be
		// larger than the journal's hard bound.
		if o.JournalBytes < o.MaxChangeMessageBytes {
			o.MaxChangeMessageBytes = o.JournalBytes
		}
	}
	if o.BlobRefreshSkew == 0 {
		o.BlobRefreshSkew = DefaultBlobRefreshSkew
	}
	if o.TransientReplayEntries == 0 {
		o.TransientReplayEntries = DefaultTransientReplayEntries
	}
	if o.TransientReplayBytes == 0 {
		o.TransientReplayBytes = DefaultTransientReplayBytes
	}
	if o.TransientLiveCapacity == 0 {
		o.TransientLiveCapacity = DefaultTransientLiveCapacity
	}
	if o.TransientLiveBytes == 0 {
		o.TransientLiveBytes = DefaultTransientLiveBytes
	}
	if o.TransientQueueCapacity == 0 {
		o.TransientQueueCapacity = DefaultTransientQueueCapacity
	}
	if o.TransientQueueBytes == 0 {
		o.TransientQueueBytes = DefaultTransientQueueBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.JournalEntries <= 0 || o.JournalBytes <= 0 || o.LiveCapacity <= 0 || o.ProjectorQueueCapacity <= 0 ||
		o.InlineSnapshotBytes <= 0 || o.HistoryLimit <= 0 || o.HistoryLimit > 1000 || o.MaxCompactionRecords <= 0 ||
		o.MaxItemContentBytes <= 0 || o.MaxItemBlobs <= 0 || o.MaxOwners <= 0 || o.MaxChangeMessageBytes <= 0 ||
		o.MaxChangeMessageBytes > o.JournalBytes || o.BlobRefreshSkew < 0 {
		return ProviderOptions{}, fmt.Errorf("session content bounds are invalid")
	}
	if o.TransientReplayEntries <= 0 || o.TransientReplayBytes <= 0 || o.TransientLiveCapacity <= 0 || o.TransientLiveBytes <= 0 || o.TransientQueueCapacity <= 0 || o.TransientQueueBytes <= 0 {
		return ProviderOptions{}, fmt.Errorf("session content transient bounds are invalid")
	}
	if strings.TrimSpace(o.StreamEpoch) == "" {
		o.StreamEpoch = "session-content"
	}
	return o, nil
}

type Provider struct {
	store   *sessions.V2Store
	options ProviderOptions

	mu              sync.Mutex
	owners          map[string]*owner
	closed          bool
	evicting        int
	closeOnce       sync.Once
	doneCh          chan struct{}
	generation      atomic.Uint64
	ownerGeneration atomic.Uint64
}

type owner struct {
	provider   *Provider
	sessionID  string
	epoch      string
	journal    *syncengine.Journal
	queue      chan ownerTask
	queueBytes int
	queueMu    sync.Mutex
	// queueOverflow is a coalesced, bounded signal separate from queue. A
	// producer which finds the work queue full must not call invalidate inline:
	// invalidation takes the owner projection mutex and would turn a slow owner
	// into a blocking execution path. The owner worker consumes this marker and
	// performs the resource-local recovery transition itself.
	queueOverflow chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc
	workerDone    chan struct{}

	mu            sync.Mutex
	projection    projection
	initialized   bool
	stale         bool
	invalid       bool
	closed        bool
	subs          map[*syncengine.LiveSubscription]struct{}
	transientSubs map[*syncengine.TransientSubscription]struct{}
	transientRun  *runState
	pendingOpens  int
	// openInterest is a producer-facing atomic hint. It spans the complete
	// beginOpen -> owner queue -> openBarrier/captureOpen -> endOpen interval,
	// including durable snapshot/blob work before a live subscription exists.
	// Execution producers read this atomically and never take owner.mu.
	openInterest      atomic.Int64
	pendingAdmissions map[string]int
	gapMu             sync.Mutex
	runGapPending     map[string]struct{}
	subscriberHint    atomic.Int64
	lastUsed          time.Time
	blobs             *itemBlobCache
	claims            atomic.Int64
}

type projection struct {
	snapshot Snapshot
	revision protocol.ResourceRevision
}

type ownerTask struct {
	mutation           *sessions.Mutation
	open               *openRequest
	runAdmitted        *runAdmission
	runGap             *runGapInput
	runEvent           *runEventInput
	runSettled         *runSettlement
	runAdmissionFailed *runAdmission
	stop               bool
	bytes              int
}

type runState struct {
	epoch               string
	runID               string
	cursor              uint64
	desynced            bool
	settled             bool
	uncovered           bool
	replay              []syncengine.TransientEvent
	replayBytes         int
	itemCursors         map[ItemKey]protocol.RunCursor
	settlementWatermark *protocol.DurableSettlementWatermark
}

type runAdmission struct {
	runID, sessionID, turnID string
	startedAt                time.Time
}
type runEventInput struct {
	runID, sessionID string
	event            execution.SessionStreamEvent
}
type runGapInput struct {
	runID, sessionID string
}
type runSettlement struct {
	runID, sessionID, turnID, status string
	result                           execution.SessionMessageResult
	err                              error
}

type openRequest struct {
	ctx             context.Context
	resume          *protocol.ResumeToken
	activeRunResume *protocol.RunResumeToken
	result          chan openResult
}

type openResult struct {
	opened syncengine.OpenedResource
	err    error
}

func NewProvider(store *sessions.V2Store, options ProviderOptions) (*Provider, error) {
	if store == nil {
		return nil, fmt.Errorf("session store is required")
	}
	resolved, err := options.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Provider{store: store, options: resolved, owners: make(map[string]*owner), doneCh: make(chan struct{})}, nil
}

func (p *Provider) Type() protocol.ResourceType { return protocol.ResourceTypeSessionContent }

func (p *Provider) Authorize(ctx context.Context, principal syncengine.Principal, key protocol.ResourceKey) error {
	if p == nil || p.isClosed() {
		return ErrProviderClosed
	}
	if key.Type != protocol.ResourceTypeSessionContent || validateSessionID(key.ID) != nil {
		return fmt.Errorf("invalid session content resource")
	}
	if strings.TrimSpace(principal.ID) == "" {
		return fmt.Errorf("principal is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	// Authorization is intentionally a durable existence check. Project
	// ownership is encoded in the durable record and there is no second
	// project capability in the current single-user gateway.
	_, err := p.store.LoadState(key.ID)
	return err
}

// PublishMutation is the store's bounded post-commit sink. A session without
// an owner has no subscribers and is intentionally ignored; its next Open
// rebuilds from the durable store and starts a fresh stream epoch.
func (p *Provider) PublishMutation(mutation sessions.Mutation) error {
	if p == nil || p.isClosed() {
		return ErrProviderClosed
	}
	if mutation.Overflow {
		p.invalidateAll(ErrQueueFull)
		return nil
	}
	if strings.TrimSpace(mutation.SessionID) == "" {
		return fmt.Errorf("session id is required")
	}
	p.mu.Lock()
	o := p.owners[mutation.SessionID]
	p.mu.Unlock()
	if o == nil {
		return nil
	}
	return o.enqueue(ownerTask{mutation: &mutation, bytes: 256})
}

// RunAdmitted/RunAdmissionFailed/RunEvent/RunSettled implement the shared
// execution event source. They only enqueue full event work when this session
// currently has a session-content subscriber; an idle active run receives at
// most a bounded identity-only gap marker, never Web DTO JSON encoding or a
// blocking producer path merely because it exists.
func (p *Provider) RunAdmitted(run *execution.CoordinatedSessionRun) {
	if run == nil {
		return
	}
	p.publishRunTask(run.SessionID(), ownerTask{runAdmitted: &runAdmission{runID: run.ID(), sessionID: run.SessionID(), startedAt: run.StartedAt()}, bytes: 128})
}

func (p *Provider) RunAdmissionFailed(run *execution.CoordinatedSessionRun) {
	if run == nil {
		return
	}
	p.clearRunGapPending(run)
	p.publishRunTask(run.SessionID(), ownerTask{runAdmissionFailed: &runAdmission{runID: run.ID(), sessionID: run.SessionID()}, bytes: 128})
}

func (p *Provider) RunEvent(run *execution.CoordinatedSessionRun, event execution.SessionStreamEvent) {
	if run == nil || event == nil {
		return
	}
	// The shared source also carries durable projector notices. They are
	// already recovered through the journal and must not turn an otherwise
	// continuous transient run into a gap merely because no session-content
	// subscriber was present for that durable notice.
	if !isTransientExecutionEvent(event) {
		return
	}
	// Estimate/encode the transport-neutral source only after the owner hint
	// says a session-content consumer exists. This both keeps the no-subscriber
	// path cheap and lets the byte bound account for typed prompt slices and
	// other JSON-shaped values that a hand-written estimator could undercount.
	if p == nil || p.isClosed() || strings.TrimSpace(run.SessionID()) == "" {
		return
	}
	p.mu.Lock()
	o := p.owners[run.SessionID()]
	p.mu.Unlock()
	if o == nil {
		return
	}
	if !o.hasOpenInterest() {
		// Do not JSON-encode a source event when no session-content
		// subscription exists. A bounded identity-only gap task preserves the
		// stronger contract: a reconnect may reuse an already encoded replay
		// buffer only when no event was missed while the resource was idle.
		if o.markRunGapPending(run.ID()) {
			if err := o.enqueue(ownerTask{runGap: &runGapInput{runID: run.ID(), sessionID: run.SessionID()}, bytes: 64}); err != nil {
				o.clearRunGapPending(run.ID())
			}
		}
		return
	}
	_ = o.enqueue(ownerTask{runEvent: &runEventInput{runID: run.ID(), sessionID: run.SessionID(), event: event}, bytes: estimateRunEventBytes(event)})
}

// RunEventObserverLoss is the explicit source-mailbox loss signal. It uses
// the same coalesced identity-only marker as the no-subscriber path: at most
// one task is accepted for a run, and the owner will mark that run recovery
// required rather than pretending that later cursors are continuous.
func (p *Provider) RunEventObserverLoss(run *execution.CoordinatedSessionRun, _ string) {
	if run == nil || p == nil || p.isClosed() {
		return
	}
	p.mu.Lock()
	o := p.owners[run.SessionID()]
	p.mu.Unlock()
	if o == nil || !o.markRunGapPending(run.ID()) {
		return
	}
	if err := o.enqueue(ownerTask{runGap: &runGapInput{runID: run.ID(), sessionID: run.SessionID()}, bytes: 64}); err != nil {
		o.clearRunGapPending(run.ID())
	}
}

func (p *Provider) RunSettled(run *execution.CoordinatedSessionRun, result execution.SessionMessageResult, err error) {
	if run == nil {
		return
	}
	p.clearRunGapPending(run)
	p.publishRunTask(run.SessionID(), ownerTask{runSettled: &runSettlement{runID: run.ID(), sessionID: run.SessionID(), turnID: result.TurnID, status: string(run.Status()), result: result, err: err}, bytes: 256})
}

// Terminal source callbacks must retire the coalesced idle-gap marker even
// when there are no subscribers and therefore no owner task is admitted. The
// marker is a recovery hint for one active run, not a historical run index;
// retaining it after settlement would make a long-lived owner grow one entry
// per completed run.
func (p *Provider) clearRunGapPending(run *execution.CoordinatedSessionRun) {
	if p == nil || run == nil {
		return
	}
	p.mu.Lock()
	o := p.owners[run.SessionID()]
	p.mu.Unlock()
	if o != nil {
		o.clearRunGapPending(run.ID())
	}
}

func (p *Provider) publishRunTask(sessionID string, task ownerTask) {
	if p == nil || p.isClosed() || strings.TrimSpace(sessionID) == "" {
		return
	}
	p.mu.Lock()
	o := p.owners[sessionID]
	p.mu.Unlock()
	if o == nil || !o.hasOpenInterest() {
		return
	}
	_ = o.enqueue(task)
}

func (p *Provider) invalidateAll(err error) {
	p.mu.Lock()
	owners := make([]*owner, 0, len(p.owners))
	for _, o := range p.owners {
		owners = append(owners, o)
	}
	p.mu.Unlock()
	for _, o := range owners {
		o.invalidate(err)
	}
}

func (p *Provider) Open(ctx context.Context, key protocol.ResourceKey, resume *protocol.ResumeToken) (syncengine.OpenedResource, error) {
	return p.OpenWithRunResume(ctx, key, resume, nil)
}

func (p *Provider) OpenWithRunResume(ctx context.Context, key protocol.ResourceKey, resume *protocol.ResumeToken, activeRunResume *protocol.RunResumeToken) (syncengine.OpenedResource, error) {
	if p == nil || p.isClosed() {
		return syncengine.OpenedResource{}, ErrProviderClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if key.Type != protocol.ResourceTypeSessionContent || validateSessionID(key.ID) != nil {
		return syncengine.OpenedResource{}, fmt.Errorf("invalid session content resource")
	}
	// Do the durable existence check before allocating an owner. Besides
	// making direct Open's not-found behavior deterministic, this prevents a
	// stream for a typo from consuming an owner slot until eviction.
	if _, err := p.store.LoadState(key.ID); err != nil {
		return syncengine.OpenedResource{}, err
	}
	o, err := p.ownerFor(key.ID)
	if err != nil {
		return syncengine.OpenedResource{}, err
	}
	defer o.claims.Add(-1)
	o.beginOpen()
	request := &openRequest{ctx: ctx, resume: cloneResume(resume), activeRunResume: cloneRunResume(activeRunResume), result: make(chan openResult, 1)}
	if err := o.enqueue(ownerTask{open: request, bytes: 512}); err != nil {
		o.endOpen()
		return syncengine.OpenedResource{}, err
	}
	select {
	case result := <-request.result:
		return result.opened, result.err
	case <-ctx.Done():
		return syncengine.OpenedResource{}, ctx.Err()
	case <-p.done():
		return syncengine.OpenedResource{}, ErrProviderClosed
	}
}

// Warm validates discovery without retaining one owner per durable session.
// A later Open is the owner barrier and reconstructs the complete bounded
// baseline from the store. This keeps startup memory bounded by MaxOwners.
func (p *Provider) Warm(ctx context.Context) error {
	if p == nil || p.isClosed() {
		return ErrProviderClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	states, err := p.store.ListStates(sessions.V2ListOptions{All: true})
	if err != nil {
		return err
	}
	for _, state := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		if validateSessionID(state.ID) != nil {
			return fmt.Errorf("durable session has empty id")
		}
		if state.LastSeq < 0 {
			return fmt.Errorf("durable session %q has negative LastSeq", state.ID)
		}
	}
	return nil
}

func (p *Provider) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.doneCh)
		owners := make([]*owner, 0, len(p.owners))
		for id, o := range p.owners {
			owners = append(owners, o)
			delete(p.owners, id)
		}
		p.mu.Unlock()
		for _, o := range owners {
			o.close()
		}
	})
}

func (p *Provider) isClosed() bool        { p.mu.Lock(); defer p.mu.Unlock(); return p.closed }
func (p *Provider) done() <-chan struct{} { return p.doneCh }

func (p *Provider) ownerFor(sessionID string) (*owner, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrProviderClosed
		}
		if o := p.owners[sessionID]; o != nil {
			o.claims.Add(1)
			p.mu.Unlock()
			return o, nil
		}
		if len(p.owners)+p.evicting < p.options.MaxOwners {
			o, err := newOwner(p, sessionID)
			if err == nil {
				p.owners[sessionID] = o
				o.claims.Add(1)
			}
			p.mu.Unlock()
			return o, err
		}

		// Select and remove an idle owner while holding only the provider map
		// lock. Closing it may wait for its worker and must never happen under
		// p.mu: an unrelated Open/Close must remain able to make progress.
		var evictID string
		var evict *owner
		var evictLast time.Time
		for id, candidate := range p.owners {
			if candidate.claims.Load() != 0 {
				continue
			}
			candidate.mu.Lock()
			idle := len(candidate.subs) == 0 && !candidate.closed
			last := candidate.lastUsed
			candidate.mu.Unlock()
			if idle && (evict == nil || last.Before(evictLast)) {
				evictID, evict = id, candidate
				evictLast = last
			}
		}
		if evict == nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("session content owner capacity exceeded")
		}
		delete(p.owners, evictID)
		p.evicting++
		p.mu.Unlock()
		evict.close()
		p.mu.Lock()
		p.evicting--
		p.mu.Unlock()
		// The slot is now free. Loop so shutdown and a concurrent owner for
		// this same ID are rechecked under the map lock.
	}
}

func newOwner(p *Provider, sessionID string) (*owner, error) {
	parent := p.options.OwnerContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	generation := p.ownerGeneration.Add(1)
	epoch := fmt.Sprintf("%s:%s:%d", strings.TrimRight(p.options.StreamEpoch, ":"), sessionID, generation)
	if len(epoch) > protocol.MaxWireIdentifierBytes {
		cancel()
		return nil, fmt.Errorf("session content stream epoch exceeds the maximum wire identifier length")
	}
	queueCapacity := p.options.ProjectorQueueCapacity
	if p.options.TransientQueueCapacity < queueCapacity {
		queueCapacity = p.options.TransientQueueCapacity
	}
	journal, err := syncengine.NewBoundedJournal(epoch, p.options.JournalEntries, p.options.JournalBytes)
	if err != nil {
		cancel()
		return nil, err
	}
	o := &owner{provider: p, sessionID: sessionID, epoch: epoch, journal: journal, queue: make(chan ownerTask, queueCapacity), queueOverflow: make(chan struct{}, 1), ctx: ctx, cancel: cancel, workerDone: make(chan struct{}), subs: make(map[*syncengine.LiveSubscription]struct{}), transientSubs: make(map[*syncengine.TransientSubscription]struct{}), pendingAdmissions: make(map[string]int), runGapPending: make(map[string]struct{}), blobs: newItemBlobCache(p.options.MaxItemBlobs)}
	go o.run()
	return o, nil
}

func (o *owner) enqueue(task ownerTask) error {
	if o == nil {
		return ErrProviderClosed
	}
	select {
	case <-o.ctx.Done():
		return ErrProviderClosed
	default:
	}
	if task.bytes <= 0 {
		task.bytes = 128
	}
	o.queueMu.Lock()
	if len(o.queue) >= cap(o.queue) || o.queueBytes+task.bytes > o.provider.options.TransientQueueBytes {
		o.queueMu.Unlock()
		o.signalQueueOverflow()
		return ErrQueueFull
	}
	select {
	case o.queue <- task:
		o.queueBytes += task.bytes
		if task.runAdmitted != nil {
			o.pendingAdmissions[task.runAdmitted.runID]++
		}
		o.queueMu.Unlock()
		return nil
	default:
		o.queueMu.Unlock()
		o.signalQueueOverflow()
		return ErrQueueFull
	}
}

// signalQueueOverflow is intentionally a non-blocking, coalesced handoff to
// the owner worker. In particular, it never takes owner.mu, so a producer is
// still bounded/non-blocking even while the worker is inside a slow snapshot
// or projection operation. One marker is enough to invalidate the resource;
// dropping further markers cannot create another silent cursor advance.
func (o *owner) signalQueueOverflow() {
	if o == nil || o.queueOverflow == nil {
		return
	}
	select {
	case o.queueOverflow <- struct{}{}:
	default:
	}
}

func (o *owner) releaseTask(task ownerTask) {
	o.queueMu.Lock()
	if o.queueBytes >= task.bytes {
		o.queueBytes -= task.bytes
	} else {
		o.queueBytes = 0
	}
	if task.runAdmitted != nil {
		if pending := o.pendingAdmissions[task.runAdmitted.runID]; pending <= 1 {
			delete(o.pendingAdmissions, task.runAdmitted.runID)
		} else {
			o.pendingAdmissions[task.runAdmitted.runID] = pending - 1
		}
	}
	o.queueMu.Unlock()
}

func (o *owner) hasPendingRunAdmission(runID string) bool {
	o.queueMu.Lock()
	defer o.queueMu.Unlock()
	return o.pendingAdmissions[runID] > 0
}

func (o *owner) hasOpenInterest() bool {
	if o == nil {
		return false
	}
	// Both counters deliberately avoid owner.mu on the execution producer
	// path. openInterest is separate from the subscriber count so a pending
	// open cannot be mistaken for an already registered subscription (or be
	// lost if a future subscriber accounting change adjusts subscriberHint).
	return o.openInterest.Load() > 0 || o.subscriberHint.Load() > 0
}

func (o *owner) markRunGapPending(runID string) bool {
	runID = strings.TrimSpace(runID)
	if o == nil || runID == "" {
		return false
	}
	o.gapMu.Lock()
	defer o.gapMu.Unlock()
	if _, exists := o.runGapPending[runID]; exists {
		return false
	}
	o.runGapPending[runID] = struct{}{}
	return true
}

func (o *owner) clearRunGapPending(runID string) {
	if o == nil {
		return
	}
	o.gapMu.Lock()
	delete(o.runGapPending, runID)
	o.gapMu.Unlock()
}

func (o *owner) beginOpen() {
	o.mu.Lock()
	if !o.closed {
		o.pendingOpens++
		o.openInterest.Add(1)
	}
	o.mu.Unlock()
}
func (o *owner) endOpen() {
	o.mu.Lock()
	if o.pendingOpens > 0 {
		o.pendingOpens--
		o.openInterest.Add(-1)
	}
	o.mu.Unlock()
}

func (o *owner) run() {
	defer close(o.workerDone)
	for {
		// Prefer a queued overflow marker over another producer task. This keeps
		// a full queue from being drained as if it were continuous after loss.
		select {
		case <-o.ctx.Done():
			return
		case <-o.queueOverflow:
			o.invalidate(ErrQueueFull)
			continue
		default:
		}
		select {
		case <-o.ctx.Done():
			return
		case <-o.queueOverflow:
			o.invalidate(ErrQueueFull)
		case task := <-o.queue:
			o.releaseTask(task)
			if task.stop {
				return
			}
			if task.open != nil {
				o.handleOpen(task.open)
				continue
			}
			if task.mutation != nil {
				o.handleMutation(*task.mutation)
			}
			if task.runAdmitted != nil {
				o.handleRunAdmitted(*task.runAdmitted)
			}
			if task.runGap != nil {
				o.handleRunGap(*task.runGap)
			}
			if task.runAdmissionFailed != nil {
				o.handleRunAdmissionFailed(*task.runAdmissionFailed)
			}
			if task.runEvent != nil {
				o.handleRunEvent(*task.runEvent)
			}
			if task.runSettled != nil {
				o.handleRunSettled(*task.runSettled)
			}
		}
	}
}

func (o *owner) handleOpen(request *openRequest) {
	if request == nil {
		return
	}
	// The owner queue is ordered and the pending-open hint keeps producers
	// enqueueing while this task waits for the durable barrier. Consequently a
	// run task accepted during Open is processed either before capture (and is
	// included in replay) or after the registered live subscription (and is
	// delivered live); no separate unbounded pending slice is needed.
	defer o.endOpen()
	opened, err := o.openBarrier(request.ctx, request.resume, request.activeRunResume)
	if request.ctx != nil && request.ctx.Err() != nil {
		if opened.Close != nil {
			opened.Close()
		}
		return
	}
	select {
	case request.result <- openResult{opened: opened, err: err}:
	default:
		if opened.Close != nil {
			opened.Close()
		}
	}
}

func (o *owner) openBarrier(ctx context.Context, resume *protocol.ResumeToken, activeRunResume *protocol.RunResumeToken) (syncengine.OpenedResource, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return syncengine.OpenedResource{}, ctx.Err()
	default:
	}
	o.mu.Lock()
	needBuild := !o.initialized || o.stale || o.invalid
	o.mu.Unlock()
	if needBuild {
		projectionContext, finishProjection := o.requestContext(ctx)
		built, err := o.provider.buildProjection(projectionContext, o.sessionID, o.blobs)
		finishProjection()
		if err != nil {
			return syncengine.OpenedResource{}, err
		}
		o.mu.Lock()
		reset := o.initialized && (o.stale || o.invalid)
		preserveTransient := o.transientRun != nil && !o.transientRun.settled && built.snapshot.ActiveRun != nil && built.snapshot.ActiveRun.RunID == o.transientRun.runID
		if reset {
			if err := o.resetJournalLocked(); err != nil {
				o.mu.Unlock()
				return syncengine.OpenedResource{}, err
			}
			// An idle unsubscribe can rebuild the durable journal while the
			// independent in-memory run is still active. Preserve that run only
			// when the rebuilt durable state names the same run; a new owner (or
			// a settled/changed run) has no trustworthy transient baseline and
			// remains recovery-required.
			if !preserveTransient {
				o.transientRun = nil
			}
		}
		if !o.initialized || o.stale || o.invalid {
			o.projection = built
			o.initialized, o.stale, o.invalid = true, false, false
		}
		o.mu.Unlock()
	}
	encoded, content, revision, epoch, sequence, decision, live, sub, transientReplay, transient, transientResync, err := o.captureOpen(ctx, resume, activeRunResume)
	if err != nil {
		return syncengine.OpenedResource{}, err
	}
	if len(encoded) > o.provider.options.InlineSnapshotBytes {
		if o.provider.options.BlobWriter == nil {
			sub.Close()
			o.removeSub(sub)
			if transient.Close != nil {
				transient.Close()
			}
			return syncengine.OpenedResource{}, fmt.Errorf("session content snapshot is %d bytes and no blob writer is configured", len(encoded))
		}
		blobContext, finishBlob := o.requestContext(ctx)
		descriptor, blobErr := o.provider.putBlob(blobContext, "application/json", encoded)
		finishBlob()
		if blobErr != nil {
			sub.Close()
			o.removeSub(sub)
			if transient.Close != nil {
				transient.Close()
			}
			return syncengine.OpenedResource{}, fmt.Errorf("store session content snapshot blob: %w", blobErr)
		}
		content = syncengine.NewBlobSnapshotContent(descriptor)
	}
	return syncengine.OpenedResource{
		Snapshot:    syncengine.Snapshot{Content: content, ResourceRevision: revision},
		StreamEpoch: epoch, Sequence: sequence, Decision: decision, LiveFromSequence: sequence + 1,
		Changes: live.Entries, Terminal: live.Terminal,
		TransientReplay: transientReplay, Transient: transient, TransientResync: transientResync,
		Close: func() {
			sub.Close()
			o.removeSub(sub)
			if transient.Close != nil {
				transient.Close()
			}
		},
	}, nil
}

// requestContext combines the owner lifetime with the caller's request
// lifetime. A server shutdown therefore cancels a durable projection or Blob
// write even when the caller supplied context.Background().
func (o *owner) requestContext(request context.Context) (context.Context, func()) {
	if request == nil {
		request = context.Background()
	}
	combined, cancel := context.WithCancel(o.ctx)
	stopRequest := context.AfterFunc(request, cancel)
	return combined, func() {
		stopRequest()
		cancel()
	}
}

func (o *owner) captureOpen(ctx context.Context, resume *protocol.ResumeToken, activeRunResume *protocol.RunResumeToken) ([]byte, syncengine.SnapshotContent, protocol.ResourceRevision, string, uint64, syncengine.ResumeDecision, syncengine.LiveDelivery, *syncengine.LiveSubscription, []syncengine.TransientEvent, syncengine.TransientDelivery, string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, syncengine.SnapshotContent{}, "", "", 0, syncengine.ResumeDecision{}, syncengine.LiveDelivery{}, nil, nil, syncengine.TransientDelivery{}, "", ErrProviderClosed
	}
	if !o.initialized || o.invalid {
		return nil, syncengine.SnapshotContent{}, "", "", 0, syncengine.ResumeDecision{}, syncengine.LiveDelivery{}, nil, nil, syncengine.TransientDelivery{}, "", ErrProviderInvalid
	}
	sequence := o.journal.LastSequence()
	epoch := o.journal.Epoch()
	sub, err := syncengine.NewLiveSubscription(epoch, sequence, o.provider.options.LiveCapacity)
	if err != nil {
		return nil, syncengine.SnapshotContent{}, "", "", 0, syncengine.ResumeDecision{}, syncengine.LiveDelivery{}, nil, nil, syncengine.TransientDelivery{}, "", err
	}
	o.subs[sub] = struct{}{}
	o.subscriberHint.Add(1)
	transientReplay, transientDelivery, transientResync, transientSub := o.captureTransientLocked(activeRunResume)
	if transientSub != nil {
		o.transientSubs[transientSub] = struct{}{}
	}
	snapshot := o.snapshotForOpenLocked()
	if err := snapshot.Validate(); err != nil {
		if transientSub != nil {
			delete(o.transientSubs, transientSub)
			transientSub.Close()
		}
		delete(o.subs, sub)
		o.subscriberHint.Add(-1)
		sub.Close()
		return nil, syncengine.SnapshotContent{}, "", "", 0, syncengine.ResumeDecision{}, syncengine.LiveDelivery{}, nil, nil, syncengine.TransientDelivery{}, "", err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		if transientSub != nil {
			delete(o.transientSubs, transientSub)
			transientSub.Close()
		}
		delete(o.subs, sub)
		o.subscriberHint.Add(-1)
		sub.Close()
		return nil, syncengine.SnapshotContent{}, "", "", 0, syncengine.ResumeDecision{}, syncengine.LiveDelivery{}, nil, nil, syncengine.TransientDelivery{}, "", err
	}
	o.lastUsed = time.Now()
	decision := o.journal.Decide(resume)
	return encoded, syncengine.NewInlineSnapshotContent(encoded), o.projection.revision, epoch, sequence, decision, sub.Delivery(), sub, transientReplay, transientDelivery, transientResync, nil
}

func (o *owner) snapshotForOpenLocked() Snapshot {
	return o.snapshotWithTransientLocked(o.projection.snapshot)
}

// snapshotWithTransientLocked decorates a durable baseline with the current
// in-memory run recovery descriptor. The decoration is not a durable write and
// does not advance either resource revision or journal sequence. It is also
// applied to active_run.replace changes, otherwise a client that was already
// subscribed would never learn the run epoch needed for a later reconnect.
func (o *owner) snapshotWithTransientLocked(snapshot Snapshot) Snapshot {
	if snapshot.ActiveRun == nil {
		return snapshot
	}
	active := *snapshot.ActiveRun
	// Do not carry a previous in-memory decoration through a durable
	// projection update. A desync/rebuild must not advertise stale replay
	// coverage from an earlier run epoch.
	active.RunEpoch = ""
	active.RunCursor = ""
	active.ReplayAvailable = false
	active.ReplayFromCursor = ""
	active.ReplayToCursor = ""
	active.SettlementWatermark = nil
	active.RecoveryRequired = true
	if o.transientRun != nil && o.transientRun.runID == active.RunID {
		active.RunEpoch = o.transientRun.epoch
		active.RunCursor = protocol.RunCursor(strconv.FormatUint(o.transientRun.cursor, 10))
		if !o.transientRun.desynced && (o.transientRun.cursor == 0 || len(o.transientRun.replay) > 0) {
			active.RecoveryRequired = false
		}
		if !o.transientRun.desynced && len(o.transientRun.replay) > 0 {
			active.ReplayAvailable = true
			active.ReplayFromCursor = o.transientRun.replay[0].Cursor
			active.ReplayToCursor = o.transientRun.replay[len(o.transientRun.replay)-1].Cursor
		}
		if o.transientRun.settlementWatermark != nil {
			watermark := *o.transientRun.settlementWatermark
			watermark.CoveredItems = make([]protocol.TransientItemWatermark, len(o.transientRun.settlementWatermark.CoveredItems))
			copy(watermark.CoveredItems, o.transientRun.settlementWatermark.CoveredItems)
			active.SettlementWatermark = &watermark
		}
	}
	snapshot.ActiveRun = &active
	return snapshot
}

func (o *owner) captureTransientLocked(resume *protocol.RunResumeToken) ([]syncengine.TransientEvent, syncengine.TransientDelivery, string, *syncengine.TransientSubscription) {
	active := o.projection.snapshot.ActiveRun
	// If the durable active run is gone and the retained in-memory run state is
	// no longer live (settled or desynced), it is stale and must not keep a
	// later open permanently recovery-required. A live transient run is
	// preserved: it may legitimately precede a durable projection that has not
	// yet caught up (or the durable session has no run row at all in tests).
	if active == nil && o.transientRun != nil && (o.transientRun.settled || o.transientRun.desynced) && !o.hasPendingRunAdmission(o.transientRun.runID) {
		o.transientRun = nil
	}
	runID := ""
	// The transport-neutral run source is ahead of the durable active-run
	// mutation during admission. Prefer it while it is live; otherwise a
	// concurrent open could replace a just-admitted run with the previous
	// durable descriptor and make all subsequent events look stale.
	if o.transientRun != nil && !o.transientRun.settled {
		runID = o.transientRun.runID
	} else if active != nil {
		runID = active.RunID
	}
	if o.transientRun == nil && active != nil && o.hasPendingRunAdmission(active.RunID) {
		// The owner worker is currently inside the open barrier, so the
		// admission task cannot have run yet. Its presence in the bounded queue
		// is the proof that this active durable run is the run whose cursor-1
		// baseline is about to be published; do not fabricate a baseline for a
		// durable run which merely survived a process restart.
		o.transientRun = &runState{epoch: o.epoch, runID: active.RunID, itemCursors: make(map[ItemKey]protocol.RunCursor)}
		runID = active.RunID
	}
	if runID == "" {
		if resume != nil {
			return nil, syncengine.TransientDelivery{}, "active_run_mismatch", nil
		}
		sub, err := syncengine.NewTransientSubscription(o.epoch, "", 0, o.provider.options.TransientLiveCapacity, o.provider.options.TransientLiveBytes)
		if err != nil {
			return nil, syncengine.TransientDelivery{}, "active_run_recovery_required", nil
		}
		delivery := sub.Delivery()
		delivery.Close = func() { o.removeTransientSub(sub) }
		return nil, delivery, "", sub
	}
	// A durable active-run descriptor without an in-memory run state is the
	// process-restart/late-join case. There is no trustworthy cursor baseline:
	// starting at zero would fabricate continuity for events already emitted.
	// Keep the durable snapshot, but force the resource-local active-run
	// recovery contract instead of manufacturing a replayable stream.
	if o.transientRun == nil || o.transientRun.runID != runID || o.transientRun.settled {
		return nil, syncengine.TransientDelivery{}, "active_run_recovery_required", nil
	}
	run := o.transientRun
	if run.desynced {
		return nil, syncengine.TransientDelivery{}, "active_run_recovery_required", nil
	}
	base := uint64(0)
	if resume != nil {
		if resume.RunID != run.runID || resume.RunEpoch != run.epoch {
			return nil, syncengine.TransientDelivery{}, "active_run_epoch_mismatch", nil
		}
		parsed, err := protocol.ParseUint64Decimal(string(resume.RunCursor))
		if err != nil {
			return nil, syncengine.TransientDelivery{}, "active_run_cursor_invalid", nil
		}
		base = parsed
	}
	if base > run.cursor {
		return nil, syncengine.TransientDelivery{}, "active_run_cursor_ahead", nil
	}
	if run.cursor > 0 && len(run.replay) == 0 {
		if resume != nil {
			return nil, syncengine.TransientDelivery{}, "active_run_cursor_too_old", nil
		}
		return nil, syncengine.TransientDelivery{}, "active_run_recovery_required", nil
	}
	if len(run.replay) > 0 {
		first, _ := protocol.ParseUint64Decimal(string(run.replay[0].Cursor))
		if base+1 < first && resume != nil {
			return nil, syncengine.TransientDelivery{}, "active_run_cursor_too_old", nil
		}
		if resume == nil && base+1 < first {
			base = first - 1
		}
	}
	replay := make([]syncengine.TransientEvent, 0, len(run.replay))
	for _, event := range run.replay {
		cursor, _ := protocol.ParseUint64Decimal(string(event.Cursor))
		if cursor > base {
			replay = append(replay, event)
		}
	}
	// Replay is sent directly by the dispatcher before the live pump starts.
	// Advance the live subscription's continuity baseline over those entries;
	// otherwise the first post-barrier event would be reported as a cursor gap
	// because the delivery object only sees live Offer calls.
	liveBase := base
	if len(replay) > 0 {
		liveBase, _ = protocol.ParseUint64Decimal(string(replay[len(replay)-1].Cursor))
	}
	sub, err := syncengine.NewTransientSubscription(run.epoch, run.runID, liveBase, o.provider.options.TransientLiveCapacity, o.provider.options.TransientLiveBytes)
	if err != nil {
		return nil, syncengine.TransientDelivery{}, "active_run_recovery_required", nil
	}
	delivery := sub.Delivery()
	delivery.Close = func() { o.removeTransientSub(sub) }
	return replay, delivery, "", sub
}

func (o *owner) handleMutation(mutation sessions.Mutation) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	if len(o.subs) == 0 && o.pendingOpens == 0 {
		if o.initialized {
			o.stale = true
			// Keep a live run's independent replay state across a short
			// unsubscribe/reconnect window. RunEvent records an identity-only
			// gap while the owner is idle, so this state is never advertised as
			// continuous if a source event was missed. A settled/no-run state can
			// be discarded immediately.
			if o.transientRun == nil || o.transientRun.settled {
				_ = o.resetJournalLocked()
				o.transientRun = nil
			}
		}
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()
	if mutation.Deleted {
		o.deleteResource()
		return
	}
	built, err := o.provider.buildProjection(o.ctx, o.sessionID, o.blobs)
	if err != nil {
		o.invalidate(err)
		return
	}
	o.applyProjection(built)
}

func (o *owner) handleRunAdmitted(admission runAdmission) {
	o.clearRunGapPending(admission.runID)
	o.mu.Lock()
	if o.closed || admission.sessionID != o.sessionID || (len(o.subs) == 0 && o.pendingOpens == 0) {
		o.mu.Unlock()
		return
	}
	// A desynced old run is already outside the continuity contract. The
	// coordinator's next admission is authoritative and must replace that
	// poisoned state; otherwise one lost run could prevent every later run in
	// the same session from establishing a fresh cursor-1 baseline.
	if o.transientRun != nil && o.transientRun.runID != admission.runID && !o.transientRun.settled && !o.transientRun.desynced {
		o.mu.Unlock()
		return
	}
	o.transientRun = &runState{epoch: o.epoch, runID: admission.runID, itemCursors: make(map[ItemKey]protocol.RunCursor)}
	event := protocol.TransientSubscriptionEvent{Type: protocol.SubscriptionEventRunStarted, SessionID: admission.sessionID, RunID: admission.runID, RunCursor: protocol.RunCursor("0"), Status: "running"}
	entries, subs, err := o.appendTransientLocked(event)
	o.mu.Unlock()
	if err != nil {
		o.desyncTransient(err)
		return
	}
	o.deliverTransient(entries, subs)
}

func (o *owner) handleRunAdmissionFailed(admission runAdmission) {
	o.clearRunGapPending(admission.runID)
	o.mu.Lock()
	shouldDesync := admission.sessionID == o.sessionID && o.transientRun != nil && o.transientRun.runID == admission.runID && !o.transientRun.settled
	if shouldDesync {
		o.transientRun.desynced = true
	}
	o.mu.Unlock()
	if shouldDesync {
		o.desyncTransient(ErrProviderInvalid)
	}
}

func (o *owner) handleRunGap(gap runGapInput) {
	defer o.clearRunGapPending(gap.runID)
	o.mu.Lock()
	shouldDesync := !o.closed && gap.sessionID == o.sessionID && o.transientRun != nil && o.transientRun.runID == gap.runID && !o.transientRun.settled && !o.transientRun.desynced
	if shouldDesync {
		o.transientRun.desynced = true
	}
	o.mu.Unlock()
	if shouldDesync {
		o.desyncTransient(ErrProviderInvalid)
	}
}

func (o *owner) handleRunEvent(input runEventInput) {
	o.mu.Lock()
	if o.closed || input.sessionID != o.sessionID {
		o.mu.Unlock()
		return
	}
	// Filter run identity before decoding the event. A late event from an old
	// run must not turn a malformed old payload into a desync of the replacement
	// active run. If no durable run is active either, there is no current
	// transient stream which this event could validly establish.
	if o.transientRun != nil {
		if o.transientRun.runID != input.runID || o.transientRun.settled || o.transientRun.desynced {
			o.mu.Unlock()
			return
		}
	} else if active := o.projection.snapshot.ActiveRun; active == nil || active.RunID != input.runID {
		o.mu.Unlock()
		return
	}
	event, ok, err := subscriptionEventFromExecution(input.event, input.sessionID, input.runID)
	if err != nil {
		o.mu.Unlock()
		o.desyncTransient(err)
		return
	}
	if !ok {
		// The execution source is shared with the durable projector. A durable
		// notice has no transient cursor and therefore cannot create a transient
		// gap when it arrives after a subscription closes.
		o.mu.Unlock()
		return
	}
	if len(o.subs) == 0 && o.pendingOpens == 0 {
		shouldDesync := o.transientRun != nil && o.transientRun.runID == input.runID && !o.transientRun.settled && !o.transientRun.desynced
		if shouldDesync {
			o.transientRun.desynced = true
		}
		o.mu.Unlock()
		if shouldDesync {
			o.desyncTransient(ErrProviderInvalid)
		}
		return
	}
	if o.transientRun == nil {
		// The admission/start event was not observed by this owner (for
		// example, a bounded owner queue overflowed). Starting the cursor from
		// an arbitrary mid-run event would be a fabricated replay baseline. If
		// the durable projection already names a different active run, this is
		// specifically a late event from the old run and must not poison the
		// replacement run's recovery state.
		o.transientRun = &runState{epoch: o.epoch, runID: input.runID, desynced: true, itemCursors: make(map[ItemKey]protocol.RunCursor)}
		o.mu.Unlock()
		o.desyncTransient(ErrProviderInvalid)
		return
	}
	if o.transientRun == nil || o.transientRun.runID != input.runID || o.transientRun.settled || o.transientRun.desynced {
		o.mu.Unlock()
		return
	}
	entries, subs, appendErr := o.appendTransientLocked(event)
	o.mu.Unlock()
	if appendErr != nil {
		o.desyncTransient(appendErr)
		return
	}
	o.deliverTransient(entries, subs)
}

func (o *owner) handleRunSettled(settlement runSettlement) {
	defer o.clearRunGapPending(settlement.runID)
	o.mu.Lock()
	if o.closed || settlement.sessionID != o.sessionID || o.transientRun == nil || o.transientRun.runID != settlement.runID || o.transientRun.desynced || o.transientRun.settled {
		o.mu.Unlock()
		return
	}
	itemCursors := make(map[ItemKey]protocol.RunCursor, len(o.transientRun.itemCursors))
	for key, cursor := range o.transientRun.itemCursors {
		itemCursors[key] = cursor
	}
	cursor := protocol.RunCursor(strconv.FormatUint(o.transientRun.cursor, 10))
	uncovered := o.transientRun.uncovered
	o.mu.Unlock()

	watermark := protocol.DurableSettlementWatermark{ResourceRevision: protocol.ResourceRevision(strconv.FormatInt(settlement.result.LastSeq, 10)), RunCursor: cursor, CoveredItems: make([]protocol.TransientItemWatermark, 0)}
	built, buildErr := o.provider.buildProjection(o.ctx, o.sessionID, o.blobs)
	if !uncovered && buildErr == nil && built.revision == watermark.ResourceRevision {
		watermark.Verified = true
		for key, itemCursor := range itemCursors {
			for _, item := range built.snapshot.History.Items {
				if item.Key == key {
					watermark.CoveredItems = append(watermark.CoveredItems, protocol.TransientItemWatermark{TurnID: key.TurnID, AgentIteration: key.AgentIteration, ItemID: key.ItemID, RunCursor: itemCursor})
					break
				}
			}
		}
		// A non-empty transient item set must be completely present in the
		// verified durable window. A revision match alone is not proof of item
		// coverage, so fall back to recovery when the window was truncated.
		if len(watermark.CoveredItems) != len(itemCursors) {
			watermark.Verified = false
		}
		if !watermark.Verified {
			// A partial list is not a usable proof. Do not leave D3 with a
			// tempting subset that could accidentally clear only part of an
			// overlay while the remaining tail still needs recovery.
			watermark.CoveredItems = make([]protocol.TransientItemWatermark, 0)
		} else {
			sort.Slice(watermark.CoveredItems, func(i, j int) bool {
				left, right := watermark.CoveredItems[i], watermark.CoveredItems[j]
				if left.TurnID != right.TurnID {
					return left.TurnID < right.TurnID
				}
				if left.AgentIteration != right.AgentIteration {
					return left.AgentIteration < right.AgentIteration
				}
				return left.ItemID < right.ItemID
			})
		}
	}
	o.mu.Lock()
	if o.transientRun == nil || o.transientRun.runID != settlement.runID || o.transientRun.desynced {
		o.mu.Unlock()
		return
	}
	event := protocol.TransientSubscriptionEvent{Type: protocol.SubscriptionEventRunSettled, SessionID: settlement.sessionID, RunID: settlement.runID, RunCursor: cursor, TurnID: settlement.turnID, Status: settlement.status, Settlement: &watermark}
	entries, subs, err := o.appendTransientLocked(event)
	if err == nil {
		o.transientRun.settlementWatermark = &watermark
		o.transientRun.settled = true
	}
	o.mu.Unlock()
	if err != nil {
		o.desyncTransient(err)
		return
	}
	o.deliverTransient(entries, subs)
}

func (o *owner) appendTransientLocked(event protocol.TransientSubscriptionEvent) ([]syncengine.TransientEvent, []*syncengine.TransientSubscription, error) {
	if o.transientRun == nil {
		return nil, nil, fmt.Errorf("active run is not registered")
	}
	if event.RunID != o.transientRun.runID {
		return nil, nil, fmt.Errorf("run identity changed")
	}
	parts := []protocol.TransientSubscriptionEvent{event}
	splitValue := ""
	switch event.Type {
	case protocol.SubscriptionEventAssistantMessageUpdated, protocol.SubscriptionEventAssistantMessageCompleted, protocol.SubscriptionEventAssistantMessageFailed:
		if !utf8.ValidString(event.AssistantContent) || !utf8.ValidString(event.Reasoning) {
			return nil, nil, fmt.Errorf("assistant message snapshot is not valid UTF-8")
		}
	case protocol.SubscriptionEventToolProgress:
		if !utf8.ValidString(event.ArgumentsDelta) {
			return nil, nil, fmt.Errorf("transient tool arguments delta is not valid UTF-8")
		}
		splitValue = event.ArgumentsDelta
	}
	if splitValue != "" && len(splitValue) > 128*1024 {
		parts = nil
		remaining := splitValue
		for remaining != "" {
			cut := 128 * 1024
			if cut > len(remaining) {
				cut = len(remaining)
			}
			for cut > 0 && cut < len(remaining) && !utf8.ValidString(remaining[:cut]) {
				cut--
			}
			if cut == 0 {
				_, size := utf8.DecodeRuneInString(remaining)
				cut = size
			}
			part := event
			part.ArgumentsDelta, remaining = remaining[:cut], remaining[cut:]
			parts = append(parts, part)
		}
	}
	entries := make([]syncengine.TransientEvent, 0, len(parts))
	acceptedParts := make([]protocol.TransientSubscriptionEvent, 0, len(parts))
	nextCursor := o.transientRun.cursor
	for _, part := range parts {
		candidateCursor := nextCursor + 1
		part.RunCursor = protocol.RunCursor(strconv.FormatUint(candidateCursor, 10))
		raw, err := json.Marshal(part)
		if err != nil {
			return nil, nil, err
		}
		frameBytes, frameErr := preflightSubscriptionEventFrame(raw, o.sessionID)
		if frameErr != nil {
			if errors.Is(frameErr, errTransientFrameTooLarge) && part.Type == protocol.SubscriptionEventAssistantMessageUpdated {
				// A cumulative live snapshot can exceed the negotiated frame size.
				// Skip this revision without consuming a cursor; a later compact
				// terminal event and durable projection still close the lifecycle.
				continue
			}
			if errors.Is(frameErr, errTransientFrameTooLarge) && (part.Type == protocol.SubscriptionEventAssistantMessageCompleted || part.Type == protocol.SubscriptionEventAssistantMessageFailed) {
				part.AssistantContent, part.Reasoning, part.ToolCalls = "", "", nil
				part.SnapshotOmitted = true
				raw, err = json.Marshal(part)
				if err != nil {
					return nil, nil, err
				}
				frameBytes, frameErr = preflightSubscriptionEventFrame(raw, o.sessionID)
			}
			if frameErr != nil {
				return nil, nil, frameErr
			}
		}
		entry := syncengine.TransientEvent{RunEpoch: o.transientRun.epoch, RunID: o.transientRun.runID, Cursor: part.RunCursor, Event: raw, Bytes: frameBytes}
		if entry.Bytes > o.provider.options.TransientReplayBytes {
			return nil, nil, fmt.Errorf("single transient event exceeds replay byte bound")
		}
		entries = append(entries, entry)
		acceptedParts = append(acceptedParts, part)
		nextCursor = candidateCursor
	}
	o.transientRun.cursor = nextCursor
	for index, entry := range entries {
		// Replay is a sliding recovery window, not a lifetime event budget.
		// Live subscribers receive every entry through their independent bounded
		// delivery queue; only reconnects older than the retained first cursor
		// require resource-local recovery. Evict before append so the retained
		// messages and bytes never exceed either hard limit.
		for len(o.transientRun.replay) >= o.provider.options.TransientReplayEntries || o.transientRun.replayBytes > o.provider.options.TransientReplayBytes-entry.Bytes {
			if len(o.transientRun.replay) == 0 {
				break
			}
			oldest := o.transientRun.replay[0]
			o.transientRun.replay = o.transientRun.replay[1:]
			if o.transientRun.replayBytes >= oldest.Bytes {
				o.transientRun.replayBytes -= oldest.Bytes
			} else {
				o.transientRun.replayBytes = 0
			}
		}
		o.transientRun.replay = append(o.transientRun.replay, entry)
		o.transientRun.replayBytes += entry.Bytes
		part := acceptedParts[index]
		switch part.Type {
		case protocol.SubscriptionEventAssistantMessageStarted, protocol.SubscriptionEventAssistantMessageUpdated, protocol.SubscriptionEventAssistantMessageCompleted:
			if part.ItemID != "" {
				o.transientRun.itemCursors[ItemKey{TurnID: part.TurnID, AgentIteration: part.AgentIteration, ItemID: part.ItemID}] = entry.Cursor
			}
		case protocol.SubscriptionEventAssistantMessageFailed:
			delete(o.transientRun.itemCursors, ItemKey{TurnID: part.TurnID, AgentIteration: part.AgentIteration, ItemID: part.ItemID})
		case protocol.SubscriptionEventRunStarted, protocol.SubscriptionEventTurnFailed, protocol.SubscriptionEventRunSettled:
		default:
			// Tool calls and prompt queue mutations have stable transient
			// identities, but the current durable projection has no atomic
			// relation from those identities to the covered assistant/user
			// item. Do not claim a verified settlement watermark for them.
			o.transientRun.uncovered = true
		}
	}
	subs := make([]*syncengine.TransientSubscription, 0, len(o.transientSubs))
	for sub := range o.transientSubs {
		subs = append(subs, sub)
	}
	return entries, subs, nil
}

func (o *owner) deliverTransient(entries []syncengine.TransientEvent, subs []*syncengine.TransientSubscription) {
	for _, entry := range entries {
		for _, sub := range subs {
			if !sub.Offer(entry) {
				o.removeTransientSub(sub)
			}
		}
	}
}

func (o *owner) desyncTransient(err error) {
	o.mu.Lock()
	if o.transientRun != nil {
		o.transientRun.desynced = true
	}
	subs := make([]*syncengine.TransientSubscription, 0, len(o.transientSubs))
	for sub := range o.transientSubs {
		subs = append(subs, sub)
	}
	o.mu.Unlock()
	for _, sub := range subs {
		sub.Desync(err)
	}
}

func (o *owner) removeTransientSub(sub *syncengine.TransientSubscription) {
	if sub == nil {
		return
	}
	o.mu.Lock()
	if _, ok := o.transientSubs[sub]; ok {
		delete(o.transientSubs, sub)
	}
	// Close even when invalidation already removed the map entry. Desync is a
	// terminal signal, but the transient delivery channel must still be closed
	// when the owning subscription is torn down.
	sub.Close()
	o.mu.Unlock()
}

func (o *owner) applyProjection(next projection) {
	o.mu.Lock()
	if o.closed || len(o.subs) == 0 {
		o.stale = true
		o.mu.Unlock()
		return
	}
	next.snapshot = o.snapshotWithTransientLocked(next.snapshot)
	previous := o.projection
	ops, err := diff(previous.snapshot, next.snapshot, next.revision)
	if err != nil {
		o.mu.Unlock()
		o.invalidate(err)
		return
	}
	if len(ops) == 0 && previous.revision == next.revision {
		o.mu.Unlock()
		return
	}
	change := syncengine.ResourceChange{ResourceRevision: next.revision, Operations: ops}
	nextSequence := o.journal.LastSequence() + 1
	if err := validateChangeSize(change, o.sessionID, nextSequence, o.provider.options.MaxChangeMessageBytes); err != nil {
		o.mu.Unlock()
		o.invalidate(err)
		return
	}
	entry, err := o.journal.Append(change)
	if err != nil {
		o.mu.Unlock()
		o.invalidate(err)
		return
	}
	o.projection, o.initialized, o.stale, o.invalid = next, true, false, false
	subs := make([]*syncengine.LiveSubscription, 0, len(o.subs))
	for sub := range o.subs {
		subs = append(subs, sub)
	}
	o.lastUsed = time.Now()
	o.mu.Unlock()
	for _, sub := range subs {
		if !sub.Offer(entry) {
			o.removeSub(sub)
		}
	}
}

func (o *owner) deleteResource() {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	subs := make([]*syncengine.LiveSubscription, 0, len(o.subs))
	for sub := range o.subs {
		subs = append(subs, sub)
		delete(o.subs, sub)
		o.subscriberHint.Add(-1)
	}
	transientSubs := make([]*syncengine.TransientSubscription, 0, len(o.transientSubs))
	for sub := range o.transientSubs {
		transientSubs = append(transientSubs, sub)
		delete(o.transientSubs, sub)
	}
	o.invalid, o.stale = true, true
	o.mu.Unlock()
	for _, sub := range subs {
		sub.Desync(sessions.ErrNotFound)
	}
	for _, sub := range transientSubs {
		sub.Desync(sessions.ErrNotFound)
	}
}

func (o *owner) invalidate(err error) {
	if err == nil {
		err = ErrProviderInvalid
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.invalid, o.stale = true, true
	if o.transientRun != nil {
		o.transientRun.desynced = true
	}
	subs := make([]*syncengine.LiveSubscription, 0, len(o.subs))
	for sub := range o.subs {
		subs = append(subs, sub)
		delete(o.subs, sub)
		o.subscriberHint.Add(-1)
	}
	transientSubs := make([]*syncengine.TransientSubscription, 0, len(o.transientSubs))
	for sub := range o.transientSubs {
		transientSubs = append(transientSubs, sub)
		delete(o.transientSubs, sub)
	}
	o.pendingOpens = 0
	o.openInterest.Store(0)
	o.subscriberHint.Store(0)
	o.mu.Unlock()
	for _, sub := range subs {
		sub.Desync(err)
	}
	for _, sub := range transientSubs {
		sub.Desync(err)
	}
}

func (o *owner) removeSub(sub *syncengine.LiveSubscription) {
	if o == nil || sub == nil {
		return
	}
	o.mu.Lock()
	if _, ok := o.subs[sub]; ok {
		delete(o.subs, sub)
		o.subscriberHint.Add(-1)
	}
	o.lastUsed = time.Now()
	o.mu.Unlock()
}

func (o *owner) resetJournalLocked() error {
	o.provider.generation.Add(1)
	epoch := fmt.Sprintf("%s:%d", o.epoch, o.provider.generation.Load())
	if err := o.journal.Reset(epoch); err != nil {
		if errors.Is(err, syncengine.ErrEpochUnchanged) {
			return nil
		}
		return err
	}
	o.epoch = epoch
	return nil
}

func (o *owner) close() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	subs := make([]*syncengine.LiveSubscription, 0, len(o.subs))
	for sub := range o.subs {
		subs = append(subs, sub)
		delete(o.subs, sub)
		o.subscriberHint.Add(-1)
	}
	transientSubs := make([]*syncengine.TransientSubscription, 0, len(o.transientSubs))
	for sub := range o.transientSubs {
		transientSubs = append(transientSubs, sub)
		delete(o.transientSubs, sub)
	}
	o.pendingOpens = 0
	o.openInterest.Store(0)
	o.subscriberHint.Store(0)
	o.mu.Unlock()
	for _, sub := range subs {
		sub.Close()
	}
	for _, sub := range transientSubs {
		sub.Close()
	}
	o.cancel()
	select {
	case <-o.workerDone:
	case <-time.After(2 * time.Second):
	}
}

func cloneResume(token *protocol.ResumeToken) *protocol.ResumeToken {
	if token == nil {
		return nil
	}
	copy := *token
	return &copy
}

func cloneRunResume(token *protocol.RunResumeToken) *protocol.RunResumeToken {
	if token == nil {
		return nil
	}
	copy := *token
	return &copy
}

type itemBlobCache struct {
	mu     sync.Mutex
	max    int
	values map[string]protocol.BlobDescriptor
	order  []string
}

func newItemBlobCache(max int) *itemBlobCache {
	return &itemBlobCache{max: max, values: make(map[string]protocol.BlobDescriptor)}
}

func (c *itemBlobCache) get(key string, now time.Time, refreshSkew time.Duration) (protocol.BlobDescriptor, bool, bool) {
	if c == nil {
		return protocol.BlobDescriptor{}, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	descriptor, ok := c.values[key]
	if !ok {
		return protocol.BlobDescriptor{}, false, false
	}
	expires, err := time.Parse(time.RFC3339Nano, descriptor.ExpiresAt)
	if err != nil || !now.Before(expires) || !now.Add(refreshSkew).Before(expires) {
		delete(c.values, key)
		for index, existing := range c.order {
			if existing == key {
				copy(c.order[index:], c.order[index+1:])
				c.order = c.order[:len(c.order)-1]
				break
			}
		}
		return descriptor, false, true
	}
	c.touchLocked(key)
	return descriptor, true, false
}

func (c *itemBlobCache) put(key string, descriptor protocol.BlobDescriptor) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] = descriptor
	c.touchLocked(key)
	for len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.values, oldest)
	}
}

func (c *itemBlobCache) touchLocked(key string) {
	for index, existing := range c.order {
		if existing == key {
			copy(c.order[index:], c.order[index+1:])
			c.order = c.order[:len(c.order)-1]
			break
		}
	}
	c.order = append(c.order, key)
}

func (p *Provider) buildProjection(ctx context.Context, sessionID string, blobCache *itemBlobCache) (projection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return projection{}, err
		}
		state, err := p.store.LoadState(sessionID)
		if err != nil {
			return projection{}, err
		}
		if state.LastSeq < 0 {
			return projection{}, fmt.Errorf("session %q has negative LastSeq", sessionID)
		}
		if p.options.BeforeSnapshot != nil {
			p.options.BeforeSnapshot(sessionID)
		}
		page, err := p.store.ReadHistoryPage(sessionID, sessions.HistoryPageOptions{Limit: p.options.HistoryLimit, VisibleOnly: true})
		if err != nil {
			return projection{}, err
		}
		check, err := p.store.LoadState(sessionID)
		if err != nil {
			return projection{}, err
		}
		if state.LastSeq != check.LastSeq || !state.UpdatedAt.Equal(check.UpdatedAt) {
			lastErr = ErrSnapshotRace
			continue
		}
		history := HistoryWindow{Items: make([]Item, 0, len(page.Items)), Descriptor: HistoryWindowDescriptor{Limit: p.options.HistoryLimit, AlignTurn: false, VisibleOnly: true, HasMoreBefore: page.HasMoreBefore, HasMoreAfter: page.HasMoreAfter}}
		for _, storeItem := range page.Items {
			item, itemErr := p.projectItem(ctx, sessionID, storeItem, state.ShowReasoning, blobCache)
			if itemErr != nil {
				return projection{}, itemErr
			}
			history.Items = append(history.Items, item)
		}
		if len(history.Items) > 0 {
			history.Descriptor.OldestItemSeq = strconv.FormatInt(history.Items[0].Seq, 10)
			history.Descriptor.NewestItemSeq = strconv.FormatInt(history.Items[len(history.Items)-1].Seq, 10)
		}
		snapshot := Snapshot{SchemaVersion: SchemaVersion, Session: sessionMetadataFromState(state), History: history, Compaction: compactionStateFromSession(state, p.options.MaxCompactionRecords)}
		if state.RunningRunID != "" {
			runID := state.RunningRunID
			startedAt := state.RunningStartedAt
			// RunningStartedAt is also used by the legacy compact state as the
			// current turn timestamp and is cleared between multi-turn requests.
			// The active-run descriptor needs the durable run timestamp, so fall
			// back to the run row rather than emitting an invalid zero time.
			if startedAt.IsZero() {
				runs, runErr := p.store.ListRuns(sessionID)
				if runErr != nil {
					return projection{}, runErr
				}
				for _, run := range runs {
					if run.ID == runID {
						startedAt = run.StartedAt
						break
					}
				}
			}
			if startedAt.IsZero() {
				return projection{}, fmt.Errorf("active run %q has no durable started_at", runID)
			}
			snapshot.ActiveRun = &ActiveRunDescriptor{RunID: runID, SessionID: state.ID, TurnID: state.RunningTurnID, StartedAt: startedAt, Status: sessions.RunStatusRunning, Recoverable: runID != "", RecoveryRequired: true}
		}
		if err := snapshot.Validate(); err != nil {
			return projection{}, err
		}
		return projection{snapshot: snapshot, revision: protocol.ResourceRevision(strconv.FormatInt(state.LastSeq, 10))}, nil
	}
	if lastErr != nil {
		return projection{}, lastErr
	}
	return projection{}, ErrSnapshotRace
}

func (p *Provider) projectItem(ctx context.Context, sessionID string, item sessions.SessionItem, showReasoning bool, blobCache *itemBlobCache) (Item, error) {
	out := Item{Key: ItemKey{TurnID: item.TurnID, AgentIteration: item.AgentIteration, ItemID: item.ID}, Seq: item.Seq, CreatedAt: item.CreatedAt, Kind: item.Kind, Visibility: item.Visibility, Audience: item.Audience, Status: item.Status}
	if item.Message == nil {
		return out, nil
	}
	message := &ItemMessage{Role: string(item.Message.Role), ToolCallID: item.Message.ToolCallID, IsError: item.Message.IsError}
	if showReasoning && item.Message.Role == model.MessageRoleAssistant {
		if item.Message.ReasoningContent != "" {
			reasoning, err := p.textContent(ctx, blobCache, out.Key, "reasoning", item.Message.ReasoningContent, "text/plain; charset=utf-8", "")
			if err != nil {
				return Item{}, err
			}
			message.Reasoning = reasoning
		}
	}
	for _, call := range item.Message.ToolCalls {
		arguments, err := p.textContent(ctx, blobCache, out.Key, "tool-arguments-"+call.ID, call.Arguments, "application/json", "")
		if err != nil {
			return Item{}, err
		}
		message.ToolCalls = append(message.ToolCalls, ToolCall{ID: call.ID, Name: call.Name, Arguments: arguments})
	}
	for _, block := range item.Message.ContentBlocks {
		if block.Type == "input_image" && block.ImageBlob != nil {
			message.Images = append(message.Images, ImageAttachment{Hash: block.ImageBlob.Hash, MediaType: block.ImageBlob.MediaType, SizeBytes: block.ImageBlob.SizeBytes})
		}
	}
	content, preview, err := p.displayContent(ctx, sessionID, item)
	if err != nil {
		return Item{}, err
	}
	if content != "" || preview != "" {
		message.Content, err = p.textContent(ctx, blobCache, out.Key, "content", content, "text/plain; charset=utf-8", preview)
		if err != nil {
			return Item{}, err
		}
	}
	out.Message = message
	return out, nil
}

func (p *Provider) displayContent(ctx context.Context, sessionID string, item sessions.SessionItem) (string, string, error) {
	if item.Message != nil {
		if item.Message.Content != "" {
			return item.Message.Content, "", nil
		}
		var blocks strings.Builder
		for _, block := range item.Message.ContentBlocks {
			if block.Text != "" {
				blocks.WriteString(block.Text)
			}
		}
		if blocks.Len() > 0 {
			return blocks.String(), "", nil
		}
	}
	if item.Content == nil {
		return "", "", nil
	}
	if item.Content.Inline != "" {
		return item.Content.Inline, "", nil
	}
	if item.Content.Blob != nil {
		ref := *item.Content.Blob
		raw, err := p.store.ReadBlobForSession(sessionID, ref)
		if err != nil {
			return "", "", err
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		default:
		}
		if !utf8.Valid(raw) && item.Message != nil && item.Message.Role == model.MessageRoleTool && strings.EqualFold(strings.TrimSpace(ref.Encoding), "utf-8") && strings.EqualFold(strings.TrimSpace(ref.MediaType), "text/plain") {
			// Some historical Windows tool results contain console-code-page
			// bytes despite being labelled UTF-8. Keep the durable blob intact,
			// but make this narrowly scoped presentation projection valid so one
			// malformed tool result cannot prevent the session from opening.
			return strings.ToValidUTF8(string(raw), "\uFFFD"), "", nil
		}
		return string(raw), "", nil
	}
	return "", item.Content.Preview, nil
}

func (p *Provider) putBlob(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error) {
	if p.options.BlobWriter == nil {
		return protocol.BlobDescriptor{}, fmt.Errorf("BlobWriter is not configured")
	}
	descriptor, err := p.options.BlobWriter.Put(ctx, contentType, content)
	if err != nil {
		return protocol.BlobDescriptor{}, err
	}
	if err := protocol.ValidateBlobDescriptor(descriptor); err != nil {
		return protocol.BlobDescriptor{}, err
	}
	if descriptor.ContentType != contentType {
		return protocol.BlobDescriptor{}, fmt.Errorf("blob content type %q does not match requested %q", descriptor.ContentType, contentType)
	}
	if !blobDescriptorUsable(descriptor, p.options.Now(), p.options.BlobRefreshSkew) {
		fresh, ok := p.options.BlobWriter.(FreshBlobWriter)
		if !ok {
			return protocol.BlobDescriptor{}, fmt.Errorf("blob descriptor is expired or too close to expiry and writer cannot refresh it")
		}
		descriptor, err = fresh.PutFresh(ctx, contentType, content)
		if err != nil {
			return protocol.BlobDescriptor{}, err
		}
		if err := protocol.ValidateBlobDescriptor(descriptor); err != nil {
			return protocol.BlobDescriptor{}, err
		}
		if descriptor.ContentType != contentType || !blobDescriptorUsable(descriptor, p.options.Now(), p.options.BlobRefreshSkew) {
			return protocol.BlobDescriptor{}, fmt.Errorf("fresh blob descriptor is invalid or expired")
		}
	}
	return descriptor, nil
}

func blobDescriptorUsable(descriptor protocol.BlobDescriptor, now time.Time, refreshSkew time.Duration) bool {
	expires, err := time.Parse(time.RFC3339Nano, descriptor.ExpiresAt)
	if err != nil {
		return false
	}
	return now.Add(refreshSkew).Before(expires)
}

func (p *Provider) textContent(ctx context.Context, cache *itemBlobCache, key ItemKey, suffix, value, contentType, preview string) (*TextContent, error) {
	resolvedContentType := contentTypeForText(contentType, value)
	if preview != "" && !utf8.ValidString(preview) {
		return nil, fmt.Errorf("%s preview is not valid UTF-8", suffix)
	}
	if value == "" {
		if preview == "" {
			return nil, nil
		}
		return &TextContent{Preview: textPreview("", preview), ContentType: resolvedContentType}, nil
	}
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("%s text is not valid UTF-8", suffix)
	}
	if len(value) <= p.options.MaxItemContentBytes {
		return &TextContent{Inline: value, ContentType: resolvedContentType}, nil
	}
	if p.options.BlobWriter == nil {
		return nil, fmt.Errorf("%s exceeds inline limit and BlobWriter is not configured", suffix)
	}
	digest := sha256.Sum256([]byte(value))
	cacheKey := fmt.Sprintf("%s:%d:%s:%s", key.String(), len(value), hex.EncodeToString(digest[:]), suffix)
	cachedDescriptor, cached, refresh := cache.get(cacheKey, p.options.Now().UTC(), p.options.BlobRefreshSkew)
	if cached {
		return &TextContent{Preview: textPreview(value, preview), Blob: &cachedDescriptor, ContentType: cachedDescriptor.ContentType}, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	var descriptor protocol.BlobDescriptor
	var err error
	if refresh {
		if fresh, ok := p.options.BlobWriter.(FreshBlobWriter); ok {
			descriptor, err = fresh.PutFresh(ctx, resolvedContentType, []byte(value))
		} else {
			descriptor, err = p.options.BlobWriter.Put(ctx, resolvedContentType, []byte(value))
		}
	} else {
		descriptor, err = p.options.BlobWriter.Put(ctx, resolvedContentType, []byte(value))
	}
	if err != nil {
		return nil, fmt.Errorf("store %s item blob: %w", suffix, err)
	}
	if err := protocol.ValidateBlobDescriptor(descriptor); err != nil {
		return nil, fmt.Errorf("invalid %s item blob descriptor: %w", suffix, err)
	}
	if descriptor.ContentType != resolvedContentType || !blobDescriptorUsable(descriptor, p.options.Now(), p.options.BlobRefreshSkew) {
		return nil, fmt.Errorf("store %s item blob returned an expired or mismatched descriptor", suffix)
	}
	cache.put(cacheKey, descriptor)
	return &TextContent{Preview: textPreview(value, preview), Blob: &descriptor, ContentType: resolvedContentType}, nil
}

func contentTypeForText(requested, value string) string {
	if requested == "application/json" && !json.Valid([]byte(value)) {
		return "text/plain; charset=utf-8"
	}
	return requested
}

func textPreview(value, preview string) string {
	if preview != "" {
		if !utf8.ValidString(preview) {
			return ""
		}
		return preview[:utf8SafePrefix(preview, 240)]
	}
	if len(value) <= 240 {
		return value
	}
	result := value[:utf8SafePrefix(value, 240)]
	return result
}

func utf8SafePrefix(value string, maxBytes int) int {
	if maxBytes >= len(value) {
		return len(value)
	}
	last := 0
	for index := range value {
		if index > maxBytes {
			break
		}
		last = index
	}
	return last
}

func diff(old, next Snapshot, revision protocol.ResourceRevision) ([]protocol.ChangeOperation, error) {
	ops := make([]protocol.ChangeOperation, 0, 8)
	if !reflectEqual(old.Session, next.Session) {
		op, err := Operation(OpMetadataReplace, struct {
			Metadata SessionMetadata `json:"metadata"`
		}{next.Session})
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	oldItems := make(map[ItemKey]Item, len(old.History.Items))
	for _, item := range old.History.Items {
		oldItems[item.Key] = item
	}
	nextItems := make(map[ItemKey]Item, len(next.History.Items))
	for _, item := range next.History.Items {
		nextItems[item.Key] = item
	}
	oldOrder := make([]ItemKey, 0, len(old.History.Items))
	for _, item := range old.History.Items {
		oldOrder = append(oldOrder, item.Key)
	}
	for _, key := range oldOrder {
		if _, ok := nextItems[key]; !ok {
			op, err := Operation(OpItemRemove, struct {
				Key ItemKey `json:"key"`
			}{key})
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
		}
	}
	for _, item := range next.History.Items {
		before, ok := oldItems[item.Key]
		if !ok || !reflectEqual(before, item) {
			op, err := Operation(OpItemUpsert, struct {
				Item Item `json:"item"`
			}{item})
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
		}
	}
	// Item operations never carry the whole window. The descriptor operation
	// is a bounded, atomic replacement of only the window metadata; clients
	// apply it after the item upserts/removes in this one Change. Thus clients
	// do not infer oldest/newest/has_more from array positions, and a window
	// slide remains exactly reproducible from the stable item key plus this
	// descriptor. The old history.window.replace name is retained in the
	// schema only for forward compatibility with pre-D1 producers and is never
	// emitted by this provider.
	if !reflectEqual(old.History.Descriptor, next.History.Descriptor) {
		op, err := Operation(OpHistoryDescriptorReplace, struct {
			Descriptor HistoryWindowDescriptor `json:"descriptor"`
		}{next.History.Descriptor})
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if !reflectEqual(old.Compaction, next.Compaction) {
		op, err := Operation(OpCompactionReplace, struct {
			Compaction CompactionState `json:"compaction"`
		}{next.Compaction})
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if !reflectEqual(old.ActiveRun, next.ActiveRun) {
		var op protocol.ChangeOperation
		var err error
		if next.ActiveRun == nil {
			op, err = Operation(OpActiveRunClear, struct{}{})
		} else {
			op, err = Operation(OpActiveRunReplace, struct {
				ActiveRun *ActiveRunDescriptor `json:"active_run"`
			}{next.ActiveRun})
		}
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		op, err := Operation(OpMetadataReplace, struct {
			Metadata SessionMetadata `json:"metadata"`
		}{next.Session})
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	_ = revision
	return ops, nil
}

func validateChangeSize(change syncengine.ResourceChange, resourceID string, nextSequence uint64, maxBytes int) error {
	if err := change.Validate(); err != nil {
		return err
	}
	// The gateway adds every field below after the provider has appended its
	// journal entry. Measure the complete typed wire envelope now, using the
	// largest legal envelope IDs and sequence/revision strings. This is
	// intentionally conservative because subscription ID and message ID are
	// chosen outside this provider. The same protocol identifier bounds are
	// enforced by DecodeMessage, so this preflight is a real upper bound rather
	// than a journal-only estimate.
	message := protocol.ChangeMessage{
		Envelope: protocol.Envelope{
			Version: 1,
			Type:    protocol.MessageTypeChange,
			ID:      strings.Repeat("m", protocol.MaxWireIdentifierBytes),
		},
		Payload: protocol.ChangePayload{
			SubscriptionID:   strings.Repeat("s", protocol.MaxWireIdentifierBytes),
			Resource:         protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: resourceID},
			StreamEpoch:      strings.Repeat("e", protocol.MaxWireIdentifierBytes),
			Sequence:         protocol.Sequence(strconv.FormatUint(nextSequence, 10)),
			PreviousSequence: protocol.Sequence(strconv.FormatUint(nextSequence-1, 10)),
			ResourceRevision: protocol.ResourceRevision(strings.Repeat("r", protocol.MaxWireIdentifierBytes)),
			Operations:       change.Operations,
		},
	}
	encoded, err := protocol.EncodeMessage(message)
	if err != nil {
		return fmt.Errorf("encode complete session content change: %w", err)
	}
	if len(encoded) > maxBytes {
		return fmt.Errorf("session content change exceeds message bound: %d > %d bytes", len(encoded), maxBytes)
	}
	return nil
}

func hasItemOperations(operations []protocol.ChangeOperation) bool {
	for _, operation := range operations {
		if operation.Op == OpItemUpsert || operation.Op == OpItemRemove {
			return true
		}
	}
	return false
}

func reflectEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}
