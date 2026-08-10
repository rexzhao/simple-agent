package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

type e7WebCompactRunner struct{}

type e7CancelingWebCompactRunner struct{}

type e7BlockingWebCompactRunner struct {
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
	cancelled atomic.Bool
}

func (e7WebCompactRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (e7WebCompactRunner) RunSessionTurn(context.Context, execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	return execution.SessionTurnResult{Incremental: true}, nil
}

func (e7WebCompactRunner) PlanSessionCompaction(_ context.Context, request execution.SessionCompactionRequest) (execution.SessionCompactionResult, error) {
	return execution.SessionCompactionResult{Compaction: execution.SessionCompactionPlan{
		SummaryItem: sessions.SessionItem{
			ID:         "web-compact-summary",
			Kind:       sessions.ItemKindMessage,
			Visibility: sessions.ItemVisibilityHidden,
			Audience:   sessions.ItemAudienceModel,
			Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "compact summary"},
		},
		Checkpoint: sessions.CompactionCheckpoint{
			ID:                    "web-compact-checkpoint",
			Reason:                "manual",
			Phase:                 "manual",
			Trigger:               "manual",
			SummaryItemID:         "web-compact-summary",
			PreviousActiveHistory: request.Session.ActiveHistory,
			ReplacementHistory:    []string{"web-compact-summary"},
		},
	}}, nil
}

func (e7CancelingWebCompactRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (e7CancelingWebCompactRunner) RunSessionTurn(context.Context, execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	return execution.SessionTurnResult{Incremental: true}, nil
}

func (e7CancelingWebCompactRunner) PlanSessionCompaction(context.Context, execution.SessionCompactionRequest) (execution.SessionCompactionResult, error) {
	return execution.SessionCompactionResult{}, context.Canceled
}

func (runner *e7BlockingWebCompactRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (*e7BlockingWebCompactRunner) RunSessionTurn(context.Context, execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	return execution.SessionTurnResult{Incremental: true}, nil
}

func (runner *e7BlockingWebCompactRunner) PlanSessionCompaction(ctx context.Context, request execution.SessionCompactionRequest) (execution.SessionCompactionResult, error) {
	runner.enterOnce.Do(func() { close(runner.entered) })
	select {
	case <-runner.release:
		return e7WebCompactRunner{}.PlanSessionCompaction(ctx, request)
	case <-ctx.Done():
		runner.cancelled.Store(true)
		return execution.SessionCompactionResult{}, ctx.Err()
	}
}

func openE7CommandConnection(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	connection := dialWebApp(t, serverURL, issueWebSocketTicket(t, serverURL), "http://"+strings.TrimPrefix(serverURL, "http://"))
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		connection.Close(websocket.StatusNormalClosure, "welcome missing")
		t.Fatal("welcome missing")
	}
	return connection
}

func sendE7Command(t *testing.T, connection *websocket.Conn, name, requestID string, arguments json.RawMessage) protocol.CommandResultMessage {
	t.Helper()
	message := protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "e7-" + requestID},
		Payload:  protocol.CommandPayload{Name: name, SchemaVersion: 1, RequestID: requestID, Arguments: arguments},
	}
	payload, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	for {
		value := readWebAppMessage(t, connection)
		result, ok := value.(protocol.CommandResultMessage)
		if ok && result.Payload.RequestID == requestID {
			return result
		}
	}
}

func e7CommandResult(t *testing.T, result protocol.CommandResultMessage) map[string]any {
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

func TestRemainingSessionCommandsUseDomainSemanticsAndGatewayCache(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, e7WebCompactRunner{})
	project, err := service.CreateProject(t.TempDir(), "E7 command project")
	if err != nil {
		t.Fatal(err)
	}
	makeSession := func(name, parentID string) execution.SessionDetail {
		t.Helper()
		detail, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
			DisplayName: name, ParentSessionID: parentID, CreatedCWD: project.Project.Root,
			Provider: "fake", ModelProfile: "default", ModelID: "model",
		})
		if err != nil {
			t.Fatal(err)
		}
		return detail
	}
	connection := openE7CommandConnection(t, server.URL)
	defer connection.Close(websocket.StatusNormalClosure, "done")

	parent := makeSession("delete parent", "")
	child := makeSession("delete child", parent.ID)
	if _, err := service.ArchiveSession(parent.ID); err != nil {
		t.Fatal(err)
	}
	deleteArgs := json.RawMessage(`{"session_id":"` + parent.ID + `"}`)
	firstDelete := e7CommandResult(t, sendE7Command(t, connection, "session.delete", "delete-cascade", deleteArgs))
	if firstDelete["session_id"] != parent.ID || firstDelete["status"] != "removed" || firstDelete["removed_sessions"] != float64(2) {
		t.Fatalf("delete result=%#v", firstDelete)
	}
	secondDelete := e7CommandResult(t, sendE7Command(t, connection, "session.delete", "delete-cascade", deleteArgs))
	if !reflect.DeepEqual(firstDelete, secondDelete) {
		t.Fatalf("cached delete result=%#v, first=%#v", secondDelete, firstDelete)
	}
	for _, id := range []string{parent.ID, child.ID} {
		if _, err := service.GetSession(id); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatalf("deleted session %q lookup error=%v", id, err)
		}
	}

	unarchived := makeSession("archive first", "")
	archiveFirst := sendE7Command(t, connection, "session.delete", "delete-archive-first", json.RawMessage(`{"session_id":"`+unarchived.ID+`"}`))
	if archiveFirst.Payload.Status != protocol.CommandStatusFailed || archiveFirst.Payload.Error == nil || archiveFirst.Payload.Error.Code != "archive_first" {
		t.Fatalf("archive-first error=%#v", archiveFirst)
	}
	busyParent := makeSession("busy parent", "")
	busyChild := makeSession("busy child", busyParent.ID)
	if _, err := service.ArchiveSession(busyParent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SessionStore().MarkTurnRunning(busyChild.ID, "busy-turn"); err != nil {
		t.Fatal(err)
	}
	busyDelete := sendE7Command(t, connection, "session.delete", "delete-busy", json.RawMessage(`{"session_id":"`+busyParent.ID+`"}`))
	if busyDelete.Payload.Status != protocol.CommandStatusFailed || busyDelete.Payload.Error == nil || busyDelete.Payload.Error.Code != "session_busy" {
		t.Fatalf("busy error=%#v", busyDelete)
	}
	if _, err := service.GetSession(busyParent.ID); err != nil {
		t.Fatalf("busy parent was removed: %v", err)
	}

	compactSession := makeSession("compact", "")
	compactArgs := json.RawMessage(`{"session_id":"` + compactSession.ID + `"}`)
	compact := e7CommandResult(t, sendE7Command(t, connection, "session.compact", "compact-success", compactArgs))
	if compact["session_id"] != compactSession.ID || compact["status"] != "committed" || compact["compaction_id"] != "web-compact-checkpoint" || compact["summary_item_id"] != "web-compact-summary" {
		t.Fatalf("compact result=%#v", compact)
	}
	compactCached := e7CommandResult(t, sendE7Command(t, connection, "session.compact", "compact-success", compactArgs))
	if !reflect.DeepEqual(compact, compactCached) {
		t.Fatalf("cached compact result=%#v, first=%#v", compactCached, compact)
	}
}

