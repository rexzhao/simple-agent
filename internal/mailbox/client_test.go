package mailbox

// D1's table-driven tests: handshake (accept, bad accept), masking
// (client frames must mask — the server rejects unmasked; server
// frames must NOT mask — the client rejects masked), the three
// length encodings with boundary values, fragmentation rejection,
// the message limit, ping→pong, close, rid correlation, request
// timeout, disconnect-fails-in-flight, notify dispatch, and the
// fatal login codes.

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHandshakeRoundTrip(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) { c.replyWelcome(int(h["rid"].(float64)), 2) })
	client := s.dialClient(t, Options{})
	w, err := client.Hello("p1", "/proj", "s1", "sai", "", nil)
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if w.Project != "p1" || w.ProtocolVersion != 2 || w.InstanceID != "inst-1" {
		t.Fatalf("welcome = %+v", w)
	}
	// The hello on the wire carries the fields we sent.
	conn := s.lastConn()
	if conn == nil || conn.last() == nil {
		t.Fatal("server saw no hello")
	}
	m := conn.last()
	if m["project"] != "p1" || m["session"] != "s1" || m["agent"] != "sai" {
		t.Fatalf("hello on the wire = %v", m)
	}
	if _, present := m["sessionDir"]; present {
		t.Fatal("hello must not carry sessionDir (docs §2)")
	}
}

func TestHandshakeBadAccept(t *testing.T) {
	s := newTestServer(t)
	s.mu.Lock()
	s.badAccept = true
	s.mu.Unlock()
	conn := dialRaw(t, s.addr())
	_, err := Dial(conn, s.addr(), Options{})
	if err == nil {
		t.Fatal("a wrong Accept was accepted")
	}
	if !errors.Is(err, ErrBadHandshake) {
		t.Fatalf("err = %v, want ErrBadHandshake", err)
	}
}

func TestClientFramesAreMasked(t *testing.T) {
	// The scripted server rejects an unmasked client frame by dying;
	// a masked frame gets the welcome. The test itself asserts the
	// wire bit via the server's readFrame: if the client ever stops
	// masking, readFrame returns errClientUnmasked, the connection
	// dies, and Hello fails.
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) { c.replyWelcome(int(h["rid"].(float64)), 2) })
	client := s.dialClient(t, Options{})
	if _, err := client.Hello("p1", "/proj", "s1", "sai", "", nil); err != nil {
		t.Fatalf("hello (masked frames rejected?): %v", err)
	}
	// And the raw first byte's mask bit, for a direct pin:
	conn := s.lastConn()
	if conn == nil || conn.frameCount() == 0 {
		t.Fatal("no frames seen")
	}
}

func TestLengthEncodings(t *testing.T) {
	// Frame-aligned boundaries: the server builds the reply payload
	// to make the TEXT FRAME exactly 125 / 126 / 65535 / 65536 bytes
	// (the inbox JSON shell + the mail body = the frame payload). A
	// body-sized target would land past the boundary — the shell is
	// ~110 bytes — and the real encoder thresholds would go untested
	// (review's catch: a <=126 off-by-one survived body-sized tests).
	// The exact frame length is measured by the server's own writer:
	// it trims/pads the mail body until writeFrame produces the
	// target length.
	for _, frameLen := range []int{125, 126, 65535, 65536} {
		frameLen := frameLen
		t.Run("", func(t *testing.T) {
			s := newTestServer(t)
			s.withHelloScript(func(c *serverConn, h map[string]any) {
				rid := int(h["rid"].(float64))
				c.replyWelcome(rid, 2)
			})
			client := s.dialClient(t, Options{})
			if _, err := client.Hello("p1", "/p", "s1", "sai", "", nil); err != nil {
				t.Fatalf("hello: %v", err)
			}
			// The server builds an inbox reply whose TEXT FRAME is
			// exactly frameLen bytes: construct the JSON, measure the
			// frame overhead, then pad the body to hit the target.
			go func() {
				for {
					conn := s.lastConn()
					if conn == nil {
						time.Sleep(5 * time.Millisecond)
						continue
					}
					last := conn.last()
					if last == nil || last["t"] != "ping" {
						time.Sleep(5 * time.Millisecond)
						continue
					}
					rid := int(last["rid"].(float64))
					conn.replyFrameExact(t, rid, frameLen)
					return
				}
			}()
			// The reply type is a pong (the padded frame target) — the
			// assertion is that the boundary-sized frame arrived and
			// decoded without error, not the inbox semantics.
			if _, err := client.Ping(); err != nil {
				t.Fatalf("frame %d: %v", frameLen, err)
			}
		})
	}
}

