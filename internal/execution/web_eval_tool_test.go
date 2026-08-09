package execution

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
	"github.com/rexzhao/simple-agent/internal/webdebug"
)

type fakeWebEvalExecutor struct {
	payload       protocol.DebugExecutionResultPayload
	err           error
	waitForCancel bool
	calls         atomic.Int32
}

type gatedWebEvalExecutor struct {
	payload protocol.DebugExecutionResultPayload
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

type unknownWebEvalError struct{}

func (unknownWebEvalError) Error() string       { return "internal secret" }
func (unknownWebEvalError) WebEvalCode() string { return "internal_secret_code" }

func (e *gatedWebEvalExecutor) Execute(context.Context, string, int) (protocol.DebugExecutionResultPayload, error) {
	e.calls.Add(1)
	close(e.started)
	<-e.release
	return e.payload, nil
}

func (f *fakeWebEvalExecutor) Execute(ctx context.Context, _ string, _ int) (protocol.DebugExecutionResultPayload, error) {
	f.calls.Add(1)
	if f.waitForCancel {
		<-ctx.Done()
		return protocol.DebugExecutionResultPayload{}, ctx.Err()
	}
	return f.payload, f.err
}

func newWebEvalTestService(t *testing.T, sessionID, projectID string) (*Service, sessions.SessionV2, *fakeWebEvalExecutor, *WebEvalExecutorRegistration) {
	t.Helper()
	store := sessions.NewV2Store(t.TempDir())
	session, err := store.SaveMetadata(sessions.SessionV2{ID: sessionID, ProjectID: projectID})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{sessionStore: store}
	executor := &fakeWebEvalExecutor{payload: protocol.DebugExecutionResultPayload{
		ExecutionID: "execution-1", PageID: "page-1", PageEpoch: "epoch-1", SessionID: sessionID,
		Status: protocol.DebugExecutionStatusSucceeded, Value: json.RawMessage("null"),
	}}
	registration := service.RegisterWebEvalExecutor(executor)
	if registration == nil {
		t.Fatal("RegisterWebEvalExecutor returned nil")
	}
	return service, session, executor, registration
}

func decodeWebEvalOutput(t *testing.T, result model.ToolResult) map[string]any {
	t.Helper()
	var output map[string]any
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("tool content is not JSON: %v: %s", err, result.Content)
	}
	return output
}

func TestWebEvalAttachmentReplacementIsCASAndDoesNotMigrate(t *testing.T) {
	service := &Service{}
	first := &fakeWebEvalExecutor{}
	second := &fakeWebEvalExecutor{}
	old := service.RegisterWebEvalExecutor(first)
	newRegistration := service.RegisterWebEvalExecutor(second)
	if old == nil || newRegistration == nil || old.Generation() >= newRegistration.Generation() {
		t.Fatalf("registrations = %#v/%#v", old, newRegistration)
	}
	old.Unregister()
	if service.CurrentWebEvalExecutor() != newRegistration || !newRegistration.IsCurrent() {
		t.Fatal("old unregister removed or invalidated replacement")
	}
	if old.IsCurrent() {
		t.Fatal("old registration is still current")
	}
	if _, err := old.Execute(context.Background(), "1", 5000); !errors.Is(err, ErrWebEvalExecutorUnavailable) {
		t.Fatalf("stale Execute() error = %v, want unavailable", err)
	}
	if first.calls.Load() != 0 || second.calls.Load() != 0 {
		t.Fatalf("stale execution migrated to an executor: first=%d second=%d", first.calls.Load(), second.calls.Load())
	}
}

