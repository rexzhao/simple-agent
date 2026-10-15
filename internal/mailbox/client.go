package mailbox

// The protocol-v2 client core (docs/mailbox.md §2): JSON over the
// frames, rid-correlated request/response with a timeout, notify
// dispatch for the un-rid'd server pushes, and the typed fatal-login
// error. The message set is P1's subset of protocol.ts — each type's
// doc comment cites its TS twin's line range.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// AgentRef identifies a session (protocol.ts:9-13).
type AgentRef struct {
	Project string `json:"project"`
	Session string `json:"session"`
}

// MailRef points at one mail: the triple IS the capability
// (protocol.ts:16-25).
type MailRef struct {
	Project string `json:"project"`
	Session string `json:"session"`
	Mail    string `json:"mail"`
}

// MailRecord is an immutable mail (protocol.ts:32-43). refs is an
// array or JSON null (never missing) on the wire; readAt is null for
// unread.
type MailRecord struct {
	ID        string    `json:"id"`
	From      AgentRef  `json:"from"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Refs      []MailRef `json:"refs"`
	CreatedAt string    `json:"createdAt"`
	ReadAt    *string   `json:"readAt"`
}

// helloMsg (protocol.ts:52-75). sessionDir is deliberately absent —
// docs/mailbox.md §2: sai does not feed the daemon's lifecycle GC.
type helloMsg struct {
	T           string            `json:"t"`
	RID         int               `json:"rid"`
	Project     string            `json:"project"`
	ProjectRoot string            `json:"projectRoot,omitempty"`
	Session     string            `json:"session"`
	Agent       string            `json:"agent"`
	Token       string            `json:"token,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`
}

// welcomeMsg (protocol.ts:139-158).
type welcomeMsg struct {
	T               string            `json:"t"`
	RID             int               `json:"rid"`
	Project         string            `json:"project"`
	Mailbox         string            `json:"mailbox,omitempty"`
	InstanceID      string            `json:"instanceId"`
	ProtocolVersion int               `json:"protocolVersion,omitempty"`
	Meta            map[string]string `json:"meta,omitempty"`
}

// pingMsg (protocol.ts:78-81) / pongMsg (protocol.ts:160-163).
type pingMsg struct {
	T   string `json:"t"`
	RID int    `json:"rid"`
}

type pongMsg struct {
	T        string `json:"t"`
	RID      int    `json:"rid"`
	Project  string `json:"project"`
	Instance string `json:"instanceId"`
}

