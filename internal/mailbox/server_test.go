package mailbox

// The minimal scripted WebSocket test server (docs/mailbox.md §10
// D1): it reuses frames.go's encoding with server-side framing
// (unmasked) and lets each test script the conversation — replies,
// delays, disconnects, notify pushes, a bad Accept, and protocol
// violations (fragmented frames). A _test.go file on purpose: it is
// test infrastructure, and keeping it out of a separate package
// means it can never leak into a production import graph; D2+ steps
// test in this same package and reuse it directly.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// testServer is a scriptable mailboxd stand-in over real TCP.
type testServer struct {
	t       *testing.T
	ln      net.Listener
	mu      sync.Mutex
	conns   []*serverConn
	onHello func(c *serverConn, h map[string]any)
	// badAccept scripts a wrong Sec-WebSocket-Accept (K2's target).
	badAccept bool
}

// errClientUnmasked: a client frame arrived unmasked (K1's target —
// the server rejects it per the RFC).
var errClientUnmasked = errors.New("test server: client frame not masked")

// serverConn is one accepted client connection. lastMessage and
// frames are written by the serve goroutine and read by test
// goroutines — the mutex keeps that safe.
type serverConn struct {
	parent *testServer
	conn   net.Conn
	br     *bufio.Reader
	mu     sync.Mutex
	// lastMessage is the most recent decoded client JSON (assertions).
	lastMessage map[string]any
	// frames is every received text payload, in order.
	frames []string
	// payloadLens is each text frame's payload byte count (the
	// client-side boundary test's assertion target).
	payloadLens []int
}

// record stores one received text message (serve goroutine only).
// payloadLen is the FRAME payload size (before JSON decode) — the
// client-side boundary test asserts against it.
func (c *serverConn) record(msg map[string]any, payload []byte) {
	c.mu.Lock()
	c.lastMessage = msg
	c.frames = append(c.frames, string(payload))
	c.payloadLens = append(c.payloadLens, len(payload))
	c.mu.Unlock()
}

// last returns the most recent message (test side).
func (c *serverConn) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastMessage
}

// frameCount returns how many text frames arrived (test side).
func (c *serverConn) frameCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

// lastPayloadLen returns the most recent text frame's payload size.
func (c *serverConn) lastPayloadLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.payloadLens) == 0 {
		return -1
	}
	return c.payloadLens[len(c.payloadLens)-1]
}

// newTestServer starts listening on 127.0.0.1:0.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{t: t, ln: ln}
	t.Cleanup(s.stop)
	go s.accept()
	return s
}

// addr is the dial target.
func (s *testServer) addr() string { return s.ln.Addr().String() }

func (s *testServer) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // stopped
		}
		sc := &serverConn{parent: s, conn: conn, br: bufio.NewReader(conn)}
		s.mu.Lock()
		s.conns = append(s.conns, sc)
		s.mu.Unlock()
		go sc.serve()
	}
}

func (s *testServer) stop() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.conn.Close()
	}
	s.mu.Unlock()
}

