package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/blobstore"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/projectindex"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

func TestProjectIndexWebSocketIsLifecycleBacked(t *testing.T) {
	server, service, _ := newWebTestAppServerWithRunner(t, webTestRunner{})
	projectResult, err := service.CreateProject(t.TempDir(), "first")
	if err != nil {
		t.Fatal(err)
	}
	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	message := protocol.SubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "project-index-subscribe"},
		Payload: protocol.SubscribePayload{
			SubscriptionID: "project-index",
			Resource:       protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: projectindex.ProjectIndexResourceID},
		},
	}
	encoded, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage); !ok {
		t.Fatal("project index subscribed missing")
	}
	snapshotMessage, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage)
	if !ok {
		t.Fatal("project index snapshot missing")
	}
	var snapshot projectindex.ProjectIndexSnapshot
	if err := json.Unmarshal(snapshotMessage.Payload.Content.Inline, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Projects) != 1 || snapshot.Projects[0].ID != projectResult.Project.ID || snapshot.Projects[0].DisplayName != "first" {
		t.Fatalf("project snapshot = %#v", snapshot.Projects)
	}

	if _, err := service.RenameProject(projectResult.Project.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	change := readProjectIndexChange(t, connection)
	if len(change.Payload.Operations) != 1 {
		t.Fatalf("project change operations = %#v", change.Payload.Operations)
	}
	var operation struct {
		Op    string                      `json:"op"`
		Key   string                      `json:"key"`
		Value projectindex.ProjectSummary `json:"value"`
	}
	if err := json.Unmarshal(change.Payload.Operations[0].Raw, &operation); err != nil {
		t.Fatal(err)
	}
	if operation.Op != projectindex.OperationUpsert || operation.Key != projectResult.Project.ID || operation.Value.DisplayName != "renamed" {
		t.Fatalf("rename operation = %#v", operation)
	}

	if _, err := service.ArchiveProject(projectResult.Project.ID); err != nil {
		t.Fatal(err)
	}
	archive := readProjectIndexChange(t, connection)
	if archive.Payload.PreviousSequence != change.Payload.Sequence {
		t.Fatalf("archive previous sequence = %s after %s", archive.Payload.PreviousSequence, change.Payload.Sequence)
	}
	if _, err := service.RestoreProject(projectResult.Project.ID); err != nil {
		t.Fatal(err)
	}
	_ = readProjectIndexChange(t, connection)
	if _, err := service.ArchiveProject(projectResult.Project.ID); err != nil {
		t.Fatal(err)
	}
	_ = readProjectIndexChange(t, connection)
	if _, err := service.RemoveProject(projectResult.Project.ID); err != nil {
		t.Fatal(err)
	}
	remove := readProjectIndexChange(t, connection)
	var removeOperation struct {
		Op  string `json:"op"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(remove.Payload.Operations[0].Raw, &removeOperation); err != nil {
		t.Fatal(err)
	}
	if removeOperation.Op != projectindex.OperationRemove || removeOperation.Key != projectResult.Project.ID {
		t.Fatalf("remove operation = %#v", removeOperation)
	}
}

func TestProjectIndexBlobUsesHTTPIntegrityAuthAndExpiry(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	blobStore, err := blobstore.New(blobstore.Options{
		BaseURL: "/api/blobs/", TTL: time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer blobStore.Close()
	service, err := execution.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerOptions{Service: service, Token: testToken, BlobStore: blobStore})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer server.Close()

	if _, err := service.CreateProject(t.TempDir(), strings.Repeat("large project ", 6000)); err != nil {
		t.Fatal(err)
	}
	if err := server.projectIndex.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	connection := dialWebApp(t, httpServer.URL, issueWebSocketTicket(t, httpServer.URL), "http://"+strings.TrimPrefix(httpServer.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}
	message := protocol.SubscribeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeSubscribe, ID: "project-blob-subscribe"},
		Payload: protocol.SubscribePayload{
			SubscriptionID: "project-index-blob",
			Resource:       protocol.ResourceKey{Type: protocol.ResourceTypeProjectIndex, ID: projectindex.ProjectIndexResourceID},
		},
	}
	encoded, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := readWebAppMessage(t, connection).(protocol.SubscribedMessage); !ok {
		t.Fatal("project index subscribed missing")
	}
	snapshotMessage, ok := readWebAppMessage(t, connection).(protocol.SnapshotMessage)
	if !ok || snapshotMessage.Payload.Content.Blob == nil {
		t.Fatalf("project snapshot was not blob-backed: %#v", snapshotMessage)
	}
	descriptor := *snapshotMessage.Payload.Content.Blob
	if descriptor.ContentType != "application/json" || descriptor.Size <= 64*1024 {
		t.Fatalf("project blob descriptor = %#v", descriptor)
	}

	unauthorizedRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+descriptor.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorizedResponse, err := http.DefaultClient.Do(unauthorizedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if unauthorizedResponse.StatusCode != http.StatusUnauthorized {
		unauthorizedResponse.Body.Close()
		t.Fatalf("unauthorized project blob status=%d", unauthorizedResponse.StatusCode)
	}
	unauthorizedResponse.Body.Close()

	authorizedRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+descriptor.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	authorizedRequest.Header.Set("Authorization", "Bearer "+testToken)
	authorizedResponse, err := http.DefaultClient.Do(authorizedRequest)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(authorizedResponse.Body)
	authorizedResponse.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	digest := sha256.Sum256(content)
	hexDigest := hex.EncodeToString(digest[:])
	if authorizedResponse.StatusCode != http.StatusOK || authorizedResponse.Header.Get("Content-Type") != "application/json" || authorizedResponse.Header.Get("ETag") != descriptor.ETag || authorizedResponse.Header.Get("X-Content-SHA256") != hexDigest || hexDigest != descriptor.SHA256 || uint64(len(content)) != descriptor.Size {
		t.Fatalf("authorized project blob status=%d headers=%v size=%d descriptor=%#v", authorizedResponse.StatusCode, authorizedResponse.Header, len(content), descriptor)
	}

	now = now.Add(2 * time.Minute)
	expiredRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+descriptor.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	expiredRequest.Header.Set("Authorization", "Bearer "+testToken)
	expiredResponse, err := http.DefaultClient.Do(expiredRequest)
	if err != nil {
		t.Fatal(err)
	}
	expiredResponse.Body.Close()
	if expiredResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("expired project blob status=%d", expiredResponse.StatusCode)
	}
}

func readProjectIndexChange(t *testing.T, connection *websocket.Conn) protocol.ChangeMessage {
	t.Helper()
	for {
		message := readWebAppMessage(t, connection)
		if change, ok := message.(protocol.ChangeMessage); ok {
			return change
		}
	}
}
