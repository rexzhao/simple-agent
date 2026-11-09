// Package wire 是"按包收发"的连接抽象。
//
// 传输层的假设（用户确认）：按包发送、不丢包、不重复、保序，单包有大小上限。
// 直连、经 relay、进程内管道都是这个接口；协议层不关心底层是什么。
package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Protocol 是当前协议版本（握手时交换）。
const Protocol = 1

// DefaultMaxPacket 是单包大小上限的默认值（用户假设 64 KiB）。
const DefaultMaxPacket = 64 * 1024

// Packet 是连接上的一次收发单元。Body 是逻辑消息的 JSON。
type Packet struct {
	// Conn 是逻辑通道 id（握手后由接受方分配，不做哈希）。
	Conn uint32
	Kind string
	Body []byte
}

// Conn 是一条按包收发、保序的连接。
type Conn interface {
	Send(Packet) error
	Recv() (Packet, error)
	Close() error
}

// ErrClosed 表示连接已关闭。
var ErrClosed = errors.New("连接已关闭")

// PipePair 返回两条互相连通的连接（进程内实现，用于单机测试）。
//
// 每条连接有自己的收/发缓冲：发送方不阻塞在接收方读取上，测试里"发完就断言"
// 不会死锁；缓冲满时 Send 阻塞（与真实连接一致）。
func PipePair() (Conn, Conn) {
	aToB := make(chan Packet, 64)
	bToA := make(chan Packet, 64)
	a := &pipeConn{out: aToB, in: bToA, done: make(chan struct{})}
	b := &pipeConn{out: bToA, in: aToB, done: make(chan struct{})}
	return a, b
}

type pipeConn struct {
	out  chan Packet
	in   chan Packet
	done chan struct{}
	once sync.Once
}

func (c *pipeConn) Send(packet Packet) error {
	select {
	case <-c.done:
		return ErrClosed
	case c.out <- packet:
		return nil
	}
}

func (c *pipeConn) Recv() (Packet, error) {
	select {
	case <-c.done:
		return Packet{}, ErrClosed
	case packet := <-c.in:
		return packet, nil
	}
}

// Close 让这条连接自己的收发都失败。
//
// **不关闭底层 channel**：另一端的 Send 仍在往里写，关掉会 panic。
// 这里只关自己的 done，对端的 Send/Recv 会继续正常返回（真实连接里对端
// 会在下一次写时拿到错误，这里由调用方显式关闭另一侧）。
func (c *pipeConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

// ---- 握手 ----

// Credentials 是握手凭据（**预留**：现在恒为 none，以后加 token / mTLS 指纹）。
type Credentials struct {
	Kind  string `json:"kind"`
	Token string `json:"token,omitempty"`
}

// Hello 由发起方发出。
type Hello struct {
	Protocol    int         `json:"protocol"`
	Role        string      `json:"role"`    // "viewer" / "executor" / "relay-client"
	Purpose     string      `json:"purpose"` // "control" / "bulk"
	DeviceName  string      `json:"device_name,omitempty"`
	Credentials Credentials `json:"credentials"`
}

// Welcome 由接受方回。
type Welcome struct {
	Protocol   int      `json:"protocol"`
	HostID     string   `json:"host_id"`
	HostName   string   `json:"host_name"`
	Capability []string `json:"capability,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// Encode 把任意载荷编成包体。
func Encode(value any) ([]byte, error) { return json.Marshal(value) }

// Decode 解出载荷。
func Decode(body []byte, target any) error { return json.Unmarshal(body, target) }

// ---- 分块 ----

// Fragment 是超大载荷的一块。同一个 MsgID 的分块按 Index 拼回原始字节。
type Fragment struct {
	// Kind 是重组完成后要交给哪个处理函数（"cmd" / "log.events" / "blob.put"…）。
	// **不要靠 MsgID 前缀猜类型**：分块是通用机制。
	Kind   string `json:"kind,omitempty"`
	MsgID  string `json:"msg_id"`
	Index  int    `json:"index"`
	Count  int    `json:"count"`
	Total  int    `json:"total"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}

// Fragments 把 payload 切成不超过 maxPacket 的分块（不做 IO，便于调用方排队发送）。
func Fragments(msgID string, payload []byte, maxPacket int) []Fragment {
	return FragmentsKind("", msgID, payload, maxPacket)
}

// FragmentsKind 与 Fragments 相同，但带上"重组后交给谁处理"的逻辑类型。
func FragmentsKind(kind, msgID string, payload []byte, maxPacket int) []Fragment {
	if maxPacket <= 0 {
		maxPacket = DefaultMaxPacket
	}
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	chunk := (maxPacket - 512) * 3 / 4
	if chunk < 256 {
		chunk = 256
	}
	total := (len(payload) + chunk - 1) / chunk
	if total == 0 {
		total = 1
	}
	out := make([]Fragment, 0, total)
	for index := 0; index < total; index++ {
		start := index * chunk
		end := start + chunk
		if end > len(payload) {
			end = len(payload)
		}
		out = append(out, Fragment{
			Kind: kind, MsgID: msgID, Index: index, Count: total,
			Total: len(payload), SHA256: hash, Data: payload[start:end],
		})
	}
	return out
}

// SendLarge 把 payload 切成不超过 maxPacket 的 Fragment 逐包发出（Kind=fragment）。
//
// maxPacket<=0 时用 DefaultMaxPacket。
func SendLarge(conn Conn, connID uint32, msgID, innerKind string, payload []byte, maxPacket int) error {
	if maxPacket <= 0 {
		maxPacket = DefaultMaxPacket
	}
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])

	// 预留头部空间，并考虑 base64 膨胀（JSON 里 []byte 编成 base64，最多 4/3）
	chunk := (maxPacket - 512) * 3 / 4
	if chunk < 256 {
		chunk = 256
	}
	total := (len(payload) + chunk - 1) / chunk
	if total == 0 {
		total = 1
	}
	for index := 0; index < total; index++ {
		start := index * chunk
		end := start + chunk
		if end > len(payload) {
			end = len(payload)
		}
		fragment := Fragment{
			Kind: innerKind, MsgID: msgID, Index: index, Count: total,
			Total: len(payload), SHA256: hash, Data: payload[start:end],
		}
		body, err := Encode(fragment)
		if err != nil {
			return err
		}
		if err := conn.Send(Packet{Conn: connID, Kind: "fragment", Body: body}); err != nil {
			return err
		}
	}
	return nil
}

