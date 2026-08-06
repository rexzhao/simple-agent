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
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/blobstore"
	"github.com/rexzhao/simple-agent/internal/execution"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/protocol"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestSessionHistoryReadRefreshesExpiredBlobWithoutWeakeningRequestTombstone(t *testing.T) {
	home := t.TempDir()
	service, err := execution.NewServiceWithOptions(home, execution.ServiceOptions{TurnRunner: webTestRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	writeWebTestConfig(t, home)

	clockMu := sync.Mutex{}
	now := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	blobStore, err := blobstore.New(blobstore.Options{
		BaseURL:         "/api/blobs/",
		TTL:             5 * time.Minute,
		JanitorInterval: -1,
		Now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer blobStore.Close()
	app, err := NewServer(ServerOptions{Context: context.Background(), Service: service, Token: testToken, CWD: home, BlobStore: blobStore})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	project, err := service.CreateProject(t.TempDir(), "history expiry project")
	if err != nil {
		t.Fatal(err)
	}
	historySession, err := service.CreateSession(project.Project.ID, execution.SessionCreateMetadata{
		CreatedCWD: project.Project.Root, Provider: "fake", ModelProfile: "default", ModelID: "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SessionStore().AppendItem(historySession.ID, sessions.SessionItem{
		ID:         "provider:item/1",
		Kind:       sessions.ItemKindMessage,
		Visibility: sessions.ItemVisibilityVisible,
		Audience:   sessions.ItemAudienceUser,
		Message:    &model.Message{Role: model.MessageRoleUser, Content: strings.Repeat("history-expiry-content-", 5000)},
	}); err != nil {
		t.Fatal(err)
	}

	connection := dialWebApp(t, server.URL, issueWebSocketTicket(t, server.URL), "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("welcome missing")
	}

	arguments := json.RawMessage(`{"session_id":"` + historySession.ID + `","cursor":2,"direction":"before","limit":1}`)
	readHistory := func() protocol.BlobDescriptor {
		t.Helper()
		message := protocol.CommandMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "history-expiry-command"},
			Payload:  protocol.CommandPayload{Name: "session.history.read", SchemaVersion: 1, RequestID: "history-expiry-request", Arguments: arguments},
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
			if !ok || result.Payload.RequestID != message.Payload.RequestID {
				continue
			}
			if result.Payload.Status != protocol.CommandStatusSucceeded || result.Payload.Error != nil {
				t.Fatalf("history command result=%#v", result)
			}
			var decoded struct {
				History json.RawMessage          `json:"history"`
				Blob    *protocol.BlobDescriptor `json:"blob"`
			}
			if err := json.Unmarshal(result.Payload.Result, &decoded); err != nil {
				t.Fatal(err)
			}
			if string(decoded.History) != "null" || decoded.Blob == nil {
				t.Fatalf("history descriptor=%#v", decoded)
			}
			return *decoded.Blob
		}
	}

	first := readHistory()
	if first.ContentType != "application/json" || first.Size == 0 || first.ETag == "" || first.SHA256 == "" {
		t.Fatalf("first descriptor=%#v", first)
	}
	fetch := func(descriptor protocol.BlobDescriptor, authorization bool) (int, []byte, http.Header) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, server.URL+descriptor.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorization {
			request.Header.Set("Authorization", "Bearer "+testToken)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return response.StatusCode, body, response.Header
	}
	status, bytes, headers := fetch(first, true)
	digest := sha256.Sum256(bytes)
	if status != http.StatusOK || len(bytes) != int(first.Size) || hex.EncodeToString(digest[:]) != first.SHA256 || headers.Get("ETag") != first.ETag || headers.Get("X-Content-SHA256") != first.SHA256 || headers.Get("Content-Type") != first.ContentType {
		t.Fatalf("first blob status=%d size=%d descriptor=%#v headers=%v", status, len(bytes), first, headers)
	}
	if unauthorizedStatus, _, _ := fetch(first, false); unauthorizedStatus != http.StatusUnauthorized {
		t.Fatalf("unauthorized blob status=%d", unauthorizedStatus)
	}

	clockMu.Lock()
	now = now.Add(6 * time.Minute)
	clockMu.Unlock()
	if expiredStatus, _, _ := fetch(first, true); expiredStatus != http.StatusNotFound {
		t.Fatalf("expired blob status=%d", expiredStatus)
	}

	second := readHistory()
	if second.ID == first.ID || second.URL == first.URL {
		t.Fatalf("history result replayed expired descriptor: first=%#v second=%#v", first, second)
	}
	if second.SHA256 != first.SHA256 || second.Size != first.Size || second.ETag != first.ETag {
		t.Fatalf("refreshed descriptor changed immutable content: first=%#v second=%#v", first, second)
	}
	status, bytes, headers = fetch(second, true)
	digest = sha256.Sum256(bytes)
	if status != http.StatusOK || hex.EncodeToString(digest[:]) != second.SHA256 || headers.Get("ETag") != second.ETag || headers.Get("Content-Type") != "application/json" {
		t.Fatalf("refreshed blob status=%d digest=%s descriptor=%#v headers=%v", status, hex.EncodeToString(digest[:]), second, headers)
	}

	// The volatile policy refreshes only the completed payload. The request-id
	// fingerprint tombstone still rejects a changed history query.
	conflict := protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "history-expiry-conflict"},
		Payload:  protocol.CommandPayload{Name: "session.history.read", SchemaVersion: 1, RequestID: "history-expiry-request", Arguments: json.RawMessage(`{"session_id":"` + historySession.ID + `","cursor":2,"direction":"before","limit":2}`)},
	}
	payload, err := protocol.EncodeMessage(conflict)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	for {
		value := readWebAppMessage(t, connection)
		result, ok := value.(protocol.CommandResultMessage)
		if ok && result.Payload.RequestID == conflict.Payload.RequestID {
			if result.Payload.Status != protocol.CommandStatusFailed || result.Payload.Error == nil || result.Payload.Error.Code != "idempotency_conflict" {
				t.Fatalf("changed request result=%#v", result)
			}
			break
		}
	}
}
