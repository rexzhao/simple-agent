package wsgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/rexzhao/simple-agent/internal/protocol"
)

const (
	DefaultMaxMessageBytes      = 256 * 1024
	DefaultMaxOutboundMessages  = 1024
	DefaultMaxOutboundBytes     = 8 * 1024 * 1024
	DefaultHeartbeatInterval    = 15 * time.Second
	DefaultPongTimeout          = 45 * time.Second
	DefaultHandshakeTimeout     = 10 * time.Second
	writerShutdownGracePeriod   = time.Second
	closeReasonProtocol         = "protocol error"
	closeReasonUnsupported      = "unsupported message"
	closeReasonQueueOverflow    = "outbound queue overflow"
	closeReasonHeartbeatTimeout = "heartbeat timeout"
	closeReasonHandshakeTimeout = "handshake timeout"
	closeReasonInternal         = "connection failure"
)

var (
	ErrConnectionClosed   = errors.New("websocket connection is closed")
	ErrOutboundQueueFull  = errors.New("websocket outbound queue is full")
	ErrMessageTooLarge    = errors.New("websocket message exceeds the configured limit")
	ErrUnsupportedMessage = errors.New("websocket message is unsupported")
)

// Limits are intentionally finite defaults. They bound both memory retained by
// a connection and the amount of work a slow peer can cause.
type Limits struct {
	MaxMessageBytes     int
	MaxOutboundMessages int
	MaxOutboundBytes    int
	HeartbeatInterval   time.Duration
	PongTimeout         time.Duration
	HandshakeTimeout    time.Duration
}

