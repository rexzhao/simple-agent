package wsgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

const (
	DefaultMaxSubscriptions    = 64
	DefaultMaxInflightCommands = 16
	DefaultMaxCachedCommands   = 4096
)

// These aliases keep the gateway assembly API convenient while the command
// contract itself remains in the transport-neutral internal/commands package.
type CommandDefinition = commands.CommandDefinition
type CommandRegistry = commands.Registry

func NewCommandRegistry(definitions ...commands.CommandDefinition) (*commands.Registry, error) {
	return commands.NewRegistry(definitions...)
}

var (
	ErrSubscriptionLimit = errors.New("subscription limit exceeded")
	ErrInflightLimit     = errors.New("inflight command limit exceeded")
	ErrCommandCacheFull  = errors.New("command idempotency cache is full")
)

// DispatcherOptions wires the gateway to transport-neutral command and
// resource registries. OwnerContext is deliberately separate from the
// per-connection Handler context: a reconnecting client must be able to join
// an accepted command without cancelling its shared execution.
type DispatcherOptions struct {
	Engine              *syncengine.Engine
	Commands            *commands.Registry
	OwnerContext        context.Context
	Observer            Observer
	MaxSubscriptions    int
	MaxInflightCommands int
	MaxCachedCommands   int
	IDGenerator         func(string) (string, error)
}

// Dispatcher is the combined command/subscription/ack handler. It stores
// state per ConnectionID and removes it when the connection context ends.
// maxCached is a hard capacity for this process/server epoch. Entries are
// never evicted because evicting an unsafe command could cause a retry to
// execute its side effect twice. Durable dedupe/outbox support belongs to a
// later application stage.
type Dispatcher struct {
	engine       *syncengine.Engine
	commands     *commands.Registry
	ownerContext context.Context
	observer     Observer
	maxSubs      int
	maxInflight  int
	maxCached    int
	idGenerator  func(string) (string, error)
	observerMu   sync.Mutex

	mu         sync.Mutex
	states     map[string]*connectionState
	commandsMu sync.Mutex
	requests   map[commandCacheKey]*sharedCommand
	sequence   atomic.Uint64
}

// NewDispatcher creates a closed-registry dispatcher. A nil command registry
// is rejected rather than silently accepting arbitrary command names.
func NewDispatcher(options DispatcherOptions) (*Dispatcher, error) {
	if options.Engine == nil {
		return nil, errors.New("sync engine is required")
	}
	if options.Commands == nil {
		return nil, errors.New("command registry is required")
	}
	if options.OwnerContext == nil {
		options.OwnerContext = context.Background()
	}
	if options.MaxSubscriptions == 0 {
		options.MaxSubscriptions = DefaultMaxSubscriptions
	}
	if options.MaxInflightCommands == 0 {
		options.MaxInflightCommands = DefaultMaxInflightCommands
	}
	if options.MaxCachedCommands == 0 {
		options.MaxCachedCommands = DefaultMaxCachedCommands
	}
	if options.MaxSubscriptions < 1 || options.MaxInflightCommands < 1 || options.MaxCachedCommands < 1 {
		return nil, errors.New("dispatcher limits must be positive")
	}
	idGenerator := options.IDGenerator
	if idGenerator == nil {
		idGenerator = func(prefix string) (string, error) {
			value := atomic.AddUint64(&dispatcherID, 1)
			return fmt.Sprintf("%s-%d", prefix, value), nil
		}
	}
	return &Dispatcher{
		engine:       options.Engine,
		commands:     options.Commands,
		ownerContext: options.OwnerContext,
		observer:     options.Observer,
		maxSubs:      options.MaxSubscriptions,
		maxInflight:  options.MaxInflightCommands,
		maxCached:    options.MaxCachedCommands,
		idGenerator:  idGenerator,
		states:       make(map[string]*connectionState),
		requests:     make(map[commandCacheKey]*sharedCommand),
	}, nil
}

var dispatcherID uint64

// NewSyncHandler is a descriptive alias used by callers assembling a
// WebSocket gateway.
func NewSyncHandler(options DispatcherOptions) (*Dispatcher, error) {
	return NewDispatcher(options)
}

// ConnectionCount, SubscriptionCount and InflightCommandCount expose only
// bounded current counts for metrics/tests; they do not expose connection or
// provider objects.
func (d *Dispatcher) ConnectionCount() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.states)
}

func (d *Dispatcher) SubscriptionCount() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	states := make([]*connectionState, 0, len(d.states))
	for _, state := range d.states {
		states = append(states, state)
	}
	d.mu.Unlock()
	count := 0
	for _, state := range states {
		count += state.subscriptionCount()
	}
	return count
}

