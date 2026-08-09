package wsgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

type manualTicker struct {
	mu      sync.Mutex
	ch      chan time.Time
	stopped bool
}

func (t *manualTicker) C() <-chan time.Time { return t.ch }
func (t *manualTicker) Stop() {
	t.mu.Lock()
	t.stopped = true
	for {
		select {
		case <-t.ch:
		default:
			t.mu.Unlock()
			return
		}
	}
}

func (t *manualTicker) deliver(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	select {
	case t.ch <- now:
	default:
	}
}

type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*manualTicker
	changed chan struct{}
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Unix(100, 0), changed: make(chan struct{})}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTicker(time.Duration) Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	ticker := &manualTicker{ch: make(chan time.Time, 1)}
	c.tickers = append(c.tickers, ticker)
	close(c.changed)
	c.changed = make(chan struct{})
	return ticker
}

func (c *manualClock) WaitForTickers(t *testing.T, count int) {
	t.Helper()
	if count < 1 {
		t.Fatalf("ticker count=%d, want positive count", count)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		if len(c.tickers) >= count {
			c.mu.Unlock()
			return
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-deadline.C:
			c.mu.Lock()
			created := len(c.tickers)
			c.mu.Unlock()
			if created >= count {
				return
			}
			t.Fatalf("created tickers=%d, want at least %d", created, count)
		}
	}
}

func (c *manualClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	now := c.now
	tickers := append([]*manualTicker(nil), c.tickers...)
	c.mu.Unlock()
	for _, ticker := range tickers {
		ticker.deliver(now)
	}
}

type recordingObserver struct {
	mu     sync.Mutex
	events []Event
	closed chan struct{}
	stream chan Event
}

func newRecordingObserver() *recordingObserver {
	return &recordingObserver{closed: make(chan struct{}), stream: make(chan Event, 128)}
}

func (o *recordingObserver) Observe(event Event) {
	o.mu.Lock()
	o.events = append(o.events, event)
	o.mu.Unlock()
	select {
	case o.stream <- event:
	default:
	}
	if event.Kind == EventConnectionClosed {
		select {
		case <-o.closed:
		default:
			close(o.closed)
		}
	}
}

func (o *recordingObserver) waitForMessage(t *testing.T, direction Direction, messageType protocol.MessageType) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-o.stream:
			if event.Kind == EventMessage && event.Direction == direction && event.MessageType == messageType {
				return
			}
		case <-deadline.C:
			t.Fatalf("did not observe %s %s", direction, messageType)
		}
	}
}

func (o *recordingObserver) Events() []Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Event(nil), o.events...)
}

type testEndpoint struct {
	server   *httptest.Server
	cancel   context.CancelFunc
	store    *TicketStore
	observer *recordingObserver
}

func newTestEndpoint(t *testing.T, options Options) *testEndpoint {
	t.Helper()
	observer, ok := options.Observer.(*recordingObserver)
	if !ok || observer == nil {
		observer = newRecordingObserver()
		options.Observer = observer
	}
	store, err := NewTicketStore(TicketStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		claims, ok := store.Consume(r.URL.Query().Get("ticket"))
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gateway.HTTPHandler(ctx, w, r, claims)
	}))
	t.Cleanup(func() {
		cancel()
		server.Close()
	})
	return &testEndpoint{server: server, cancel: cancel, store: store, observer: observer}
}

func issueTestTicket(t *testing.T, endpoint *testEndpoint) string {
	t.Helper()
	ticket, _, err := endpoint.store.Issue(TicketClaims{Principal: "test-principal"})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func dialTest(t *testing.T, endpoint *testEndpoint, ticket string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(endpoint.server.URL, "http") + "/api/ws?ticket=" + ticket
	header := http.Header{}
	header.Set("Origin", "http://"+strings.TrimPrefix(endpoint.server.URL, "http://"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("websocket dial status=%v: %v", response, err)
	}
	return connection
}

func writeHello(t *testing.T, connection *websocket.Conn, clientID string) {
	t.Helper()
	hello := protocol.HelloMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeHello, ID: "hello-1"},
		Payload:  protocol.HelloPayload{SupportedVersions: []int{1}, ClientID: clientID},
	}
	writeProtocol(t, connection, hello)
}

