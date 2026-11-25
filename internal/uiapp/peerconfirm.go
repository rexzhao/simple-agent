package uiapp

import (
	"github.com/egoist/mygo/ui"

	"simple-agent/internal/trust"
)

// viewer 连接对端（relay）时的指纹确认界面。
//
// # 与 fingerprint.go 的区别
//
//	fingerprint.go  执行端**注册到 relay** 时的指纹确认（探测 + 确认 + 注册）
//	本文件          viewer **连接到 relay** 时的指纹确认（TLS 握手期间阻塞等）
//
// 两者方向不同、时机不同：
//   - 前者是"我要把执行端挂到这台 relay 上"
//   - 后者是"我要连这台 relay 取会话"，等的地方是 TLS 握手中的
//     VerifyPeerCertificate 回调 —— 用户同意则握手继续、连接成功；
//     取消则握手失败、连接建立不起来。
//
// 所以这里**不能**做成"失败后重连"：那会让用户看到"连接失败"然后
// 莫名其妙自己又连上了。

// PeerConfirmer 是 viewer 侧指纹确认的界面接口（由 main 包实现）。
type PeerConfirmer interface {
	// PendingPeers 返回等待确认的对端。
	PendingPeers() []PeerConfirm
	// ResolvePeer 给出结论：信任或拒绝。
	ResolvePeer(peer PeerConfirm, allow bool)
}

// PeerConfirm 描述一个待确认的对端。
type PeerConfirm interface {
	// PeerAddr 是对端地址（host:port）。
	PeerAddr() string
	// PeerFingerprint 是证书指纹（规范化十六进制）。
	PeerFingerprint() string
	// PeerChanged 表示以前确认过别的指纹。
	PeerChanged() bool
	// PeerKnown 是以前确认过的指纹（PeerChanged 为真时有效）。
	PeerKnown() string
}

// SetPeerConfirmer 注入 viewer 侧确认界面（main 启动时调用）。
func (a *App) SetPeerConfirmer(confirmer PeerConfirmer) {
	a.peerConfirmer = confirmer
}

// peerConfirmDialog 渲染"要信任这个对端吗"的确认框。
//
// 一次只显示一条：同时出现多个时逐个问，堆叠弹窗会让人不知道在确认什么。
func (a *App) peerConfirmDialog(c *ui.Context) {
	if a.peerConfirmer == nil {
		return
	}
	pending := a.peerConfirmer.PendingPeers()
	if len(pending) == 0 {
		return
	}
	peer := pending[0]

	title := a.tr("peer.tofu.title")
	message := a.tr("peer.tofu.message",
		peer.PeerAddr(), trust.Format(peer.PeerFingerprint()))
	if peer.PeerChanged() {
		title = a.tr("peer.tofu.changedTitle")
		message = a.tr("peer.tofu.changedMessage",
			peer.PeerAddr(),
			trust.Format(peer.PeerKnown()),
			trust.Format(peer.PeerFingerprint()))
	}
	// AlertDialog 返回 0 = 第一个按钮（信任并连接），1 = 取消
	var open = true
	switch ui.AlertDialog(c, &open, title, message,
		a.tr("peer.tofu.trust"), a.tr("common.cancel")) {
	case 0:
		a.peerConfirmer.ResolvePeer(peer, true)
	case 1:
		a.peerConfirmer.ResolvePeer(peer, false)
	}
	// 用户直接关掉对话框不算决定：连接继续保持握手状态等这条弹窗再次出现
	// 或超时。这样"手滑关掉"不会变成静默拒绝 —— 拒绝必须是明确动作。
}