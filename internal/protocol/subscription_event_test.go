package protocol

import (
	"encoding/json"
	"testing"
)

func TestSubscriptionEventRejectsUnknownAndVariantFields(t *testing.T) {
	valid := json.RawMessage(`{"type":"text.delta","session_id":"session-1","run_id":"run-1","run_cursor":"2","turn_id":"turn-1","agent_iteration":1,"item_id":"item-1","delta":"hello"}`)
	if err := ValidateSubscriptionEvent(valid); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	for name, raw := range map[string]json.RawMessage{
		"unknown field":       json.RawMessage(`{"type":"text.delta","session_id":"session-1","run_id":"run-1","run_cursor":"2","turn_id":"turn-1","agent_iteration":1,"item_id":"item-1","delta":"hello","future":true}`),
		"noncanonical cursor": json.RawMessage(`{"type":"text.delta","session_id":"session-1","run_id":"run-1","run_cursor":"02","turn_id":"turn-1","agent_iteration":1,"item_id":"item-1","delta":"hello"}`),
		"wrong variant field": json.RawMessage(`{"type":"text.delta","session_id":"session-1","run_id":"run-1","run_cursor":"2","turn_id":"turn-1","agent_iteration":1,"item_id":"item-1","delta":"hello","name":"shell"}`),
		"null boolean":        json.RawMessage(`{"type":"tool.finished","session_id":"session-1","run_id":"run-1","run_cursor":"2","turn_id":"turn-1","agent_iteration":1,"tool_call_id":"call-1","name":"shell","is_error":null}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSubscriptionEvent(raw); err == nil {
				t.Fatal("invalid event was accepted")
			}
		})
	}
}

func TestSubscriptionEventMarshalRejectsVariantFieldsAndNilWatermarkItems(t *testing.T) {
	if _, err := json.Marshal(TransientSubscriptionEvent{
		Type: SubscriptionEventTextDelta, SessionID: "session-1", RunID: "run-1", RunCursor: "1",
		TurnID: "turn-1", AgentIteration: 1, ItemID: "item-1", Delta: "hello", Name: "unexpected",
	}); err == nil {
		t.Fatal("typed event with a variant-only field was marshaled")
	}
	if _, err := json.Marshal(TransientSubscriptionEvent{
		Type: SubscriptionEventRunSettled, SessionID: "session-1", RunID: "run-1", RunCursor: "1",
		Status: "committed", Settlement: &DurableSettlementWatermark{
			ResourceRevision: "4", RunCursor: "1", Verified: false,
		}}); err == nil {
		t.Fatal("settlement with nil covered_items was marshaled")
	}
	if _, err := json.Marshal(TransientSubscriptionEvent{
		Type: SubscriptionEventRunSettled, SessionID: "session-1", RunID: "run-1", RunCursor: "2",
		Status: "committed", Settlement: &DurableSettlementWatermark{
			ResourceRevision: "4", RunCursor: "2", Verified: false,
			CoveredItems: []TransientItemWatermark{{TurnID: "turn-1", AgentIteration: 1, ItemID: "item-1", RunCursor: "1"}},
		}}); err == nil {
		t.Fatal("unverified settlement with covered items was marshaled")
	}
	if _, err := json.Marshal(TransientSubscriptionEvent{
		Type: SubscriptionEventRunSettled, SessionID: "session-1", RunID: "run-1", RunCursor: "2",
		Status: "committed", Settlement: &DurableSettlementWatermark{
			ResourceRevision: "4", RunCursor: "1", Verified: true,
			CoveredItems: []TransientItemWatermark{{TurnID: "turn-1", AgentIteration: 1, ItemID: "item-1", RunCursor: "2"}},
		}}); err == nil {
		t.Fatal("settlement with a covered cursor after the watermark was marshaled")
	}
}

func TestSubscriptionEventMessageBindsSessionIdentityToResource(t *testing.T) {
	message := SubscriptionEventMessage{
		Envelope: Envelope{Version: 1, Type: MessageTypeSubscriptionEvent, ID: "event-1"},
		Payload: SubscriptionEventPayload{
			SubscriptionID: "sub-1",
			Resource:       ResourceKey{Type: ResourceTypeSessionContent, ID: "session-a"},
			Event:          json.RawMessage(`{"type":"run.started","session_id":"session-b","run_id":"run-1","run_cursor":"1","status":"running"}`),
		},
	}
	if _, err := EncodeMessage(message); err == nil {
		t.Fatal("subscription event with a different session identity was encoded")
	}
}

func TestRunCursorComparisonsRequireCanonicalDecimal(t *testing.T) {
	if _, err := CompareRunCursor("01", "1"); err == nil {
		t.Fatal("non-canonical run cursor was compared")
	}
	comparison, err := CompareRunCursor("2", "10")
	if err != nil || comparison >= 0 {
		t.Fatalf("CompareRunCursor(2, 10) = %d/%v, want -1/nil", comparison, err)
	}
}
