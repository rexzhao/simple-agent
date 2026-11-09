package host

import (
	"errors"
	"fmt"

	"simple-agent/internal/wire"
)

// NewRemote 连接一个执行端并同步全部会话。
func NewRemote(conn wire.Conn, post func(func())) (*Remote, error) {
	return newRemote(conn, nil, post)
}

// NewRemoteToken 与 NewRemote 相同，但握手时带上执行端的 token（`--listen` 直连）。
func NewRemoteToken(conn wire.Conn, post func(func()), token string) (*Remote, error) {
	return newRemoteToken(conn, nil, post, token)
}

// NewRemoteWithDial 连上执行端，并在断线时自动重连（3s 起，指数退避到 30s）。
func NewRemoteWithDial(addr string, post func(func())) (*Remote, error) {
	return newRemoteWithDialer(func() (wire.Conn, error) { return wire.Dial(addr) }, post)
}

// NewRemoteWithToken 与 NewRemoteWithDialer 相同，但握手时带上执行端的 token。
func NewRemoteWithToken(dial func() (wire.Conn, error), post func(func()), token string) (*Remote, error) {
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	return newRemoteToken(conn, dial, post, token)
}

// NewRemoteWithDialer 用自定义拨号函数构造展示端（测试可注入进程内管道）。
func NewRemoteWithDialer(dial func() (wire.Conn, error), post func(func())) (*Remote, error) {
	return newRemoteWithDialer(dial, post)
}

func newRemoteWithDialer(dial func() (wire.Conn, error), post func(func())) (*Remote, error) {
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	return newRemote(conn, dial, post)
}

func newRemote(conn wire.Conn, dial func() (wire.Conn, error), post func(func())) (*Remote, error) {
	return newRemoteToken(conn, dial, post, "")
}

// newRemoteToken 与 newRemote 相同，但握手时带上执行端要求的 token。
//
// token 必须在**构造时**给出：构造过程里就会发出第一条 hello，
// 之后再 SetToken 就晚了（那条 hello 已经被执行端拒掉）。
func newRemoteToken(conn wire.Conn, dial func() (wire.Conn, error), post func(func()), token string) (*Remote, error) {
	if post == nil {
		post = func(fn func()) { fn() }
	}
	remote := &Remote{
		post: post, subs: newSubscribers(),
		reasm: wire.NewReassembler(), sessions: map[string]*Session{},
		cursors: map[string]cursor{}, lastTruncated: map[string]bool{}, attachCount: map[string]int{},
		noticeSeen: map[string]bool{}, fullSync: map[string]bool{},
		noHistoryWarned: map[string]bool{}, synced: map[string]bool{},
	}
	remote.blobs = newRemoteBlobs(nil, blobCacheBudget)
	remote.pendingBlob = map[string][]byte{}
	remote.mgr = newConnManager(post, dial)
	remote.mgr.hostGen = &remote.hostGen
	remote.blobs.mgr = remote.mgr
	remote.mgr.onDisconnect = func() {
		// 断线唤醒所有等待中的图片请求，否则界面一直"正在加载"
		remote.blobs.FailAll(errors.New("连接中断，图片传输中止"))
	}
	remote.mgr.deliver = remote.deliver
	remote.mgr.deliverConnected = remote.deliverConnected
	remote.mgr.deliverResult = func(result CmdResult) {
		remote.subs.emit(Change{Kind: "cmdResult", CmdID: result.CmdID, Result: result})
	}
	remote.mgr.start(conn)
	remote.connectedState.Store(true)
	// 握手与拉会话列表也走队列（不阻塞界面线程）。
	// 如果连接已经完成过握手（relay.OpenViewer 分辨对端时发过 hello），就不要再发一次：
	// 执行端会回两个 welcome，epoch/catalog/attach 都可能被做两遍。
	handshaken := false
	if marker, ok := conn.(wire.Handshaken); ok && marker.Handshaken() {
		handshaken = true
	}
	remote.mgr.SetToken(token)
	if !handshaken {
		remote.mgr.submit(mItem{packet: wire.Packet{Kind: msgHello, Body: mustEncode(wire.Hello{
			Protocol: wire.Protocol, Role: "viewer", Purpose: "control",
			Credentials: wire.Credentials{Kind: "none", Token: token},
		})}})
	}
	remote.mgr.submit(mItem{packet: wire.Packet{Kind: msgCatalog}})
	return remote, nil
}

// deliver 在**界面线程**上处理一个已经收到的包（分片在这里重组）。
func (r *Remote) deliver(packet wire.Packet) {
	if packet.Kind == msgFragment {
		var fragment wire.Fragment
		if err := wire.Decode(packet.Body, &fragment); err != nil {
			return
		}
		if kind, payload, done, err := r.reasm.Add(fragment); err == nil && done {
			// 兼容旧发送方（没有 kind）：日志事件
			if kind == "" {
				kind = msgLogEvents
			}
			if kind == msgBlobData {
				// 数据分块到齐：按 blob.ok 再走一遍投递（等待方在管理器线程上被唤醒）
				r.pendingBlob[fragment.MsgID] = payload
				r.handle(wire.Packet{Kind: msgBlobOK, Body: mustEncode(map[string]any{"hash": fragment.MsgID})})
				return
			}
			r.handle(wire.Packet{Kind: kind, Body: payload})
		}
		return
	}
	r.handle(packet)
}

