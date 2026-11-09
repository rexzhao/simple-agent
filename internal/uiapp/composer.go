package uiapp

import (
	"strings"

	"github.com/egoist/mygo/ui"
)

// Composer 是输入框的键位语义实现。
//
// 背景（M4 探针实测）：mygo 的多行编辑器对 Enter 的处理是"带修饰键就丢弃"
// （editor_navigation.go 的 wants()：`case KeyEnter: return m == 0`），所以
// Alt+Enter 到不了编辑器，不会插入换行；而外层 Shortcut 又抢不过编辑器。
//
// 唯一能在编辑器之前拿到按键的地方是焦点元素的 HandleInput。因此：
//   - Enter（无修饰键）→ 认领，交给调用方发送；
//   - Alt+Enter → 认领，在光标处插入换行（编辑器不暴露插入 API，只能改写绑定值）；
//   - Shift+Enter → 放行（编辑器把 Shift 剥掉后当普通 Enter 处理，会插换行）；
//   - 输入法组合期间一律放行，任何键都归输入法（Composing()）。
type Composer struct {
	// Draft 是输入框绑定的文本。
	Draft *string
	// Send 在 Enter 时调用（返回 true 表示已处理，按键被认领）。
	Send func()
	// Area 是本帧的文本输入元素，用于读写光标位置。
	Area *ui.Element
	// History 处理 ↑/↓ 翻历史：delta 为 -1（更早）/ +1（更晚），caret 是
	// 当前光标位置（rune 下标）。返回 true 表示已认领该按键（文本已被替换）。
	History func(delta, caret int) bool
	// Paste 处理 Ctrl/Cmd+V：返回 true 表示"我拿走了这张图片"，按键被认领
	// （编辑器不会再粘贴文本）。剪贴板里没有图片时必须返回 false。
	Paste func() bool
	// RequestCaret 请求把光标放到指定位置（**下一帧**生效）。
	//
	// 为什么不在改完文本的当下设置：HandleInput 执行时编辑器缓冲还是**旧**
	// 文本，SetTextSelection 会被旧长度截断（实测光标停在 0）。
	RequestCaret func(pos int)
}

// HandleInput 是挂在输入框上的 HandleInput 回调。
func (c *Composer) HandleInput(ev ui.InputEvent) bool {
	if ev.Kind != ui.InputKeyDown {
		return false
	}
	// 输入法组合期间所有按键都归输入法（Enter 是选词，↑↓ 是选候选）
	if c.Area != nil && c.Area.Composing() {
		return false
	}
	if ev.Key == ui.KeyUp || ev.Key == ui.KeyDown {
		return c.handleArrow(ev)
	}
	if ev.Key == ui.KeyV && ev.Mods == ui.Cmd {
		// 只有剪贴板里**真的有图片**才认领；否则放行给编辑器按文本粘贴
		return c.Paste != nil && c.Paste()
	}
	if ev.Key != ui.KeyEnter {
		return false
	}

	switch {
	case ev.Mods&ui.Alt != 0:
		c.insertNewlineAtCaret()
		return true
	case ev.Mods == 0:
		if c.Send != nil {
			c.Send()
		}
		return true
	default:
		// Shift+Enter 交给编辑器（它会插入换行）；Ctrl+Enter 等不处理
		return false
	}
}

// handleArrow 处理 ↑/↓：只有光标在第一行（↑）/最后一行（↓）时才翻历史，
// 其余情况交给编辑器移动光标。
//
// 带修饰键时不认领（Alt+↑ 之类留给编辑器/系统）。
func (c *Composer) handleArrow(ev ui.InputEvent) bool {
	if c.History == nil || c.Draft == nil || c.Area == nil || ev.Mods != 0 {
		return false
	}
	line, lines, caret := c.caretState()
	switch ev.Key {
	case ui.KeyUp:
		if line != 0 {
			return false
		}
		if !c.History(-1, caret) {
			return false
		}
	case ui.KeyDown:
		if line != lines-1 {
			return false
		}
		if !c.History(1, caret) {
			return false
		}
	}
	if c.RequestCaret != nil {
		c.RequestCaret(len([]rune(*c.Draft)))
	}
	// 光标位置由调用方在**下一帧**设置：这一帧编辑器的缓冲区还没拿到新文本，
	// 立刻 SetTextSelection 会被旧长度截断（实测会把光标留到 0）。
	return true
}

// caretState 返回光标所在行号、草稿的逻辑行数与光标位置（rune 下标）。
func (c *Composer) caretState() (line, lines, caret int) {
	if c.Draft == nil || c.Area == nil {
		return 0, 1, 0
	}
	start, _ := c.Area.TextSelection()
	runes := []rune(*c.Draft)
	start = clamp(start, 0, len(runes))
	line = strings.Count(string(runes[:start]), "\n")
	lines = strings.Count(*c.Draft, "\n") + 1
	return line, lines, start
}

// insertNewlineAtCaret 在光标处插入换行。
//
// 编辑器没有"插入文本"的公开 API，只能改写绑定的 *string 再用
// SetTextSelection 把光标放到换行之后。副作用：换行本身不是独立的撤销步骤
// （Ctrl+Z 会连同这一轮输入一起撤销），与编辑器把连续输入合并成一条撤销记录的
// 行为一致，不算新增缺陷。
func (c *Composer) insertNewlineAtCaret() {
	if c.Draft == nil || c.Area == nil {
		return
	}
	start, end := c.Area.TextSelection()
	runes := []rune(*c.Draft)
	start = clamp(start, 0, len(runes))
	end = clamp(end, start, len(runes))

	var builder strings.Builder
	builder.Grow(len(*c.Draft) + 1)
	builder.WriteString(string(runes[:start]))
	builder.WriteByte('\n')
	builder.WriteString(string(runes[end:]))
	*c.Draft = builder.String()

	// 光标要落在换行之后：同样留到下一帧设置（这一帧缓冲还是旧文本）
	if c.RequestCaret != nil {
		c.RequestCaret(start + 1)
	}
}

func clamp(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
