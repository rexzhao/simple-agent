package webdebug

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/wsgateway"
)

type fakeConnection struct {
	info wsgateway.ConnectionInfo

	mu       sync.Mutex
	messages []protocol.Message
	sendErr  error
}

func (c *fakeConnection) Info() wsgateway.ConnectionInfo { return c.info }

func (c *fakeConnection) Send(message protocol.Message) error {
	c.mu.Lock()
	c.messages = append(c.messages, message)
	err := c.sendErr
	c.mu.Unlock()
	return err
}

func (c *fakeConnection) lastMessage(t *testing.T) protocol.Message {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.messages) == 0 {
		t.Fatal("connection has no messages")
	}
	return c.messages[len(c.messages)-1]
}

func (c *fakeConnection) findMessage(t *testing.T, kind protocol.MessageType) protocol.Message {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for index := len(c.messages) - 1; index >= 0; index-- {
			message := c.messages[index]
			if message.Kind() == kind {
				c.mu.Unlock()
				return message
			}
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("connection did not receive %s", kind)
	return nil
}

func (c *fakeConnection) findNewExecution(t *testing.T, previous string) protocol.DebugExecuteMessage {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for index := len(c.messages) - 1; index >= 0; index-- {
			message, ok := c.messages[index].(protocol.DebugExecuteMessage)
			if ok && message.Payload.ExecutionID != previous {
				c.mu.Unlock()
				return message
			}
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("connection did not receive a new execution")
	return protocol.DebugExecuteMessage{}
}

func testIdentity(page, epoch, session string, focused bool) protocol.DebugExecutorPayload {
	return protocol.DebugExecutorPayload{PageID: page, PageEpoch: epoch, SessionID: session, Focused: focused}
}

func registerMessage(payload protocol.DebugExecutorPayload) protocol.DebugRegisterMessage {
	return protocol.DebugRegisterMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugRegister, ID: "register-request"},
		Payload:  payload,
	}
}

func focusMessage(payload protocol.DebugExecutorPayload) protocol.DebugFocusMessage {
	return protocol.DebugFocusMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugFocus, ID: "focus-request"},
		Payload:  payload,
	}
}

func unregisterMessage(payload protocol.DebugExecutorPayload) protocol.DebugUnregisterMessage {
	return protocol.DebugUnregisterMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugUnregister, ID: "unregister-request"},
		Payload:  payload,
	}
}

func newTestBroker(t *testing.T, eligibility Eligibility) *Broker {
	t.Helper()
	broker, err := NewBroker(Options{Enabled: true, Eligibility: eligibility})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	return broker
}

func assertCurrent(t *testing.T, broker *Broker, want LeaseIdentity) {
	t.Helper()
	got, err := broker.Current()
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if got != want {
		t.Fatalf("Current() = %#v, want %#v", got, want)
	}
}

func assertErrorMessage(t *testing.T, connection *fakeConnection, want string) {
	t.Helper()
	message, ok := connection.lastMessage(t).(protocol.ErrorMessage)
	if !ok {
		t.Fatalf("last message = %T, want ErrorMessage", connection.lastMessage(t))
	}
	if message.Payload.Code != want {
		t.Fatalf("error code = %q, want %q", message.Payload.Code, want)
	}
}

func assertErrorRequestID(t *testing.T, connection *fakeConnection, want string) {
	t.Helper()
	message, ok := connection.lastMessage(t).(protocol.ErrorMessage)
	if !ok {
		t.Fatalf("last message = %T, want ErrorMessage", connection.lastMessage(t))
	}
	if message.Payload.RequestID == nil || *message.Payload.RequestID != want {
		t.Fatalf("error request_id = %v, want %q", message.Payload.RequestID, want)
	}
}

