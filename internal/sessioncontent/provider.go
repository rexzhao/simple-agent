package sessioncontent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

const (
	DefaultJournalEntries       = 4096
	DefaultJournalBytes         = 8 * 1024 * 1024
	DefaultLiveCapacity         = 256
	DefaultProjectorQueue       = 256
	DefaultInlineSnapshot       = 64 * 1024
	DefaultHistoryLimit         = 50
	DefaultMaxCompactionRecords = 64
	DefaultMaxItemContentBytes  = 64 * 1024
	DefaultMaxItemBlobs         = 256
	DefaultMaxOwners            = 1024
	DefaultBlobRefreshSkew      = time.Minute
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
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.JournalEntries <= 0 || o.JournalBytes <= 0 || o.LiveCapacity <= 0 || o.ProjectorQueueCapacity <= 0 ||
		o.InlineSnapshotBytes <= 0 || o.HistoryLimit <= 0 || o.HistoryLimit > 1000 || o.MaxCompactionRecords <= 0 ||
		o.MaxItemContentBytes <= 0 || o.MaxItemBlobs <= 0 || o.MaxOwners <= 0 || o.MaxChangeMessageBytes <= 0 ||
		o.MaxChangeMessageBytes > o.JournalBytes || o.BlobRefreshSkew < 0 {
		return ProviderOptions{}, fmt.Errorf("session content bounds are invalid")
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
	ctx        context.Context
	cancel     context.CancelFunc
	workerDone chan struct{}

	mu          sync.Mutex
	projection  projection
	initialized bool
	stale       bool
	invalid     bool
	closed      bool
	subs        map[*syncengine.LiveSubscription]struct{}
	lastUsed    time.Time
	blobs       *itemBlobCache
	claims      atomic.Int64
}

type projection struct {
	snapshot Snapshot
	revision protocol.ResourceRevision
}

type ownerTask struct {
	mutation *sessions.Mutation
	open     *openRequest
	stop     bool
}

type openRequest struct {
	ctx    context.Context
	resume *protocol.ResumeToken
	result chan openResult
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
	return o.enqueue(ownerTask{mutation: &mutation})
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
	request := &openRequest{ctx: ctx, resume: cloneResume(resume), result: make(chan openResult, 1)}
	if err := o.enqueue(ownerTask{open: request}); err != nil {
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
	journal, err := syncengine.NewBoundedJournal(epoch, p.options.JournalEntries, p.options.JournalBytes)
	if err != nil {
		cancel()
		return nil, err
	}
	o := &owner{provider: p, sessionID: sessionID, epoch: epoch, journal: journal, queue: make(chan ownerTask, p.options.ProjectorQueueCapacity), ctx: ctx, cancel: cancel, workerDone: make(chan struct{}), subs: make(map[*syncengine.LiveSubscription]struct{}), blobs: newItemBlobCache(p.options.MaxItemBlobs)}
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
	select {
	case o.queue <- task:
		return nil
	default:
		o.invalidate(ErrQueueFull)
		return ErrQueueFull
	}
}

func (o *owner) run() {
	defer close(o.workerDone)
	for {
		select {
		case <-o.ctx.Done():
			return
		case task := <-o.queue:
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
		}
	}
}

func (o *owner) handleOpen(request *openRequest) {
	if request == nil {
		return
	}
	opened, err := o.openBarrier(request.ctx, request.resume)
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

func (o *owner) openBarrier(ctx context.Context, resume *protocol.ResumeToken) (syncengine.OpenedResource, error) {
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
		if reset {
			if err := o.resetJournalLocked(); err != nil {
				o.mu.Unlock()
				return syncengine.OpenedResource{}, err
			}
		}
		if !o.initialized || o.stale || o.invalid {
			o.projection = built
			o.initialized, o.stale, o.invalid = true, false, false
		}
		o.mu.Unlock()
	}
	encoded, content, revision, epoch, sequence, decision, live, sub, err := o.captureOpen(ctx, resume)
	if err != nil {
		return syncengine.OpenedResource{}, err
	}
	if len(encoded) > o.provider.options.InlineSnapshotBytes {
		if o.provider.options.BlobWriter == nil {
			sub.Close()
			o.removeSub(sub)
			return syncengine.OpenedResource{}, fmt.Errorf("session content snapshot is %d bytes and no blob writer is configured", len(encoded))
		}
		blobContext, finishBlob := o.requestContext(ctx)
		descriptor, blobErr := o.provider.putBlob(blobContext, "application/json", encoded)
		finishBlob()
		if blobErr != nil {
			sub.Close()
			o.removeSub(sub)
			return syncengine.OpenedResource{}, fmt.Errorf("store session content snapshot blob: %w", blobErr)
		}
		content = syncengine.NewBlobSnapshotContent(descriptor)
	}
	return syncengine.OpenedResource{
		Snapshot:    syncengine.Snapshot{Content: content, ResourceRevision: revision},
		StreamEpoch: epoch, Sequence: sequence, Decision: decision, LiveFromSequence: sequence + 1,
		Changes: live.Entries, Terminal: live.Terminal,
		Close: func() { sub.Close(); o.removeSub(sub) },
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

func (o *owner) captureOpen(ctx context.Context, resume *protocol.ResumeToken) ([]byte, syncengine.SnapshotContent, protocol.ResourceRevision, string, uint64, syncengine.ResumeDecision, syncengine.LiveDelivery, *syncengine.LiveSubscription, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, syncengine.SnapshotContent{}, "", "", 0, syncengine.ResumeDecision{}, syncengine.LiveDelivery{}, nil, ErrProviderClosed
	}
	if !o.initialized || o.invalid {
		return nil, syncengine.SnapshotContent{}, "", "", 0, syncengine.ResumeDecision{}, syncengine.LiveDelivery{}, nil, ErrProviderInvalid
	}
	if err := o.projection.snapshot.Validate(); err != nil {
		return nil, syncengine.SnapshotContent{}, "", "", 0, syncengine.ResumeDecision{}, syncengine.LiveDelivery{}, nil, err
	}
	encoded, err := json.Marshal(o.projection.snapshot)
	if err != nil {
		return nil, syncengine.SnapshotContent{}, "", "", 0, syncengine.ResumeDecision{}, syncengine.LiveDelivery{}, nil, err
	}
	sequence := o.journal.LastSequence()
	epoch := o.journal.Epoch()
	sub, err := syncengine.NewLiveSubscription(epoch, sequence, o.provider.options.LiveCapacity)
	if err != nil {
		return nil, syncengine.SnapshotContent{}, "", "", 0, syncengine.ResumeDecision{}, syncengine.LiveDelivery{}, nil, err
	}
	o.subs[sub] = struct{}{}
	o.lastUsed = time.Now()
	decision := o.journal.Decide(resume)
	return encoded, syncengine.NewInlineSnapshotContent(encoded), o.projection.revision, epoch, sequence, decision, sub.Delivery(), sub, nil
}

func (o *owner) handleMutation(mutation sessions.Mutation) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	if len(o.subs) == 0 {
		if o.initialized {
			o.stale = true
			_ = o.resetJournalLocked()
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

func (o *owner) applyProjection(next projection) {
	o.mu.Lock()
	if o.closed || len(o.subs) == 0 {
		o.stale = true
		o.mu.Unlock()
		return
	}
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
	}
	o.invalid, o.stale = true, true
	o.mu.Unlock()
	for _, sub := range subs {
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
	subs := make([]*syncengine.LiveSubscription, 0, len(o.subs))
	for sub := range o.subs {
		subs = append(subs, sub)
		delete(o.subs, sub)
	}
	o.mu.Unlock()
	for _, sub := range subs {
		sub.Desync(err)
	}
}

func (o *owner) removeSub(sub *syncengine.LiveSubscription) {
	if o == nil || sub == nil {
		return
	}
	o.mu.Lock()
	delete(o.subs, sub)
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
	}
	o.mu.Unlock()
	for _, sub := range subs {
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
			snapshot.ActiveRun = &ActiveRunDescriptor{RunID: runID, SessionID: state.ID, TurnID: state.RunningTurnID, StartedAt: state.RunningStartedAt, Status: sessions.RunStatusRunning, Recoverable: runID != ""}
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
		raw, err := p.store.ReadBlobForSession(sessionID, *item.Content.Blob)
		if err != nil {
			return "", "", err
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		default:
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
