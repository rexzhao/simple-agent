package uiapp

import (
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/timeline"
)

// 会话流的"渲染条目"构建。
//
// 分组规则（哪些 part 进过程组、回合何时结束、标题怎么拼）已经搬到
// `internal/timeline`：Android 展示端要用同一份语义，否则"手机和电脑看到的
// 不一样"。这里只留渲染。

// 条目类型常量（与 timeline.Kind 对应）。
const (
	// itemNotice 是本地通知。
	itemNotice = timeline.KindNotice
	// itemContent 是消息里的正文（用户气泡或助手正文）。
	itemContent = timeline.KindContent
	// itemProcess 是折叠的过程组（思考/工具调用，可能跨消息）。
	itemProcess = timeline.KindProcess
	// itemRoundFooter 是回合结束行（dsh 的「Completed in 22s ⌄」）。
	itemRoundFooter = timeline.KindRoundFooter
)

// chatItem 是会话列表里的一行（timeline.Item 的别名，字段不必重复搬）。
type chatItem = timeline.Item

// partRef 指向某条消息里的一个 part。
type partRef = timeline.Ref

// buildChatItems 把会话切成渲染条目（规则在 timeline.Build）。
//
// 把**本轮**的用量一起传下去：结束行要显示缓存命中率。
// 会话忙碌时（还在跑）传的是当前这一轮的实时数据，空闲后是最后一轮的。
//
// running 单独传：**工具执行期间会话不空闲、但消息已不流式**，
// 只看 idle 会让「● 运行中」那一行消失（用户以为任务结束了）。
func (a *App) buildChatItems(session *Session) []chatItem {
	busy := session.Busy()
	return timeline.Build(session.Conversation().Entries(),
		busy == driver.BusyIdle, busy != driver.BusyIdle, sessionUsaget(session))
}

// sessionUsaget 把 host 侧的用量转成 timeline 的口径（nil 表示拿不到）。
//
// 没有任何 token 时返回 nil：重放的历史会话拿不到用量，那时显示
// "缓存 0%" 会误导（看着像缓存失效，其实只是没数据）。
//
// InputExcludesCache 的判据与状态栏 cacheHitBasisPoints 一致：
// Anthropic 会报告 cache_creation（>0），它的 input 不含命中部分；
// 其余（DeepSeek/OpenAI 兼容）的 input **已含**命中，不能重复计入分母。
func sessionUsaget(session *Session) *timeline.Usage {
	if session == nil {
		return nil
	}
	u := session.Usage()
	if u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 {
		return nil
	}
	return &timeline.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		// Anthropic 的 input 不含缓存（要三者相加）；
		// 其它协议 prompt_tokens 已含命中部分（分母就是 input）。
		InputExcludesCache: u.CacheWriteTokens > 0,
	}
}