func (d *Dispatcher) InflightCommandCount() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	states := make([]*connectionState, 0, len(d.states))
	for _, state := range d.states {
		states = append(states, state)
	}
	d.mu.Unlock()
	count := 0
	for _, state := range states {
		count += state.inflightCount()
	}
	return count
}

func (d *Dispatcher) CommandCacheCount() int {
	if d == nil {
		return 0
	}
	d.commandsMu.Lock()
	defer d.commandsMu.Unlock()
	return len(d.requests)
}

func (d *Dispatcher) Handle(ctx context.Context, connection Connection, message protocol.Message) error {
	if d == nil || connection == nil || message == nil {
		return ErrConnectionClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	state := d.stateFor(ctx, connection)
	if state == nil {
		return ErrConnectionClosed
	}
	state.mu.Lock()
	closed := state.closed
	state.mu.Unlock()
	if closed || state.ctx.Err() != nil || ctx.Err() != nil {
		return ErrConnectionClosed
	}
	switch typed := message.(type) {
	case protocol.CommandMessage:
		return d.handleCommand(state, typed)
	case protocol.SubscribeMessage:
		return d.handleSubscribe(state, typed)
	case protocol.UnsubscribeMessage:
		return d.handleUnsubscribe(state, typed)
	case protocol.AckMessage:
		return d.handleAck(state, typed)
	default:
		return ErrUnsupportedMessage
	}
}

func (d *Dispatcher) stateFor(ctx context.Context, connection Connection) *connectionState {
	key := connection.Info().ConnectionID
	if strings.TrimSpace(key) == "" {
		// ConnectionID is the production identity. For pointer-backed fake
		// transports, use object identity rather than Principal or ClientID:
		// neither authenticated principal nor client ID identifies one live
		// connection, and using either would merge per-connection state.
		key = pointerConnectionKey(connection)
	}
	if key == "" {
		return nil
	}
	d.mu.Lock()
	if state := d.states[key]; state != nil {
		d.mu.Unlock()
		return state
	}
	state := &connectionState{
		dispatcher:   d,
		key:          key,
		connection:   connection,
		ctx:          ctx,
		subs:         make(map[string]*subscription),
		pendingSubs:  make(map[string]*pendingSubscription),
		pendingTasks: make(map[*pendingSubscription]struct{}),
		inflight:     make(map[string]string),
		done:         make(chan struct{}),
	}
	d.states[key] = state
	d.mu.Unlock()
	go d.cleanupState(state)
	return state
}

func pointerConnectionKey(connection Connection) string {
	value := reflect.ValueOf(connection)
	if !value.IsValid() {
		return ""
	}
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		if value.IsNil() {
			return ""
		}
		return fmt.Sprintf("%T@%x", connection, value.Pointer())
	default:
		return ""
	}
}

func (d *Dispatcher) cleanupState(state *connectionState) {
	<-state.ctx.Done()
	state.close()
	d.mu.Lock()
	if d.states[state.key] == state {
		delete(d.states, state.key)
	}
	d.mu.Unlock()
}

func (d *Dispatcher) handleSubscribe(state *connectionState, message protocol.SubscribeMessage) error {
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return ErrConnectionClosed
	}
	if _, exists := state.subs[message.Payload.SubscriptionID]; exists {
		state.mu.Unlock()
		d.sendError(state, "subscription_exists", "subscription ID is already active", "")
		return nil
	}
	if _, pending := state.pendingSubs[message.Payload.SubscriptionID]; pending {
		state.mu.Unlock()
		d.sendError(state, "subscription_exists", "subscription ID is already active", "")
		return nil
	}
	if len(state.subs)+state.pendingCount >= d.maxSubs {
		state.mu.Unlock()
		d.sendError(state, "subscription_limit", "subscription limit exceeded", "")
		return nil
	}
	openContext, openCancel := context.WithCancel(state.ctx)
	pending := &pendingSubscription{
		id:     message.Payload.SubscriptionID,
		ctx:    openContext,
		cancel: openCancel,
		done:   make(chan struct{}),
	}
	pending.reserved = true
	state.pendingSubs[message.Payload.SubscriptionID] = pending
	state.pendingTasks[pending] = struct{}{}
	state.pendingCount++
	state.mu.Unlock()

	// Admission is the only synchronous part of subscribe. Provider Open may
	// perform durable I/O or wait for an owner barrier and must not block the
	// gateway reader from processing pong, commands or unsubscribe.
	go d.openSubscription(state, message, pending)
	return nil
}

