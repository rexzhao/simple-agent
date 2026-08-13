package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/eventbus"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessioncontent"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

// blockingTransientWebTestRunner gives the integration test a real execution
// producer which emits a stable item identity, then leaves the run active long
// enough to reconnect with an independent run cursor.
type blockingTransientWebTestRunner struct {
	startedOnce sync.Once
	started     chan struct{}
	release     chan struct{}
}

func (r *blockingTransientWebTestRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (r *blockingTransientWebTestRunner) RunSessionTurn(ctx context.Context, request execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	request.Emit(model.AgentIterationStartedEvent{Iteration: 1})
	request.Emit(model.AssistantMessageStartedEvent{ItemID: "assistant-live", AgentIteration: 1})
	request.Emit(model.AssistantMessageUpdatedEvent{ItemID: "assistant-live", AgentIteration: 1, Revision: 1, Message: model.Message{Role: model.MessageRoleAssistant, Content: "live text"}})
	if err := request.Publisher.Publish(eventbus.AssistantReady{
		TurnID: request.TurnID, AgentIteration: 1, ItemID: "assistant-live",
		Message: model.Message{Role: model.MessageRoleAssistant, Content: "live text"},
	}); err != nil {
		return execution.SessionTurnResult{}, err
	}
	request.Emit(model.AssistantMessageCompletedEvent{ItemID: "assistant-live", AgentIteration: 1, Revision: 2, Message: model.Message{Role: model.MessageRoleAssistant, Content: "live text"}})
	r.startedOnce.Do(func() { close(r.started) })
	select {
	case <-r.release:
		return execution.SessionTurnResult{Incremental: true}, nil
	case <-ctx.Done():
		return execution.SessionTurnResult{}, ctx.Err()
	}
}

func TestSessionContentWebSocketTransientExecutionResumeAndSettlement(t *testing.T) {
	runner := &blockingTransientWebTestRunner{started: make(chan struct{}), release: make(chan struct{})}
	server, service, app := newWebTestAppServerWithRunner(t, runner)
	project, err := service.CreateProject(t.TempDir(), "Transient content")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
		DisplayName: "Transient session", CreatedCWD: project.Project.Root,
		ConfigPath: service.ConfigPath(), Provider: "fake", ModelProfile: "default", ModelID: "model-default",
	})
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + strings.TrimPrefix(server.URL, "http://")
	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), origin)
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	writeContentSubscribe(t, connection, session.ID, nil)
	subscribed, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage)
	if !ok {
		t.Fatal("subscribed missing")
	}
	snapshot, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage)
	if !ok {
		t.Fatal("snapshot missing")
	}

	run, err := app.runs.coordinator.Start(session.ID, execution.SessionMessageInput{Content: "start"}, nil)
	if err != nil {
		t.Fatalf("start run error = %v", err)
	}
	acceptedRunID := run.ID()
	if acceptedRunID == "" {
		t.Fatal("accepted run has no id")
	}

	var startedEvent, textEvent protocol.TransientSubscriptionEvent
	var activeDescriptor sessioncontent.ActiveRunDescriptor
	gotActive := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (startedEvent.Type == "" || textEvent.Type == "" || !gotActive) {
		message := readWebAppMessage(t, connection)
		switch typed := message.(type) {
		case protocol.SubscriptionEventMessage:
			decoded, decodeErr := protocol.DecodeSubscriptionEvent(typed.Payload.Event)
			if decodeErr != nil {
				t.Fatalf("live subscription event decode: %v", decodeErr)
			}
			if decoded.RunID != acceptedRunID || typed.Payload.Resource.ID != session.ID {
				t.Fatalf("live event identity = %#v/%#v", decoded, typed.Payload.Resource)
			}
			switch decoded.Type {
			case protocol.SubscriptionEventRunStarted:
				startedEvent = decoded
			case protocol.SubscriptionEventAssistantMessageUpdated:
				textEvent = decoded
			}
		case protocol.ChangeMessage:
			for _, operation := range typed.Payload.Operations {
				if operation.Op != sessioncontent.OpActiveRunReplace {
					continue
				}
				var body struct {
					ActiveRun *sessioncontent.ActiveRunDescriptor `json:"active_run"`
				}
				if err := json.Unmarshal(operation.Raw, &body); err != nil {
					t.Fatal(err)
				}
				if body.ActiveRun != nil && body.ActiveRun.RunID == acceptedRunID {
					activeDescriptor = *body.ActiveRun
					gotActive = true
				}
			}
		}
	}
	if startedEvent.Type != protocol.SubscriptionEventRunStarted || startedEvent.RunCursor != "1" {
		t.Fatalf("run.started = %#v, want cursor 1", startedEvent)
	}
	if textEvent.Type != protocol.SubscriptionEventAssistantMessageUpdated || textEvent.RunCursor != "3" || textEvent.ItemID != "assistant-live" || textEvent.AssistantContent != "live text" {
		t.Fatalf("assistant message update = %#v, want stable snapshot at cursor 3", textEvent)
	}
	if !gotActive || activeDescriptor.RunEpoch == "" || !activeDescriptor.ReplayAvailable {
		t.Fatalf("active recovery descriptor = %#v, want run epoch and replay availability", activeDescriptor)
	}
	if activeDescriptor.ReplayFromCursor != "1" || activeDescriptor.ReplayToCursor != "1" || activeDescriptor.RunCursor != "1" {
		t.Fatalf("active replay descriptor = %#v, want the admitted baseline at cursor 1", activeDescriptor)
	}

	// Close the first live subscription without settling the run. The owner
	// remains alive and the run replay buffer is independent of the durable
	// journal, so the second real WebSocket can resume only cursor 1.
	if err := connection.Close(websocket.StatusNormalClosure, "reconnect"); err != nil {
		t.Fatal(err)
	}
	second := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), origin)
	defer second.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, second)
	if _, ok := readWebAppMessage(t, second).(protocol.WelcomeMessage); !ok {
		t.Fatal("second welcome missing")
	}
	writeContentSubscribeWithRunResume(t, second, session.ID, &protocol.ResumeToken{
		StreamEpoch: subscribed.Payload.StreamEpoch, Sequence: snapshot.Payload.Sequence,
	}, &protocol.RunResumeToken{RunEpoch: activeDescriptor.RunEpoch, RunID: acceptedRunID, RunCursor: "1"})
	if _, ok := readWebAppMessage(t, second).(protocol.SubscribedMessage); !ok {
		t.Fatal("second subscribed missing")
	}
	ackPayload, err := protocol.EncodeMessage(protocol.AckMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeAck, ID: "durable-ack"},
		Payload:  protocol.AckPayload{SubscriptionID: "session-content:" + session.ID, StreamEpoch: subscribed.Payload.StreamEpoch, Sequence: snapshot.Payload.Sequence},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Write(context.Background(), websocket.MessageText, ackPayload); err != nil {
		t.Fatal(err)
	}

	var replayed protocol.TransientSubscriptionEvent
	var reconnectKinds []string
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && replayed.Type == "" {
		message, readErr := tryReadWebAppMessage(second, 200*time.Millisecond)
		if readErr != nil {
			continue
		}
		reconnectKinds = append(reconnectKinds, describeWebAppMessage(message))
		if event, ok := message.(protocol.SubscriptionEventMessage); ok {
			decoded, decodeErr := protocol.DecodeSubscriptionEvent(event.Payload.Event)
			if decodeErr != nil {
				t.Fatalf("replayed event decode: %v", decodeErr)
			}
			if decoded.Type == protocol.SubscriptionEventAssistantMessageUpdated {
				replayed = decoded
			}
		}
	}
	if replayed.RunID != acceptedRunID || replayed.RunCursor != "3" || replayed.ItemID != "assistant-live" {
		detail, detailErr := service.GetSession(session.ID)
		t.Fatalf("replayed transient event = %#v, want run cursor 2; messages=%v durable=%#v durable_err=%v", replayed, reconnectKinds, detail, detailErr)
	}

	// An epoch mismatch is resource-local recovery, not a connection failure.
	wrongEpoch := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), origin)
	writeWebAppHello(t, wrongEpoch)
	if _, ok := readWebAppMessage(t, wrongEpoch).(protocol.WelcomeMessage); !ok {
		t.Fatal("wrong-epoch welcome missing")
	}
	writeContentSubscribeWithRunResume(t, wrongEpoch, session.ID, &protocol.ResumeToken{
		StreamEpoch: subscribed.Payload.StreamEpoch, Sequence: snapshot.Payload.Sequence,
	}, &protocol.RunResumeToken{RunEpoch: "wrong-epoch", RunID: acceptedRunID, RunCursor: "1"})
	if _, ok := readWebAppMessage(t, wrongEpoch).(protocol.SubscribedMessage); !ok {
		t.Fatal("wrong-epoch subscribed missing")
	}
	var wrongEpochResync protocol.ResyncRequiredMessage
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && wrongEpochResync.Payload.Reason == "" {
		message := readWebAppMessage(t, wrongEpoch)
		if candidate, ok := message.(protocol.ResyncRequiredMessage); ok {
			wrongEpochResync = candidate
		}
	}
	if wrongEpochResync.Payload.Reason != "active_run_epoch_mismatch" || wrongEpochResync.Payload.Resource.ID != session.ID {
		t.Fatalf("wrong-epoch recovery = %#v", wrongEpochResync.Payload)
	}
	// A transient-only resync is resource-local: the durable subscription stays
	// alive. Trigger a fresh durable mutation and verify the same subscription
	// receives it (after the durable replay frames that precede it), proving
	// the connection is still usable and the durable stream continues.
	durableItem := sessions.SessionItemFromMessage("after-resync", model.Message{Role: model.MessageRoleUser, Content: "after-resync"})
	durableItem.TurnID, durableItem.AgentIteration = "turn-resync", 1
	if _, err := service.SessionStore().AppendItem(session.ID, durableItem); err != nil {
		t.Fatal(err)
	}
	var sawResyncChange bool
	resyncDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(resyncDeadline) && !sawResyncChange {
		message := readWebAppMessage(t, wrongEpoch)
		change, ok := message.(protocol.ChangeMessage)
		if !ok {
			t.Fatalf("expected durable change after transient resync, got %T", message)
		}
		for _, operation := range change.Payload.Operations {
			if operation.Op != sessioncontent.OpItemUpsert {
				continue
			}
			var body struct {
				Item sessioncontent.Item `json:"item"`
			}
			if err := json.Unmarshal(operation.Raw, &body); err != nil {
				t.Fatal(err)
			}
			if body.Item.Key.ItemID == "after-resync" {
				sawResyncChange = true
			}
		}
	}
	if !sawResyncChange {
		t.Fatal("wrong-epoch subscription did not receive the durable mutation after transient resync")
	}
	// The same socket still accepts a fresh subscription for the same resource.
	writeContentSubscribeWithRunResumeID(t, "session-content-recovery:"+session.ID, session.ID, wrongEpoch, nil, nil)
	if _, ok := readWebAppMessage(t, wrongEpoch).(protocol.SubscribedMessage); !ok {
		t.Fatal("replacement subscription missing")
	}
	if replacement, ok := readWebAppMessage(t, wrongEpoch).(protocol.SnapshotMessage); !ok || replacement.Payload.SubscriptionID != "session-content-recovery:"+session.ID {
		t.Fatalf("replacement subscription baseline = %#v, want snapshot for replacement id", replacement)
	}
	wrongEpoch.Close(websocket.StatusNormalClosure, "done")

	close(runner.release)
	var settled protocol.TransientSubscriptionEvent
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && settled.Type == "" {
		message := readWebAppMessage(t, second)
		if event, ok := message.(protocol.SubscriptionEventMessage); ok {
			decoded, decodeErr := protocol.DecodeSubscriptionEvent(event.Payload.Event)
			if decodeErr != nil {
				t.Fatalf("settlement event decode: %v", decodeErr)
			}
			if decoded.Type == protocol.SubscriptionEventRunSettled {
				settled = decoded
			}
		}
	}
	if settled.RunID != acceptedRunID || settled.RunCursor != "5" || settled.Settlement == nil {
		t.Fatalf("settlement event = %#v, want cursor 5 and watermark", settled)
	}
	if settled.Settlement.RunCursor != "4" || settled.Settlement.ResourceRevision == "" {
		t.Fatalf("settlement watermark = %#v, want durable revision covering cursor 4", settled.Settlement)
	}
	if !settled.Settlement.Verified || len(settled.Settlement.CoveredItems) == 0 {
		t.Fatalf("settlement watermark = %#v, want verified item coverage", settled.Settlement)
	}
}

