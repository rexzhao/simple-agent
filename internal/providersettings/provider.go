package providersettings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

const (
	DefaultJournalEntries   = 256
	DefaultJournalBytes     = 4 * 1024 * 1024
	DefaultLiveCapacity     = 256
	DefaultProjectorQueue   = 256
	DefaultInlineSnapshot   = 64 * 1024
	DefaultMaxProviders     = 1024
	DefaultMaxModels        = 4096
	DefaultMaxSnapshotBytes = 16 * 1024 * 1024
)

var (
	ErrProviderClosed     = errors.New("provider settings provider is closed")
	ErrProviderInvalid    = errors.New("provider settings provider is invalid; resync required")
	ErrProjectorQueueFull = errors.New("provider settings projector queue is full")
)

type BlobWriter interface {
	Put(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error)
}

type ProviderOptions struct {
	ConfigPath             string
	ServerRoot             string
	JournalEntries         int
	JournalBytes           int
	LiveCapacity           int
	ProjectorQueueCapacity int
	InlineSnapshotBytes    int
	MaxProviders           int
	MaxModels              int
	MaxSnapshotBytes       int
	MaxChangeMessageBytes  int
	StreamEpoch            string
	OwnerContext           context.Context
	BlobWriter             BlobWriter
	BeforeApply            func(CommittedChange)
	BeforeWarm             func()
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
	if o.MaxProviders == 0 {
		o.MaxProviders = DefaultMaxProviders
	}
	if o.MaxModels == 0 {
		o.MaxModels = DefaultMaxModels
	}
	if o.MaxSnapshotBytes == 0 {
		o.MaxSnapshotBytes = DefaultMaxSnapshotBytes
	}
	if o.MaxChangeMessageBytes == 0 {
		o.MaxChangeMessageBytes = o.JournalBytes
	}
	if o.JournalEntries <= 0 || o.JournalBytes <= 0 || o.LiveCapacity <= 0 || o.ProjectorQueueCapacity <= 0 || o.InlineSnapshotBytes <= 0 || o.MaxProviders <= 0 || o.MaxModels <= 0 || o.MaxSnapshotBytes <= 0 || o.MaxChangeMessageBytes <= 0 {
		return ProviderOptions{}, fmt.Errorf("provider settings bounds must be positive")
	}
	if strings.TrimSpace(o.StreamEpoch) == "" {
		o.StreamEpoch = "provider-settings"
	}
	if strings.TrimSpace(o.ConfigPath) == "" {
		return ProviderOptions{}, fmt.Errorf("provider settings config path is required")
	}
	return o, nil
}

type Provider struct {
	options    ProviderOptions
	mu         sync.Mutex
	owner      *owner
	closed     bool
	generation atomic.Uint64
	closeOnce  sync.Once
}

type owner struct {
	provider   *Provider
	epoch      string
	journal    *syncengine.Journal
	queue      chan ownerTask
	ctx        context.Context
	cancel     context.CancelFunc
	workerDone chan struct{}

	mu               sync.Mutex
	resourceRevision uint64
	snapshot         ProviderSettingsSnapshot
	initialized      bool
	invalid          bool
	lastError        error
	closed           bool
	subs             map[*syncengine.LiveSubscription]struct{}
}

