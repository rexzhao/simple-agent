package uiapp

import (
	"github.com/egoist/mygo/ui"

	"simple-agent/internal/trust"
)

// 执行端验证进来的 viewer：待确认队列的界面部分。
//
// # 与 viewer 侧的指纹确认（fingerprint.go）的区别
//
//	fingerprint.go   viewer 连出去时确认**对端**（relay/执行端）的指纹
//	本文件           执行端接受连接时确认**来访者**（viewer）的指纹
//
// 两者都是 TOFU，但方向相反、提示语气也不同：前者是"我要连的这台可靠吗"，
// 后者是"这个人要进来，让它进吗"。

// ViewerPeer 描述一条待确认的来访连接（由 main 包适配）。
type ViewerPeer interface {
	// PeerAddr 是对端地址（host:port）。
	PeerAddr() string
	// Fingerprint 是对端证书指纹（规范化十六进制）。
	Fingerprint() string
	// Changed 报告这个对端以前信任过别的指纹。
	Changed() bool
	// Known 是以前信任过的指纹（Changed 为真时有效）。
	Known() string
}

// ViewerTrust 是执行端信任体系的界面接口（由 main 包实现并注入）。
type ViewerTrust interface {
	// PendingViewers 返回当前等待确认的连接。
	PendingViewers() []ViewerPeer
	// ResolveViewer 给出结论：放行或拒绝。
	ResolveViewer(peer ViewerPeer, allow bool)
}

// SetViewerTrustUI 注入执行端的信任界面适配器（main 启动时调用）。
func (a *App) SetViewerTrustUI(trust ViewerTrust) {
	a.viewerTrust = trust
}

// viewerTrustDialog 渲染"有 viewer 要连进来"的确认框。
//
// **一次只显示一条**：同时来多个时逐个问，堆叠弹窗会让人不知道在确认什么。
// 其余的等这一条解决后自然轮到（每帧重新取 Pending 的第一个）。
func (a *App) viewerTrustDialog(c *ui.Context) {
	if a.viewerTrust == nil {
		return
	}
	pending := a.viewerTrust.PendingViewers()
	if len(pending) == 0 {
		return
	}
	peer := pending[0]

	title := a.tr("viewer.trust.title")
	message := a.tr("viewer.trust.message", peer.PeerAddr(), trust.Format(peer.Fingerprint()))
	if peer.Changed() {
		title = a.tr("viewer.trust.changedTitle")
		message = a.tr("viewer.trust.changedMessage",
			peer.PeerAddr(), trust.Format(peer.Known()), trust.Format(peer.Fingerprint()))
	}
	// AlertDialog 返回 0 = 第一个按钮（允许），1 = 拒绝
	var open = true
	switch ui.AlertDialog(c, &open, title, message,
		a.tr("viewer.trust.allow"), a.tr("viewer.trust.deny")) {
	case 0:
		a.viewerTrust.ResolveViewer(peer, true)
	case 1:
		a.viewerTrust.ResolveViewer(peer, false)
	}
	// 用户关掉对话框（点外部/ESC）不当作决定：连接继续挂着等超时。
	// 这样"手滑关掉"不会变成静默拒绝 —— 拒绝必须是明确的动作。
}