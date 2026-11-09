package wire

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"
)

// MaxFrameBytes is the hard cap on one framed packet body (defends against a
// corrupt length prefix turning into a huge allocation).
const MaxFrameBytes = 8 * 1024 * 1024

// writeTimeout 是单次写操作的超时（对端卡住就断开，交给断线续传恢复）。
const writeTimeout = 30 * time.Second

// NewTCPConn wraps a net.Conn into a Packet connection using a 4-byte
// big-endian length prefix per packet.
//
// 返回值带**心跳**（§1.1）：15 秒没发就 ping，30 秒没收到任何东西就判定断开。
// 半开连接（NAT 超时、拔网线）下 TCP 自己不会报错，没有心跳执行端不会重新注册、
// 展示端也不会重连。
func NewTCPConn(conn net.Conn) Conn {
	base := &tcpConn{conn: conn}
	hb := &heartbeatConn{inner: base, lastSeen: time.Now(), done: make(chan struct{})}
	hb.start()
	return hb
}

type tcpConn struct {
	conn net.Conn
	mu   sync.Mutex
	once sync.Once
}

func (c *tcpConn) Send(packet Packet) error {
	body, err := Encode(packet)
	if err != nil {
		return err
	}
	if len(body) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)

	c.mu.Lock()
	defer c.mu.Unlock()
	// 写超时：对端卡住（Wi-Fi 断开/进程挂起）时不能永远阻塞写线程
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	_, err = c.conn.Write(frame)
	return err
}

func (c *tcpConn) Recv() (Packet, error) {
	var header [4]byte
	if _, err := io.ReadFull(c.conn, header[:]); err != nil {
		return Packet{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxFrameBytes {
		return Packet{}, ErrFrameTooLarge
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		return Packet{}, err
	}
	var packet Packet
	if err := Decode(body, &packet); err != nil {
		return Packet{}, err
	}
	return packet, nil
}

func (c *tcpConn) Close() error {
	var err error
	c.once.Do(func() { err = c.conn.Close() })
	return err
}

// Dial 连接一个执行端（默认只允许回环/内网；对外监听由执行端显式指定）。
func Dial(addr string) (Conn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return NewTCPConn(conn), nil
}

// ---- 心跳 ----

// HeartbeatPing / HeartbeatPong 是心跳帧（不进入上层协议）。
const (
	HeartbeatPing = "wire.ping"
	HeartbeatPong = "wire.pong"
)

var (
	// pingEvery 是主动发心跳的间隔；deadAfter 是判定断开的静默时长（测试可调）。
	pingEvery = 15 * time.Second
	deadAfter = 30 * time.Second
)

// SetHeartbeatForTest 调小心跳间隔（测试用）。
func SetHeartbeatForTest(ping, dead time.Duration) {
	pingEvery = ping
	deadAfter = dead
}

type heartbeatConn struct {
	inner    *tcpConn
	mu       sync.Mutex
	lastSeen time.Time
	done     chan struct{}
	once     sync.Once
}

func (h *heartbeatConn) start() {
	go func() {
		ticker := time.NewTicker(pingEvery)
		defer ticker.Stop()
		for {
			select {
			case <-h.done:
				return
			case <-ticker.C:
				h.mu.Lock()
				idle := time.Since(h.lastSeen)
				h.mu.Unlock()
				if idle > deadAfter {
					h.Close()
					return
				}
				if err := h.inner.Send(Packet{Kind: HeartbeatPing}); err != nil {
					h.Close()
					return
				}
			}
		}
	}()
}

// Send 只写，**不刷新 lastSeen**：向半开连接推送事件（流式输出）会让"静默检测"
// 永远不触发——"对端还活着"只能由收到的东西证明。
func (h *heartbeatConn) Send(packet Packet) error {
	return h.inner.Send(packet)
}

func (h *heartbeatConn) Recv() (Packet, error) {
	for {
		packet, err := h.inner.Recv()
		if err != nil {
			return Packet{}, err
		}
		h.mu.Lock()
		h.lastSeen = time.Now()
		h.mu.Unlock()
		switch packet.Kind {
		case HeartbeatPing:
			_ = h.inner.Send(Packet{Kind: HeartbeatPong})
		case HeartbeatPong:
			// 只是"对端还活着"的信号
		default:
			return packet, nil
		}
	}
}

func (h *heartbeatConn) Close() error {
	h.once.Do(func() { close(h.done) })
	return h.inner.Close()
}
