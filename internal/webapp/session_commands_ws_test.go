package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type countingDurableWebRunner struct {
	starts atomic.Int32
}

func (*countingDurableWebRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (runner *countingDurableWebRunner) RunSessionTurn(context.Context, execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	runner.starts.Add(1)
	return execution.SessionTurnResult{Incremental: true}, nil
}

type blockingDurableWebRunner struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	starts  atomic.Int32
}

func (*blockingDurableWebRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (runner *blockingDurableWebRunner) RunSessionTurn(ctx context.Context, _ execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	runner.starts.Add(1)
	runner.once.Do(func() { close(runner.entered) })
	select {
	case <-runner.release:
		return execution.SessionTurnResult{Incremental: true}, nil
	case <-ctx.Done():
		return execution.SessionTurnResult{}, ctx.Err()
	}
}

func TestDurableRunStartReusesIdentityWithoutStartingAgain(t *testing.T) {
	_, service, app := newWebTestAppServerWithRunner(t, webTestRunner{})
	project, err := service.CreateProject(t.TempDir(), "durable run project")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}

	status, err := app.runs.startDurable(first.ID, "hello", "run-durable-stable", "fingerprint-one")
	if err != nil || status != string(execution.SessionRunRunning) {
		t.Fatalf("first durable start status=%q err=%v", status, err)
	}
	managed, ok := app.runs.get("run-durable-stable")
	if !ok {
		t.Fatal("durable run was not registered in the shared replay adapter")
	}
	if _, err := managed.run.Wait(); err != nil {
		t.Fatalf("durable run wait error=%v", err)
	}

	retryStatus, err := app.runs.startDurable(first.ID, "hello", "run-durable-stable", "fingerprint-one")
	if err != nil || retryStatus != string(execution.SessionRunCommitted) {
		t.Fatalf("durable retry status=%q err=%v", retryStatus, err)
	}
	if runs, err := service.SessionStore().ListRuns(first.ID); err != nil || len(runs) != 1 || runs[0].ID != "run-durable-stable" {
		t.Fatalf("durable run records=%#v err=%v", runs, err)
	}
	if _, err := app.runs.startDurable(first.ID, "different", "run-durable-stable", "fingerprint-two"); !errors.Is(err, sessions.ErrIdempotencyConflict) {
		t.Fatalf("different fingerprint error=%v, want idempotency conflict", err)
	}
	if _, err := app.runs.startDurable(second.ID, "hello", "run-durable-stable", "fingerprint-one"); !errors.Is(err, sessions.ErrIdempotencyConflict) {
		t.Fatalf("cross-session identity error=%v, want idempotency conflict", err)
	}
}

func TestDurableRunStartConcurrentSameIdentityExecutesOnce(t *testing.T) {
	runner := &countingDurableWebRunner{}
	_, service, app := newWebTestAppServerWithRunner(t, runner)
	project, err := service.CreateProject(t.TempDir(), "concurrent durable run project")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}

	statuses := make([]string, 2)
	errs := make([]error, 2)
	var group sync.WaitGroup
	for index := range statuses {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			statuses[index], errs[index] = app.runs.startDurable(session.ID, "same input", "run-concurrent-stable", "fingerprint-concurrent")
		}(index)
	}
	group.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent start %d error=%v", index, err)
		}
		if statuses[index] != string(execution.SessionRunRunning) && statuses[index] != string(execution.SessionRunCommitted) {
			t.Fatalf("concurrent start %d status=%q", index, statuses[index])
		}
	}
	managed, ok := app.runs.get("run-concurrent-stable")
	if !ok {
		t.Fatal("concurrent durable run was not registered")
	}
	if _, err := managed.run.Wait(); err != nil {
		t.Fatalf("concurrent durable run wait error=%v", err)
	}
	if got := runner.starts.Load(); got != 1 {
		t.Fatalf("model execution count=%d, want one", got)
	}
}

