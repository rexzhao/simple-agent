package protocol

import (
	"encoding/json"
	"os"
	"testing"
)

type fixtureFile struct {
	Valid   []protocolFixture `json:"valid"`
	Invalid []protocolFixture `json:"invalid"`
}

type protocolFixture struct {
	Name    string          `json:"name"`
	Message json.RawMessage `json:"message"`
}

func loadFixtures(t *testing.T) fixtureFile {
	t.Helper()
	data, err := os.ReadFile("testdata/fixtures.json")
	if err != nil {
		t.Fatalf("read shared fixtures: %v", err)
	}
	var fixtures fixtureFile
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("decode shared fixtures: %v", err)
	}
	return fixtures
}

func TestSharedGoldenFixtures(t *testing.T) {
	fixtures := loadFixtures(t)
	for _, fixture := range fixtures.Valid {
		fixture := fixture
		t.Run("valid/"+fixture.Name, func(t *testing.T) {
			message, err := DecodeMessage(fixture.Message)
			if err != nil {
				t.Fatalf("DecodeMessage() error = %v", err)
			}
			var envelope struct {
				Type MessageType `json:"type"`
			}
			if err := json.Unmarshal(fixture.Message, &envelope); err != nil {
				t.Fatal(err)
			}
			if message.Kind() != envelope.Type {
				t.Fatalf("Kind() = %q, want %q", message.Kind(), envelope.Type)
			}
			encoded, err := EncodeMessage(message)
			if err != nil {
				t.Fatalf("EncodeMessage() error = %v", err)
			}
			if _, err := DecodeMessage(encoded); err != nil {
				t.Fatalf("encoded message does not decode: %v", err)
			}
		})
	}

	for _, fixture := range fixtures.Invalid {
		fixture := fixture
		t.Run("invalid/"+fixture.Name, func(t *testing.T) {
			if _, err := DecodeMessage(fixture.Message); err == nil {
				t.Fatal("DecodeMessage() error = nil, want validation error")
			}
		})
	}
}