func TestBrokerDebugErrorsCorrelateToControlRequest(t *testing.T) {
	disabled, err := NewBroker(Options{Enabled: false, Eligibility: func(context.Context, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Close()
	disabledConnection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "disabled-connection"}}
	if err := NewHandler(disabled, nil).Handle(context.Background(), disabledConnection, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	assertErrorMessage(t, disabledConnection, ErrorCodeDisabled)
	assertErrorRequestID(t, disabledConnection, "register-request")

	broker := newTestBroker(t, func(context.Context, string) error { return nil })
	handler := NewHandler(broker, nil)
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "connection-1"}}
	if err := handler.Handle(context.Background(), connection, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background(), connection, focusMessage(testIdentity("page-1", "old-epoch", "session-1", false))); err != nil {
		t.Fatal(err)
	}
	assertErrorMessage(t, connection, ErrorCodePageNotRegistered)
	assertErrorRequestID(t, connection, "focus-request")
	if err := handler.Handle(context.Background(), connection, unregisterMessage(testIdentity("page-1", "old-epoch", "session-1", false))); err != nil {
		t.Fatal(err)
	}
	assertErrorMessage(t, connection, ErrorCodePageNotRegistered)
	assertErrorRequestID(t, connection, "unregister-request")

	encoded, err := protocol.EncodeMessage(connection.lastMessage(t))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"session_id"`)) || bytes.Contains(encoded, []byte(`"project_id"`)) {
		t.Fatalf("debug error wire unexpectedly contains session/project identity: %s", encoded)
	}
}

func TestBrokerDisabledDoesNotCreateCandidate(t *testing.T) {
	broker, err := NewBroker(Options{Enabled: false, Eligibility: func(context.Context, string) error {
		t.Fatal("disabled broker called eligibility")
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-1"}}
	handler := NewHandler(broker, nil)
	if err := handler.Handle(context.Background(), connection, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	assertErrorMessage(t, connection, ErrorCodeDisabled)
	if _, err := broker.Current(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Current() error = %v, want ErrNotConnected", err)
	}
}

func TestBrokerUsesAuthoritativeEligibilityWithoutProjectParameter(t *testing.T) {
	broker := newTestBroker(t, func(_ context.Context, sessionID string) error {
		switch sessionID {
		case "missing":
			return ErrSessionNotFound
		case "wrong-project":
			return ErrProjectMismatch
		default:
			return nil
		}
	})
	handler := NewHandler(broker, nil)
	for _, test := range []struct {
		name    string
		session string
		code    string
	}{
		{name: "missing session", session: "missing", code: ErrorCodeNotEligible},
		{name: "wrong project", session: "wrong-project", code: ErrorCodeNotEligible},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: test.name}}
			if err := handler.Handle(context.Background(), connection, registerMessage(testIdentity("page-1", "epoch-1", test.session, true))); err != nil {
				t.Fatal(err)
			}
			assertErrorMessage(t, connection, test.code)
		})
	}

	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-target"}}
	if err := handler.Handle(context.Background(), connection, registerMessage(testIdentity("page-1", "epoch-1", "target", true))); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, broker, LeaseIdentity{ConnectionID: "conn-target", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "target"})
	encoded, err := protocol.EncodeMessage(registerMessage(testIdentity("page-1", "epoch-1", "target", true)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"project_id"`)) {
		t.Fatalf("registration wire unexpectedly contains client project selection: %s", encoded)
	}
}

func TestBrokerFocusChoosesRecentFocusedAndFallsBackDeterministically(t *testing.T) {
	broker := newTestBroker(t, func(context.Context, string) error { return nil })
	handler := NewHandler(broker, nil)
	first := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-1"}}
	second := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-2"}}
	if err := handler.Handle(context.Background(), first, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background(), second, registerMessage(testIdentity("page-2", "epoch-1", "session-2", true))); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, broker, LeaseIdentity{ConnectionID: "conn-2", PageID: "page-2", PageEpoch: "epoch-1", SessionID: "session-2"})

	if err := handler.Handle(context.Background(), second, focusMessage(testIdentity("page-2", "epoch-1", "session-2", false))); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, broker, LeaseIdentity{ConnectionID: "conn-1", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1"})

	if err := handler.Handle(context.Background(), first, unregisterMessage(testIdentity("page-1", "epoch-1", "session-1", false))); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, broker, LeaseIdentity{ConnectionID: "conn-2", PageID: "page-2", PageEpoch: "epoch-1", SessionID: "session-2"})
	if err := handler.Handle(context.Background(), second, unregisterMessage(testIdentity("page-2", "epoch-1", "session-2", false))); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Current(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Current() error = %v, want ErrNotConnected", err)
	}
}