func tryReadWebAppMessage(connection *websocket.Conn, timeout time.Duration) (protocol.Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		return nil, err
	}
	if messageType != websocket.MessageText {
		return nil, fmt.Errorf("unexpected websocket frame type %v", messageType)
	}
	return protocol.DecodeMessage(payload)
}

func describeWebAppMessage(message protocol.Message) string {
	switch typed := message.(type) {
	case protocol.ResyncRequiredMessage:
		return fmt.Sprintf("resync:%s", typed.Payload.Reason)
	case protocol.ErrorMessage:
		return fmt.Sprintf("error:%s", typed.Payload.Code)
	default:
		return fmt.Sprintf("%T", message)
	}
}

func writeContentSubscribeWithRunResume(t *testing.T, connection *websocket.Conn, sessionID string, resume *protocol.ResumeToken, active *protocol.RunResumeToken) {
	writeContentSubscribeWithRunResumeID(t, "session-content:"+sessionID, sessionID, connection, resume, active)
}

func writeContentSubscribeWithRunResumeID(t *testing.T, subscriptionID, sessionID string, connection *websocket.Conn, resume *protocol.ResumeToken, active *protocol.RunResumeToken) {
	t.Helper()
	message := protocol.SubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "content-subscribe-resume-" + subscriptionID},
		Payload: protocol.SubscribePayload{
			SubscriptionID: subscriptionID,
			Resource:       protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: sessionID},
			Resume:         resume, ActiveRunResume: active,
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