func (d *Dispatcher) openSubscription(state *connectionState, message protocol.SubscribeMessage, pending *pendingSubscription) {
	defer close(pending.done)
	defer state.releasePending(pending)
	defer pending.cancel()

	principal := syncengine.Principal{ID: state.connection.Info().Principal}
	opened, err := d.engine.Open(pending.ctx, principal, message.Payload.Resource, message.Payload.Resume)
	if err != nil {
		if pending.cancelled.Load() || pending.ctx.Err() != nil {
			return
		}
		d.sendError(state, syncErrorCode(err), "subscription could not be opened", "")
		return
	}
	if pending.cancelled.Load() {
		opened.Close()
		return
	}

	var deliveryState *syncengine.SubscriptionState
	switch opened.Decision.Action {
	case syncengine.SyncActionSnapshot, syncengine.SyncActionResync:
		deliveryState, err = syncengine.NewSubscriptionStateForSnapshot(opened.StreamEpoch, opened.Sequence)
	case syncengine.SyncActionCurrent:
		resumeSequence, parseErr := protocol.ParseUint64Decimal(string(message.Payload.Resume.Sequence))
		if parseErr != nil {
			err = parseErr
		} else {
			deliveryState, err = syncengine.NewSubscriptionStateForLive(opened.StreamEpoch, resumeSequence)
		}
	case syncengine.SyncActionReplay:
		resumeSequence, parseErr := protocol.ParseUint64Decimal(string(message.Payload.Resume.Sequence))
		if parseErr != nil {
			err = parseErr
		} else {
			deliveryState, err = syncengine.NewSubscriptionStateForReplay(opened.StreamEpoch, resumeSequence)
		}
	default:
		err = errors.New("unknown sync decision")
	}
	if err != nil {
		opened.Close()
		if !pending.cancelled.Load() {
			d.sendError(state, "subscription_open_failed", "subscription could not be initialized", "")
		}
		return
	}

	subContext, subCancel := context.WithCancel(state.ctx)
	sub := &subscription{
		id:       message.Payload.SubscriptionID,
		resource: message.Payload.Resource,
		opened:   opened,
		state:    deliveryState,
		ctx:      subContext,
		cancel:   subCancel,
		done:     make(chan struct{}),
	}

	// Activate before queueing any ACK-bearing frame so a client can look up the
	// subscription as soon as the writer confirms snapshot/replay delivery. The
	// barrier lets unsubscribe wait for an already-started initial delivery
	// without waiting for provider Open itself.
	pending.initialMu.Lock()
	defer pending.initialMu.Unlock()
	state.mu.Lock()
	if state.closed || pending.cancelled.Load() || pending.ctx.Err() != nil || state.pendingSubs[sub.id] != pending {
		state.mu.Unlock()
		opened.Close()
		return
	}
	delete(state.pendingSubs, sub.id)
	if pending.reserved {
		pending.reserved = false
		state.pendingCount--
	}
	sub.initialMu = &pending.initialMu
	state.subs[sub.id] = sub
	state.mu.Unlock()
	// The provider Open context is only an admission context. The opened
	// resource's Close function owns the live source after this point.
	pending.cancel()
	if owned, ok := state.connection.(*connection); ok {
		owned.clearSubscriptionDesynced(sub.id)
	}
	d.observe(Event{Kind: EventSubscriptionOpened, SubscriptionID: sub.id, ResourceType: sub.resource.Type, ResourceID: sub.resource.ID, StreamEpoch: opened.StreamEpoch, Sequence: string(decimal(opened.Sequence)), ResourceRevision: string(opened.Snapshot.ResourceRevision), ConnectionID: state.connection.Info().ConnectionID, ClientID: state.connection.Info().ClientID, SubscriptionsCurrent: state.subscriptionCount()})
	if pending.cancelled.Load() {
		d.removeSubscription(state, sub, false)
		return
	}
	if err := d.send(state, protocol.SubscribedMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribed, ID: d.nextID("subscribed")},
		Payload: protocol.SubscribedPayload{
			SubscriptionID: sub.id,
			Resource:       sub.resource,
			StreamEpoch:    opened.StreamEpoch,
			Sequence:       decimal(opened.Sequence),
		},
	}, SendOptions{}); err != nil {
		d.removeSubscription(state, sub, false)
		return
	}

	if pending.cancelled.Load() {
		d.removeSubscription(state, sub, false)
		return
	}
	if opened.Decision.Action == syncengine.SyncActionResync {
		if err := d.send(state, protocol.ResyncRequiredMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeResyncRequired, ID: d.nextID("resync")},
			Payload:  protocol.ResyncRequiredPayload{SubscriptionID: sub.id, Resource: sub.resource, Reason: string(opened.Decision.Reason)},
		}, SendOptions{SubscriptionID: sub.id}); err != nil {
			d.removeSubscription(state, sub, false)
			return
		}
	}
	if opened.Decision.Action == syncengine.SyncActionSnapshot || opened.Decision.Action == syncengine.SyncActionResync {
		if pending.cancelled.Load() {
			d.removeSubscription(state, sub, false)
			return
		}
		snapshot := snapshotMessage(sub, opened)
		snapshot.Envelope.ID = d.nextID("snapshot")
		if err := d.send(state, snapshot, SendOptions{SubscriptionID: sub.id, OnWritten: func() {
			_ = sub.state.MarkSent(opened.StreamEpoch, opened.Sequence)
		}}); err != nil {
			d.removeSubscription(state, sub, false)
			return
		}
	}
	if opened.Decision.Action == syncengine.SyncActionReplay {
		for _, entry := range opened.Decision.Entries {
			if pending.cancelled.Load() {
				d.removeSubscription(state, sub, false)
				return
			}
			change := changeMessage(sub, entry)
			change.Envelope.ID = d.nextID("change")
			if err := d.send(state, change, SendOptions{SubscriptionID: sub.id, OnWritten: func() {
				_ = sub.state.MarkSent(entry.StreamEpoch, entry.Sequence)
			}}); err != nil {
				d.removeSubscription(state, sub, false)
				return
			}
		}
	}

	if sub.startPump() {
		go d.pumpSubscription(state, sub)
	}
}

