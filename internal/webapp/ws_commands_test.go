package webapp

import (
	"encoding/json"
	"testing"
)

func TestSessionMarkReadCommandSchemaIsStrict(t *testing.T) {
	valid := json.RawMessage(`{"session_id":"s","run_id":"r"}`)
	if err := validateSessionMarkReadArguments(valid); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"session_id":"","run_id":"r"}`),
		json.RawMessage(`{"session_id":"s","run_id":"r","unknown":true}`),
		json.RawMessage(`{"session_id":"s","run_id":"r"} {}`),
		json.RawMessage(`{"session_id":"s","run_id":"r","project_id":""}`),
	} {
		if err := validateSessionMarkReadArguments(invalid); err == nil {
			t.Fatalf("invalid command arguments accepted: %s", invalid)
		}
	}
}
