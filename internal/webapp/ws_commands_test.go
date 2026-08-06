package webapp

import (
	"context"
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
			name: "delete", validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.delete") },
			valid:   json.RawMessage(`{"session_id":"s"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":"s","session_id":"other"}`), json.RawMessage(`{"session_id":"s","extra":true}`), json.RawMessage(`{"session_id":"../escape"}`), json.RawMessage(`{"session_id":1}`), json.RawMessage(`{"session_id":"s"} trailing`)},
		},
		{
			name: "compact", validate: func(raw json.RawMessage) error { return validateSessionIDArguments(raw, "session.compact") },
			valid:   json.RawMessage(`{"session_id":"s"}`),
			invalid: []json.RawMessage{json.RawMessage(`{"session_id":""}`), json.RawMessage(`{"session_id":"."}`), json.RawMessage(`{"session_id":"s","unknown":false}`), json.RawMessage(`{"session_id":"s"}{}`)},
		},
		{
			name: "history_read", validate: validateSessionHistoryReadArguments,
			valid: json.RawMessage(`{"session_id":"s","cursor":12,"direction":"before","limit":20,"align_turn":true}`),
			invalid: []json.RawMessage{
				json.RawMessage(`{"session_id":"s","unknown":true}`),
				json.RawMessage(`{"session_id":"s","session_id":"other"}`),
				json.RawMessage(`{"session_id":"s","cursor":1}`),
				json.RawMessage(`{"session_id":"s","direction":"after"}`),
				json.RawMessage(`{"session_id":"s","cursor":0,"direction":"before"}`),
				json.RawMessage(`{"session_id":"s","cursor":1,"direction":"sideways"}`),
				json.RawMessage(`{"session_id":"s","limit":0}`),
				json.RawMessage(`{"session_id":"s","limit":201}`),
				json.RawMessage(`{"session_id":"s","align_turn":"yes"}`),
				json.RawMessage(`{"session_id":"s"} trailing`),
				json.RawMessage(`{"session_id":""}`),
			},
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

func TestProjectCommandSchemasAreStrict(t *testing.T) {
	validCreate := json.RawMessage(`{"operation_id":"operation_project_1","root":"/tmp/project","display_name":"Project"}`)
	if err := validateProjectCreateArguments(validCreate); err != nil {
		t.Fatalf("valid project.create rejected: %v", err)
	}
	invalidCreates := []json.RawMessage{
		json.RawMessage(`{"operation_id":"operation_project_1","root":"/tmp/project","display_name":"Project","unknown":true}`),
		json.RawMessage(`{"operation_id":"operation_project_1","root":"/tmp/project","display_name":"Project","operation_id":"other"}`),
		json.RawMessage(`{"operation_id":"operation_project_1","root":null,"display_name":"Project"}`),
		json.RawMessage(`{"operation_id":"operation_project_1","root":"/tmp/project"}`),
		json.RawMessage(`{"operation_id":"../escape","root":"/tmp/project","display_name":"Project"}`),
		json.RawMessage(`{"operation_id":"operation_project_1","root":"/tmp/project","display_name":"Project"} trailing`),
	}
	for _, raw := range invalidCreates {
		if err := validateProjectCreateArguments(raw); err == nil {
			t.Fatalf("invalid project.create accepted: %s", raw)
		}
	}

	for _, test := range []struct {
		name     string
		validate func(json.RawMessage) error
		valid    json.RawMessage
		invalid  []json.RawMessage
	}{
		{name: "rename", validate: validateProjectRenameArguments, valid: json.RawMessage(`{"project_id":"project_1","display_name":"Renamed"}`), invalid: []json.RawMessage{
			json.RawMessage(`{"project_id":"project_1","display_name":1}`),
			json.RawMessage(`{"project_id":"project_1","display_name":"Renamed","extra":true}`),
		}},
		{name: "archive", validate: func(raw json.RawMessage) error { return validateProjectIDArguments(raw, "project.archive") }, valid: json.RawMessage(`{"project_id":"project_1"}`), invalid: []json.RawMessage{
			json.RawMessage(`{"project_id":null}`), json.RawMessage(`{"project_id":"project_1","extra":false}`),
		}},
		{name: "restore", validate: func(raw json.RawMessage) error { return validateProjectIDArguments(raw, "project.restore") }, valid: json.RawMessage(`{"project_id":"project_1"}`), invalid: []json.RawMessage{
			json.RawMessage(`{"project_id":""}`), json.RawMessage(`{"project_id":"project_1"}{}`),
		}},
		{name: "delete", validate: func(raw json.RawMessage) error { return validateProjectIDArguments(raw, "project.delete") }, valid: json.RawMessage(`{"project_id":"project_1"}`), invalid: []json.RawMessage{
			json.RawMessage(`{"project_id":"project_1","project_id":"other"}`), json.RawMessage(`{"project_id":1}`),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(test.valid); err != nil {
				t.Fatalf("valid arguments rejected: %v", err)
			}
			for _, raw := range test.invalid {
				if err := test.validate(raw); err == nil {
					t.Fatalf("invalid arguments accepted: %s", raw)
				}
			}
		})
	}
}

func TestProjectCommandLifecycleUsesTypedExecutionRules(t *testing.T) {
	service, err := execution.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := newSessionCommandRegistry(service, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	execute := func(name string, arguments any) json.RawMessage {
		t.Helper()
		definition, err := registry.Definition(name, 1)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		result, err := definition.Execute(context.Background(), commands.CommandRequest{Name: name, SchemaVersion: 1, Arguments: raw})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	var created projectCreateResult
	if err := json.Unmarshal(execute("project.create", map[string]string{
		"operation_id": "operation_lifecycle", "root": root, "display_name": "Lifecycle",
	}), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Created || created.ProjectID == "" || created.OperationID != "operation_lifecycle" {
		t.Fatalf("project.create result = %#v", created)
	}

	var renamed projectRenameResult
	if err := json.Unmarshal(execute("project.rename", map[string]string{"project_id": created.ProjectID, "display_name": "Renamed"}), &renamed); err != nil {
		t.Fatal(err)
	}
	if renamed.ProjectID != created.ProjectID || renamed.DisplayName != "Renamed" {
		t.Fatalf("project.rename result = %#v", renamed)
	}
	// Rename to the same value is a no-op domain operation and therefore safe
	// to replay across a server epoch.
	execute("project.rename", map[string]string{"project_id": created.ProjectID, "display_name": "Renamed"})

	session, err := service.CreateSession(created.ProjectID, execution.SessionCreateMetadata{CreatedCWD: root})
	if err != nil {
		t.Fatal(err)
	}
	archive := projectArchiveResult{}
	if err := json.Unmarshal(execute("project.archive", map[string]string{"project_id": created.ProjectID}), &archive); err != nil {
		t.Fatal(err)
	}
	if !archive.Archived || archive.ProjectID != created.ProjectID {
		t.Fatalf("project.archive result = %#v", archive)
	}
	var restored projectArchiveResult
	if err := json.Unmarshal(execute("project.restore", map[string]string{"project_id": created.ProjectID}), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Archived {
		t.Fatalf("project.restore result = %#v", restored)
	}
	// Re-archive is idempotent; the session is idle, so this also verifies the
	// archive/busy rule is delegated to execution rather than the command.
	execute("project.archive", map[string]string{"project_id": created.ProjectID})
	var removed projectDeleteResult
	if err := json.Unmarshal(execute("project.delete", map[string]string{"project_id": created.ProjectID}), &removed); err != nil {
		t.Fatal(err)
	}
	if removed.ProjectID != created.ProjectID || removed.Status != "removed" || removed.RemovedSessions != 1 || session.ID == "" {
		t.Fatalf("project.delete result = %#v", removed)
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
	wantNames := []string{"project.archive", "project.create", "project.delete", "project.rename", "project.restore", "run.cancel", "run.continue", "run.prompt.append", "run.prompt.move", "run.prompt.remove", "run.prompt.steer", "run.start", "run.tool.cancel", "session.archive", "session.compact", "session.create", "session.delete", "session.history.read", "session.mark_read", "session.rename", "session.restore", "session.set_debug", "session.set_full_access"}
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
		if name == "session.history.read" {
			if definition.CachePolicy != commands.ResultCacheVolatile {
				t.Fatalf("%s must retain a volatile-result policy", name)
			}
		} else if definition.CachePolicy != commands.ResultCacheDurable {
			t.Fatalf("%s unexpectedly has a volatile-result policy", name)
		}
		if name == "run.cancel" || name == "run.prompt.move" || name == "run.prompt.remove" || name == "run.prompt.steer" || name == "run.tool.cancel" || name == "session.compact" || name == "session.delete" || name == "project.delete" {
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

func TestSessionCompactCommandErrorsRemainTyped(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "busy", err: execution.ErrSessionBusy, code: "session_busy"},
		{name: "planner cancellation", err: context.Canceled, code: "cancelled"},
		{name: "planner failure", err: execution.ErrTurnFailed, code: "compact_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapped := sessionCompactCommandError(test.err)
			var domain *commands.DomainError
			if !errors.As(mapped, &domain) || domain == nil || domain.Code != test.code {
				t.Fatalf("mapped error=%#v, want code %q", mapped, test.code)
			}
		})
	}
}
