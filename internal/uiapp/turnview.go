package uiapp

import (
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
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
	// itemTurnFooter 是回合结束行（dsh 的「Completed in 22s ⌄」）。
	itemTurnFooter = timeline.KindTurnFooter
)

// chatItem 是会话列表里的一行（timeline.Item 的别名，字段不必重复搬）。
type chatItem = timeline.Item

// partRef 指向某条消息里的一个 part。
type partRef = timeline.Ref

// buildChatItems 把会话切成渲染条目（规则在 timeline.Build）。
func (a *App) buildChatItems(session *Session) []chatItem {
	return timeline.Build(session.Conversation().Entries(), session.Busy() == driver.BusyIdle)
}

// renderTurnFooter 渲染回合结束行（dsh 的 turn footer）：
//
//	已完成，用时 22s ⌄        ← 成功
//	处理失败 ⌄                 ← 失败（展开看原因与建议）
//	已中止                     ← 用户停止
//
// 对照 dsh：`ui-chat/src/client/chat/` 的 turn footer 组件 + locale 的
// `message.turnProcess.{worked,took,failed}`。
//
// 文案与图标都走 locale / dsh 令牌；展开状态存在 App.expandedParts 里（key = footer）。
func (a *App) renderTurnFooter(c *ui.Context, item chatItem) {
	theme := c.Theme()
	chat := item.Refs[0].Chat
	if chat.Status().IsStreaming() {
		return // 还在生成：结束行没有意义
	}
	failed := chat.Status() == conversation.StatusFailed
	cancelled := chat.Status() == conversation.StatusCancelled

	// 标题：失败（带原因摘要）/ 已中止 / 已完成（带耗时）
	label := a.tr("message.turnProcess.worked")
	switch {
	case failed:
		// 失败原因要**一眼看到**（用户不必先展开），展开后是完整原因 + 建议
		reason := clipLine(chat.FailureReason(), 60)
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
			ui.Text(c, a.tr("turn.usage", tokens)).FontSize(11).TextColor(theme.TextMuted)
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

// footerActions 返回回合结束行右侧的按钮组。
//
// 目前只有「复制」，且**只复制本回合的最终输出**（不是整个回合的旁白堆叠）。
// 以后加「重新生成」这类按钮时在这里追加，渲染顺序即切片顺序。
func (a *App) footerActions(item chatItem) []messageAction {
	chat := tailMessage(item)
	if chat == nil {
		return nil
	}
	return []messageAction{a.copyAction(chat, a.tr("common.copyAnswer"))}
}

// formatDuration 把耗时格式化成本地化短文本（22s / 1m5s / 22秒 / 1分5秒）。
func (a *App) formatDuration(d time.Duration) string {
	seconds := int(d.Round(time.Second).Seconds())
	if seconds < 60 {
		return a.tr("duration.seconds", seconds)
	}
	return a.tr("duration.minutes", seconds/60, seconds%60)
}
