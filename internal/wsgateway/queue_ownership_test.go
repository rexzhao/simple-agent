package wsgateway

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/rexzhao/simple-agent/internal/protocol"
)

func ownedChange(subscriptionID string, sequence, previous string) protocol.ChangeMessage {
	return protocol.ChangeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeChange, ID: "change-" + subscriptionID + "-" + sequence},
		Payload: protocol.ChangePayload{
			SubscriptionID: subscriptionID,
			Resource:       protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: "server"},
			StreamEpoch:    "epoch",
			Sequence:       protocol.Sequence(sequence), PreviousSequence: protocol.Sequence(previous),
			ResourceRevision: protocol.ResourceRevision(sequence),
			Operations:       []protocol.ChangeOperation{{Op: "upsert"}},
		},
	}
}

func TestOutboundOverflowPurgesOnlyOwnedChangesAndQueuesRecovery(t *testing.T) {
	gateway, err := New(Options{Limits: Limits{MaxMessageBytes: 4096, MaxOutboundMessages: 4, MaxOutboundBytes: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	connection := newConnection(gateway, nil, TicketClaims{}, "owned-queue", nil)
	if err := connection.queue.enqueue(outboundFrame{kind: frameMessage, payload: []byte("a-old"), subscriptionID: "a", purgeable: true}); err != nil {
		t.Fatal(err)
	}
	if err := connection.queue.enqueue(outboundFrame{kind: frameMessage, payload: []byte("a-old-2"), subscriptionID: "a", purgeable: true}); err != nil {
		t.Fatal(err)
	}
	if err := connection.queue.enqueue(outboundFrame{kind: frameMessage, payload: []byte("b-old"), subscriptionID: "b", purgeable: true}); err != nil {
		t.Fatal(err)
	}
	if err := connection.queue.enqueue(outboundFrame{kind: frameMessage, payload: []byte("control")}); err != nil {
		t.Fatal(err)
	}
	purgedCallback := false
	err = connection.SendWithOptions(ownedChange("a", "4", "3"), SendOptions{SubscriptionID: "a", OnWritten: func() { purgedCallback = true }})
	if !errors.Is(err, ErrSubscriptionDesynced) {
		t.Fatalf("overflow error=%v, want ErrSubscriptionDesynced", err)
	}
	if purgedCallback {
		t.Fatal("purged change callback ran")
	}
	if connection.isTerminated() {
		t.Fatal("subscription overflow closed the connection")
	}
	connection.queue.mu.Lock()
	items := append([]outboundFrame(nil), connection.queue.items...)
	connection.queue.mu.Unlock()
	if len(items) != 3 {
		t.Fatalf("queue items=%d, want B/control/recovery", len(items))
	}
	if string(items[0].payload) != "b-old" || string(items[1].payload) != "control" {
		t.Fatalf("unrelated frames changed: %#v", items)
	}
	var recovery protocol.Envelope
	if err := json.Unmarshal(items[2].payload, &recovery); err != nil {
		t.Fatal(err)
	}
	if recovery.Type != protocol.MessageTypeResyncRequired {
		t.Fatalf("recovery type=%s", recovery.Type)
	}
	if err := connection.Send(ownedChange("a", "5", "4")); !errors.Is(err, ErrSubscriptionDesynced) {
		t.Fatalf("post-desync send=%v", err)
	}
	if err := connection.Send(ownedChange("b", "4", "3")); err != nil {
		t.Fatalf("other subscription send=%v", err)
	}
}