func TestBrokerRefreshInvalidatesOldEpochAndLeaseCopiesStayFixed(t *testing.T) {
	broker := newTestBroker(t, func(context.Context, string) error { return nil })
	handler := NewHandler(broker, nil)
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-1"}}
	if err := handler.Handle(context.Background(), connection, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	old, err := broker.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background(), connection, registerMessage(testIdentity("page-1", "epoch-2", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, broker, LeaseIdentity{ConnectionID: "conn-1", PageID: "page-1", PageEpoch: "epoch-2", SessionID: "session-1"})
	if old.PageEpoch != "epoch-1" {
		t.Fatalf("acquired identity changed after refresh: %#v", old)
	}
	if err := handler.Handle(context.Background(), connection, focusMessage(testIdentity("page-1", "epoch-1", "session-1", false))); err != nil {
		t.Fatal(err)
	}
	assertErrorMessage(t, connection, ErrorCodePageNotRegistered)
	assertCurrent(t, broker, LeaseIdentity{ConnectionID: "conn-1", PageID: "page-1", PageEpoch: "epoch-2", SessionID: "session-1"})
}

func TestBrokerConnectionWatcherIsOwnedByConnectionAcrossCandidateChanges(t *testing.T) {
	broker := newTestBroker(t, func(context.Context, string) error { return nil })
	handler := NewHandler(broker, nil)
	connectionContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-1"}}

	register := func(epoch string) {
		t.Helper()
		if err := handler.Handle(connectionContext, connection, registerMessage(testIdentity("page-1", epoch, "session-1", true))); err != nil {
			t.Fatal(err)
		}
	}
	unregister := func(epoch string) {
		t.Helper()
		if err := handler.Handle(connectionContext, connection, unregisterMessage(testIdentity("page-1", epoch, "session-1", false))); err != nil {
			t.Fatal(err)
		}
	}

	register("epoch-1")
	waitForWatcherStats(t, broker, 1, 1)
	for i := 2; i <= 20; i++ {
		epoch := "epoch-" + strconv.Itoa(i)
		register(epoch)
		unregister(epoch)
		watchers, starts := broker.watcherStats()
		if watchers != 1 || starts != 1 {
			t.Fatalf("watcher stats after candidate change = %d/%d, want 1/1", watchers, starts)
		}
	}

	secondContext, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	second := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-2"}}
	if err := handler.Handle(secondContext, second, registerMessage(testIdentity("page-2", "epoch-1", "session-2", true))); err != nil {
		t.Fatal(err)
	}
	waitForWatcherStats(t, broker, 2, 2)
	cancel()
	waitForWatcherStats(t, broker, 1, 2)
	assertCurrent(t, broker, LeaseIdentity{ConnectionID: "conn-2", PageID: "page-2", PageEpoch: "epoch-1", SessionID: "session-2"})

	secondCancel()
	waitForWatcherStats(t, broker, 0, 2)
	if _, err := broker.Current(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Current() after all connection cancellation = %v, want ErrNotConnected", err)
	}
}

func TestBrokerAcquireRechecksAuthorityAndFallsBack(t *testing.T) {
	eligible := true
	broker := newTestBroker(t, func(context.Context, string) error {
		if !eligible {
			return ErrSessionNotFound
		}
		return nil
	})
	handler := NewHandler(broker, nil)
	first := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-1"}}
	second := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-2"}}
	if err := handler.Handle(context.Background(), first, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(context.Background(), second, registerMessage(testIdentity("page-2", "epoch-1", "session-2", false))); err != nil {
		t.Fatal(err)
	}
	eligible = false
	identity, err := broker.Acquire(context.Background())
	if !errors.Is(err, ErrNotConnected) || identity != (LeaseIdentity{}) {
		t.Fatalf("Acquire() = %#v, %v, want empty ErrNotConnected", identity, err)
	}
	assertCurrent(t, broker, LeaseIdentity{ConnectionID: "conn-2", PageID: "page-2", PageEpoch: "epoch-1", SessionID: "session-2"})
}

func TestBrokerAcquireDoesNotDeleteReplacementDuringAuthorityCheck(t *testing.T) {
	authorityStarted := make(chan struct{})
	releaseAuthority := make(chan struct{})
	var blockAuthority bool
	var authorityMu sync.Mutex
	broker := newTestBroker(t, func(_ context.Context, sessionID string) error {
		authorityMu.Lock()
		blocked := blockAuthority && sessionID == "session-1"
		authorityMu.Unlock()
		if blocked {
			select {
			case <-authorityStarted:
			default:
				close(authorityStarted)
			}
			<-releaseAuthority
			return ErrSessionNotFound
		}
		return nil
	})
	handler := NewHandler(broker, nil)
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-1"}}
	if err := handler.Handle(context.Background(), connection, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	authorityMu.Lock()
	blockAuthority = true
	authorityMu.Unlock()
	acquireDone := make(chan error, 1)
	go func() {
		_, err := broker.Acquire(context.Background())
		acquireDone <- err
	}()
	select {
	case <-authorityStarted:
	case <-time.After(time.Second):
		t.Fatal("Acquire() did not reach authority check")
	}
	if err := handler.Handle(context.Background(), connection, registerMessage(testIdentity("page-1", "epoch-2", "session-2", true))); err != nil {
		t.Fatal(err)
	}
	close(releaseAuthority)
	if err := <-acquireDone; !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Acquire() error = %v, want ErrNotConnected", err)
	}
	assertCurrent(t, broker, LeaseIdentity{ConnectionID: "conn-1", PageID: "page-1", PageEpoch: "epoch-2", SessionID: "session-2"})
}

func TestBrokerAcquireDoesNotDeleteSameIdentityReplacement(t *testing.T) {
	authorityStarted := make(chan struct{})
	releaseAuthority := make(chan struct{})
	var calls atomic.Int32
	broker := newTestBroker(t, func(context.Context, string) error {
		if calls.Add(1) == 2 {
			close(authorityStarted)
			<-releaseAuthority
			return ErrSessionNotFound
		}
		return nil
	})
	handler := NewHandler(broker, nil)
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-1"}}
	identity := testIdentity("page-1", "epoch-1", "session-1", true)
	if err := handler.Handle(context.Background(), connection, registerMessage(identity)); err != nil {
		t.Fatal(err)
	}
	acquireDone := make(chan error, 1)
	go func() {
		_, err := broker.Acquire(context.Background())
		acquireDone <- err
	}()
	select {
	case <-authorityStarted:
	case <-time.After(time.Second):
		t.Fatal("Acquire() did not reach authority check")
	}
	if err := handler.Handle(context.Background(), connection, registerMessage(identity)); err != nil {
		t.Fatal(err)
	}
	close(releaseAuthority)
	if err := <-acquireDone; !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Acquire() error = %v, want ErrNotConnected", err)
	}
	assertCurrent(t, broker, LeaseIdentity{ConnectionID: "conn-1", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1"})
}

func TestBrokerConnectionCancelAndCloseInvalidateLease(t *testing.T) {
	broker := newTestBroker(t, func(context.Context, string) error { return nil })
	handler := NewHandler(broker, nil)
	connectionContext, cancel := context.WithCancel(context.Background())
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-1"}}
	if err := handler.Handle(connectionContext, connection, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitForNotConnected(t, broker)

	second := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-2"}}
	if err := handler.Handle(context.Background(), second, registerMessage(testIdentity("page-2", "epoch-1", "session-2", true))); err != nil {
		t.Fatal(err)
	}
	broker.Close()
	if _, err := broker.Current(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Current() after Close error = %v, want ErrNotConnected", err)
	}
	if err := handler.Handle(context.Background(), second, focusMessage(testIdentity("page-2", "epoch-1", "session-2", true))); err != nil {
		t.Fatal(err)
	}
	assertErrorMessage(t, second, ErrorCodeClosed)
}

func TestBrokerDelegatesExistingMessages(t *testing.T) {
	broker := newTestBroker(t, func(context.Context, string) error { return nil })
	called := make(chan struct{}, 1)
	delegate := wsgateway.HandlerFunc(func(context.Context, wsgateway.Connection, protocol.Message) error {
		called <- struct{}{}
		return nil
	})
	handler := NewHandler(broker, delegate)
	message := protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "command-1"},
		Payload:  protocol.CommandPayload{Name: "session.rename", SchemaVersion: 1, RequestID: "request-1", Arguments: []byte(`{}`)},
	}
	if err := handler.Handle(context.Background(), &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-1"}}, message); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("delegate was not called")
	}
}

func TestBrokerCloseIsSafeWhileReadingCurrent(t *testing.T) {
	broker := newTestBroker(t, func(context.Context, string) error { return nil })
	handler := NewHandler(broker, nil)
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "conn-1"}}
	if err := handler.Handle(context.Background(), connection, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	for i := 0; i < 16; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				_, _ = broker.Current()
			}
		}()
	}
	broker.Close()
	readers.Wait()
}