// serve performs the server handshake then reads frames until death.
func (c *serverConn) serve() {
	// --- the server handshake: answer the upgrade with a correct
	// (or scripted-bad) Accept.
	req, err := http.ReadRequest(c.br)
	if err != nil {
		return
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	accept := c.acceptFor(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := c.conn.Write([]byte(resp)); err != nil {
		return
	}
	for {
		frame, err := c.readFrame()
		if err != nil {
			return
		}
		switch frame.opcode {
		case opPing:
			_ = c.writeFrame(opPong, frame.payload)
		case opClose:
			_ = c.writeFrame(opClose, nil)
			return
		case opText:
			var msg map[string]any
			if err := json.Unmarshal(frame.payload, &msg); err != nil {
				return
			}
			c.record(msg, frame.payload)
			if msg["t"] == "hello" && c.parent.onHello != nil {
				c.parent.onHello(c, msg)
			}
			// Auto-reply to JSON pings: most tests don't script them, and
			// the liveness checks (handshake-deadline-cleared, etc.) just
			// need a pong back.
			if msg["t"] == "ping" {
				rid := int(msg["rid"].(float64))
				_ = c.pushJSON(map[string]any{"t": "pong", "rid": rid, "project": "p1", "instanceId": "inst-1"})
			}
		}
	}
}

// acceptFor computes the handshake Accept (testServer.badAccept
// flips it to a wrong value).
func (c *serverConn) acceptFor(key string) string {
	if c.parent.badAccept {
		return "definitely-wrong"
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// --- server-side frame encoding: UNMASKED (RFC6455 §5.1), with a
// switch for protocol-violation scripts (masking, fragmentation).

// readFrame decodes a client frame (must be masked) using frames.go's
// shared length logic but its own validation.
func (c *serverConn) readFrame() (frame, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return frame{}, err
	}
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	if !masked {
		// A client frame must be masked — reject per the RFC.
		return frame{}, errClientUnmasked
	}
	// Frame order (RFC6455 §5.2): head, extended length, THEN the
	// mask key, then the payload.
	switch {
	case length == 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return frame{}, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case length == 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return frame{}, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var maskKey [4]byte
	if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
		return frame{}, err
	}
	if length > maxMessageBytes {
		return frame{}, ErrTooLarge
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return frame{}, err
	}
	for i := range payload {
		payload[i] ^= maskKey[i%4]
	}
	return frame{opcode: opcode, payload: payload}, nil
}

// writeFrame sends an unmasked server frame.
func (c *serverConn) writeFrame(opcode byte, payload []byte) error {
	return c.writeFrameMask(opcode, payload, false, false)
}

// writeFrameMask is writeFrame with scriptable protocol violations:
// masked=true sends a masked server frame (illegal), fin=false sends
// a FIN=0 frame (fragmentation).
func (c *serverConn) writeFrameMask(opcode byte, payload []byte, masked, noFIN bool) error {
	first := byte(opcode)
	if !noFIN {
		first |= 0x80
	}
	head := []byte{first}
	lenByte := byte(len(payload))
	if masked {
		lenByte |= 0x80
	}
	switch {
	case len(payload) <= 125:
		head = append(head, lenByte)
	case len(payload) <= 65535:
		head = append(head, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		head = append(head, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(len(payload)))
		head = append(head, ext[:]...)
	}
	if masked {
		var key [4]byte
		key[0], key[1], key[2], key[3] = 1, 2, 3, 4
		maskedPayload := make([]byte, len(payload))
		for i, b := range payload {
			maskedPayload[i] = b ^ key[i%4]
		}
		_, err := c.conn.Write(append(append(head, key[:]...), maskedPayload...))
		return err
	}
	_, err := c.conn.Write(append(head, payload...))
	return err
}

// pushJSON marshals and sends one text frame.
func (c *serverConn) pushJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(opText, data)
}

// --- scripting helpers on the server (not per-connection):

// withHelloScript installs the conversation driver: every hello gets
// the callback, which scripts replies/pushes on that connection.
func (s *testServer) withHelloScript(fn func(c *serverConn, h map[string]any)) *testServer {
	s.mu.Lock()
	s.onHello = fn
	s.mu.Unlock()
	return s
}

// lastConn returns the most recent accepted connection (tests assert
// against it after the client acted).
func (s *testServer) lastConn() *serverConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) == 0 {
		return nil
	}
	return s.conns[len(s.conns)-1]
}

// replyWelcome sends a welcome for the given rid (v2 by default).
func (c *serverConn) replyWelcome(rid int, version int) {
	_ = c.pushJSON(map[string]any{
		"t": "welcome", "rid": rid, "project": "p1",
		"instanceId": "inst-1", "protocolVersion": version,
	})
}

// replyInbox sends an inbox reply for the rid.
func (c *serverConn) replyInbox(rid int, mails []MailRecord) {
	_ = c.pushJSON(map[string]any{
		"t": "inbox", "rid": rid, "mails": mails,
		"total": len(mails), "totalUnread": len(mails),
	})
}

// replyError sends an error reply for the rid.
func (c *serverConn) replyError(rid int, code, message string) {
	_ = c.pushJSON(map[string]any{
		"t": "error", "rid": rid, "code": code, "message": message,
	})
}

// replyFrameExact sends a text frame whose payload is EXACTLY target
// bytes: a pong reply with a padded project field (the smallest JSON
// type with a free-form string field we control). The frame payload
// IS the JSON bytes — this pins the encoder thresholds at the true
// wire boundaries, not at body sizes (review's catch).
func (c *serverConn) replyFrameExact(t *testing.T, rid int, target int) {
	t.Helper()
	// The fixed JSON shell around the project value:
	// {"t":"pong","rid":<rid>,"project":"<pad>","instanceId":"i"}
	// Measure it with an empty pad, then pad to the target.
	shell := len(`{"t":"pong","rid":`) + len(fmt.Sprintf("%d", rid)) +
		len(`,"project":"","instanceId":"i"}`)
	padLen := target - shell
	if padLen < 0 {
		t.Fatalf("frame %d: pong shell %d alone exceeds it", target, shell)
	}
	data := `{"t":"pong","rid":` + fmt.Sprintf("%d", rid) +
		`,"project":"` + strings.Repeat("p", padLen) + `","instanceId":"i"}`
	if len(data) != target {
		t.Fatalf("frame %d: built %d", target, len(data))
	}
	_ = c.writeFrame(opText, []byte(data))
}

// dialClient connects a real Client to the test server.
func (s *testServer) dialClient(t *testing.T, opts Options) *Client {
	t.Helper()
	conn, err := net.Dial("tcp", s.addr())
	if err != nil {
		t.Fatal(err)
	}
	client, err := Dial(conn, s.addr(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}
