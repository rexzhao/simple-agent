// Package webdebug contains the server-side stage-1 Web debug executor
// broker. It owns only debug control messages and delegates all existing
// WebSocket command/subscription messages to the normal gateway handler.
package webdebug

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/wsgateway"
)

const TargetProjectID = "project-f25c5aac78f681b52aabf5c0"

const (
	ErrorCodeDisabled              = "web_debug_disabled"
	ErrorCodeClosed                = "web_debug_closed"
	ErrorCodeNotConnected          = "web_debug_not_connected"
	ErrorCodeInvalidConnection     = "web_debug_invalid_connection"
	ErrorCodeInvalidIdentity       = "web_debug_invalid_identity"
	ErrorCodeNotEligible           = "web_debug_not_eligible"
	ErrorCodeSessionNotFound       = "web_debug_session_not_found"
	ErrorCodeProjectMismatch       = "web_debug_project_mismatch"
	ErrorCodeSessionUnavailable    = "web_debug_session_unavailable"
	ErrorCodePageNotRegistered     = "web_debug_page_not_registered"
	ErrorCodeConnectionNotAllowed  = "web_debug_connection_not_allowed"
	ErrorCodeInvalidExecution      = "web_debug_invalid_execution"
	ErrorCodeExecutionBusy         = "web_debug_busy"
	ErrorCodeExecutionTimeout      = "web_debug_timeout"
	ErrorCodeExecutionDisconnected = "web_debug_disconnected"
)

// Error is a stable, typed stage-1 broker error. It intentionally contains no
// session, project, or connection details.
type Error struct {
	Code string
}

func (e *Error) Error() string {
	if e == nil {
		return "web debug error"
	}
	return e.Code
}

var (
	ErrDisabled              = &Error{Code: ErrorCodeDisabled}
	ErrClosed                = &Error{Code: ErrorCodeClosed}
	ErrNotConnected          = &Error{Code: ErrorCodeNotConnected}
	ErrInvalidConnection     = &Error{Code: ErrorCodeInvalidConnection}
	ErrInvalidIdentity       = &Error{Code: ErrorCodeInvalidIdentity}
	ErrNotEligible           = &Error{Code: ErrorCodeNotEligible}
	ErrPageNotRegistered     = &Error{Code: ErrorCodePageNotRegistered}
	ErrSessionNotFound       = &Error{Code: ErrorCodeSessionNotFound}
	ErrProjectMismatch       = &Error{Code: ErrorCodeProjectMismatch}
	ErrSessionUnavailable    = &Error{Code: ErrorCodeSessionUnavailable}
	ErrInvalidExecution      = &Error{Code: ErrorCodeInvalidExecution}
	ErrExecutionBusy         = &Error{Code: ErrorCodeExecutionBusy}
	ErrExecutionTimeout      = &Error{Code: ErrorCodeExecutionTimeout}
	ErrExecutionDisconnected = &Error{Code: ErrorCodeExecutionDisconnected}
)

const DefaultExecutionTimeoutMS = 5_000

// Eligibility is the server-side authority for session/project admission. A
// client never supplies a project ID to the broker.
type Eligibility func(context.Context, string) error

type Options struct {
	Enabled     bool
	Eligibility Eligibility
	IDGenerator func(string) (string, error)
}

// LeaseIdentity is a copyable, fixed identity for one executor page epoch.
// The broker never changes a value already returned by Current or Acquire.
type LeaseIdentity struct {
	ConnectionID string
	PageID       string
	PageEpoch    string
	SessionID    string
}

type candidate struct {
	identity       LeaseIdentity
	connection     wsgateway.Connection
	focused        bool
	registrationAt uint64
	focusAt        uint64
}

type pendingExecution struct {
	identity    LeaseIdentity
	candidate   *candidate
	connection  wsgateway.Connection
	executionID string
	done        chan struct{}
	result      protocol.DebugExecutionResultPayload
	err         error
}