type ownerTask struct {
	change  *CommittedChange
	rebuild bool
	barrier chan error
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
	p := &Provider{options: resolved}
	if _, err := p.ownerFor(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Provider) Type() protocol.ResourceType { return protocol.ResourceTypeProviderSettings }

func (p *Provider) Authorize(_ context.Context, principal syncengine.Principal, key protocol.ResourceKey) error {
	if p == nil || p.isClosed() {
		return ErrProviderClosed
	}
	if key.Type != protocol.ResourceTypeProviderSettings || key.ID != ResourceID {
		return fmt.Errorf("invalid provider settings resource")
	}
	if strings.TrimSpace(principal.ID) == "" {
		return fmt.Errorf("principal is required")
	}
	return nil
}

func (p *Provider) Open(ctx context.Context, key protocol.ResourceKey, resume *protocol.ResumeToken) (syncengine.OpenedResource, error) {
	if p == nil {
		return syncengine.OpenedResource{}, ErrProviderClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if key.Type != protocol.ResourceTypeProviderSettings || key.ID != ResourceID {
		return syncengine.OpenedResource{}, fmt.Errorf("invalid provider settings resource")
	}
	if err := ctx.Err(); err != nil {
		return syncengine.OpenedResource{}, err
	}
	o, err := p.ownerFor()
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
	case <-o.workerDone:
		return syncengine.OpenedResource{}, ErrProviderClosed
	}
}

// Warm rebuilds from config.Load, the durable/current provider settings
// authority. The config is loaded by the owner, so an open barrier and a
// successful post-commit publication share one serialization domain.
func (p *Provider) Warm(ctx context.Context) error {
	if p == nil {
		return ErrProviderClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if p.options.BeforeWarm != nil {
		p.options.BeforeWarm()
	}
	o, err := p.ownerFor()
	if err != nil {
		return err
	}
	if err := o.enqueue(ctx, ownerTask{rebuild: true}); err != nil {
		return err
	}
	return p.Flush(ctx)
}

func (p *Provider) Flush(ctx context.Context) error {
	if p == nil {
		return ErrProviderClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	o, err := p.ownerFor()
	if err != nil {
		return err
	}
	barrier := make(chan error, 1)
	if err := o.enqueue(ctx, ownerTask{barrier: barrier}); err != nil {
		return err
	}
	select {
	case err := <-barrier:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-o.workerDone:
		return ErrProviderClosed
	}
}

// PublishCommitted is nonblocking. It is the only input from execution and
// carries no provider configuration or credential-bearing fields.
func (p *Provider) PublishCommitted(change CommittedChange) error {
	if p == nil {
		return ErrProviderClosed
	}
	if err := validateCommittedChange(change); err != nil {
		if o, ownerErr := p.ownerFor(); ownerErr == nil {
			o.invalidate("invalid_committed_change")
		}
		return err
	}
	o, err := p.ownerFor()
	if err != nil {
		return err
	}
	o.mu.Lock()
	invalid, closed := o.invalid, o.closed
	o.mu.Unlock()
	if closed {
		return ErrProviderClosed
	}
	if invalid {
		return ErrProviderInvalid
	}
	if err := o.enqueueNonblocking(ownerTask{change: cloneCommittedChange(change)}); err != nil {
		o.invalidate("projector_queue_full")
		return err
	}
	return nil
}

// Invalidate marks the projection stale. The next Open performs a durable
// rebuild with a new epoch; existing live clients receive a terminal resync.
func (p *Provider) Invalidate(reason string) error {
	if p == nil {
		return ErrProviderClosed
	}
	o, err := p.ownerFor()
	if err != nil {
		return err
	}
	o.invalidate(reason)
	return nil
}

func validateCommittedChange(change CommittedChange) error {
	switch change.Kind {
	case CommittedProviderRefresh:
		return nil
	case CommittedProviderUpsert, CommittedProviderRemove:
		if err := ValidateProviderName(change.ProviderName); err != nil {
			return fmt.Errorf("provider name is not canonical: %w", err)
		}
	case CommittedDefaultChanged:
		if change.DefaultProvider != "" {
			if err := ValidateProviderName(change.DefaultProvider); err != nil {
				return fmt.Errorf("default provider is not canonical: %w", err)
			}
		}
		if !utf8.ValidString(change.DefaultModel) {
			return fmt.Errorf("default model is not valid UTF-8")
		}
	default:
		return fmt.Errorf("invalid committed provider settings change kind %q", change.Kind)
	}
	return nil
}

func cloneCommittedChange(value CommittedChange) *CommittedChange {
	copyValue := value
	return &copyValue
}

func (p *Provider) ownerFor() (*owner, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrProviderClosed
	}
	if p.owner != nil {
		return p.owner, nil
	}
	parent := p.options.OwnerContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	journal, err := syncengine.NewBoundedJournal(p.options.StreamEpoch, p.options.JournalEntries, p.options.JournalBytes)
	if err != nil {
		cancel()
		return nil, err
	}
	o := &owner{provider: p, epoch: p.options.StreamEpoch, journal: journal, queue: make(chan ownerTask, p.options.ProjectorQueueCapacity), ctx: ctx, cancel: cancel, workerDone: make(chan struct{}), snapshot: ProviderSettingsSnapshot{Providers: []ProviderSettings{}}, subs: make(map[*syncengine.LiveSubscription]struct{})}
	p.owner = o
	go o.run(ctx)
	return o, nil
}

func (p *Provider) isClosed() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.closed }

func (o *owner) enqueue(ctx context.Context, task ownerTask) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-o.workerDone:
		return ErrProviderClosed
	case <-ctx.Done():
		return ctx.Err()
	case o.queue <- task:
		return nil
	}
}
func (o *owner) enqueueNonblocking(task ownerTask) error {
	select {
	case <-o.workerDone:
		return ErrProviderClosed
	default:
	}
	select {
	case o.queue <- task:
		return nil
	default:
		return ErrProjectorQueueFull
	}
}

