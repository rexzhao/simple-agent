package webapp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

func TestWebSocketTicketHTTPBoundaryAndUpgrade(t *testing.T) {
	server, _ := newWebTestServer(t)

	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/ws-ticket", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST ws-ticket status=%d body=%s", response.StatusCode, readBody(response))
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("ws-ticket cache-control=%q, want no-store", response.Header.Get("Cache-Control"))
	}
	var ticketResponse struct {
		Ticket    string `json:"ticket"`
		ExpiresAt string `json:"expires_at"`
	}
	decodeResponse(t, response, &ticketResponse)
	if ticketResponse.Ticket == "" || ticketResponse.ExpiresAt == "" {
		t.Fatalf("ws-ticket response=%#v", ticketResponse)
	}
	if _, err := time.Parse(time.RFC3339Nano, ticketResponse.ExpiresAt); err != nil {
		t.Fatalf("ws-ticket expires_at=%q: %v", ticketResponse.ExpiresAt, err)
	}

	unauthorizedRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/ws-ticket", nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized, err := http.DefaultClient.Do(unauthorizedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if unauthorized.StatusCode != http.StatusUnauthorized || responseErrorCode(t, unauthorized) != "unauthorized" {
		t.Fatalf("unauthorized ws-ticket status=%d", unauthorized.StatusCode)
	}

	connection := dialWebApp(t, server.URL, ticketResponse.Ticket, "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	writeWebAppHello(t, connection)
	message := readWebAppMessage(t, connection)
	welcome, ok := message.(protocol.WelcomeMessage)
	if !ok || welcome.Payload.SelectedVersion != 1 || welcome.Payload.MaxMessageBytes != 256*1024 {
		t.Fatalf("welcome=%#v", message)
	}

	// The URL credential is atomically consumed by the successful Upgrade.
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/ws?ticket=" + ticketResponse.Ticket
	header := http.Header{}
	header.Set("Origin", "http://"+strings.TrimPrefix(server.URL, "http://"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, replayResponse, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err == nil || replayResponse == nil || replayResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed ticket err=%v response=%v", err, replayResponse)
	}
}

func TestWebSocketOriginRejectsBeforeConsumingTicket(t *testing.T) {
	server, _ := newWebTestServer(t)
	response := doJSONRequest(t, http.MethodPost, server.URL+"/api/ws-ticket", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST ws-ticket status=%d", response.StatusCode)
	}
	var ticketResponse struct {
		Ticket string `json:"ticket"`
	}
	decodeResponse(t, response, &ticketResponse)

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/ws?ticket=" + ticketResponse.Ticket
	badHeader := http.Header{}
	badHeader.Set("Origin", "http://attacker.invalid")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, badResponse, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: badHeader})
	if err == nil || badResponse == nil || badResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("invalid origin err=%v response=%v", err, badResponse)
	}

	connection := dialWebApp(t, server.URL, ticketResponse.Ticket, "http://"+strings.TrimPrefix(server.URL, "http://"))
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	writeWebAppHello(t, connection)
	if _, ok := readWebAppMessage(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("valid-origin retry did not receive welcome")
	}
}

func dialWebApp(t *testing.T, serverURL, ticket, origin string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/api/ws?ticket=" + ticket
	header := http.Header{}
	header.Set("Origin", origin)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("websocket dial status=%v: %v", response, err)
	}
	return connection
}

func writeWebAppHello(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	hello := protocol.HelloMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeHello, ID: "hello-webapp"},
		Payload:  protocol.HelloPayload{SupportedVersions: []int{1}, ClientID: "webapp-test"},
	}
	payload, err := protocol.EncodeMessage(hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func readWebAppMessage(t *testing.T, connection *websocket.Conn) protocol.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("webapp websocket frame type=%v", messageType)
	}
	message, err := protocol.DecodeMessage(payload)
	if err != nil {
		t.Fatalf("decode webapp websocket message: %v", err)
	}
	return message
}