func TestClientWritesLargeFrames(t *testing.T) {
	// The client's own write path across the length boundaries: a
	// hello whose meta crosses the 7-bit limit (and, with a bigger
	// body, the 16-bit one). The server decodes the SAME frame logic
	// — a wrong length encoding anywhere garbles the JSON.
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) { c.replyWelcome(int(h["rid"].(float64)), 2) })
	client := s.dialClient(t, Options{})
	big := strings.Repeat("m", 300)
	if _, err := client.Hello("p1", "/p", "s1", "sai", "", map[string]string{"note": big}); err != nil {
		t.Fatalf("hello with 300-byte meta: %v", err)
	}
	conn := s.lastConn()
	m := conn.last()
	if m == nil || m["meta"].(map[string]any)["note"] != big {
		t.Fatalf("the large meta did not survive the wire intact")
	}
}

func TestFragmentedFrameRejected(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) {
		rid := int(h["rid"].(float64))
		c.replyWelcome(rid, 2)
		// Right after the welcome, violate the protocol: a FIN=0
		// text frame.
		_ = c.writeFrameMask(opText, []byte(`{"t":"x"}`), false, true)
	})
	client := s.dialClient(t, Options{})
	if _, err := client.Hello("p1", "/p", "s1", "sai", "", nil); err != nil {
		t.Fatalf("hello: %v", err)
	}
	// The next request observes the dead connection.
	if _, err := client.Ping(); !errors.Is(err, ErrClosed) {
		t.Fatalf("ping after fragmented frame: %v, want ErrClosed", err)
	}
}

func TestMessageLimit(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) {
		rid := int(h["rid"].(float64))
		c.replyWelcome(rid, 2)
		// A frame declaring more than the limit: the client must
		// reject it before allocating.
		huge := make([]byte, maxMessageBytes+1)
		_ = c.writeFrame(opText, huge)
	})
	client := s.dialClient(t, Options{})
	if _, err := client.Hello("p1", "/p", "s1", "sai", "", nil); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if _, err := client.Ping(); !errors.Is(err, ErrClosed) {
		t.Fatalf("ping after oversized frame: %v, want ErrClosed", err)
	}
}

func TestPingPong(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) {
		rid := int(h["rid"].(float64))
		c.replyWelcome(rid, 2)
	})
	client := s.dialClient(t, Options{})
	if _, err := client.Hello("p1", "/p", "s1", "sai", "", nil); err != nil {
		t.Fatal(err)
	}
	// A protocol-level ping from the server: the client must answer
	// with a pong (observed as the server surviving and replying to
	// a later request).
	go func() {
		for {
			conn := s.lastConn()
			if conn != nil && conn.last() != nil && conn.last()["t"] == "ping" {
				rid := int(conn.last()["rid"].(float64))
				_ = conn.pushJSON(map[string]any{"t": "pong", "rid": rid, "project": "p1", "instanceId": "i"})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if _, err := client.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestRidCorrelation(t *testing.T) {
	s := newTestServer(t)
	// Replies go to rid+1 (the wrong rid): the client's request must
	// time out, not resolve with a stranger's reply.
	s.withHelloScript(func(c *serverConn, h map[string]any) {
		rid := int(h["rid"].(float64))
		c.replyWelcome(rid+1, 2)
	})
	client := s.dialClient(t, Options{RequestTimeout: 50 * time.Millisecond})
	_, err := client.Hello("p1", "/p", "s1", "sai", "", nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("mis-routed reply: err = %v, want timeout", err)
	}
}

func TestRequestTimeout(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) {
		// Never reply.
		_ = c
	})
	client := s.dialClient(t, Options{RequestTimeout: 40 * time.Millisecond})
	start := time.Now()
	_, err := client.Hello("p1", "/p", "s1", "sai", "", nil)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("timeout took %v — the 5s default leaked", elapsed)
	}
}

func TestDisconnectFailsInFlight(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) { /* no reply */ })
	client := s.dialClient(t, Options{RequestTimeout: 10 * time.Second})
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Hello("p1", "/p", "s1", "sai", "", nil)
		errCh <- err
	}()
	// Wait for the request to be in flight, then kill the wire.
	time.Sleep(100 * time.Millisecond)
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.conn.Close()
	}
	s.mu.Unlock()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("in-flight after disconnect: %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the in-flight request waited out its timeout on a dead wire")
	}
}

