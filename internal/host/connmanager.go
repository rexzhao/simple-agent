package host

import (
	"errors"
	"sync/atomic"
	"time"

	"simple-agent/internal/wire"
)

// connManager 是展示端的**连接管理器**（actor 模式）。
//
// 为什么这样做：读循环、写循环、重连、连接替换如果分散在多个 goroutine 里，
// 就会出现"旧 loop 迟到的断线处理把新连接标记成断线"这类交错——加 epoch 校验
// 只是在打补丁。这里把所有可变的连接状态收归一个 goroutine 所有：
//
//   - 每条物理连接一个**读 goroutine**（只把包/错误转发进 inbox，不碰状态）；
//   - 每条物理连接一个**写 goroutine**（只消费自己的发送通道）；
//   - 连接、队列、待确认命令、重连计时全部由 run() 独占；
//   - 过期消息按 connID 丢弃；
//   - 界面线程只接收 `post` 出来的包与失败结果，mirror 只在界面线程修改。
type connManager struct {
	inbox            chan mIn
	reqs             chan mReq
	post             func(func())
	dial             func() (wire.Conn, error)
	deliver          func(wire.Packet)
	deliverConnected func(bool)
	deliverResult    func(CmdResult)

	// 以下字段只由 run() 读写
	conn      wire.Conn
	connID    uint64
	connected bool
	out       chan mItem
	pending   map[string]cmdState
	closed    bool
	attach    []map[string]any
	// attachCh 是"只保留最新一份"的 attach 计划（合并写入，永不丢最后一次）。
	attachCh chan []map[string]any
	// hostGen 是"当前执行端"的代际：切换执行端时 +1。管理器 post 给界面线程的
	// 每个回调都带上发出时的代际，界面执行时不一致就丢弃（否则旧机器的包会在
	// Reset 之后落进来，出现"在 B 上看到 A 的内容"）。
	hostGen *atomic.Uint64
	// onDisconnect 在断线时被调用（展示端用它唤醒挂着的 blob 请求）。
	onDisconnect func()
	// done 在 Close 时关闭：读写 goroutine 用它从阻塞发送里脱身。
	done chan struct{}
	// attachSent 表示"这条连接上已经用当前 attach 计划发过 attach 请求"。
	attachSent bool
	backoff    time.Duration
}

// mIn 是读/写 goroutine 与重连拨号送回管理器的消息。
type mIn struct {
	connID  uint64
	packet  *wire.Packet
	err     error
	written string
	dialed  wire.Conn
	dialErr error
	// retry 表示"立即重连"（不走退避）。
	retry bool
}

// mReq 是界面线程发起的请求（非阻塞入队）。
type mReq struct {
	kind string // submit / useConn / close / attach
	item mItem
	// ack 非空时，run 处理完这次提交后回报是否真的入队（blob 需要知道）。
	ack    chan error
	conn   wire.Conn
	attach []map[string]any
}

// cmdState 是命令在管理器里的状态。
type cmdState int

const (
	cmdQueued cmdState = iota // 还在发送队列里，没写出去
	cmdSent                   // 已经写出去，还没收到结果
)

// mItem 是发送队列里的一项。
//
// large 非空时表示"一条**逻辑**大消息"：队列里只占一项，由写 goroutine 在写出时
// 再切块（否则 20MB 图 ≈320 块会撑爆队列、被当成拥塞断开）。
type mItem struct {
	packet wire.Packet
	cmdID  string
	large  *largePayload
}

// largePayload 是一条要分块发送的逻辑消息。
type largePayload struct {
	kind    string
	msgID   string
	payload []byte
}

func newConnManager(post func(func()), dial func() (wire.Conn, error)) *connManager {
	return &connManager{
		inbox:    make(chan mIn, 256),
		reqs:     make(chan mReq, 64),
		attachCh: make(chan []map[string]any, 1),
		done:     make(chan struct{}),
		post:     post,
		dial:     dial,
		pending:  map[string]cmdState{},
		backoff:  3 * time.Second,
	}
}

// start 用首条连接启动管理器。
func (m *connManager) start(conn wire.Conn) {
	m.conn = conn
	m.connID = 1
	m.connected = true
	m.out = make(chan mItem, 256)
	go m.readLoop(conn, m.connID)
	go m.writeLoop(conn, m.connID, m.out)
	go m.run()
}

// submit 非阻塞提交一条命令（返回 false 表示管理器忙/已关闭）。
func (m *connManager) submit(item mItem) bool {
	select {
	case m.reqs <- mReq{kind: "submit", item: item}:
		return true
	default:
		return false
	}
}

