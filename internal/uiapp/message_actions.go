package uiapp

import (
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/render"
)

// 消息操作按钮组（对齐 deepseek-harness 的 MessageIconActions）。
//
// dsh 的结构是**一行横向按钮**：copy 图标 + 插件插入的 extraActions + 末尾的
// usageAction，靠 gap:8px 排开。以后要加「分支」「重新生成」这类按钮，
// 往同一个 row 里追加即可，不要各自另起一行。
//
// 尺寸照 dsh 的 `MessageIconActions.module.css`：
//   - 行高 28、间距 8、图标 15（回合尾部那一行是 17）
//   - 按钮 28×28、圆角 sm(4)、透明底，悬停才出底色

const (
	// actionButtonSize 是单个图标按钮的边长（dsh `.action { width/height: 28px }`）。
	actionButtonSize = 28
	// actionIconSize 是消息旁边图标的尺寸（dsh `.action svg { 15px }`）。
	actionIconSize = 15
	// actionIconSizeTail 是**回合尾部**那一行图标的尺寸
	// （dsh `.actions[data-clock='end'] .action svg { 17px }`）。
	actionIconSizeTail = 17
)

// messageActions 渲染一行消息操作按钮。
//
// tail 表示这是回合最终输出那一行（图标稍大、后面还要跟耗时文字）。
// actions 是调用方按顺序追加的按钮，返回 true 表示「这一帧被点了」。
type messageAction struct {
	// key 区分「已复制」的反馈状态（每条消息各自独立）。
	key string
	// a11y 是按钮的无障碍名/悬停提示。
	//
	// **必须每个位置不同**：同一条会话里会有多个复制按钮（用户气泡一个、
	// 回合结束行一个），名字相同时测试与读屏都分不清点到了哪个。
	a11y string
	// onRun 是点击动作；返回是否接受了这次点击（false 不显示反馈）。
	// 拿到 Context 是因为写剪贴板要走 `c.WriteClipboard`。
	onRun func(c *ui.Context) bool
}

// messageActionsRow 渲染横向按钮组。
func (a *App) messageActionsRow(c *ui.Context, actions []messageAction, tail bool) {
	if len(actions) == 0 {
		return
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Height(actionButtonSize).Children(func() {
		for _, action := range actions {
			a.actionIconButton(c, action, tail)
		}
	})
}

// actionIconButton 渲染一个图标动作按钮：复制在 copyFeedback 内换成对勾。
func (a *App) actionIconButton(c *ui.Context, action messageAction, tail bool) {
	copied := a.actionCopied(action.key)
	size := float32(actionIconSize)
	if tail {
		size = actionIconSizeTail
	}

	base := action.a11y
	if base == "" {
		base = a.tr("common.copy")
	}
	label := base
	if copied {
		// 反馈时**保留位置信息**（"复制回答" → "已复制回答"），否则两个按钮
		// 同时进入反馈态就都叫"已复制"，测试与读屏都分不清。
		label = a.tr("common.copied") + strings.TrimPrefix(base, a.tr("common.copy"))
		// 到点请求一帧，把对勾换回复制图标
		if deadline, ok := a.copiedAt[action.key]; ok {
			if remaining := time.Until(deadline); remaining > 0 {
				c.After(remaining)
			}
		}
	}

	button := ui.Row(c).Size(actionButtonSize, actionButtonSize).Center().
		Radius(4).Cursor(ui.CursorPointer).
		Label(label).Tooltip(label)
	if button.Hovered() {
		// dsh 的 `--dsw-alias-interactive-bg-hover`
		button.Background(dsh.SurfaceHover)
	}
	icon := iconCopySVG
	if copied {
		icon = iconCheckSVG
	}
	button.Children(func() {
		ui.Icon(c, icon).Size(size, size).TextColor(dsh.LabelTertiary)
	})
	if !button.Clicked() {
		return
	}
	// 已经在反馈窗口内：不重复复制，也不叠加计时器（dsh 的 copyPending 语义）
	if copied {
		return
	}
	if !action.onRun(c) {
		return
	}
	if a.copiedAt == nil {
		a.copiedAt = map[string]time.Time{}
	}
	a.copiedAt[action.key] = time.Now().Add(copyFeedback)
}

// actionCopied 报告某个动作是否处在「刚做完」的反馈窗口内。
func (a *App) actionCopied(key string) bool {
	deadline, ok := a.copiedAt[key]
	if !ok {
		return false
	}
	if time.Until(deadline) > 0 {
		return true
	}
	delete(a.copiedAt, key)
	return false
}

// copyAction 构造「复制这条消息正文」的动作。
//
// a11y 由调用方给，且要区分位置（用户气泡 / 回合最终输出），
// 否则测试与读屏无法分辨点到了哪个按钮。
func (a *App) copyAction(chat *conversation.ChatEntry, a11y string) messageAction {
	return messageAction{
		key:  "msg:" + chat.Message.MessageID,
		a11y: a11y,
		onRun: func(c *ui.Context) bool {
			text := messageCopyText(chat)
			if text == "" {
				return false
			}
			c.WriteClipboard(text)
			return true
		},
	}
}

// tailMessage 返回回合的**最终输出**那一条消息（没有则 nil）。
//
// 判定用 turnEntries（回合内的全部 agent 消息）里的最后一条，并且要求它
// 确实带正文：工具轮结束后最后一条若只有工具调用，正文其实在前一条里。
func tailMessage(item chatItem) *conversation.ChatEntry {
	var last *conversation.ChatEntry
	for _, entry := range item.TurnEntries {
		if entry == nil || entry.Message == nil {
			continue
		}
		if entry.Status().IsStreaming() || entry.Status() == conversation.StatusCancelled {
			continue
		}
		if !hasBodyText(entry) {
			continue
		}
		last = entry
	}
	return last
}

// hasBodyText 报告这条消息是否有可复制的正文（不含推理/工具/摘要/图片）。
func hasBodyText(chat *conversation.ChatEntry) bool {
	for _, part := range chat.Message.Parts {
		switch part.MediaType {
		case render.MediaReasoning, render.MediaToolUse, render.MediaToolResult, render.MediaSummary:
			continue
		}
		if text, ok := part.AsText(); ok && text != "" {
			return true
		}
	}
	return false
}
