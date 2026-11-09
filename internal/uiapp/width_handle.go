package uiapp

import (
	"github.com/egoist/mygo/ui"
)

// 会话正文区的宽度把手（对齐 deepseek-harness 的 ConversationWidthControls）。
//
// 形态：正文列**两侧**各一条贴边的窄条，悬停/拖动时才显示一条跟随指针的
// 竖向光条。默认"看不见"，因为它是从正文列外缘起算的留白里的一条透明热区。
//
// 窄窗口下它自然消失，这是 dsh 的算法而非特判：
//
//	width = min(10px, (容器宽 - 正文宽)/2 - 24px - 24px)
//
// 窗口越窄，括号里越小；放不下时宽度归零 → 抓不到 → 等于隐藏。
// 我们照这个式子算，所以行为一致。
//
// 拖动的宽度变化是 **2×**：只有一侧在动，但正文列两侧同时扩张（dsh 的做法）。

// widthHandleStrip 返回把手的可用宽度（0 表示这一帧不可用）。
//
// 分母用**正文区容器的宽度**，不是整个窗口：把手挂在 chatArea 里（侧栏右侧），
// 用窗口宽度会算出偏移量偏大，两条把手就会左右不对称。
func (a *App) widthHandleStrip() float32 {
	return handleStripFor(a.chatAreaWidth(), a.contentWidth())
}

// handleStripFor 按 dsh 的式子算把手宽度：
//
//	width = min(10px, (容器宽 - 正文宽)/2 - 24px - 24px)
//
// 窗口越窄括号里越小；放不下时归零 → 抓不到 → 等于隐藏。
func handleStripFor(area, content float32) float32 {
	free := (area - content) / 2
	width := free - widthHandleInset*2
	if width <= 0 {
		return 0
	}
	if width > widthHandleWidth {
		width = widthHandleWidth
	}
	return width
}

// chatAreaWidth 返回正文区容器的宽度（上一帧实测值）。
//
// 首帧还没测出来（0）时按窗口宽度估：此时把手只是一帧的过渡，
// 下一帧就有真值了。
func (a *App) chatAreaWidth() float32 {
	if a.chatAreaW > 0 {
		return a.chatAreaW
	}
	return a.viewportW
}

// setChatAreaWidth 记录正文区容器的实测宽度。
func (a *App) setChatAreaWidth(width float32) {
	if width > 0 {
		a.chatAreaW = width
	}
}

// contentWidthHandle 渲染一侧的宽度把手；返回本次拖动是否改变了宽度。
//
// side = -1 是左侧、+1 是右侧。把手用 Absolute 定位到正文列外缘之外，
// 覆盖整列高度（与 dsh 的 `top:0; bottom:0` 一致）。
func (a *App) contentWidthHandle(c *ui.Context, side int) bool {
	strip := a.widthHandleStrip()
	if strip <= 0 {
		return false
	}
	theme := c.Theme()
	// 正文列居中：两侧外缘位置相同（对称），只是左右各一条
	// 正文列在**容器内**居中，所以把手位置从容器中线往外算：
	// 距容器中心 = 正文宽/2 + 间距。
	//
	// 用容器宽度（chatAreaWidth）而不是窗口宽度：容器在侧栏右边，
	// 用窗口宽度会让左右两侧的留白不相等。
	half := a.chatAreaWidth() / 2
	offset := half - (a.contentWidth()/2 + widthHandleInset)

	var handle *ui.Element
	// 两侧用不同的无障碍名：它们只是为了测试与读屏能区分左右，
	// 界面上都不可见（dsh 用 data-side 属性区分）。
	//
	// 定位语义（mygo 的 inset 与 CSS 一致）：
	//   - `Left(v)`  = 距父容器**左**边 v（即靠左）；
	//   - `Right(v)` = 距父容器**右**边 v（即靠右）。
	// 正文列居中，所以两侧对称：左边那条距左边 `offset`，
	// 右边那条距右边 `offset`。
	label := "调整会话宽度（右）"
	box := ui.Box(c).Absolute().Top(0).Bottom(0).Width(strip).
		Cursor(ui.CursorResizeEW).
		Label(label)
	if side < 0 {
		box.Label("调整会话宽度（左）").Left(offset)
	} else {
		box.Right(offset)
	}
	handle = box

	// 悬停/拖动时的竖向光条：**跟随指针的 Y 位置**（对齐 dsh 的 `::after`，
	// 它的渐变中心就是 `--dsh-width-handle-pointer-y`）。
	//
	// 坐标注意：`PointerPosition()` 返回的是**相对把手矩形**的坐标，
	// 而 `p.Fill` 用的是绝对坐标，必须 `r.Y + y` 换算，否则光条会画到别处。
	handle.Draw(func(p *ui.Painter, r ui.Rect) {
		_, y, over := handle.PointerPosition()
		if !over && !a.widthDragging {
			return
		}
		// 拖动期间指针可能移出这条窄条（横向拖很远），此时必须继续重绘，
		// 否则光条会停在最后一帧的位置。
		if a.widthDragging {
			p.AnimationFrame()
		}
		core := theme.TextMuted
		barX := r.X + r.W/2 - 1
		// 没拿到过指针位置时（纯拖动、指针已移出矩形）落在中央
		pointerY := r.Y + y
		if !over {
			pointerY = r.Y + r.H/2
		}
		// 16px 实芯 + 上下各 14px 渐隐（dsh：实芯 16px、每侧淡出 28px）
		const core16, fade = float32(16), float32(14)
		p.Fill(ui.Rect{X: barX, Y: pointerY - core16/2, W: 2, H: core16}, core, 1)
		p.Fill(ui.Rect{X: barX, Y: pointerY - core16/2 - fade, W: 2, H: fade}, core.Alpha(0.18), 1)
		p.Fill(ui.Rect{X: barX, Y: pointerY + core16/2, W: 2, H: fade}, core.Alpha(0.18), 1)
	})

	dx, _, held := handle.Dragged()
	wasDragging := a.widthDragging
	a.widthDragging = held
	// 松手这一帧落盘（对齐 dsh 的 onCommit：只在拖动结束时写）
	if wasDragging && !held {
		a.commitContentWidth()
	}
	if !held {
		return false
	}
	if dx == 0 {
		return false
	}
	// 只有一侧在动，但两侧同时扩：放大 2 倍（dsh 的 `outward * 2`）
	outward := dx
	if side < 0 {
		outward = -dx
	}
	a.dragContentWidth(outward * 2)
	return true
}

// beginContentWidthDrag 记录拖动起点（每帧增量累加需要基线）。
func (a *App) dragContentWidth(delta float32) {
	base := a.contentWidthPreference
	if base <= 0 {
		base = a.contentWidth()
	}
	a.contentWidthPreference = base + delta
	// 拖动过程中先按当前窗口夹住，松手时再落盘（避免存进越界值）
	a.contentWidthPreference = a.clampContentWidth(a.contentWidthPreference)
}

// clampContentWidth 把宽度夹到 [min, 随窗口衰减的上限]。
func (a *App) clampContentWidth(width float32) float32 {
	maxWidth := int(a.viewportW) - chatContentEdgeBudget
	if maxWidth < chatContentMin {
		maxWidth = chatContentMin
	}
	if maxWidth > chatContentMax {
		maxWidth = chatContentMax
	}
	return float32(clampInt(int(width), chatContentMin, maxWidth))
}

// commitContentWidth 在松手时保存偏好（对齐 dsh 的 onCommit）。
func (a *App) commitContentWidth() {
	if a.contentWidthPreference <= 0 {
		return
	}
	a.saveUIState()
}