// submitWait 与 submit 相同，但会等管理器确认"确实入队"。
//
// blob 的读写需要这个确认：被丢弃的包不能让调用方以为已经发出去了。
func (m *connManager) submitWait(item mItem) error {
	ack := make(chan error, 1)
	select {
	case m.reqs <- mReq{kind: "submit", item: item, ack: ack}:
	case <-m.done:
		return errNotConnected
	}
	select {
	case err := <-ack:
		return err
	case <-m.done:
		return errNotConnected
	}
}

var errNotConnected = errors.New("连接中断")

// useConn 换一条新连接（测试/手动重连用）。
func (m *connManager) useConn(conn wire.Conn) bool {
	select {
	case m.reqs <- mReq{kind: "useConn", conn: conn}:
		return true
	default:
		return false
	}
}

// setAttach 发布"重连时要 attach 的会话与游标"（界面线程调用，非阻塞）。
//
// 只保留最新一份：旧计划被新计划替换，绝不会因为队列满而丢掉**最后一次**发布。
func (m *connManager) setAttach(specs []map[string]any) {
	for {
		select {
		case m.attachCh <- specs:
			return
		default:
		}
		// 满了：丢掉旧的，再试一次
		select {
		case <-m.attachCh:
		default:
		}
	}
}

func (m *connManager) close() {
	select {
	case m.reqs <- mReq{kind: "close"}:
	default:
	}
}

// readLoop 只负责把这条连接上的包/错误送进 inbox。
func (m *connManager) readLoop(conn wire.Conn, id uint64) {
	for {
		packet, err := conn.Recv()
		if err != nil {
			m.notify(mIn{connID: id, err: err})
			return
		}
		netLog("⇐", "executor", id, packet.Kind, len(packet.Body))
		p := packet
		m.notify(mIn{connID: id, packet: &p})
	}
}

// writeLoop 只消费这条连接的发送通道。
func (m *connManager) writeLoop(conn wire.Conn, id uint64, out chan mItem) {
	for item := range out {
		// 先声明"已经开始写这条命令"：写失败时无法确定对端是否收到，
		// 只能按"结果未知"处理（宁可不还原草稿，也不要让用户发两遍）。
		if item.large != nil {
			for _, fragment := range wire.FragmentsKind(item.large.kind, item.large.msgID, item.large.payload, wire.DefaultMaxPacket) {
				raw, err := wire.Encode(fragment)
				if err != nil {
					break
				}
				if err := conn.Send(wire.Packet{Kind: msgFragment, Body: raw}); err != nil {
					m.notify(mIn{connID: id, err: err})
					return
				}
			}
			continue
		}
		if item.cmdID != "" {
			m.notify(mIn{connID: id, written: item.cmdID})
		}
		netLog("⇒", "executor", id, item.packet.Kind, len(item.packet.Body))
		if err := conn.Send(item.packet); err != nil {
			m.notify(mIn{connID: id, err: err})
			return
		}
	}
}

func (m *connManager) run() {
	for {
		select {
		case in := <-m.inbox:
			if in.retry {
				m.backoff = time.Second
				if m.connected {
					// disconnect 自己会安排重连，不要再排一次（否则同时拨两条）
					m.disconnect()
				} else if m.dial != nil {
					m.scheduleReconnect()
				}
				continue
			}
			m.handleIn(in)
		case req := <-m.reqs:
			m.handleReq(req)
		case specs := <-m.attachCh:
			m.handleAttach(specs)
		}
		if m.closed {
			return
		}
	}
}

func (m *connManager) handleIn(in mIn) {
	if in.connID != m.connID {
		return // 过期连接的消息：直接丢
	}
	switch {
	case in.packet != nil:
		packet := *in.packet
		// 收到命令结果：这条命令的故事结束了（断线时不该再给它回失败）
		if packet.Kind == msgCmdResult {
			var result CmdResult
			if err := wire.Decode(packet.Body, &result); err == nil {
				delete(m.pending, result.CmdID)
			}
		}
		m.postHost(func() { m.deliver(packet) })
	case in.written != "":
		// 命令已经写出去：从"未送达"变成"等结果"（断线时两种结局不同）
		if _, ok := m.pending[in.written]; ok {
			m.pending[in.written] = cmdSent
		}
	case in.dialed != nil:
		m.adopt(in.dialed)
	case in.dialErr != nil:
		m.scheduleReconnect()
	case in.err != nil:
		m.disconnect()
	}
}

