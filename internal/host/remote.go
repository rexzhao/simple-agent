package host

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/wire"
)

// 协议消息名（见 docs/remote-design.md §12）。
const (
	msgHello     = "hello"
	msgWelcome   = "welcome"
	msgCatalog   = "catalog"
	msgModels    = "catalog.models"
	msgAttach    = "attach"
	msgLogBegin  = "log.begin"
	msgLogEvents = "log.events"
	msgLogEnd    = "log.end"
	msgState     = "state.snapshot"
	msgStream    = "stream"
	msgNotice    = "notice"
	msgEvent     = "event"
	msgCmd       = "cmd"
	msgCmdResult = "cmd.result"
	msgFragment  = "fragment"

	// blob（P3）：展示端上传/按需拉取图片
	msgBlobPut    = "blob.put"
	msgBlobCommit = "blob.commit"
	msgBlobGet    = "blob.get"
	msgBlobData   = "blob.data"
	msgBlobOK     = "blob.ok"
	msgBlobError  = "blob.error"
)

// outQueueLimit 是每条连接的发送队列上限（按包数）。
//
// 满了就断开该展示端（丢包会破坏"有序"这个传输层前提），它会走断线续传追上来。
var outQueueLimitValue atomic.Int32

func outQueueLimit() int {
	if value := int(outQueueLimitValue.Load()); value > 0 {
		return value
	}
	return 2000
}

// SetOutQueueLimitForTest 调整发送队列上限（测试用：用小值制造溢出）。
func SetOutQueueLimitForTest(limit int) { outQueueLimitValue.Store(int32(limit)) }

// Observer 是执行端"把变化告诉传输层"的钩子（单机模式下不设置）。
type Observer interface {
	Record(sessionID string, event store.Event)
	// StreamDelta 的 seq 与快照的 Seq **同一个数列**（设计 §3.3.1），
	// 展示端只接受 seq > snapshot.Seq 的增量。
	StreamDelta(sessionID, messageID string, ev providers.Event, seq uint64)
	Notice(sessionID string, notice Notice)
	SessionsChanged()
	CmdResult(result CmdResult)
	StateChanged(sessionID string)
}

// ---- 状态快照 ----

// ViewState 是一个会话的**瞬时**状态（快照与增量都用它）。
type ViewState struct {
	Epoch      string              `json:"epoch"`
	Generation uint64              `json:"generation"`
	Seq        uint64              `json:"seq"`
	Busy       string              `json:"busy"`
	Queued     []string            `json:"queued"`
	Asks       []AskView           `json:"asks"`
	Usage      UsageView           `json:"usage"`
	Counts     CountsView          `json:"counts"`
	Selection  providers.Selection `json:"selection"`
	Effort     string              `json:"effort"`
	Vision     bool                `json:"vision"`
	Window     int                 `json:"window"`
	Tokens     int                 `json:"tokens"`
	Compacting bool                `json:"compacting"`
	// Inflight 是正在流式输出的消息的完整 parts（中途连上时补内容用）。
	Inflight []InflightView `json:"inflight,omitempty"`
	// Notices 是已有的显示型提示（按 ID 去重）。
	Notices []Notice `json:"notices,omitempty"`
	// Todos 是当前任务清单（展示端据此渲染清单面板）。
	//
	// 清单是会话状态、不在消息里，所以快照必须单独带上；否则展示端只能靠
	// todo.write 事件增量看到它，中途连上就永远看不到已有清单。
	Todos []TodoView `json:"todos,omitempty"`
}

// TodoView 是任务清单项的快照表示。
type TodoView struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// InflightView 是一条正在流式输出的消息。
type InflightView struct {
	MessageID string         `json:"message_id"`
	Parts     []message.Part `json:"parts"`
}

// AskView 是提问面板要展示的内容。
type AskView struct {
	MessageID string              `json:"message_id"`
	CallID    string              `json:"call_id"`
	Questions []toolsQuestionView `json:"questions"`
}

// toolsQuestionView 是提问项的精简表示（避免外部依赖 tools 的完整结构）。
type toolsQuestionView struct {
	ID          string   `json:"id"`
	Question    string   `json:"question"`
	Header      string   `json:"header"`
	MultiSelect bool     `json:"multi_select"`
	Options     []string `json:"options,omitempty"`
}

// UsageView 是 token 用量。
type UsageView struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// CountsView 是轮数/步数。
type CountsView struct {
	Turns int `json:"turns"`
	Steps int `json:"steps"`
}