func writeProtocol(t *testing.T, connection *websocket.Conn, message protocol.Message) {
	t.Helper()
	payload, err := protocol.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func readProtocol(t *testing.T, connection *websocket.Conn) protocol.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type=%v, want text", messageType)
	}
	message, err := protocol.DecodeMessage(payload)
	if err != nil {
		t.Fatalf("decode protocol message: %v; payload=%s", err, payload)
	}
	return message
}

func readClose(t *testing.T, connection *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := connection.Read(ctx)
	if err == nil {
		t.Fatal("read after close succeeded")
	}
	return websocket.CloseStatus(err)
}

func TestTicketStoreExpiryAndConcurrentSingleUse(t *testing.T) {
	now := time.Unix(10, 0)
	store, err := NewTicketStore(TicketStoreOptions{Now: func() time.Time { return now }, TTL: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ticket, expiresAt, err := store.Issue(TicketClaims{Principal: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if expiresAt != now.Add(30*time.Second) || store.Size() != 1 {
		t.Fatalf("ticket expiry/size = %v/%d", expiresAt, store.Size())
	}
	var successes atomic.Int64
	var wg sync.WaitGroup
	wg.Add(64)
	for i := 0; i < 64; i++ {
		go func() {
			defer wg.Done()
			if _, ok := store.Consume(ticket); ok {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("concurrent ticket consumes = %d, want 1", successes.Load())
	}
	if _, ok := store.Consume(ticket); ok {
		t.Fatal("replayed ticket was accepted")
	}
	expired, _, err := store.Issue(TicketClaims{Principal: "p"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	if _, ok := store.Consume(expired); ok {
		t.Fatal("expired ticket was accepted")
	}
}

func TestOutboundQueueEnforcesCountAndBytes(t *testing.T) {
	queue := newOutboundQueue(2, 5)
	if err := queue.enqueue(outboundFrame{kind: frameMessage, payload: []byte("123")}); err != nil {
		t.Fatal(err)
	}
	if err := queue.enqueue(outboundFrame{kind: frameMessage, payload: []byte("12")}); err != nil {
		t.Fatal(err)
	}
	if queue.bytesQueued() != 5 {
		t.Fatalf("queued bytes=%d, want 5", queue.bytesQueued())
	}
	if err := queue.enqueue(outboundFrame{kind: frameMessage, payload: []byte("1")}); !errors.Is(err, ErrOutboundQueueFull) {
		t.Fatalf("byte overflow=%v, want ErrOutboundQueueFull", err)
	}
	frame, ok := queue.next(context.Background())
	if !ok || string(frame.payload) != "123" {
		t.Fatalf("first dequeue=%q/%v", frame.payload, ok)
	}
	queue.discardAndClose()
	if err := queue.enqueue(outboundFrame{payload: []byte("x")}); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("enqueue after close=%v, want ErrConnectionClosed", err)
	}
}

func TestConnectionQueueOverflowTerminatesWithoutBlockingProducer(t *testing.T) {
	gateway, err := New(Options{Limits: Limits{MaxMessageBytes: 1024, MaxOutboundMessages: 1, MaxOutboundBytes: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	connection := newConnection(gateway, nil, TicketClaims{}, "conn-test", nil)
	message := protocol.ErrorMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeError, ID: "one"},
		Payload:  protocol.ErrorPayload{Code: "fake", Message: "one"},
	}
	if err := connection.Send(message); err != nil {
		t.Fatal(err)
	}
	if err := connection.Send(message); !errors.Is(err, ErrOutboundQueueFull) {
		t.Fatalf("second send=%v, want ErrOutboundQueueFull", err)
	}
	if !connection.isTerminated() {
		t.Fatal("queue overflow did not terminate the connection")
	}
	code, reason := connection.closeDetails()
	if code != websocket.StatusTryAgainLater || reason != closeReasonQueueOverflow {
		t.Fatalf("close=%v/%q, want 1013/queue overflow", code, reason)
	}
	if connection.queue.bytesQueued() != 0 {
		t.Fatalf("overflow left queued bytes=%d", connection.queue.bytesQueued())
	}
}

func TestGatewayHelloWelcomeAndFakeHandlerUsesSingleWriter(t *testing.T) {
	observer := newRecordingObserver()
	const total = 48
	handler := HandlerFunc(func(_ context.Context, connection Connection, message protocol.Message) error {
		if message.Kind() != protocol.MessageTypeCommand {
			return fmt.Errorf("unexpected message %s", message.Kind())
		}
		var wg sync.WaitGroup
		wg.Add(total)
		for i := 0; i < total; i++ {
			go func(i int) {
				defer wg.Done()
				response := protocol.ErrorMessage{
					Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeError, ID: fmt.Sprintf("error-%d", i)},
					Payload:  protocol.ErrorPayload{Code: "fake", Message: "fake handler response"},
				}
				if err := connection.Send(response); err != nil {
					t.Errorf("handler Send: %v", err)
				}
			}(i)
		}
		wg.Wait()
		return nil
	})
	endpoint := newTestEndpoint(t, Options{Handler: handler, Observer: observer})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	writeHello(t, connection, "tab-1")
	welcome := readProtocol(t, connection)
	welcomeMessage, ok := welcome.(protocol.WelcomeMessage)
	if !ok || welcomeMessage.Payload.SelectedVersion != 1 || welcomeMessage.Payload.ConnectionID == "" || welcomeMessage.Payload.ServerEpoch == "" || welcomeMessage.Payload.MaxMessageBytes != DefaultMaxMessageBytes {
		t.Fatalf("welcome=%#v", welcome)
	}
	command := protocol.CommandMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "command-1"},
		Payload:  protocol.CommandPayload{Name: "fake.command", SchemaVersion: 1, RequestID: "request-1", Arguments: json.RawMessage(`{}`)},
	}
	payload, err := protocol.EncodeMessage(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		message := readProtocol(t, connection)
		errorMessage, ok := message.(protocol.ErrorMessage)
		if !ok || errorMessage.Payload.Code != "fake" {
			t.Fatalf("handler response=%#v", message)
		}
		seen[errorMessage.ID] = true
	}
	if len(seen) != total {
		t.Fatalf("unique handler responses=%d, want %d", len(seen), total)
	}
	var welcomeObserved, commandObserved bool
	for _, event := range observer.Events() {
		if event.Direction == DirectionOutbound && event.MessageType == protocol.MessageTypeWelcome {
			welcomeObserved = true
		}
		if event.Direction == DirectionInbound && event.MessageType == protocol.MessageTypeCommand {
			commandObserved = true
		}
	}
	if !welcomeObserved || !commandObserved {
		t.Fatal("observer did not see handshake and command")
	}
}

func TestGatewayProtocolErrorsAndUnsupportedHandler(t *testing.T) {
	cases := []struct {
		name      string
		write     func(*testing.T, *websocket.Conn)
		wantCode  string
		wantClose websocket.StatusCode
	}{
		{"unknown version", func(t *testing.T, c *websocket.Conn) {
			_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"version":2,"type":"hello","id":"h","payload":{"supported_versions":[1],"client_id":"tab"}}`))
		}, "unsupported_version", websocket.StatusProtocolError},
		{"unknown type", func(t *testing.T, c *websocket.Conn) {
			_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"version":1,"type":"nope","id":"h","payload":{}}`))
		}, "unknown_type", websocket.StatusProtocolError},
		{"binary frame", func(t *testing.T, c *websocket.Conn) {
			_ = c.Write(context.Background(), websocket.MessageBinary, []byte(`{}`))
		}, "invalid_message", websocket.StatusProtocolError},
		{"not hello", func(t *testing.T, c *websocket.Conn) {
			ping := protocol.PingMessage{Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypePing, ID: "ping-1"}, Payload: protocol.PingPayload{}}
			payload, err := protocol.EncodeMessage(ping)
			if err != nil {
				t.Fatal(err)
			}
			_ = c.Write(context.Background(), websocket.MessageText, payload)
		}, "handshake_required", websocket.StatusProtocolError},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			endpoint := newTestEndpoint(t, Options{Observer: newRecordingObserver()})
			connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
			defer connection.Close(websocket.StatusNormalClosure, "test done")
			test.write(t, connection)
			message := readProtocol(t, connection)
			errorMessage, ok := message.(protocol.ErrorMessage)
			if !ok || errorMessage.Payload.Code != test.wantCode {
				t.Fatalf("protocol error=%#v", message)
			}
			if got := readClose(t, connection); got != test.wantClose {
				t.Fatalf("close code=%v, want %v", got, test.wantClose)
			}
		})
	}

	t.Run("valid message without handler", func(t *testing.T) {
		endpoint := newTestEndpoint(t, Options{Observer: newRecordingObserver()})
		connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
		defer connection.Close(websocket.StatusNormalClosure, "test done")
		writeHello(t, connection, "tab")
		_ = readProtocol(t, connection)
		writeProtocol(t, connection, protocol.CommandMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeCommand, ID: "command-unsupported"},
			Payload: protocol.CommandPayload{
				Name:          "fake.unsupported",
				SchemaVersion: 1,
				RequestID:     "request-unsupported",
				Arguments:     []byte(`{}`),
			},
		})
		message := readProtocol(t, connection)
		errorMessage, ok := message.(protocol.ErrorMessage)
		if !ok || errorMessage.Payload.Code != "unsupported_message" {
			t.Fatalf("unsupported response=%#v", message)
		}
		if got := readClose(t, connection); got != websocket.StatusUnsupportedData {
			t.Fatalf("close code=%v, want unsupported data", got)
		}
	})
}

