package protocol

import "testing"

func TestDebugControlMessagesRoundTrip(t *testing.T) {
	messages := []Message{
		DebugRegisterMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypeDebugRegister, ID: "debug-register-1"},
			Payload:  DebugRegisterPayload{PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1", Focused: true},
		},
		DebugFocusMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypeDebugFocus, ID: "debug-focus-1"},
			Payload:  DebugFocusPayload{PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1", Focused: false},
		},
		DebugFocusedMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypeDebugFocused, ID: "debug-focused-1"},
			Payload:  DebugFocusedPayload{PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1", Focused: true},
		},
		DebugUnregisterMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypeDebugUnregister, ID: "debug-unregister-1"},
			Payload:  DebugUnregisterPayload{PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1", Focused: false},
		},
		DebugUnregisteredMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypeDebugUnregistered, ID: "debug-unregistered-1"},
			Payload:  DebugUnregisteredPayload{PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1", Focused: false},
		},
		DebugRegisteredMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypeDebugRegistered, ID: "debug-registered-1"},
			Payload:  DebugRegisteredPayload{PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1", Focused: true},
		},
	}
	for _, original := range messages {
		encoded, err := EncodeMessage(original)
		if err != nil {
			t.Fatalf("EncodeMessage(%s) error = %v", original.Kind(), err)
		}
		decoded, err := DecodeMessage(encoded)
		if err != nil {
			t.Fatalf("DecodeMessage(%s) error = %v", original.Kind(), err)
		}
		if decoded.Kind() != original.Kind() {
			t.Fatalf("decoded kind = %q, want %q", decoded.Kind(), original.Kind())
		}
	}
}

func TestDecodeRejectsMalformedDebugControlMessages(t *testing.T) {
	tests := []struct {
		name string
		wire string
	}{
		{
			name: "missing focused",
			wire: `{"version":1,"type":"debug_register","id":"debug-1","payload":{"page_id":"page-1","page_epoch":"epoch-1","session_id":"session-1"}}`,
		},
		{
			name: "null focused",
			wire: `{"version":1,"type":"debug_register","id":"debug-1","payload":{"page_id":"page-1","page_epoch":"epoch-1","session_id":"session-1","focused":null}}`,
		},
		{
			name: "whitespace epoch",
			wire: `{"version":1,"type":"debug_register","id":"debug-1","payload":{"page_id":"page-1","page_epoch":" ","session_id":"session-1","focused":true}}`,
		},
		{
			name: "control character page",
			wire: "{\"version\":1,\"type\":\"debug_register\",\"id\":\"debug-1\",\"payload\":{\"page_id\":\"page-\\u0001\",\"page_epoch\":\"epoch-1\",\"session_id\":\"session-1\",\"focused\":true}}",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeMessage([]byte(test.wire)); err == nil {
				t.Fatal("DecodeMessage() error = nil, want validation error")
			}
		})
	}
}

func TestEncodeRejectsMalformedDebugIdentity(t *testing.T) {
	message := DebugRegisterMessage{
		Envelope: Envelope{Version: 1, Type: MessageTypeDebugRegister, ID: "debug-1"},
		Payload:  DebugRegisterPayload{PageID: "page-1", PageEpoch: " ", SessionID: "session-1"},
	}
	if _, err := EncodeMessage(message); err == nil {
		t.Fatal("EncodeMessage() error = nil, want malformed epoch error")
	}
}
