package wsgateway

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

type asyncClientMessage struct {
	message protocol.Message
	err     error
}

func TestDispatcherSlowOpenDoesNotBlockWebSocketReader(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	provider.openStarted = make(chan struct{}, 1)
	provider.openFinished = make(chan struct{}, 1)
	provider.openGate = make(chan struct{})
	provider.ignoreOpenCancel = true
	clock := newManualClock()
	definition := commands.CommandDefinition{
		Name:          "fake.fast",
		SchemaVersion: 1,
		Execute: func(context.Context, commands.CommandRequest) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		},
	}
	endpoint, dispatcher := newDispatcherForTestWithGatewayOptions(t, provider, func(options *DispatcherOptions) {
		options.MaxSubscriptions = 1
	}, func(options *Options) {
		options.Clock = clock
	}, definition)
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	var writeMu sync.Mutex
	writeClient := func(message protocol.Message) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		payload, err := protocol.EncodeMessage(message)
		if err != nil {
			return err
		}
		return connection.Write(context.Background(), websocket.MessageText, payload)
	}
	writeHello(t, connection, "slow-open-client")
	if _, ok := readProtocol(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	clock.WaitForTickers(t, 2)

	writeSubscribeMessage := func(id string) {
		t.Helper()
		if err := writeClient(protocol.SubscribeMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "subscribe-" + id},
			Payload:  protocol.SubscribePayload{SubscriptionID: id, Resource: protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeSubscribeMessage("slow")
	<-provider.openStarted

	received := make(chan asyncClientMessage, 64)
	heartbeatPongs := make(chan struct{}, 4)
	var seenMu sync.Mutex
	var seen []protocol.Message
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			messageType, payload, err := connection.Read(context.Background())
			if err != nil {
				received <- asyncClientMessage{err: err}
				return
			}
			if messageType != websocket.MessageText {
				received <- asyncClientMessage{err: context.Canceled}
				return
			}
			message, err := protocol.DecodeMessage(payload)
			if err != nil {
				received <- asyncClientMessage{err: err}
				return
			}
			seenMu.Lock()
			seen = append(seen, message)
			seenMu.Unlock()
			if message.Kind() == protocol.MessageTypePing {
				if err := writeClient(protocol.PongMessage{
					Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypePong, ID: "client-pong"},
					Payload:  protocol.PongPayload{},
				}); err != nil {
					received <- asyncClientMessage{err: err}
					return
				}
				select {
				case heartbeatPongs <- struct{}{}:
				default:
				}
			}
			received <- asyncClientMessage{message: message}
		}
	}()

	waitForKind := func(kind protocol.MessageType) protocol.Message {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case result := <-received:
				if result.err != nil {
					t.Fatalf("reader error while waiting for %s: %v", kind, result.err)
				}
				if result.message.Kind() == kind {
					return result.message
				}
			case <-deadline.C:
				t.Fatalf("timed out waiting for %s", kind)
			}
		}
	}

	writeSubscribeMessage("other")
	if errorMessage, ok := waitForKind(protocol.MessageTypeError).(protocol.ErrorMessage); !ok || errorMessage.Payload.Code != "subscription_limit" {
		t.Fatalf("pending subscription limit response=%#v", errorMessage)
	}
	writeSubscribeMessage("slow")
	if errorMessage, ok := waitForKind(protocol.MessageTypeError).(protocol.ErrorMessage); !ok || errorMessage.Payload.Code != "subscription_exists" {
		t.Fatalf("pending duplicate response=%#v", errorMessage)
	}

	if err := writeClient(protocol.PingMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypePing, ID: "client-ping"}, Payload: protocol.PingPayload{}}); err != nil {
		t.Fatal(err)
	}
	waitForKind(protocol.MessageTypePong)
	// Drive two heartbeat ticks while Open remains blocked. The reader must
	// receive each ping and send its pong; the synchronous pre-B2 handler would
	// instead leave the heartbeat waiting until its timeout.
	clock.Advance(DefaultHeartbeatInterval)
	select {
	case <-heartbeatPongs:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat ping was not processed during slow Open")
	}
	endpoint.observer.waitForMessage(t, DirectionInbound, protocol.MessageTypePong)
	clock.Advance(DefaultPongTimeout - time.Second)
	select {
	case <-heartbeatPongs:
	case <-time.After(5 * time.Second):
		t.Fatal("second heartbeat ping was not processed during slow Open")
	}
	endpoint.observer.waitForMessage(t, DirectionInbound, protocol.MessageTypePong)

	if err := writeClient(protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "fast-command"},
		Payload:  protocol.CommandPayload{Name: "fake.fast", SchemaVersion: 1, RequestID: "fast-request", Arguments: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}
	waitForKind(protocol.MessageTypeCommandAccepted)
	waitForKind(protocol.MessageTypeCommandResult)

	if err := writeClient(protocol.UnsubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeUnsubscribe, ID: "unsubscribe-slow"},
		Payload:  protocol.UnsubscribePayload{SubscriptionID: "slow"},
	}); err != nil {
		t.Fatal(err)
	}
	waitForKind(protocol.MessageTypeUnsubscribed)
	close(provider.openGate)
	select {
	case <-provider.openFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("late Open did not return after pending unsubscribe")
	}
	waitForProviderCloses(t, provider, 1)
	waitForDispatcherState(t, dispatcher, 1, 0)
	seenMu.Lock()
	lateInitial := protocol.MessageType("")
	for _, message := range seen {
		if message.Kind() == protocol.MessageTypeSubscribed || message.Kind() == protocol.MessageTypeSnapshot {
			lateInitial = message.Kind()
			break
		}
	}
	seenMu.Unlock()
	if lateInitial != "" {
		t.Fatalf("pending unsubscribe received late %s", lateInitial)
	}
	if err := connection.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Log(err)
	}
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("client reader did not stop")
	}
}

func TestDispatcherConnectionCloseCancelsPendingOpenOverWebSocket(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	provider.openStarted = make(chan struct{}, 1)
	provider.openFinished = make(chan struct{}, 1)
	provider.openGate = make(chan struct{})
	provider.ignoreOpenCancel = true
	endpoint, dispatcher := newDispatcherForTest(t, provider)
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	writeHello(t, connection, "close-pending-client")
	if _, ok := readProtocol(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	writeSubscribe(t, connection, "close-pending", nil)
	<-provider.openStarted
	if err := connection.Close(websocket.StatusNormalClosure, "client closed"); err != nil {
		t.Log(err)
	}
	select {
	case <-endpoint.observer.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("connection did not close")
	}
	close(provider.openGate)
	select {
	case <-provider.openFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("pending Open did not return after connection close")
	}
	waitForProviderCloses(t, provider, 1)
	waitForDispatcherState(t, dispatcher, 0, 0)
	if provider.closed.Load() != 1 {
		t.Fatalf("late opened resource closes=%d, want exactly one", provider.closed.Load())
	}
}
