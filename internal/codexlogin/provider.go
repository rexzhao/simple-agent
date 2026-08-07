package codexlogin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

const (
	defaultJournalEntries   = 256
	defaultJournalBytes     = 512 * 1024
	defaultLiveCapacity     = 64
	defaultQueueCapacity    = 128
	defaultMaxSnapshotBytes = 32 * 1024
	defaultMaxChangeBytes   = 32 * 1024
)

type ProviderOptions struct {
	StreamEpoch            string
	OwnerContext           context.Context
	JournalEntries         int
	JournalBytes           int
	LiveCapacity           int
	ProjectorQueueCapacity int
	MaxSnapshotBytes       int
	MaxChangeMessageBytes  int
	ValidateProvider       func(string) error
	Status                 func(string) (execution.CodexAuthStatus, error)
}

func (o ProviderOptions) withDefaults() (ProviderOptions, error) {
	if strings.TrimSpace(o.StreamEpoch) == "" {
		o.StreamEpoch = "codex-login"
	}
	if o.OwnerContext == nil {
		o.OwnerContext = context.Background()
	}
	if o.JournalEntries == 0 {
		o.JournalEntries = defaultJournalEntries
	}
	if o.JournalBytes == 0 {
		o.JournalBytes = defaultJournalBytes
	}
	if o.LiveCapacity == 0 {
		o.LiveCapacity = defaultLiveCapacity
	}
	if o.ProjectorQueueCapacity == 0 {
		o.ProjectorQueueCapacity = defaultQueueCapacity
	}
	if o.MaxSnapshotBytes == 0 {
		o.MaxSnapshotBytes = defaultMaxSnapshotBytes
	}
	if o.MaxChangeMessageBytes == 0 {
		o.MaxChangeMessageBytes = defaultMaxChangeBytes
	}
	if o.JournalEntries <= 0 || o.JournalBytes <= 0 || o.LiveCapacity <= 0 || o.ProjectorQueueCapacity <= 0 || o.MaxSnapshotBytes <= 0 || o.MaxChangeMessageBytes <= 0 {
		return ProviderOptions{}, fmt.Errorf("Codex login resource bounds must be positive")
	}
	if o.ValidateProvider == nil || o.Status == nil {
		return ProviderOptions{}, fmt.Errorf("Codex login resource callbacks are required")
	}
	return o, nil
}

// Provider owns one bounded journal per authorized provider identity. It is
// deliberately lazy: opening a resource establishes the current snapshot and
// creates its stream, while a publication for an already-open identity is
// delivered through that identity's owner queue.
type Provider struct {
	options ProviderOptions

	mu        sync.Mutex
	owners    map[string]*owner
	closed    bool
	closeOnce sync.Once
}

type owner struct {
	provider *Provider
	identity string
	epoch    string
	ctx      context.Context
	cancel   context.CancelFunc
	queue    chan ownerTask
	done     chan struct{}

	mu               sync.Mutex
	resourceRevision uint64
	snapshot         Snapshot
	initialized      bool
	invalid          bool
	lastError        error
	closed           bool
	journal          *syncengine.Journal
	subs             map[*syncengine.LiveSubscription]struct{}
}