// Snapshot 生成一个会话的瞬时状态快照。
func (l *Local) Snapshot(sessionID string) (ViewState, bool) {
	session := l.Session(sessionID)
	if session == nil {
		return ViewState{}, false
	}
	view := ViewState{
		Epoch:      l.epoch,
		Generation: session.generation,
		Seq:        l.currentStreamSeq(sessionID),
		Busy:       string(session.Busy()),
		Queued:     session.Queued(),
		Usage:      UsageView{Input: session.Usage().InputTokens, Output: session.Usage().OutputTokens},
		Counts:     CountsView{Turns: session.Counts().Turns, Steps: session.Counts().Steps},
		Selection:  session.Selection(),
		Effort:     session.Effort(),
		Vision:     session.Vision(),
		Window:     session.EffectiveContextWindow(),
		Tokens:     session.TokensInUse(),
		Compacting: session.Compacting(),
	}
	for _, todo := range session.Todos() {
		view.Todos = append(view.Todos, TodoView{Content: todo.Content, Status: todo.Status})
	}
	for _, group := range session.AskGroups() {
		ask := AskView{MessageID: group.MessageID, CallID: group.CallID}
		for _, question := range group.Questions {
			options := make([]string, 0, len(question.Options))
			for _, option := range question.Options {
				options = append(options, option.Label)
			}
			ask.Questions = append(ask.Questions, toolsQuestionView{
				ID: question.ID, Question: question.Question, Header: question.Header,
				MultiSelect: question.MultiSelect, Options: options,
			})
		}
		view.Asks = append(view.Asks, ask)
	}
	// 正在流式输出的消息：中途连上的展示端要靠它补齐"已经输出的那一半"
	for _, entry := range session.Conversation().Entries() {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		if entry.Message.Status() != conversation.StatusStreaming {
			continue
		}
		view.Inflight = append(view.Inflight, InflightView{
			MessageID: entry.Message.Message.MessageID,
			Parts:     append([]message.Part(nil), entry.Message.Message.Parts...),
		})
	}
	l.mu.Lock()
	view.Notices = append([]Notice(nil), l.notices[sessionID]...)
	l.mu.Unlock()
	return view, true
}

// AttachResult 是一次 attach 的结果。
type AttachResult struct {
	Events []store.Event
	// Truncated 表示这批事件是**全量**（展示端据此重建，而不是续传）。
	Truncated bool
	// ToSeq 是这批全量事件对应的 **eventSeq**。游标必须落在这个数列上，
	// 不能落在日志文件的 seq 上（日志 seq 压缩后会从 0 重置）。
	ToSeq    uint64
	Snapshot ViewState
}

// Attach 返回展示端需要的持久事件（增量或全量）与瞬时快照。
//
// 游标是 (epoch, generation, seq)：epoch 变了（执行端重启）或 generation 变了
// （压缩过）或 ring 覆盖不到，都走全量。
func (l *Local) Attach(sessionID, epoch string, generation, seq uint64) (AttachResult, bool) {
	session := l.Session(sessionID)
	if session == nil {
		return AttachResult{}, false
	}
	snapshot, _ := l.Snapshot(sessionID)
	if epoch == l.epoch && generation != ^uint64(0) && generation == session.generation {
		if ring := l.ringEvents(sessionID, seq); ring != nil {
			return AttachResult{
				Events: ring, ToSeq: l.currentEventSeq(sessionID), Snapshot: snapshot,
			}, true
		}
	}
	// 全量：从日志读全部事件（快照文件本身就是新的起点）
	if session.journal != nil {
		if loaded, err := session.journal.Load(); err == nil {
			return AttachResult{
				Events: loaded.Events, Truncated: true,
				ToSeq: l.currentEventSeq(sessionID), Snapshot: snapshot,
			}, true
		}
	}
	// 内存后端（没有 JSONL）给不出完整历史：只能给瞬时快照 + 后续增量。
	// 远程模式**建议**执行端配置 data_dir；纯内存会话在长断线后重建不了历史，
	// 这一点在设计 §3.2 与 §10 里写明了。
	return AttachResult{Truncated: true, ToSeq: l.currentEventSeq(sessionID), Snapshot: snapshot}, true
}

// RingInfoForTest 返回 ring 的 (条数, 最早 seq, 当前 eventSeq)（测试用）。
func (l *Local) RingInfoForTest(sessionID string) (int, uint64, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ring := l.rings[sessionID]
	var first uint64
	if len(ring) > 0 {
		first = ring[0].Seq
	}
	return len(ring), first, l.eventSeq[sessionID]
}

// Persistent 报告执行端是否有持久化日志。
func (l *Local) Persistent() bool { return l.cfg.Catalog != nil }

// warnOnce 只打一次同样的启动警告。
func (l *Local) warnOnce(text string) {
	l.mu.Lock()
	if l.warned[text] {
		l.mu.Unlock()
		return
	}
	if l.warned == nil {
		l.warned = map[string]bool{}
	}
	l.warned[text] = true
	l.mu.Unlock()
	log.Print(text)
}

// Epoch 返回本次运行的实例 id（重启会变；续传游标必须带它）。
func (l *Local) Epoch() string { return l.epoch }

// currentEventSeq 返回该会话当前的持久事件序号（全量同步后游标落在这里）。
func (l *Local) currentEventSeq(sessionID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.eventSeq[sessionID]
}

