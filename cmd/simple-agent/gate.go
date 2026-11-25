package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"log"
	"net"

	"simple-agent/internal/peerverify"
	"simple-agent/internal/trust"
	"simple-agent/internal/uiapp"
)

// 执行端验证进来的 viewer。
//
// 判定逻辑本身在 `internal/peerverify`（网络层，三端共用）；本文件只做
// 两件"执行端专属"的事：
//  1. 从已完成握手的连接上取对端证书
//  2. 把"等用户确认"接到界面（`uiapp.ViewerTrust`）
//
// # 不做"同一台机器默认信任"的特殊判断
//
// viewer 与执行端在同一台机器时共用 `<DataDir>/tls` 那一份证书，指纹天然
// 相同 —— 而"指纹相同"在信任列表里本来就是"已确认"，不需要额外规则。
// 少一条特例就少一处将来会失效的地方。

var (
	// viewerGate 是待确认队列（界面据此弹窗）。
	viewerGate *peerverify.Gate
	// viewerTrustStore 是信任列表。
	viewerTrustStore *trust.Store
	// viewerVerifier 是判定器（角色固定为 RoleViewer）。
	viewerVerifier *peerverify.Verifier
)

// setupViewerTrust 初始化执行端的信任体系。
func setupViewerTrust(store *trust.Store, confirmViewer func()) {
	viewerTrustStore = store
	viewerGate = &peerverify.Gate{Notify: confirmViewer}
	viewerVerifier = &peerverify.Verifier{
		Store: store,
		Role:  trust.RoleViewer,
		// **viewer 的身份就是它的证书指纹，地址没有意义**。
		//
		// 它每次连接用新的源端口（临时端口 + NAT），按"地址"记录的话
		// 每次都查不到已信任项 —— 表现是执行端每次启动都要重新问一遍
		// 同一个 viewer（真实踩到：known_hosts 里堆了四条同指纹、
		// 只有端口不同的 viewer 记录）。
		ByFingerprint: true,
		Ask:           viewerGate.Ask,
	}
}

// admitViewer 决定这条进来的连接该不该被接受。
func admitViewer(conn net.Conn) bool {
	fingerprint := peerFingerprint(conn)
	if fingerprint == "" {
		// 没有证书 = 非 TLS 或单向 TLS → 拒绝（加密与明文严格隔离）
		log.Printf("执行端：拒绝 %s —— 没有出示客户端证书（加密与明文严格隔离）",
			conn.RemoteAddr())
		return false
	}
	if viewerVerifier == nil {
		return false
	}
	// verify 内部处理"已信任 / 需要问用户 / 指纹变了"的全部逻辑
	if err := viewerVerifier.VerifyFingerprint(conn.RemoteAddr().String(), fingerprint); err != nil {
		log.Printf("执行端：拒绝 %s（%v）", conn.RemoteAddr(), err)
		return false
	}
	return true
}

// peerFingerprint 从一条已完成握手的 TLS 连接上取对端证书指纹（十六进制）。
func peerFingerprint(conn net.Conn) string {
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}
	// **必须先完成握手**，否则 ConnectionState() 里没有对端证书。
	//
	// accept 返回的 *tls.Conn 是"懒握手"的：证书要等第一次 Read/Write
	// 才会拿到。而我们在交给 host.Serve 之前就要判断"来的是谁"，
	// 那时还没读过任何字节 —— 直接查 ConnectionState 得到的是空，
	// 于是每个连接都被判成"没有出示客户端证书"而无条件拒绝
	// （真实踩到：viewer 用对了证书依然被拒）。
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("执行端：TLS 握手失败（%s）：%v", conn.RemoteAddr(), err)
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

// gateAdapter 把 peerverify.Gate 适配成 uiapp.ViewerTrust（界面只认接口）。
type gateAdapter struct{ gate *peerverify.Gate }

// PendingViewers 返回等待确认的连接（实现 uiapp.ViewerTrust）。
func (a gateAdapter) PendingViewers() []uiapp.ViewerPeer {
	pendings := a.gate.Pending()
	out := make([]uiapp.ViewerPeer, len(pendings))
	for i, pending := range pendings {
		out[i] = pending
	}
	return out
}

// ResolveViewer 给出结论（实现 uiapp.ViewerTrust）。
func (a gateAdapter) ResolveViewer(peer uiapp.ViewerPeer, allow bool) {
	if pending, ok := peer.(*peerverify.Pending); ok {
		a.gate.Resolve(pending, allow)
	}
}

// ---- 同时适配 viewer 侧（uiapp.PeerConfirmer）----
//
// 两个界面接口的字段语义相同、方向相反（"验证来连我的" 与 "验证我要连的"），
// 共用同一份适配器可以少一套样板。

// PendingPeers 返回等待确认的对端（实现 uiapp.PeerConfirmer）。
func (a gateAdapter) PendingPeers() []uiapp.PeerConfirm {
	pendings := a.gate.Pending()
	out := make([]uiapp.PeerConfirm, len(pendings))
	for i, pending := range pendings {
		out[i] = pending
	}
	return out
}

// ResolvePeer 给出结论（实现 uiapp.PeerConfirmer）。
func (a gateAdapter) ResolvePeer(peer uiapp.PeerConfirm, allow bool) {
	if pending, ok := peer.(*peerverify.Pending); ok {
		a.gate.Resolve(pending, allow)
	}
}