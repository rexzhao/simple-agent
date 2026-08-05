package wsgateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

func blockingCommandHandler(entered, exited chan struct{}) Handler {
	var once sync.Once
	return HandlerFunc(func(ctx context.Context, _ Connection, message protocol.Message) error {
		if message.Kind() != protocol.MessageTypeCommand {
			return ErrUnsupportedMessage
		}
		once.Do(func() { close(entered) })
		<-ctx.Done()
		close(exited)
		return nil
	})
}

func writeBlockingCommand(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	writeProtocol(t, connection, protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "blocking-command"},
		Payload: protocol.CommandPayload{
			Name:          "fake.blocking",
			SchemaVersion: 1,
			RequestID:     "blocking-request",
			Arguments:     []byte(`{}`),
		},
	})
}

func waitClosed(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish", label)
	}
}

func TestGatewayShutdownCancelsContextAwareHandler(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	endpoint := newTestEndpoint(t, Options{Handler: blockingCommandHandler(entered, exited)})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	writeHello(t, connection, "blocking-shutdown")
	_ = readProtocol(t, connection)
	writeBlockingCommand(t, connection)
	waitClosed(t, entered, "blocking handler entry")

	endpoint.cancel()
	if got := readClose(t, connection); got != websocket.StatusGoingAway {
		t.Fatalf("shutdown close code=%v, want going away", got)
	}
	waitClosed(t, exited, "context-aware handler shutdown")
	_ = connection.Close(websocket.StatusNormalClosure, "test done")
}

func TestGatewayHeartbeatCancelsContextAwareHandler(t *testing.T) {
	clock := newManualClock()
	entered := make(chan struct{})
	exited := make(chan struct{})
	endpoint := newTestEndpoint(t, Options{
		Clock:   clock,
		Handler: blockingCommandHandler(entered, exited),
		Limits:  Limits{HeartbeatInterval: 15 * time.Second, PongTimeout: 45 * time.Second},
	})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	writeHello(t, connection, "blocking-heartbeat")
	_ = readProtocol(t, connection)
	writeBlockingCommand(t, connection)
	waitClosed(t, entered, "blocking handler entry")
	clock.WaitForTickers(t, 2)

	clock.Advance(45 * time.Second)
	if got := readClose(t, connection); got != websocket.StatusPolicyViolation {
		t.Fatalf("heartbeat close code=%v, want policy violation", got)
	}
	waitClosed(t, exited, "context-aware handler heartbeat termination")
	_ = connection.Close(websocket.StatusNormalClosure, "test done")
}