// ringEvents 返回 ring 里 seq 之后的事件；覆盖不到返回 nil。
func (l *Local) ringEvents(sessionID string, after uint64) []store.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	ring := l.rings[sessionID]
	if len(ring) == 0 {
		if after == 0 {
			return []store.Event{}
		}
		return nil
	}
	// ring 里最早的 seq 必须 <= after+1，否则中间有缺口
	if ring[0].Seq > after+1 {
		return nil
	}
	out := make([]store.Event, 0, len(ring))
	for _, event := range ring {
		if event.Seq > after {
			out = append(out, event)
		}
	}
	return out
}

// recordWireEvent 给一条持久事件打序号、写进 ring，并返回带序号的事件副本。
//
// 打序号必须在这里（执行端），不能放在连接层：断线期间没有连接，但事件照样产生、
// 照样要进 ring，否则重连时会被误判成"没有缺口"，静默丢消息。
func (l *Local) recordWireEvent(sessionID string, event store.Event) store.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.eventSeq[sessionID]++
	stamped := event
	stamped.Seq = l.eventSeq[sessionID]
	ring := append(l.rings[sessionID], stamped)
	limit := l.cfg.RingLimit
	if limit <= 0 {
		limit = 2000
	}
	if len(ring) > limit {
		ring = ring[len(ring)-limit:]
	}
	l.rings[sessionID] = ring
	return stamped
}

// resetRing 在压缩（generation 变化）时清空 ring：新代事件不能与旧代混用。
func (l *Local) resetRing(sessionID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.rings, sessionID)
}

// nextStreamSeq 给瞬时增量打序号（与快照 Seq 同一个数列）。
func (l *Local) nextStreamSeq(sessionID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.streamSeq[sessionID]++
	return l.streamSeq[sessionID]
}

// currentStreamSeq 返回该会话当前的瞬时序号（快照用）。
func (l *Local) currentStreamSeq(sessionID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.streamSeq[sessionID]
}

// ---- 执行端服务：serve ----

// Serve 在执行端处理一条 control 连接（展示端连过来之后调用）。
func Serve(conn wire.Conn, local *Local, post func(func())) error {
	return ServeLimit(conn, local, post, 0)
}

// ServeLimit 与 Serve 相同，但可以指定发送队列上限（测试/受限连接用）。
func ServeLimit(conn wire.Conn, local *Local, post func(func()), queueLimit int) error {
	if !local.Persistent() {
		// 没有 data_dir 就是内存后端：远程展示端只能拿到"从现在起"的状态，
		// 断线久了重建不了历史。启动时把这件事说清楚，别让用户以为数据丢了。
		local.warnOnce("会话未启用持久化（未配置 data_dir）：远程展示端无法同步更早的历史")
	}
	server := &execServer{
		conn: conn, local: local, post: post, reasm: wire.NewReassembler(),
		out: make(chan execItem, effectiveLimit(queueLimit)), done: make(chan struct{}),
		putDone: map[string][]byte{},
		maxOut:  effectiveLimit(queueLimit),
	}
	go server.writer()
	local.AddObserver(server)
	defer local.RemoveObserver(server)
	err := server.loop()
	server.close()
	return err
}

type execServer struct {
	conn  wire.Conn
	local *Local
	post  func(func())
	reasm *wire.Reassembler

	// blob 暂存：展示端上传的分块拼好后按 hash 等 commit。
	putDone map[string][]byte

	// out 是**发送队列**：观察者回调（在主线程上）只编码入队，绝不碰 socket。
	// 网络卡住的展示端不能冻结执行端界面、也不能拖住其它端。
	// 大块数据（日志页、图片）作为**一个** execItem.large 入队，写 goroutine 再切块。
	out      chan execItem
	done     chan struct{}
	closeOne sync.Once
	// maxOut 是队列上限（按包数），满了直接断开该展示端（丢包会破坏有序前提）。
	maxOut int
}

// execItem 是执行端发送队列的一项：普通包，或一条待切块的逻辑大消息。
type execItem struct {
	packet wire.Packet
	large  *largePayload
}

// writer 是唯一的写 socket 的地方。
func (s *execServer) writer() {
	for {
		select {
		case <-s.done:
			return
		case item := <-s.out:
			if item.large != nil {
				if err := s.sendLarge(item.large); err != nil {
					s.close()
					return
				}
				continue
			}
			netLog("⇒", "viewer", uint64(item.packet.Conn), item.packet.Kind, len(item.packet.Body))
			if err := s.conn.Send(item.packet); err != nil {
				s.close()
				return
			}
		}
	}
}

// close 关闭连接（幂等）：队列溢出、写失败、Serve 返回都会走到这里。
func (s *execServer) close() {
	s.closeOne.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}

func (s *execServer) loop() error {
	for {
		packet, err := s.conn.Recv()
		if err != nil {
			return nil
		}
		netLog("⇐", "viewer", uint64(packet.Conn), packet.Kind, len(packet.Body))
		if packet.Kind == msgFragment {
			var fragment wire.Fragment
			if err := wire.Decode(packet.Body, &fragment); err != nil {
				continue
			}
			if kind, payload, done, err := s.reasm.Add(fragment); err == nil && done {
				body := payload
				// 用分块自带的 kind 分发（不再一律当 cmd：blob/日志都是这条通道）
				s.run(func() { s.handle(wire.Packet{Conn: packet.Conn, Kind: kind, Body: body}) })
			}
			continue
		}
		// 所有处理都回到主线程：Snapshot/Submit 都会读 driver 的状态，
		// 在网络 goroutine 里读会与流事件竞争。
		body, kind, connID := packet.Body, packet.Kind, packet.Conn
		s.run(func() { s.handle(wire.Packet{Conn: connID, Kind: kind, Body: body}) })
	}
}

