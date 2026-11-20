package uiapp

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/render"
)

// 复制相关的界面逻辑（第 3 项需求：可选中复制 / 消息复制按钮 / 代码块复制按钮）。

// copyFeedback 是「已复制」提示的持续时间（测试里可以调小）。
var copyFeedback = 1500 * time.Millisecond

// copyButton 渲染一个小的复制按钮：点击写剪贴板，并在 copyFeedback 内显示「已复制」。
//
// 反馈用**到期时间 + c.After** 实现：到点自动重绘一次把文字变回去，不需要
// goroutine，也不依赖下一次用户操作。
func (a *App) copyButton(c *ui.Context, key, text, a11y string) bool {
	theme := c.Theme()
	label := a.tr("common.copy")
	if deadline, ok := a.copiedAt[key]; ok {
		if remaining := time.Until(deadline); remaining > 0 {
			label = a.tr("common.copied")
			// 到点请求一帧，把「已复制」换回「复制」
			c.After(remaining)
		} else {
			delete(a.copiedAt, key)
		}
	}
	element := ui.Text(c, label).FontSize(11).TextColor(theme.TextMuted).
		Padding(2, 6).Radius(6).Cursor(ui.CursorPointer)
	// 代码块按钮带无障碍标签（"复制代码 go"）：多个代码块才区分得开。
	// 注意：mygo 的 label 会**取代**文字进入无障碍/测试的文本索引，所以消息按钮
	// 刻意不设 label（否则 "已复制" 这类可见文字就查不到了）。
	if a11y != "" {
		element.Label(a11y)
	}
	if element.Hovered() {
		element.Background(theme.SurfaceHover)
	}
	if !element.Clicked() {
		return false
	}
	// 代码块原文末尾常带一个换行：复制「代码本身」时不该带上它
	text = strings.TrimRight(text, "\n")
	c.WriteClipboard(text)
	if a.copiedAt == nil {
		a.copiedAt = map[string]time.Time{}
	}
	a.copiedAt[key] = time.Now().Add(copyFeedback)
	return true
}

// copyButtonIdle 与 copyButton 相同，但可以"隐藏"（不悬停时）。
//
// 隐藏只改文字颜色（透明），**元素照样创建**：mygo 的原生菜单靠元素 id
// 匹配，兄弟元素在悬停时出现/消失会让后面的元素 id 位移，菜单就选不中了
// （侧栏会话行里的「⋯」踩过这个坑）。
//
// 优点是不破坏元素树；代价是隐藏时仍占位 —— 所以用它时该行的布局要
// 能接受这块固定宽度。
func (a *App) copyButtonIdle(c *ui.Context, key, text, a11y string, visible bool) bool {
	theme := c.Theme()
	label := a.tr("common.copy")
	showingFeedback := false
	if deadline, ok := a.copiedAt[key]; ok {
		if remaining := time.Until(deadline); remaining > 0 {
			label = a.tr("common.copied")
			showingFeedback = true
			c.After(remaining)
		} else {
			delete(a.copiedAt, key)
		}
	}
	element := ui.Text(c, label).FontSize(10).Padding(1, 4).Radius(6).
		Cursor(ui.CursorPointer)
	// 反馈期间即使不悬停也要看得见 —— 否则用户点了没有任何视觉回应
	if visible || showingFeedback {
		element.TextColor(theme.TextMuted)
	} else {
		element.TextColor(theme.TextMuted.Alpha(0))
	}
	if a11y != "" {
		element.Label(a11y)
	}
	if element.Hovered() && visible {
		element.Background(theme.SurfaceHover)
	}
	if !element.Clicked() {
		return false
	}
	text = strings.TrimRight(text, "\n")
	c.WriteClipboard(text)
	if a.copiedAt == nil {
		a.copiedAt = map[string]time.Time{}
	}
	a.copiedAt[key] = time.Now().Add(copyFeedback)
	return true
}

// messageCopyText 返回一条消息里"可复制"的 markdown 原文。
//
// 只取正文文本 part：**不含**推理、工具调用/结果、压缩摘要与图片；
// 多个 part 之间用空行隔开。
func messageCopyText(chat *conversation.ChatEntry) string {
	parts := make([]string, 0, len(chat.Message.Parts))
	for _, part := range chat.Message.Parts {
		switch part.MediaType {
		case render.MediaReasoning, render.MediaToolUse, render.MediaToolResult, render.MediaSummary:
			continue
		}
		text, ok := part.AsText()
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n")
}

// CopyFeedbackForTest 返回「已复制」反馈时长（测试用）。
func CopyFeedbackForTest() time.Duration { return copyFeedback }

// SetCopyFeedbackForTest 设置「已复制」反馈时长（测试用）。
func SetCopyFeedbackForTest(d time.Duration) { copyFeedback = d }

// formatMessageTime 把时间格式化成气泡下方的短时间戳（10/23 17:15）。
//
// dsh 用 locale 的 `date.ymd` + 时分；这里保持同样的信息量，省略年份。
func (a *App) formatMessageTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	local := t.Local()
	return fmt.Sprintf("%02d/%02d %02d:%02d",
		int(local.Month()), local.Day(), local.Hour(), local.Minute())
}
