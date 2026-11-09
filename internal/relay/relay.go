// Package relay 是把「展示端 ─▶ relay ─▶ 执行端」串起来的转发服务。
//
// 设计约束（docs/remote-design.md §7.2）：relay **只做三件事**——执行端注册、
// 列出在线执行端、按 hostID 转发帧；它不认识 event/cmd 等会话协议，也不解析包体，
// 所以协议演进不需要同步改 relay。
//
// 实现上执行端与 relay 之间只有一条 TCP 连接，用 Conn 字段复用多条逻辑通道
// （wire.Mux）：每个展示端一条，互不干扰。
package relay

import (
	"crypto/subtle"
	"errors"
	"log"
	"sync"

	"simple-agent/internal/wire"
)

// 协议消息名（只属于 relay 层，与会话协议无关）。
const (
	MsgRegister  = "relay.register"  // 执行端 → relay：我是 hostID
	MsgList      = "relay.list"      // 展示端 → relay：有哪些执行端
	MsgExecutors = "relay.executors" // relay → 展示端：[{host_id,name}]
	MsgRoute     = "relay.route"     // 展示端 → relay：我要连 hostID
	MsgRouted    = "relay.routed"    // relay → 展示端：通道已就绪（失败带 error）
	MsgPing      = "relay.ping"
	MsgPong      = "relay.pong"
	// MsgRegistered 是注册结果（失败时带 error；同名/超限都拒绝后来的那个）
	MsgRegistered = "relay.registered"
	// MsgClose 通知对端"这条逻辑通道关了"（与 wire.MuxCloseKind 同义）
	MsgClose = wire.MuxCloseKind
)

// Executor 是注册上来的执行端。
type Executor struct {
	HostID string `json:"host_id"`
	Name   string `json:"name,omitempty"`
	// Token 是 relay 的固定口令（relay 未配置 token 时留空即可）。
	// 放在注册/路由帧里而不是握手层，是因为 relay 本来就不解析会话协议，
	// 加一个字段比给 wire.Hello 增加一种"relay 专用凭据"更简单。
	Token string `json:"token,omitempty"`
}

// tokenPayload 是"首包里带 token"的最小视图（注册 / 路由 / 列表查询共用）。
type tokenPayload struct {
	Token string `json:"token,omitempty"`
}

// ErrBadToken 是 token 不匹配时返回给客户端的错误文案。
var ErrBadToken = errors.New("relay 拒绝了连接：token 不正确")

// sameToken 做**定长比较**，避免用比较耗时逐字节试探 token。
func sameToken(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// Relay 是一个 relay 实例。
type Relay struct {
	mu        sync.Mutex
	executors map[string]*executorLink
	nextID    uint32
	// MaxExecutors 限制注册的执行端数量（0 表示不限）。
	MaxExecutors int
	// Token 非空时要求所有连上来的执行端与展示端都带同一个固定 token。
	//
	// 只保护 relay 这一段（用户决定）：执行端直连（--listen）仍是开放的，
	// 需要保护就用隧道或只在回环上监听。relay 只做固定口令比较，不做会话密钥、
	// 不加密流量——它的作用是"别人扫到端口也连不上"，不是防止中间人。
	Token string
}

type executorLink struct {
	info Executor
	conn wire.Conn
	// writeMu 串行化"relay → 执行端"的写（多个展示端并发写同一条连接）
	writeMu sync.Mutex
	// viewers: 逻辑通道 id → 展示端连接与它自己的发送队列
	mu      sync.Mutex
	viewers map[uint32]*viewerLink
	closed  bool
}

// viewerLink 是 relay 到某个展示端的一段：**每个展示端一个有界队列 + 专属写 goroutine**。
//
// 直接在主读循环里写 socket 的话，一个卡住的展示端会让 relay 不再读执行端，
// 进而把同一个执行端下的所有展示端都拖死。
type viewerLink struct {
	conn  wire.Conn
	queue chan wire.Packet
	done  chan struct{}
	once  sync.Once
}

func newViewerLink(conn wire.Conn) *viewerLink {
	return &viewerLink{conn: conn, queue: make(chan wire.Packet, viewerQueueLimit), done: make(chan struct{})}
}

func (v *viewerLink) start() {
	go func() {
		for {
			select {
			case <-v.done:
				return
			case packet := <-v.queue:
				if err := v.conn.Send(packet); err != nil {
					v.close()
					return
				}
			}
		}
	}()
}

func (v *viewerLink) send(packet wire.Packet) bool {
	select {
	case <-v.done:
		return false
	case v.queue <- packet:
		return true
	default:
		return false
	}
}

func (v *viewerLink) close() {
	v.once.Do(func() {
		close(v.done)
		_ = v.conn.Close()
	})
}

// outQueueLimit 是单个展示端在 relay 的发送队列上限（按包数）。
const outQueueLimit = 2000

// SetOutQueueLimitForTest 调小展示端发送队列（测试用：模拟卡住的展示端）。
func SetOutQueueLimitForTest(limit int) { viewerQueueLimit = limit }

var viewerQueueLimit = outQueueLimit

// send 给某个逻辑通道发一帧（执行端用 Conn 字段区分观众）。
//
// 注意：relay 这一侧**不**再起读循环（wire.Mux 是给执行端用的），
// 否则两个读循环会抢同一条连接的帧。
func (l *executorLink) send(id uint32, packet wire.Packet) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	packet.Conn = id
	return l.conn.Send(packet)
}

