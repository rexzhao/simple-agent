package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

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
		DebugExecuteMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypeDebugExecute, ID: "execution-1"},
			Payload: DebugExecutionPayload{
				ExecutionID: "execution-1", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1",
				Code: "await Promise.resolve({ok: true})", TimeoutMS: 500,
			},
		},
		DebugExecutionResultMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypeDebugExecutionResult, ID: "result-1"},
			Payload: DebugExecutionResultPayload{
				ExecutionID: "execution-1", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1",
				Status: DebugExecutionStatusSucceeded, Value: json.RawMessage(`{"ok":true}`),
				Console: []DebugConsoleEntry{{Level: DebugConsoleLog, Arguments: []json.RawMessage{json.RawMessage(`"value"`)}}},
			},
		},
		DebugExecutionResultMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypeDebugExecutionResult, ID: "result-2"},
			Payload: DebugExecutionResultPayload{
				ExecutionID: "execution-2", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1",
				Status: DebugExecutionStatusFailed, Error: &DebugExecutionError{Code: "web_debug_execution_error", Message: "boom"},
			},
		},
		DebugExecutionResultMessage{
			Envelope: Envelope{Version: 1, Type: MessageTypeDebugExecutionResult, ID: "result-3"},
			Payload: DebugExecutionResultPayload{
				ExecutionID: "execution-3", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1",
				Status: DebugExecutionStatusSucceeded, Value: json.RawMessage(`null`),
			},
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
		{
			name: "execute timeout below wire bound",
			wire: `{"version":1,"type":"debug_execute","id":"execute-1","payload":{"execution_id":"execution-1","page_id":"page-1","page_epoch":"epoch-1","session_id":"session-1","code":"1 + 1","timeout_ms":99}}`,
		},
		{
			name: "result status is not an open string",
			wire: `{"version":1,"type":"debug_execution_result","id":"result-1","payload":{"execution_id":"execution-1","page_id":"page-1","page_epoch":"epoch-1","session_id":"session-1","status":"pending"}}`,
		},
		{
			name: "failed result requires typed error",
			wire: `{"version":1,"type":"debug_execution_result","id":"result-1","payload":{"execution_id":"execution-1","page_id":"page-1","page_epoch":"epoch-1","session_id":"session-1","status":"failed"}}`,
		},
		{
			name: "console arguments must be an array",
			wire: `{"version":1,"type":"debug_execution_result","id":"result-1","payload":{"execution_id":"execution-1","page_id":"page-1","page_epoch":"epoch-1","session_id":"session-1","status":"succeeded","console":[{"level":"log"}]}}`,
		},
		{
			name: "result exceeds inline budget",
			wire: `{"version":1,"type":"debug_execution_result","id":"result-1","payload":{"execution_id":"execution-1","page_id":"page-1","page_epoch":"epoch-1","session_id":"session-1","status":"succeeded","value":"` + strings.Repeat("x", DebugExecutionResultMaxBytes) + `"}}`,
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

func TestEncodeRejectsUnboundedDebugExecutionResultShape(t *testing.T) {
	message := DebugExecutionResultMessage{
		Envelope: Envelope{Version: 1, Type: MessageTypeDebugExecutionResult, ID: "result-1"},
		Payload: DebugExecutionResultPayload{
			ExecutionID: "execution-1", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1",
			Status:  DebugExecutionStatusSucceeded,
			Console: []DebugConsoleEntry{{Level: DebugConsoleLog}},
		},
	}
	if _, err := EncodeMessage(message); err == nil {
		t.Fatal("EncodeMessage() error = nil, want console arguments validation error")
	}
}

func TestValidateDebugExecutionResultPayloadUsesPayloadBudgetAndKeepsNull(t *testing.T) {
	payload := DebugExecutionResultPayload{
		ExecutionID: "execution-1", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1",
		Status: DebugExecutionStatusSucceeded, Value: json.RawMessage(`null`),
	}
	if err := ValidateDebugExecutionResultPayload(payload); err != nil {
		t.Fatalf("null payload validation error = %v", err)
	}

	payload.Value = json.RawMessage(`"` + strings.Repeat("x", DebugExecutionResultMaxBytes-512) + `"`)
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) >= DebugExecutionResultMaxBytes {
		t.Fatalf("test payload size = %d, marshal error = %v; want under payload budget", len(encoded), err)
	}
	if err := ValidateDebugExecutionResultPayload(payload); err != nil {
		t.Fatalf("bounded payload validation error = %v", err)
	}

	payload.ExecutionID = strings.Repeat("x", MaxWireIdentifierBytes+1)
	if err := ValidateDebugExecutionResultPayload(payload); err == nil {
		t.Fatal("overlong execution identity accepted by payload validator")
	}
}
