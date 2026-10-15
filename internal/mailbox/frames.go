// Package mailbox is the sai-side mailbox client (milestone D,
// docs/mailbox.md): a zero-dependency WebSocket connection to a
// mailboxd daemon speaking protocol v2.
//
// This file implements the RFC6455 subset the protocol needs
// (docs/mailbox.md §9): the client handshake, client-masked /
// server-unmasked frames, the three payload-length encodings,
// text/ping/pong/close opcodes only, automatic pong, and the close
// handshake. Fragmentation and unknown opcodes are rejected — the
// mailbox messages are small JSON, never fragmented.
package mailbox

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wsGUID is the RFC6455 handshake constant.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes (RFC6455 §5.2). Only these four are handled; everything
// else — including continuation (0), which only exists for
// fragmentation — is rejected.
const (
	opContinuation = 0x0
	opText         = 0x1
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxMessageBytes is the single-frame payload ceiling. No
// fragmentation means one frame carries a whole message; the limit
// guards allocation against a buggy or hostile peer. 1 MiB is ~1000×
// the largest expected mailbox JSON (a mail with a long body).
const maxMessageBytes = 1 << 20

// errFrameShape names the frame-level rejections. Typed so tests (and
// callers triaging a dead connection) can distinguish protocol
// breakage from transport errors.
var (
	ErrFragmented   = errors.New("mailbox: fragmented frames are not supported")
	ErrBadOpcode    = errors.New("mailbox: unsupported websocket opcode")
	ErrTooLarge     = errors.New("mailbox: frame exceeds the message limit")
	ErrBadHandshake = errors.New("mailbox: handshake rejected")
)

// wsAccept computes the Sec-WebSocket-Accept value for a key.
func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// wsConn is one established WebSocket connection. Client-side: all
// OUTGOING frames are masked; incoming frames must NOT be (RFC6455
// §5.1 — a server must never mask). The test server flips this via
// writeFrame's mask parameter.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	// writeMu serializes whole-frame writes: request goroutines (which
	// may run concurrently) and the readLoop's automatic pong both
	// call writeFrame. Go's TCP implementation locks single Write
	// calls, but that is not the net.Conn interface's promise — a
	// whole frame must be one indivisible byte sequence.
	writeMu sync.Mutex
}

// wsDial performs the client handshake over conn: the random
// Sec-WebSocket-Key, the upgrade request, and the Accept validation —
// a mismatching or missing Accept fails the handshake (a rogue server
// is not a mailboxd).
func wsDial(conn net.Conn, host string, timeout time.Duration) (*wsConn, error) {
	var keyRaw [16]byte
	if _, err := rand.Read(keyRaw[:]); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mailbox: handshake key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyRaw[:])
	req := "GET / HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	// The handshake is deadline-bound: a silent or half-open peer
	// (a port grabbed by another process, a hung daemon) must not
	// hang Dial forever — the D3 reconnect loop would be stuck.
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mailbox: send handshake: %w", err)
	}
	// ONE buffered reader for the connection's whole life: the
	// response read may buffer bytes past the handshake (the first
	// server frames), and a throwaway reader here would swallow them.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mailbox: read handshake: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: status %d", ErrBadHandshake, resp.StatusCode)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: Sec-WebSocket-Accept mismatch", ErrBadHandshake)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: not a websocket upgrade", ErrBadHandshake)
	}
	// Clear the handshake deadline: the connection is live now, the
	// request timeout governs the rest.
	if timeout > 0 {
		_ = conn.SetDeadline(time.Time{})
	}
	return &wsConn{conn: conn, br: br}, nil
}

// writeFrame sends one frame. mask selects client (true) or server
// (false) framing — production code always passes true from the
// client side; the test server is the only false caller.
func (c *wsConn) writeFrame(opcode byte, payload []byte, mask bool) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	var head []byte
	first := byte(0x80 | opcode) // FIN=1, no RSV
	head = append(head, first)
	lenByte := byte(len(payload))
	if mask {
		lenByte |= 0x80
	}
	switch {
	case len(payload) <= 125:
		head = append(head, lenByte)
	case len(payload) <= 65535:
		// The 126/127 markers must carry the mask bit too — writing
		// the bare marker makes the server see an unmasked frame and
		// reject it (found by the client-side large-frame test).
		marker := byte(126)
		if mask {
			marker |= 0x80
		}
		head = append(head, marker, byte(len(payload)>>8), byte(len(payload)))
	default:
		marker := byte(127)
		if mask {
			marker |= 0x80
		}
		head = append(head, marker)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(len(payload)))
		head = append(head, ext[:]...)
	}
	if mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return fmt.Errorf("mailbox: mask key: %w", err)
		}
		masked := make([]byte, len(payload))
		for i, b := range payload {
			masked[i] = b ^ key[i%4]
		}
		head = append(head, key[:]...)
		if _, err := c.conn.Write(append(head, masked...)); err != nil {
			return err
		}
		return nil
	}
	if _, err := c.conn.Write(append(head, payload...)); err != nil {
		return err
	}
	return nil
}

// frame is one decoded incoming frame.
type frame struct {
	opcode  byte
	payload []byte
}

// readFrame reads, validates, and decodes one incoming frame. Server
// frames must be unmasked; FIN must be set; the opcode must be one of
// text/close/ping/pong; the declared length must fit the message
// limit before any large allocation.
func (c *wsConn) readFrame() (frame, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return frame{}, err
	}
	fin := head[0]&0x80 != 0
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	switch {
	case !fin || opcode == opContinuation:
		return frame{}, ErrFragmented
	case opcode != opText && opcode != opClose && opcode != opPing && opcode != opPong:
		return frame{}, fmt.Errorf("%w: 0x%x", ErrBadOpcode, opcode)
	case masked:
		// RFC6455 §5.1: a server MUST NOT mask.
		return frame{}, errors.New("mailbox: server frame must not be masked")
	}
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
	if length > maxMessageBytes {
		return frame{}, fmt.Errorf("%w: %d bytes", ErrTooLarge, length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return frame{}, err
	}
	return frame{opcode: opcode, payload: payload}, nil
}

// close sends a close frame and tears down the TCP connection — best
// effort, the peer may already be gone.
func (c *wsConn) close() {
	_ = c.writeFrame(opClose, nil, true)
	_ = c.conn.Close()
}