func TestWebEvalRegistrationCurrentCheckLinearizesReplacementWithoutReplay(t *testing.T) {
	service := &Service{}
	oldExecutor := &gatedWebEvalExecutor{
		payload: protocol.DebugExecutionResultPayload{
			ExecutionID: "old-execution", PageID: "old-page", PageEpoch: "old-epoch", SessionID: "old-session",
			Status: protocol.DebugExecutionStatusSucceeded, Value: json.RawMessage(`null`),
		},
		started: make(chan struct{}), release: make(chan struct{}),
	}
	newExecutor := &fakeWebEvalExecutor{}
	oldRegistration := service.RegisterWebEvalExecutor(oldExecutor)
	if oldRegistration == nil {
		t.Fatal("old registration is nil")
	}
	resultCh := make(chan protocol.DebugExecutionResultPayload, 1)
	errorCh := make(chan error, 1)
	go func() {
		payload, err := oldRegistration.Execute(context.Background(), "1", 5000)
		resultCh <- payload
		errorCh <- err
	}()
	<-oldExecutor.started
	newRegistration := service.RegisterWebEvalExecutor(newExecutor)
	if newRegistration == nil {
		t.Fatal("replacement registration is nil")
	}
	// The old current check already linearized the call. Replacement does not
	// cancel or migrate that bound invocation; it can only affect later calls.
	close(oldExecutor.release)
	if err := <-errorCh; err != nil {
		t.Fatalf("old owner execution error = %v", err)
	}
	if payload := <-resultCh; payload.ExecutionID != "old-execution" {
		t.Fatalf("old owner result = %#v, want old execution", payload)
	}
	if oldExecutor.calls.Load() != 1 || newExecutor.calls.Load() != 0 {
		t.Fatalf("replacement migrated/replayed execution: old=%d new=%d", oldExecutor.calls.Load(), newExecutor.calls.Load())
	}
	if _, err := oldRegistration.Execute(context.Background(), "2", 5000); !errors.Is(err, ErrWebEvalExecutorUnavailable) {
		t.Fatalf("call after replacement error = %v, want unavailable", err)
	}
	newRegistration.Unregister()
}

func TestPrepareWebEvalToolUsesTargetAuthorityAndAttachment(t *testing.T) {
	service, target, _, registration := newWebEvalTestService(t, "target", webdebug.TargetProjectID)
	tool := prepareWebEvalTool(target, service.sessionStore, service)
	if tool == nil {
		t.Fatal("target session did not receive a dynamic web.eval executor")
	}
	if tool := prepareWebEvalTool(sessions.SessionV2{ID: target.ID, ProjectID: "other-project"}, service.sessionStore, service); tool != nil {
		t.Fatal("non-target snapshot received web.eval")
	}
	if err := service.sessionStore.Delete(target.ID); err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), map[string]any{"code": "1"})
	if err != nil || !result.IsError || !strings.Contains(result.Content, webdebug.ErrorCodeSessionNotFound) {
		t.Fatalf("deleted authoritative session call = %#v, err=%v", result, err)
	}
	registration.Unregister()
	if tool := prepareWebEvalTool(target, service.sessionStore, service); tool != nil {
		t.Fatal("unregistered executor remained visible")
	}

	otherStore := sessions.NewV2Store(t.TempDir())
	other, err := otherStore.SaveMetadata(sessions.SessionV2{ID: "other", ProjectID: "other-project"})
	if err != nil {
		t.Fatal(err)
	}
	if tool := prepareWebEvalTool(other, otherStore, service); tool != nil {
		t.Fatal("other-project session received web.eval")
	}
}

func TestWebEvalSchemaIsStrictAndDoesNotEnableSessionTools(t *testing.T) {
	registry, schemas, err := enabledToolsForRun(t.TempDir(), []string{WebEvalToolName}, false)
	if err != nil || registry != nil || len(schemas) != 0 {
		t.Fatalf("configured web.eval was treated as a built-in: registry=%p schemas=%#v err=%v", registry, schemas, err)
	}
	schema := WebEvalToolSchema()
	if schema.Name != WebEvalToolName || schema.InputSchema["additionalProperties"] != false {
		t.Fatalf("schema = %#v", schema)
	}
	properties := schema.InputSchema["properties"].(map[string]any)
	codeSchema := properties["code"].(map[string]any)
	if codeSchema["maxLength"] != protocol.DebugExecutionCodeMaxBytes || !strings.Contains(codeSchema["description"].(string), "UTF-8 bytes") {
		t.Fatal("code schema does not carry the byte budget")
	}
	timeoutSchema := properties["timeout_ms"].(map[string]any)
	if timeoutSchema["default"] != webEvalDefaultTimeout || !strings.Contains(timeoutSchema["description"].(string), "100 through 30000") {
		t.Fatalf("timeout schema = %#v", timeoutSchema)
	}
	if !strings.Contains(schema.Description, "same-origin") || !strings.Contains(schema.Description, "never retries") {
		t.Fatalf("schema description omits safety contract: %q", schema.Description)
	}

	valid := []map[string]any{
		{"code": "1"},
		{"code": "await 1", "timeout_ms": json.Number("100")},
		{"code": "x", "timeout_ms": int64(30000)},
		{"code": "x", "timeout_ms": 5000.0},
	}
	for _, arguments := range valid {
		if _, _, err := validateWebEvalArguments(arguments); err != nil {
			t.Errorf("valid arguments %#v rejected: %v", arguments, err)
		}
	}
	invalid := []map[string]any{
		nil,
		{},
		{"code": ""},
		{"code": "\xff"},
		{"code": strings.Repeat("x", protocol.DebugExecutionCodeMaxBytes+1)},
		{"code": strings.Repeat("界", protocol.DebugExecutionCodeMaxBytes/2+1)}, // fewer than 64 KiB runes, more than 64 KiB UTF-8 bytes
		{"code": "x", "timeout_ms": "5000"},
		{"code": "x", "timeout_ms": json.Number("5000.5")},
		{"code": "x", "timeout_ms": json.Number("NaN")},
		{"code": "x", "timeout_ms": float64(5000.5)},
		{"code": "x", "timeout_ms": math.NaN()},
		{"code": "x", "timeout_ms": math.Inf(1)},
		{"code": "x", "timeout_ms": 99},
		{"code": "x", "timeout_ms": 30001},
		{"code": "x", "unknown": true},
	}
	for _, arguments := range invalid {
		if _, _, err := validateWebEvalArguments(arguments); err == nil {
			t.Errorf("invalid arguments accepted: %#v", arguments)
		}
	}
}

