package webapp

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/commands"
	"github.com/rexzhao/simple-agent/internal/execution"
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
			name: "create", validate: validateSessionCreateArguments,
			valid: json.RawMessage(`{"session_id":"session_s","project_id":"project_p","display_name":"new","full_access":false}`),
			invalid: []json.RawMessage{
				json.RawMessage(`{"session_id":"session_s","project_id":"project_p","unknown":true}`),
				json.RawMessage(`{"session_id":"session_s","project_id":"project_p","session_id":"other"}`),
				json.RawMessage(`{"session_id":"../escape","project_id":"project_p"}`),
				json.RawMessage(`{"session_id":"session_s","project_id":"project_p"} trailing`),
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
		{
			name: "run_start", validate: validateRunStartArguments,
			valid:   json.RawMessage(`{"session_id":"session","run_id":"run","content":"hello"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"session","run_id":"run","content":""}`), json.RawMessage(`{"session_id":"session","run_id":"run","content":"hello","images":[]}`), json.RawMessage(`{"session_id":"session","run_id":"run","content":"hello"} trailing`), json.RawMessage(`{"session_id":"session","run_id":"run","content":"hello","content":"again"}`), json.RawMessage(`{"session_id":"session","run_id":"run","content":"` + strings.Repeat("x", maxRunStartContentBytes+1) + `"}`)},
		},
		{
			name: "run_continue", validate: validateRunContinueArguments,
			valid:   json.RawMessage(`{"session_id":"session","run_id":"run"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"session","run_id":"run","content":"new"}`), json.RawMessage(`{"session_id":"session","run_id":"run","blob":null}`), json.RawMessage(`{"session_id":"session","run_id":"run"} trailing`), json.RawMessage(`{"session_id":"session","run_id":"run","run_id":"other"}`), json.RawMessage(`{"session_id":"session","run_id":1}`), json.RawMessage(`{"session_id":"session","run_id":"run","content":null}`)},
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

