package syncengine

import (
	"encoding/json"
	"testing"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

func transientTestEvent(t *testing.T, event protocol.TransientSubscriptionEvent) TransientEvent {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return TransientEvent{RunEpoch: "epoch-1", RunID: event.RunID, Cursor: event.RunCursor, Event: raw, Bytes: len(raw)}
}

func TestTransientSubscriptionKeepsRunCursorIndependentAndRejectsGaps(t *testing.T) {
	sub, err := NewTransientSubscription("epoch-1", "", 0, 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	termCh := sub.Delivery().Terminal
	started := transientTestEvent(t, protocol.TransientSubscriptionEvent{
		Type: protocol.SubscriptionEventRunStarted, SessionID: "session-1", RunID: "run-1", RunCursor: "1", Status: "running",
	})
	if !sub.Offer(started) {
		t.Fatal("run.started was rejected")
	}
	text := transientTestEvent(t, protocol.TransientSubscriptionEvent{
		Type: protocol.SubscriptionEventAssistantMessageUpdated, SessionID: "session-1", RunID: "run-1", RunCursor: "2",
		TurnID: "turn-1", AgentIteration: 1, ItemID: "item-1", MessageRevision: "1", AssistantContent: "hello",
	})
	if !sub.Offer(text) {
		t.Fatal("assistant message update was rejected")
	}
	if got := sub.RunCursor(); got != "2" {
		t.Fatalf("RunCursor() = %q, want 2", got)
	}

	wrongRun := transientTestEvent(t, protocol.TransientSubscriptionEvent{
		Type: protocol.SubscriptionEventAssistantMessageUpdated, SessionID: "session-1", RunID: "run-old", RunCursor: "3",
		TurnID: "turn-1", AgentIteration: 1, ItemID: "item-1", MessageRevision: "2", AssistantContent: "hello late",
	})
	if sub.Offer(wrongRun) {
		t.Fatal("wrong-run event was accepted")
	}
	select {
	case terminal := <-termCh:
		if terminal.Reason == nil {
			t.Fatal("wrong-run terminal has no reason")
		}
	default:
		t.Fatal("wrong-run event did not mark the subscription desynced")
	}
}

func TestTransientSubscriptionAcceptsNewRunOnlyAtCursorOne(t *testing.T) {
	sub, err := NewTransientSubscription("epoch-1", "", 0, 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for _, runID := range []string{"run-1", "run-2"} {
		if !sub.Offer(transientTestEvent(t, protocol.TransientSubscriptionEvent{
			Type: protocol.SubscriptionEventRunStarted, SessionID: "session-1", RunID: runID, RunCursor: "1", Status: "running",
		})) {
			t.Fatalf("run.started for %s was rejected", runID)
		}
	}
	if got := sub.RunCursor(); got != "1" {
		t.Fatalf("new run cursor = %q, want 1", got)
	}
}

func TestTransientSubscriptionOverflowIsTerminal(t *testing.T) {
	sub, err := NewTransientSubscription("epoch-1", "run-1", 0, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	started := transientTestEvent(t, protocol.TransientSubscriptionEvent{
		Type: protocol.SubscriptionEventRunStarted, SessionID: "session-1", RunID: "run-1", RunCursor: "1", Status: "running",
	})
	if sub.Offer(started) {
		t.Fatal("event over byte bound was accepted")
	}
	select {
	case terminal := <-sub.Delivery().Terminal:
		if terminal.Reason == nil {
			t.Fatal("overflow terminal has no reason")
		}
	default:
		t.Fatal("overflow did not produce a terminal")
	}
}

func TestTransientEventRejectsNonCanonicalCursor(t *testing.T) {
	raw := json.RawMessage(`{"type":"run.started","session_id":"session-1","run_id":"run-1","run_cursor":"01","status":"running"}`)
	event := TransientEvent{RunEpoch: "epoch-1", RunID: "run-1", Cursor: "01", Event: raw}
	if err := event.Validate(); err == nil {
		t.Fatal("non-canonical run cursor was accepted")
	}
}
