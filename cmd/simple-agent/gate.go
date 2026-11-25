package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"log"
	"net"
	"sync"
	"time"

	"simple-agent/internal/tlsx"
	"simple-agent/internal/trust"
	"simple-agent/internal/uiapp"
)

// viewerGate 决定"这条进来的 viewer 连接该不该被接受"。
//
// # 为什么需要它
//
// 执行端监听在局域网上，任何能连上来的人都能发 hello。token 挡了一层，
// 但 token 会泄漏（配置文件、屏幕截图、终端历史）。证书指纹是第二道：
// 只有**用户信任过的 viewer**才能接进来。
//
// # 为什么是非阻塞的
//
// 校验发生在 Accept 之后、交给 host.Serve 之前。用户可能要看弹窗、
// 想一会 —— 这期间：
//   - **不能**阻塞 Accept 循环（否则后面所有连接都排队等，包括新 viewer
//     的重连和本地直连）
//   - **不能**关掉这条连接要求重连（握手已完成，重来一遍用户体验很差，
//     而且弹窗期间对端会超时）
//
// 所以：连接先挂起（只完成 TLS 握手，不读业务数据），等用户决定后再
// 放行或断开。挂起超时就断开，避免大量半开连接堆积。
const gateTimeout = 2 * time.Minute

// gateRequest 是一条等待用户决定的连接。
type gateRequest struct {
	conn        net.Conn
	peerAddr    string
	fingerprint string
	// changed 表示这个对端以前信任过别的指纹（而不是首次见到）。
	// 界面据此用不同措辞：首次是"要信任它吗"，变了是"它跟上次不一样"。
	changed bool
	// known 是以前信任的指纹（changed 时有效，用于弹窗里并列展示）。
	known string
	// decided 由界面线程关闭：true = 放行，false = 拒绝
	decide chan bool
}

// PeerAddr 是对端地址（界面展示用）。
func (r *gateRequest) PeerAddr() string { return r.peerAddr }

// Fingerprint 是对端证书指纹（规范化后的十六进制）。
func (r *gateRequest) Fingerprint() string { return r.fingerprint }

// Changed 报告这个对端是不是"指纹变了"。
func (r *gateRequest) Changed() bool { return r.changed }

// Known 返回以前信任过的指纹（Changed 为真时有效）。
func (r *gateRequest) Known() string { return r.known }

// gate 管理所有"等待用户决定"的连接。
type gate struct {
	mu      sync.Mutex
	pending []*gateRequest
	// notify 通知界面"有新的待确认连接"（界面据此弹窗）
	notify func()
}

// newGate 构造 gate；notify 可以为 nil（没有界面时直接拒绝未知对端）。
func newGate(notify func()) *gate {
	return &gate{notify: notify}
}

// review 阻塞等待用户对这条连接的决定，返回是否放行。
//
// 超时返回 false（拒绝）：挂起太久既占资源也让对端一直等。
//
// store 用于判断"首次见到"还是"指纹变了"——两种情形的提示语气完全不同，
// 不能混成一句"要信任吗"。
func (g *gate) review(conn net.Conn, fingerprint string, store *trust.Store) bool {
	request := &gateRequest{
		conn:        conn,
		peerAddr:    conn.RemoteAddr().String(),
		fingerprint: fingerprint,
		decide:      make(chan bool, 1),
	}
	if store != nil {
		if status, err := store.Check(trust.RoleViewer, request.peerAddr, fingerprint); status == trust.StatusChanged {
			request.changed = true
			if changed, ok := err.(*trust.ChangedError); ok {
				request.known = changed.Known
			}
		}
	}
	g.mu.Lock()
	g.pending = append(g.pending, request)
	g.mu.Unlock()

	if g.notify != nil {
		g.notify()
	} else {
		// 没有界面（--headless）：没人能确认，只能拒绝。
		// 这不是"忘了处理"——headless 场景下就必须预先信任，否则无从确认。
		log.Printf("执行端：收到来自 %s 的未信任 viewer（无界面可确认），拒绝", request.peerAddr)
		g.remove(request)
		return false
	}

	select {
	case allowed := <-request.decide:
		g.remove(request)
		return allowed
	case <-time.After(gateTimeout):
		log.Printf("执行端：等待确认超时，断开 %s", request.peerAddr)
		g.remove(request)
		return false
	}
}

// Pending 返回当前的待确认列表（界面渲染用）。
func (g *gate) Pending() []*gateRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*gateRequest, len(g.pending))
	copy(out, g.pending)
	return out
}

// Resolve 由界面调用：放行或拒绝某条待确认连接。
func (g *gate) Resolve(request *gateRequest, allow bool) {
	select {
	case request.decide <- allow:
	default:
		// 已经有结论了（超时后用户才点）：忽略
	}
}

