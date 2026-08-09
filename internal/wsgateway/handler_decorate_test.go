package wsgateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

type decorateTestHandler struct{}

func (*decorateTestHandler) Handle(context.Context, Connection, protocol.Message) error { return nil }

func TestDecorateHandlerPreservesAndWrapsDelegate(t *testing.T) {
	delegate := &decorateTestHandler{}
	gateway, err := New(Options{Handler: delegate})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &decorateTestHandler{}
	if err := gateway.DecorateHandler(func(previous Handler) Handler {
		if previous != delegate {
			t.Fatalf("decorator delegate = %T, want original delegate", previous)
		}
		return wrapped
	}); err != nil {
		t.Fatal(err)
	}
	if got := gateway.handlerSnapshot(); got != wrapped {
		t.Fatalf("installed handler = %T, want decorated handler", got)
	}
}

func TestDecorateHandlerRejectsAfterHTTPHandlerStarts(t *testing.T) {
	gateway, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "http://example.test/api/ws", nil)
	gateway.HTTPHandler(context.Background(), httptest.NewRecorder(), request, TicketClaims{})
	if err := gateway.DecorateHandler(func(Handler) Handler {
		return HandlerFunc(func(context.Context, Connection, protocol.Message) error { return nil })
	}); !errors.Is(err, ErrGatewayAlreadyServing) {
		t.Fatalf("DecorateHandler() error = %v, want ErrGatewayAlreadyServing", err)
	}
}

func TestDecorateHandlerAndServeStartHaveOneSafeOutcome(t *testing.T) {
	for i := 0; i < 100; i++ {
		gateway, err := New(Options{Handler: HandlerFunc(func(context.Context, Connection, protocol.Message) error { return nil })})
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		result := make(chan error, 1)
		var sawNil atomic.Bool
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			result <- gateway.DecorateHandler(func(previous Handler) Handler {
				if previous == nil {
					sawNil.Store(true)
				}
				return HandlerFunc(func(context.Context, Connection, protocol.Message) error { return nil })
			})
		}()
		go func() {
			defer wait.Done()
			<-start
			gateway.markServeStarted()
		}()
		close(start)
		wait.Wait()
		if err := <-result; err != nil && !errors.Is(err, ErrGatewayAlreadyServing) {
			t.Fatalf("iteration %d: DecorateHandler() error = %v", i, err)
		}
		if sawNil.Load() {
			t.Fatalf("iteration %d: decorator saw nil delegate", i)
		}
	}
}