func TestDurableRunContinueUsesInterruptedTargetAndSharedAdmission(t *testing.T) {
	runner := &blockingDurableWebRunner{entered: make(chan struct{}), release: make(chan struct{})}
	_, service, app := newWebTestAppServerWithRunner(t, runner)
	project, err := service.CreateProject(t.TempDir(), "continue command project")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SessionStore().CreateRun(session.ID, "run-interrupted-web", "", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SessionStore().StartTurn(session.ID, "run-interrupted-web", "turn-interrupted-web", 0, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SessionStore().SetTurnStatus(session.ID, "run-interrupted-web", "turn-interrupted-web", sessions.TurnStatusFailed, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SessionStore().SetRunStatus(session.ID, "run-interrupted-web", sessions.RunStatusFailed, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if state, err := service.SessionStore().LoadState(session.ID); err != nil || state.RunningRunID != "" || state.RunningTurnID != "" || state.InterruptedRunID != "run-interrupted-web" || state.InterruptedTurnID != "turn-interrupted-web" {
		t.Fatalf("seeded interrupted state=%#v err=%v", state, err)
	}

	firstResult := make(chan struct {
		status string
		err    error
	}, 1)
	go func() {
		status, err := app.runs.continueDurable(session.ID, "run-continue-web", "fingerprint-continue-web")
		firstResult <- struct {
			status string
			err    error
		}{status, err}
	}()
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		result := <-firstResult
		managed, ok := app.runs.get("run-continue-web")
		if ok && managed != nil && managed.run != nil {
			_, waitErr := managed.run.Wait()
			t.Fatalf("continue did not reach the model runner: first=%#v status=%s wait=%v", result, managed.run.Status(), waitErr)
		}
		t.Fatalf("continue did not reach the model runner: first=%#v managed=%t", result, ok)
	}

	if status, err := app.runs.continueDurable(session.ID, "run-continue-other", "fingerprint-other"); !errors.Is(err, execution.ErrSessionBusy) || status != "" {
		t.Fatalf("different continue identity status=%q err=%v, want busy before admission", status, err)
	}
	if status, err := app.runs.continueDurable(session.ID, "run-continue-web", "fingerprint-continue-web"); err != nil || status != string(execution.SessionRunRunning) {
		t.Fatalf("same continue identity retry status=%q err=%v, want running", status, err)
	}
	close(runner.release)
	select {
	case result := <-firstResult:
		if result.err != nil || result.status != string(execution.SessionRunRunning) {
			t.Fatalf("first continue result=%#v, want running acknowledgement", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("continue admission did not return")
	}
	managed, ok := app.runs.get("run-continue-web")
	if !ok {
		t.Fatal("continued run was not registered in the shared replay adapter")
	}
	if _, err := managed.run.Wait(); err != nil {
		t.Fatalf("continued run wait error=%v", err)
	}
	if got := runner.starts.Load(); got != 1 {
		t.Fatalf("continue model execution count=%d, want one", got)
	}
	if status, err := app.runs.continueDurable(session.ID, "run-continue-web", "fingerprint-continue-web"); err != nil || status != string(execution.SessionRunCommitted) {
		t.Fatalf("settled continue retry status=%q err=%v", status, err)
	}
	if _, err := app.runs.continueDurable(session.ID, "run-continue-web", "different-fingerprint"); !errors.Is(err, sessions.ErrIdempotencyConflict) {
		t.Fatalf("different continue fingerprint error=%v, want idempotency conflict", err)
	}
	if _, err := app.runs.continueDurable(other.ID, "run-continue-web", "fingerprint-continue-web"); !errors.Is(err, sessions.ErrIdempotencyConflict) {
		t.Fatalf("cross-session continue identity error=%v, want idempotency conflict", err)
	}
	runs, err := service.SessionStore().ListRuns(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.ID == "run-continue-web" && run.PreviousRunID != "run-interrupted-web" {
			t.Fatalf("continued run previous=%q, want interrupted target", run.PreviousRunID)
		}
	}
}

func TestSessionCommandsMutateServiceAndProjectThroughWebSocket(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	project, err := service.CreateProject(t.TempDir(), "Command project")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
		DisplayName: "Before", CreatedCWD: project.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	writeIndexSubscribe(t, connection, project.Project.ID, nil)
	if _, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage); !ok {
		t.Fatal("subscribed missing")
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage); !ok {
		t.Fatal("snapshot missing")
	}

	requestNumber := 0
	send := func(name string, arguments string, expectedChanges int) protocol.CommandResultMessage {
		t.Helper()
		requestNumber++
		message := protocol.CommandMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: fmt.Sprintf("command-%d", requestNumber)},
			Payload:  protocol.CommandPayload{Name: name, SchemaVersion: 1, RequestID: fmt.Sprintf("request-%d", requestNumber), Arguments: json.RawMessage(arguments)},
		}
		payload, err := protocol.EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		var result *protocol.CommandResultMessage
		changes := 0
		for result == nil || changes < expectedChanges {
			message := readWebAppMessage(t, connection)
			switch value := message.(type) {
			case protocol.CommandResultMessage:
				result = &value
			case protocol.ChangeMessage:
				changes++
			}
		}
		return *result
	}
	assertSuccess := func(result protocol.CommandResultMessage) map[string]any {
		t.Helper()
		if result.Payload.Status != protocol.CommandStatusSucceeded || result.Payload.Error != nil {
			t.Fatalf("command result=%#v", result)
		}
		var decoded map[string]any
		if err := json.Unmarshal(result.Payload.Result, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}

	rename := assertSuccess(send("session.rename", `{"session_id":"`+session.ID+`","display_name":"After"}`, 1))
	if rename["session_id"] != session.ID || rename["display_name"] != "After" {
		t.Fatalf("rename result=%#v", rename)
	}
	stored, err := service.SessionStore().LoadState(session.ID)
	if err != nil || stored.DisplayName != "After" {
		t.Fatalf("renamed durable state=%#v err=%v", stored, err)
	}
	// A retry that reaches the service after a new epoch is a logical no-op,
	// not a second metadata mutation.
	assertSuccess(send("session.rename", `{"session_id":"`+session.ID+`","display_name":"After"}`, 0))

	archive := assertSuccess(send("session.archive", `{"session_id":"`+session.ID+`"}`, 1))
	if archive["session_id"] != session.ID || archive["archived"] != true {
		t.Fatalf("archive result=%#v", archive)
	}
	if stored, err = service.SessionStore().LoadState(session.ID); err != nil || !stored.Archived {
		t.Fatalf("archived durable state=%#v err=%v", stored, err)
	}
	assertSuccess(send("session.archive", `{"session_id":"`+session.ID+`"}`, 0))
	failedRename := send("session.rename", `{"session_id":"`+session.ID+`","display_name":"must fail"}`, 0)
	if failedRename.Payload.Status != protocol.CommandStatusFailed || failedRename.Payload.Error == nil || failedRename.Payload.Error.Code != "session_archived" {
		t.Fatalf("archived rename error=%#v", failedRename)
	}

	assertSuccess(send("session.restore", `{"session_id":"`+session.ID+`"}`, 1))
	assertSuccess(send("session.set_full_access", `{"session_id":"`+session.ID+`","full_access":true}`, 1))
	stored, err = service.SessionStore().LoadState(session.ID)
	if err != nil || !stored.FullAccess {
		t.Fatalf("full access durable state=%#v err=%v", stored, err)
	}
	assertSuccess(send("session.set_debug", `{"session_id":"`+session.ID+`","request_bodies":true}`, 1))
	stored, err = service.SessionStore().LoadState(session.ID)
	if err != nil || !stored.Debug.RequestBodies {
		t.Fatalf("debug durable state=%#v err=%v", stored, err)
	}

	missing := send("session.archive", `{"session_id":"missing"}`, 0)
	if missing.Payload.Status != protocol.CommandStatusFailed || missing.Payload.Error == nil || missing.Payload.Error.Code != "not_found" {
		t.Fatalf("missing session error=%#v", missing)
	}

	// The command result above is only an acknowledgement. Every successful
	// mutation was also observed as a provider change while this test was
	// reading the command stream; no local summary was patched by the command.
}

func TestSessionCreateCommandIsDurableAndProjectsOnlyThroughSessionIndex(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	project, err := service.CreateProject(t.TempDir(), "Create command project")
	if err != nil {
		t.Fatal(err)
	}
	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	writeIndexSubscribe(t, connection, project.Project.ID, nil)
	if _, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage); !ok {
		t.Fatal("subscribed missing")
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage); !ok {
		t.Fatal("snapshot missing")
	}

	sessionID := "session-ws-durable-create"
	arguments := json.RawMessage(`{"session_id":"` + sessionID + `","project_id":"` + project.Project.ID + `","display_name":"Created from command"}`)
	queuedChanges := make([]protocol.ChangeMessage, 0, 1)
	send := func(requestID string, args json.RawMessage) protocol.CommandResultMessage {
		t.Helper()
		message := protocol.CommandMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "command-" + requestID},
			Payload:  protocol.CommandPayload{Name: "session.create", SchemaVersion: 1, RequestID: requestID, Arguments: args},
		}
		payload, err := protocol.EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		var result protocol.CommandResultMessage
		for {
			value := readWebAppMessage(t, connection)
			if change, ok := value.(protocol.ChangeMessage); ok {
				queuedChanges = append(queuedChanges, change)
				continue
			}
			if candidate, ok := value.(protocol.CommandResultMessage); ok && candidate.Payload.RequestID == requestID {
				result = candidate
				break
			}
		}
		return result
	}
	first := send("create-request-1", arguments)
	if first.Payload.Status != protocol.CommandStatusSucceeded || first.Payload.Error != nil {
		t.Fatalf("first create result=%#v", first)
	}
	var change protocol.ChangeMessage
	if len(queuedChanges) > 0 {
		change = queuedChanges[0]
	} else {
		change = readIndexChange(t, connection)
	}
	summary := decodeIndexSummary(t, change)
	if summary.SessionID != sessionID || summary.ProjectID != project.Project.ID {
		t.Fatalf("created index summary=%#v", summary)
	}
	stored, err := service.SessionStore().LoadState(sessionID)
	if err != nil || stored.DisplayName != "Created from command" || stored.Provider == "" || stored.ModelProfile == "" || stored.ModelID == "" {
		t.Fatalf("created durable state=%#v err=%v", stored, err)
	}

	// A different request id is a new gateway/cache entry, so this exercises
	// the durable store claim rather than the epoch-local request cache.
	retry := send("create-request-2", arguments)
	if retry.Payload.Status != protocol.CommandStatusSucceeded || retry.Payload.Error != nil {
		t.Fatalf("durable retry result=%#v", retry)
	}
	var result map[string]any
	if err := json.Unmarshal(retry.Payload.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result["session_id"] != sessionID || result["project_id"] != project.Project.ID {
		t.Fatalf("retry result=%#v", result)
	}
	childID := "session-ws-inherited-create"
	child := send("create-request-child", json.RawMessage(`{"session_id":"`+childID+`","project_id":"`+project.Project.ID+`","parent_session_id":"`+sessionID+`","display_name":"Inherited child"}`))
	if child.Payload.Status != protocol.CommandStatusSucceeded || child.Payload.Error != nil {
		t.Fatalf("inherited create result=%#v", child)
	}
	childState, err := service.SessionStore().LoadState(childID)
	if err != nil || childState.ParentSessionID != sessionID || childState.RootSessionID != sessionID || childState.CreatedBy != "agent" {
		t.Fatalf("inherited durable state=%#v err=%v", childState, err)
	}
	conflict := send("create-request-3", json.RawMessage(`{"session_id":"`+sessionID+`","project_id":"`+project.Project.ID+`","display_name":"Different"}`))
	if conflict.Payload.Status != protocol.CommandStatusFailed || conflict.Payload.Error == nil || conflict.Payload.Error.Code != "idempotency_conflict" {
		t.Fatalf("different create parameters result=%#v", conflict)
	}
}

func TestRunCancelCommandCancelsActiveRun(t *testing.T) {
	server, service, app := newWebTestAppServerWithRunner(t, blockingWebTestRunner{})
	project, err := service.CreateProject(t.TempDir(), "Run command project")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
		DisplayName: "Run command", CreatedCWD: project.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := app.runs.coordinator.Start(session.ID, execution.SessionMessageInput{Content: "block"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	active := app.runs.activeRuns()
	if len(active) != 1 || active[0].RunID == "" {
		t.Fatalf("active runs=%#v", active)
	}
	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	arguments, err := json.Marshal(map[string]string{"run_id": active[0].RunID})
	if err != nil {
		t.Fatal(err)
	}
	writeWebAppCommand(t, connection, "run.cancel", arguments)
	var result protocol.CommandResultMessage
	for {
		message := readWebAppMessage(t, connection)
		if value, ok := message.(protocol.CommandResultMessage); ok {
			result = value
			break
		}
	}
	if result.Payload.Status != protocol.CommandStatusSucceeded || result.Payload.Error != nil {
		t.Fatalf("run.cancel result=%#v", result)
	}
	if _, err := run.Wait(); err == nil {
		t.Fatal("cancelled run unexpectedly succeeded")
	}
}
