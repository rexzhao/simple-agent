package sessionindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

const (
	DefaultJournalEntries = 4096
	DefaultJournalBytes   = 8 * 1024 * 1024
	DefaultLiveCapacity   = 256
	DefaultProjectorQueue = 256
	DefaultInlineSnapshot = 64 * 1024
)

var (
	ErrProviderClosed     = errors.New("session index provider is closed")
	ErrProviderInvalid    = errors.New("session index provider is invalid; resync required")
	ErrProjectorQueueFull = errors.New("session index projector queue is full")
)

// Event is deliberately metadata-only observability. It never contains a
// prompt, conversation item, token, ticket, or complete snapshot.
type Event struct {
	Kind      string
	ProjectID string
	SessionID string
	Sequence  uint64
	Reason    string
}

type Observer interface {
	ObserveSessionIndex(Event)
}

// BlobWriter is the provider's neutral immutable data-plane boundary. The
// webapp supplies an implementation; sessionindex does not serve or own URLs.
type BlobWriter interface {
	Put(ctx context.Context, contentType string, content []byte) (protocol.BlobDescriptor, error)
}

type ProviderOptions struct {
	JournalEntries         int
	JournalBytes           int
	LiveCapacity           int
	ProjectorQueueCapacity int
	InlineSnapshotBytes    int
	StreamEpoch            string
	OwnerContext           context.Context
	Observer               Observer
	BlobWriter             BlobWriter
	// BeforeApply is an optional synchronization hook used by deterministic
	// tests to hold a projector worker; it is never called by the commit path.
	BeforeApply func(CommittedChange)
	// BeforeWarm is a deterministic test hook invoked after durable discovery
	// and before owner rebuild tasks are queued.
	BeforeWarm func()
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
	if o.JournalEntries <= 0 || o.JournalBytes <= 0 || o.LiveCapacity <= 0 || o.ProjectorQueueCapacity <= 0 || o.InlineSnapshotBytes <= 0 {
		return ProviderOptions{}, fmt.Errorf("session index bounds must be positive")
	}
	if strings.TrimSpace(o.StreamEpoch) == "" {
		o.StreamEpoch = "session-index"
	}
	return o, nil
}

// Provider owns one bounded asynchronous projector per project. Durable
// callbacks only enqueue a typed change. The owner worker alone performs
// durable rebuild, summary encoding, journal append, and live delivery.
type Provider struct {
	store   *sessions.V2Store
	options ProviderOptions

	mu         sync.Mutex
	owners     map[string]*owner
	closed     bool
	generation atomic.Uint64
	closeOnce  sync.Once
}