// deliverConnected 由连接管理器在（断线/重连）时投递到界面线程。
func (r *Remote) deliverConnected(connected bool) {
	r.connectedState.Store(connected)
	if connected {
		r.subs.emit(Change{Kind: "connection", Connected: true, Notice: &Notice{Level: "info", Text: "已重新连接"}})
		return
	}
	r.subs.emit(Change{Kind: "connection", Notice: &Notice{Level: "warn", Text: "已断开，正在重连…"}})
}

// publishAttach 用**当前游标**重建 attach 计划并发布给连接管理器（重连时用）。
func (r *Remote) publishAttach() {
	r.mu.Lock()
	specs := make([]map[string]any, 0, len(r.order))
	for _, id := range r.order {
		cursor := r.cursors[id]
		specs = append(specs, map[string]any{
			"session_id": id, "epoch": r.epoch,
			"generation": cursor.generation,
			"seq":        cursor.seq,
			"resume":     r.synced[id],
		})
	}
	r.mu.Unlock()
	if r.mgr != nil {
		r.mgr.setAttach(specs)
	}
}

// SetToken 设置连接执行端时使用的握手口令（`--connect` 直连执行端时用）。
//
// 与 relay 的 token 是两套：relay 那套在拨号时由 relay.OpenViewerDialer 带上，
// 这里这套用于**执行端自己的**握手校验。
//
// 必须在建立连接之前调用（`NewRemote*` 返回后、首条 hello 发出前）。
func (r *Remote) SetToken(token string) {
	if r.mgr != nil {
		r.mgr.SetToken(token)
	}
}

// Blobs 返回展示端的图片服务（上传转发到执行端，读取按需拉取 + LRU 缓存）。
func (r *Remote) Blobs() BlobService { return r.blobs }

// ResetForNewHost 切换执行端前清空所有"属于旧机器"的状态。
//
// 同一个 Remote 只换连接是不够的：旧执行端的会话、续传游标、已合并提示、完成的同步标记
// 都必须清掉，否则旧会话会挂在侧栏里，旧机器迟到的 cmdResult 也会被当成新机器的结果。
// 待确认的命令一律按"结果未知"结算（草稿会由界面按旧执行端的槽位还原）。
func (r *Remote) ResetForNewHost() {
	// 先加代际：之后所有旧机器的 post 回调都会被丢弃
	r.hostGen.Add(1)
	r.mu.Lock()
	r.sessions = map[string]*Session{}
	r.order = nil
	r.cursors = map[string]cursor{}
	r.attachCount = map[string]int{}
	r.lastTruncated = map[string]bool{}
	r.noticeSeen = map[string]bool{}
	r.synced = map[string]bool{}
	r.fullSync = map[string]bool{}
	r.noHistoryWarned = map[string]bool{}
	r.attachSpecs = nil
	r.activeID = ""
	r.epoch = ""
	r.models = nil
	r.pendingBlob = map[string][]byte{}
	r.mu.Unlock()
	if r.mgr != nil {
		r.mgr.resetPendingUnknown()
	}
	r.subs.emit(Change{Kind: "sessions"})
}

// DropConn 关闭当前连接并等管理器确认（切换执行端的第一步）。
func (r *Remote) DropConn() {
	if r.mgr != nil {
		r.mgr.dropCurrentConn()
	}
}

// RetryNow 立即重连（切换执行端/手动重试）；断线退避会被跳过。
func (r *Remote) RetryNow() {
	if r.mgr != nil {
		r.mgr.reconnectNow()
	}
}

// Connected 报告当前是否连着执行端（界面用来禁用输入/显示横幅）。
func (r *Remote) Connected() bool { return r.connectedState.Load() }

// Submit 提交一条命令：**只入队**，不写 socket、不等结果。
//
// 断线时立即失败（不补发：用户可能早就不想发了）；队列满/管理器忙也立即失败。
// 结果统一通过 cmdResult 回到界面线程。
func (r *Remote) Submit(cmd Command) (string, error) {
	if cmd.CmdID == "" {
		cmd.CmdID = defaultNewID()[:16]
	}
	body, err := wire.Encode(cmd)
	if err != nil {
		return cmd.CmdID, err
	}
	if !r.Connected() {
		return cmd.CmdID, fmt.Errorf("已断开，命令未发送")
	}
	item := mItem{packet: wire.Packet{Kind: msgCmd, Body: body}, cmdID: cmd.CmdID}
	if r.mgr == nil || !r.mgr.submit(item) {
		return cmd.CmdID, fmt.Errorf("客户端繁忙，命令未发送")
	}
	return cmd.CmdID, nil
}

// Reconnect 换一条新连接（测试/手动重连用）。
func (r *Remote) Reconnect(conn wire.Conn) error {
	if r.mgr == nil || !r.mgr.useConn(conn) {
		return fmt.Errorf("连接管理器繁忙")
	}
	return nil
}

// Close 关闭连接（幂等）。
func (r *Remote) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	if r.mgr != nil {
		r.mgr.close()
	}
	return nil
}

// NewRemoteDeferred 构造一个**尚未连接**的 Remote（展示端还没选执行端时用）：
// 内部用占位连接顶一下，随后 DropConn + RetryNow 才真正按 dial 建立连接。
func NewRemoteDeferred(dial func() (wire.Conn, error), post func(func())) (*Remote, error) {
	return NewRemoteDeferredToken(dial, post, "")
}

// NewRemoteDeferredToken 与 NewRemoteDeferred 相同，但握手带上执行端的 token。
func NewRemoteDeferredToken(dial func() (wire.Conn, error), post func(func()), token string) (*Remote, error) {
	remote, err := newRemoteToken(wire.NullConn(), dial, post, token)
	if err != nil {
		return nil, err
	}
	remote.mgr.dropCurrentConn()
	return remote, nil
}
