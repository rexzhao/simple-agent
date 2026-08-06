package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

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
