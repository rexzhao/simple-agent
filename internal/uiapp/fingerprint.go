package uiapp

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/tlsx"
)

// TOFU（trust on first use）：连接 relay 时的证书指纹确认。
//
// 思路与 SSH 的 known_hosts 一致：
//  1. 第一次连某台 relay：先探测它的证书指纹，**展示给用户确认**
//  2. 确认过的指纹存进 relay.json，以后同一台 relay 直接用它严格校验
//  3. 指纹变了（换了证书/被中间人）：**停下来重新问**，绝不自作主张
//
// 为什么不沿用"让用户自己抄指纹"：抄错一个字符就连不上，而且错误信息
// （"connection aborted"一类）完全看不出是抄错 —— 这是真实踩过的坑。
// 展示 + 确认既省事，又把"你信任的是哪个指纹"这件事摆到台面上说清楚。

// pendingFingerprint 是"等用户确认"的那次连接。
type pendingFingerprint struct {
	addr        string
	token       string
	fingerprint string
	// probeErr 是探测失败的原因（那时没有指纹可确认，只报错）。
	probeErr string
	// open 表示确认框是否正在显示。
	open bool
	// changed 表示这是**指纹变了**（以前确认过别的值），而不是首次连接。
	//
	// 两种情形的文案必须不同：首次是"你要信任这台吗"，
	// 变了是"它跟上次不一样了，可能是换了证书，也可能是被冒充"——
	// 后者要显眼得多。
	changed bool
}

// probeThenConnect 探测对端是不是 TLS relay，再决定走确认框还是明文连接。
//
// previous 是**用户以前确认过的指纹**（可以是空，表示首次）。
// 必须由调用方传进来：StartRelayRegistration 一开始就会把当前参数写进
// relay.json，等这里再去读就已经被覆盖了（真实踩过，导致"指纹变了"
// 永远判不出来）。
//
// 为什么需要"探测失败就退回明文"：客户端**没法预先知道**对端要不要 TLS
// （relay 不暴露这个信息）。而明文 relay 是完全合法的老用法 ——
// 不能因为加了 TOFU 就让它们连不上。
//
// 判据很直接：能完成 TLS 握手并拿到证书 = 对端是 TLS relay；
// 握手失败 = 对端是明文。
//
// **探测是同步的**（短超时）：异步会让明文连接也变成"稍后才连"，
// 界面上表现为点连接没反应 —— 那是老用户完全不期待的行为。
// 明文场景下握手会立刻被拒（对端不是 TLS，几十毫秒），不会真等到超时。
func (a *App) probeThenConnect(addr, token, previous string) string {
	// 3 秒够走完一次本地/正常网络的 TLS 握手。
	fingerprint, plain, err := tlsx.ProbeFingerprintOrPlain(addr, 3*time.Second)
	if err != nil {
		// 真连不上（超时）：报出来。这时不该假装是明文——那会给出一个
		// 更误导的错误（"连接被拒"之类，用户以为是口令问题）。
		return "连不上 relay：" + err.Error()
	}
if plain {
		// 对端是明文服务：**拒绝**（加密与明文严格隔离）。
		//
		// 以前这里会"退回明文连接，不打扰用户"。但那是个降级漏洞：
		// 攻击者只要让 TLS 握手失败，客户端就自动改用无校验的明文连接，
		// 指纹体系形同虚设。
		//
		// 用户的要求是"用 tls 和不用 tls 的不能互相通讯，完全隔离"，
		// 所以这里直接报错，让用户自己决定要不要改用正确的配置。
		return fmt.Sprintf(
			"对端 %s 未启用 TLS。本端只与加密对端通讯（加密与明文严格隔离）。\n"+
				"如果这台 relay 确实没开 TLS，请用它自己的配置方式连接。", addr)
	}
	fingerprint = normalizedFingerprint(fingerprint)

	// **与已确认的一致 → 直接连，不打扰用户**。
	//
	// 这是"跟 SSH 一样"的关键：确认过一次之后，正常连接必须一键完成，
	// 不能每次都弹窗问一遍。只有首次（previous 为空）或指纹变了才问。
	if previous != "" && previous == fingerprint {
		a.skipProbe = true
		message := a.StartRelayRegistration(addr, token, fingerprint)
		a.skipProbe = false
		return message
	}

	a.pendingFingerprint = &pendingFingerprint{
		addr:        addr,
		token:       token,
		fingerprint: fingerprint,
		open:        true,
		// previous 非空且不等于新值 = 指纹变了
		changed: previous != "" && previous != fingerprint,
	}
	return a.tr("relay.tofu.probing", addr)
}

// normalizedFingerprint 去掉冒号/空格并转小写（与 tlsx 内部一致）。
func normalizedFingerprint(value string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "", "-", "").Replace(value))
}

// fingerprintDialog 渲染指纹确认框（在 layout 的每帧末尾调用）。
func (a *App) fingerprintDialog(c *ui.Context) {
	pending := a.pendingFingerprint
	if pending == nil || !pending.open {
		return
	}
	// AlertDialog 返回 0 = 第一个按钮（信任并连接），1 = 取消
	title := a.tr("relay.tofu.title")
	if pending.changed {
		title = a.tr("relay.tofu.changedTitle")
	}
	message := a.tr("relay.tofu.message",
		pending.addr, tlsx.FormatFingerprint(pending.fingerprint))
	if pending.changed {
		message = a.tr("relay.tofu.changedMessage",
			pending.addr, tlsx.FormatFingerprint(pending.fingerprint))
	}
	switch ui.AlertDialog(c, &pending.open, title, message,
		a.tr("relay.tofu.trust"), a.tr("common.cancel")) {
	case 0:
		// 用户确认：保存指纹，然后用它严格校验去连
		fingerprint := pending.fingerprint
		addr, token := pending.addr, pending.token
		a.pendingFingerprint = nil
		// 指纹存进 relay.json：下次同一台 relay 直接用它严格校验，不再询问。
		// 这正是"跟 SSH 一样"的关键一步 —— 记住已确认的，只问新的。
		a.saveRelay(savedRelay{Addr: addr, Token: token, Fingerprint: fingerprint})
		a.NotifyDisplay(a.StartRelayRegistration(addr, token, fingerprint))
	case 1:
		a.pendingFingerprint = nil
	}
}
