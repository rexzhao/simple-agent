package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessionindex"
)

func TestSessionIndexWebSocketTracksBackgroundRunAndMarkRead(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	projectResult, err := service.CreateProject(t.TempDir(), "Project")
	if err != nil {
		t.Fatal(err)
	}
	makeSession := func(name string) execution.SessionDetail {
		t.Helper()
		saveToolResults := true
		detail, createErr := service.CreateSession(projectResult.Project.ID, execution.SessionCreateMetadata{
			DisplayName: name, CreatedCWD: projectResult.Project.Root,
			ConfigPath: service.ConfigPath(), Provider: "fake", ModelProfile: "default", ModelID: "model-default",
			SaveToolResults: &saveToolResults,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return detail
	}
	a := makeSession("A")
	b := makeSession("B")

	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	writeIndexSubscribe(t, connection, projectResult.Project.ID, nil)
	if _, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage); !ok {
		t.Fatal("subscribed missing")
	}
	snapshotMessage, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage)
	if !ok {
		t.Fatal("session index snapshot missing")
	}
	var snapshot sessionindex.SessionIndexSnapshot
	if err := json.Unmarshal(snapshotMessage.Payload.Content.Inline, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sessions) != 2 || snapshot.Sessions[0].SessionID != a.ID || snapshot.Sessions[1].SessionID != b.ID {
		t.Fatalf("snapshot sessions = %#v", snapshot.Sessions)
	}

	run := service.StartSessionRun(context.Background(), b.ID, "background", nil)
	started := readIndexChange(t, connection)
	startedSummary := decodeIndexSummary(t, started)
	if startedSummary.SessionID != b.ID || startedSummary.Status != sessionindex.StatusRunning || startedSummary.RunID == "" || startedSummary.HasUnreadResult {
		t.Fatalf("started B summary = %#v", startedSummary)
	}
	startedSequence := started.Payload.Sequence
	if _, err := run.Wait(); err != nil {
		t.Fatal(err)
	}

	// Reconnect from the running sequence before consuming the old connection's
	// settled frame. The new subscription must replay the complete B summary,
	// without subscribing to B content.
	replayConnection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer replayConnection.Close(websocket.StatusNormalClosure, "replay done")
	writeWebAppHello(t, replayConnection)
	if _, ok := readWebAppMessage(t, replayConnection).(protocol.WelcomeMessage); !ok {
		t.Fatal("replay welcome missing")
	}
	writeIndexSubscribe(t, replayConnection, projectResult.Project.ID, &protocol.ResumeToken{StreamEpoch: started.Payload.StreamEpoch, Sequence: startedSequence})
	if _, ok := readWebAppMessage(t, replayConnection).(protocol.SubscribedMessage); !ok {
		t.Fatal("replay subscribed missing")
	}
	replayed := readIndexChange(t, replayConnection)
	replayedSummary := decodeIndexSummary(t, replayed)
	if replayedSummary.SessionID != b.ID || replayedSummary.Status != sessionindex.StatusCompleted || replayedSummary.RunID != startedSummary.RunID || !replayedSummary.HasUnreadResult {
		t.Fatalf("replayed settled B summary = %#v", replayedSummary)
	}
	settled := readIndexChange(t, connection)
	settledSummary := decodeIndexSummary(t, settled)
	if settledSummary.SessionID != b.ID || settledSummary.HasUnreadResult != replayedSummary.HasUnreadResult {
		t.Fatalf("original settled B summary = %#v", settledSummary)
	}
	if settledSummary.SessionID == a.ID {
		t.Fatal("A was polluted by B run")
	}

	arguments, err := json.Marshal(map[string]string{"session_id": b.ID, "run_id": replayedSummary.RunID})
	if err != nil {
		t.Fatal(err)
	}
	writeWebAppCommand(t, connection, "session.mark_read", arguments)
	var result *protocol.CommandResultMessage
	var markedChange *protocol.ChangeMessage
	for result == nil || markedChange == nil {
		message := readWebAppMessage(t, connection)
		switch value := message.(type) {
		case protocol.CommandResultMessage:
			result = &value
		case protocol.ChangeMessage:
			markedChange = &value
		}
	}
	if result.Payload.Error != nil {
		t.Fatalf("mark_read command error = %#v", result.Payload.Error)
	}
	markedSummary := decodeIndexSummary(t, *markedChange)
	if markedSummary.SessionID != b.ID || markedSummary.RunID != replayedSummary.RunID || markedSummary.HasUnreadResult {
		t.Fatalf("mark_read authoritative summary = %#v", markedSummary)
	}
}

func issueWebSocketTicket(t *testing.T, serverURL string) string {
	t.Helper()
	response := doJSONRequest(t, http.MethodPost, serverURL+"/api/ws-ticket", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ticket status=%d body=%s", response.StatusCode, readBody(response))
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	decodeResponse(t, response, &ticket)
	return ticket.Ticket
}

func writeIndexSubscribe(t *testing.T, connection *websocket.Conn, projectID string, resume *protocol.ResumeToken) {
	t.Helper()
	message := protocol.SubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "index-subscribe"},
		Payload: protocol.SubscribePayload{
			SubscriptionID: "session-index:" + projectID,
			Resource:       protocol.ResourceKey{Type: protocol.ResourceTypeSessionIndex, ID: projectID},
			Resume:         resume,
		},
	}
	payload, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func writeWebAppCommand(t *testing.T, connection *websocket.Conn, name string, arguments json.RawMessage) {
	t.Helper()
	message := protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "command-" + name},
		Payload: protocol.CommandPayload{
			Name: name, SchemaVersion: 1, RequestID: "request-" + name, Arguments: arguments,
		},
	}
	payload, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func readIndexChange(t *testing.T, connection *websocket.Conn) protocol.ChangeMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		message := readWebAppMessage(t, connection)
		if change, ok := message.(protocol.ChangeMessage); ok {
			return change
		}
	}
	t.Fatal("timed out waiting for index change")
	return protocol.ChangeMessage{}
}

func decodeIndexSummary(t *testing.T, change protocol.ChangeMessage) sessionindex.SessionSummary {
	t.Helper()
	if len(change.Payload.Operations) != 1 {
		t.Fatalf("index operation count=%d", len(change.Payload.Operations))
	}
	var operation struct {
		Op    string                      `json:"op"`
		Key   string                      `json:"key"`
		Value sessionindex.SessionSummary `json:"value"`
	}
	if err := json.Unmarshal(change.Payload.Operations[0].Raw, &operation); err != nil {
		t.Fatal(err)
	}
	if operation.Op != sessionindex.OperationUpsert {
		t.Fatalf("index operation=%#v", operation)
	}
	return operation.Value
}