type Broker struct {
	enabled     bool
	eligibility Eligibility
	idGenerator func(string) (string, error)

	mu            sync.Mutex
	closed        bool
	sequence      uint64
	candidates    map[string]*candidate
	watching      map[string]struct{}
	watcherStarts uint64
	current       LeaseIdentity
	execution     *pendingExecution

	done      chan struct{}
	watchers  sync.WaitGroup
	closeOnce sync.Once
}

func NewBroker(options Options) (*Broker, error) {
	if options.Enabled && options.Eligibility == nil {
		return nil, errors.New("web debug eligibility is required when enabled")
	}
	idGenerator := options.IDGenerator
	if idGenerator == nil {
		idGenerator = func(prefix string) (string, error) {
			value := atomic.AddUint64(&brokerID, 1)
			return fmt.Sprintf("%s-%d", prefix, value), nil
		}
	}
	return &Broker{
		enabled:     options.Enabled,
		eligibility: options.Eligibility,
		idGenerator: idGenerator,
		candidates:  make(map[string]*candidate),
		watching:    make(map[string]struct{}),
		done:        make(chan struct{}),
	}, nil
}

var brokerID uint64

// Enabled reports the immutable server-side switch injected at construction.
func (b *Broker) Enabled() bool {
	return b != nil && b.enabled
}

// Current returns a lock-protected copy of the current lease without doing
// authority I/O. It is only an observational snapshot, not permission to
// execute against the returned session; execution callers must use Acquire.
func (b *Broker) Current() (LeaseIdentity, error) {
	identity, _, err := b.currentSnapshot()
	return identity, err
}

// Acquire rechecks server-side session authority before returning a lease. It
// never returns a snapshot that stopped being the current candidate while the
// authority check was in flight.
func (b *Broker) Acquire(ctx context.Context) (LeaseIdentity, error) {
	identity, _, err := b.acquireCandidate(ctx)
	return identity, err
}

// Execute binds one request to the current live page after rechecking session
// authority. The caller supplies no connection, page, or session selector.
// A zero timeout uses the server default; the wire message always carries the
// resulting bounded timeout.
func (b *Broker) Execute(ctx context.Context, code string, timeoutMS int) (protocol.DebugExecutionResultPayload, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return protocol.DebugExecutionResultPayload{}, err
	}
	if !b.Enabled() {
		return protocol.DebugExecutionResultPayload{}, ErrDisabled
	}
	if b.isClosed() {
		return protocol.DebugExecutionResultPayload{}, ErrClosed
	}
	if timeoutMS == 0 {
		timeoutMS = DefaultExecutionTimeoutMS
	}
	if code == "" || !utf8.ValidString(code) || len([]byte(code)) > protocol.DebugExecutionCodeMaxBytes {
		return protocol.DebugExecutionResultPayload{}, ErrInvalidExecution
	}
	if timeoutMS < protocol.DebugExecutionMinTimeoutMS || timeoutMS > protocol.DebugExecutionMaxTimeoutMS {
		return protocol.DebugExecutionResultPayload{}, ErrInvalidExecution
	}

	identity, item, err := b.acquireCandidate(ctx)
	if err != nil {
		return protocol.DebugExecutionResultPayload{}, err
	}
	executionID, err := b.idGenerator("execution")
	if err != nil {
		return protocol.DebugExecutionResultPayload{}, err
	}
	pending := &pendingExecution{
		identity: identity, candidate: item, connection: item.connection,
		executionID: executionID, done: make(chan struct{}),
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return protocol.DebugExecutionResultPayload{}, ErrClosed
	}
	if b.execution != nil {
		b.mu.Unlock()
		return protocol.DebugExecutionResultPayload{}, ErrExecutionBusy
	}
	current := b.candidates[identity.ConnectionID]
	if current != item || !sameLeaseIdentity(b.current, identity) || current.connection == nil {
		b.mu.Unlock()
		return protocol.DebugExecutionResultPayload{}, ErrNotConnected
	}
	b.execution = pending
	b.mu.Unlock()

	message := protocol.DebugExecuteMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugExecute, ID: executionID},
		Payload: protocol.DebugExecutionPayload{
			ExecutionID: executionID,
			PageID:      identity.PageID, PageEpoch: identity.PageEpoch,
			SessionID: identity.SessionID, Code: code, TimeoutMS: timeoutMS,
		},
	}
	if err := pending.connection.Send(message); err != nil {
		b.failSend(pending)
		return protocol.DebugExecutionResultPayload{}, ErrExecutionDisconnected
	}

	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-pending.done:
		return pending.result, pending.err
	case <-ctx.Done():
		if b.finishPending(pending, protocol.DebugExecutionResultPayload{}, ctx.Err()) {
			return protocol.DebugExecutionResultPayload{}, ctx.Err()
		}
		<-pending.done
		return pending.result, pending.err
	case <-timer.C:
		if b.finishPending(pending, protocol.DebugExecutionResultPayload{}, ErrExecutionTimeout) {
			return protocol.DebugExecutionResultPayload{}, ErrExecutionTimeout
		}
		<-pending.done
		return pending.result, pending.err
	}
}