// run 把处理投递到执行端主线程（没有 Post 时直接执行，用于单线程测试）。
func (s *execServer) run(fn func()) {
	if s.post != nil {
		s.post(fn)
		return
	}
	fn()
}

func (s *execServer) handle(packet wire.Packet) {
	switch packet.Kind {
	case msgHello:
		var hello wire.Hello
		_ = wire.Decode(packet.Body, &hello)
		capability := []string{"bulk"}
		// 权限随握手告诉展示端：展示端不读执行端的配置，猜不出来。
		if s.local.AllowShell() {
			capability = append(capability, "allow_shell")
		}
		_ = s.send(msgWelcome, wire.Welcome{
			Protocol: wire.Protocol, HostID: s.local.epoch, HostName: "local",
			Capability: capability,
		})
	case msgCatalog:
		_ = s.send(msgCatalog, s.local.Sessions())
		// 模型/档位列表只存在于执行端（API Key 与模型配置都在那边），
		// 展示端不能按自己的配置显示，否则切模型的菜单会是错的。
		_ = s.send(msgModels, s.local.Models())
	case msgAttach:
		var request struct {
			SessionID  string `json:"session_id"`
			Epoch      string `json:"epoch"`
			Generation uint64 `json:"generation"`
			Seq        uint64 `json:"seq"`
			Resume     bool   `json:"resume"`
		}
		if err := wire.Decode(packet.Body, &request); err != nil {
			return
		}
		generation := request.Generation
		if !request.Resume {
			generation = ^uint64(0) // 首次同步：强制全量
		}
		if !s.local.Persistent() {
			s.local.notifyNoPersistence(request.SessionID)
		}
		result, ok := s.local.Attach(request.SessionID, request.Epoch, generation, request.Seq)
		events, truncated, snapshot := result.Events, result.Truncated, result.Snapshot
		if !ok {
			// 内存后端（没有 JSONL）拿不出完整历史：明确告知，不要假装同步成功
			_ = s.send(msgLogBegin, map[string]any{
				"session_id": request.SessionID, "truncated": true,
				"generation": snapshot.Generation, "epoch": s.local.epoch,
				"error": "执行端没有可用的会话日志（远程模式要求配置 data_dir）",
			})
			return
		}
		_ = s.send(msgLogBegin, map[string]any{
			"session_id": request.SessionID, "truncated": truncated,
			"generation": snapshot.Generation, "epoch": s.local.epoch,
			"persistent": s.local.Persistent(),
			"to_seq":     result.ToSeq,
		})
		body, _ := wire.Encode(map[string]any{"session_id": request.SessionID, "events": events})
		_ = s.enqueueLarge(msgLogEvents, "log-"+request.SessionID, body)
		_ = s.send(msgLogEnd, map[string]any{
			"session_id": request.SessionID, "generation": snapshot.Generation,
			"to_seq": result.ToSeq,
		})
		_ = s.send(msgState, map[string]any{"session_id": request.SessionID, "state": snapshot})
	case msgBlobPut:
		s.handleBlobPutChunk(packet.Body)
	case msgBlobCommit:
		s.handleBlobCommit(packet.Body)
	case msgBlobGet:
		s.handleBlobGet(packet.Body)
	case msgCmd:
		var cmd Command
		if err := wire.Decode(packet.Body, &cmd); err != nil {
			return
		}
		if s.post != nil {
			s.post(func() { _, _ = s.local.Submit(cmd) })
		} else {
			_, _ = s.local.Submit(cmd)
		}
	}
}

// ---- Observer 实现：把执行端的变化推给展示端 ----

func (s *execServer) Record(sessionID string, event store.Event) {
	// 序号与 ring 都由执行端（Local）负责；这里只转发。
	_ = s.send(msgEvent, map[string]any{"session_id": sessionID, "event": event})
}

func (s *execServer) StreamDelta(sessionID, messageID string, ev providers.Event, seq uint64) {
	_ = s.send(msgStream, map[string]any{
		"session_id": sessionID, "message_id": messageID, "event": ev, "seq": seq,
	})
}

func (s *execServer) CmdResult(result CmdResult) {
	_ = s.send(msgCmdResult, result)
}

func (s *execServer) StateChanged(sessionID string) {
	if snapshot, ok := s.local.Snapshot(sessionID); ok {
		_ = s.send(msgState, map[string]any{"session_id": sessionID, "state": snapshot})
	}
}

func (s *execServer) Notice(sessionID string, notice Notice) {
	_ = s.send(msgNotice, map[string]any{"session_id": sessionID, "notice": notice})
}

