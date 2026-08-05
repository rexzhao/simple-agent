package wsgateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

func TestDispatcherCloseWaitsForCommandTask(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	started := make(chan struct{})
	release := make(chan struct{})
	definition := commands.CommandDefinition{
		Name: "fake.shutdown", SchemaVersion: 1,
		Execute: func(context.Context, commands.CommandRequest) (json.RawMessage, error) {
			close(started)
			<-release
			return json.RawMessage(`{"ok":true}`), nil
		},
	}
	endpoint, dispatcher := newDispatcherForTest(t, provider, definition)
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeHello(t, connection, "shutdown-client")
	_ = readProtocol(t, connection)
	writeProtocol(t, connection, protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "shutdown-command"},
		Payload:  protocol.CommandPayload{Name: "fake.shutdown", SchemaVersion: 1, RequestID: "shutdown-request", Arguments: json.RawMessage(`{}`)},
	})
	_ = readProtocol(t, connection)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("command did not start")
	}
	closed := make(chan struct{})
	go func() {
		dispatcher.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("dispatcher Close returned before command task converged")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher Close did not wait then converge")
	}
}
