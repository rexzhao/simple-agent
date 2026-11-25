// Package peerverify 是"连接对端时如何确认它是谁"的网络层实现。
//
// # 为什么单独一个包
//
// 指纹确认本质是网络层的事（发生在 TLS 握手里），不该散落在启动编排层
// （cmd/simple-agent）和各端各自实现一遍。这个包给三处共用：
//
//	viewer 连 relay           —— 确认对面是那台 relay
//	viewer 直连执行端          —— 确认对面是那台执行端
//	执行端接受 viewer 连接     —— 确认来访者是谁
//
// 三者的判断逻辑完全一样，只是"角色"不同（信任列表按角色分开记）。
//
// # 不设"等用户"的超时
//
// 这一点很关键：**等待用户确认的时间不计入任何超时**。
//
// 网络 IO 有自己的超时（拨号、握手、读包），但"弹窗在那儿等用户点"是
// 人的速度，不是网络的速度 —— 给它设 2 分钟上限，用户去倒杯水回来就被
// 判成"取消"了，而这恰恰是最容易发生的事。
//
// 所以：网络失败照常超时；用户犹豫多久都可以。只有进程退出、对端断开
// 这类真实事件才会让等待结束（由调用方通过 Cancel 显式取消）。
package peerverify

import (
	"crypto/sha256"
	"fmt"
	"sync"

	"simple-agent/internal/trust"
)

// Verifier 判断某个对端是否可以信任。
//
// 用法（在 TLS 的 VerifyPeerCertificate 回调里）：
//
//	if err := verifier.Verify(addr, certDER); err != nil {
//	    return err // 中止握手 = 连接建立不起来
//	}
type Verifier struct {
	// Store 是信任列表（可以为 nil，那样每次都会问）。
	Store *trust.Store
	// Role 是这个对端在信任列表里的角色（relay / executor / viewer）。
	Role trust.Role
	// Pinned 是命令行显式给的指纹（非空时严格校验，不询问）。
	Pinned string
	// Ask 阻塞等待用户对"是否信任这个对端"的决定。
	//
	// 返回 true 才继续握手（连接成功），false 中止（连接失败）。
	// 为 nil 时一律拒绝：没人能确认就不该假设可信。
	//
	// **实现方不得加超时**（见包注释）。
	Ask func(req Request) bool
}

// Request 是一次待确认的对端信息。
type Request struct {
	Addr        string
	Fingerprint string
	// Changed 表示以前确认过别的指纹（不是首次）。
	Changed bool
	// Known 是以前确认过的指纹（Changed 为真时有效）。
	Known string
	// Role 是对端在信任列表里的角色。
	Role trust.Role
}

// Verify 在 TLS 握手时调用：决定接不接受这个对端。
//
// 返回 error 会中止握手 —— 连接建立不起来，这正是"用户取消"该有的
// 效果（而不是先连上再说）。
func (v *Verifier) Verify(addr string, certDER []byte) error {
	if len(certDER) == 0 {
		return fmt.Errorf("对端 %s 没有提供证书", addr)
	}
	sum := sha256.Sum256(certDER)
	return v.VerifyFingerprint(addr, fmt.Sprintf("%x", sum[:]))
}

// VerifyFingerprint 与 [Verify] 相同，但直接接受已算好的指纹。
//
// 为什么需要这个入口：执行端从 `tls.ConnectionState()` 拿到的已经是
// 解析好的证书对象，重新序列化成 DER 再算哈希是多余的一步（而且不同
// 路径取到的字节未必一致）。直接用指纹更直白。
func (v *Verifier) VerifyFingerprint(addr, fingerprint string) error {
	got := normalize(fingerprint)

	// 1. 命令行指定了指纹：严格校验（用户明确指定，不询问）
	if v.Pinned != "" {
		if normalize(v.Pinned) != got {
			return fmt.Errorf("证书指纹不匹配：%s 实际是 %s，期望 %s",
				addr, trust.Format(got), trust.Format(v.Pinned))
		}
		return nil
	}

	// 2. 查信任列表
	changed, known := false, ""
	if v.Store != nil {
		status, err := v.Store.Check(v.Role, addr, got)
		switch status {
		case trust.StatusKnown:
			// 指纹一致：静默通过（"确认过就不再问"）
			return nil
		case trust.StatusChanged:
			// 指纹变了：必须让用户看到"变了"再决定 —— 自动更新的话，
			// 攻击者第一次拦截之后就一直"合法"。
			changed = true
			if changedErr, ok := err.(*trust.ChangedError); ok {
				known = changedErr.Known
			}
		}
	}

	// 3. 首次见到 / 指纹变了：**等用户决定**（不设超时）
	if v.Ask == nil {
		return fmt.Errorf("对端 %s 的证书未经确认，且当前没有界面可确认", addr)
	}
	allowed := v.Ask(Request{
		Addr: addr, Fingerprint: got, Changed: changed, Known: known, Role: v.Role,
	})
	if !allowed {
		return fmt.Errorf("用户拒绝了对端 %s 的证书", addr)
	}

	// 用户同意：写进信任列表（"trust **on first use**" —— 问过才记）。
	//
	// 写在这个位置而不是连接成功后：握手继续意味着连接即将建立，
	// 用户确认的是"这台机器可信"，与后续业务是否成功无关。
	if v.Store != nil {
		if err := v.Store.Trust(v.Role, addr, got); err != nil {
			// 写入失败不该中断连接：用户已经同意了，这次连接继续。
			// 代价是下次还会问一遍（比连不上好）。
			return nil
		}
	}
	return nil
}