func executionResultMessage(payload protocol.DebugExecutionResultPayload) protocol.DebugExecutionResultMessage {
	return protocol.DebugExecutionResultMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugExecutionResult, ID: "execution-result"},
		Payload:  payload,
	}
}

func TestBrokerExecuteBindsAndMatchesOneLiveConnection(t *testing.T) {
	b := newTestBroker(t, func(context.Context, string) error { return nil })
	h := NewHandler(b, nil)
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "execution-connection"}}
	identity := testIdentity("page-1", "epoch-1", "session-1", true)
	if err := h.Handle(context.Background(), connection, registerMessage(identity)); err != nil {
		t.Fatal(err)
	}
	result := make(chan protocol.DebugExecutionResultPayload, 1)
	errorsOut := make(chan error, 1)
	go func() {
		value, err := b.Execute(context.Background(), "1 + 1", 500)
		result <- value
		errorsOut <- err
	}()
	execute := connection.findMessage(t, protocol.MessageTypeDebugExecute).(protocol.DebugExecuteMessage)
	if execute.Payload.TimeoutMS != 500 || execute.Payload.PageID != "page-1" || execute.Payload.SessionID != "session-1" {
		t.Fatalf("execute payload = %#v", execute.Payload)
	}
	if err := h.Handle(context.Background(), connection, executionResultMessage(protocol.DebugExecutionResultPayload{
		ExecutionID: execute.Payload.ExecutionID, PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1",
		Status: protocol.DebugExecutionStatusSucceeded, Value: []byte(`2`),
	})); err != nil {
		t.Fatal(err)
	}
	if err := <-errorsOut; err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := string((<-result).Value); got != "2" {
		t.Fatalf("Execute() value = %s, want 2", got)
	}
}