func snapshotMessage(sub *subscription, opened syncengine.OpenedResource) protocol.SnapshotMessage {
	content := opened.Snapshot.Content
	wireContent := protocol.SnapshotContent{}
	if descriptor, ok := content.BlobDescriptor(); ok {
		wireContent.Blob = &descriptor
	} else {
		wireContent.Inline = content.InlineBytes()
	}
	return protocol.SnapshotMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSnapshot},
		Payload: protocol.SnapshotPayload{
			SubscriptionID:   sub.id,
			Resource:         sub.resource,
			StreamEpoch:      opened.StreamEpoch,
			Sequence:         decimal(opened.Sequence),
			ResourceRevision: opened.Snapshot.ResourceRevision,
			Content:          wireContent,
		},
	}
}

func changeMessage(sub *subscription, entry syncengine.JournalEntry) protocol.ChangeMessage {
	operations := make([]protocol.ChangeOperation, len(entry.Change.Operations))
	copy(operations, entry.Change.Operations)
	return protocol.ChangeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeChange},
		Payload: protocol.ChangePayload{
			SubscriptionID:   sub.id,
			Resource:         sub.resource,
			StreamEpoch:      entry.StreamEpoch,
			Sequence:         decimal(entry.Sequence),
			PreviousSequence: decimal(entry.PreviousSequence),
			ResourceRevision: entry.Change.ResourceRevision,
			Operations:       operations,
		},
	}
}

func (d *Dispatcher) pumpSubscription(state *connectionState, sub *subscription) {
	defer close(sub.done)
	changes := sub.opened.Changes
	terminal := sub.opened.Terminal
	for changes != nil || terminal != nil {
		select {
		case <-sub.ctx.Done():
			return
		case entry, ok := <-changes:
			if !ok {
				changes = nil
				continue
			}
			change := changeMessage(sub, entry)
			change.Envelope.ID = d.nextID("change")
			err := d.send(state, change, SendOptions{SubscriptionID: sub.id, OnWritten: func() {
				_ = sub.state.MarkSent(entry.StreamEpoch, entry.Sequence)
			}})
			if err != nil {
				// Any send error means this subscription can no longer make a
				// continuity claim. Desync is the normal bounded-queue path;
				// transport-neutral connections may return any other terminal
				// error, and must not leave the provider registered.
				d.removeSubscription(state, sub, false)
				return
			}
		case end, ok := <-terminal:
			if !ok {
				terminal = nil
				continue
			}
			if end.Reason == syncengine.LiveTerminalOverflow || end.Reason == syncengine.LiveTerminalSequence {
				_ = d.send(state, protocol.ResyncRequiredMessage{
					Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeResyncRequired, ID: d.nextID("resync")},
					Payload:  protocol.ResyncRequiredPayload{SubscriptionID: sub.id, Resource: sub.resource, Reason: string(end.Reason)},
				}, SendOptions{SubscriptionID: sub.id})
				d.observe(Event{Kind: EventSubscriptionResync, SubscriptionID: sub.id, ResourceType: sub.resource.Type, ResourceID: sub.resource.ID, Reason: string(end.Reason), ConnectionID: state.connection.Info().ConnectionID})
			}
			d.removeSubscription(state, sub, false)
			return
		}
	}
	// A provider that closes both delivery channels without a terminal value
	// has ended its source unexpectedly. Remove it instead of leaving an
	// unreachable subscription in the connection map.
	d.removeSubscription(state, sub, false)
}