func TestDecodeIgnoresUnknownOptionalFields(t *testing.T) {
	message, err := DecodeMessage([]byte(`{
		"version": 1,
		"type": "ping",
		"id": "ping_1",
		"future_optional": {"ignored": true},
		"payload": {"future_payload_field": "ignored"}
	}`))
	if err != nil {
		t.Fatalf("DecodeMessage() error = %v", err)
	}
	if message.Kind() != MessageTypePing {
		t.Fatalf("Kind() = %q, want %q", message.Kind(), MessageTypePing)
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	if _, err := DecodeMessage([]byte(`{"version":1`)); err == nil {
		t.Fatal("DecodeMessage() error = nil, want invalid JSON error")
	} else {
		protocolErr, ok := err.(*ProtocolError)
		if !ok || protocolErr.Code != ErrorCodeInvalidJSON {
			t.Fatalf("error = %#v, want invalid JSON ProtocolError", err)
		}
	}
}

func TestEncodePreservesResourceSpecificRawFields(t *testing.T) {
	fixtures := loadFixtures(t)
	for _, name := range []string{"change", "subscription_event"} {
		var fixture protocolFixture
		for _, candidate := range fixtures.Valid {
			if candidate.Name == name {
				fixture = candidate
				break
			}
		}
		if len(fixture.Message) == 0 {
			t.Fatalf("fixture %q not found", name)
		}
		message, err := DecodeMessage(fixture.Message)
		if err != nil {
			t.Fatalf("DecodeMessage(%s) error = %v", name, err)
		}
		encoded, err := EncodeMessage(message)
		if err != nil {
			t.Fatalf("EncodeMessage(%s) error = %v", name, err)
		}
		var wire struct {
			Payload struct {
				Operations []map[string]json.RawMessage `json:"operations"`
				Event      map[string]json.RawMessage   `json:"event"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatalf("decode encoded %s: %v", name, err)
		}
		if name == "change" {
			if _, ok := wire.Payload.Operations[0]["metadata"]; !ok {
				t.Fatal("encoded change lost resource-specific metadata field")
			}
			if _, ok := wire.Payload.Operations[1]["reason"]; !ok {
				t.Fatal("encoded change lost resource-specific reason field")
			}
		} else {
			for _, field := range []string{"session_id", "run_id", "run_cursor", "item_id", "delta"} {
				if _, ok := wire.Payload.Event[field]; !ok {
					t.Fatalf("encoded event lost resource-specific %s field", field)
				}
			}
		}
	}
}

func TestEncodeValidatesTimestampTraceAndBlobExpiry(t *testing.T) {
	invalidMessages := []Message{
		PingMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypePing, ID: "ping_1", Timestamp: "not-a-timestamp"},
			Payload:  PingPayload{},
		},
		PingMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypePing, ID: "ping_1", TraceID: " "},
			Payload:  PingPayload{},
		},
	}
	for index, message := range invalidMessages {
		if _, err := EncodeMessage(message); err == nil {
			t.Fatalf("EncodeMessage(invalid %d) error = nil", index)
		}
	}

	message := SnapshotMessage{
		Envelope: Envelope{Version: 1, Type: MessageTypeSnapshot, ID: "snapshot_1"},
		Payload: SnapshotPayload{
			SubscriptionID:   "session-content:session_1",
			Resource:         ResourceKey{Type: ResourceTypeSessionContent, ID: "session_1"},
			StreamEpoch:      "stream_1",
			Sequence:         Sequence("1"),
			ResourceRevision: ResourceRevision("revision-1"),
			Content: SnapshotContent{Blob: &BlobDescriptor{
				ID: "blob_1", URL: "/api/blobs/blob_1", ContentType: "application/json",
				Size: 1, SHA256: "abc123", ETag: "etag-1", ExpiresAt: "not-a-timestamp",
			}},
		},
	}
	if _, err := EncodeMessage(message); err == nil {
		t.Fatal("EncodeMessage(invalid blob expiry) error = nil")
	}
}

func TestFailedCommandResultCarriesTypedError(t *testing.T) {
	fixtures := loadFixtures(t)
	for _, fixture := range fixtures.Valid {
		if fixture.Name != "command_result_failed" {
			continue
		}
		message, err := DecodeMessage(fixture.Message)
		if err != nil {
			t.Fatal(err)
		}
		result, ok := message.(CommandResultMessage)
		if !ok {
			t.Fatalf("decoded type = %T, want CommandResultMessage", message)
		}
		if result.Payload.Status != CommandStatusFailed || result.Payload.Error == nil {
			t.Fatalf("failed command result = %#v, want typed error", result.Payload)
		}
		return
	}
	t.Fatal("command_result_failed fixture not found")
}

func TestEncodeRejectsMismatchedRawChangeOperation(t *testing.T) {
	for _, testCase := range []struct {
		name string
		raw  string
	}{
		{name: "empty raw op", raw: `{"op":"","field":"display_name"}`},
		{name: "mismatched raw op", raw: `{"op":"item.remove","item_id":"item_1"}`},
		{name: "raw is not an object", raw: `[]`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			message := ChangeMessage{
				Envelope: Envelope{Version: 1, Type: MessageTypeChange, ID: "change_encode_1"},
				Payload: ChangePayload{
					SubscriptionID:   "session-index:project_1",
					Resource:         ResourceKey{Type: ResourceTypeSessionIndex, ID: "project_1"},
					StreamEpoch:      "stream_1",
					Sequence:         Sequence("2"),
					PreviousSequence: Sequence("1"),
					ResourceRevision: ResourceRevision("revision-2"),
					Operations: []ChangeOperation{{
						Op:  "metadata.replace",
						Raw: json.RawMessage(testCase.raw),
					}},
				},
			}
			if _, err := EncodeMessage(message); err == nil {
				t.Fatal("EncodeMessage() error = nil, want raw operation validation error")
			}
		})
	}
}