func TestRunStartPreservesExactContentAndUsesUTF8ByteLimit(t *testing.T) {
	exactContent := "  hello\n"
	raw, err := json.Marshal(map[string]string{
		"session_id": "session-content-boundary",
		"run_id":     "run-content-boundary",
		"content":    exactContent,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := decodeRunStartArguments(raw)
	if err != nil {
		t.Fatalf("decodeRunStartArguments() error = %v", err)
	}
	if arguments.Content != exactContent {
		t.Fatalf("decoded content = %q, want exact wire value %q", arguments.Content, exactContent)
	}
	roundTrip, err := json.Marshal(arguments.Content)
	if err != nil || string(roundTrip) != `"  hello\n"` {
		t.Fatalf("content round trip = %s/%v, want preserved whitespace", roundTrip, err)
	}

	request := commands.CommandRequest{Name: "run.start", SchemaVersion: 1, Arguments: raw}
	fingerprint, err := runStartFingerprint(request, arguments)
	if err != nil {
		t.Fatalf("runStartFingerprint() error = %v", err)
	}
	trimmedRaw, err := json.Marshal(map[string]string{
		"session_id": "session-content-boundary",
		"run_id":     "run-content-boundary",
		"content":    "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	trimmedArguments, err := decodeRunStartArguments(trimmedRaw)
	if err != nil {
		t.Fatalf("trimmed decode error = %v", err)
	}
	trimmedFingerprint, err := runStartFingerprint(request, trimmedArguments)
	if err != nil {
		t.Fatalf("trimmed runStartFingerprint() error = %v", err)
	}
	if fingerprint == trimmedFingerprint {
		t.Fatal("content whitespace was lost from the durable fingerprint")
	}

	whitespaceOnly, err := json.Marshal(map[string]string{
		"session_id": "session-content-boundary",
		"run_id":     "run-content-whitespace",
		"content":    " \n\t\u2003",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRunStartArguments(whitespaceOnly); err == nil {
		t.Fatal("pure whitespace content was accepted")
	}

	unit := "界"
	exactBytes := strings.Repeat(unit, maxRunStartContentBytes/len(unit)) + "x"
	if len(exactBytes) != maxRunStartContentBytes {
		t.Fatalf("test exact-boundary content bytes = %d, want %d", len(exactBytes), maxRunStartContentBytes)
	}
	exactBoundary, err := json.Marshal(map[string]string{
		"session_id": "session-content-boundary",
		"run_id":     "run-content-exact-bytes",
		"content":    exactBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := decodeRunStartArguments(exactBoundary); err != nil || len(parsed.Content) != maxRunStartContentBytes {
		t.Fatalf("exact UTF-8 byte boundary decode = %d/%v, want accepted %d bytes", len(parsed.Content), err, maxRunStartContentBytes)
	}
	overBoundary, err := json.Marshal(map[string]string{
		"session_id": "session-content-boundary",
		"run_id":     "run-content-over-bytes",
		"content":    exactBytes + "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRunStartArguments(overBoundary); err == nil {
		t.Fatal("content over the UTF-8 byte boundary was accepted")
	}
}

func TestRunContinueFingerprintNormalizesOnlyWireOperationAndSeparatesRunStart(t *testing.T) {
	request := commands.CommandRequest{Name: "run.continue", SchemaVersion: 1, Arguments: json.RawMessage(`{"session_id":"session","run_id":"run-a"}`)}
	first, err := runContinueFingerprint(request, runContinueArguments{SessionID: "session", RunID: "run-a"})
	if err != nil {
		t.Fatalf("runContinueFingerprint() error = %v", err)
	}
	second, err := runContinueFingerprint(request, runContinueArguments{SessionID: "session", RunID: "run-b"})
	if err != nil {
		t.Fatalf("runContinueFingerprint(second) error = %v", err)
	}
	if first != second {
		t.Fatalf("continue fingerprint depends on stable run identity: %q != %q", first, second)
	}
	start, err := runStartFingerprint(commands.CommandRequest{Name: "run.start", SchemaVersion: 1}, runStartArguments{SessionID: "session", RunID: "run-a", Content: ""})
	if err != nil {
		t.Fatalf("runStartFingerprint() error = %v", err)
	}
	if first == start {
		t.Fatal("run.start and run.continue share an idempotency fingerprint")
	}
}

func TestRunPromptAppendArgumentsAreStrictAndPreserveContentBytes(t *testing.T) {
	content := "\n  keep both edges  \t"
	arguments, err := decodeRunPromptAppendArguments(json.RawMessage(`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"\n  keep both edges  \t"}`))
	if err != nil {
		t.Fatalf("valid append arguments rejected: %v", err)
	}
	if arguments.Content != content {
		t.Fatalf("content=%q, want exact whitespace=%q", arguments.Content, content)
	}
	invalid := []string{
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"ok","extra":true}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","operation_id":"other","content":"ok"}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"ok"} {}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":[]}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"   \t"}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"ok","images":[]}`,
		`{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"ok","blob":"not-supported"}`,
	}
	for _, raw := range invalid {
		if _, err := decodeRunPromptAppendArguments(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid append arguments accepted: %s", raw)
		}
	}
	tooLarge := `{"session_id":"session-append","run_id":"run-append","operation_id":"operation-append","content":"` + strings.Repeat("x", maxRunPromptAppendContentBytes+1) + `"}`
	if _, err := decodeRunPromptAppendArguments(json.RawMessage(tooLarge)); err == nil {
		t.Fatal("append content over byte bound was accepted")
	}
}

func TestActiveRunControlCommandSchemasAreStrict(t *testing.T) {
	tests := []struct {
		name     string
		validate func(json.RawMessage) error
		valid    string
		invalid  []string
	}{
		{
			name: "remove", validate: validateRunPromptRemoveArguments,
			valid: `{"session_id":"session","run_id":"run","prompt_id":"ap-1"}`,
			invalid: []string{
				`{"session_id":"session","run_id":"run"}`,
				`{"session_id":"session","run_id":"run","prompt_id":""}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","unknown":true}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","prompt_id":"ap-2"}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1"} trailing`,
				`{"session_id":"session","run_id":1,"prompt_id":"ap-1"}`,
			},
		},
		{
			name: "steer", validate: validateRunPromptSteerArguments,
			valid: `{"session_id":"session","run_id":"run","prompt_id":"ap-1","steer":false}`,
			invalid: []string{
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1"}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","steer":1}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","steer":false,"extra":null}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","prompt_id":"ap-2","steer":true}`,
			},
		},
		{
			name: "move", validate: validateRunPromptMoveArguments,
			valid: `{"session_id":"session","run_id":"run","prompt_id":"ap-1","delta":-1}`,
			invalid: []string{
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","delta":1.5}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","delta":65}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","delta":null}`,
				`{"session_id":"session","run_id":"run","prompt_id":"ap-1","delta":0,"direction":"up"}`,
			},
		},
		{
			name: "tool_cancel", validate: validateRunToolCancelArguments,
			valid: `{"session_id":"session","run_id":"run","tool_call_id":"call-1"}`,
			invalid: []string{
				`{"session_id":"session","run_id":"run","tool_call_id":""}`,
				`{"session_id":"session","run_id":"run","tool_call_id":false}`,
				`{"session_id":"session","run_id":"run","tool_call_id":"call-1","tool_call_id":"call-2"}`,
				`{"session_id":"session","run_id":"run","tool_call_id":"call-1"}{}`,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(json.RawMessage(test.valid)); err != nil {
				t.Fatalf("valid arguments rejected: %v", err)
			}
			for _, raw := range test.invalid {
				if err := test.validate(json.RawMessage(raw)); err == nil {
					t.Fatalf("invalid arguments accepted: %s", raw)
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
	wantNames := []string{"run.cancel", "run.continue", "run.prompt.append", "run.prompt.move", "run.prompt.remove", "run.prompt.steer", "run.start", "run.tool.cancel", "session.archive", "session.create", "session.mark_read", "session.rename", "session.restore", "session.set_debug", "session.set_full_access"}
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
		if name == "run.cancel" || name == "run.prompt.move" || name == "run.prompt.remove" || name == "run.prompt.steer" || name == "run.tool.cancel" {
			if definition.CrossEpochRetrySafe {
				t.Fatalf("%s must remain cross-epoch unsafe", name)
			}
		} else if !definition.CrossEpochRetrySafe {
			t.Fatalf("%s must be cross-epoch safe", name)
		}
	}
}

func TestPromptAppendOutcomeErrorsRemainTypedFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
		want string
	}{
		{name: "not applied", err: execution.ErrPromptAppendNotApplied, code: "operation_not_applied", want: "was not applied"},
		{name: "outcome unknown", err: execution.ErrPromptAppendOutcomeUnknown, code: "operation_outcome_unknown", want: "may already have been applied"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapped := sessionCommandError(test.err)
			var domain *commands.DomainError
			if !errors.As(mapped, &domain) || domain == nil || domain.Code != test.code || !strings.Contains(domain.Message, test.want) {
				t.Fatalf("mapped error=%#v, want %s containing %q", mapped, test.code, test.want)
			}
		})
	}
}