func (d *Dispatcher) handleUnsubscribe(state *connectionState, message protocol.UnsubscribeMessage) error {
	state.mu.Lock()
	sub := state.subs[message.Payload.SubscriptionID]
	pendingCancel := state.pendingSubs[message.Payload.SubscriptionID]
	if sub != nil {
		delete(state.subs, sub.id)
	}
	if sub == nil && pendingCancel != nil {
		// Keep the reservation until the Open call unwinds, but cancel its
		// admission context while still holding the state lock. This closes
		// the race where a slow Open could otherwise be inserted after the
		// unsubscribe was observed.
		pendingCancel.cancelled.Store(true)
		pendingCancel.cancel()
	}
	state.mu.Unlock()
	if sub == nil && pendingCancel != nil {
		// Open itself may be arbitrarily slow, but once initial frames begin
		// they must finish before the unsubscribed acknowledgement is queued.
		// This is a short queue/send barrier, not a wait for provider I/O.
		pendingCancel.initialMu.Lock()
		pendingCancel.initialMu.Unlock()
	}
	if sub != nil {
		d.stopSubscriptionAfterInitial(sub)
		<-sub.done
		d.observe(Event{Kind: EventSubscriptionClosed, SubscriptionID: sub.id, ResourceType: sub.resource.Type, ResourceID: sub.resource.ID, Reason: "unsubscribe", ConnectionID: state.connection.Info().ConnectionID, SubscriptionsCurrent: state.subscriptionCount()})
	}
	// V1 chooses idempotent unsubscribe: an unknown/already closed ID still
	// receives the same unsubscribed acknowledgement.
	return d.send(state, protocol.UnsubscribedMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeUnsubscribed, ID: d.nextID("unsubscribed")},
		Payload:  protocol.UnsubscribedPayload{SubscriptionID: message.Payload.SubscriptionID},
	}, SendOptions{})
}

func (d *Dispatcher) handleAck(state *connectionState, message protocol.AckMessage) error {
	state.mu.Lock()
	sub := state.subs[message.Payload.SubscriptionID]
	state.mu.Unlock()
	if sub == nil {
		d.sendError(state, "subscription_not_found", "subscription is not active", "")
		return nil
	}
	sequence, err := protocol.ParseUint64Decimal(string(message.Payload.Sequence))
	if err != nil {
		err = syncengine.ErrAckAhead
	}
	if err == nil {
		_, err = sub.state.Ack(message.Payload.StreamEpoch, sequence)
	}
	if err != nil {
		code := ackErrorCode(err)
		d.observe(Event{Kind: EventAckRejected, Code: code, SubscriptionID: sub.id, ResourceType: sub.resource.Type, ResourceID: sub.resource.ID, StreamEpoch: message.Payload.StreamEpoch, Sequence: string(message.Payload.Sequence), ConnectionID: state.connection.Info().ConnectionID})
		d.sendError(state, code, "acknowledgement was rejected", "")
	}
	return nil
}

func (d *Dispatcher) handleCommand(state *connectionState, message protocol.CommandMessage) error {
	request := commands.CommandRequest{
		Name: message.Payload.Name, SchemaVersion: message.Payload.SchemaVersion, RequestID: message.Payload.RequestID,
		ExpectedRevision: message.Payload.ExpectedRevision, Arguments: append(json.RawMessage(nil), message.Payload.Arguments...), Principal: state.connection.Info().Principal,
	}
	fingerprint, err := commands.Fingerprint(request)
	if err != nil {
		d.sendCommandFailure(state, request.RequestID, "invalid_arguments", "invalid command arguments")
		return nil
	}
	cacheKey := commandCacheKey{Principal: request.Principal, RequestID: request.RequestID}
	// Check the global cache before looking up the current schema. A reused
	// request ID must remain an idempotency conflict even if the second
	// message also changes its command name or schema version.
	d.commandsMu.Lock()
	existing := d.requests[cacheKey]
	d.commandsMu.Unlock()
	if existing != nil {
		if existing.fingerprint != fingerprint {
			d.observe(Event{Kind: EventCommandConflict, CommandName: request.Name, RequestID: request.RequestID, ConnectionID: state.connection.Info().ConnectionID})
			d.sendCommandFailure(state, request.RequestID, "idempotency_conflict", "request ID was used with different command content")
			return nil
		}
		return d.joinSharedCommand(state, existing, request)
	}

	definition, err := d.commands.Definition(message.Payload.Name, message.Payload.SchemaVersion)
	if err != nil {
		d.sendCommandFailure(state, request.RequestID, registryErrorCode(err), registryErrorMessage(err))
		return nil
	}
	if err := d.commands.Validate(definition, message.Payload.Arguments); err != nil {
		d.sendCommandFailure(state, request.RequestID, registryErrorCode(err), registryErrorMessage(err))
		return nil
	}
	d.commandsMu.Lock()
	shared := d.requests[cacheKey]
	if shared != nil && shared.fingerprint != fingerprint {
		d.commandsMu.Unlock()
		d.observe(Event{Kind: EventCommandConflict, CommandName: request.Name, RequestID: request.RequestID, ConnectionID: state.connection.Info().ConnectionID})
		d.sendCommandFailure(state, request.RequestID, "idempotency_conflict", "request ID was used with different command content")
		return nil
	}
	if shared == nil {
		if len(d.requests) >= d.maxCached {
			d.commandsMu.Unlock()
			d.observe(Event{Kind: EventCommandCacheRejected, CommandName: request.Name, RequestID: request.RequestID, Code: "command_cache_full", ConnectionID: state.connection.Info().ConnectionID, CommandCacheCurrent: d.CommandCacheCount()})
			d.sendCommandFailure(state, request.RequestID, "command_cache_full", "command idempotency cache is full")
			return nil
		}
		state.mu.Lock()
		if len(state.inflight) >= d.maxInflight {
			state.mu.Unlock()
			d.commandsMu.Unlock()
			d.sendCommandFailure(state, request.RequestID, "inflight_limit", "inflight command limit exceeded")
			return nil
		}
		state.inflight[request.RequestID] = fingerprint
		state.mu.Unlock()
		shared = &sharedCommand{fingerprint: fingerprint, request: request.Clone(), definition: definition, done: make(chan struct{}), startedAt: time.Now()}
		d.requests[cacheKey] = shared
		d.commandsMu.Unlock()
		// Delivery failure does not cancel the owner-scoped execution; another
		// connection in this server epoch can recover the cached result.
		_ = d.sendCommandAccepted(state, request.RequestID)
		d.observe(Event{Kind: EventCommandAccepted, CommandName: request.Name, RequestID: request.RequestID, ConnectionID: state.connection.Info().ConnectionID, InflightCommandsCurrent: state.inflightCount(), CommandCacheCurrent: d.CommandCacheCount()})
		go d.executeCommand(shared)
		d.deliverResult(state, shared)
		return nil
	}
	d.commandsMu.Unlock()
	return d.joinSharedCommand(state, shared, request)
}