func (s *execServer) SessionsChanged() {
	_ = s.send(msgCatalog, s.local.Sessions())
	_ = s.send(msgModels, s.local.Models())
}

// send 只入队；队列满就断开这个展示端（它会走断线续传追上来）。
func (s *execServer) send(kind string, payload any) error {
	body, err := wire.Encode(payload)
	if err != nil {
		return err
	}
	return s.enqueue(wire.Packet{Kind: kind, Body: body})
}

// enqueueLarge 把一条逻辑大消息作为一个队列项入队（写 goroutine 再切块）。
func (s *execServer) enqueueLarge(kind, msgID string, payload []byte) error {
	return s.enqueueItem(execItem{large: &largePayload{kind: kind, msgID: msgID, payload: payload}})
}

// sendLarge 在写 goroutine 里把一条逻辑消息切块发出。
func (s *execServer) sendLarge(large *largePayload) error {
	for _, fragment := range wire.FragmentsKind(large.kind, large.msgID, large.payload, wire.DefaultMaxPacket) {
		raw, err := wire.Encode(fragment)
		if err != nil {
			return err
		}
		if err := s.conn.Send(wire.Packet{Kind: msgFragment, Body: raw}); err != nil {
			return err
		}
	}
	return nil
}

func (s *execServer) enqueue(packet wire.Packet) error {
	return s.enqueueItem(execItem{packet: packet})
}

// enqueueItem 是队列的唯一入口（普通包与大消息共用）。
func (s *execServer) enqueueItem(item execItem) error {
	select {
	case <-s.done:
		return wire.ErrClosed
	default:
	}
	select {
	case s.out <- item:
		return nil
	default:
		// 队列满：断开这个展示端，而不是丢包（有序是传输层的前提）
		s.close()
		return wire.ErrClosed
	}
}

// ---- 展示端：Remote ----

// Remote 是展示端的 Host 实现：读本地镜像，写经连接提交命令。
type Remote struct {
	mgr   *connManager
	post  func(func())
	subs  *subscribers
	reasm *wire.Reassembler
	// hostGen 是"当前执行端"的代际：切换执行端时 +1，旧机器迟到的包会被丢弃。
	hostGen atomic.Uint64
	// connectedState 是给界面读的原子快照（真正的连接状态归 connManager 所有）。
	connectedState atomic.Bool

	mu       sync.Mutex
	sessions map[string]*Session
	order    []string
	// closed 只用于 Close 幂等。
	closed bool
	// cursors 是每个会话的续传游标 (generation, seq)。
	cursors map[string]cursor
	// attachCount 记录每个会话收到过几次 log.begin（测试用：等重连同步完成）。
	attachCount map[string]int
	// lastTruncated 记录最近一次 attach 是否走了全量（测试用）。
	lastTruncated map[string]bool
	// lastError 是最近一条协议/解码错误（测试用）。
	lastError string
	// noticeSeen 记录已合并的提示（按 id 去重，重连不会重复）。
	noticeSeen map[string]bool
	// blobs 是展示端的图片服务（上传转发 + 按需拉取 + LRU）。
	blobs *remoteBlobs
	// pendingBlob 暂存执行端回传、等待 blob.ok 的分块数据。
	pendingBlob map[string][]byte
	// attachSpecs 是"重连时要 attach 的会话与游标"（界面线程写，发布给管理器）。
	attachSpecs []map[string]any
	// epoch 是执行端实例 id（握手时拿到；重启后会变，届时一律全量）。
	epoch string
	// allowShell 是执行端的 allow_shell（来自握手的能力标记）。
	// 展示端不能用本地配置显示权限，否则标签会与执行端实际情况不符。
	allowShell bool
	// models 是执行端公布的模型/档位列表（catalog.models 消息）。
	// 展示端不自己算：API Key 与模型配置只在执行端。
	models []ProviderChoice
	// synced 记录"这个会话至少完成过一次同步"（游标为 0 时也能续传）。
	synced map[string]bool
	// fullSync 标记"这个会话正在做全量同步"：其间的事件不推进游标，
	// 游标在 log.end 时用 toSeq 一次性落定。
	fullSync map[string]bool
	// activeID 是展示端当前看着的会话（纯界面状态）。
	activeID string
	// noHistoryWarned 记录已经提示过"执行端没有持久化"的会话（每会话一次）。
	noHistoryWarned map[string]bool
}

// cursor 是持久状态的续传游标 (epoch, generation, seq)。
//
// epoch 不能省：执行端重启后 eventSeq 从 1 重新开始，只比 generation/seq 会把
// "新进程的 1..k" 当成"旧进程已经收到过的区间"，静默丢消息。
type cursor struct {
	epoch      string
	generation uint64
	seq        uint64
}

// NewRemote 连接一个执行端并同步全部会话（只读）。
// NewRemoteWithDial 连上执行端，并在断线时自动重连（3s 起，指数退避到 30s）。

// NewRemoteWithDialer 用自定义拨号函数构造展示端（测试可注入进程内管道）。