type ownerTask struct {
	refresh bool
	open    *openRequest
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

func NewProvider(options ProviderOptions) (*Provider, error) {
	resolved, err := options.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Provider{options: resolved, owners: make(map[string]*owner)}, nil
}

func (p *Provider) Type() protocol.ResourceType { return protocol.ResourceTypeCodexLogin }

func (p *Provider) Authorize(_ context.Context, principal syncengine.Principal, key protocol.ResourceKey) error {
	if p == nil || p.isClosed() {
		return ErrProviderUnavailable
	}
	if key.Type != protocol.ResourceTypeCodexLogin || !validProviderIdentity(key.ID) {
		return ErrProviderInvalid
	}
	if strings.TrimSpace(principal.ID) == "" {
		return ErrProviderUnavailable
	}
	if err := p.options.ValidateProvider(key.ID); err != nil {
		return normalizeValidationError(err)
	}
	return nil
}

func (p *Provider) Open(ctx context.Context, key protocol.ResourceKey, resume *protocol.ResumeToken) (syncengine.OpenedResource, error) {
	if p == nil || p.isClosed() {
		return syncengine.OpenedResource{}, ErrProviderUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if key.Type != protocol.ResourceTypeCodexLogin || !validProviderIdentity(key.ID) {
		return syncengine.OpenedResource{}, ErrProviderInvalid
	}
	if err := ctx.Err(); err != nil {
		return syncengine.OpenedResource{}, err
	}
	o, err := p.ownerFor(key.ID)
	if err != nil {
		return syncengine.OpenedResource{}, err
	}
	request := &openRequest{ctx: ctx, resume: resume, result: make(chan openResult, 1)}
	if err := o.enqueue(ctx, ownerTask{open: request}); err != nil {
		return syncengine.OpenedResource{}, err
	}
	select {
	case result := <-request.result:
		return result.opened, result.err
	case <-ctx.Done():
		return syncengine.OpenedResource{}, ctx.Err()
	case <-o.done:
		return syncengine.OpenedResource{}, ErrProviderUnavailable
	}
}

// PublishCommitted is intentionally nonblocking. The publication carries
// only an identity; the owner reloads status through the registry callback in
// its own serialization domain, so no auth material can enter the journal.
func (p *Provider) PublishCommitted(change CommittedChange) error {
	if p == nil || p.isClosed() {
		return ErrProviderUnavailable
	}
	if !validProviderIdentity(change.Provider) {
		return ErrProviderInvalid
	}
	o, err := p.ownerFor(change.Provider)
	if err != nil {
		return err
	}
	if err := o.enqueueNonblocking(ownerTask{refresh: true}); err != nil {
		o.invalidate("projector_queue_full")
		return err
	}
	return nil
}

func (p *Provider) ownerFor(identity string) (*owner, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrProviderUnavailable
	}
	if current := p.owners[identity]; current != nil {
		return current, nil
	}
	ctx, cancel := context.WithCancel(p.options.OwnerContext)
	// Provider identities are allowed to contain Unicode and spaces. A digest
	// keeps them out of the epoch syntax while retaining one epoch per owner.
	epoch := fmt.Sprintf("%s/%x", p.options.StreamEpoch, identityDigest(identity))
	journal, err := syncengine.NewBoundedJournal(epoch, p.options.JournalEntries, p.options.JournalBytes)
	if err != nil {
		cancel()
		return nil, err
	}
	o := &owner{
		provider: p, identity: identity, epoch: epoch, ctx: ctx, cancel: cancel,
		queue: make(chan ownerTask, p.options.ProjectorQueueCapacity), done: make(chan struct{}),
		journal: journal, subs: make(map[*syncengine.LiveSubscription]struct{}),
	}
	p.owners[identity] = o
	go o.run()
	return o, nil
}

func identityDigest(identity string) [32]byte {
	return sha256.Sum256([]byte(identity))
}

func (p *Provider) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (o *owner) enqueue(ctx context.Context, task ownerTask) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-o.done:
		return ErrProviderUnavailable
	case <-ctx.Done():
		return ctx.Err()
	case o.queue <- task:
		return nil
	}
}

func (o *owner) enqueueNonblocking(task ownerTask) error {
	select {
	case <-o.done:
		return ErrProviderUnavailable
	default:
	}
	select {
	case o.queue <- task:
		return nil
	default:
		return errors.New("Codex login projector queue is full")
	}
}

func (o *owner) run() {
	defer close(o.done)
	for {
		select {
		case <-o.ctx.Done():
			o.closeState()
			return
		case task := <-o.queue:
			if task.open != nil {
				opened, err := o.openTask(task.open)
				task.open.result <- openResult{opened: opened, err: err}
				continue
			}
			if task.refresh {
				_ = o.refresh()
			}
		}
	}
}

func (o *owner) openTask(request *openRequest) (syncengine.OpenedResource, error) {
	if err := request.ctx.Err(); err != nil {
		return syncengine.OpenedResource{}, err
	}
	if err := o.ensureInitialized(); err != nil {
		return syncengine.OpenedResource{}, err
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return syncengine.OpenedResource{}, ErrProviderUnavailable
	}
	if o.invalid {
		err := o.lastError
		o.mu.Unlock()
		if err == nil {
			err = ErrProviderUnavailable
		}
		return syncengine.OpenedResource{}, err
	}
	snapshot := o.snapshot
	sequence := o.journal.LastSequence()
	revision := protocol.ResourceRevision(fmt.Sprintf("%d", o.resourceRevision))
	epoch := o.epoch
	decision := o.journal.Decide(request.resume)
	subscription, err := syncengine.NewLiveSubscription(epoch, sequence, o.provider.options.LiveCapacity)
	if err != nil {
		o.mu.Unlock()
		return syncengine.OpenedResource{}, err
	}
	o.subs[subscription] = struct{}{}
	o.mu.Unlock()

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, ErrProviderUnavailable
	}
	if len(encoded) > o.provider.options.MaxSnapshotBytes {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, fmt.Errorf("Codex login snapshot exceeds maximum size")
	}
	if err := request.ctx.Err(); err != nil {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, err
	}
	o.mu.Lock()
	closed, invalid := o.closed, o.invalid
	o.mu.Unlock()
	if closed || invalid {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, ErrProviderUnavailable
	}
	delivery := subscription.Delivery()
	return syncengine.OpenedResource{
		Snapshot:         syncengine.Snapshot{Content: syncengine.NewInlineSnapshotContent(encoded), ResourceRevision: revision},
		StreamEpoch:      epoch,
		Sequence:         sequence,
		Decision:         decision,
		LiveFromSequence: sequence + 1,
		Changes:          delivery.Entries,
		Terminal:         delivery.Terminal,
		Close:            func() { subscription.Close(); o.removeSubscription(subscription) },
	}, nil
}