func (d *Dispatcher) joinSharedCommand(state *connectionState, shared *sharedCommand, request commands.CommandRequest) error {
	state.mu.Lock()
	_, attached := state.inflight[request.RequestID]
	if !attached && !shared.finished() {
		if len(state.inflight) >= d.maxInflight {
			state.mu.Unlock()
			d.sendCommandFailure(state, request.RequestID, "inflight_limit", "inflight command limit exceeded")
			return nil
		}
		state.inflight[request.RequestID] = shared.fingerprint
	}
	state.mu.Unlock()
	if attached {
		// A duplicate on the same connection joins the existing delivery and
		// consumes no additional slot or goroutine.
		d.observe(Event{Kind: EventCommandDeduped, CommandName: request.Name, RequestID: request.RequestID, ConnectionID: state.connection.Info().ConnectionID})
		return nil
	}
	d.observe(Event{Kind: EventCommandDeduped, CommandName: request.Name, RequestID: request.RequestID, ConnectionID: state.connection.Info().ConnectionID})
	if !shared.finished() {
		_ = d.sendCommandAccepted(state, request.RequestID)
	}
	d.deliverResult(state, shared)
	return nil
}

func (d *Dispatcher) sendCommandAccepted(state *connectionState, requestID string) error {
	return d.send(state, protocol.CommandAcceptedMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommandAccepted, ID: d.nextID("accepted")},
		Payload:  protocol.CommandAcceptedPayload{RequestID: requestID},
	}, SendOptions{})
}

func (d *Dispatcher) executeCommand(shared *sharedCommand) {
	var result json.RawMessage
	var commandErr *protocol.CommandError
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				commandErr = &protocol.CommandError{Code: "command_panic", Message: "command execution failed"}
			}
		}()
		var err error
		result, err = shared.definition.Execute(d.ownerContext, shared.request.Clone())
		if err != nil {
			commandErr = commandErrorFrom(err)
			return
		}
		if len(result) > 0 && (strings.TrimSpace(string(result)) == "null" || !json.Valid(result)) {
			commandErr = &protocol.CommandError{Code: "invalid_command_result", Message: "command returned invalid result"}
			return
		}
		result = append(json.RawMessage(nil), result...)
	}()
	shared.finish(result, commandErr)
	d.observe(Event{Kind: EventCommandCompleted, CommandName: shared.request.Name, RequestID: shared.request.RequestID, Code: commandStatus(commandErr), Duration: time.Since(shared.startedAt), CommandCacheCurrent: d.CommandCacheCount()})
}

func commandStatus(err *protocol.CommandError) string {
	if err == nil {
		return string(protocol.CommandStatusSucceeded)
	}
	return string(protocol.CommandStatusFailed)
}

func commandErrorFrom(err error) *protocol.CommandError {
	var typed *commands.RegistryError
	if errors.As(err, &typed) && typed != nil {
		return &protocol.CommandError{Code: typed.Code, Message: typed.Message}
	}
	return &protocol.CommandError{Code: "command_failed", Message: "command execution failed"}
}

