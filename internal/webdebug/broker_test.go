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
}

func (c *fakeConnection) Info() wsgateway.ConnectionInfo { return c.info }

func (c *fakeConnection) Send(message protocol.Message) error {
	c.mu.Lock()
	c.messages = append(c.messages, message)
	c.mu.Unlock()
	return nil
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