// renderRoundFooter 渲染回合结束行（dsh 的 turn footer）：
//
//	已完成，用时 22s ⌄        ← 成功
//	处理失败 ⌄                 ← 失败（展开看原因与建议）
//	已中止                     ← 用户停止
//
// 对照 dsh：`ui-chat/src/client/chat/` 的 turn footer 组件 + locale 的
// `message.turnProcess.{worked,took,failed}`。
//
// 文案与图标都走 locale / dsh 令牌；展开状态存在 App.expandedParts 里（key = footer）。
func (a *App) renderRoundFooter(c *ui.Context, item chatItem) {
	theme := c.Theme()
	chat := item.Refs[0].Chat
	// 流式期间**也渲染**：用户要在跑的时候就看到时间/token/缓存率。
	// 结束后同一个位置变成"已完成 + 本轮总计"，始终只有这一行。
	if item.Live {
		a.renderLiveFooter(c, item, theme)
		return
	}
	failed := chat.Status() == conversation.StatusFailed
	cancelled := chat.Status() == conversation.StatusCancelled

	// 标题：失败（带原因摘要）/ 已中止 / 已完成（带耗时）
	label := a.tr("message.turnProcess.worked")
	switch {
	case failed:
		// 折叠行显示**人话**（failureHint 认得的场景），认不出才退回原始原因。
		//
		// 原始原因带着内部错误码前缀（"network: Post ... wsarecv: An existing
		// connection was forcibly closed..."），直接摆在折叠行里用户看不懂。
		// 完整原因仍然展开可见 —— 那时是要排查的人在看，细节才有价值。
		//
		// 用户不必先展开：网络断了的下一句话是"怎么办"，不是"wsarecv 是什么"。
		reason := a.failureHint(chat.FailureReason())
		if reason == "" {
			reason = clipLine(chat.FailureReason(), 60)
		}
		label = a.tr("message.turnProcess.failed")
		if reason != "" {
			label += " · " + reason
		}
	case cancelled:
		label = a.tr("chat.aborted")
	case timeline.Duration(item) >= time.Second:
		// 不到 1 秒就只说「已完成」（dsh 也是这样的取舍，别显示"用时 0秒"）
		label = a.tr("message.turnProcess.took", a.formatDuration(timeline.Duration(item)))
	}

	key := chat.Message.MessageID + "#footer"
	expanded := a.expandedParts[key]
	detail := expanded && failed

	// 一行：状态文字（失败可展开）+ token 用量 + 右侧图标按钮组（复制…）。
	//
	// 对照 dsh 的 MessageIconActions：**按钮与状态文字同一行**，
	// 以后加按钮就往 actions 里追加，不要另起一行。
	actions := a.footerActions(item)
	ui.Row(c).Gap(8).Padding(2, 2).AlignItems(ui.Center).Children(func() {
		if failed {
			// 只有"可展开"的失败行才带三角与点击（成功的行不带，免得被当成过程组）
			icon := "▸"
			if expanded {
				icon = "▾"
			}
			if textButtonLines(c, icon+" "+label, 12, theme.Danger, 1) {
				a.expandedParts[key] = !expanded
				detail = !expanded
			}
		} else {
			ui.Text(c, label).FontSize(12).TextColor(theme.TextMuted)
		}
		if tokens := timeline.Tokens(item); tokens > 0 {
			// 用 k/M 缩写：一轮几十万 token 时原文是 "456789 tokens"，
			// 挤在状态行里既占宽又难一眼读出量级。
			ui.Text(c, a.tr("turn.usage", compactTokens(tokens))).FontSize(11).TextColor(theme.TextMuted)
		}
		// 本轮缓存命中率：紧跟 token 用量。
		//
		// 只在命中过时显示（CacheHitBasisPoints 返回 -1 表示没有数据）：
		// 第一轮纯写缓存时显示 "缓存 0%" 会让人以为缓存失效。
		//
		// 与状态条的**累计**命中率是两回事：那里看的是"这个会话一共省了多少"，
		// 这里看的是"这一轮省了多少"。两者都要，因为累计值看不出单轮退步。
		if item.Usage != nil {
			if bp := item.Usage.CacheHitBasisPoints(); bp >= 0 {
				ui.Text(c, a.tr("composer.cacheHit", formatBasisPoints(bp))).
					FontSize(11).TextColor(theme.TextMuted)
			}
		}
	// 失败且"重发有意义"时，在按钮组前给一个「重试」文字按钮。
		//
		// 位置在这一行（不必展开看详情就能点）：网络断了的第一反应是
		// 再试一次，不该逼用户先展开再找按钮。
		if failed && isRetryable(chat.FailureReason()) {
			a.retryButton(c, theme)
		}
		a.messageActionsRow(c, actions, true)
	})
	if detail {
		ui.Column(c).Gap(2).Padding(0, 18).Children(func() {
			ui.Text(c, a.tr("chat.failed", chat.FailureReason())).FontSize(12).TextColor(theme.Danger)
			if hint := a.failureHint(chat.FailureReason()); hint != "" {
				ui.Text(c, hint).FontSize(11).TextColor(theme.TextMuted)
			}
		})
	}
	// 注意：这里**不画**分隔线。dsh 的 turn footer 只有一行文字，没有横线；
	// 之前那条 Box(Height(1)) 是多余的视觉噪音。
}