// handleDisconnect 断线后按退避重连；游标保留，重连只补增量。

// resyncAll 重新握手并按游标 attach 所有会话。

// Connected 报告当前是否连着执行端（界面用来禁用输入/显示横幅）。

// currentConn 取当前连接（Reconnect 会替换它）。

// decodeResultData 把 JSON 解出来的 map 还原成具体类型（Data 是 any）。
func decodeResultData(result CmdResult) any {
	if result.Data == nil || result.Error != "" {
		return result.Data
	}
	body, err := wire.Encode(result.Data)
	if err != nil {
		return result.Data
	}
	switch result.Kind {
	case "loadOlder":
		var page OlderPage
		if err := wire.Decode(body, &page); err == nil {
			return page
		}
	case "newSession":
		var data NewSessionData
		if err := wire.Decode(body, &data); err == nil {
			return data
		}
	}
	return result.Data
}

func (r *Remote) handle(packet wire.Packet) {
	if packet.Kind == msgBlobOK || packet.Kind == msgBlobError {
		var payload struct {
			Hash  string `json:"hash"`
			Error string `json:"error"`
		}
		_ = wire.Decode(packet.Body, &payload)
		data := r.pendingBlob[payload.Hash]
		delete(r.pendingBlob, payload.Hash)
		var err error
		if packet.Kind == msgBlobError {
			err = errors.New(payload.Error)
		}
		if r.blobs != nil {
			r.blobs.deliverBlob(payload.Hash, data, err)
		}
		return
	}
	switch packet.Kind {
	case msgCatalog:
		var refs []SessionRef
		if err := wire.Decode(packet.Body, &refs); err != nil {
			return
		}
		r.applyCatalog(refs)
		// 侧栏靠这条通知刷新：只发 "conversation" 的话界面永远不会重建会话列表
		r.subs.emit(Change{Kind: "sessions"})
	case msgModels:
		var models []ProviderChoice
		if err := wire.Decode(packet.Body, &models); err != nil {
			r.noteError("catalog.models: " + err.Error())
			return
		}
		r.applyModels(models)
	case msgWelcome:
		var payload wire.Welcome
		if err := wire.Decode(packet.Body, &payload); err != nil {
			return
		}
		r.epoch = payload.HostID
		r.applyCapability(payload.Capability)
	case msgLogBegin:
		var payload struct {
			SessionID  string `json:"session_id"`
			Truncated  bool   `json:"truncated"`
			Generation uint64 `json:"generation"`
			Epoch      string `json:"epoch"`
			Persistent *bool  `json:"persistent"`
		}
		if err := wire.Decode(packet.Body, &payload); err != nil {
			return
		}
		r.lastTruncated[payload.SessionID] = payload.Truncated
		r.attachCount[payload.SessionID]++
		r.fullSync[payload.SessionID] = payload.Truncated
		if payload.Epoch != "" {
			r.epoch = payload.Epoch
		}
		cursor := r.cursors[payload.SessionID]
		cursor.epoch = payload.Epoch
		cursor.generation = payload.Generation
		if payload.Truncated {
			// 全量同步：中间事件只重建状态，不推进游标
			cursor.seq = 0
		}
		r.cursors[payload.SessionID] = cursor
	case msgLogEnd:
		var payload struct {
			SessionID string `json:"session_id"`
			ToSeq     uint64 `json:"to_seq"`
		}
		if err := wire.Decode(packet.Body, &payload); err != nil {
			return
		}
		r.fullSync[payload.SessionID] = false
		r.synced[payload.SessionID] = true
		cursor := r.cursors[payload.SessionID]
		// 游标落在**执行端的 eventSeq** 上（不是日志文件的 seq）
		if payload.ToSeq > cursor.seq {
			cursor.seq = payload.ToSeq
		}
		r.cursors[payload.SessionID] = cursor
		r.publishAttach()
	case msgLogEvents:
		var payload struct {
			SessionID string        `json:"session_id"`
			Events    []store.Event `json:"events"`
		}
		if err := wire.Decode(packet.Body, &payload); err != nil {
			r.noteError("log.events: " + err.Error())
			return
		}
		r.applyEvents(payload.SessionID, payload.Events)
	case msgEvent:
		var payload struct {
			SessionID string      `json:"session_id"`
			Event     store.Event `json:"event"`
		}
		if err := wire.Decode(packet.Body, &payload); err != nil {
			return
		}
		r.applyEvents(payload.SessionID, []store.Event{payload.Event})
	case msgState:
		var payload struct {
			SessionID string    `json:"session_id"`
			State     ViewState `json:"state"`
		}
		if err := wire.Decode(packet.Body, &payload); err != nil {
			return
		}
		r.applyState(payload.SessionID, payload.State)
	case msgStream:
		var payload struct {
			SessionID string          `json:"session_id"`
			MessageID string          `json:"message_id"`
			Event     providers.Event `json:"event"`
		}
		if err := wire.Decode(packet.Body, &payload); err != nil {
			return
		}
		if session := r.Session(payload.SessionID); session != nil && session.mirror != nil {
			_ = session.mirror.ApplyStreamDelta(payload.MessageID, payload.Event)
		}
	case msgNotice:
		var payload struct {
			SessionID string `json:"session_id"`
			Notice    Notice `json:"notice"`
		}
		if err := wire.Decode(packet.Body, &payload); err != nil {
			return
		}
		if session := r.Session(payload.SessionID); session != nil {
			_, _ = session.Conversation().AddNotice(conversation.NoticeLevel(payload.Notice.Level), payload.Notice.Text)
		}
		r.subs.emit(Change{SessionID: payload.SessionID, Kind: "notice", Notice: &payload.Notice})
	case msgCmdResult:
		var result CmdResult
		if err := wire.Decode(packet.Body, &result); err != nil {
			return
		}
		result.Data = decodeResultData(result)
		r.subs.emit(Change{Kind: "cmdResult", CmdID: result.CmdID, Result: result})
	}
	r.subs.emit(Change{Kind: "conversation"})
	// 游标/epoch 可能变了：把最新的 attach 计划发布给连接管理器（非阻塞）
	r.publishAttach()
}