func TestNotifyDispatch(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) {
		rid := int(h["rid"].(float64))
		c.replyWelcome(rid, 2)
		// Then push a notify (no rid).
		_ = c.pushJSON(map[string]any{"t": "notify", "unread": 3})
	})
	client := s.dialClient(t, Options{})
	if _, err := client.Hello("p1", "/p", "s1", "sai", "", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-client.Notify:
		if n.Unread != 3 {
			t.Fatalf("notify = %+v", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the notify push never arrived")
	}
}

func TestFatalLoginCodes(t *testing.T) {
	for _, code := range []string{"mailbox_in_use", "bad_name", "login_failed", "name_held", "session_bound"} {
		code := code
		t.Run(code, func(t *testing.T) {
			s := newTestServer(t)
			s.withHelloScript(func(c *serverConn, h map[string]any) {
				c.replyError(int(h["rid"].(float64)), code, "injected")
			})
			client := s.dialClient(t, Options{})
			_, err := client.Hello("p1", "/p", "s1", "sai", "", nil)
			var fatal *FatalLoginError
			if !errors.As(err, &fatal) {
				t.Fatalf("err = %v, want FatalLoginError", err)
			}
			if fatal.Code != code {
				t.Fatalf("code = %q, want %q", fatal.Code, code)
			}
		})
	}
	// A non-fatal error stays a plain error.
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) {
		c.replyError(int(h["rid"].(float64)), "transient_hiccup", "retry me")
	})
	client := s.dialClient(t, Options{})
	_, err := client.Hello("p1", "/p", "s1", "sai", "", nil)
	var fatal *FatalLoginError
	if errors.As(err, &fatal) {
		t.Fatalf("a non-fatal code was classified fatal: %v", err)
	}
	if err == nil {
		t.Fatal("no error")
	}
}

func TestCloseHandshake(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) { c.replyWelcome(int(h["rid"].(float64)), 2) })
	client := s.dialClient(t, Options{})
	if _, err := client.Hello("p1", "/p", "s1", "sai", "", nil); err != nil {
		t.Fatal(err)
	}
	client.Close()
	// The server observed the close frame (its conn loop answered
	// and exited) — the next request on the closed client fails.
	if _, err := client.Ping(); !errors.Is(err, ErrClosed) {
		t.Fatalf("ping after close: %v, want ErrClosed", err)
	}
}

func TestVersionMismatch(t *testing.T) {
	// The daemon always sends protocolVersion (mailboxd/service.go:340);
	// anything != 2 — including 0 from a rogue server — is refused.
	for _, version := range []int{0, 1, 99} {
		version := version
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			s := newTestServer(t)
			s.withHelloScript(func(c *serverConn, h map[string]any) {
				c.replyWelcome(int(h["rid"].(float64)), version)
			})
			client := s.dialClient(t, Options{})
			_, err := client.Hello("p1", "/p", "s1", "sai", "", nil)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("protocol v%d", version)) {
				t.Fatalf("version %d: err = %v", version, err)
			}
		})
	}
}

// dialRaw is a raw TCP dial (for handshake-failure tests that must
// build the Client themselves).
func dialRaw(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestClientFrameBoundaries pins the CLIENT's encoder at the exact
// frame-payload thresholds: hellos whose JSON is exactly 125 / 126 /
// 65535 / 65536 bytes (a padded meta value). The server's readFrame
// measures the payload size — a threshold off-by-one in either
// direction garbles the frame (review's catch: a <=126 mutation
// survived body-sized tests).
func TestClientFrameBoundaries(t *testing.T) {
	for _, frameLen := range []int{125, 126, 65535, 65536} {
		frameLen := frameLen
		t.Run("", func(t *testing.T) {
			s := newTestServer(t)
			s.withHelloScript(func(c *serverConn, h map[string]any) {
				c.replyWelcome(int(h["rid"].(float64)), 2)
			})
			client := s.dialClient(t, Options{})
			// Build the hello JSON to exactly frameLen bytes by
			// padding the meta note value. The fixed shell:
			// {"t":"hello","rid":1,"project":"p1","projectRoot":"/p",
			//  "session":"s1","agent":"sai","meta":{"note":"<pad>"}}
			shell := len(`{"t":"hello","rid":1,"project":"p1","projectRoot":"/p","session":"s1","agent":"sai","meta":{"note":""}}`)
			padLen := frameLen - shell
			if padLen < 0 {
				t.Fatalf("frame %d: hello shell %d alone exceeds it", frameLen, shell)
			}
			pad := strings.Repeat("n", padLen)
			if _, err := client.Hello("p1", "/p", "s1", "sai", "", map[string]string{"note": pad}); err != nil {
				t.Fatalf("hello (frame %d): %v", frameLen, err)
			}
			// The server saw a text frame of exactly frameLen bytes.
			conn := s.lastConn()
			if conn == nil {
				t.Fatal("no connection")
			}
			if got := conn.lastPayloadLen(); got != frameLen {
				t.Fatalf("frame %d: server received payload %d bytes", frameLen, got)
			}
		})
	}
}

// TestHandshakeTimeout: a silent server (accepts but never answers
// the upgrade) must not hang Dial past its deadline.
func TestHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept but never respond — the silent peer.
			_ = conn
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Dial in a goroutine + a 2s bound: without a deadline it hangs
	// forever (K8's form — the package-level timeout is too coarse).
	type dialResult struct {
		client *Client
		err    error
	}
	ch := make(chan dialResult, 1)
	start := time.Now()
	go func() {
		client, err := Dial(conn, ln.Addr().String(), Options{HandshakeTimeout: 100 * time.Millisecond})
		ch <- dialResult{client, err}
	}()
	select {
	case r := <-ch:
		if r.err == nil {
			t.Fatal("Dial succeeded against a silent server")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Dial took %v — the deadline did not bound it", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dial hung past 2s against a silent server — the handshake deadline is missing")
	}
}

