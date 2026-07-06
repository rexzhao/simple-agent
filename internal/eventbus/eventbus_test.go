package eventbus

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestBusPublishDurableInvokesHandlerAndFansOut(t *testing.T) {
	var handled []string
	bus := NewBus(func(event Event) error {
		handled = append(handled, event.Kind())
		return nil
	})
	defer bus.Close()

	sub := bus.Subscribe()
	event := TurnStarted{TurnID: "turn-1"}
	if err := bus.Publish(event); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got, want := handled, []string{KindTurnStarted}; !equalStrings(got, want) {
		t.Fatalf("handled = %#v, want %#v", got, want)
	}
	assertReceived(t, sub, event)
}

func TestBusPublishDurableErrorDoesNotFanOutAndReleasesSerialization(t *testing.T) {
	handlerErr := errors.New("write failed")
	bus := NewBus(func(event Event) error {
		if started, ok := event.(TurnStarted); ok && started.TurnID == "bad" {
			return handlerErr
		}
		return nil
	})
	defer bus.Close()

	sub := bus.Subscribe()
	if err := bus.Publish(TurnStarted{TurnID: "bad"}); !errors.Is(err, handlerErr) {
		t.Fatalf("Publish() error = %v, want %v", err, handlerErr)
	}
	assertNoEvent(t, sub)

	good := TurnStarted{TurnID: "good"}
	if err := bus.Publish(good); err != nil {
		t.Fatalf("Publish() after handler error = %v", err)
	}
	assertReceived(t, sub, good)
}

func TestBusPublishTransientBypassesHandler(t *testing.T) {
	var handlerCalls int
	bus := NewBus(func(Event) error {
		handlerCalls++
		return nil
	})
	defer bus.Close()

	sub := bus.Subscribe()
	event := ModelEvent{Event: model.TextDeltaEvent{Text: "hello"}}
	if err := bus.Publish(event); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if handlerCalls != 0 {
		t.Fatalf("handlerCalls = %d, want 0", handlerCalls)
	}
	assertReceived(t, sub, event)
}

func TestBusLosslessSubscriptionDoesNotDropTransientEvents(t *testing.T) {
	const eventCount = defaultSubscriberBuffer + 32

	bus := NewBus(nil)
	defer bus.Close()

	sub := bus.SubscribeLossless(1)
	published := make(chan error, 1)
	go func() {
		for i := 0; i < eventCount; i++ {
			if err := bus.Publish(ModelEvent{Event: model.TextDeltaEvent{Text: "x"}}); err != nil {
				published <- err
				return
			}
		}
		published <- nil
	}()

	for i := 0; i < eventCount; i++ {
		select {
		case got := <-sub:
			if _, ok := got.(ModelEvent); !ok {
				t.Fatalf("event %d = %T, want ModelEvent", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
	if err := <-published; err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
}

func TestBusCloseUnblocksLosslessPublish(t *testing.T) {
	bus := NewBus(nil)
	sub := bus.SubscribeLossless(1)
	if err := bus.Publish(ModelEvent{Event: model.TextDeltaEvent{Text: "first"}}); err != nil {
		t.Fatalf("Publish(first) error = %v", err)
	}

	attempting := make(chan struct{})
	published := make(chan error, 1)
	go func() {
		close(attempting)
		published <- bus.Publish(ModelEvent{Event: model.TextDeltaEvent{Text: "blocked"}})
	}()
	<-attempting
	select {
	case err := <-published:
		t.Fatalf("Publish(blocked) returned before Close with error %v", err)
	default:
	}

	bus.Close()
	select {
	case err := <-published:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Publish(blocked) error = %v, want %v", err, ErrClosed)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish(blocked) did not unblock after Close")
	}

	<-sub
	if _, ok := <-sub; ok {
		t.Fatal("subscription remained open after Close")
	}
}

func TestBusClose(t *testing.T) {
	bus := NewBus(func(Event) error { return nil })
	sub := bus.Subscribe()

	bus.Close()
	bus.Close()

	if _, ok := <-sub; ok {
		t.Fatal("subscription remained open after Close")
	}
	if err := bus.Publish(TurnStarted{TurnID: "turn-1"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish() error = %v, want %v", err, ErrClosed)
	}
	closedSub := bus.Subscribe()
	if _, ok := <-closedSub; ok {
		t.Fatal("subscription opened after Close")
	}
}

func TestBusSerializesConcurrentDurablePublishes(t *testing.T) {
	const publishCount = 24

	var inHandler int32
	var overlap int32
	var handled int32
	bus := NewBus(func(Event) error {
		if atomic.AddInt32(&inHandler, 1) != 1 {
			atomic.StoreInt32(&overlap, 1)
		}
		time.Sleep(time.Millisecond)
		atomic.AddInt32(&inHandler, -1)
		atomic.AddInt32(&handled, 1)
		return nil
	})
	defer bus.Close()

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, publishCount)
	for i := 0; i < publishCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- bus.Publish(TurnStarted{TurnID: "turn"})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
	}
	if overlap != 0 {
		t.Fatal("durable handler ran concurrently")
	}
	if got := atomic.LoadInt32(&handled); got != publishCount {
		t.Fatalf("handled = %d, want %d", got, publishCount)
	}
}

func TestEventKinds(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{"turn started", TurnStarted{TurnID: "turn"}, KindTurnStarted},
		{"compaction requested", CompactionRequested{TurnID: "turn", Summary: sessions.SessionItem{ID: "summary"}, Checkpoint: sessions.CompactionCheckpoint{ID: "checkpoint"}}, KindCompactionRequested},
		{"turn input ready", TurnInputReady{TurnID: "turn", Message: model.Message{Role: model.MessageRoleUser, Content: "hi"}}, KindTurnInputReady},
		{"assistant ready", AssistantReady{TurnID: "turn", Message: model.Message{Role: model.MessageRoleAssistant, Content: "hello"}}, KindAssistantReady},
		{"tool result ready", ToolResultReady{TurnID: "turn", Result: model.ToolResult{ToolCallID: "call", Content: "ok"}}, KindToolResultReady},
		{"turn completed", TurnCompleted{TurnID: "turn"}, KindTurnCompleted},
		{"turn interrupted", TurnInterrupted{TurnID: "turn"}, KindTurnInterrupted},
		{"model event", ModelEvent{Event: model.TextDeltaEvent{Text: "hi"}}, KindModelEvent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.event.Kind(); got != tt.want {
				t.Fatalf("Kind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPublishRejectsUnsupportedEvent(t *testing.T) {
	bus := NewBus(func(Event) error { return nil })
	defer bus.Close()

	err := bus.Publish(unsupportedEvent{})
	if err == nil {
		t.Fatal("Publish() error = nil, want unsupported event error")
	}
}

type unsupportedEvent struct{}

func (unsupportedEvent) Kind() string { return "unsupported" }

func assertReceived[T comparable](t *testing.T, ch <-chan Event, want T) {
	t.Helper()

	select {
	case got := <-ch:
		typed, ok := got.(T)
		if !ok {
			t.Fatalf("received %T, want %T", got, want)
		}
		if typed != want {
			t.Fatalf("received %#v, want %#v", typed, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %T", want)
	}
}

func assertNoEvent(t *testing.T, ch <-chan Event) {
	t.Helper()

	select {
	case got := <-ch:
		t.Fatalf("received unexpected event %#v", got)
	default:
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