func TestAssembleAgentToolSelectionOwnsDynamicWebEvalRuntimePath(t *testing.T) {
	store := sessions.NewV2Store(t.TempDir())
	service := &Service{sessionStore: store}
	executor := &fakeWebEvalExecutor{payload: protocol.DebugExecutionResultPayload{
		ExecutionID: "execution-runtime", PageID: "page-runtime", PageEpoch: "epoch-runtime", SessionID: "runtime-session",
		Status: protocol.DebugExecutionStatusSucceeded, Value: json.RawMessage(`null`),
	}}
	registration := service.RegisterWebEvalExecutor(executor)
	if registration == nil {
		t.Fatal("RegisterWebEvalExecutor returned nil")
	}
	session, err := store.SaveMetadata(sessions.SessionV2{ID: "runtime-session", ProjectID: webdebug.TargetProjectID, EnabledTools: []string{WebEvalToolName}})
	if err != nil {
		t.Fatal(err)
	}
	names, _, schemas, webEval, err := assembleAgentToolSelection(t.TempDir(), session, store, service)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 || webEval == nil {
		t.Fatalf("runtime enabled names/executor = %#v/%p, want no configured web.eval and a dynamic executor", names, webEval)
	}
	count := 0
	for _, schema := range schemas {
		if schema.Name == WebEvalToolName {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("runtime web.eval schema count = %d, schemas = %#v", count, schemas)
	}
	if result, err := webEval.Execute(context.Background(), map[string]any{"code": "null"}); err != nil || result.IsError {
		t.Fatalf("runtime dynamic executor result = %#v, err=%v", result, err)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("runtime dynamic executor calls = %d, want 1", executor.calls.Load())
	}

	runtime := &agentRunnerRuntime{sessionStore: store, session: session, enabledTools: names}
	if err := runtime.saveRuntimeMetadataForSession(session.ID); err != nil {
		t.Fatal(err)
	}
	saved, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if containsWebEvalString(saved.EnabledTools, WebEvalToolName) || !containsWebEvalString(session.EnabledTools, WebEvalToolName) {
		t.Fatalf("enabled tools persisted/input = %#v/%#v", saved.EnabledTools, session.EnabledTools)
	}

	child := session
	child.ID = "runtime-child"
	child.ParentSessionID = session.ID
	child.EnabledTools = []string{WebEvalToolName}
	child, err = store.SaveMetadata(child)
	if err != nil {
		t.Fatal(err)
	}
	childNames, _, childSchemas, childExecutor, err := assembleAgentToolSelection(t.TempDir(), child, store, service)
	if err != nil {
		t.Fatal(err)
	}
	if containsWebEvalString(childNames, WebEvalToolName) || childExecutor == nil || countToolSchemas(childSchemas, WebEvalToolName) != 1 {
		t.Fatalf("child dynamic assembly = names %#v schemas %#v executor %p", childNames, childSchemas, childExecutor)
	}
	changed := session
	changed.ID = "runtime-changed"
	changed, err = store.SaveMetadata(changed)
	if err != nil {
		t.Fatal(err)
	}
	changed.ProjectID = "project-other"
	if _, err := store.SaveMetadata(changed); err != nil {
		t.Fatal(err)
	}
	if _, _, schemas, executor, err := assembleAgentToolSelection(t.TempDir(), sessionWithID(session, "runtime-changed"), store, service); err != nil || executor != nil || countToolSchemas(schemas, WebEvalToolName) != 0 {
		t.Fatalf("assembly after project change = schemas %#v executor %p err=%v", schemas, executor, err)
	}

	registration.Unregister()
	if _, _, schemas, executor, err := assembleAgentToolSelection(t.TempDir(), session, store, service); err != nil || executor != nil || countToolSchemas(schemas, WebEvalToolName) != 0 {
		t.Fatalf("assembly without registration = schemas %#v executor %p err=%v", schemas, executor, err)
	}
	if err := store.Delete(child.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, schemas, executor, err := assembleAgentToolSelection(t.TempDir(), child, store, service); err != nil || executor != nil || countToolSchemas(schemas, WebEvalToolName) != 0 {
		t.Fatalf("assembly after child deletion = schemas %#v executor %p err=%v", schemas, executor, err)
	}
}

func countToolSchemas(schemas []model.Tool, name string) int {
	count := 0
	for _, schema := range schemas {
		if schema.Name == name {
			count++
		}
	}
	return count
}

func containsWebEvalString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sessionWithID(session sessions.SessionV2, id string) sessions.SessionV2 {
	session.ID = id
	return session
}

func TestWebEvalStructuredResultsPreserveNullConsoleAndIdentity(t *testing.T) {
	service, session, executor, registration := newWebEvalTestService(t, "result-session", webdebug.TargetProjectID)
	tool := prepareWebEvalTool(session, service.sessionStore, service)
	result, err := tool.Execute(context.Background(), map[string]any{"code": "null"})
	if err != nil || result.IsError {
		t.Fatalf("success result = %#v, err=%v", result, err)
	}
	output := decodeWebEvalOutput(t, result)
	if output["status"] != "succeeded" || output["execution_id"] != "execution-1" || output["page_id"] != "page-1" || output["page_epoch"] != "epoch-1" || output["session_id"] != session.ID {
		t.Fatalf("success identity = %#v", output)
	}
	if value, ok := output["value"]; !ok || value != nil {
		t.Fatalf("success value = %#v, want explicit null", output["value"])
	}

	executor.payload.Console = []protocol.DebugConsoleEntry{{Level: protocol.DebugConsoleLog, Arguments: []json.RawMessage{json.RawMessage(`"hello"`)}}}
	result, err = tool.Execute(context.Background(), map[string]any{"code": "console.log('hello')", "timeout_ms": 100})
	if err != nil || result.IsError || !strings.Contains(result.Content, `"console"`) || !strings.Contains(result.Content, `"hello"`) {
		t.Fatalf("console result = %#v, err=%v", result, err)
	}

	executor.payload.Status = protocol.DebugExecutionStatusFailed
	executor.payload.Value = nil
	executor.payload.Error = &protocol.DebugExecutionError{Code: "browser_exception", Message: "ReferenceError"}
	failedBrowser, err := tool.Execute(context.Background(), map[string]any{"code": "throw new Error()"})
	if err != nil || !failedBrowser.IsError {
		t.Fatalf("browser failure = %#v, err=%v", failedBrowser, err)
	}
	browserFailure := decodeWebEvalOutput(t, failedBrowser)
	if browserFailure["execution_id"] != "execution-1" || browserFailure["error"].(map[string]any)["code"] != "browser_exception" {
		t.Fatalf("browser failure identity/error = %#v", browserFailure)
	}

	registration.Unregister()
	failed, err := tool.Execute(context.Background(), map[string]any{"code": "1"})
	if err != nil || !failed.IsError {
		t.Fatalf("stale result = %#v, err=%v", failed, err)
	}
	failure := decodeWebEvalOutput(t, failed)
	if failure["status"] != "failed" || failure["error"].(map[string]any)["code"] != webdebug.ErrorCodeNotConnected {
		t.Fatalf("stale failure = %#v", failure)
	}
}

func TestWebEvalMalformedExecutorPayloadFailsClosed(t *testing.T) {
	base := protocol.DebugExecutionResultPayload{
		ExecutionID: "execution-1", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1",
		Status: protocol.DebugExecutionStatusSucceeded, Value: json.RawMessage(`null`),
	}
	tests := []struct {
		name   string
		mutate func(*protocol.DebugExecutionResultPayload)
		leak   string
	}{
		{name: "missing value", mutate: func(payload *protocol.DebugExecutionResultPayload) { payload.Value = nil }},
		{name: "unknown status", mutate: func(payload *protocol.DebugExecutionResultPayload) { payload.Status = "partial" }},
		{name: "success with error", mutate: func(payload *protocol.DebugExecutionResultPayload) {
			payload.Error = &protocol.DebugExecutionError{Code: "secret", Message: "secret"}
		}, leak: "secret"},
		{name: "failed without error", mutate: func(payload *protocol.DebugExecutionResultPayload) {
			payload.Status = protocol.DebugExecutionStatusFailed
			payload.Value = nil
		}},
		{name: "failed with value", mutate: func(payload *protocol.DebugExecutionResultPayload) {
			payload.Status = protocol.DebugExecutionStatusFailed
			payload.Error = &protocol.DebugExecutionError{Code: "failed", Message: "failed"}
		}},
		{name: "missing identity", mutate: func(payload *protocol.DebugExecutionResultPayload) { payload.SessionID = "" }},
		{name: "blank identity", mutate: func(payload *protocol.DebugExecutionResultPayload) { payload.PageID = "   " }},
		{name: "identity whitespace", mutate: func(payload *protocol.DebugExecutionResultPayload) { payload.PageEpoch = " epoch-1" }},
		{name: "execution identity whitespace", mutate: func(payload *protocol.DebugExecutionResultPayload) { payload.ExecutionID = " execution-1" }},
		{name: "identity too long", mutate: func(payload *protocol.DebugExecutionResultPayload) {
			payload.ExecutionID = strings.Repeat("x", protocol.MaxWireIdentifierBytes+1)
		}},
		{name: "invalid console raw", mutate: func(payload *protocol.DebugExecutionResultPayload) {
			payload.Console = []protocol.DebugConsoleEntry{{Level: protocol.DebugConsoleLog, Arguments: []json.RawMessage{json.RawMessage(`{`)}}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := base
			test.mutate(&payload)
			result := webEvalBrowserResult(time.Now(), payload)
			if !result.IsError {
				t.Fatalf("malformed payload produced success: %#v", result)
			}
			output := decodeWebEvalOutput(t, result)
			if output["status"] != "failed" || output["error"].(map[string]any)["code"] != "web_debug_internal_error" {
				t.Fatalf("malformed payload output = %#v", output)
			}
			if test.leak != "" && strings.Contains(result.Content, test.leak) {
				t.Fatalf("malformed payload leaked executor data: %s", result.Content)
			}
		})
	}
}

func TestWebEvalBrokerTypedFailuresAndContextCancellation(t *testing.T) {
	service, session, _, registration := newWebEvalTestService(t, "typed-session", webdebug.TargetProjectID)
	defer registration.Unregister()
	typed := &fakeWebEvalExecutor{err: webdebug.ErrExecutionBusy}
	registration.executor = typed
	tool := prepareWebEvalTool(session, service.sessionStore, service)
	result, err := tool.Execute(context.Background(), map[string]any{"code": "1"})
	if err != nil || !result.IsError {
		t.Fatalf("typed failure = %#v, err=%v", result, err)
	}
	output := decodeWebEvalOutput(t, result)
	if !reflect.DeepEqual(output, map[string]any{
		"status": output["status"], "elapsed_ms": output["elapsed_ms"],
		"error": map[string]any{"code": webdebug.ErrorCodeExecutionBusy, "message": "another web evaluation is already running"},
	}) {
		t.Fatalf("typed failure leaked or changed shape: %#v", output)
	}

	cancelExecutor := &fakeWebEvalExecutor{waitForCancel: true}
	registration.executor = cancelExecutor
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, callErr := tool.Execute(ctx, map[string]any{"code": "1"})
		resultCh <- callErr
	}()
	for cancelExecutor.calls.Load() == 0 {
		// The call has to pass the authoritative session and registration
		// checks before cancellation exercises the broker context path.
		runtime.Gosched()
	}
	cancel()
	if err := <-resultCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled tool error = %v, want context.Canceled", err)
	}
}

func TestWebEvalUnknownCodedFailureIsGeneric(t *testing.T) {
	service, session, _, registration := newWebEvalTestService(t, "unknown-error-session", webdebug.TargetProjectID)
	defer registration.Unregister()
	registration.executor = &fakeWebEvalExecutor{err: unknownWebEvalError{}}
	tool := prepareWebEvalTool(session, service.sessionStore, service)
	result, err := tool.Execute(context.Background(), map[string]any{"code": "1"})
	if err != nil || !result.IsError {
		t.Fatalf("unknown coded failure = %#v, err=%v", result, err)
	}
	output := decodeWebEvalOutput(t, result)
	if output["error"].(map[string]any)["code"] != "web_debug_internal_error" || strings.Contains(result.Content, "internal_secret_code") || strings.Contains(result.Content, "internal secret") {
		t.Fatalf("unknown coded failure leaked details: %#v", output)
	}
}