func (d *Dispatcher) deliverResult(state *connectionState, shared *sharedCommand) {
	go func() {
		select {
		case <-shared.done:
		case <-state.ctx.Done():
			state.release(shared.request.RequestID, shared.fingerprint)
			return
		}
		outcome := shared.outcome()
		payload := protocol.CommandResultPayload{RequestID: shared.request.RequestID, Status: protocol.CommandStatusSucceeded, Result: outcome.result}
		if outcome.err != nil {
			payload.Status = protocol.CommandStatusFailed
			payload.Error = outcome.err
		}
		_ = d.send(state, protocol.CommandResultMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommandResult, ID: d.nextID("result")},
			Payload:  payload,
		}, SendOptions{})
		state.release(shared.request.RequestID, shared.fingerprint)
	}()
}

func (d *Dispatcher) send(state *connectionState, message protocol.Message, options SendOptions) error {
	if owned, ok := state.connection.(OwnedConnection); ok {
		return owned.SendWithOptions(message, options)
	}
	if err := state.connection.Send(message); err != nil {
		return err
	}
	if options.OnWritten != nil {
		options.OnWritten()
	}
	return nil
}

func (d *Dispatcher) sendError(state *connectionState, code, message, requestID string) error {
	var request *string
	if requestID != "" {
		request = &requestID
	}
	return d.send(state, protocol.ErrorMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeError, ID: d.nextID("error")},
		Payload:  protocol.ErrorPayload{Code: code, Message: message, RequestID: request},
	}, SendOptions{})
}

// sendCommandFailure implements the 5.7 command state machine: once a
// command frame has decoded successfully, business-level rejection is a
// failed command_result rather than a generic protocol ErrorMessage.
func (d *Dispatcher) sendCommandFailure(state *connectionState, requestID, code, message string) error {
	return d.send(state, protocol.CommandResultMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommandResult, ID: d.nextID("result")},
		Payload: protocol.CommandResultPayload{
			RequestID: requestID,
			Status:    protocol.CommandStatusFailed,
			Error:     &protocol.CommandError{Code: code, Message: message},
		},
	}, SendOptions{})
}

func (d *Dispatcher) removeSubscription(state *connectionState, sub *subscription, wait bool) {
	state.mu.Lock()
	removed := false
	if state.subs[sub.id] == sub {
		delete(state.subs, sub.id)
		removed = true
	}
	state.mu.Unlock()
	d.stopSubscription(sub)
	if wait {
		<-sub.done
	}
	if !removed {
		return
	}
	d.observe(Event{Kind: EventSubscriptionClosed, SubscriptionID: sub.id, ResourceType: sub.resource.Type, ResourceID: sub.resource.ID, Reason: "closed", ConnectionID: state.connection.Info().ConnectionID, SubscriptionsCurrent: state.subscriptionCount()})
}

func (d *Dispatcher) stopSubscription(sub *subscription) {
	sub.stopOnce.Do(func() {
		if sub.cancel != nil {
			sub.cancel()
		}
		sub.opened.Close()
		sub.mu.Lock()
		sub.stopped = true
		if !sub.started {
			close(sub.done)
		}
		sub.mu.Unlock()
	})
}

func (d *Dispatcher) stopSubscriptionAfterInitial(sub *subscription) {
	if sub != nil && sub.initialMu != nil {
		sub.initialMu.Lock()
		sub.initialMu.Unlock()
	}
	d.stopSubscription(sub)
}

func (d *Dispatcher) observe(event Event) {
	if d == nil || d.observer == nil {
		return
	}
	d.observerMu.Lock()
	defer d.observerMu.Unlock()
	d.observer.Observe(event)
}

func (d *Dispatcher) nextID(prefix string) string {
	if id, err := d.idGenerator(prefix); err == nil && id != "" {
		return id
	}
	return fmt.Sprintf("%s-%d", prefix, d.sequence.Add(1))
}

func (s *connectionState) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		subs := make([]*subscription, 0, len(s.subs))
		pendingTasks := make([]*pendingSubscription, 0, len(s.pendingTasks))
		for _, sub := range s.subs {
			subs = append(subs, sub)
		}
		for pending := range s.pendingTasks {
			pendingTasks = append(pendingTasks, pending)
			pending.reserved = false
		}
		s.subs = make(map[string]*subscription)
		s.pendingSubs = make(map[string]*pendingSubscription)
		s.pendingTasks = make(map[*pendingSubscription]struct{})
		s.pendingCount = 0
		s.inflight = make(map[string]string)
		s.mu.Unlock()
		for _, pending := range pendingTasks {
			pending.cancelled.Store(true)
			pending.cancel()
		}
		for _, pending := range pendingTasks {
			<-pending.done
		}
		for _, sub := range subs {
			s.dispatcher.stopSubscriptionAfterInitial(sub)
			<-sub.done
			s.dispatcher.observe(Event{Kind: EventSubscriptionClosed, SubscriptionID: sub.id, ResourceType: sub.resource.Type, ResourceID: sub.resource.ID, Reason: "connection_closed", ConnectionID: s.connection.Info().ConnectionID})
		}
		close(s.done)
	})
}