func (l Limits) withDefaults() (Limits, error) {
	if l.MaxMessageBytes == 0 {
		l.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if l.MaxOutboundMessages == 0 {
		l.MaxOutboundMessages = DefaultMaxOutboundMessages
	}
	if l.MaxOutboundBytes == 0 {
		l.MaxOutboundBytes = DefaultMaxOutboundBytes
	}
	if l.HeartbeatInterval == 0 {
		l.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if l.PongTimeout == 0 {
		l.PongTimeout = DefaultPongTimeout
	}
	if l.HandshakeTimeout == 0 {
		l.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if l.MaxMessageBytes < 1 || l.MaxOutboundMessages < 1 || l.MaxOutboundBytes < 1 || l.HeartbeatInterval <= 0 || l.PongTimeout <= 0 || l.HandshakeTimeout <= 0 {
		return Limits{}, fmt.Errorf("websocket limits must be positive")
	}
	if l.MaxOutboundBytes < l.MaxMessageBytes {
		return Limits{}, fmt.Errorf("outbound queue byte limit must fit one message")
	}
	return l, nil
}

// Clock is injectable so heartbeat lifecycle tests do not need wall-clock
// sleeps. Production uses RealClock.
type Clock interface {
	Now() time.Time
	NewTicker(time.Duration) Ticker
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }
func (RealClock) NewTicker(interval time.Duration) Ticker {
	return realTicker{ticker: time.NewTicker(interval)}
}

type realTicker struct{ ticker *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.ticker.C }
func (t realTicker) Stop()               { t.ticker.Stop() }

// ConnectionInfo contains only non-secret connection metadata. In particular,
// it intentionally has no capability or ticket field.
type ConnectionInfo struct {
	ConnectionID string
	ClientID     string
	Principal    string
}

// Connection is the transport-neutral outbound hook for later command and
// subscription dispatchers. Send copies the encoded message into a bounded
// queue before returning.
type Connection interface {
	Send(protocol.Message) error
	Info() ConnectionInfo
}

type Handler interface {
	// Handle processes one validated client-to-server message. Implementations
	// must return when ctx is cancelled. The gateway can cancel this context
	// during shutdown or connection termination, but cannot reclaim a handler
	// that ignores cancellation forever.
	Handle(context.Context, Connection, protocol.Message) error
}

type HandlerFunc func(context.Context, Connection, protocol.Message) error

func (f HandlerFunc) Handle(ctx context.Context, connection Connection, message protocol.Message) error {
	return f(ctx, connection, message)
}

// Event is a safe structured observation hook. It contains labels and sizes,
// never raw payloads, capability tokens, tickets, or provider secrets.
type Event struct {
	Kind         EventKind
	Direction    Direction
	MessageType  protocol.MessageType
	Bytes        int
	QueueBytes   int
	Code         string
	Reason       string
	ConnectionID string
	ClientID     string
}

type EventKind string

const (
	EventConnectionOpened EventKind = "connection_opened"
	EventConnectionClosed EventKind = "connection_closed"
	EventMessage          EventKind = "message"
	EventProtocolError    EventKind = "protocol_error"
	EventQueueChanged     EventKind = "queue_changed"
)

type Direction string

const (
	DirectionInbound  Direction = "inbound"
	DirectionOutbound Direction = "outbound"
)

type Observer interface {
	Observe(Event)
}

type Options struct {
	Limits      Limits
	Clock       Clock
	ServerEpoch string
	Handler     Handler
	Observer    Observer
	IDGenerator func(prefix string) (string, error)
}

// Gateway owns the WebSocket transport but exposes only protocol messages and
// Connection to its handler. No third-party websocket type crosses this API.
type Gateway struct {
	limits      Limits
	clock       Clock
	serverEpoch string
	handler     Handler
	observer    Observer
	idGenerator func(prefix string) (string, error)
	observerMu  sync.Mutex
}

func New(options Options) (*Gateway, error) {
	limits, err := options.Limits.withDefaults()
	if err != nil {
		return nil, err
	}
	clock := options.Clock
	if clock == nil {
		clock = RealClock{}
	}
	idGenerator := options.IDGenerator
	if idGenerator == nil {
		idGenerator = secureID
	}
	epoch := strings.TrimSpace(options.ServerEpoch)
	if epoch == "" {
		epoch, err = idGenerator("server_epoch")
		if err != nil {
			return nil, fmt.Errorf("generate websocket server epoch: %w", err)
		}
	}
	return &Gateway{
		limits:      limits,
		clock:       clock,
		serverEpoch: epoch,
		handler:     options.Handler,
		observer:    options.Observer,
		idGenerator: idGenerator,
	}, nil
}

func secureID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(raw), nil
}

// Serve accepts an already authenticated, already consumed ticket. Ticket
// consumption deliberately lives in the HTTP boundary so it occurs before
// websocket.Accept upgrades the request.
func (g *Gateway) HTTPHandler(ctx context.Context, w http.ResponseWriter, r *http.Request, claims TicketClaims) {
	if g == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	connectionID, err := g.idGenerator("conn")
	if err != nil {
		writeHandshakeFailure(w, "internal_error")
		return
	}
	var lastPong atomic.Int64
	lastPong.Store(g.clock.Now().UnixNano())
	acceptOptions := &websocket.AcceptOptions{
		// OriginPatterns takes host patterns, not URI origins. The outer HTTP
		// boundary still requires the exact http://<Host> Origin before ticket
		// consumption; this is only a same-host defense in depth check.
		OriginPatterns: []string{r.Host},
	}
	// Deliberately do not use the library's RFC6455 Pong callback for product
	// liveness. Only a validated V1 protocol Pong can satisfy our heartbeat.
	ws, err := websocket.Accept(w, r, acceptOptions)
	if err != nil {
		return
	}
	ws.SetReadLimit(int64(g.limits.MaxMessageBytes))
	connection := newConnection(g, ws, claims, connectionID, &lastPong)
	connection.run(ctx)
}

func writeHandshakeFailure(w http.ResponseWriter, _ string) {
	// The caller has not upgraded the request. Do not include any request URL
	// data in this response; in particular, the ticket is never echoed.
	w.WriteHeader(http.StatusInternalServerError)
}

// connection is the sole owner of lifecycle state. There is exactly one
// writer goroutine; all application and heartbeat frames enter outboundQueue.
type connection struct {
	gateway  *Gateway
	ws       *websocket.Conn
	info     ConnectionInfo
	lastPong *atomic.Int64

	queue *outboundQueue
	clock Clock

	mu            sync.Mutex
	terminated    bool
	closeCode     websocket.StatusCode
	closeWhy      string
	done          chan struct{}
	terminateOnce sync.Once
	handlerCancel context.CancelFunc
	handshake     chan struct{}
}

func newConnection(gateway *Gateway, ws *websocket.Conn, claims TicketClaims, connectionID string, lastPong *atomic.Int64) *connection {
	connection := &connection{
		gateway:   gateway,
		ws:        ws,
		info:      ConnectionInfo{ConnectionID: connectionID, Principal: claims.Principal},
		lastPong:  lastPong,
		queue:     newOutboundQueue(gateway.limits.MaxOutboundMessages, gateway.limits.MaxOutboundBytes),
		clock:     gateway.clock,
		done:      make(chan struct{}),
		handshake: make(chan struct{}),
	}
	if connection.lastPong == nil {
		connection.lastPong = &atomic.Int64{}
		connection.lastPong.Store(gateway.clock.Now().UnixNano())
	}
	return connection
}

func (c *connection) Info() ConnectionInfo {
	return c.infoSnapshot()
}

func (c *connection) infoSnapshot() ConnectionInfo {
	if c == nil {
		return ConnectionInfo{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

func (c *connection) setClientID(clientID string) {
	c.mu.Lock()
	c.info.ClientID = clientID
	c.mu.Unlock()
}

func (c *connection) Send(message protocol.Message) error {
	if c == nil || message == nil {
		return ErrConnectionClosed
	}
	payload, err := protocol.EncodeMessage(message)
	if err != nil {
		return err
	}
	if len(payload) > c.gateway.limits.MaxMessageBytes {
		c.terminate(websocket.StatusMessageTooBig, "outbound message too large", nil)
		return ErrMessageTooLarge
	}
	if err := c.queue.enqueue(outboundFrame{kind: frameMessage, payload: payload}); err != nil {
		if errors.Is(err, ErrOutboundQueueFull) {
			c.terminate(websocket.StatusTryAgainLater, closeReasonQueueOverflow, nil)
		}
		return err
	}
	c.gateway.observe(Event{Kind: EventQueueChanged, QueueBytes: c.queue.bytesQueued(), ConnectionID: c.infoSnapshot().ConnectionID, ClientID: c.infoSnapshot().ClientID})
	return nil
}

func (c *connection) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	handlerCtx, handlerCancel := context.WithCancel(ctx)
	c.handlerCancel = handlerCancel
	defer handlerCancel()

	c.gateway.observe(Event{Kind: EventConnectionOpened, ConnectionID: c.infoSnapshot().ConnectionID, ClientID: c.infoSnapshot().ClientID})

	writerDone := make(chan struct{})
	go c.writer(writerDone)
	handshakeTimeoutDone := make(chan struct{})
	go c.handshakeTimeout(ctx, handshakeTimeoutDone)
	readerDone := make(chan struct{})
	// Read uses the socket close handshake for lifecycle cancellation. The
	// websocket implementation closes the underlying socket when a Read
	// context is canceled, which would discard the status code the writer is
	// about to send.
	go c.reader(context.Background(), handlerCtx, readerDone)
	heartbeatDone := make(chan struct{})
	go c.heartbeat(ctx, heartbeatDone)
	contextDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.terminate(websocket.StatusGoingAway, "server shutdown", nil)
		case <-c.done:
		}
		close(contextDone)
	}()

	<-c.done
	cancel()
	handlerCancel()
	c.queue.closeAfterDrain()
	select {
	case <-writerDone:
	case <-time.After(writerShutdownGracePeriod):
		// A blocked Write must not keep the HTTP handler or its goroutine set
		// alive forever. CloseNow is only the final unblock path; normal close
		// frames are always emitted by the writer goroutine.
		_ = c.ws.CloseNow()
	}
	<-readerDone
	<-heartbeatDone
	<-contextDone
	<-handshakeTimeoutDone
	closeCode, closeWhy := c.closeDetails()
	if closeWhy == "" {
		closeWhy = closeReasonInternal
	}
	c.gateway.observe(Event{Kind: EventConnectionClosed, Code: fmt.Sprintf("%d", closeCode), Reason: closeWhy, ConnectionID: c.infoSnapshot().ConnectionID, ClientID: c.infoSnapshot().ClientID})
}

func (c *connection) handshakeTimeout(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := c.clock.NewTicker(c.gateway.limits.HandshakeTimeout)
	defer ticker.Stop()
	select {
	case <-c.handshake:
		return
	case <-ctx.Done():
		return
	case <-ticker.C():
		// Prefer an already-observed shutdown or successful handshake when
		// both become ready at the same clock boundary.
		select {
		case <-c.handshake:
			return
		case <-ctx.Done():
			return
		default:
		}
		c.gateway.observe(Event{Kind: EventProtocolError, Code: "handshake_timeout", ConnectionID: c.infoSnapshot().ConnectionID, ClientID: c.infoSnapshot().ClientID})
		c.terminate(websocket.StatusProtocolError, closeReasonHandshakeTimeout, nil)
	}
}

func (c *connection) reader(ctx, handlerCtx context.Context, done chan<- struct{}) {
	defer close(done)
	first := true
	for {
		messageType, payload, err := c.ws.Read(ctx)
		if err != nil {
			if c.isTerminated() {
				return
			}
			if websocket.CloseStatus(err) == websocket.StatusMessageTooBig {
				c.protocolFailure("message_too_large", websocket.StatusMessageTooBig, "message too large")
				return
			}
			if closeCode := websocket.CloseStatus(err); closeCode >= websocket.StatusNormalClosure && closeCode <= websocket.StatusTLSHandshake && closeCode != websocket.StatusNoStatusRcvd && closeCode != websocket.StatusAbnormalClosure && closeCode != websocket.StatusTLSHandshake {
				c.terminate(closeCode, "peer closed", nil)
				return
			}
			c.terminate(websocket.StatusInternalError, closeReasonInternal, nil)
			return
		}
		if messageType != websocket.MessageText {
			c.protocolFailure("invalid_message", websocket.StatusProtocolError, closeReasonProtocol)
			return
		}
		if len(payload) > c.gateway.limits.MaxMessageBytes {
			c.protocolFailure("message_too_large", websocket.StatusMessageTooBig, "message too large")
			return
		}
		message, err := protocol.DecodeMessage(payload)
		if err != nil {
			c.protocolFailure(protocolErrorCode(err), websocket.StatusProtocolError, closeReasonProtocol)
			return
		}
		if message.Kind() == protocol.MessageTypePong {
			// Record liveness before publishing the observation so tests and
			// metrics consumers cannot observe a Pong before it takes effect.
			c.recordProtocolPong()
		}
		c.gateway.observe(Event{Kind: EventMessage, Direction: DirectionInbound, MessageType: message.Kind(), Bytes: len(payload), ConnectionID: c.infoSnapshot().ConnectionID, ClientID: c.infoSnapshot().ClientID})

		if first {
			first = false
			hello, ok := message.(protocol.HelloMessage)
			if !ok {
				c.protocolFailure("handshake_required", websocket.StatusProtocolError, closeReasonProtocol)
				return
			}
			c.setClientID(hello.Payload.ClientID)
			if err := c.sendWelcome(hello); err != nil {
				c.terminate(websocket.StatusInternalError, closeReasonInternal, nil)
				return
			}
			close(c.handshake)
			continue
		}

		if !isClientMessage(message.Kind()) {
			c.unsupportedMessage()
			return
		}

		switch message.Kind() {
		case protocol.MessageTypePing:
			if err := c.sendPong(); err != nil {
				c.terminate(websocket.StatusInternalError, closeReasonInternal, nil)
				return
			}
		case protocol.MessageTypePong:
			// Product heartbeat is an application-level V1 message. Do not
			// dispatch it to business code, even when a handler is absent.
			// Liveness was recorded immediately after validation above.
		default:
			if c.gateway.handler == nil {
				c.unsupportedMessage()
				return
			}
			if err := c.gateway.handler.Handle(handlerCtx, c, message); err != nil {
				c.handleHandlerError(err)
				return
			}
		}
	}
}

// isClientMessage is the V1 direction gate. Messages emitted by the server
// are never allowed to enter the business handler as if they were client
// requests.
func isClientMessage(messageType protocol.MessageType) bool {
	switch messageType {
	case protocol.MessageTypePing, protocol.MessageTypePong,
		protocol.MessageTypeCommand, protocol.MessageTypeSubscribe,
		protocol.MessageTypeUnsubscribe, protocol.MessageTypeAck:
		return true
	default:
		return false
	}
}

func (c *connection) sendWelcome(_ protocol.HelloMessage) error {
	id, err := c.gateway.idGenerator("welcome")
	if err != nil {
		return err
	}
	welcome := protocol.WelcomeMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeWelcome, ID: id},
		Payload: protocol.WelcomePayload{
			SelectedVersion:     1,
			ConnectionID:        c.infoSnapshot().ConnectionID,
			ServerEpoch:         c.gateway.serverEpoch,
			HeartbeatIntervalMS: int(c.gateway.limits.HeartbeatInterval / time.Millisecond),
			MaxMessageBytes:     c.gateway.limits.MaxMessageBytes,
		},
	}
	return c.Send(welcome)
}