// New 创建 relay。
func New() *Relay { return &Relay{executors: map[string]*executorLink{}, nextID: 1} }

// Serve 处理一条来自执行端或展示端的连接（连接已握手）。
func (r *Relay) Serve(conn wire.Conn) {
	defer conn.Close()
	// 客户端（relay.OpenViewerToken）会先发一条 hello 来分辨"对面是执行端还是
	// relay"。relay 不是执行端：不回 welcome，而是回执行端列表作为探测应答。
	//
	// 这里**必须先校验 token**：否则未认证的人发一条 hello 就能拿到执行端列表。
	// token 走 wire.Hello.Credentials（协议里本来就有的预留字段）。
	var packet wire.Packet
	for {
		first, err := conn.Recv()
		if err != nil {
			return
		}
		if first.Kind != "hello" {
			packet = first
			break
		}
		var hello wire.Hello
		_ = wire.Decode(first.Body, &hello)
		if !r.allowToken(hello.Credentials.Token) {
			_ = conn.Send(wire.Packet{Kind: MsgExecutors, Body: mustEncode(map[string]any{
				"error": ErrBadToken.Error(),
			})})
			return
		}
		_ = conn.Send(wire.Packet{Kind: MsgExecutors, Body: mustEncode(r.List())})
	}
	// 配了 token 时，请求帧里的 token 也要对（注册/路由/列表查询三条路径共用）。
	if !r.authorize(conn, packet) {
		return
	}
	switch packet.Kind {
	case MsgRegister:
		var info Executor
		if err := wire.Decode(packet.Body, &info); err != nil || info.HostID == "" {
			return
		}
		r.serveExecutor(conn, info)
	case MsgRoute:
		var request struct {
			HostID string `json:"host_id"`
		}
		if err := wire.Decode(packet.Body, &request); err != nil {
			return
		}
		r.serveViewer(conn, request.HostID)
	default:
		// 展示端第一句不是 route：按"查询列表"处理
		_ = conn.Send(wire.Packet{Kind: MsgExecutors, Body: mustEncode(r.List())})
		r.serveViewer(conn, "")
	}
}

// allowToken 判断给定 token 是否可用（relay 没配 token 时恒为真）。
func (r *Relay) allowToken(token string) bool {
	return r.Token == "" || sameToken(token, r.Token)
}

// authorize 校验首包里的 token（未配置 token 时恒为真）。
//
// 失败时回一条明确的错误帧再断开：客户端要能把「token 不对」和「连不上」区分开，
// 否则用户只会看到"一直重连中"。
func (r *Relay) authorize(conn wire.Conn, packet wire.Packet) bool {
	if r.Token == "" {
		return true
	}
	var auth tokenPayload
	if err := wire.Decode(packet.Body, &auth); err != nil || !sameToken(auth.Token, r.Token) {
		kind := MsgExecutors
		if packet.Kind == MsgRegister {
			kind = MsgRegistered
		} else if packet.Kind == MsgRoute {
			kind = MsgRouted
		}
		_ = conn.Send(wire.Packet{Kind: kind, Body: mustEncode(map[string]any{
			"error": ErrBadToken.Error(),
		})})
		log.Printf("relay：拒绝了一个 token 不正确/缺失的连接（%s）", packet.Kind)
		return false
	}
	return true
}

// Close 断开所有执行端与展示端（模拟 relay 进程退出/重启）。
func (r *Relay) Close() {
	r.mu.Lock()
	links := r.executors
	r.executors = map[string]*executorLink{}
	r.mu.Unlock()
	for _, link := range links {
		link.mu.Lock()
		viewers := link.viewers
		link.viewers = map[uint32]*viewerLink{}
		link.closed = true
		link.mu.Unlock()
		for _, viewer := range viewers {
			viewer.close()
		}
		_ = link.conn.Close()
	}
}

// List 返回在线执行端（不暴露会话内容）。
func (r *Relay) List() []Executor {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Executor, 0, len(r.executors))
	for _, link := range r.executors {
		out = append(out, link.info)
	}
	return out
}