func TestBrokerCommittedResultWinsTimeoutSettlement(t *testing.T) {
	testExecutionSettlementWinner(t, ErrExecutionTimeout)
}

func TestBrokerCommittedResultWinsContextSettlement(t *testing.T) {
	testExecutionSettlementWinner(t, context.Canceled)
}

func testExecutionSettlementWinner(t *testing.T, losingErr error) {
	t.Helper()
	b := newTestBroker(t, func(context.Context, string) error { return nil })
	h := NewHandler(b, nil)
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "settlement-connection"}}
	identity := testIdentity("page-1", "epoch-1", "session-1", true)
	if err := h.Handle(context.Background(), connection, registerMessage(identity)); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result protocol.DebugExecutionResultPayload
		err    error
	}
	outcomes := make(chan outcome, 1)
	go func() {
		result, err := b.Execute(context.Background(), "1 + 1", 5000)
		outcomes <- outcome{result: result, err: err}
	}()
	execute := connection.findMessage(t, protocol.MessageTypeDebugExecute).(protocol.DebugExecuteMessage)
	b.mu.Lock()
	pending := b.execution
	b.mu.Unlock()
	if pending == nil {
		t.Fatal("execution did not create a pending settlement")
	}
	winner := protocol.DebugExecutionResultPayload{
		ExecutionID: execute.Payload.ExecutionID, PageID: execute.Payload.PageID,
		PageEpoch: execute.Payload.PageEpoch, SessionID: execute.Payload.SessionID,
		Status: protocol.DebugExecutionStatusSucceeded, Value: []byte(`2`),
	}
	if err := h.Handle(context.Background(), connection, executionResultMessage(winner)); err != nil {
		t.Fatal(err)
	}
	if b.finishPending(pending, protocol.DebugExecutionResultPayload{}, losingErr) {
		t.Fatalf("losing settlement %v unexpectedly won", losingErr)
	}
	select {
	case got := <-outcomes:
		if got.err != nil || string(got.result.Value) != "2" {
			t.Fatalf("Execute() outcome = %#v, want committed result", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Execute() did not settle from the committed result")
	}
}

func TestBrokerExecutionResultRejectsForeignDuplicateAndStaleMessages(t *testing.T) {
	b := newTestBroker(t, func(context.Context, string) error { return nil })
	h := NewHandler(b, nil)
	first := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "execution-first"}}
	foreign := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "execution-foreign"}}
	firstIdentity := testIdentity("page-1", "epoch-1", "session-1", true)
	foreignIdentity := testIdentity("page-2", "epoch-1", "session-2", false)
	if err := h.Handle(context.Background(), first, registerMessage(firstIdentity)); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), foreign, registerMessage(foreignIdentity)); err != nil {
		t.Fatal(err)
	}
	result := make(chan protocol.DebugExecutionResultPayload, 1)
	errorsOut := make(chan error, 1)
	go func() {
		value, err := b.Execute(context.Background(), "({ ok: true })", 500)
		result <- value
		errorsOut <- err
	}()
	execute := first.findMessage(t, protocol.MessageTypeDebugExecute).(protocol.DebugExecuteMessage)
	stale := protocol.DebugExecutionResultPayload{
		ExecutionID: execute.Payload.ExecutionID, PageID: execute.Payload.PageID, PageEpoch: execute.Payload.PageEpoch, SessionID: execute.Payload.SessionID,
		Status: protocol.DebugExecutionStatusSucceeded, Value: []byte(`"foreign"`),
	}
	if err := h.Handle(context.Background(), foreign, executionResultMessage(stale)); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), first, executionResultMessage(protocol.DebugExecutionResultPayload{
		ExecutionID: "other-execution", PageID: execute.Payload.PageID, PageEpoch: execute.Payload.PageEpoch, SessionID: execute.Payload.SessionID,
		Status: protocol.DebugExecutionStatusSucceeded, Value: []byte(`"stale"`),
	})); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), first, executionResultMessage(stale)); err != nil {
		t.Fatal(err)
	}
	if err := <-errorsOut; err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := string((<-result).Value); got != `"foreign"` {
		t.Fatalf("matched result = %s, want original result", got)
	}
	// A duplicate result after completion cannot become the next execution's
	// result, even when the connection and page identity are reused.
	secondResult := make(chan protocol.DebugExecutionResultPayload, 1)
	secondError := make(chan error, 1)
	go func() {
		value, err := b.Execute(context.Background(), "3 + 3", 500)
		secondResult <- value
		secondError <- err
	}()
	second := first.findNewExecution(t, execute.Payload.ExecutionID)
	if err := h.Handle(context.Background(), first, executionResultMessage(stale)); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), first, executionResultMessage(protocol.DebugExecutionResultPayload{
		ExecutionID: second.Payload.ExecutionID, PageID: second.Payload.PageID, PageEpoch: second.Payload.PageEpoch, SessionID: second.Payload.SessionID,
		Status: protocol.DebugExecutionStatusSucceeded, Value: []byte(`6`),
	})); err != nil {
		t.Fatal(err)
	}
	if err := <-secondError; err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if got := string((<-secondResult).Value); got != "6" {
		t.Fatalf("second matched result = %s, want 6", got)
	}
}