func (m *connManager) handleReq(req mReq) {
	switch req.kind {
	case "dropConn":
		if m.connected {
			m.disconnect()
		}
		if req.ack != nil {
			req.ack <- nil
		}
	case "resetHost":
		for id := range m.pending {
			m.postHost(func() {
				m.deliverResult(CmdResult{CmdID: id, Error: "已切换执行端，命令结果未知", Unknown: true})
			})
		}
		m.pending = map[string]cmdState{}
	case "submit":
		if !m.connected || m.conn == nil {
			if req.ack != nil {
				req.ack <- errNotConnected
			}
			// 注意：这里可能是"Submit 先入队、断线先被处理"的正常竞态，
			// 结论与断线时一致——没送出去，界面还原草稿即可。
			m.failCmd(req.item.cmdID, "连接中断，命令未送达")
			return
		}
		if req.ack != nil {
			req.ack <- nil
		}
		if req.item.cmdID != "" {
			m.pending[req.item.cmdID] = cmdQueued
		}
		select {
		case m.out <- req.item:
		default:
			// 队列满：断开自己（重连后靠续传），并把这条命令回失败
			m.failCmd(req.item.cmdID, "连接拥塞，命令未发送")
			m.disconnect()
		}
	case "useConn":
		m.adopt(req.conn)
	case "attach":
		m.handleAttach(req.attach)
	case "connected":
		connected := m.connected
		m.postHost(func() { m.deliverConnected(connected) })
	case "close":
		m.closed = true
		close(m.done)
		m.failPending()
		if m.conn != nil {
			_ = m.conn.Close()
		}
	}
}

// tryEnqueue 非阻塞入队。
func (m *connManager) tryEnqueue(item mItem) bool {
	select {
	case m.out <- item:
		return true
	default:
		return false
	}
}

// handleAttach 保存最新计划，并在"这条连接还没发过 attach"时立即发送。
func (m *connManager) handleAttach(specs []map[string]any) {
	m.attach = specs
	if m.connected && m.out != nil && !m.attachSent && len(specs) > 0 {
		for _, spec := range specs {
			if !m.tryEnqueue(mItem{packet: wire.Packet{Kind: msgAttach, Body: mustEncode(spec)}}) {
				m.disconnect()
				return
			}
		}
		m.attachSent = true
	}
}

// adopt 换用一条连接：重启读写 goroutine，重新握手并 attach。
func (m *connManager) adopt(conn wire.Conn) {
	if m.conn != nil {
		_ = m.conn.Close()
	}
	m.connID++
	m.conn = conn
	m.connected = true
	m.backoff = 3 * time.Second
	m.out = make(chan mItem, 256)
	go m.readLoop(conn, m.connID)
	go m.writeLoop(conn, m.connID, m.out)

	// 握手 + 按游标 attach（重连时不补发断线期间的命令）。
	// 全部**非阻塞**写：写不进去就断开这条新连接、走重连流程，
	// 绝不能让管理器 goroutine 卡在 out 上（整个 actor 会停摆）。
	hello := mItem{packet: wire.Packet{Kind: msgHello, Body: mustEncode(wire.Hello{
		Protocol: wire.Protocol, Role: "viewer", Purpose: "control",
		Credentials: wire.Credentials{Kind: "none"},
	})}}
	if !m.tryEnqueue(hello) {
		_ = conn.Close()
		m.connected = false
		m.scheduleReconnect()
		return
	}
	// 每次（重）连都要重新拉一次会话列表：切换执行端/重连后本地缓存可能已经清空，
	// 只靠构造时那一次会让侧栏永远空着。
	if !m.tryEnqueue(mItem{packet: wire.Packet{Kind: msgCatalog}}) {
		_ = conn.Close()
		m.connected = false
		m.scheduleReconnect()
		return
	}
	for _, spec := range m.attach {
		if !m.tryEnqueue(mItem{packet: wire.Packet{Kind: msgAttach, Body: mustEncode(spec)}}) {
			_ = conn.Close()
			m.connected = false
			m.scheduleReconnect()
			return
		}
	}
	// 还没有 attach 计划时保持 false：等界面发布最新游标后再发，否则这条连接
	// 会一直没有 attach（界面看到的永远是空会话）
	m.attachSent = len(m.attach) > 0
	m.postHost(func() { m.deliverConnected(true) })
}

// disconnect 处理"当前连接断了"：失败所有未写出的命令，按退避重连。
func (m *connManager) disconnect() {
	// 唤醒挂着的 blob 请求要**无条件**做：连接可能已经被对端关掉而我们这边
	// 还没处理完，早退的话等待方会一直挂到超时。
	if m.onDisconnect != nil {
		m.onDisconnect()
	}
	if !m.connected {
		return
	}
	m.connected = false
	m.drainAcks()
	m.failPending()
	if m.conn != nil {
		_ = m.conn.Close()
	}
	m.conn = nil
	m.postHost(func() { m.deliverConnected(false) })
	if m.dial != nil {
		m.scheduleReconnect()
	}
}