func (o *owner) run(ctx context.Context) {
	defer close(o.workerDone)
	for {
		select {
		case <-ctx.Done():
			o.closeState()
			return
		case task := <-o.queue:
			o.execute(task)
		}
	}
}
func (o *owner) execute(task ownerTask) {
	if task.open != nil {
		opened, err := o.openTask(task.open)
		task.open.result <- openResult{opened: opened, err: err}
		return
	}
	if task.rebuild {
		if err := o.rebuild(); err != nil {
			o.mu.Lock()
			o.lastError = err
			o.mu.Unlock()
		}
	}
	if task.change != nil {
		o.mu.Lock()
		invalid, closed := o.invalid, o.closed
		o.mu.Unlock()
		if !invalid && !closed && o.provider.options.BeforeApply != nil {
			o.provider.options.BeforeApply(*task.change)
		}
		_ = o.applyCommitted(*task.change)
	}
	if task.barrier != nil {
		o.mu.Lock()
		err := error(nil)
		if o.closed {
			err = ErrProviderClosed
		} else if o.invalid {
			err = o.lastError
			if err == nil {
				err = ErrProviderInvalid
			}
		}
		o.mu.Unlock()
		task.barrier <- err
	}
}

func (o *owner) openTask(request *openRequest) (syncengine.OpenedResource, error) {
	if err := request.ctx.Err(); err != nil {
		return syncengine.OpenedResource{}, err
	}
	if err := o.ensureRebuilt(); err != nil {
		return syncengine.OpenedResource{}, err
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return syncengine.OpenedResource{}, ErrProviderClosed
	}
	if o.invalid {
		o.mu.Unlock()
		return syncengine.OpenedResource{}, ErrProviderInvalid
	}
	snapshot := cloneSnapshot(o.snapshot)
	sequence := o.journal.LastSequence()
	revision := protocol.ResourceRevision(strconv.FormatUint(o.resourceRevision, 10))
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
		return syncengine.OpenedResource{}, fmt.Errorf("encode provider settings snapshot: %w", err)
	}
	if err := request.ctx.Err(); err != nil {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, err
	}
	if len(encoded) > o.provider.options.MaxSnapshotBytes {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, fmt.Errorf("provider settings snapshot exceeds maximum size")
	}
	content := syncengine.NewInlineSnapshotContent(encoded)
	if len(encoded) > o.provider.options.InlineSnapshotBytes {
		if o.provider.options.BlobWriter == nil {
			subscription.Close()
			o.removeSubscription(subscription)
			return syncengine.OpenedResource{}, fmt.Errorf("provider settings snapshot exceeds inline limit")
		}
		blobContext, cancel := context.WithCancel(o.ctx)
		stop := context.AfterFunc(request.ctx, cancel)
		descriptor, putErr := o.provider.options.BlobWriter.Put(blobContext, "application/json", encoded)
		stop()
		cancel()
		if putErr != nil {
			subscription.Close()
			o.removeSubscription(subscription)
			return syncengine.OpenedResource{}, putErr
		}
		if err := request.ctx.Err(); err != nil {
			subscription.Close()
			o.removeSubscription(subscription)
			return syncengine.OpenedResource{}, err
		}
		if err := validateSnapshotBlobDescriptor(descriptor, encoded, o.provider.options.MaxSnapshotBytes); err != nil {
			subscription.Close()
			o.removeSubscription(subscription)
			return syncengine.OpenedResource{}, err
		}
		content = syncengine.NewBlobSnapshotContent(descriptor)
	}
	o.mu.Lock()
	closed, invalid := o.closed, o.invalid
	o.mu.Unlock()
	if err := request.ctx.Err(); err != nil {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, err
	}
	if closed {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, ErrProviderClosed
	}
	if invalid {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, ErrProviderInvalid
	}
	delivery := subscription.Delivery()
	return syncengine.OpenedResource{Snapshot: syncengine.Snapshot{Content: content, ResourceRevision: revision}, StreamEpoch: epoch, Sequence: sequence, Decision: decision, LiveFromSequence: sequence + 1, Changes: delivery.Entries, Terminal: delivery.Terminal, Close: func() { subscription.Close(); o.removeSubscription(subscription) }}, nil
}