// applyModels 记录执行端公布的模型列表，并让界面重绘一次选择器。
func (r *Remote) applyModels(models []ProviderChoice) {
	r.mu.Lock()
	r.models = models
	r.mu.Unlock()
	r.subs.emit(Change{Kind: "models"})
}

// Providers 返回执行端公布的模型/档位列表（还没同步到时为空）。
func (r *Remote) Providers() []ProviderChoice {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ProviderChoice(nil), r.models...)
}

// LoadOlder 同步取一页更早的归档（**会阻塞**：要等执行端回包）。
//
// 与 `loadOlder` **命令**的区别：命令是异步的，结果以 cmdResult 回到界面线程，
// Data 还是 any；有些调用方（Android 的 bridge）需要"调用即得结果"的形态，
// 又不能占界面线程，所以这里给一个阻塞版本，调用方自己在后台 goroutine 里用。
//
// 游标由**调用方**携带（`cursor` 为空表示从最近一份归档开始）：执行端的会话是
// 共享的，两个展示端各自翻页时不能共用一个"读到哪了"。
func (r *Remote) LoadOlder(sessionID, cursor string) (OlderPage, error) {
	session := r.Session(sessionID)
	if session == nil {
		return OlderPage{}, errors.New("会话不存在")
	}
	return session.loadOlder(cursor)
}

func (r *Remote) applyCatalog(refs []SessionRef) {
	var attachIDs []string
	r.mu.Lock()
	seen := map[string]bool{}
	for _, ref := range refs {
		seen[ref.ID] = true
		if _, ok := r.sessions[ref.ID]; ok {
			continue
		}
		session := newMirrorSession(ref, r.post)
		r.sessions[ref.ID] = session
		r.order = append(r.order, ref.ID)
		attachIDs = append(attachIDs, ref.ID)
	}
	for id := range r.sessions {
		if !seen[id] {
			delete(r.sessions, id)
			for i, existing := range r.order {
				if existing == id {
					r.order = append(r.order[:i], r.order[i+1:]...)
					break
				}
			}
		}
	}
	cursors := make([]cursor, len(attachIDs))
	for i, id := range attachIDs {
		cursors[i] = r.cursors[id]
	}
	r.mu.Unlock()

	// 锁外发布给连接管理器：它负责在（重新）连上时按游标 attach
	for i, id := range attachIDs {
		_ = cursors[i]
		spec := map[string]any{
			"session_id": id, "epoch": r.epoch,
			"generation": cursors[i].generation,
			"seq":        cursors[i].seq,
			"resume":     cursors[i].seq > 0 || cursors[i].generation > 0,
		}
		r.attachSpecs = append(r.attachSpecs, spec)
	}
	r.publishAttach()
}

func (r *Remote) applyEvents(sessionID string, events []store.Event) {
	session := r.Session(sessionID)
	if session == nil || session.mirror == nil {
		return
	}
	for _, event := range events {
		_ = session.mirror.ApplyPersisted(event)
		if r.fullSync[sessionID] {
			continue // 全量同步期间不推进游标（见 log.end 的 to_seq）
		}
		if event.Seq > r.cursors[sessionID].seq {
			cursor := r.cursors[sessionID]
			cursor.seq = event.Seq
			r.cursors[sessionID] = cursor
		}
	}
}