// remove 从待确认列表里摘掉。
func (g *gate) remove(request *gateRequest) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, existing := range g.pending {
		if existing == request {
			g.pending = append(g.pending[:i], g.pending[i+1:]...)
			return
		}
	}
}

// admitViewer 决定这条进来的连接该不该被接受。
//
// 判定顺序（从"最不该问"到"最该问"）：
//
//  1. **不是 TLS 连接，或对端没出示证书 → 拒绝**（加密与明文严格隔离）
//  2. 指纹与本执行端自己的证书相同 → 同一台机器上的 viewer，同一身份，放行
//  3. 信任列表里指纹一致 → 静默放行（最常见的情况）
//  4. 未知 / 指纹变了 → 挂起这条连接，等用户弹窗确认
//
// # 第 1 条：为什么不放行明文
//
// 以前明文连接直接放行（"token 仍然是那道门"）。但那等于留了个降级通道：
// 攻击者只要让 TLS 握手失败，客户端/服务端就退化到无证书校验的明文，
// 整个指纹体系白做。
//
// 用户的要求是"用 tls 和不用 tls 的不能互相通讯，完全隔离"，所以现在：
// 执行端开了 TLS → 只接受出示证书的客户端；没开 TLS → 监听就是明文的，
// 那种情况下 peerFingerprint 为空、这里也走不到（tlsx 不回 TLS 连接）。
//
// # 第 2 条：用指纹而不是 IP
//
// 同一台机器上的 viewer 与执行端共用 `<DataDir>/tls` 那一份证书，
// "自己连自己"的指纹天然等于本机指纹 —— 同一个身份，默认信任。
// 用 IP 判断不可靠（可以伪造/绑定），证书不行。
func admitViewer(conn net.Conn) bool {
	// 1. 没有证书 = 非 TLS 或单向 TLS → 拒绝
	fingerprint := peerFingerprint(conn)
	if fingerprint == "" {
		log.Printf("执行端：拒绝 %s —— 没有出示客户端证书（加密与明文严格隔离）",
			conn.RemoteAddr())
		return false
	}
	// 2. 与自己的证书指纹相同 = 同一身份（同一台机器上的 viewer）
	if own := tlsx.ActiveServerFingerprint(); own != "" && own == fingerprint {
		return true
	}
	// 3. 已信任的指纹直接放行
	peerAddr := conn.RemoteAddr().String()
	if trustStore != nil {
		status, err := trustStore.Check(trust.RoleViewer, peerAddr, fingerprint)
		if status == trust.StatusKnown && err == nil {
			return true
		}
	}
	// 4. 未知或变了：交给界面确认（非阻塞地挂起这条连接）
	if viewerGate == nil {
		return false
	}
	allowed := viewerGate.review(conn, fingerprint, trustStore)
	if allowed && trustStore != nil {
		// 用户点了"信任"才写进列表 —— "自动信任"在结构上就不可能
		_ = trustStore.Trust(trust.RoleViewer, peerAddr, fingerprint)
	}
	return allowed
}

// viewerGate / trustStore 由 main 在启动时注入（见 setupViewerGate）。
var (
	viewerGate *gate
	trustStore *trust.Store
)

// SetViewerTrust 注入信任列表与待确认队列（供 main 调用）。
func setViewerTrust(store *trust.Store, gate *gate) {
	trustStore = store
	viewerGate = gate
}

// gateAdapter 把 gate 适配成 uiapp.ViewerTrust（界面只认接口，不认 gate）。
type gateAdapter struct{ gate *gate }

// PendingViewers 返回等待确认的连接（实现 uiapp.ViewerTrust）。
func (a gateAdapter) PendingViewers() []uiapp.ViewerPeer {
	requests := a.gate.Pending()
	out := make([]uiapp.ViewerPeer, len(requests))
	for i, request := range requests {
		out[i] = request
	}
	return out
}

// ResolveViewer 给出结论（实现 uiapp.ViewerTrust）。
func (a gateAdapter) ResolveViewer(peer uiapp.ViewerPeer, allow bool) {
	if request, ok := peer.(*gateRequest); ok {
		a.gate.Resolve(request, allow)
	}
}
func peerFingerprint(conn net.Conn) string {
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return ""
	}
	return certFingerprint(state.PeerCertificates[0].Raw)
}

// certFingerprint 计算证书 DER 的 SHA-256（小写十六进制）。
func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// isLoopbackPeer 判断对端是不是本机（127.0.0.0/8、::1）。
//
// **同进程启动的 viewer 连自己时走的就是回环**，那种连接不该弹窗：
// 同一个可执行文件自己信任自己，问用户"要不要信任你自己"毫无意义。
//
// 注意这不构成安全漏洞：回环连接只能由本机进程发起，能发起的进程
// 本来就有本机的完全控制权。
func isLoopbackPeer(conn net.Conn) bool {
	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return false
	}
	return addr.IP.IsLoopback()
}