// inboxMsg (protocol.ts:105-115) / inboxReply (protocol.ts:195-206).
type inboxMsg struct {
	T      string `json:"t"`
	RID    int    `json:"rid"`
	Unread bool   `json:"unreadOnly,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type inboxReply struct {
	T           string       `json:"t"`
	RID         int          `json:"rid"`
	Mails       []MailRecord `json:"mails"`
	NextCursor  string       `json:"nextCursor,omitempty"`
	Total       int          `json:"total"`
	TotalUnread int          `json:"totalUnread"`
}

// readMsg (protocol.ts:125-128).
type readMsg struct {
	T    string  `json:"t"`
	RID  int     `json:"rid"`
	Mail MailRef `json:"mail"`
}

// notifyMsg (protocol.ts:221-225): the un-rid'd server push.
type notifyMsg struct {
	T      string `json:"t"`
	Unread int    `json:"unread"`
}

// errorReply (protocol.ts:211-216).
type errorReply struct {
	T       string `json:"t"`
	RID     int    `json:"rid,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// fatalLoginCodes mirrors client.ts:45 — retrying can never fix
// these, the reconnect loop must stop on them.
var fatalLoginCodes = map[string]bool{
	"mailbox_in_use": true,
	"bad_name":       true,
	"login_failed":   true,
	"name_held":      true,
	"session_bound":  true,
}

// FatalLoginError is a rejection retrying can never fix. Callers
// (the D3 reconnect loop) test with errors.As and stop.
type FatalLoginError struct {
	Code    string
	Message string
}

func (e *FatalLoginError) Error() string {
	return fmt.Sprintf("mailbox: fatal login rejection: %s: %s", e.Code, e.Message)
}

// ErrClosed is returned to callers of a client whose connection is
// gone; in-flight requests fail with it immediately (not on timeout).
var ErrClosed = errors.New("mailbox: connection closed")

// Options tunes the client for tests.
type Options struct {
	// RequestTimeout defaults to 5s (mirroring pi's client); tests
	// shrink it to milliseconds instead of really waiting.
	RequestTimeout time.Duration
	// HandshakeTimeout bounds the dial's upgrade exchange (default
	// 5s). A silent or half-open peer must not hang Dial — the D3
	// reconnect loop would be stuck.
	HandshakeTimeout time.Duration
}

// serverReply is what a pending request resolves with.
type serverReply struct {
	msg json.RawMessage
	err error
}

// Client is one connection: request/response by rid plus the notify
// push channel. Reconnection is the D3 plugin's business — a Client
// is single-connection by design.
type Client struct {
	opts    Options
	ws      *wsConn
	nextRID int

	mu      sync.Mutex
	pending map[int]chan serverReply
	closed  bool

	// Notify carries every un-rid'd server push (the unread count).
	// It is NOT closed on disconnect: the D3 supervisor uses Done()
	// for lifecycle, and a closed Notify would force every consumer
	// to also select on Done (a leaked goroutine per reader
	// otherwise). Pushes stop when the connection dies.
	Notify chan notifyMsg

	// done closes when the connection is gone (any cause); the D3
	// supervisor watches it to reconnect and pull the backlog.
	done chan struct{}
	// err is the first shutdown cause (ErrClosed for a purposeful
	// Close). Written once, before done closes.
	err error
}

// Dial connects and handshakes but does not hello — registration is
// the caller's first request.
func Dial(conn net.Conn, host string, opts Options) (*Client, error) {
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 5 * time.Second
	}
	handshakeTimeout := opts.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = 5 * time.Second
	}
	ws, err := wsDial(conn, host, handshakeTimeout)
	if err != nil {
		return nil, err
	}
	c := &Client{
		opts:    opts,
		ws:      ws,
		pending: make(map[int]chan serverReply),
		Notify:  make(chan notifyMsg, 16),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// request sends one client message and awaits the rid-correlated
// reply. A closed connection fails the call immediately; a timed-out
// rid is dropped (a late reply resolves to nobody).
func (c *Client) request(msg any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.nextRID++
	rid := c.nextRID
	ch := make(chan serverReply, 1)
	c.pending[rid] = ch
	c.mu.Unlock()

	payload, err := json.Marshal(msg)
	if err != nil {
		c.forget(rid)
		return nil, err
	}
	// Stamp the rid: unmarshal to a generic map, set rid, remarshal.
	// Slower than an interface with a RID field would be, but the
	// messages are tiny and the stamping stays in one place.
	var withRID map[string]json.RawMessage
	if err := json.Unmarshal(payload, &withRID); err != nil {
		c.forget(rid)
		return nil, err
	}
	ridJSON, _ := json.Marshal(rid)
	withRID["rid"] = ridJSON
	stamped, err := json.Marshal(withRID)
	if err != nil {
		c.forget(rid)
		return nil, err
	}
	if err := c.ws.writeFrame(opText, stamped, true); err != nil {
		c.forget(rid)
		c.shutdown(err)
		return nil, ErrClosed
	}

	select {
	case reply := <-ch:
		return reply.msg, reply.err
	case <-time.After(c.opts.RequestTimeout):
		c.forget(rid)
		return nil, fmt.Errorf("mailbox: request timed out after %s", c.opts.RequestTimeout)
	}
}

// forget drops a pending rid (timeout or pre-send failure).
func (c *Client) forget(rid int) {
	c.mu.Lock()
	delete(c.pending, rid)
	c.mu.Unlock()
}

// resolve hands one reply to its waiter (if any).
func (c *Client) resolve(rid int, reply serverReply) {
	c.mu.Lock()
	ch, ok := c.pending[rid]
	delete(c.pending, rid)
	c.mu.Unlock()
	if ok {
		ch <- reply
	}
}

// readLoop decodes frames until the connection dies: text frames
// dispatch by rid (or to Notify when un-rid'd), pings get an
// automatic pong, and any error tears every pending request down.
func (c *Client) readLoop() {
	for {
		f, err := c.ws.readFrame()
		if err != nil {
			c.shutdown(err)
			return
		}
		switch f.opcode {
		case opPing:
			// Automatic pong: the RFC's keepalive contract.
			if err := c.ws.writeFrame(opPong, f.payload, true); err != nil {
				c.shutdown(err)
				return
			}
		case opPong:
			// WebSocket-layer pongs carry no rid; the rid'd pong is
			// the JSON reply to Ping(), not this WS control frame.
			// Stray (unsolicited) control pongs are ignored.
		case opClose:
			c.shutdown(ErrClosed)
			return
		case opText:
			if err := c.handleText(f.payload); err != nil {
				c.shutdown(err)
				return
			}
		}
	}
}

// handleText dispatches one text frame.
func (c *Client) handleText(payload []byte) error {
	var head struct {
		T   string `json:"t"`
		RID int    `json:"rid"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return err
	}
	switch head.T {
	case "error":
		var e errorReply
		if err := json.Unmarshal(payload, &e); err != nil {
			return err
		}
		if e.RID == 0 {
			// An un-rid'd error cannot be attributed to any caller —
			// a protocol violation, the connection dies.
			return fmt.Errorf("mailbox: protocol error: %s", e.Message)
		}
		c.resolve(e.RID, serverReply{err: replyError(e)})
	case "notify":
		var n notifyMsg
		if err := json.Unmarshal(payload, &n); err != nil {
			return err
		}
		select {
		case c.Notify <- n:
		default:
			// A full notify buffer means nobody is draining — counts
			// are snapshots, not queues; the plugin pulls the inbox
			// on reconnect anyway.
		}
	default:
		// A rid'd reply: hand the raw JSON to the waiter.
		if head.RID != 0 {
			c.resolve(head.RID, serverReply{msg: json.RawMessage(payload)})
		}
	}
	return nil
}

// replyError classifies an error reply: fatal login codes become the
// typed FatalLoginError, everything else stays a plain error.
func replyError(e errorReply) error {
	if fatalLoginCodes[e.Code] {
		return &FatalLoginError{Code: e.Code, Message: e.Message}
	}
	return fmt.Errorf("mailbox: %s: %s", e.Code, e.Message)
}

// shutdown closes the connection and fails every in-flight request
// immediately — nobody waits out their timeout on a dead wire.
func (c *Client) shutdown(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = cause
	for rid, ch := range c.pending {
		ch <- serverReply{err: fmt.Errorf("%w: %v", ErrClosed, cause)}
		delete(c.pending, rid)
	}
	c.mu.Unlock()
	c.ws.close()
	close(c.done) // the supervisor's signal: the connection is gone
}

// Hello registers this session (protocol.ts:52-75). The Welcome
// carries the applied mailbox id and the daemon's protocol version —
// a version other than 2 (or absent-but-nonzero) is refused.
func (c *Client) Hello(project, projectRoot, session, agent, token string, meta map[string]string) (welcomeMsg, error) {
	raw, err := c.request(helloMsg{
		T:       "hello",
		Project: project, ProjectRoot: projectRoot, Session: session,
		Agent: agent, Token: token, Meta: meta,
	})
	if err != nil {
		return welcomeMsg{}, err
	}
	var w welcomeMsg
	if err := json.Unmarshal(raw, &w); err != nil {
		return welcomeMsg{}, err
	}
	// The daemon ALWAYS carries protocolVersion in the welcome
	// (mailboxd/service.go:340, ProtocolVersion = 2). Accepting 0 was
	// a placeholder for "version absent" — the real daemon never
	// omits it, so a non-2 value (including 0 from a rogue server)
	// is a mismatch.
	if w.ProtocolVersion != 2 {
		return w, fmt.Errorf("mailbox: daemon speaks protocol v%d, we speak v2", w.ProtocolVersion)
	}
	return w, nil
}

// Ping probes liveness (protocol.ts:78-81).
func (c *Client) Ping() (pongMsg, error) {
	raw, err := c.request(pingMsg{T: "ping"})
	if err != nil {
		return pongMsg{}, err
	}
	var p pongMsg
	return p, json.Unmarshal(raw, &p)
}

// Inbox lists mails, newest first (protocol.ts:105-115, 195-206).
func (c *Client) Inbox(unreadOnly bool, limit int, cursor string) (inboxReply, error) {
	raw, err := c.request(inboxMsg{T: "inbox", Unread: unreadOnly, Limit: limit, Cursor: cursor})
	if err != nil {
		return inboxReply{}, err
	}
	var r inboxReply
	return r, json.Unmarshal(raw, &r)
}

// Read marks one mail read (protocol.ts:125-128). Idempotent by the
// daemon's semantics.
func (c *Client) Read(ref MailRef) error {
	_, err := c.request(readMsg{T: "read", Mail: ref})
	return err
}

// Done closes when the connection is gone (server disconnect, IO
// error, or a purposeful Close). The D3 supervisor watches it to
// reconnect and pull the backlog.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the first shutdown cause; ErrClosed for a purposeful
// Close. Only meaningful after Done has closed.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close closes the connection on purpose; pending requests fail with
// ErrClosed.
func (c *Client) Close() {
	c.shutdown(ErrClosed)
}
