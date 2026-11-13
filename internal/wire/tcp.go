package wire

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
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

// RemoteAddr 暴露对端地址（relay 按 IP 限流要用）。
//
// wire.Conn 接口本身不含地址（协议层抽象），但这里实现它之后，
// relay 可以用接口断言拿到来源 IP。包装层（heartbeatConn）也要转发。
func (c *tcpConn) RemoteAddr() net.Addr {
	if c.conn == nil {
		return nil
	}
	return c.conn.RemoteAddr()
}

// DialFunc 是底层连接的建立方式（默认 net.Dial）。
//
// 抽成钩子是为了让上层能插入 TLS：wire 包本身不认识 TLS，
// 但"怎么建连接"和"帧怎么切"是两件正交的事。
//
// **必须用 atomic**：Android 端会在协程里切换拨号方式（连接/断开/重连），
// 而 Dial 可能在另一条协程里同时执行。以前是裸全局变量，并发读写会触发
// Go 的 fatal error（表现为 SIGABRT、runtime.raise，进程直接消失）。
type dialHandler func(addr string) (net.Conn, error)

func defaultDial(addr string) (net.Conn, error) { return net.Dial("tcp", addr) }

var dialFunc atomic.Pointer[dialHandler]

func init() {
	handler := dialHandler(defaultDial)
	dialFunc.Store(&handler)
}

// SetDialFunc 替换底层拨号方式（传 nil 恢复默认）。
//
// 这样 wire.Dial 的所有调用点（relay 客户端、执行端直连等）都会自动走 TLS，
// 不需要在每个调用点加参数。全程无锁，可在运行期任意并发调用。
func SetDialFunc(fn func(addr string) (net.Conn, error)) {
	handler := dialHandler(defaultDial)
	if fn != nil {
		handler = dialHandler(fn)
	}
	dialFunc.Store(&handler)
}

// DialFunc 返回当前的拨号函数（用于"临时替换再还原"，例如移动端按连接切换 TLS）。
func DialFunc() func(addr string) (net.Conn, error) {
	handler := dialFunc.Load()
	if handler == nil {
		return defaultDial
	}
	return func(addr string) (net.Conn, error) { return (*handler)(addr) }
}

// Dial 连接一个执行端（默认只允许回环/内网；对外监听由执行端显式指定）。
func Dial(addr string) (Conn, error) {
	handler := dialFunc.Load()
	if handler == nil {
		return nil, errors.New("wire: 拨号函数未初始化")
	}
	conn, err := (*handler)(addr)
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

// RemoteAddr 转发内层的对端地址。
//
// 必须显式转发：heartbeatConn 是 wire.Conn 的实现，但接口里没有 RemoteAddr，
// 不转发的话 relay 的接口断言会失败，按 IP 限流就形同虚设（曾经就是这样）。
func (h *heartbeatConn) RemoteAddr() net.Addr {
	if h.inner == nil {
		return nil
	}
	return h.inner.RemoteAddr()
}