func (b *Broker) acquireCandidate(ctx context.Context) (LeaseIdentity, *candidate, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return LeaseIdentity{}, nil, ctx.Err()
		default:
		}
	}
	identity, expectedCandidate, err := b.currentSnapshot()
	if err != nil {
		return LeaseIdentity{}, nil, err
	}
	if b.eligibility == nil {
		return LeaseIdentity{}, nil, ErrNotConnected
	}
	authorityContext := ctx
	if authorityContext == nil {
		authorityContext = context.Background()
	}
	if err := b.eligibility(authorityContext, identity.SessionID); err != nil {
		b.invalidateAcquireIdentity(identity, expectedCandidate)
		return LeaseIdentity{}, nil, ErrNotConnected
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return LeaseIdentity{}, nil, err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.candidates[identity.ConnectionID]
	if b.closed || !sameLeaseIdentity(b.current, identity) || item == nil || item != expectedCandidate || !sameLeaseIdentity(item.identity, identity) {
		return LeaseIdentity{}, nil, ErrNotConnected
	}
	return identity, item, nil
}

func (b *Broker) finishPending(pending *pendingExecution, result protocol.DebugExecutionResultPayload, err error) bool {
	if b == nil || pending == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.execution != pending {
		return false
	}
	pending.result = result
	pending.err = err
	b.execution = nil
	close(pending.done)
	return true
}

func (b *Broker) failSend(pending *pendingExecution) {
	if b == nil || pending == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.execution == pending {
		pending.err = ErrExecutionDisconnected
		b.execution = nil
		close(pending.done)
	}
	if item := b.candidates[pending.identity.ConnectionID]; item == pending.candidate {
		b.removeCandidateLocked(pending.identity.ConnectionID)
		b.selectCurrentLocked()
	}
}

func (b *Broker) currentSnapshot() (LeaseIdentity, *candidate, error) {
	if b == nil {
		return LeaseIdentity{}, nil, ErrNotConnected
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.current.ConnectionID == "" {
		return LeaseIdentity{}, nil, ErrNotConnected
	}
	item := b.candidates[b.current.ConnectionID]
	if item == nil || !sameLeaseIdentity(item.identity, b.current) {
		return LeaseIdentity{}, nil, ErrNotConnected
	}
	return item.identity, item, nil
}

// Close invalidates all candidates and the current lease. It is safe to call
// repeatedly and waits for connection-context watchers to stop.
func (b *Broker) Close() {
	if b == nil {
		return
	}
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		if b.execution != nil {
			pending := b.execution
			pending.err = ErrClosed
			b.execution = nil
			close(pending.done)
		}
		close(b.done)
		b.candidates = make(map[string]*candidate)
		b.watching = make(map[string]struct{})
		b.current = LeaseIdentity{}
		b.mu.Unlock()
		b.watchers.Wait()
	})
}

// Handler consumes only the six debug control messages. All other messages
// are delegated unchanged, preserving the existing Dispatcher behavior.
type Handler struct {
	broker   *Broker
	delegate wsgateway.Handler
}

func NewHandler(broker *Broker, delegate wsgateway.Handler) *Handler {
	return &Handler{broker: broker, delegate: delegate}
}

