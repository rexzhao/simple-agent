package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/blobstore"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessioncontent"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestSessionContentWebSocketUsesDurableProvider(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	project, err := service.CreateProject(t.TempDir(), "Content project")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
		DisplayName: "Content session", CreatedCWD: project.Project.Root,
		ConfigPath: service.ConfigPath(), Provider: "fake", ModelProfile: "default", ModelID: "model-default",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := service.SessionStore().AppendItem(session.ID, sessions.SessionItemFromMessage(fmt.Sprintf("before-%d", i), model.Message{Role: model.MessageRoleUser, Content: strings.Repeat("before-", 500)})); err != nil {
			t.Fatal(err)
		}
	}
	largeContent := strings.Repeat("界", 40000)
	if _, err := service.SessionStore().AppendItem(session.ID, sessions.SessionItemFromMessage("large-item", model.Message{Role: model.MessageRoleUser, Content: largeContent})); err != nil {
		t.Fatal(err)
	}
	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	writeContentSubscribe(t, connection, session.ID, nil)
	if _, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage); !ok {
		t.Fatal("content subscribed missing")
	}
	snapshotMessage, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage)
	if !ok {
		t.Fatal("content snapshot missing")
	}
	snapshotJSON := snapshotMessage.Payload.Content.Inline
	if snapshotMessage.Payload.Content.Blob == nil {
		t.Fatal("large session-content snapshot did not use the Blob data plane")
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+snapshotMessage.Payload.Content.Blob.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("snapshot blob status = %d", response.StatusCode)
	}
	snapshotJSON, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot sessioncontent.Snapshot
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.History.Items) != 21 || !strings.HasPrefix(snapshot.History.Items[0].Key.ItemID, "before-") {
		t.Fatalf("content snapshot = %#v", snapshot.History.Items)
	}
	var largeItem *sessioncontent.Item
	for index := range snapshot.History.Items {
		if snapshot.History.Items[index].Key.ItemID == "large-item" {
			largeItem = &snapshot.History.Items[index]
			break
		}
	}
	if largeItem == nil || largeItem.Message == nil || largeItem.Message.Content == nil || largeItem.Message.Content.Blob == nil {
		t.Fatalf("large durable item was not represented by an item Blob descriptor: %#v", largeItem)
	}
	itemRequest, err := http.NewRequest(http.MethodGet, server.URL+largeItem.Message.Content.Blob.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	itemRequest.Header.Set("Authorization", "Bearer "+testToken)
	itemResponse, err := http.DefaultClient.Do(itemRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer itemResponse.Body.Close()
	if itemResponse.StatusCode != http.StatusOK {
		t.Fatalf("item blob status = %d", itemResponse.StatusCode)
	}
	itemBytes, err := io.ReadAll(itemResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(itemBytes)
	if string(itemBytes) != largeContent || uint64(len(itemBytes)) != largeItem.Message.Content.Blob.Size || largeItem.Message.Content.Blob.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("item blob content/metadata mismatch: size=%d descriptor=%#v", len(itemBytes), largeItem.Message.Content.Blob)
	}
}

func TestSessionContentWebSocketDurableOperationsIsolationAndUnsubscribe(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	project, err := service.CreateProject(t.TempDir(), "Content operations")
	if err != nil {
		t.Fatal(err)
	}
	create := func(id string) string {
		session, createErr := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
			DisplayName: id, CreatedCWD: project.Project.Root,
			ConfigPath: service.ConfigPath(), Provider: "fake", ModelProfile: "default", ModelID: "model-default",
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return session.ID
	}
	sessionA, sessionB := create("ws-content-a"), create("ws-content-b")
	origin := "http://" + strings.TrimPrefix(server.URL, "http://")
	connectionA := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), origin)
	defer connectionA.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connectionA)
	if _, ok := readWebAppMessage(t, connectionA).(protocol.WelcomeMessage); !ok {
		t.Fatal("A welcome missing")
	}
	connectionB := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), origin)
	defer connectionB.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connectionB)
	if _, ok := readWebAppMessage(t, connectionB).(protocol.WelcomeMessage); !ok {
		t.Fatal("B welcome missing")
	}

	writeContentSubscribe(t, connectionA, sessionA, nil)
	if _, ok := readWebAppMessage(t, connectionA).(protocol.SubscribedMessage); !ok {
		t.Fatal("A content subscribed missing")
	}
	if _, ok := readWebAppMessage(t, connectionA).(protocol.SnapshotMessage); !ok {
		t.Fatal("A content snapshot missing")
	}
	appendAndCheck := func(sessionID, itemID, wantResource string, readConnection *websocket.Conn) protocol.ChangeMessage {
		item := sessions.SessionItemFromMessage(itemID, model.Message{Role: model.MessageRoleUser, Content: itemID})
		if _, appendErr := service.SessionStore().AppendItem(sessionID, item); appendErr != nil {
			t.Fatal(appendErr)
		}
		change := readContentChange(t, readConnection)
		if change.Payload.Resource.ID != wantResource || change.Payload.Resource.Type != protocol.ResourceTypeSessionContent {
			t.Fatalf("change resource = %#v, want session_content/%s", change.Payload.Resource, wantResource)
		}
		state, loadErr := service.SessionStore().LoadState(sessionID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if change.Payload.ResourceRevision != protocol.ResourceRevision(fmt.Sprint(state.LastSeq)) {
			t.Fatalf("resource revision = %q, want committed LastSeq %d", change.Payload.ResourceRevision, state.LastSeq)
		}
		return change
	}
	change := appendAndCheck(sessionA, "a-1", sessionA, connectionA)
	if len(change.Payload.Operations) == 0 || change.Payload.Operations[0].Op != sessioncontent.OpItemUpsert {
		t.Fatalf("A append operations = %#v, want item.upsert", change.Payload.Operations)
	}
	var upsert struct {
		Item sessioncontent.Item `json:"item"`
	}
	if err := json.Unmarshal(change.Payload.Operations[0].Raw, &upsert); err != nil {
		t.Fatal(err)
	}
	if upsert.Item.Key.ItemID != "a-1" {
		t.Fatalf("A item operation identity = %#v", upsert.Item.Key)
	}
	if _, err := service.SessionStore().UpdateItem(sessionA, sessions.SessionItem{ID: "a-1", Message: &model.Message{Role: model.MessageRoleUser, Content: "a-1-updated"}}); err != nil {
		t.Fatal(err)
	}
	updated := readContentChange(t, connectionA)
	if !hasOperation(updated.Payload.Operations, sessioncontent.OpItemUpsert) {
		t.Fatalf("item update operations = %#v", updated.Payload.Operations)
	}
	if _, err := service.RenameSession(sessionA, "A renamed"); err != nil {
		t.Fatal(err)
	}
	metadataChange := readContentChange(t, connectionA)
	if !hasOperation(metadataChange.Payload.Operations, sessioncontent.OpMetadataReplace) {
		t.Fatalf("metadata operations = %#v", metadataChange.Payload.Operations)
	}
	if _, err := service.SessionStore().CreateRun(sessionA, "run-ws", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	runChange := readContentChange(t, connectionA)
	if !hasOperation(runChange.Payload.Operations, sessioncontent.OpActiveRunReplace) {
		t.Fatalf("run baseline operations = %#v", runChange.Payload.Operations)
	}
	if _, err := service.SessionStore().SetRunStatus(sessionA, "run-ws", sessions.RunStatusCancelled, time.Now()); err != nil {
		t.Fatal(err)
	}
	clearChange := readContentChange(t, connectionA)
	if !hasOperation(clearChange.Payload.Operations, sessioncontent.OpActiveRunClear) {
		t.Fatalf("run clear operations = %#v", clearChange.Payload.Operations)
	}

	// B is not subscribed, so neither connection can receive B's detailed
	// durable item. The A connection is also an explicit session isolation
	// assertion, not merely a message-type assertion.
	if _, err := service.SessionStore().AppendItem(sessionB, sessions.SessionItemFromMessage("b-secret", model.Message{Role: model.MessageRoleUser, Content: "B secret"})); err != nil {
		t.Fatal(err)
	}
	assertWebSocketNoMessage(t, connectionB, 150*time.Millisecond)

	writeContentUnsubscribe(t, connectionA, "session-content:"+sessionA)
	if _, ok := readWebAppMessage(t, connectionA).(protocol.UnsubscribedMessage); !ok {
		t.Fatal("A unsubscribe acknowledgement missing")
	}
	if _, err := service.SessionStore().AppendItem(sessionA, sessions.SessionItemFromMessage("after-unsubscribe", model.Message{Role: model.MessageRoleUser, Content: "must not stream"})); err != nil {
		t.Fatal(err)
	}
	// A second subscribe is a deterministic ordering check: if the old
	// subscription leaked after unsubscribe, its detailed change would arrive
	// before this new subscription's acknowledgement/snapshot.
	writeContentSubscribe(t, connectionA, sessionA, nil)
	if _, ok := readWebAppMessage(t, connectionA).(protocol.SubscribedMessage); !ok {
		t.Fatal("resubscribe received a leaked post-unsubscribe change")
	}
	if _, ok := readWebAppMessage(t, connectionA).(protocol.SnapshotMessage); !ok {
		t.Fatal("resubscribe snapshot missing")
	}
}

func TestSessionContentOversizedLiveChangeResyncKeepsConnectionUsable(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	project, err := service.CreateProject(t.TempDir(), "Oversized content change")
	if err != nil {
		t.Fatal(err)
	}
	create := func(id string) string {
		session, createErr := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
			DisplayName: id, CreatedCWD: project.Project.Root,
			ConfigPath: service.ConfigPath(), Provider: "fake", ModelProfile: "default", ModelID: "model-default",
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return session.ID
	}
	sessionA, sessionB := create("ws-content-frame-a"), create("ws-content-frame-b")
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

	metadata, err := service.SessionStore().LoadState(sessionA)
	if err != nil {
		t.Fatal(err)
	}
	metadata.ModelParameters = map[string]any{"oversized": strings.Repeat("x", 300*1024)}
	if _, err := service.SessionStore().SaveMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	var recovery protocol.ResyncRequiredMessage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		message := readWebAppMessage(t, connection)
		if candidate, ok := message.(protocol.ResyncRequiredMessage); ok {
			recovery = candidate
			break
		}
	}
	if recovery.Payload.SubscriptionID != "session-content:"+sessionA || recovery.Payload.Resource.ID != sessionA {
		t.Fatalf("oversized recovery = %#v", recovery)
	}

	// The oversized resource is desynced and removed, not allowed to make the
	// gateway close the whole connection. A different content subscription
	// and a control ping still use the same WebSocket successfully.
	writeContentSubscribe(t, connection, sessionB, nil)
	if subscribed, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage); !ok || subscribed.Payload.Resource.ID != sessionB {
		t.Fatalf("B subscription after A resync = %#v", subscribed)
	}
	if snapshot, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage); !ok || snapshot.Payload.Resource.ID != sessionB {
		t.Fatalf("B snapshot after A resync = %#v", snapshot)
	}
	ping := protocol.PingMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypePing, ID: "oversized-ping"}, Payload: protocol.PingPayload{}}
	payload, err := protocol.EncodeMessage(ping)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	if pong, ok := readWebAppMessage(t, connection).(protocol.PongMessage); !ok {
		t.Fatalf("connection was not usable after oversized change: %#v", pong)
	}
}