func (c *connection) sendPong() error {
	id, err := c.gateway.idGenerator("pong")
	if err != nil {
		return err
	}
	return c.Send(protocol.PongMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypePong, ID: id},
		Payload:  protocol.PongPayload{},
	})
}

func (c *connection) sendPing() error {
	id, err := c.gateway.idGenerator("ping")
	if err != nil {
		return err
	}
	return c.Send(protocol.PingMessage{
		Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypePing, ID: id},
		Payload:  protocol.PingPayload{},
	})
}

func (c *connection) recordProtocolPong() {
	if c == nil || c.lastPong == nil {
		return
	}
	c.lastPong.Store(c.clock.Now().UnixNano())
}

func (c *connection) heartbeat(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	select {
	case <-c.handshake:
	case <-ctx.Done():
		return
	}
	ticker := c.clock.NewTicker(c.gateway.limits.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
			last := time.Unix(0, c.lastPong.Load())
			if c.clock.Now().Sub(last) >= c.gateway.limits.PongTimeout {
				c.terminate(websocket.StatusPolicyViolation, closeReasonHeartbeatTimeout, nil)
				return
			}
			// Heartbeat is a V1 text message, not an RFC6455 control Ping.
			// Send uses the same bounded queue as every other outbound message.
			if err := c.sendPing(); err != nil {
				if !errors.Is(err, ErrOutboundQueueFull) {
					c.terminate(websocket.StatusInternalError, closeReasonInternal, nil)
				}
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *connection) writer(done chan<- struct{}) {
	defer close(done)
	for {
		frame, ok := c.queue.next(context.Background())
		if !ok {
			code, reason := c.closeDetails()
			_ = c.ws.Close(code, reason)
			return
		}
		c.gateway.observe(Event{Kind: EventQueueChanged, QueueBytes: c.queue.bytesQueued(), ConnectionID: c.infoSnapshot().ConnectionID, ClientID: c.infoSnapshot().ClientID})
		var err error
		switch frame.kind {
		case frameMessage:
			err = c.ws.Write(context.Background(), websocket.MessageText, frame.payload)
			if err == nil {
				var envelope protocol.Envelope
				if json.Unmarshal(frame.payload, &envelope) == nil {
					c.gateway.observe(Event{Kind: EventMessage, Direction: DirectionOutbound, MessageType: envelope.Type, Bytes: len(frame.payload), ConnectionID: c.infoSnapshot().ConnectionID, ClientID: c.infoSnapshot().ClientID})
				}
			}
		}
		if err != nil {
			c.terminate(websocket.StatusInternalError, closeReasonInternal, nil)
			return
		}
	}
}

func (c *connection) protocolFailure(code string, closeCode websocket.StatusCode, message string) {
	c.gateway.observe(Event{Kind: EventProtocolError, Code: code, ConnectionID: c.infoSnapshot().ConnectionID, ClientID: c.infoSnapshot().ClientID})
	id, err := c.gateway.idGenerator("error")
	if err == nil {
		errorMessage := protocol.ErrorMessage{
			Envelope: protocol.Envelope{Version: 1, Type: protocol.MessageTypeError, ID: id},
			Payload:  protocol.ErrorPayload{Code: code, Message: message},
		}
		if payload, encodeErr := protocol.EncodeMessage(errorMessage); encodeErr == nil && len(payload) <= c.gateway.limits.MaxMessageBytes {
			c.terminate(closeCode, message, payload)
			return
		}
	}
	c.terminate(closeCode, message, nil)
}

func (c *connection) unsupportedMessage() {
	c.protocolFailure("unsupported_message", websocket.StatusUnsupportedData, closeReasonUnsupported)
}

func (c *connection) handleHandlerError(err error) {
	if errors.Is(err, ErrUnsupportedMessage) {
		c.unsupportedMessage()
		return
	}
	c.protocolFailure("handler_error", websocket.StatusInternalError, "message handler failed")
}

func protocolErrorCode(err error) string {
	var protocolErr *protocol.ProtocolError
	if errors.As(err, &protocolErr) {
		return string(protocolErr.Code)
	}
	return "invalid_message"
}

func (c *connection) terminate(code websocket.StatusCode, reason string, errorPayload []byte) {
	c.terminateOnce.Do(func() {
		c.mu.Lock()
		c.terminated = true
		c.closeCode = code
		c.closeWhy = reason
		c.mu.Unlock()
		if errorPayload != nil {
			c.queue.replaceAndClose(outboundFrame{kind: frameMessage, payload: errorPayload})
		} else {
			c.queue.discardAndClose()
		}
		if c.handlerCancel != nil {
			c.handlerCancel()
		}
		close(c.done)
	})
}

func (c *connection) isTerminated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminated
}

func (c *connection) closeDetails() (websocket.StatusCode, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeCode, c.closeWhy
}

func (g *Gateway) observe(event Event) {
	if g == nil || g.observer == nil {
		return
	}
	g.observerMu.Lock()
	defer g.observerMu.Unlock()
	g.observer.Observe(event)
}

func (g *Gateway) Limits() Limits {
	if g == nil {
		return Limits{}
	}
	return g.limits
}

type frameKind uint8

const (
	frameMessage frameKind = iota
)

type outboundFrame struct {
	kind    frameKind
	payload []byte
}

type outboundQueue struct {
	mu          sync.Mutex
	items       []outboundFrame
	bytes       int
	maxMessages int
	maxBytes    int
	closed      bool
	notify      chan struct{}
}

func newOutboundQueue(maxMessages, maxBytes int) *outboundQueue {
	return &outboundQueue{maxMessages: maxMessages, maxBytes: maxBytes, notify: make(chan struct{})}
}

func (q *outboundQueue) enqueue(frame outboundFrame) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrConnectionClosed
	}
	if len(q.items) >= q.maxMessages || q.bytes+len(frame.payload) > q.maxBytes {
		return ErrOutboundQueueFull
	}
	frame.payload = append([]byte(nil), frame.payload...)
	q.items = append(q.items, frame)
	q.bytes += len(frame.payload)
	q.signalLocked()
	return nil
}