// Reassembler 把分块拼回原始载荷。
type Reassembler struct {
	parts map[string]*fragments
}

type fragments struct {
	total int
	kind  string
	hash  string
	data  [][]byte
	seen  int
}

// NewReassembler 构造拼装器。
func NewReassembler() *Reassembler {
	return &Reassembler{parts: map[string]*fragments{}}
}

// Add 收下一块；收齐并校验通过时返回 (逻辑类型, 完整载荷, ok)。
func (r *Reassembler) Add(fragment Fragment) (string, []byte, bool, error) {
	entry := r.parts[fragment.MsgID]
	if entry == nil {
		entry = &fragments{total: fragment.Count, kind: fragment.Kind, hash: fragment.SHA256, data: make([][]byte, fragment.Count)}
		r.parts[fragment.MsgID] = entry
	}
	if fragment.Index < 0 || fragment.Index >= len(entry.data) {
		return "", nil, false, fmt.Errorf("分块下标越界：%d", fragment.Index)
	}
	if entry.data[fragment.Index] == nil {
		entry.seen++
	}
	entry.data[fragment.Index] = fragment.Data
	if entry.seen < entry.total {
		return "", nil, false, nil
	}
	delete(r.parts, fragment.MsgID)

	joined := make([]byte, 0, fragment.Total)
	for _, chunk := range entry.data {
		joined = append(joined, chunk...)
	}
	sum := sha256.Sum256(joined)
	if hex.EncodeToString(sum[:]) != entry.hash {
		return "", nil, false, errors.New("分块校验失败（sha256 不匹配）")
	}
	return entry.kind, joined, true, nil
}

// ErrFrameTooLarge is returned when a frame's length prefix exceeds MaxFrameBytes.
var ErrFrameTooLarge = errors.New("数据包超过上限")

// Handshaken 标记"这条连接已经完成握手（hello/welcome 交换过了）"。
// 上层据此不再重复发 hello。
type Handshaken interface{ Handshaken() bool }

// Prefetch 把已经读出的一条包垫回连接最前面（握手探测时用：
// 先自己发 hello 读第一条回包判断对端，再把这条回包交给上层，不浪费连接也不超时）。
func Prefetch(conn Conn, packet Packet) Conn {
	return &prefetchConn{inner: conn, buffered: []Packet{packet}}
}

type prefetchConn struct {
	inner    Conn
	mu       sync.Mutex
	buffered []Packet
}

func (c *prefetchConn) Send(packet Packet) error { return c.inner.Send(packet) }

func (c *prefetchConn) Recv() (Packet, error) {
	c.mu.Lock()
	if len(c.buffered) > 0 {
		packet := c.buffered[0]
		c.buffered = c.buffered[1:]
		c.mu.Unlock()
		return packet, nil
	}
	c.mu.Unlock()
	return c.inner.Recv()
}

func (c *prefetchConn) Close() error { return c.inner.Close() }

// Handshaken 报告握手已完成（OpenViewer 已经发过 hello 并收到 welcome）。
func (c *prefetchConn) Handshaken() bool { return true }

// NullConn 是一条"丢弃发送、阻塞接收"的占位连接：用来构造一个暂时不该
// 建立连接的 Remote（例如展示端还没选执行端），随后再真正拨号替换它。
func NullConn() Conn { return &nullConn{done: make(chan struct{})} }

type nullConn struct {
	once sync.Once
	done chan struct{}
}

func (c *nullConn) Send(Packet) error { return nil }

func (c *nullConn) Recv() (Packet, error) {
	<-c.done
	return Packet{}, ErrClosed
}

func (c *nullConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}