func (r *Remote) applyState(sessionID string, state ViewState) {
	session := r.Session(sessionID)
	if session == nil {
		return
	}
	session.view = &state
	// snapshot 里的显示型提示按 id 去重后补上（不落盘，但后连上来的端也要看到）
	for _, notice := range state.Notices {
		if r.noticeSeen[sessionID+"#"+notice.ID] {
			continue
		}
		r.noticeSeen[sessionID+"#"+notice.ID] = true
		// 按锚点插回原来的位置（找不到才落到末尾）
		_, _ = session.Conversation().InsertNoticeAfter(
			conversation.NoticeLevel(notice.Level), notice.Text, notice.After)
	}
	cursor := r.cursors[sessionID]
	cursor.generation = state.Generation
	r.cursors[sessionID] = cursor
	// 执行端说"已经空闲/没有在飞的回合"，但镜像里还有 streaming 的条目：
	// 说明终结事件在断线/切换期间丢了（或游标跳过），就地把它们收尾，
	// 否则工具卡片会永远显示 running、输入框也一直像"忙"。
	if state.Busy == "idle" && len(state.Inflight) == 0 && session.Conversation() != nil {
		for _, item := range session.Conversation().Entries() {
			if item.Message == nil || item.Message.Message == nil {
				continue
			}
			if item.Message.Status() == conversation.StatusStreaming {
				_ = session.Conversation().CancelStream(item.Message.Message.MessageID)
			}
		}
	}
	// 快照里的 Inflight 用来补齐"已经输出的那一半"（中途连上/重连）
	if session.mirror != nil {
		for _, inflight := range state.Inflight {
			_ = session.mirror.ReplaceInflight(inflight.MessageID, inflight.Parts, conversation.StatusStreaming)
		}
	}
	r.subs.emit(Change{SessionID: sessionID, Kind: "runtime"})
}

// Reconnect 换一条新连接并重新同步（游标保留：只补断线期间的增量）。

// AttachCountForTest 返回该会话收到过几次同步（测试用）。
func (r *Remote) AttachCountForTest(sessionID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attachCount[sessionID]
}

// CursorsForTest 返回续传游标（测试用）。
func (r *Remote) CursorsForTest(sessionID string) cursor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursors[sessionID]
}

// HasSeen 报告展示端是否已经收到该会话的同步（测试用）。
func (r *Remote) HasSeen(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursors[sessionID].seq > 0
}

// EpochForTest 返回握手得到的执行端 epoch（测试用）。
func (r *Remote) EpochForTest() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.epoch
}

// LastAttachTruncated 报告最近一次 attach 是否走了全量（测试用）。
func (r *Remote) LastAttachTruncated(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastTruncated[sessionID]
}

// noteError 记录解码/协议错误（测试与日志用）。
func (r *Remote) noteError(text string) {
	r.mu.Lock()
	r.lastError = text
	r.mu.Unlock()
}

// LastErrorForTest 返回最近一条协议错误（测试用）。
func (r *Remote) LastErrorForTest() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastError
}

// ---- Host 接口 ----

func (r *Remote) Info() HostInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return HostInfo{ID: "remote", Name: "remote", AllowShell: r.allowShell}
}

// applyCapability 记录握手带上来的执行端能力（目前只关心 allow_shell）。
func (r *Remote) applyCapability(capability []string) {
	allow := false
	for _, item := range capability {
		if item == "allow_shell" {
			allow = true
			break
		}
	}
	r.mu.Lock()
	changed := r.allowShell != allow
	r.allowShell = allow
	r.mu.Unlock()
	if changed {
		// 权限变了界面要重绘（"权限"标签就在输入卡上）
		r.subs.emit(Change{Kind: "runtime"})
	}
}

func (r *Remote) Sessions() []SessionRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SessionRef, 0, len(r.order))
	for _, id := range r.order {
		session := r.sessions[id]
		out = append(out, SessionRef{ID: id, Title: session.Title, Workspace: session.Workspace})
	}
	return out
}

func (r *Remote) Session(id string) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

func (r *Remote) Subscribe(fn func(Change)) func() {
	cancel := r.subs.add(fn)
	// 新订阅者立刻补一次"会话列表"：目录可能在订阅之前就到了
	// （界面连接 relay 比直连慢，很容易踩到），不补的话侧栏一直是空的。
	r.post(func() { r.subs.emit(Change{Kind: "sessions"}) })
	return cancel
}

// Active 返回展示端当前看着的会话（纯界面状态，不发命令）。
func (r *Remote) Active() *Session {
	r.mu.Lock()
	id := r.activeID
	r.mu.Unlock()
	if id == "" {
		return nil
	}
	return r.Session(id)
}

// SetActive 切换展示端当前会话。
func (r *Remote) SetActive(id string) {
	r.mu.Lock()
	r.activeID = id
	r.mu.Unlock()
}

// StartupError 在展示端没有意义（执行端的错误会经接口传过来）。
func (r *Remote) StartupError() string { return "" }

// newMirrorSession 构造展示端的一个镜像会话（只读）。
func newMirrorSession(ref SessionRef, post func(func())) *Session {
	replayed, _ := driver.Replay(nil)
	session := &Session{
		ID: ref.ID, Title: ref.Title, Workspace: ref.Workspace,
		// 默认按空闲处理：view 的空串不等于 idle，会让界面以为会话还在跑
		// （新建会话后 Stop 一直亮着、模型也切不了）。真正的快照到了会覆盖它。
		view: &ViewState{Busy: string(driver.BusyIdle)},
	}
	session.mirror = driver.NewMirror(&replayed, driver.Options{})
	session.conv = session.mirror.Conversation()
	return session
}