func validateSnapshotBlobDescriptor(descriptor protocol.BlobDescriptor, content []byte, maxBytes int) error {
	if descriptor.ContentType != "application/json" || descriptor.Size != uint64(len(content)) || descriptor.Size > uint64(maxBytes) {
		return fmt.Errorf("provider settings blob metadata does not match snapshot")
	}
	digest := sha256.Sum256(content)
	encoded := hex.EncodeToString(digest[:])
	if descriptor.SHA256 != encoded || descriptor.ETag != `"`+encoded+`"` {
		return fmt.Errorf("provider settings blob integrity metadata does not match snapshot")
	}
	return nil
}

func (o *owner) ensureRebuilt() error {
	o.mu.Lock()
	need, closed := !o.initialized || o.invalid, o.closed
	o.mu.Unlock()
	if closed {
		return ErrProviderClosed
	}
	if !need {
		return nil
	}
	return o.rebuild()
}
func (o *owner) ensureInitializedForChange() error {
	o.mu.Lock()
	initialized, invalid, closed := o.initialized, o.invalid, o.closed
	o.mu.Unlock()
	if closed {
		return ErrProviderClosed
	}
	if invalid {
		return ErrProviderInvalid
	}
	if initialized {
		return nil
	}
	return o.rebuild()
}

func (o *owner) applyCommitted(_ CommittedChange) error {
	if err := o.ensureInitializedForChange(); err != nil {
		return err
	}
	candidate, err := o.loadSnapshot()
	if err != nil {
		o.invalidate("unsafe_snapshot")
		return err
	}
	if err := o.validateBounds(candidate); err != nil {
		o.invalidate("snapshot_bounds")
		return err
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return ErrProviderClosed
	}
	if o.invalid {
		o.mu.Unlock()
		return ErrProviderInvalid
	}
	previous := cloneSnapshot(o.snapshot)
	if reflect.DeepEqual(previous, candidate) {
		o.mu.Unlock()
		return nil
	}
	currentRevision := o.resourceRevision
	o.mu.Unlock()
	operations := diffSnapshots(previous, candidate)
	if len(operations) == 0 {
		return nil
	}
	if currentRevision == ^uint64(0) {
		o.invalidate("resource_revision_exhausted")
		return fmt.Errorf("provider settings resource revision exhausted")
	}
	next := currentRevision + 1
	change, err := (Change{ResourceRevision: strconv.FormatUint(next, 10), Operations: operations}).ToResourceChange()
	if err != nil {
		o.invalidate("encode_operation")
		return err
	}
	encoded, err := json.Marshal(change)
	if err != nil {
		o.invalidate("encode_change")
		return err
	}
	if len(encoded) > o.provider.options.MaxChangeMessageBytes {
		o.invalidate("change_too_large")
		return fmt.Errorf("provider settings change exceeds maximum size")
	}
	entry, err := o.journal.Append(change)
	if err != nil {
		o.invalidate("journal_append")
		return err
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return ErrProviderClosed
	}
	if o.invalid || o.resourceRevision != currentRevision {
		o.mu.Unlock()
		o.invalidate("resource_revision_race")
		return ErrProviderInvalid
	}
	o.snapshot = candidate
	o.resourceRevision = next
	subscriptions := make([]*syncengine.LiveSubscription, 0, len(o.subs))
	for subscription := range o.subs {
		subscriptions = append(subscriptions, subscription)
	}
	o.mu.Unlock()
	for _, subscription := range subscriptions {
		if !subscription.Offer(entry) {
			o.removeSubscription(subscription)
		}
	}
	return nil
}