func (o *owner) ensureInitialized() error {
	o.mu.Lock()
	initialized, invalid, closed := o.initialized, o.invalid, o.closed
	o.mu.Unlock()
	if closed {
		return ErrProviderUnavailable
	}
	if invalid {
		return ErrProviderUnavailable
	}
	if initialized {
		return nil
	}
	return o.rebuild()
}

func (o *owner) rebuild() error {
	status, err := o.provider.options.Status(o.identity)
	snapshot := snapshotFromError(o.identity)
	if err == nil {
		snapshot = SnapshotFromAuthStatus(o.identity, status)
	}
	if err := snapshot.Validate(); err != nil {
		snapshot = snapshotFromError(o.identity)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return ErrProviderUnavailable
	}
	for subscription := range o.subs {
		subscription.Desync(errors.New("Codex login resource rebuilt"))
	}
	o.subs = make(map[*syncengine.LiveSubscription]struct{})
	o.journal.Clear()
	o.snapshot = snapshot
	o.resourceRevision = 0
	o.initialized = true
	o.invalid = false
	o.lastError = nil
	return nil
}

func (o *owner) refresh() error {
	if err := o.ensureInitialized(); err != nil {
		return err
	}
	status, err := o.provider.options.Status(o.identity)
	candidate := snapshotFromError(o.identity)
	if err == nil {
		candidate = SnapshotFromAuthStatus(o.identity, status)
	}
	if candidate.Validate() != nil {
		candidate = snapshotFromError(o.identity)
	}
	o.mu.Lock()
	if o.closed || o.invalid {
		o.mu.Unlock()
		return ErrProviderUnavailable
	}
	if candidate == o.snapshot {
		o.mu.Unlock()
		return nil
	}
	if o.resourceRevision == ^uint64(0) {
		o.invalidateLocked("resource_revision_exhausted")
		o.mu.Unlock()
		return ErrProviderUnavailable
	}
	nextRevision := o.resourceRevision + 1
	change, changeErr := (Operation{Provider: o.identity, Value: candidate}).ToResourceChange(fmt.Sprintf("%d", nextRevision))
	if changeErr != nil {
		o.invalidateLocked("encode_operation")
		o.mu.Unlock()
		return ErrProviderUnavailable
	}
	encoded, encodeErr := json.Marshal(change)
	if encodeErr != nil || len(encoded) > o.provider.options.MaxChangeMessageBytes {
		o.invalidateLocked("change_too_large")
		o.mu.Unlock()
		return ErrProviderUnavailable
	}
	entry, appendErr := o.journal.Append(change)
	if appendErr != nil {
		o.invalidateLocked("journal_append")
		o.mu.Unlock()
		return ErrProviderUnavailable
	}
	o.snapshot = candidate
	o.resourceRevision = nextRevision
	subs := make([]*syncengine.LiveSubscription, 0, len(o.subs))
	for subscription := range o.subs {
		subs = append(subs, subscription)
	}
	o.mu.Unlock()
	for _, subscription := range subs {
		if !subscription.Offer(entry) {
			o.removeSubscription(subscription)
		}
	}
	return nil
}

func (o *owner) invalidate(reason string) {
	o.mu.Lock()
	o.invalidateLocked(reason)
	o.mu.Unlock()
}

func (o *owner) invalidateLocked(reason string) {
	if o.closed || o.invalid {
		return
	}
	o.invalid = true
	o.lastError = errors.New(reason)
	for subscription := range o.subs {
		subscription.Desync(ErrProviderUnavailable)
	}
	o.subs = make(map[*syncengine.LiveSubscription]struct{})
}

func (o *owner) removeSubscription(subscription *syncengine.LiveSubscription) {
	o.mu.Lock()
	delete(o.subs, subscription)
	o.mu.Unlock()
}

func (o *owner) closeState() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	for subscription := range o.subs {
		subscription.Close()
	}
	o.subs = make(map[*syncengine.LiveSubscription]struct{})
}

func (p *Provider) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		owners := make([]*owner, 0, len(p.owners))
		for _, current := range p.owners {
			owners = append(owners, current)
		}
		p.mu.Unlock()
		for _, current := range owners {
			current.cancel()
			<-current.done
		}
	})
}

func validProviderIdentity(value string) bool {
	return config.ValidateProviderName(value) == nil
}

func normalizeValidationError(err error) error {
	if errors.Is(err, ErrProviderNotFound) {
		return ErrProviderNotFound
	}
	if errors.Is(err, ErrProviderNotCodex) {
		return ErrProviderNotCodex
	}
	if errors.Is(err, ErrProviderUnavailable) {
		return ErrProviderUnavailable
	}
	if errors.Is(err, execution.ErrCodexProviderNotFound) {
		return ErrProviderNotFound
	}
	if errors.Is(err, execution.ErrCodexProviderNotCodex) || errors.Is(err, execution.ErrCodexProviderNoAuthFile) {
		return ErrProviderNotCodex
	}
	return ErrProviderUnavailable
}