func TestGatewayDirectionGateRejectsServerMessagesBeforeHandler(t *testing.T) {
	cases := []struct {
		name  string
		write func(*testing.T, *websocket.Conn)
	}{
		{
			name: "duplicate hello",
			write: func(t *testing.T, connection *websocket.Conn) {
				writeHello(t, connection, "duplicate")
			},
		},
		{
			name: "inbound welcome",
			write: func(t *testing.T, connection *websocket.Conn) {
				writeProtocol(t, connection, protocol.WelcomeMessage{
					Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeWelcome, ID: "welcome-client"},
					Payload: protocol.WelcomePayload{
						SelectedVersion:     1,
						ConnectionID:        "server-connection",
						ServerEpoch:         "server-epoch",
						HeartbeatIntervalMS: 15000,
						MaxMessageBytes:     DefaultMaxMessageBytes,
					},
				})
			},
		},
		{
			name: "inbound debug execute",
			write: func(t *testing.T, connection *websocket.Conn) {
				writeProtocol(t, connection, protocol.DebugExecuteMessage{
					Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugExecute, ID: "execute-client"},
					Payload: protocol.DebugExecutionPayload{
						ExecutionID: "execution-1", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1",
						Code: "1 + 1", TimeoutMS: 500,
					},
				})
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			calls := make(chan protocol.Message, 1)
			handler := HandlerFunc(func(_ context.Context, _ Connection, message protocol.Message) error {
				calls <- message
				return nil
			})
			endpoint := newTestEndpoint(t, Options{Handler: handler})
			connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
			defer connection.Close(websocket.StatusNormalClosure, "test done")
			writeHello(t, connection, "tab")
			if _, ok := readProtocol(t, connection).(protocol.WelcomeMessage); !ok {
				t.Fatal("handshake did not return welcome")
			}
			test.write(t, connection)
			message := readProtocol(t, connection)
			errorMessage, ok := message.(protocol.ErrorMessage)
			if !ok || errorMessage.Payload.Code != "unsupported_message" {
				t.Fatalf("direction-gate response=%#v", message)
			}
			if got := readClose(t, connection); got != websocket.StatusUnsupportedData {
				t.Fatalf("close code=%v, want unsupported data", got)
			}
			select {
			case called := <-calls:
				t.Fatalf("server-directed message reached handler: %#v", called)
			default:
			}
		})
	}
}