func TestBrokerExecutionBusyTimeoutCancellationAndSendFailure(t *testing.T) {
	b := newTestBroker(t, func(context.Context, string) error { return nil })
	h := NewHandler(b, nil)
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "execution-connection"}}
	identity := testIdentity("page-1", "epoch-1", "session-1", true)
	if err := h.Handle(context.Background(), connection, registerMessage(identity)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	firstError := make(chan error, 1)
	go func() { _, err := b.Execute(ctx, "await new Promise(() => {})", 500); firstError <- err }()
	_ = connection.findMessage(t, protocol.MessageTypeDebugExecute)
	if _, err := b.Execute(context.Background(), "2", 500); !errors.Is(err, ErrExecutionBusy) {
		t.Fatalf("busy Execute() error = %v", err)
	}
	cancel()
	if err := <-firstError; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Execute() error = %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	timeoutError := make(chan error, 1)
	go func() { _, err := b.Execute(ctx2, "await new Promise(() => {})", 100); timeoutError <- err }()
	_ = connection.findMessage(t, protocol.MessageTypeDebugExecute)
	if err := <-timeoutError; !errors.Is(err, ErrExecutionTimeout) {
		t.Fatalf("timed out Execute() error = %v", err)
	}
	connection.mu.Lock()
	connection.sendErr = errors.New("send failed")
	connection.mu.Unlock()
	if _, err := b.Execute(context.Background(), "3", 500); !errors.Is(err, ErrExecutionDisconnected) {
		t.Fatalf("send failure Execute() error = %v", err)
	}
	if _, err := b.Execute(context.Background(), "4", 500); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Execute() after send failure error = %v, want ErrNotConnected", err)
	}
}

