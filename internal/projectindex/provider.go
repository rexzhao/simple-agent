package projectindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	projectstore "github.com/rexzhao/simple-agent/internal/projects"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

const (
	DefaultJournalEntries   = 4096
	DefaultJournalBytes     = 8 * 1024 * 1024
	DefaultLiveCapacity     = 256
	DefaultProjectorQueue   = 256
	DefaultInlineSnapshot   = 64 * 1024
	DefaultMaxProjects      = 100000
	DefaultMaxSnapshotBytes = 16 * 1024 * 1024
	ProjectIndexResourceID  = "server"
)

var (
	ErrProviderClosed     = errors.New("project index provider is closed")
	ErrProviderInvalid    = errors.New("project index provider is invalid; resync required")
	ErrProjectorQueueFull = errors.New("project index projector queue is full")
)

type Event struct {
	Kind      string
	ProjectID string
	Sequence  uint64
	Reason    string
}

type Observer interface {
	ObserveProjectIndex(Event)
}

type BlobWriter interface {
	Put(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error)
}

type ProviderOptions struct {
	JournalEntries         int
	JournalBytes           int
	LiveCapacity           int
	ProjectorQueueCapacity int
	InlineSnapshotBytes    int
	MaxProjects            int
	MaxSnapshotBytes       int
	StreamEpoch            string
	OwnerContext           context.Context
	Observer               Observer
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
	if o.MaxProjects == 0 {
		o.MaxProjects = DefaultMaxProjects
	}
	if o.MaxSnapshotBytes == 0 {
		o.MaxSnapshotBytes = DefaultMaxSnapshotBytes
	}
	if o.JournalEntries <= 0 || o.JournalBytes <= 0 || o.LiveCapacity <= 0 || o.ProjectorQueueCapacity <= 0 || o.InlineSnapshotBytes <= 0 || o.MaxProjects <= 0 || o.MaxSnapshotBytes <= 0 {
		return ProviderOptions{}, fmt.Errorf("project index bounds must be positive")
	}
	if strings.TrimSpace(o.StreamEpoch) == "" {
		o.StreamEpoch = "project-index"
	}
	return o, nil
}