func TestGatewayDirectionGateAllowsExecutionResults(t *testing.T) {
	calls := make(chan protocol.Message, 1)
	endpoint := newTestEndpoint(t, Options{Handler: HandlerFunc(func(_ context.Context, _ Connection, message protocol.Message) error {
		calls <- message
		return nil
	})})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	writeHello(t, connection, "tab")
	if _, ok := readProtocol(t, connection).(protocol.WelcomeMessage); !ok {
		t.Fatal("handshake did not return welcome")
	}
	writeProtocol(t, connection, protocol.DebugExecutionResultMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeDebugExecutionResult, ID: "result-client"},
		Payload: protocol.DebugExecutionResultPayload{
			ExecutionID: "execution-1", PageID: "page-1", PageEpoch: "epoch-1", SessionID: "session-1",
			Status: protocol.DebugExecutionStatusFailed,
			Error:  &protocol.DebugExecutionError{Code: "web_debug_execution_error", Message: "boom"},
		},
	})
	select {
	case message := <-calls:
		if message.Kind() != protocol.MessageTypeDebugExecutionResult {
			t.Fatalf("handler message = %s, want execution result", message.Kind())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("execution result did not reach handler")
	}
}

func TestGatewayHeartbeatUsesV1PingPongAndNilHandler(t *testing.T) {
	clock := newManualClock()
	observer := newRecordingObserver()
	endpoint := newTestEndpoint(t, Options{Clock: clock, Observer: observer, Limits: Limits{HeartbeatInterval: 15 * time.Second, PongTimeout: 45 * time.Second}})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	writeHello(t, connection, "tab")
	_ = readProtocol(t, connection)
	clock.WaitForTickers(t, 2)

	clock.Advance(15 * time.Second)
	ping := readProtocol(t, connection)
	if pingMessage, ok := ping.(protocol.PingMessage); !ok || pingMessage.Type != protocol.MessageTypePing {
		t.Fatalf("heartbeat message=%#v, want a decodable protocol ping", ping)
	}
	writeProtocol(t, connection, protocol.PongMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypePong, ID: "pong-1"},
		Payload:  protocol.PongPayload{},
	})
	observer.waitForMessage(t, DirectionInbound, protocol.MessageTypePong)

	// The Pong was handled by the transport even though no business handler is
	// installed. It extends liveness beyond the original 45-second deadline.
	clock.Advance(30 * time.Second)
	if next := readProtocol(t, connection); next.Kind() != protocol.MessageTypePing {
		t.Fatalf("second heartbeat=%s, want ping", next.Kind())
	}
	writeProtocol(t, connection, protocol.PongMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypePong, ID: "pong-2"},
		Payload:  protocol.PongPayload{},
	})
	observer.waitForMessage(t, DirectionInbound, protocol.MessageTypePong)
	clock.Advance(44 * time.Second)
	if next := readProtocol(t, connection); next.Kind() != protocol.MessageTypePing {
		t.Fatalf("third heartbeat=%s, want ping before timeout", next.Kind())
	}
	clock.Advance(time.Second)
	if got := readClose(t, connection); got != websocket.StatusPolicyViolation {
		t.Fatalf("heartbeat close code=%v, want policy violation", got)
	}
	select {
	case <-observer.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("connection closed event was not observed")
	}
}

