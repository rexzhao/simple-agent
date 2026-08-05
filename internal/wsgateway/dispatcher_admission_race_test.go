package wsgateway

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/syncengine"
)

type admissionTestConnection struct{ info ConnectionInfo }

func (c *admissionTestConnection) Send(protocol.Message) error { return nil }
func (c *admissionTestConnection) Info() ConnectionInfo        { return c.info }

func TestDispatcherCloseSerializesStateAdmissionAndTracksCleanup(t *testing.T) {
	provider := newDispatcherFakeProvider(t)
	registry := syncengine.NewProviderRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	engine, err := syncengine.NewEngine(registry)
	if err != nil {
		t.Fatal(err)
	}
	commandsRegistry, err := commands.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{Engine: engine, Commands: commandsRegistry})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				connection := &admissionTestConnection{info: ConnectionInfo{ConnectionID: fmt.Sprintf("admission-%d-%d", worker, i)}}
				_ = dispatcher.stateFor(ctx, connection)
			}
		}()
	}
	closed := make(chan struct{})
	go func() {
		dispatcher.Close()
		close(closed)
	}()
	wg.Wait()
	<-closed
	if got := dispatcher.ConnectionCount(); got != 0 {
		t.Fatalf("states remain after Close: %d", got)
	}
	dispatcher.taskMu.Lock()
	closing := dispatcher.closing
	dispatcher.taskMu.Unlock()
	if !closing {
		t.Fatal("dispatcher did not enter closing state")
	}
}