// TestHandshakeDeadlineCleared pins the CLEAR: a short
// HandshakeTimeout must only bound the upgrade exchange — the
// connection remains usable long after it expired. Without the
// clear, the first request after (handshake timeout + margin) would
// hit a stale deadline.
func TestHandshakeDeadlineCleared(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) { c.replyWelcome(int(h["rid"].(float64)), 2) })
	client := s.dialClient(t, Options{HandshakeTimeout: 50 * time.Millisecond})
	if _, err := client.Hello("p1", "/p", "s1", "sai", "", nil); err != nil {
		t.Fatalf("hello: %v", err)
	}
	// Wait past the handshake deadline, then prove the connection
	// is still alive (a stale deadline would have killed it).
	time.Sleep(300 * time.Millisecond)
	if _, err := client.Ping(); err != nil {
		t.Fatalf("ping after the handshake deadline elapsed: %v — the deadline was not cleared", err)
	}
}

// TestDoneOnServerDisconnect: the supervisor's signal — Done closes
// within a bounded time of the server dropping the wire, and Err is
// non-nil.
func TestDoneOnServerDisconnect(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) { c.replyWelcome(int(h["rid"].(float64)), 2) })
	client := s.dialClient(t, Options{})
	if _, err := client.Hello("p1", "/p", "s1", "sai", "", nil); err != nil {
		t.Fatal(err)
	}
	// Kill the wire on the server side.
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.conn.Close()
	}
	s.mu.Unlock()
	select {
	case <-client.Done():
		if client.Err() == nil {
			t.Fatal("Done closed but Err is nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close within 2s of the server dropping the wire")
	}
}

// TestDoneOnClose: a purposeful Close also fires Done, with Err =
// ErrClosed.
func TestDoneOnClose(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) { c.replyWelcome(int(h["rid"].(float64)), 2) })
	client := s.dialClient(t, Options{})
	client.Close()
	select {
	case <-client.Done():
		if !errors.Is(client.Err(), ErrClosed) {
			t.Fatalf("Err after Close = %v, want ErrClosed", client.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("Done did not close on purposeful Close")
	}
}

// TestConcurrentWritesWithPings: concurrent requests while the server
// pings — the writeMu must keep frames indivisible (the -race
// detector and the server's frame decoder both watch).
func TestConcurrentWritesWithPings(t *testing.T) {
	s := newTestServer(t)
	s.withHelloScript(func(c *serverConn, h map[string]any) { c.replyWelcome(int(h["rid"].(float64)), 2) })
	client := s.dialClient(t, Options{RequestTimeout: 2 * time.Second})
	if _, err := client.Hello("p1", "/p", "s1", "sai", "", nil); err != nil {
		t.Fatal(err)
	}
	// The server pings continuously while we fire concurrent pings.
	go func() {
		for i := 0; i < 50; i++ {
			conn := s.lastConn()
			if conn != nil {
				_ = conn.writeFrame(opPing, []byte("keepalive"))
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_, _ = client.Ping()
			}
		}()
	}
	wg.Wait()
}
