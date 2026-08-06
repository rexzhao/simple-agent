package webapp

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSessionCommandSchemasAreStrict(t *testing.T) {
	tests := []struct {
		name     string
		validate func(json.RawMessage) error
		valid    json.RawMessage
		invalid  []json.RawMessage
	}{
		{
			name: "mark_read", validate: validateSessionMarkReadArguments,
			valid: json.RawMessage(`{"session_id":"s","run_id":"r","project_id":"p"}`),
			invalid: []json.RawMessage{
				json.RawMessage(`{"session_id":"s","run_id":"r","project_id":null}`),
				json.RawMessage(`{"session_id":"s","run_id":"r","unknown":true}`),
				json.RawMessage(`{"session_id":"s","run_id":"r"} {}`),
				json.RawMessage(`{"session_id":"s","run_id":"r","session_id":"other"}`),
			},
		},
		{
			name: "rename", validate: validateSessionRenameArguments,
			valid: json.RawMessage(`{"session_id":"s","display_name":"new name"}`),
			invalid: []json.RawMessage{
				json.RawMessage(`{"session_id":"s","display_name":"new","unknown":1}`),
				json.RawMessage(`{"session_id":"s","display_name":1}`),
				json.RawMessage(`{"session_id":"","display_name":"new"}`),
				json.RawMessage(`{"session_id":"s","display_name":"new"} {}`),
			},
		},
		{
			name: "archive", validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.archive") },
			valid:   json.RawMessage(`{"session_id":"s"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"s","extra":true}`), json.RawMessage(`{"session_id":null}`), json.RawMessage(`{"session_id":9007199254740993}`)},
		},
		{
			name: "restore", validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.restore") },
			valid:   json.RawMessage(`{"session_id":"s"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"s","extra":true}`), json.RawMessage(`{"session_id":" "}`)},
		},
		{
			name: "full_access", validate: validateSessionFullAccessArguments,
			valid: json.RawMessage(`{"session_id":"s","full_access":false}`),
			invalid: []json.RawMessage{
				json.RawMessage(`{"session_id":"s","full_access":1}`),
				json.RawMessage(`{"session_id":"s","full_access":null}`),
				json.RawMessage(`{"session_id":"s","full_access":true,"extra":false}`),
			},
		},
		{
			name: "debug", validate: validateSessionDebugArguments,
			valid:   json.RawMessage(`{"session_id":"s","request_bodies":true}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"s","request_bodies":9007199254740993}`), json.RawMessage(`{"session_id":"s","request_bodies":null}`), json.RawMessage(`{"session_id":"s","request_bodies":true} trailing`)},
		},
		{
			name: "run_cancel", validate: validateRunCancelArguments,
			valid:   json.RawMessage(`{"run_id":"run"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"run_id":""}`), json.RawMessage(`{"run_id":"run","extra":true}`), json.RawMessage(`{"run_id":"run"}{}`)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(test.valid); err != nil {
				t.Fatalf("valid arguments rejected: %v", err)
			}
			for _, invalid := range test.invalid {
				if err := test.validate(invalid); err == nil {
					t.Fatalf("invalid arguments accepted: %s", invalid)
				}
			}
		})
	}
}

func TestSessionCommandRegistryIsClosedAndFlagsAreExplicit(t *testing.T) {
	registry, err := newSessionCommandRegistry(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"run.cancel", "session.archive", "session.mark_read", "session.rename", "session.restore", "session.set_debug", "session.set_full_access"}
	if got := registry.Names(); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("registry names=%v, want %v", got, wantNames)
	}
	for _, name := range wantNames {
		definition, err := registry.Definition(name, 1)
		if err != nil {
			t.Fatal(err)
		}
		if definition.SupportsExpectedRevision {
			t.Fatalf("%s unexpectedly supports expected_revision", name)
		}
		if name == "run.cancel" {
			if definition.CrossEpochRetrySafe {
				t.Fatalf("run.cancel must remain cross-epoch unsafe")
			}
		} else if !definition.CrossEpochRetrySafe {
			t.Fatalf("%s must be cross-epoch safe", name)
		}
	}
}