func (h *Handler) Handle(ctx context.Context, connection wsgateway.Connection, message protocol.Message) error {
	if h == nil || h.broker == nil {
		if h != nil && h.delegate != nil {
			return h.delegate.Handle(ctx, connection, message)
		}
		return wsgateway.ErrUnsupportedMessage
	}
	switch typed := message.(type) {
	case protocol.DebugRegisterMessage:
		return h.broker.handleRegister(ctx, connection, typed)
	case protocol.DebugFocusMessage:
		return h.broker.handleFocus(ctx, connection, typed)
	case protocol.DebugUnregisterMessage:
		return h.broker.handleUnregister(ctx, connection, typed)
	case protocol.DebugExecutionResultMessage:
		return h.broker.handleExecutionResult(connection, typed)
	default:
		if h.delegate == nil {
			return wsgateway.ErrUnsupportedMessage
		}
		return h.delegate.Handle(ctx, connection, message)
	}
}

func (b *Broker) handleExecutionResult(connection wsgateway.Connection, message protocol.DebugExecutionResultMessage) error {
	connectionID, err := b.connectionID(connection)
	if err != nil {
		return nil
	}
	b.mu.Lock()
	pending := b.execution
	item := b.candidates[connectionID]
	if pending == nil || item == nil || item != pending.candidate || !sameConnection(item.connection, connection) ||
		pending.identity.ConnectionID != connectionID ||
		pending.identity.PageID != message.Payload.PageID || pending.identity.PageEpoch != message.Payload.PageEpoch ||
		pending.identity.SessionID != message.Payload.SessionID || pending.executionID != message.Payload.ExecutionID {
		b.mu.Unlock()
		return nil
	}
	pending.result = message.Payload
	pending.err = nil
	b.execution = nil
	close(pending.done)
	b.mu.Unlock()
	return nil
}

func (b *Broker) handleRegister(ctx context.Context, connection wsgateway.Connection, message protocol.DebugRegisterMessage) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	connectionID, err := b.connectionID(connection)
	if err != nil {
		return b.sendError(connection, message.Envelope.ID, err)
	}
	if err := protocol.ValidateDebugExecutorIdentity(message.Payload.PageID, message.Payload.PageEpoch, message.Payload.SessionID); err != nil {
		b.removeCandidate(connectionID)
		return b.sendError(connection, message.Envelope.ID, ErrInvalidIdentity)
	}
	if !b.Enabled() {
		return b.sendError(connection, message.Envelope.ID, ErrDisabled)
	}
	if b.isClosed() {
		return b.sendError(connection, message.Envelope.ID, ErrClosed)
	}
	// A replacement registration invalidates the prior epoch before the new
	// authority check. A failed refresh therefore cannot retain an old lease.
	b.removeCandidate(connectionID)
	if err := b.eligibility(ctx, message.Payload.SessionID); err != nil {
		return b.sendError(connection, message.Envelope.ID, ErrNotEligible)
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return wsgateway.ErrConnectionClosed
		default:
		}
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return b.sendError(connection, message.Envelope.ID, ErrClosed)
	}
	b.sequence++
	item := &candidate{
		identity: LeaseIdentity{
			ConnectionID: connectionID,
			PageID:       message.Payload.PageID,
			PageEpoch:    message.Payload.PageEpoch,
			SessionID:    message.Payload.SessionID,
		},
		connection:     connection,
		focused:        message.Payload.Focused,
		registrationAt: b.sequence,
	}
	if item.focused {
		item.focusAt = b.sequence
	}
	b.candidates[connectionID] = item
	b.selectCurrentLocked()
	b.ensureWatcherLocked(ctx, connectionID)
	b.mu.Unlock()
	if err := b.sendMessage(connection, protocol.DebugRegisteredMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugRegistered},
		Payload:  message.Payload,
	}); err != nil {
		b.removeCandidateIfCurrent(connectionID, item)
		return err
	}
	return nil
}