type Provider struct {
	store   *projectstore.Store
	options ProviderOptions

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
	snapshot         ProjectIndexSnapshot
	initialized      bool
	invalid          bool
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

func NewProvider(store *projectstore.Store, options ProviderOptions) (*Provider, error) {
	if store == nil {
		return nil, fmt.Errorf("project store is required")
	}
	resolved, err := options.withDefaults()
	if err != nil {
		return nil, err
	}
	p := &Provider{store: store, options: resolved}
	if _, err := p.ownerFor(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Provider) Type() protocol.ResourceType { return protocol.ResourceTypeProjectIndex }

func (p *Provider) Authorize(_ context.Context, principal syncengine.Principal, key protocol.ResourceKey) error {
	if p == nil || p.isClosed() {
		return ErrProviderClosed
	}
	if key.Type != protocol.ResourceTypeProjectIndex || key.ID != ProjectIndexResourceID {
		return fmt.Errorf("invalid project index resource")
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
	if key.Type != protocol.ResourceTypeProjectIndex || key.ID != ProjectIndexResourceID {
		return syncengine.OpenedResource{}, fmt.Errorf("invalid project index resource")
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

// Warm rebuilds from the durable project store. The single owner queue makes
// the discovery/rebuild barrier race-safe with post-commit callbacks.
func (p *Provider) Warm(ctx context.Context) error {
	if p == nil {
		return ErrProviderClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Match the other durable resource providers: complete durable discovery
	// precedes the test/lifecycle hook and the queued rebuild. A mutation that
	// races this discovery is either already visible to rebuild or is queued
	// after it, so it cannot be lost at the warm barrier.
	projects, err := p.store.ListAll()
	if err != nil {
		return err
	}
	if len(projects) > p.options.MaxProjects {
		return fmt.Errorf("project index project count exceeds maximum")
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
	o := &owner{provider: p, epoch: p.options.StreamEpoch, journal: journal,
		queue: make(chan ownerTask, p.options.ProjectorQueueCapacity), ctx: ctx, cancel: cancel,
		workerDone: make(chan struct{}), snapshot: ProjectIndexSnapshot{Projects: []ProjectSummary{}},
		subs: make(map[*syncengine.LiveSubscription]struct{})}
	p.owner = o
	go o.run(ctx)
	return o, nil
}

func (p *Provider) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// PublishCommitted is intentionally nonblocking. A saturated projection queue
// invalidates the resource and asks clients to resync rather than delaying the
// execution transaction that already committed.
func (p *Provider) PublishCommitted(change CommittedChange) error {
	if p == nil {
		return ErrProviderClosed
	}
	if err := validateCommittedChange(change); err != nil {
		if owner, ownerErr := p.ownerFor(); ownerErr == nil {
			owner.invalidate("invalid_committed_change")
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

// Invalidate is the explicit post-commit projection failure path. The next
// open rebuilds the singleton from the durable project store and receives a
// fresh stream epoch.
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
	if !validProjectID(change.ProjectID) || change.ProjectID != strings.TrimSpace(change.ProjectID) {
		return fmt.Errorf("committed project change has no canonical project id")
	}
	switch change.Kind {
	case CommittedProjectUpsert:
		if change.Project == nil {
			return fmt.Errorf("project upsert is missing project state")
		}
		if change.Project.ID != change.ProjectID {
			return fmt.Errorf("project id does not match change project id")
		}
		if err := SummaryFromProject(*change.Project).Validate(); err != nil {
			return err
		}
	case CommittedProjectRemove:
		if change.Project != nil {
			return fmt.Errorf("project remove must not contain project state")
		}
	default:
		return fmt.Errorf("invalid committed project change kind %q", change.Kind)
	}
	return nil
}

func cloneCommittedChange(change CommittedChange) *CommittedChange {
	clone := change
	if change.Project != nil {
		value := *change.Project
		clone.Project = &value
	}
	return &clone
}

func (p *Provider) observe(event Event) {
	if p != nil && p.options.Observer != nil {
		p.options.Observer.ObserveProjectIndex(event)
	}
}

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
		_ = o.rebuild()
	}
	if task.change != nil {
		o.mu.Lock()
		invalid := o.invalid || o.closed
		o.mu.Unlock()
		if !invalid && o.provider.options.BeforeApply != nil {
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
			err = ErrProviderInvalid
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
	resourceRevision := protocol.ResourceRevision(strconv.FormatUint(o.resourceRevision, 10))
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
		return syncengine.OpenedResource{}, fmt.Errorf("encode project index snapshot: %w", err)
	}
	if err := request.ctx.Err(); err != nil {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, err
	}
	if len(encoded) > o.provider.options.MaxSnapshotBytes {
		subscription.Close()
		o.removeSubscription(subscription)
		return syncengine.OpenedResource{}, fmt.Errorf("project index snapshot exceeds maximum size")
	}
	content := syncengine.NewInlineSnapshotContent(encoded)
	if len(encoded) > o.provider.options.InlineSnapshotBytes {
		if o.provider.options.BlobWriter == nil {
			subscription.Close()
			o.removeSubscription(subscription)
			return syncengine.OpenedResource{}, fmt.Errorf("project index snapshot exceeds inline limit")
		}
		blobContext, cancelBlob := context.WithCancel(o.ctx)
		stopRequest := context.AfterFunc(request.ctx, cancelBlob)
		descriptor, putErr := o.provider.options.BlobWriter.Put(blobContext, "application/json", encoded)
		stopRequest()
		cancelBlob()
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
		if err := protocol.ValidateBlobDescriptor(descriptor); err != nil {
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
	return syncengine.OpenedResource{Snapshot: syncengine.Snapshot{Content: content, ResourceRevision: resourceRevision},
		StreamEpoch: epoch, Sequence: sequence, Decision: decision, LiveFromSequence: sequence + 1,
		Changes: delivery.Entries, Terminal: delivery.Terminal,
		Close: func() { subscription.Close(); o.removeSubscription(subscription) }}, nil
}

func validateSnapshotBlobDescriptor(descriptor protocol.BlobDescriptor, content []byte, maxBytes int) error {
	if descriptor.ContentType != "application/json" {
		return fmt.Errorf("project index blob content type must be application/json")
	}
	if descriptor.Size != uint64(len(content)) || descriptor.Size > uint64(maxBytes) {
		return fmt.Errorf("project index blob size does not match snapshot")
	}
	digest := sha256.Sum256(content)
	hexDigest := hex.EncodeToString(digest[:])
	if descriptor.SHA256 != hexDigest || descriptor.ETag != `"`+hexDigest+`"` {
		return fmt.Errorf("project index blob integrity metadata does not match snapshot")
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

// ensureInitializedForChange deliberately differs from ensureRebuilt. An
// invalid owner is recoverable by Open (which performs a fresh snapshot
// barrier), but a stale queued change must never be allowed to trigger that
// recovery and then apply its pre-invalidation payload.
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

func (o *owner) applyCommitted(change CommittedChange) error {
	// A queue overflow or explicit invalidation is a generation boundary. Do
	// not let tasks that were already buffered before that boundary call
	// rebuild-and-apply: their captured state may be older than the durable
	// rebuild and can roll it back.
	if err := o.ensureInitializedForChange(); err != nil {
		return err
	}

	var summary ProjectSummary
	if change.Kind == CommittedProjectUpsert {
		if change.Project != nil {
			summary = SummaryFromProject(*change.Project)
		} else {
			loaded, loadErr := o.provider.store.Load(change.ProjectID)
			if loadErr != nil {
				o.invalidate("durable_reload")
				return loadErr
			}
			summary = SummaryFromProject(loaded)
		}
		if err := summary.Validate(); err != nil {
			o.invalidate("invalid_summary")
			return err
		}
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
	var operation Operation
	if change.Kind == CommittedProjectRemove {
		operation = Operation{Op: OperationRemove, Key: change.ProjectID}
		if _, exists := o.summaryByID(operation.Key); !exists {
			o.mu.Unlock()
			return nil
		}
	} else {
		current, exists := o.summaryByID(summary.ID)
		if exists && reflectSummaryEqual(current, summary) {
			o.mu.Unlock()
			return nil
		}
		if !exists && len(o.snapshot.Projects) >= o.provider.options.MaxProjects {
			o.mu.Unlock()
			o.invalidate("project_count_limit")
			return fmt.Errorf("project index project count exceeds maximum")
		}
		operation = Operation{Op: OperationUpsert, Key: summary.ID, Value: &summary}
	}
	o.mu.Unlock()
	if err := operation.Validate(); err != nil {
		o.invalidate("invalid_operation")
		return err
	}
	o.mu.Lock()
	candidate := snapshotWithOperation(o.snapshot, operation)
	o.mu.Unlock()
	if err := validateSnapshotBounds(candidate, o.provider.options.MaxProjects, o.provider.options.MaxSnapshotBytes); err != nil {
		o.invalidate("snapshot_bounds")
		return err
	}
	o.mu.Lock()
	currentRevision, closed, invalid := o.resourceRevision, o.closed, o.invalid
	o.mu.Unlock()
	if closed {
		return ErrProviderClosed
	}
	if invalid {
		return ErrProviderInvalid
	}
	if currentRevision == ^uint64(0) {
		o.invalidate("resource_revision_exhausted")
		return fmt.Errorf("project index resource revision exhausted")
	}
	nextRevision := currentRevision + 1
	changeValue, err := (Change{ResourceRevision: strconv.FormatUint(nextRevision, 10), Operations: []Operation{operation}}).ToResourceChange()
	if err != nil {
		o.invalidate("encode_operation")
		return err
	}
	entry, err := o.journal.Append(changeValue)
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
	o.provider.observe(Event{Kind: "change_published", ProjectID: operation.Key, Sequence: entry.Sequence})
	return nil
}

func snapshotWithOperation(snapshot ProjectIndexSnapshot, operation Operation) ProjectIndexSnapshot {
	candidate := cloneSnapshot(snapshot)
	if operation.Op == OperationRemove {
		filtered := make([]ProjectSummary, 0, len(candidate.Projects)-1)
		for _, item := range candidate.Projects {
			if item.ID != operation.Key {
				filtered = append(filtered, item)
			}
		}
		candidate.Projects = filtered
		return candidate
	}
	updated := false
	for i := range candidate.Projects {
		if candidate.Projects[i].ID == operation.Key {
			candidate.Projects[i] = *operation.Value
			updated = true
			break
		}
	}
	if !updated {
		candidate.Projects = append(candidate.Projects, *operation.Value)
	}
	sortSummaries(candidate.Projects)
	return candidate
}

func validateSnapshotBounds(snapshot ProjectIndexSnapshot, maxProjects, maxBytes int) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if len(snapshot.Projects) > maxProjects {
		return fmt.Errorf("project index project count exceeds maximum")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode project index snapshot: %w", err)
	}
	if len(encoded) > maxBytes {
		return fmt.Errorf("project index snapshot exceeds maximum size")
	}
	return nil
}

func reflectSummaryEqual(a, b ProjectSummary) bool {
	return a.ID == b.ID && a.Root == b.Root && a.DisplayName == b.DisplayName && a.Archived == b.Archived && a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt)
}

func (o *owner) summaryByID(id string) (ProjectSummary, bool) {
	for _, summary := range o.snapshot.Projects {
		if summary.ID == id {
			return summary, true
		}
	}
	return ProjectSummary{}, false
}

func (o *owner) rebuild() error {
	projects, err := o.provider.store.ListAll()
	if err != nil {
		o.invalidate("durable_list")
		return err
	}
	if len(projects) > o.provider.options.MaxProjects {
		o.invalidate("project_count_limit")
		return fmt.Errorf("project index project count exceeds maximum")
	}
	summaries := make([]ProjectSummary, 0, len(projects))
	for _, project := range projects {
		summary := SummaryFromProject(project)
		if err := summary.Validate(); err != nil {
			o.invalidate("invalid_summary")
			return err
		}
		summaries = append(summaries, summary)
	}
	sortSummaries(summaries)
	rebuiltSnapshot := ProjectIndexSnapshot{Projects: summaries}
	if err := validateSnapshotBounds(rebuiltSnapshot, o.provider.options.MaxProjects, o.provider.options.MaxSnapshotBytes); err != nil {
		o.invalidate("invalid_snapshot")
		return err
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return ErrProviderClosed
	}
	for subscription := range o.subs {
		subscription.Desync(fmt.Errorf("project index rebuilt"))
	}
	o.subs = make(map[*syncengine.LiveSubscription]struct{})
	generation := o.provider.generation.Add(1)
	o.epoch = fmt.Sprintf("%s/%d", o.provider.options.StreamEpoch, generation)
	journal, err := syncengine.NewBoundedJournal(o.epoch, o.provider.options.JournalEntries, o.provider.options.JournalBytes)
	if err != nil {
		o.invalid = true
		o.mu.Unlock()
		return err
	}
	o.journal = journal
	o.snapshot = rebuiltSnapshot
	o.resourceRevision = 0
	o.initialized = true
	o.invalid = false
	o.mu.Unlock()
	o.provider.observe(Event{Kind: "rebuild_completed"})
	return nil
}

func (o *owner) invalidate(reason string) {
	o.mu.Lock()
	if o.closed || o.invalid {
		o.mu.Unlock()
		return
	}
	o.invalid = true
	for subscription := range o.subs {
		subscription.Desync(fmt.Errorf("project index invalid: %s", reason))
	}
	o.subs = make(map[*syncengine.LiveSubscription]struct{})
	sequence := uint64(0)
	if o.journal != nil {
		sequence = o.journal.LastSequence()
	}
	o.mu.Unlock()
	o.provider.observe(Event{Kind: "invalid", Sequence: sequence, Reason: reason})
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

func cloneSnapshot(snapshot ProjectIndexSnapshot) ProjectIndexSnapshot {
	clone := ProjectIndexSnapshot{Projects: append([]ProjectSummary(nil), snapshot.Projects...)}
	if clone.Projects == nil {
		clone.Projects = []ProjectSummary{}
	}
	return clone
}

var _ syncengine.ResourceProvider = (*Provider)(nil)