// reconnectNow 立即重连（用户在界面上"切换执行端"、或手动重试时用）：
// 丢掉当前连接并立刻拨号，不走退避。
func (m *connManager) reconnectNow() {
	select {
	case m.inbox <- mIn{connID: m.connID, retry: true}:
	case <-m.done:
	}
}

func (m *connManager) scheduleReconnect() {
	if m.dial == nil || m.closed {
		return
	}
	delay := m.backoff
	if m.backoff < 30*time.Second {
		m.backoff *= 2
		if m.backoff > 30*time.Second {
			m.backoff = 30 * time.Second
		}
	}
	connID := m.connID
	go func() {
		time.Sleep(delay)
		conn, err := m.dial()
		if err != nil {
			select {
			case m.inbox <- mIn{connID: connID, dialErr: err}:
			default:
			}
			return
		}
		select {
		case m.inbox <- mIn{connID: connID, dialed: conn}:
		default:
		}
	}()
}

// failPending 断开时给未完成的命令回执：
//
//   - 还在队列里（没写出去）→ 未送达：界面还原草稿是对的；
//   - 已写出但没结果 → 结果未知：**不能**还原草稿（执行端可能已经执行了）。
//
// notify 把消息送进 inbox。
//
// **阻塞发送**，不用 select+default：管理器 run 永远在消费，而且它往 out 写是
// 非阻塞的，所以不会死锁；丢掉 written 会让命令被误判"未送达"（可能重复发送），
// 丢掉 err 会让管理器永远以为连接还在。
// postHost 把回调投递到界面线程，并绑定"发出时的执行端代际"。
func (m *connManager) postHost(fn func()) {
	if m.hostGen == nil {
		m.post(fn)
		return
	}
	gen := m.hostGen.Load()
	m.post(func() {
		if m.hostGen.Load() != gen {
			return // 已经切换到别的执行端：旧机器的包一律丢弃
		}
		fn()
	})
}

func (m *connManager) notify(in mIn) {
	select {
	case m.inbox <- in:
	case <-m.done:
	}
}

// drainAcks 先处理 inbox 里已经排到的"已写出"回执：它们可能比错误晚一步到达，
// 否则会把"已经写出去"的命令误判成"未送达"，让界面还原草稿、导致重复发送。
func (m *connManager) drainAcks() {
	var deferred []mIn
	for {
		select {
		case in := <-m.inbox:
			if in.connID != m.connID {
				continue // 过期连接的消息：丢弃
			}
			if in.written != "" {
				if _, ok := m.pending[in.written]; ok {
					m.pending[in.written] = cmdSent
				}
				continue
			}
			// 其它消息（尤其是 cmd.result）**不能丢**：丢了会把已经有结果的
			// 命令判成"结果未知"，newSession 的 id 之类 Data 也会丢。
			deferred = append(deferred, in)
		default:
			for _, remaining := range deferred {
				m.handleIn(remaining)
			}
			return
		}
	}
}

// dropCurrentConn 关闭当前连接并等待管理器处理完（切换执行端时先做这一步，
// 保证之后不会再有旧机器的包被 post 出去）。
func (m *connManager) dropCurrentConn() {
	ack := make(chan error, 1)
	select {
	case m.reqs <- mReq{kind: "dropConn", ack: ack}:
	case <-m.done:
		return
	}
	<-ack
}

// resetPendingUnknown 在"切换执行端"时结算待确认的命令：
// 它们是在**旧机器**上发出的，结果一律算未知（不能重放），草稿由界面按旧执行端还原。
func (m *connManager) resetPendingUnknown() {
	select {
	case m.reqs <- mReq{kind: "resetHost"}:
	case <-m.done:
	}
}

func (m *connManager) failPending() {
	for id, state := range m.pending {
		if state == cmdSent {
			m.post(func() {
				m.deliverResult(CmdResult{CmdID: id, Error: "连接中断，命令结果未知", Unknown: true})
			})
			continue
		}
		m.failCmd(id, "连接中断，命令未送达")
	}
	m.pending = map[string]cmdState{}
}

func (m *connManager) failCmd(cmdID, reason string) {
	if cmdID == "" {
		return
	}
	m.post(func() {
		m.deliverResult(CmdResult{CmdID: cmdID, Error: reason})
	})
}

func mustEncode(value any) []byte {
	body, _ := wire.Encode(value)
	return body
}

// effectiveLimit 返回发送队列上限（参数优先，其次全局测试值，最后默认 2000）。
func effectiveLimit(limit int) int {
	if limit > 0 {
		return limit
	}
	return outQueueLimit()
}
