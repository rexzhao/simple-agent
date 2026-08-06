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

func TestDurablePromptAppendIsAtMostOnceAndOnlyActiveTarget(t *testing.T) {
	runner := &blockingDurableWebRunner{entered: make(chan struct{}), release: make(chan struct{})}
	_, service, app := newWebTestAppServerWithRunner(t, runner)
	project, err := service.CreateProject(t.TempDir(), "prompt append project")
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
	startDone := make(chan error, 1)
	go func() {
		_, startErr := app.runs.startDurable(session.ID, "initial", "run-prompt-append", "prompt-start-fingerprint")
		startDone <- startErr
	}()
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking runner did not reach active turn")
	}
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	// A failure before the durable operation claim is admitted is safe to
	// retry with the same identity. Cancelling the admission lock is a
	// deterministic gate; it must not leave a claim that would turn the later
	// exact retry into an at-most-once tombstone.
	heldAdmissionLock, err := service.SessionStore().AcquirePromptAppendAdmissionLock(context.Background(), "operation-pre-admission")
	if err != nil {
		t.Fatal(err)
	}
	preAdmissionContext, cancelPreAdmission := context.WithCancel(context.Background())
	cancelPreAdmission()
	if _, err := service.AppendPromptDurable(preAdmissionContext, session.ID, "run-prompt-append", "operation-pre-admission", "retry after admission failure"); !errors.Is(err, context.Canceled) {
		_ = heldAdmissionLock.Release()
		t.Fatalf("pre-admission failure=%v, want context cancellation", err)
	}
	if err := heldAdmissionLock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SessionStore().GetPromptAppendClaim("operation-pre-admission"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("pre-admission failure left a durable claim: %v", err)
	}
	if result, err := service.AppendPromptDurable(context.Background(), session.ID, "run-prompt-append", "operation-pre-admission", "retry after admission failure"); err != nil || !result.Accepted {
		t.Fatalf("exact pre-admission retry result=%#v err=%v", result, err)
	}

	results := make([]execution.PromptAppendResult, 2)
	errorsByCall := make([]error, 2)
	var group sync.WaitGroup
	for index := range results {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			results[index], errorsByCall[index] = service.AppendPromptDurable(context.Background(), session.ID, "run-prompt-append", "operation-prompt-append", "  queued exact  ")
		}(index)
	}
	group.Wait()
	for index := range results {
		if errorsByCall[index] != nil || !results[index].Accepted {
			t.Fatalf("append call %d result=%#v err=%v", index, results[index], errorsByCall[index])
		}
	}
	claim, err := service.SessionStore().GetPromptAppendClaim("operation-prompt-append")
	if err != nil || claim.Status != sessions.PromptAppendStatusApplied {
		t.Fatalf("append claim=%#v err=%v, want applied", claim, err)
	}

	// The queue event is produced by the existing coordinator stream, not by
	// the command result. Read that authority directly from the managed replay
	// buffer without using a sleep-based concurrency gate.
	managed, ok := app.runs.get("run-prompt-append")
	if !ok {
		t.Fatal("managed run missing")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		events, _, _, _, changed := managed.snapshot(0)
		found := false
		for _, event := range events {
			var payload struct {
				Type    string `json:"type"`
				Prompts []struct {
					Content string `json:"content"`
				} `json:"prompts"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Type == "run.prompt_queue" {
				for _, prompt := range payload.Prompts {
					if prompt.Content == "  queued exact  " {
						found = true
					}
				}
			}
		}
		if found {
			break
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatal("authoritative prompt queue event was not observed")
		}
	}

	// Reusing the operation ID for different content or a different run is a
	// durable collision, even though the gateway request ID would be new.
	if _, err := service.AppendPromptDurable(context.Background(), session.ID, "run-prompt-append", "operation-prompt-append", "different"); !errors.Is(err, sessions.ErrIdempotencyConflict) {
		t.Fatalf("different content collision=%v, want idempotency conflict", err)
	}
	if _, err := service.AppendPromptDurable(context.Background(), other.ID, "run-prompt-append", "operation-prompt-append", "  queued exact  "); !errors.Is(err, sessions.ErrIdempotencyConflict) {
		t.Fatalf("cross-session operation collision=%v, want idempotency conflict", err)
	}
	if _, err := service.AppendPromptDurable(context.Background(), other.ID, "run-prompt-append", "operation-other-session", "new"); !errors.Is(err, execution.ErrPromptAppendWrongSession) {
		t.Fatalf("wrong-session target error=%v, want typed wrong session", err)
	}
	if _, err := service.AppendPromptDurable(context.Background(), session.ID, "missing-run", "operation-missing", "new"); !errors.Is(err, execution.ErrPromptAppendRunNotFound) {
		t.Fatalf("missing target error=%v, want typed not found", err)
	}

	// A second active run proves the same operation cannot migrate to a new
	// run identity. It is enough to admit it before the shared release gate.
	if _, err := app.runs.startDurable(other.ID, "other initial", "run-prompt-append-other", "prompt-start-other"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AppendPromptDurable(context.Background(), other.ID, "run-prompt-append-other", "operation-prompt-append", "  queued exact  "); !errors.Is(err, sessions.ErrIdempotencyConflict) {
		t.Fatalf("cross-run operation collision=%v, want idempotency conflict", err)
	}
	close(runner.release)
	managedOther, ok := app.runs.get("run-prompt-append-other")
	if !ok {
		t.Fatal("second managed run missing")
	}
	if _, err := managed.run.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := managedOther.run.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := service.AppendPromptDurable(context.Background(), session.ID, "run-prompt-append", "operation-after-settle", "new"); !errors.Is(err, execution.ErrPromptAppendRunSettled) {
		t.Fatalf("settled target error=%v, want typed settled", err)
	}
}

func TestDurablePromptAppendCommandCacheAndNewRequestRetry(t *testing.T) {
	runner := &blockingDurableWebRunner{entered: make(chan struct{}), release: make(chan struct{})}
	server, service, app := newWebTestAppServerWithRunner(t, runner)
	project, err := service.CreateProject(t.TempDir(), "prompt append gateway project")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	startDone := make(chan error, 1)
	go func() {
		_, startErr := app.runs.startDurable(session.ID, "initial", "run-prompt-gateway", "prompt-gateway-start")
		startDone <- startErr
	}()
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking runner did not reach active turn")
	}
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}

	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	arguments := json.RawMessage(`{"session_id":"` + session.ID + `","run_id":"run-prompt-gateway","operation_id":"operation-prompt-gateway","content":"  gateway exact  "}`)
	type commandResponse struct {
		result   protocol.CommandResultMessage
		accepted bool
	}
	send := func(envelopeID, requestID string) commandResponse {
		t.Helper()
		message := protocol.CommandMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: envelopeID},
			Payload:  protocol.CommandPayload{Name: "run.prompt.append", SchemaVersion: 1, RequestID: requestID, Arguments: arguments},
		}
		payload, err := protocol.EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		response := commandResponse{}
		for {
			value := readWebAppMessage(t, connection)
			switch value := value.(type) {
			case protocol.CommandAcceptedMessage:
				response.accepted = true
			case protocol.CommandResultMessage:
				if value.Payload.RequestID == requestID {
					response.result = value
					return response
				}
			}
		}
	}

	first := send("append-command-first", "append-request-stable")
	if first.result.Payload.Status != protocol.CommandStatusSucceeded || first.result.Payload.Error != nil || !first.accepted {
		t.Fatalf("first append command=%#v accepted=%v", first.result, first.accepted)
	}
	var firstResult runPromptAppendResult
	if err := json.Unmarshal(first.result.Payload.Result, &firstResult); err != nil {
		t.Fatal(err)
	}
	if firstResult.OperationID != "operation-prompt-gateway" || firstResult.SessionID != session.ID || firstResult.RunID != "run-prompt-gateway" || !firstResult.Accepted {
		t.Fatalf("first append result=%#v", firstResult)
	}

	// The completed gateway cache returns the exact command result without a
	// second accepted/execution path. A new request ID then reaches the durable
	// operation claim and also returns the same compact acknowledgement.
	cached := send("append-command-cache-retry", "append-request-stable")
	if cached.result.Payload.Status != protocol.CommandStatusSucceeded || cached.result.Payload.Error != nil || cached.accepted {
		t.Fatalf("cached append command=%#v accepted=%v", cached.result, cached.accepted)
	}
	retry := send("append-command-new-request", "append-request-new")
	if retry.result.Payload.Status != protocol.CommandStatusSucceeded || retry.result.Payload.Error != nil || !retry.accepted {
		t.Fatalf("new-request append command=%#v accepted=%v", retry.result, retry.accepted)
	}
	claim, err := service.SessionStore().GetPromptAppendClaim("operation-prompt-gateway")
	if err != nil || claim.Status != sessions.PromptAppendStatusApplied {
		t.Fatalf("gateway append claim=%#v err=%v", claim, err)
	}

	managed, ok := app.runs.get("run-prompt-gateway")
	if !ok {
		t.Fatal("gateway managed run missing")
	}
	events, _, _, _, _ := managed.snapshot(0)
	queueOccurrences := 0
	for _, event := range events {
		var payload struct {
			Type    string `json:"type"`
			Prompts []struct {
				Content string `json:"content"`
			} `json:"prompts"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.Type != "run.prompt_queue" {
			continue
		}
		for _, prompt := range payload.Prompts {
			if prompt.Content == "  gateway exact  " {
				queueOccurrences++
			}
		}
	}
	if queueOccurrences != 1 {
		t.Fatalf("authoritative queue occurrences=%d, want one", queueOccurrences)
	}
	close(runner.release)
	if _, err := managed.run.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestActiveRunControlCommandsUseProcessLocalOwnerAndGatewayCache(t *testing.T) {
	runner := &blockingDurableWebRunner{entered: make(chan struct{}), release: make(chan struct{})}
	server, service, app := newWebTestAppServerWithRunner(t, runner)
	project, err := service.CreateProject(t.TempDir(), "active run control project")
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

	startDone := make(chan error, 1)
	go func() {
		_, startErr := app.runs.startDurable(session.ID, "initial", "run-control", "run-control-start")
		startDone <- startErr
	}()
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking runner did not reach active turn")
	}
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	managed, ok := app.runs.get("run-control")
	if !ok || managed == nil || managed.run == nil {
		t.Fatal("active managed run missing")
	}
	for index := 0; index < 4; index++ {
		if err := managed.run.AppendActive(fmt.Sprintf("queued-%d", index)); err != nil {
			t.Fatalf("AppendActive(%d): %v", index, err)
		}
	}

	released := false
	defer func() {
		if !released {
			close(runner.release)
		}
	}()

	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}

	type commandResponse struct {
		result   protocol.CommandResultMessage
		accepted bool
	}
	send := func(name, envelopeID, requestID string, arguments any) commandResponse {
		t.Helper()
		rawArguments, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		message := protocol.CommandMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: envelopeID},
			Payload:  protocol.CommandPayload{Name: name, SchemaVersion: 1, RequestID: requestID, Arguments: rawArguments},
		}
		payload, err := protocol.EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		response := commandResponse{}
		for {
			value := readWebAppMessage(t, connection)
			switch value := value.(type) {
			case protocol.CommandAcceptedMessage:
				if value.Payload.RequestID == requestID {
					response.accepted = true
				}
			case protocol.CommandResultMessage:
				if value.Payload.RequestID == requestID {
					response.result = value
					return response
				}
			}
		}
	}
	expectSucceeded := func(response commandResponse, wantAccepted bool) {
		t.Helper()
		if response.result.Payload.Status != protocol.CommandStatusSucceeded || response.result.Payload.Error != nil || response.accepted != wantAccepted {
			t.Fatalf("command response=%#v accepted=%v, want succeeded accepted=%t", response.result, response.accepted, wantAccepted)
		}
	}
	expectFailed := func(response commandResponse, code string) {
		t.Helper()
		if response.result.Payload.Status != protocol.CommandStatusFailed || response.result.Payload.Error == nil || response.result.Payload.Error.Code != code || !response.accepted {
			t.Fatalf("command response=%#v accepted=%v, want failed code %q", response.result, response.accepted, code)
		}
	}
	// The Web adapter can retain a run identity during the short coordinator
	// admission window before its SessionRun owner is installed. It must report
	// not_active rather than searching a durable row or claiming the command.
	inactive := newManagedRun("run-not-active", session.ID, app.runs.options)
	app.runs.mu.Lock()
	app.runs.byID[inactive.id] = inactive
	app.runs.mu.Unlock()
	expectFailed(send("run.prompt.remove", "not-active", "not-active-request", map[string]any{
		"session_id": session.ID, "run_id": inactive.id, "prompt_id": "ap-1",
	}), "run_not_active")

	removeArguments := map[string]any{"session_id": session.ID, "run_id": "run-control", "prompt_id": "ap-1"}
	removed := send("run.prompt.remove", "remove-first", "remove-request", removeArguments)
	expectSucceeded(removed, true)
	var removedResult runPromptRemoveResult
	if err := json.Unmarshal(removed.result.Payload.Result, &removedResult); err != nil {
		t.Fatal(err)
	}
	if removedResult.SessionID != session.ID || removedResult.RunID != "run-control" || removedResult.PromptID != "ap-1" || !removedResult.Removed {
		t.Fatalf("remove result=%#v", removedResult)
	}
	removedCached := send("run.prompt.remove", "remove-cache", "remove-request", removeArguments)
	expectSucceeded(removedCached, false)
	if string(removedCached.result.Payload.Result) != string(removed.result.Payload.Result) {
		t.Fatalf("cached remove result=%s, first=%s", removedCached.result.Payload.Result, removed.result.Payload.Result)
	}
	expectFailed(send("run.prompt.remove", "remove-missing", "remove-new-request", removeArguments), "prompt_not_found")

	steerArguments := map[string]any{"session_id": session.ID, "run_id": "run-control", "prompt_id": "ap-2", "steer": true}
	steered := send("run.prompt.steer", "steer-first", "steer-request", steerArguments)
	expectSucceeded(steered, true)
	var steeredResult runPromptSteerResult
	if err := json.Unmarshal(steered.result.Payload.Result, &steeredResult); err != nil {
		t.Fatal(err)
	}
	if steeredResult.SessionID != session.ID || steeredResult.RunID != "run-control" || steeredResult.PromptID != "ap-2" || !steeredResult.Steer {
		t.Fatalf("steer result=%#v", steeredResult)
	}
	expectSucceeded(send("run.prompt.steer", "steer-cache", "steer-request", steerArguments), false)

	moveArguments := map[string]any{"session_id": session.ID, "run_id": "run-control", "prompt_id": "ap-4", "delta": -1}
	moved := send("run.prompt.move", "move-first", "move-request", moveArguments)
	expectSucceeded(moved, true)
	var movedResult runPromptMoveResult
	if err := json.Unmarshal(moved.result.Payload.Result, &movedResult); err != nil {
		t.Fatal(err)
	}
	if movedResult.SessionID != session.ID || movedResult.RunID != "run-control" || movedResult.PromptID != "ap-4" || !movedResult.Moved {
		t.Fatalf("move result=%#v", movedResult)
	}
	// A cached retry returns the first move result. A second execution would
	// have been a different, clamped move after ap-4 reached the front of its
	// plain priority group.
	expectSucceeded(send("run.prompt.move", "move-cache", "move-request", moveArguments), false)

	type queuePrompt struct {
		ID    string `json:"id"`
		Steer bool   `json:"steer"`
	}
	type queueEvent struct {
		Type    string        `json:"type"`
		Prompts []queuePrompt `json:"prompts"`
	}
	waitForQueue := func(wantIDs []string, steerID string) {
		t.Helper()
		want := strings.Join(wantIDs, ",")
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			events, _, _, _, changed := managed.snapshot(0)
			for _, event := range events {
				var payload queueEvent
				if json.Unmarshal(event.Payload, &payload) != nil || payload.Type != "run.prompt_queue" {
					continue
				}
				ids := make([]string, 0, len(payload.Prompts))
				steer := false
				for _, prompt := range payload.Prompts {
					ids = append(ids, prompt.ID)
					if prompt.ID == steerID {
						steer = prompt.Steer
					}
				}
				if strings.Join(ids, ",") == want && (steerID == "" || steer) {
					return
				}
			}
			select {
			case <-changed:
			case <-deadline.C:
				t.Fatalf("authoritative queue snapshot %q was not observed", want)
			}
		}
	}
	// This snapshot is emitted by SessionRun's queue owner; no command result
	// or Web adapter replica is used to synthesize the queue ordering.
	waitForQueue([]string{"ap-2", "ap-4", "ap-3"}, "ap-2")
	noMove := send("run.prompt.move", "move-clamped", "move-clamped-request", map[string]any{
		"session_id": session.ID, "run_id": "run-control", "prompt_id": "ap-3", "delta": 64,
	})
	expectSucceeded(noMove, true)
	var noMoveResult runPromptMoveResult
	if err := json.Unmarshal(noMove.result.Payload.Result, &noMoveResult); err != nil {
		t.Fatal(err)
	}
	if noMoveResult.PromptID != "ap-3" || noMoveResult.Moved {
		t.Fatalf("clamped move result=%#v, want moved=false", noMoveResult)
	}

	expectFailed(send("run.prompt.remove", "wrong-session", "wrong-session-request", map[string]any{
		"session_id": other.ID, "run_id": "run-control", "prompt_id": "ap-2",
	}), "run_wrong_session")
	expectFailed(send("run.prompt.remove", "missing-run", "missing-run-request", map[string]any{
		"session_id": session.ID, "run_id": "run-missing", "prompt_id": "ap-2",
	}), "run_not_found")
	expectFailed(send("run.tool.cancel", "missing-tool", "missing-tool-request", map[string]any{
		"session_id": session.ID, "run_id": "run-control", "tool_call_id": "call-missing",
	}), "tool_call_not_active")

	close(runner.release)
	released = true
	if _, err := managed.run.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	expectFailed(send("run.prompt.remove", "settled-run", "settled-run-request", map[string]any{
		"session_id": session.ID, "run_id": "run-control", "prompt_id": "ap-2",
	}), "run_settled")
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
