package wire

import (
	"errors"
	"sync"
)

// MuxCloseKind 是"关闭这条逻辑通道"的控制帧（双向）。
//
// 没有它，一侧关闭通道时对端永远收不到 EOF：执行端的 Serve goroutine 会一直挂着、
// 观察者不摘除，每个事件还继续编码发送（展示端每重连一次泄漏一套）。
const MuxCloseKind = "mux.close"

// Mux 在**一条**连接上复用多条逻辑通道（relay 转发时用：执行端只连 relay 一次，
// 多个展示端各自拿到一条虚拟连接）。
//
// 帧格式：外层 Packet 的 Conn 字段就是通道 id；首包在该 id 上的出现即"打开通道"。
// relay 只搬帧，不解析会话协议。
type Mux struct {
	conn Conn

	mu        sync.Mutex
	chans     map[uint32]*muxChan
	accept    chan Conn
	closed    chan struct{}
	closeOnce sync.Once
}

type muxChan struct {
	id   uint32
	mux  *Mux
	in   chan Packet
	done chan struct{}
	once sync.Once
}

// NewMux 在 conn 上启动读循环。
func NewMux(conn Conn) *Mux {
	m := &Mux{conn: conn, chans: map[uint32]*muxChan{}, accept: make(chan Conn, 16), closed: make(chan struct{})}
	go m.readLoop()
	return m
}

// Accept 等待一条新的逻辑通道（对端在该 id 上发了包）。
func (m *Mux) Accept() (Conn, error) {
	select {
	case <-m.closed:
		return nil, ErrClosed
	case conn := <-m.accept:
		return conn, nil
	}
}

// Open 主动打开一条逻辑通道（执行端/展示端不一定用得到，保留给对称场景）。
func (m *Mux) Open(id uint32) Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.chans[id]; ok {
		return existing
	}
	channel := &muxChan{id: id, mux: m, in: make(chan Packet, 64), done: make(chan struct{})}
	m.chans[id] = channel
	return channel
}

// Close 关闭底层连接与所有通道。
func (m *Mux) Close() error {
	m.closeOnce.Do(func() {
		close(m.closed)
		m.mu.Lock()
		channels := m.chans
		m.chans = map[uint32]*muxChan{}
		m.mu.Unlock()
		for _, channel := range channels {
			channel.close()
		}
	})
	return m.conn.Close()
}

// dropChannel 断开某条逻辑通道（队列满/对端不读），并通知对端。
func (m *Mux) dropChannel(channel *muxChan) {
	m.mu.Lock()
	delete(m.chans, channel.id)
	m.mu.Unlock()
	channel.notifyClose()
	channel.close()
}

func (m *Mux) readLoop() {
	for {
		packet, err := m.conn.Recv()
		if err != nil {
			m.Close()
			return
		}
		if packet.Kind == MuxCloseKind {
			m.mu.Lock()
			channel, ok := m.chans[packet.Conn]
			delete(m.chans, packet.Conn)
			m.mu.Unlock()
			if ok {
				channel.close()
			}
			continue
		}
		m.mu.Lock()
		channel, ok := m.chans[packet.Conn]
		if !ok {
			channel = &muxChan{id: packet.Conn, mux: m, in: make(chan Packet, 64), done: make(chan struct{})}
			m.chans[packet.Conn] = channel
		}
		m.mu.Unlock()
		if !ok {
			// 先解锁再投递：accept 缓冲满了也不能拿着锁等（Close 需要这把锁）
			select {
			case m.accept <- channel:
			case <-m.closed:
				channel.close()
				continue
			}
		}
		select {
		case channel.in <- packet:
		default:
			// 某个通道满了不能堵住读循环（否则一个卡住的展示端拖垮所有通道）
			m.dropChannel(channel)
		}
	}
}

func (c *muxChan) Send(packet Packet) error {
	select {
	case <-c.done:
		return ErrClosed
	case <-c.mux.closed:
		return ErrClosed
	default:
	}
	packet.Conn = c.id
	return c.mux.conn.Send(packet)
}

func (c *muxChan) Recv() (Packet, error) {
	select {
	case <-c.done:
		return Packet{}, ErrClosed
	case <-c.mux.closed:
		return Packet{}, ErrClosed
	case packet := <-c.in:
		return packet, nil
	}
}

func (c *muxChan) Close() error {
	c.mux.mu.Lock()
	delete(c.mux.chans, c.id)
	c.mux.mu.Unlock()
	// 通知对端：relay 收到后关掉对应展示端；执行端收到后结束 Serve
	c.notifyClose()
	c.close()
	return nil
}

// notifyClose 尽力发一帧"关闭通道"（连接已断就忽略）。
func (c *muxChan) notifyClose() {
	_ = c.mux.conn.Send(Packet{Conn: c.id, Kind: MuxCloseKind})
}

func (c *muxChan) close() { c.once.Do(func() { close(c.done) }) }

// ErrRelayUnavailable 表示 relay 上没有这个执行端（或已下线）。
var ErrRelayUnavailable = errors.New("relay 上没有这个执行端")
