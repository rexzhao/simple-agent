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
	"errors"
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
	// ByFingerprint 表示"这个对端的身份就是它的指纹"，信任记录不按地址。
	//
	// 用于 viewer（执行端接受连接时）：它的源端口每次连接都不同，
	// 按地址记录的话每次都查不到已信任记录 —— 表现是执行端每次启动
	// 都要重新问一遍同一个 viewer（真实踩到：known_hosts 里堆了四条
	// 同指纹、只有端口不同的 viewer 记录）。
	//
	// relay 和执行端不要设它：它们监听固定地址，同一地址换了证书
	// 必须报"指纹变了"。
	ByFingerprint bool
	// Ask 阻塞等待用户对"是否信任这个对端"的决定。
	//
	// 返回 (true, nil) 才继续握手（连接成功）；(false, nil) 表示用户拒绝
	// （连接失败）；**返回 [ErrNotReady] 表示界面还没就绪**，调用方应
	// 稍后重试而不是当作拒绝。
	//
	// 为 nil 时一律拒绝：没人能确认就不该假设可信。
	//
	// **实现方不得加超时**（见包注释）。
	Ask func(req Request) (bool, error)
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
	//
	// **两种查询方式，按对端性质选**：
	//   - ByFingerprint：身份就是指纹（viewer）。它每次连接用新的源端口，
	//     按地址查永远查不到 —— 执行端会每次都重新问一遍（真实踩到：
	//     known_hosts 里堆了四条同指纹不同端口的 viewer 记录）。
	//   - 默认（角色+地址）：地址稳定的对端（relay、执行端）。
	//     同一地址换了证书必须报"变了"。
	changed, known := false, ""
	if v.Store != nil {
		if v.ByFingerprint {
			// 只看指纹：见过就是可信，没见过去问用户
			switch v.Store.CheckByFingerprint(v.Role, got) {
			case trust.StatusKnown:
				return nil
			}
		} else {
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
	}

	// 3. 首次见到 / 指纹变了：**等用户决定**（不设超时）
	if v.Ask == nil {
		return fmt.Errorf("对端 %s 的证书未经确认，且当前没有界面可确认", addr)
	}
	allowed, askErr := v.Ask(Request{
		Addr: addr, Fingerprint: got, Changed: changed, Known: known, Role: v.Role,
	})
	if askErr != nil {
		// 界面没就绪：**原样传出去**，让调用方决定要不要重试。
		// 不能变成"用户拒绝"——用户什么都没点。
		return askErr
	}
	if !allowed {
		return fmt.Errorf("用户拒绝了对端 %s 的证书", addr)
	}

	// 用户同意：写进信任列表（"trust **on first use**" —— 问过才记）。
	//
	// 写在这个位置而不是连接成功后：握手继续意味着连接即将建立，
	// 用户确认的是"这台机器可信"，与后续业务是否成功无关。
	if v.Store != nil {
		var writeErr error
		if v.ByFingerprint {
			// 只记指纹：viewer 的地址每次都不同，记了也没用
			writeErr = v.Store.TrustByFingerprint(v.Role, got)
		} else {
			writeErr = v.Store.Trust(v.Role, addr, got)
		}
		if writeErr != nil {
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

// ErrNotReady 表示"现在还没有界面可以问用户"。
//
// 与"用户拒绝"必须区分：
//   - 用户拒绝 → 连接失败，报给用户看（明确的决定）
//   - 界面没就绪 → **稍后重试**（不是决定，只是时机不对）
//
// 混成一个 false 的话，启动期（界面还没建好）的首次连接会被当成
// "用户拒绝"直接退出 —— 用户明明什么都没点，却看到"用户拒绝了对端的
// 证书"。这是真实踩到的 bug。
var ErrNotReady = errors.New("界面尚未就绪，无法询问用户")

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
	// byFingerprint 记录"正在等确认的指纹 → 决定通道"。
	//
	// 用途：同一个对端的多条连接**共用一次确认**。用户点得慢时 viewer
	// 会再发起一次拨号，执行端就挂着两条未知指纹的连接，界面上弹两次
	// 确认框（用户报告"执行端弹了两次"，点得慢时更明显）。
	//
	// 值是 *decision：**一个决定要能被多个等待者同时拿到**。用裸
	// chan bool 的话只有第一个接收者能取到值，其余连接会一直等下去
	// （真实踩到：三条同指纹连接，点一次只放行一条）。
	byFingerprint map[string]*decision
}

// decision 是一次"用户决定"，可被多个等待者同时读取。
type decision struct {
	// done 在决定产生后关闭（关闭是广播，所有等待者都会被唤醒）。
	done chan struct{}
	// allow 是决定内容（done 关闭后才有意义）。
	allow bool
}

// Pending 是一条等待确认的记录。
type Pending struct {
	Req Request
	// decided 接收用户结论。容量 1，不会阻塞 Resolve。
	decided chan bool
	// shared 是本次确认的共享决定（同指纹的其他连接也在等它）。
	shared *decision
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
func (g *Gate) Ask(req Request) (bool, error) {
	// 界面还没起来：返回 ErrNotReady，让调用方**稍后重试**。
	//
	// 这里不能返回 (false, nil) —— 那会被理解成"用户拒绝"。启动期
	// （窗口还没建好）的首次连接会因此直接失败，用户什么都没点却看到
	// "用户拒绝了对端的证书"（真实踩到）。
	if g.Ready != nil && !g.Ready() {
		return false, ErrNotReady
	}

	// **按指纹去重**：同一个对端可能同时有多条连接在等确认。
	//
	// 典型场景：用户看弹窗看得慢，viewer 那边等不及又发起了一次拨号 ——
	// 执行端于是挂着两条"未知指纹"的连接，界面上就弹两次确认框
	// （用户报告"执行端弹了两次"，点得慢时更明显）。
	//
	// **只按指纹、不按地址**：重试时源端口变了，地址是不同的字符串，
	// 但指纹相同就意味着"同一台机器" —— 那才是用户真正在回答的问题。
	key := req.Fingerprint
	g.mu.Lock()
	if existing, ok := g.byFingerprint[key]; ok {
		g.mu.Unlock()
		// 等同一个决定（done 关闭时所有等待者一起醒）
		<-existing.done
		return existing.allow, nil
	}
	pending := &Pending{Req: req, decided: make(chan bool, 1)}
	shared := &decision{done: make(chan struct{})}
	if g.byFingerprint == nil {
		g.byFingerprint = map[string]*decision{}
	}
	g.byFingerprint[key] = shared
	g.pending = append(g.pending, pending)
	pending.shared = shared
	g.mu.Unlock()

	if g.Notify != nil {
		g.Notify()
	}
	allowed := <-pending.decided
	return allowed, nil
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
	// 清掉去重表里的记录：结论已经给了，后来的同指纹连接应当重新问
	// （那时它对端可能已经换了证书 —— 不能沿用早先的结论）。
	if g.byFingerprint != nil {
		delete(g.byFingerprint, pending.Req.Fingerprint)
	}
	// **广播结论给所有等待者**（同指纹的其他连接可能还有几条在等）。
	shared := pending.shared
	if shared != nil {
		shared.allow = allow
		close(shared.done)
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
	g.byFingerprint = nil
	g.mu.Unlock()
	for _, item := range pending {
		// 广播"拒绝"，让同指纹的所有等待者一起醒来
		if item.shared != nil {
			item.shared.allow = false
			close(item.shared.done)
		}
		select {
		case item.decided <- false:
		default:
		}
	}
}