func TestSessionContentBlobCapacityResyncKeepsOtherSubscriptionAlive(t *testing.T) {
	home := t.TempDir()
	service, err := execution.NewServiceWithOptions(home, execution.ServiceOptions{TurnRunner: webTestRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	writeWebTestConfig(t, home)
	blobs, err := blobstore.New(blobstore.Options{
		MaxEntries:      3,
		MaxBytes:        4 * 1024 * 1024,
		MaxBlobBytes:    2 * 1024 * 1024,
		JanitorInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	app, err := NewServer(ServerOptions{Context: context.Background(), Service: service, Token: testToken, CWD: home, BlobStore: blobs})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	project, err := service.CreateProject(t.TempDir(), "Capacity content")
	if err != nil {
		t.Fatal(err)
	}
	create := func(id string) string {
		session, createErr := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
			DisplayName: id, CreatedCWD: project.Project.Root,
			ConfigPath: service.ConfigPath(), Provider: "fake", ModelProfile: "default", ModelID: "model-default",
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return session.ID
	}
	sessionA, sessionB := create("ws-capacity-a"), create("ws-capacity-b")
	large := strings.Repeat("capacity-界", 10000)
	for i := 0; i < 3; i++ {
		content := large + fmt.Sprintf("-%d", i)
		if _, err := service.SessionStore().AppendItem(sessionA, sessions.SessionItemFromMessage(fmt.Sprintf("large-%d", i), model.Message{Role: model.MessageRoleUser, Content: content})); err != nil {
			t.Fatal(err)
		}
	}
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
	writeContentSubscribe(t, connection, sessionB, nil)
	if _, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage); !ok {
		t.Fatal("B subscribed missing")
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage); !ok {
		t.Fatal("B snapshot missing")
	}
	if got := blobs.Stats().Entries; got != 3 {
		t.Fatalf("A item blobs did not fill the configured bound: %+v", blobs.Stats())
	}

	if _, err := service.SessionStore().AppendItem(sessionA, sessions.SessionItemFromMessage("large-4", model.Message{Role: model.MessageRoleUser, Content: large + "new"})); err != nil {
		t.Fatal(err)
	}
	var recovery protocol.ResyncRequiredMessage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		message := readWebAppMessage(t, connection)
		if candidate, ok := message.(protocol.ResyncRequiredMessage); ok {
			recovery = candidate
			break
		}
	}
	if recovery.Payload.SubscriptionID != "session-content:"+sessionA || recovery.Payload.Resource.ID != sessionA {
		t.Fatalf("capacity recovery = %#v", recovery)
	}
	if _, err := service.SessionStore().AppendItem(sessionB, sessions.SessionItemFromMessage("small-b", model.Message{Role: model.MessageRoleUser, Content: "B"})); err != nil {
		t.Fatal(err)
	}
	change := readContentChange(t, connection)
	if change.Payload.Resource.ID != sessionB || change.Payload.ResourceRevision == "" || len(change.Payload.Operations) == 0 {
		t.Fatalf("other subscription after capacity resync = %#v", change)
	}
}

func hasOperation(operations []protocol.ChangeOperation, want string) bool {
	for _, operation := range operations {
		if operation.Op == want {
			return true
		}
	}
	return false
}

func assertWebSocketNoMessage(t *testing.T, connection *websocket.Conn, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _, err := connection.Read(ctx)
	if err == nil {
		t.Fatal("received an unexpected WebSocket message")
	}
}

func writeContentUnsubscribe(t *testing.T, connection *websocket.Conn, subscriptionID string) {
	t.Helper()
	message := protocol.UnsubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeUnsubscribe, ID: "content-unsubscribe-" + subscriptionID},
		Payload:  protocol.UnsubscribePayload{SubscriptionID: subscriptionID},
	}
	payload, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func writeContentSubscribe(t *testing.T, connection *websocket.Conn, sessionID string, resume *protocol.ResumeToken) {
	t.Helper()
	message := protocol.SubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "content-subscribe-" + sessionID},
		Payload: protocol.SubscribePayload{
			SubscriptionID: "session-content:" + sessionID,
			Resource:       protocol.ResourceKey{Type: protocol.ResourceTypeSessionContent, ID: sessionID},
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

func readContentChange(t *testing.T, connection *websocket.Conn) protocol.ChangeMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		message := readWebAppMessage(t, connection)
		if change, ok := message.(protocol.ChangeMessage); ok {
			return change
		}
	}
	t.Fatal("timed out waiting for content change")
	return protocol.ChangeMessage{}
}