func TestGatewayHeartbeatTimeoutUsesInjectedClock(t *testing.T) {
	clock := newManualClock()
	endpoint := newTestEndpoint(t, Options{Clock: clock, Limits: Limits{HeartbeatInterval: 15 * time.Second, PongTimeout: 45 * time.Second}})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	writeHello(t, connection, "tab")
	_ = readProtocol(t, connection)
	clock.WaitForTickers(t, 2)
	clock.Advance(15 * time.Second)
	if message := readProtocol(t, connection); message.Kind() != protocol.MessageTypePing {
		t.Fatalf("heartbeat message=%s, want ping", message.Kind())
	}
	// No application Pong is sent. The next interval reaches the 45-second
	// timeout and closes the connection.
	clock.Advance(30 * time.Second)
	if got := readClose(t, connection); got != websocket.StatusPolicyViolation {
		t.Fatalf("heartbeat close code=%v, want policy violation", got)
	}
}

func TestGatewayCleanShutdownCancelsAllConnectionGoroutines(t *testing.T) {
	observer := newRecordingObserver()
	endpoint := newTestEndpoint(t, Options{Observer: observer})
	connection := dialTest(t, endpoint, issueTestTicket(t, endpoint))
	writeHello(t, connection, "tab")
	_ = readProtocol(t, connection)
	endpoint.cancel()
	if got := readClose(t, connection); got != websocket.StatusGoingAway {
		t.Fatalf("shutdown close code=%v, want going away", got)
	}
	select {
	case <-observer.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("connection did not finish shutdown")
	}
	_ = connection.Close(websocket.StatusNormalClosure, "test done")
}

func TestGatewayRejectsInvalidOriginBeforeTicketConsumption(t *testing.T) {
	endpoint := newTestEndpoint(t, Options{Observer: newRecordingObserver()})
	ticket := issueTestTicket(t, endpoint)
	url := "ws" + strings.TrimPrefix(endpoint.server.URL, "http") + "/api/ws?ticket=" + ticket
	header := http.Header{}
	header.Set("Origin", "http://attacker.invalid")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("invalid origin dial err=%v response=%v", err, response)
	}
	connection := dialTest(t, endpoint, ticket)
	defer connection.Close(websocket.StatusNormalClosure, "test done")
	writeHello(t, connection, "tab")
	_ = readProtocol(t, connection)
}