type isolatedTransientWebTestRunner struct{}

func (isolatedTransientWebTestRunner) SupportsIncrementalSessionTurn(context.Context, execution.SessionTurnRequest) (bool, error) {
	return true, nil
}

func (isolatedTransientWebTestRunner) RunSessionTurn(_ context.Context, request execution.SessionTurnRequest) (execution.SessionTurnResult, error) {
	request.Emit(model.AgentIterationStartedEvent{Iteration: 1})
	request.Emit(model.AssistantMessageStartedEvent{ItemID: "b-item", AgentIteration: 1})
	request.Emit(model.AssistantMessageUpdatedEvent{ItemID: "b-item", AgentIteration: 1, Revision: 1, Message: model.Message{Role: model.MessageRoleAssistant, Content: "B only"}})
	if err := request.Publisher.Publish(eventbus.AssistantReady{
		TurnID: request.TurnID, AgentIteration: 1, ItemID: "b-item",
		Message: model.Message{Role: model.MessageRoleAssistant, Content: "B only"},
	}); err != nil {
		return execution.SessionTurnResult{}, err
	}
	request.Emit(model.AssistantMessageCompletedEvent{ItemID: "b-item", AgentIteration: 1, Revision: 2, Message: model.Message{Role: model.MessageRoleAssistant, Content: "B only"}})
	return execution.SessionTurnResult{Incremental: true}, nil
}

func TestSessionContentWebSocketDoesNotFanoutUnsubscribedRun(t *testing.T) {
	server, service, app := newWebTestAppServerWithRunner(t, isolatedTransientWebTestRunner{})
	project, err := service.CreateProject(t.TempDir(), "Transient isolation")
	if err != nil {
		t.Fatal(err)
	}
	create := func(name string) string {
		session, createErr := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
			DisplayName: name, CreatedCWD: project.Project.Root,
			ConfigPath: service.ConfigPath(), Provider: "fake", ModelProfile: "default", ModelID: "default",
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return session.ID
	}
	sessionA, sessionB := create("A"), create("B")
	origin := "http://" + strings.TrimPrefix(server.URL, "http://")
	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), origin)
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	writeContentSubscribe(t, connection, sessionA, nil)
	if _, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage); !ok {
		t.Fatal("A subscribed missing")
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage); !ok {
		t.Fatal("A snapshot missing")
	}

	if _, err := app.runs.coordinator.Start(sessionB, execution.SessionMessageInput{Content: "run B"}, nil); err != nil {
		t.Fatalf("B run error = %v", err)
	}
	// shared coordinator but must not be encoded or delivered to A.
	assertWebSocketNoMessage(t, connection, 250*time.Millisecond)
}