func (q *outboundQueue) next(ctx context.Context) (outboundFrame, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			frame := q.items[0]
			q.items = q.items[1:]
			q.bytes -= len(frame.payload)
			q.signalLocked()
			q.mu.Unlock()
			return frame, true
		}
		if q.closed {
			q.mu.Unlock()
			return outboundFrame{}, false
		}
		notify := q.notify
		q.mu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return outboundFrame{}, false
		}
	}
}

func (q *outboundQueue) discardAndClose() {
	q.mu.Lock()
	q.items = nil
	q.bytes = 0
	q.closed = true
	q.signalLocked()
	q.mu.Unlock()
}

func (q *outboundQueue) replaceAndClose(frame outboundFrame) {
	q.mu.Lock()
	q.items = nil
	q.bytes = 0
	if len(frame.payload) <= q.maxBytes && q.maxMessages > 0 {
		frame.payload = append([]byte(nil), frame.payload...)
		q.items = append(q.items, frame)
		q.bytes = len(frame.payload)
	}
	q.closed = true
	q.signalLocked()
	q.mu.Unlock()
}

func (q *outboundQueue) closeAfterDrain() {
	q.mu.Lock()
	q.closed = true
	q.signalLocked()
	q.mu.Unlock()
}

func (q *outboundQueue) bytesQueued() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
}

func (q *outboundQueue) signalLocked() {
	close(q.notify)
	q.notify = make(chan struct{})
}
