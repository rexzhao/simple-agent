package wsgateway

import (
	"context"
	"errors"
	"testing"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

func TestDispatcherCloseWaitsForAdmittedHandleAndRejectsLateStateMutation(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	registry := syncengine.NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	engine, err := syncengine.NewEngine(registry)
	if err != nil {
		t.Fatal(err)
	}
	commandRegistry, err := commands.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	handleEntered := make(chan struct{})
	releaseHandle := make(chan struct{})
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Engine: engine, Commands: commandRegistry,
		BeforeHandleDispatch: func() {
			close(handleEntered)
			<-releaseHandle
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := &admissionTestConnection{info: ConnectionInfo{ConnectionID: "handle-close", Principal: "principal"}}
	handleDone := make(chan error, 1)
	go func() {
		handleDone <- dispatcher.Handle(context.Background(), connection, protocol.CommandMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "handle"},
			Payload:  protocol.CommandPayload{Name: "not-used", SchemaVersion: 1, RequestID: "request", Arguments: []byte(`{}`)},
		})
	}()
	<-handleEntered
	dispatcher.mu.Lock()
	state := dispatcher.states["handle-close"]
	dispatcher.mu.Unlock()
	if state == nil {
		t.Fatal("admitted Handle did not install connection state")
	}

	closeDone := make(chan struct{})
	go func() {
		dispatcher.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("Close returned while an admitted Handle was blocked")
	default:
	}
	// connectionState.done closes only after Close has cancelled and marked
	// this state. The admitted Handle is still held by its test hook here.
	<-state.done
	select {
	case <-closeDone:
		t.Fatal("Close returned before the admitted Handle was released")
	default:
	}
	close(releaseHandle)
	if err := <-handleDone; !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("blocked Handle error=%v, want ErrConnectionClosed", err)
	}
	<-closeDone
	if got := dispatcher.ConnectionCount(); got != 0 {
		t.Fatalf("connections after Close=%d", got)
	}
	if got := dispatcher.InflightCommandCount(); got != 0 {
		t.Fatalf("inflight commands after Close=%d", got)
	}
	if got := dispatcher.CommandCacheCount(); got != 0 {
		t.Fatalf("command cache after Close=%d", got)
	}

	if err := dispatcher.Handle(context.Background(), connection, protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "late"},
		Payload:  protocol.CommandPayload{Name: "not-used", SchemaVersion: 1, RequestID: "late-request", Arguments: []byte(`{}`)},
	}); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("Handle after Close=%v, want ErrConnectionClosed", err)
	}
	if got := dispatcher.ConnectionCount(); got != 0 {
		t.Fatalf("late Handle created state: %d", got)
	}
}
