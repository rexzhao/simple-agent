package wsgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

func readCloseDetails(t *testing.T, connection *websocket.Conn) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := connection.Read(ctx)
	if err == nil {
		t.Fatal("read after close succeeded")
	}
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("close error=%v, want websocket.CloseError", err)
	}
	return closeErr.Code, closeErr.Reason
}

func TestGatewayHandshakeTimeoutWithoutHello(t *testing.T) {
	clock := newManualClock()
	endpoint := newTestEndpoint(t, Options{
		Clock: clock,
		Limits: Limits{
			HandshakeTimeout:  10 * time.Second,
			HeartbeatInterval: 15 * time.Second,
			PongTimeout:       45 * time.Second,
		},
	})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	clock.WaitForTickers(t, 1)
	clock.Advance(10 * time.Second)
	code, reason := readCloseDetails(t, connection)
	if code != websocket.StatusProtocolError || reason != closeReasonHandshakeTimeout {
		t.Fatalf("handshake timeout close=%v/%q, want protocol error/%q", code, reason, closeReasonHandshakeTimeout)
	}
}

func TestGatewayTimelyHelloCancelsHandshakeTimeout(t *testing.T) {
	clock := newManualClock()
	endpoint := newTestEndpoint(t, Options{
		Clock: clock,
		Limits: Limits{
			HandshakeTimeout:  10 * time.Second,
			HeartbeatInterval: 15 * time.Second,
			PongTimeout:       45 * time.Second,
		},
	})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	writeHello(t, connection, "timely-hello")
	if _, ok := readProtocol(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("timely hello did not receive welcome")
	}
	clock.WaitForTickers(t, 2)

	// Advancing beyond the handshake deadline must not close a connection that
	// already completed hello. An application ping/pong also proves the reader
	// and writer remain live.
	clock.Advance(11 * time.Second)
	writeProtocol(t, connection, protocol.PingMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypePing, ID: "post-handshake-ping"},
		Payload:  protocol.PingPayload{},
	})
	for i := 0; i < 2; i++ {
		if message := readProtocol(t, connection); message.Kind() == protocol.MessageTypePong {
			return
		}
	}
	t.Fatal("post-handshake ping did not receive pong")
}

func TestGatewayShutdownWinsBeforeHandshakeTimeout(t *testing.T) {
	clock := newManualClock()
	endpoint := newTestEndpoint(t, Options{
		Clock: clock,
		Limits: Limits{
			HandshakeTimeout:  10 * time.Second,
			HeartbeatInterval: 15 * time.Second,
			PongTimeout:       45 * time.Second,
		},
	})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	clock.WaitForTickers(t, 1)
	endpoint.cancel()
	code, reason := readCloseDetails(t, connection)
	if code != websocket.StatusGoingAway || reason != "server shutdown" {
		t.Fatalf("shutdown close=%v/%q, want going away/server shutdown", code, reason)
	}
}