func (r *Relay) serveExecutor(conn wire.Conn, info Executor) {
	r.mu.Lock()
	if existing, taken := r.executors[info.HostID]; taken && existing != nil {
		r.mu.Unlock()
		_ = conn.Send(wire.Packet{Kind: MsgRegistered, Body: mustEncode(map[string]any{
			"error": "同名执行端已在线（拒绝后来的注册）"})})
		return
	}
	if r.MaxExecutors > 0 && len(r.executors) >= r.MaxExecutors {
		r.mu.Unlock()
		_ = conn.Send(wire.Packet{Kind: MsgRegistered, Body: mustEncode(map[string]any{
			"error": "relay 的执行端数量已达上限"})})
		return
	}
	link := &executorLink{info: info, conn: conn, viewers: map[uint32]*viewerLink{}}
	_ = conn.Send(wire.Packet{Kind: MsgRegistered, Body: mustEncode(map[string]any{"host_id": info.HostID})})
	r.executors[info.HostID] = link
	r.mu.Unlock()
	log.Printf("relay：执行端已注册 %s（%s）", info.HostID, info.Name)

	defer func() {
		link.mu.Lock()
		link.closed = true
		viewers := link.viewers
		link.viewers = map[uint32]*viewerLink{}
		link.mu.Unlock()
		for _, viewer := range viewers {
			viewer.close()
		}
		r.mu.Lock()
		if r.executors[info.HostID] == link {
			delete(r.executors, info.HostID)
		}
		r.mu.Unlock()
		log.Printf("relay：执行端已断开 %s", info.HostID)
	}()

	// 执行端 → 展示端：按通道 id 找观众
	for {
		packet, err := conn.Recv()
		if err != nil {
			return
		}
		if packet.Kind == MsgPing {
			_ = conn.Send(wire.Packet{Kind: MsgPong})
			continue
		}
		link.mu.Lock()
		viewer, ok := link.viewers[packet.Conn]
		link.mu.Unlock()
		if !ok {
			continue
		}
		if packet.Kind == MsgClose {
			// 执行端关掉了这条通道（例如发送队列溢出想让它续传）：
			// 关掉对应展示端的物理连接，展示端会自动重连并补齐
			link.mu.Lock()
			delete(link.viewers, packet.Conn)
			link.mu.Unlock()
			viewer.close()
			continue
		}
		if !viewer.send(packet) {
			// 这个展示端卡住了：只断开它，别拖累同一个执行端下的其它展示端
			link.mu.Lock()
			delete(link.viewers, packet.Conn)
			link.mu.Unlock()
			viewer.close()
			link.send(packet.Conn, wire.Packet{Conn: packet.Conn, Kind: MsgClose})
		}
	}
}

func (r *Relay) serveViewer(conn wire.Conn, hostID string) {
	// 没有指定 hostID：只提供列表，然后按后续 route 再切
	if hostID == "" {
		_ = conn.Send(wire.Packet{Kind: MsgExecutors, Body: mustEncode(r.List())})
		next, err := conn.Recv()
		if err != nil || next.Kind != MsgRoute {
			return
		}
		var request struct {
			HostID string `json:"host_id"`
		}
		if err := wire.Decode(next.Body, &request); err != nil {
			return
		}
		hostID = request.HostID
	}

	r.mu.Lock()
	link, ok := r.executors[hostID]
	if ok {
		r.nextID++
		if r.nextID == 0 {
			r.nextID = 1
		}
	}
	id := r.nextID
	r.mu.Unlock()
	if !ok {
		_ = conn.Send(wire.Packet{Kind: MsgRouted, Body: mustEncode(map[string]any{"error": "执行端不在线"})})
		return
	}

	link.mu.Lock()
	if link.closed {
		link.mu.Unlock()
		_ = conn.Send(wire.Packet{Kind: MsgRouted, Body: mustEncode(map[string]any{"error": "执行端已断开"})})
		return
	}
	viewer := newViewerLink(conn)
	viewer.start()
	link.viewers[id] = viewer
	link.mu.Unlock()
	viewer.send(wire.Packet{Kind: MsgRouted, Body: mustEncode(map[string]any{"host_id": hostID})})

	defer func() {
		link.mu.Lock()
		delete(link.viewers, id)
		link.mu.Unlock()
		viewer.close()
		// 通知执行端：这条逻辑通道关了，让它的 Serve 退出、观察者摘除
		link.send(id, wire.Packet{Conn: id, Kind: MsgClose})
	}()

	// 展示端 → 执行端：原样转发（relay 不解析）
	for {
		packet, err := conn.Recv()
		if err != nil {
			return
		}
		if packet.Kind == MsgPing {
			viewer.send(wire.Packet{Kind: MsgPong})
			continue
		}
		if err := link.send(id, packet); err != nil {
			return
		}
	}
}

// ServeAll 在 listener 上不断接受连接（测试与真实进程共用）。
func (r *Relay) ServeAll(accept func() (wire.Conn, error)) error {
	for {
		conn, err := accept()
		if err != nil {
			if errors.Is(err, wire.ErrClosed) {
				return nil
			}
			return err
		}
		go r.Serve(conn)
	}
}

func mustEncode(value any) []byte {
	body, _ := wire.Encode(value)
	return body
}