type owner struct {
	provider   *Provider
	projectID  string
	epoch      string
	journal    *syncengine.Journal
	queue      chan ownerTask
	ctx        context.Context
	cancel     context.CancelFunc
	workerDone chan struct{}

	mu               sync.Mutex
	resourceRevision uint64 // project resource revision; independent of summary revision
	snapshot         SessionIndexSnapshot
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

func NewProvider(store *sessions.V2Store, options ProviderOptions) (*Provider, error) {
	if store == nil {
		return nil, fmt.Errorf("session store is required")
	}
	resolved, err := options.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Provider{store: store, options: resolved, owners: make(map[string]*owner)}, nil
}

func (p *Provider) Type() protocol.ResourceType { return protocol.ResourceTypeSessionIndex }

// Authorize implements the current single-user capability model.
func (p *Provider) Authorize(_ context.Context, principal syncengine.Principal, key protocol.ResourceKey) error {
	if p == nil || p.isClosed() {
		return ErrProviderClosed
	}
	if key.Type != protocol.ResourceTypeSessionIndex || strings.TrimSpace(key.ID) == "" {
		return fmt.Errorf("invalid session index resource")
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
	if key.Type != protocol.ResourceTypeSessionIndex {
		return syncengine.OpenedResource{}, fmt.Errorf("invalid session index resource type")
	}
	if err := ctx.Err(); err != nil {
		return syncengine.OpenedResource{}, err
	}
	projectID := strings.TrimSpace(key.ID)
	if projectID == "" {
		return syncengine.OpenedResource{}, fmt.Errorf("project id is required")
	}
	o, err := p.ownerFor(projectID)
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

// Flush is a deterministic test and lifecycle barrier. It waits until every
// task queued for the selected owner has been applied. It never runs on the
// durable mutation callback path.
func (p *Provider) Flush(ctx context.Context, projectIDs ...string) error {
	if p == nil {
		return ErrProviderClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	owners, err := p.flushOwners(projectIDs)
	if err != nil {
		return err
	}
	for _, o := range owners {
		barrier := make(chan error, 1)
		if err := o.enqueue(ctx, ownerTask{barrier: barrier}); err != nil {
			return err
		}
		select {
		case err := <-barrier:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-o.workerDone:
			return ErrProviderClosed
		}
	}
	return nil
}

func (p *Provider) flushOwners(projectIDs []string) ([]*owner, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrProviderClosed
	}
	if len(projectIDs) == 0 {
		owners := make([]*owner, 0, len(p.owners))
		for _, o := range p.owners {
			owners = append(owners, o)
		}
		sort.Slice(owners, func(i, j int) bool { return owners[i].projectID < owners[j].projectID })
		return owners, nil
	}
	owners := make([]*owner, 0, len(projectIDs))
	for _, projectID := range projectIDs {
		o := p.owners[strings.TrimSpace(projectID)]
		if o == nil {
			return nil, fmt.Errorf("project owner %q not found", projectID)
		}
		owners = append(owners, o)
	}
	return owners, nil
}

// Warm first discovers projects from durable state, then asks each owner to
// rebuild in its own serialized queue. A callback racing the discovery is
// either before the rebuild task or after it; in both cases it is not lost.
func (p *Provider) Warm(ctx context.Context) error {
	if p == nil {
		return ErrProviderClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	states, err := p.store.ListStates(sessions.V2ListOptions{All: true})
	if err != nil {
		p.observe(Event{Kind: "rebuild_failed", Reason: "durable_list"})
		return err
	}
	projects := make(map[string]struct{})
	for _, state := range states {
		if projectID := strings.TrimSpace(state.ProjectID); projectID != "" {
			projects[projectID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(projects))
	for projectID := range projects {
		ids = append(ids, projectID)
	}
	sort.Strings(ids)
	if hook := p.options.BeforeWarm; hook != nil {
		hook()
	}
	for _, projectID := range ids {
		o, err := p.ownerFor(projectID)
		if err != nil {
			return err
		}
		if err := o.enqueue(ctx, ownerTask{rebuild: true}); err != nil {
			return err
		}
	}
	return p.Flush(ctx, ids...)
}

func (p *Provider) ownerFor(projectID string) (*owner, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("project id is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrProviderClosed
	}
	if o := p.owners[projectID]; o != nil {
		return o, nil
	}
	generation := p.generation.Add(1)
	epoch := fmt.Sprintf("%s/%s/%d", p.options.StreamEpoch, projectID, generation)
	journal, err := syncengine.NewBoundedJournal(epoch, p.options.JournalEntries, p.options.JournalBytes)
	if err != nil {
		return nil, err
	}
	ownerParent := p.options.OwnerContext
	if ownerParent == nil {
		ownerParent = context.Background()
	}
	ownerContext, cancel := context.WithCancel(ownerParent)
	o := &owner{
		provider: p, projectID: projectID, epoch: epoch, journal: journal,
		queue: make(chan ownerTask, p.options.ProjectorQueueCapacity), ctx: ownerContext, cancel: cancel,
		workerDone: make(chan struct{}), snapshot: SessionIndexSnapshot{Sessions: []SessionSummary{}},
		subs: make(map[*syncengine.LiveSubscription]struct{}),
	}
	p.owners[projectID] = o
	go o.run(ownerContext)
	return o, nil
}

func (p *Provider) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// PublishCommitted is the typed durable-commit boundary. It is deliberately
// nonblocking: queue saturation invalidates the resource instead of dropping
// a committed change or making execution wait for projection.
func (p *Provider) PublishCommitted(change CommittedChange) error {
	if p == nil {
		return ErrProviderClosed
	}
	if err := validateCommittedChange(change); err != nil {
		p.invalidateRejectedChange(change, "invalid_committed_change")
		return err
	}
	projectID := strings.TrimSpace(change.ProjectID)
	o, err := p.ownerFor(projectID)
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

func validateCommittedChange(change CommittedChange) error {
	projectID := strings.TrimSpace(change.ProjectID)
	if projectID == "" {
		return fmt.Errorf("committed session change has no project id")
	}
	if change.ProjectID != projectID {
		return fmt.Errorf("change project_id is not canonical")
	}
	if change.Summary != nil {
		if change.Summary.ProjectID != projectID {
			return fmt.Errorf("summary project_id does not match change project_id")
		}
		if strings.TrimSpace(change.SessionID) == "" || change.Summary.SessionID != strings.TrimSpace(change.SessionID) {
			return fmt.Errorf("summary session_id does not match change session_id")
		}
	}
	sessionID := strings.TrimSpace(change.SessionID)
	runID := strings.TrimSpace(change.RunID)
	if change.SessionID != sessionID || change.RunID != runID {
		return fmt.Errorf("change identifiers are not canonical")
	}
	switch change.Kind {
	case CommittedSessionUpsert:
		if sessionID == "" {
			return fmt.Errorf("upsert session_id is required")
		}
	case CommittedSessionRemove:
		if sessionID == "" {
			return fmt.Errorf("remove session_id is required")
		}
		if runID != "" || change.Summary != nil {
			return fmt.Errorf("remove must not contain run or summary")
		}
	case CommittedRunStarted, CommittedRunSettled, CommittedSessionMarkRead:
		if sessionID == "" || runID == "" {
			return fmt.Errorf("lifecycle session_id and run_id are required")
		}
		if change.Summary != nil && change.Summary.RunID != runID {
			return fmt.Errorf("summary run_id does not match change run_id")
		}
	case CommittedProjectRefresh:
		if sessionID != "" || runID != "" || change.Summary != nil {
			return fmt.Errorf("project refresh must not contain session data")
		}
	default:
		return fmt.Errorf("invalid committed change kind %q", change.Kind)
	}
	return nil
}

func (p *Provider) invalidateRejectedChange(change CommittedChange, reason string) {
	if projectID := strings.TrimSpace(change.ProjectID); projectID != "" {
		_ = p.InvalidateProject(projectID, reason)
	}
}

// InvalidateProject is the explicit failure path after a durable commit could
// not be completely projected. It immediately terminates current subscribers;
// the next Open rebuilds from durable state.
func (p *Provider) InvalidateProject(projectID, reason string) error {
	if p == nil {
		return ErrProviderClosed
	}
	o, err := p.ownerFor(projectID)
	if err != nil {
		return err
	}
	o.invalidate(reason)
	return nil
}

func (p *Provider) observe(event Event) {
	if p != nil && p.options.Observer != nil {
		p.options.Observer.ObserveSessionIndex(event)
	}
}

func cloneCommittedChange(change CommittedChange) *CommittedChange {
	clone := change
	if change.Summary != nil {
		summary := *change.Summary
		clone.Summary = &summary
	}
	return &clone
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
		if hook := o.provider.options.BeforeApply; hook != nil {
			hook(*task.change)
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

	// The owner worker is the open barrier. Changes queued during blob
	// serialization remain behind this task and are offered after registration.
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		subscription.Close()
		o.removeSubscription(subscription)
		o.provider.observe(Event{Kind: "snapshot_materialization_failed", ProjectID: o.projectID, Reason: "encode_snapshot"})
		return syncengine.OpenedResource{}, fmt.Errorf("encode session index snapshot: %w", err)
	}
	content := syncengine.NewInlineSnapshotContent(encoded)
	if len(encoded) > o.provider.options.InlineSnapshotBytes {
		if o.provider.options.BlobWriter == nil {
			subscription.Close()
			o.removeSubscription(subscription)
			o.provider.observe(Event{Kind: "snapshot_materialization_failed", ProjectID: o.projectID, Reason: "blob_unavailable"})
			return syncengine.OpenedResource{}, fmt.Errorf("session index snapshot exceeds inline limit")
		}
		blobContext, cancelBlob := context.WithCancel(o.ctx)
		stopRequest := context.AfterFunc(request.ctx, cancelBlob)
		descriptor, putErr := o.provider.options.BlobWriter.Put(blobContext, "application/json", encoded)
		stopRequest()
		cancelBlob()
		if putErr != nil {
			subscription.Close()
			o.removeSubscription(subscription)
			// The blob is an open-local materialization. A disconnected client or
			// provider shutdown must not desynchronize a healthy authoritative
			// projection (or its other subscribers). Capacity and writer errors
			// also reject only this Open; only projection/journal failures call
			// invalidate.
			reason := "blob_write"
			if errors.Is(putErr, context.Canceled) || errors.Is(putErr, context.DeadlineExceeded) || request.ctx.Err() != nil || o.ctx.Err() != nil {
				reason = "blob_write_cancelled"
			}
			o.provider.observe(Event{Kind: "snapshot_materialization_failed", ProjectID: o.projectID, Reason: reason})
			return syncengine.OpenedResource{}, putErr
		}
		if descriptorErr := protocol.ValidateBlobDescriptor(descriptor); descriptorErr != nil {
			subscription.Close()
			o.removeSubscription(subscription)
			o.provider.observe(Event{Kind: "snapshot_materialization_failed", ProjectID: o.projectID, Reason: "blob_descriptor"})
			return syncengine.OpenedResource{}, descriptorErr
		}
		content = syncengine.NewBlobSnapshotContent(descriptor)
	}
	o.mu.Lock()
	closed, invalid := o.closed, o.invalid
	o.mu.Unlock()
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
	return syncengine.OpenedResource{
		Snapshot:    syncengine.Snapshot{Content: content, ResourceRevision: resourceRevision},
		StreamEpoch: epoch, Sequence: sequence, Decision: decision, LiveFromSequence: sequence + 1,
		Changes: delivery.Entries, Terminal: delivery.Terminal,
		Close: func() { subscription.Close(); o.removeSubscription(subscription) },
	}, nil
}

func (o *owner) ensureRebuilt() error {
	o.mu.Lock()
	need := !o.initialized || o.invalid
	closed := o.closed
	o.mu.Unlock()
	if closed {
		return ErrProviderClosed
	}
	if !need {
		return nil
	}
	return o.rebuild()
}

func (o *owner) applyCommitted(change CommittedChange) error {
	if change.Kind == CommittedProjectRefresh {
		return o.rebuild()
	}
	if err := o.ensureRebuilt(); err != nil {
		return err
	}

	// Missing summaries are intentionally reloaded by the worker, not by the
	// durable callback. This read is outside the owner state lock.
	var summary SessionSummary
	var hasSummary bool
	if change.Kind != CommittedSessionRemove && change.Summary != nil {
		summary = *change.Summary
		hasSummary = true
	} else if change.Kind != CommittedSessionRemove && change.SessionID != "" {
		loaded, err := o.provider.store.ListStates(sessions.V2ListOptions{All: true})
		if err != nil {
			o.invalidate("durable_reload")
			return fmt.Errorf("reload committed session %q: %w", change.SessionID, err)
		}
		archived := effectiveArchived(loaded)
		for _, state := range loaded {
			if state.ID == change.SessionID && state.ProjectID == o.projectID {
				summary = SummaryFromSession(state, archived[state.ID])
				hasSummary = true
				break
			}
		}
	}

	if change.Kind == CommittedSessionRemove {
		if strings.TrimSpace(change.SessionID) == "" {
			return fmt.Errorf("remove session id is required")
		}
	} else {
		if !hasSummary {
			o.invalidate("missing_summary")
			return fmt.Errorf("committed session upsert has no summary")
		}
		if err := summary.Validate(); err != nil || summary.ProjectID != o.projectID {
			if err == nil {
				err = fmt.Errorf("summary belongs to project %q", summary.ProjectID)
			}
			o.invalidate("invalid_summary")
			return err
		}
	}

	// Read the current run guard and no-op check under the short state lock.
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
	if change.Kind == CommittedSessionRemove {
		operation = Operation{Op: OperationRemove, Key: strings.TrimSpace(change.SessionID)}
		if _, exists := o.summaryByID(operation.Key); !exists {
			o.mu.Unlock()
			return nil
		}
	} else {
		current, exists := o.summaryByID(summary.SessionID)
		// Lifecycle events are accepted only for the run named by the current
		// complete summary. Metadata changes have no run guard.
		if change.RunID != "" && summary.RunID != change.RunID {
			o.mu.Unlock()
			return nil
		}
		if change.Kind == CommittedRunSettled && (!exists || current.RunID != change.RunID) {
			o.mu.Unlock()
			return nil
		}
		if exists && reflectSummaryEqual(summary, current) {
			o.mu.Unlock()
			return nil
		}
		operation = Operation{Op: OperationUpsert, Key: summary.SessionID, Value: &summary}
	}
	o.mu.Unlock()

	// Validation, opaque adapter encoding, and journal append are outside the
	// owner state lock. The worker itself is the serial owner, so no other
	// projector mutation can interleave here.
	if err := operation.Validate(); err != nil {
		o.invalidate("invalid_operation")
		return err
	}
	var currentRevision uint64
	o.mu.Lock()
	currentRevision = o.resourceRevision
	closed, invalid := o.closed, o.invalid
	o.mu.Unlock()
	if closed {
		return ErrProviderClosed
	}
	if invalid {
		return ErrProviderInvalid
	}
	if currentRevision == ^uint64(0) {
		o.invalidate("resource_revision_exhausted")
		return fmt.Errorf("session index resource revision exhausted")
	}
	nextRevision := currentRevision + 1
	changeValue, err := (Change{ResourceRevision: strconv.FormatUint(nextRevision, 10), Operations: []Operation{operation}}).ToResourceChange()
	if err != nil {
		o.invalidate("encode_operation")
		return err
	}
	entry, err := o.journal.Append(changeValue)
	if err != nil {
		// Append validates before advancing sequence. Therefore this failure
		// cannot publish a false change or advance the stream.
		o.invalidate("journal_append")
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
	if o.resourceRevision != currentRevision {
		// This should be impossible while the owner worker is serial, but a
		// defensive invalidation prevents a duplicate project revision if a
		// future path mutates state outside the worker.
		o.mu.Unlock()
		o.invalidate("resource_revision_race")
		return ErrProviderInvalid
	}
	if operation.Op == OperationRemove {
		filtered := make([]SessionSummary, 0, len(o.snapshot.Sessions)-1)
		for _, item := range o.snapshot.Sessions {
			if item.SessionID != operation.Key {
				filtered = append(filtered, item)
			}
		}
		o.snapshot.Sessions = filtered
	} else {
		updated := false
		for i := range o.snapshot.Sessions {
			if o.snapshot.Sessions[i].SessionID == summary.SessionID {
				o.snapshot.Sessions[i] = summary
				updated = true
				break
			}
		}
		if !updated {
			o.snapshot.Sessions = append(o.snapshot.Sessions, summary)
		}
		sortSummaries(o.snapshot.Sessions)
	}
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
	o.provider.observe(Event{Kind: "change_published", ProjectID: o.projectID, SessionID: operation.Key, Sequence: entry.Sequence})
	return nil
}

func reflectSummaryEqual(a, b SessionSummary) bool {
	return a.SessionID == b.SessionID && a.ProjectID == b.ProjectID && a.ParentSessionID == b.ParentSessionID && a.DisplayName == b.DisplayName && a.Archived == b.Archived && a.Status == b.Status && a.RunID == b.RunID && a.ResourceRevision == b.ResourceRevision && a.UpdatedAt.Equal(b.UpdatedAt) && a.HasUnreadResult == b.HasUnreadResult
}

func (o *owner) summaryByID(id string) (SessionSummary, bool) {
	for _, summary := range o.snapshot.Sessions {
		if summary.SessionID == id {
			return summary, true
		}
	}
	return SessionSummary{}, false
}

// rebuild performs all durable I/O before replacing owner state. A failed
// rebuild marks the owner invalid but never creates a journal entry.
func (o *owner) rebuild() error {
	states, err := o.provider.store.ListStates(sessions.V2ListOptions{All: true})
	if err != nil {
		o.invalidate("durable_list")
		o.provider.observe(Event{Kind: "rebuild_failed", ProjectID: o.projectID, Reason: "durable_list"})
		return err
	}
	filtered := make([]sessions.SessionV2, 0, len(states))
	for _, state := range states {
		if state.ProjectID == o.projectID {
			filtered = append(filtered, state)
		}
	}
	archived := effectiveArchived(filtered)
	summaries := make([]SessionSummary, 0, len(filtered))
	for _, state := range filtered {
		summary := SummaryFromSession(state, archived[state.ID])
		if err := summary.Validate(); err != nil {
			o.invalidate("invalid_summary")
			o.provider.observe(Event{Kind: "rebuild_failed", ProjectID: o.projectID, SessionID: state.ID, Reason: "invalid_summary"})
			return err
		}
		summaries = append(summaries, summary)
	}
	sortSummaries(summaries)
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return ErrProviderClosed
	}
	for subscription := range o.subs {
		subscription.Desync(fmt.Errorf("session index rebuilt"))
	}
	o.subs = make(map[*syncengine.LiveSubscription]struct{})
	generation := o.provider.generation.Add(1)
	o.epoch = fmt.Sprintf("%s/%s/%d", o.provider.options.StreamEpoch, o.projectID, generation)
	journal, err := syncengine.NewBoundedJournal(o.epoch, o.provider.options.JournalEntries, o.provider.options.JournalBytes)
	if err != nil {
		o.invalid = true
		o.mu.Unlock()
		return err
	}
	o.journal = journal
	o.snapshot = SessionIndexSnapshot{Sessions: summaries}
	// A new epoch has a fresh project resource revision. Summary revisions are
	// durable row revisions and are intentionally not reused here.
	o.resourceRevision = 0
	o.initialized = true
	o.invalid = false
	o.mu.Unlock()
	o.provider.observe(Event{Kind: "rebuild_completed", ProjectID: o.projectID})
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
		subscription.Desync(fmt.Errorf("session index invalid: %s", reason))
	}
	o.subs = make(map[*syncengine.LiveSubscription]struct{})
	sequence := uint64(0)
	if o.journal != nil {
		sequence = o.journal.LastSequence()
	}
	o.mu.Unlock()
	o.provider.observe(Event{Kind: "invalid", ProjectID: o.projectID, Reason: reason, Sequence: sequence})
}

func (o *owner) removeSubscription(subscription *syncengine.LiveSubscription) {
	o.mu.Lock()
	delete(o.subs, subscription)
	o.mu.Unlock()
}

func (o *owner) closeState() {
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		for subscription := range o.subs {
			subscription.Close()
		}
		o.subs = make(map[*syncengine.LiveSubscription]struct{})
	}
	o.mu.Unlock()
}

func (o *owner) close() {
	o.cancel()
	<-o.workerDone
}

func (p *Provider) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		owners := make([]*owner, 0, len(p.owners))
		for _, o := range p.owners {
			owners = append(owners, o)
		}
		p.mu.Unlock()
		for _, o := range owners {
			o.close()
		}
	})
}

func cloneSnapshot(snapshot SessionIndexSnapshot) SessionIndexSnapshot {
	clone := SessionIndexSnapshot{Sessions: append([]SessionSummary(nil), snapshot.Sessions...)}
	if clone.Sessions == nil {
		clone.Sessions = []SessionSummary{}
	}
	return clone
}

func effectiveArchived(states []sessions.SessionV2) map[string]bool {
	byID := make(map[string]sessions.SessionV2, len(states))
	for _, state := range states {
		byID[state.ID] = state
	}
	result := make(map[string]bool, len(states))
	var visit func(string, map[string]bool) bool
	visit = func(id string, path map[string]bool) bool {
		if value, ok := result[id]; ok {
			return value
		}
		if path[id] {
			return false
		}
		state, ok := byID[id]
		if !ok {
			return false
		}
		path[id] = true
		value := state.Archived
		if !value && strings.TrimSpace(state.ParentSessionID) != "" {
			value = visit(strings.TrimSpace(state.ParentSessionID), path)
		}
		delete(path, id)
		result[id] = value
		return value
	}
	for _, state := range states {
		visit(state.ID, make(map[string]bool))
	}
	return result
}

var _ syncengine.ResourceProvider = (*Provider)(nil)