// renderLiveFooter 渲染**执行中**的回合行：运行时间 + 实时 token + 当前缓存率。
//
// 与结束行是**同一个位置**（同一个 Item.Key），只是内容不同：
// 跑的时候说"正在跑多久、用了多少"，跑完换成"总计"。
// 这样用户不用等结束就能判断"这轮会不会太久 / 缓存有没有生效"。
//
// 时间用 StartedAt 到现在：回合结束行用的是 finished-started，
// 两者在结束时会对上（结束的瞬间 现在 ≈ FinishedAt）。
func (a *App) renderLiveFooter(c *ui.Context, item chatItem, theme *ui.Theme) {
	// 用实体圆点 + 强调色表示"在跑"：mygo 没有内建动画组件，
// 与其画一个静态的"加载图标"（看着像卡住了），不如用颜色明确标示活动态
// ——和侧栏会话行的状态点、todo 面板的 StateDot 是同一套做法。
	ui.Row(c).Gap(8).Padding(2, 2).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "●").FontSize(9).TextColor(theme.Accent)
		ui.Text(c, a.tr("turn.running")).FontSize(12).TextColor(theme.Accent)
		if elapsed := time.Since(item.TurnStart); elapsed >= time.Second {
			ui.Text(c, a.formatDuration(elapsed)).FontSize(11).TextColor(theme.TextMuted)
		}
		// 实时 token：本轮已经产生的量（含缓存读，那是真实消耗过的）。
		// 同样用 k/M 缩写 —— 长会话一轮就是几十万，原文挤爆状态行。
		if item.Usage != nil {
			if total := item.Usage.TotalTokens(); total > 0 {
				ui.Text(c, a.tr("turn.usage", compactTokens(total))).FontSize(11).TextColor(theme.TextMuted)
			}
			// 实时缓存率：只在这一轮已经命中过时显示
			if bp := item.Usage.CacheHitBasisPoints(); bp >= 0 {
				ui.Text(c, a.tr("composer.cacheHit", formatBasisPoints(bp))).
					FontSize(11).TextColor(theme.TextMuted)
			}
		}
	})
}

// footerActions 返回回合结束行右侧的按钮组。
//
// 目前只有「复制」，且**只复制本回合的最终输出**（不是整个回合的旁白堆叠）。
// 失败且可重试时前面再加一个「重试」。以后加「重新生成」这类按钮时
// 在这里追加，渲染顺序即切片顺序。
func (a *App) footerActions(item chatItem) []messageAction {
	chat := tailMessage(item)
	if chat == nil {
		return nil
	}
	// 重试不放在这里：messageActions 那套按钮是**图标按钮**
	// （尺寸固定、带"已复制"反馈），硬塞一个文字按钮会破坏一致性。
	// 重试在 renderRoundFooter 里单独画（见下方 retryButton）。
	return []messageAction{a.copyAction(chat, a.tr("common.copyAnswer"))}
}

// retryButton 在失败行画一个「重试」文字按钮。
//
// **重试 ≠ 发"继续"**：后者会在对话里多出一条用户消息，既弄脏历史
// 又让缓存前缀失效（整段缓存作废）。重试是原样重发这一轮请求。
//
// 只对"重发有意义"的失败出现（见 isRetryable）：401、上下文超限
// 重发一万次也一样，给按钮只会让人白点。
func (a *App) retryButton(c *ui.Context, theme *ui.Theme) {
	session := a.Current()
	if session == nil {
		return
	}
	label := a.tr("chat.retry")
	if !session.CanSend() {
		// 忙碌时按钮仍在（位置别跳），但点了没反应 —— 用弱化色表达不可用
		ui.Text(c, label).FontSize(11).TextColor(theme.TextMuted)
		return
	}
	if textButtonLines(c, label, 11, theme.Accent, 1) {
		if err := a.Retry(); err != nil {
			a.flashErr = err.Error()
		}
	}
}

// Retry 重发这一轮请求（不追加用户消息）。
func (a *App) Retry() error {
	session := a.Current()
	if session == nil {
		return nil
	}
	// 忙碌时不能重试：会让两个流同时跑。
	// 界面上按钮本就显示为不可用，这里是兜底（双击、快捷键都可能绕过）。
	if !session.CanSend() {
		return nil
	}
	_, err := a.host.Submit(host.Command{
		SessionID: session.ID,
		Kind:      "retry",
	})
	return err
}

// formatDuration 把耗时格式化成本地化短文本（22s / 1m5s / 22秒 / 1分5秒）。
func (a *App) formatDuration(d time.Duration) string {
	seconds := int(d.Round(time.Second).Seconds())
	if seconds < 60 {
		return a.tr("duration.seconds", seconds)
	}
	return a.tr("duration.minutes", seconds/60, seconds%60)
}