func (b *Broker) handleFocus(ctx context.Context, connection wsgateway.Connection, message protocol.DebugFocusMessage) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	connectionID, err := b.connectionID(connection)
	if err != nil {
		return b.sendError(connection, message.Envelope.ID, err)
	}
	if err := protocol.ValidateDebugExecutorIdentity(message.Payload.PageID, message.Payload.PageEpoch, message.Payload.SessionID); err != nil {
		return b.sendError(connection, message.Envelope.ID, ErrInvalidIdentity)
	}
	if !b.Enabled() {
		return b.sendError(connection, message.Envelope.ID, ErrDisabled)
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return b.sendError(connection, message.Envelope.ID, ErrClosed)
	}
	item := b.candidates[connectionID]
	if item == nil || item.identity.PageID != message.Payload.PageID || item.identity.PageEpoch != message.Payload.PageEpoch || item.identity.SessionID != message.Payload.SessionID {
		b.mu.Unlock()
		return b.sendError(connection, message.Envelope.ID, ErrPageNotRegistered)
	}
	b.sequence++
	item.focused = message.Payload.Focused
	if item.focused {
		item.focusAt = b.sequence
	}
	b.selectCurrentLocked()
	b.mu.Unlock()
	return b.sendMessage(connection, protocol.DebugFocusedMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugFocused},
		Payload:  message.Payload,
	})
}

func (b *Broker) handleUnregister(ctx context.Context, connection wsgateway.Connection, message protocol.DebugUnregisterMessage) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	connectionID, err := b.connectionID(connection)
	if err != nil {
		return b.sendError(connection, message.Envelope.ID, err)
	}
	if err := protocol.ValidateDebugExecutorIdentity(message.Payload.PageID, message.Payload.PageEpoch, message.Payload.SessionID); err != nil {
		return b.sendError(connection, message.Envelope.ID, ErrInvalidIdentity)
	}
	if !b.Enabled() {
		return b.sendError(connection, message.Envelope.ID, ErrDisabled)
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return b.sendError(connection, message.Envelope.ID, ErrClosed)
	}
	item := b.candidates[connectionID]
	if item == nil || item.identity.PageID != message.Payload.PageID || item.identity.PageEpoch != message.Payload.PageEpoch || item.identity.SessionID != message.Payload.SessionID {
		b.mu.Unlock()
		return b.sendError(connection, message.Envelope.ID, ErrPageNotRegistered)
	}
	b.removeCandidateLocked(connectionID)
	b.selectCurrentLocked()
	b.mu.Unlock()
	return b.sendMessage(connection, protocol.DebugUnregisteredMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugUnregistered},
		Payload:  message.Payload,
	})
}

func (b *Broker) connectionID(connection wsgateway.Connection) (string, error) {
	if connection == nil {
		return "", ErrInvalidConnection
	}
	connectionID := connection.Info().ConnectionID
	if strings.TrimSpace(connectionID) != connectionID || connectionID == "" {
		return "", ErrInvalidConnection
	}
	return connectionID, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return wsgateway.ErrConnectionClosed
	default:
		return nil
	}
}

func (b *Broker) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *Broker) ensureWatcherLocked(ctx context.Context, connectionID string) {
	if ctx == nil {
		return
	}
	if _, exists := b.watching[connectionID]; exists {
		return
	}
	// The watcher is intentionally per live connection. Registration replaces
	// a page epoch but does not create another watcher for the same connection.
	b.watching[connectionID] = struct{}{}
	b.watcherStarts++
	b.watchers.Add(1)
	done := b.done
	go func() {
		defer b.watchers.Done()
		select {
		case <-ctx.Done():
			b.watcherStopped(connectionID)
		case <-done:
		}
	}()
}

// removeCandidate invalidates only the current page candidate. It deliberately
// leaves the connection watcher owned by the live connection in place.
func (b *Broker) removeCandidate(connectionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removeCandidateLocked(connectionID)
	b.selectCurrentLocked()
}

func (b *Broker) removeCandidateIfCurrent(connectionID string, expected *candidate) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.candidates[connectionID] != expected {
		return
	}
	b.removeCandidateLocked(connectionID)
	b.selectCurrentLocked()
}

func (b *Broker) removeCandidateLocked(connectionID string) {
	if item := b.candidates[connectionID]; item != nil {
		b.failExecutionLocked(item.identity, ErrExecutionDisconnected)
	}
	delete(b.candidates, connectionID)
	if b.current.ConnectionID == connectionID {
		b.current = LeaseIdentity{}
	}
}