func TestCompactCommandPlannerCancellationUsesCancelledErrorCode(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, e7CancelingWebCompactRunner{})
	project, err := service.CreateProject(t.TempDir(), "Compact cancellation project")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{CreatedCWD: project.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	connection := openE7CommandConnection(t, server.URL)
	defer connection.Close(websocket.StatusNormalClosure, "done")
	result := sendE7Command(t, connection, "session.compact", "compact-cancel", json.RawMessage(`{"session_id":"`+session.ID+`"}`))
	if result.Payload.Status != protocol.CommandStatusFailed || result.Payload.Error == nil || result.Payload.Error.Code != "cancelled" {
		t.Fatalf("planner cancellation result=%#v", result)
	}
}

func TestCompactCommandOwnerSurvivesWebSocketDisconnectAndBusyIsTyped(t *testing.T) {
	runner := &e7BlockingWebCompactRunner{entered: make(chan struct{}), release: make(chan struct{})}
	server, service, _ := newWebTestAppServerWithRunner(t, runner)
	project, err := service.CreateProject(t.TempDir(), "Compact owner project")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{CreatedCWD: project.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	arguments := json.RawMessage(`{"session_id":"` + session.ID + `"}`)
	first := openE7CommandConnection(t, server.URL)
	sendE7CommandAccepted := func(connection *websocket.Conn, requestID string) {
		t.Helper()
		message := protocol.CommandMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "e7-" + requestID},
			Payload:  protocol.CommandPayload{Name: "session.compact", SchemaVersion: 1, RequestID: requestID, Arguments: arguments},
		}
		payload, err := protocol.EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		if message := readWebAppMessage(t, connection); message.Kind() != protocol.MessageTypeCommandAccepted {
			t.Fatalf("compact accepted=%s", message.Kind())
		}
	}
	sendE7CommandAccepted(first, "compact-owner-request")
	select {
	case <-runner.entered:
	case <-time.After(2 * time.Second):
		first.Close(websocket.StatusNormalClosure, "planner timeout")
		t.Fatal("blocking compact planner did not start")
	}
	_ = first.Close(websocket.StatusNormalClosure, "reconnect")

	second := openE7CommandConnection(t, server.URL)
	defer second.Close(websocket.StatusNormalClosure, "done")
	sendE7CommandAccepted(second, "compact-owner-request")
	close(runner.release)
	result := sendE7CommandResultAfterAccepted(t, second, "compact-owner-request")
	if result.Payload.Status != protocol.CommandStatusSucceeded || result.Payload.Error != nil || runner.cancelled.Load() {
		t.Fatalf("owner-scoped compact result=%#v cancelled=%t", result, runner.cancelled.Load())
	}

	busySession, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{CreatedCWD: project.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := service.SessionStore().AcquireSessionWriteLock(context.Background(), busySession.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	busyResult := sendE7Command(t, second, "session.compact", "compact-busy-request", json.RawMessage(`{"session_id":"`+busySession.ID+`"}`))
	if busyResult.Payload.Status != protocol.CommandStatusFailed || busyResult.Payload.Error == nil || busyResult.Payload.Error.Code != "session_busy" {
		t.Fatalf("busy compact result=%#v", busyResult)
	}
}

func sendE7CommandResultAfterAccepted(t *testing.T, connection *websocket.Conn, requestID string) protocol.CommandResultMessage {
	t.Helper()
	for {
		message := readWebAppMessage(t, connection)
		if result, ok := message.(protocol.CommandResultMessage); ok && result.Payload.RequestID == requestID {
			return result
		}
	}
}