func diffSnapshots(previous, next ProviderSettingsSnapshot) []Operation {
	oldProviders, newProviders := map[string]ProviderSettings{}, map[string]ProviderSettings{}
	for _, provider := range previous.Providers {
		oldProviders[provider.Name] = provider
	}
	for _, provider := range next.Providers {
		newProviders[provider.Name] = provider
	}
	keys := make([]string, 0, len(oldProviders)+len(newProviders))
	seen := map[string]struct{}{}
	for key := range oldProviders {
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for key := range newProviders {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	operations := make([]Operation, 0, len(keys)+1)
	for _, key := range keys {
		oldValue, oldOK := oldProviders[key]
		newValue, newOK := newProviders[key]
		if oldOK && !newOK {
			operations = append(operations, Operation{Op: OperationRemove, Key: key})
		} else if newOK && (!oldOK || !reflect.DeepEqual(oldValue, newValue)) {
			value := newValue
			operations = append(operations, Operation{Op: OperationUpsertDefault, Key: key, Value: &value})
		}
	}
	if previous.DefaultProvider != next.DefaultProvider || previous.DefaultModel != next.DefaultModel {
		operations = append(operations, Operation{Op: OperationReplaceDefault, Key: ResourceID, Default: &DefaultSelection{Provider: next.DefaultProvider, Model: next.DefaultModel}})
	}
	return operations
}

func (o *owner) validateBounds(snapshot ProviderSettingsSnapshot) error {
	if len(snapshot.Providers) > o.provider.options.MaxProviders {
		return fmt.Errorf("provider count exceeds maximum")
	}
	models := 0
	for _, provider := range snapshot.Providers {
		models += len(provider.Models)
	}
	if models > o.provider.options.MaxModels {
		return fmt.Errorf("model count exceeds maximum")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if len(encoded) > o.provider.options.MaxSnapshotBytes {
		return fmt.Errorf("provider settings snapshot exceeds maximum size")
	}
	return nil
}

func (o *owner) rebuild() error {
	snapshot, err := o.loadSnapshot()
	if err != nil {
		o.invalidate("unsafe_snapshot")
		return err
	}
	if err := o.validateBounds(snapshot); err != nil {
		o.invalidate("snapshot_bounds")
		return err
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return ErrProviderClosed
	}
	for subscription := range o.subs {
		subscription.Desync(fmt.Errorf("provider settings rebuilt"))
	}
	o.subs = make(map[*syncengine.LiveSubscription]struct{})
	generation := o.provider.generation.Add(1)
	epoch := fmt.Sprintf("%s/%d", o.provider.options.StreamEpoch, generation)
	journal, err := syncengine.NewBoundedJournal(epoch, o.provider.options.JournalEntries, o.provider.options.JournalBytes)
	if err != nil {
		o.invalid = true
		o.mu.Unlock()
		return err
	}
	o.epoch, o.journal, o.snapshot, o.resourceRevision, o.initialized, o.invalid = epoch, journal, snapshot, 0, true, false
	o.lastError = nil
	o.mu.Unlock()
	return nil
}

func (o *owner) loadSnapshot() (ProviderSettingsSnapshot, error) {
	cfg, err := config.Load(o.provider.options.ConfigPath)
	if err != nil {
		// A service may be started before its optional config has been created.
		// Keep the resource available with an empty, safe snapshot; the first
		// successful provider/default commit or explicit refresh rebuilds it
		// from the durable authority. Other config errors remain fatal.
		if errors.Is(err, os.ErrNotExist) {
			return ProviderSettingsSnapshot{
				ServerRoot: o.provider.options.ServerRoot,
				ConfigPath: o.provider.options.ConfigPath,
				Providers:  []ProviderSettings{},
			}, nil
		}
		return ProviderSettingsSnapshot{}, fmt.Errorf("load provider settings authority: %w", err)
	}
	return SnapshotFromConfig(*cfg, o.provider.options.ServerRoot)
}

func (o *owner) invalidate(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.invalid {
		return
	}
	o.invalid = true
	for subscription := range o.subs {
		subscription.Desync(fmt.Errorf("provider settings invalid: %s", reason))
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
func (o *owner) close() { o.cancel(); <-o.workerDone }
func (p *Provider) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		owner := p.owner
		p.mu.Unlock()
		if owner != nil {
			owner.close()
		}
	})
}

var _ syncengine.ResourceProvider = (*Provider)(nil)