// normalize 去冒号/空格并转小写。
func normalize(value string) string {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch c {
		case ':', ' ', '-':
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// Gate 是一次"等待用户确认"的登记（界面据此渲染弹窗）。
//
// 它把阻塞的 Ask 和异步的界面连接起来：
//
//	握手 goroutine          界面主线程
//	  Gate.Ask(req)  ──登记──▶  Gate.Pending()
//	       阻塞               弹窗渲染
//	        ◀──决定───────  Gate.Resolve(req, allow)
type Gate struct {
	mu      sync.Mutex
	pending []*Pending
	// Notify 通知界面"有新的待确认项"（可以为 nil，那界面靠帧循环轮询）。
	Notify func()
	// Ready 报告界面是否已经可以弹窗（可以为 nil，表示"随时可以"）。
	//
	// 为什么需要：执行端启动时**先装 TLS 再建界面** —— 注册 relay 的
	// 连接可能比窗口先就绪。那时如果直接登记等待，就没人能点那个弹窗，
	// 会一直挂到进程结束。
	//
	// 所以这里先问 Ready：还没就绪就**立刻返回"暂时无人可问"**，
	// 由调用方（注册循环的退避重试）稍后再来 —— 等界面起来那一轮
	// 自然会弹窗。
	Ready func() bool
}

// Pending 是一条等待确认的记录。
type Pending struct {
	Req Request
	// decided 接收用户结论。容量 1，不会阻塞 Resolve。
	decided chan bool
}

// Addr 返回对端地址（实现界面侧的展示接口）。
func (p *Pending) Addr() string { return p.Req.Addr }

// Fingerprint 返回证书指纹。
func (p *Pending) Fingerprint() string { return p.Req.Fingerprint }

// Changed 报告是否"指纹变了"。
func (p *Pending) Changed() bool { return p.Req.Changed }

// Known 返回以前确认过的指纹。
func (p *Pending) Known() string { return p.Req.Known }

// ---- 同时满足 uiapp.ViewerPeer 与 uiapp.PeerConfirm 的命名风格 ----
//
// 两处界面接口（"验证来连我的" 与 "验证我要连的"）字段语义相同，
// 只是方法名不同。这里都实现，避免上层写两套适配。

// PeerAddr 同 [Pending.Addr]。
func (p *Pending) PeerAddr() string { return p.Req.Addr }

// PeerFingerprint 同 [Pending.Fingerprint]。
func (p *Pending) PeerFingerprint() string { return p.Req.Fingerprint }

// PeerChanged 同 [Pending.Changed]。
func (p *Pending) PeerChanged() bool { return p.Req.Changed }

// PeerKnown 同 [Pending.Known]。
func (p *Pending) PeerKnown() string { return p.Req.Known }

// Ask 阻塞等待用户决定（实现 [Verifier.Ask] 的签名）。
//
// **不设超时**：见包注释。等待经进程退出或调用方 Cancel 结束。
func (g *Gate) Ask(req Request) bool {
	// 界面还没起来：立刻返回 false，别让连接一直挂着等一个不会出现的弹窗。
	// 调用方（注册循环）会退避重试，等界面就绪那一轮自然会走到弹窗。
	if g.Ready != nil && !g.Ready() {
		return false
	}
	pending := &Pending{Req: req, decided: make(chan bool, 1)}
	g.mu.Lock()
	g.pending = append(g.pending, pending)
	g.mu.Unlock()
	if g.Notify != nil {
		g.Notify()
	}
	return <-pending.decided
}

// Pending 返回当前等待确认的列表（界面渲染用）。
func (g *Gate) Pending() []*Pending {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*Pending, len(g.pending))
	copy(out, g.pending)
	return out
}

// Resolve 由界面调用：给出结论。
func (g *Gate) Resolve(pending *Pending, allow bool) {
	g.mu.Lock()
	for i, existing := range g.pending {
		if existing == pending {
			g.pending = append(g.pending[:i], g.pending[i+1:]...)
			break
		}
	}
	g.mu.Unlock()
	select {
	case pending.decided <- allow:
	default:
		// 已经有结论了（重复点击）：忽略
	}
}

// CancelAll 让所有等待中的确认返回 false（进程退出时调用）。
func (g *Gate) CancelAll() {
	g.mu.Lock()
	pending := g.pending
	g.pending = nil
	g.mu.Unlock()
	for _, item := range pending {
		select {
		case item.decided <- false:
		default:
		}
	}
}