func (s *connectionState) release(requestID, fingerprint string) {
	s.mu.Lock()
	if s.inflight[requestID] == fingerprint {
		delete(s.inflight, requestID)
	}
	s.mu.Unlock()
}

func (s *connectionState) releasePending(state *pendingSubscription) {
	s.mu.Lock()
	if s.pendingSubs[state.id] == state {
		delete(s.pendingSubs, state.id)
	}
	if state.reserved {
		state.reserved = false
		s.pendingCount--
	}
	delete(s.pendingTasks, state)
	s.mu.Unlock()
}

func (s *connectionState) subscriptionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}
func (s *connectionState) inflightCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight)
}

func decimal(value uint64) protocol.Sequence { return protocol.Sequence(strconv.FormatUint(value, 10)) }

func syncErrorCode(err error) string {
	switch {
	case errors.Is(err, syncengine.ErrUnknownResourceType):
		return "unknown_resource"
	case errors.Is(err, syncengine.ErrProviderNotRegistered):
		return "resource_not_registered"
	case errors.Is(err, syncengine.ErrInvalidResourceKey):
		return "invalid_resource"
	default:
		return "subscription_open_failed"
	}
}

func ackErrorCode(err error) string {
	switch {
	case errors.Is(err, syncengine.ErrAckEpochMismatch):
		return "ack_epoch_mismatch"
	case errors.Is(err, syncengine.ErrAckBelowFloor):
		return "ack_below_floor"
	case errors.Is(err, syncengine.ErrAckAhead):
		return "ack_ahead"
	case errors.Is(err, syncengine.ErrAckRegression):
		return "ack_regression"
	default:
		return "ack_invalid"
	}
}

func registryErrorCode(err error) string {
	var typed *commands.RegistryError
	if errors.As(err, &typed) && typed != nil {
		return typed.Code
	}
	return "invalid_command"
}

func registryErrorMessage(err error) string {
	var typed *commands.RegistryError
	if errors.As(err, &typed) && typed != nil && typed.Message != "" {
		return typed.Message
	}
	return "command was rejected"
}

type subscription struct {
	id        string
	resource  protocol.ResourceKey
	opened    syncengine.OpenedResource
	state     *syncengine.SubscriptionState
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	initialMu *sync.Mutex
	stopOnce  sync.Once
	mu        sync.Mutex
	started   bool
	stopped   bool
}

// commandCacheKey is intentionally principal-scoped. Principal is opaque and
// compared byte-for-byte; it is never normalized or included in logs.
type commandCacheKey struct {
	Principal string
	RequestID string
}

func (s *subscription) startPump() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.started = true
	return true
}

type connectionState struct {
	dispatcher   *Dispatcher
	key          string
	connection   Connection
	ctx          context.Context
	done         chan struct{}
	closeOnce    sync.Once
	mu           sync.Mutex
	closed       bool
	subs         map[string]*subscription
	pendingSubs  map[string]*pendingSubscription
	pendingTasks map[*pendingSubscription]struct{}
	pendingCount int
	inflight     map[string]string
}

type pendingSubscription struct {
	id        string
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	cancelled atomic.Bool
	initialMu sync.Mutex
	reserved  bool
}

type sharedCommand struct {
	fingerprint  string
	request      commands.CommandRequest
	definition   commands.CommandDefinition
	done         chan struct{}
	mu           sync.Mutex
	finishedFlag bool
	result       json.RawMessage
	err          *protocol.CommandError
	startedAt    time.Time
}

func (s *sharedCommand) finish(result json.RawMessage, err *protocol.CommandError) {
	s.mu.Lock()
	if s.finishedFlag {
		s.mu.Unlock()
		return
	}
	s.finishedFlag = true
	s.result = append(json.RawMessage(nil), result...)
	if err != nil {
		copyErr := *err
		s.err = &copyErr
	}
	close(s.done)
	s.mu.Unlock()
}

func (s *sharedCommand) finished() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.finishedFlag }

type commandOutcome struct {
	result json.RawMessage
	err    *protocol.CommandError
}

func (s *sharedCommand) outcome() commandOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	outcome := commandOutcome{result: append(json.RawMessage(nil), s.result...)}
	if s.err != nil {
		copyErr := *s.err
		outcome.err = &copyErr
	}
	return outcome
}