func TestBrokerConnectionWatcherCancellationFailsExecutionImmediately(t *testing.T) {
	b := newTestBroker(t, func(context.Context, string) error { return nil })
	h := NewHandler(b, nil)
	connectionContext, cancelConnection := context.WithCancel(context.Background())
	defer cancelConnection()
	connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "watcher-execution-connection"}}
	if err := h.Handle(connectionContext, connection, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
		t.Fatal(err)
	}
	waitForWatcherStats(t, b, 1, 1)
	executionDone := make(chan error, 1)
	go func() {
		_, err := b.Execute(context.Background(), "await new Promise(() => {})", 5000)
		executionDone <- err
	}()
	_ = connection.findMessage(t, protocol.MessageTypeDebugExecute)
	started := time.Now()
	cancelConnection()
	select {
	case err := <-executionDone:
		if !errors.Is(err, ErrExecutionDisconnected) {
			t.Fatalf("watcher cancellation error = %v, want ErrExecutionDisconnected", err)
		}
		if elapsed := time.Since(started); elapsed >= time.Second {
			t.Fatalf("watcher cancellation took %s, want immediate failure", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher cancellation left execution pending until timeout")
	}
}

func TestBrokerExecutionFailsImmediatelyOnRefreshUnregisterAndClose(t *testing.T) {
	for _, test := range []struct {
		name       string
		invalidate func(*testing.T, *Broker, *Handler, *fakeConnection)
	}{
		{name: "refresh", invalidate: func(t *testing.T, _ *Broker, h *Handler, c *fakeConnection) {
			if err := h.Handle(context.Background(), c, registerMessage(testIdentity("page-1", "epoch-2", "session-1", true))); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unregister", invalidate: func(t *testing.T, _ *Broker, h *Handler, c *fakeConnection) {
			if err := h.Handle(context.Background(), c, unregisterMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "close", invalidate: func(_ *testing.T, b *Broker, _ *Handler, _ *fakeConnection) { b.Close() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := newTestBroker(t, func(context.Context, string) error { return nil })
			h := NewHandler(b, nil)
			connection := &fakeConnection{info: wsgateway.ConnectionInfo{ConnectionID: "execution-connection"}}
			if err := h.Handle(context.Background(), connection, registerMessage(testIdentity("page-1", "epoch-1", "session-1", true))); err != nil {
				t.Fatal(err)
			}
			executionError := make(chan error, 1)
			go func() { _, err := b.Execute(context.Background(), "1", 5000); executionError <- err }()
			_ = connection.findMessage(t, protocol.MessageTypeDebugExecute)
			test.invalidate(t, b, h, connection)
			err := <-executionError
			if test.name == "close" {
				if !errors.Is(err, ErrClosed) {
					t.Fatalf("Close() execution error = %v", err)
				}
			} else if !errors.Is(err, ErrExecutionDisconnected) {
				t.Fatalf("%s execution error = %v", test.name, err)
			}
		})
	}
}

func waitForNotConnected(t *testing.T, broker *Broker) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := broker.Current(); errors.Is(err, ErrNotConnected) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if identity, err := broker.Current(); err == nil {
		t.Fatalf("Current() after cancellation = %#v, want ErrNotConnected", identity)
	} else {
		t.Fatalf("Current() after cancellation error = %v, want ErrNotConnected", err)
	}
}

func (b *Broker) watcherStats() (watchers int, starts uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.watching), b.watcherStarts
}

func waitForWatcherStats(t *testing.T, broker *Broker, wantWatchers int, wantStarts uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		watchers, starts := broker.watcherStats()
		if watchers == wantWatchers && starts == wantStarts {
			return
		}
		time.Sleep(time.Millisecond)
	}
	watchers, starts := broker.watcherStats()
	t.Fatalf("watcher stats = %d/%d, want %d/%d", watchers, starts, wantWatchers, wantStarts)
}