// watcherStopped is called only by the connection watcher itself when its
// context ends. Broker Close clears watcher bookkeeping for all watchers.
func (b *Broker) watcherStopped(connectionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if item := b.candidates[connectionID]; item != nil {
		b.failExecutionLocked(item.identity, ErrExecutionDisconnected)
	}
	delete(b.candidates, connectionID)
	delete(b.watching, connectionID)
	b.selectCurrentLocked()
}

func (b *Broker) selectCurrentLocked() {
	var focused *candidate
	for _, item := range b.candidates {
		if !item.focused {
			continue
		}
		if focused == nil || item.focusAt > focused.focusAt || (item.focusAt == focused.focusAt && item.registrationAt > focused.registrationAt) {
			focused = item
		}
	}
	if focused != nil {
		b.current = focused.identity
		return
	}
	if b.current.ConnectionID != "" {
		if item := b.candidates[b.current.ConnectionID]; item != nil {
			b.current = item.identity
			return
		}
	}
	var recent *candidate
	for _, item := range b.candidates {
		if recent == nil || item.registrationAt > recent.registrationAt {
			recent = item
		}
	}
	if recent == nil {
		b.current = LeaseIdentity{}
		return
	}
	b.current = recent.identity
}

func (b *Broker) sendMessage(connection wsgateway.Connection, message protocol.Message) error {
	if connection == nil {
		return wsgateway.ErrConnectionClosed
	}
	id, err := b.idGenerator("debug")
	if err != nil {
		return err
	}
	switch typed := message.(type) {
	case protocol.DebugRegisteredMessage:
		typed.Envelope.ID = id
		return connection.Send(typed)
	case protocol.DebugFocusedMessage:
		typed.Envelope.ID = id
		return connection.Send(typed)
	case protocol.DebugUnregisteredMessage:
		typed.Envelope.ID = id
		return connection.Send(typed)
	default:
		return wsgateway.ErrUnsupportedMessage
	}
}

func (b *Broker) sendError(connection wsgateway.Connection, requestID string, err error) error {
	if connection == nil {
		return wsgateway.ErrConnectionClosed
	}
	status := asStatusError(err)
	id, idErr := b.idGenerator("debug_error")
	if idErr != nil {
		return idErr
	}
	payload := protocol.ErrorPayload{Code: status.Code, Message: status.Code}
	if requestID != "" {
		payload.RequestID = &requestID
	}
	return connection.Send(protocol.ErrorMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeError, ID: id},
		Payload:  payload,
	})
}

func asStatusError(err error) *Error {
	var status *Error
	if errors.As(err, &status) && status != nil {
		return status
	}
	return &Error{Code: ErrorCodeConnectionNotAllowed}
}

func (b *Broker) invalidateAcquireIdentity(identity LeaseIdentity, expectedCandidate *candidate) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || !sameLeaseIdentity(b.current, identity) {
		return
	}
	item := b.candidates[identity.ConnectionID]
	if item == nil || item != expectedCandidate || !sameLeaseIdentity(item.identity, identity) {
		return
	}
	b.removeCandidateLocked(identity.ConnectionID)
	b.selectCurrentLocked()
}

func (b *Broker) failExecutionLocked(identity LeaseIdentity, err error) {
	if b.execution == nil || b.execution.identity != identity {
		return
	}
	pending := b.execution
	pending.err = err
	b.execution = nil
	close(pending.done)
}

func sameConnection(left, right wsgateway.Connection) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() {
		return false
	}
	if leftValue.Type().Comparable() {
		return leftValue.Interface() == rightValue.Interface()
	}
	if leftValue.Kind() == reflect.Pointer || leftValue.Kind() == reflect.Map || leftValue.Kind() == reflect.Func || leftValue.Kind() == reflect.Slice {
		return leftValue.Pointer() == rightValue.Pointer()
	}
	return false
}

func sameLeaseIdentity(left, right LeaseIdentity) bool {
	return left == right
}
