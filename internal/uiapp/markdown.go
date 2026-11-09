package uiapp

import (
	"hash/fnv"
	"strings"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/markdown"
)

// 本文件把 markdown 的中间表示画成 mygo 元素。
//
// 缓存：键是 (messageID, part 序号, 内容哈希)。视图是状态的纯函数，每帧都会重新
// 渲染全部可见消息；没有缓存的话流式时每帧都要重新解析所有可见消息。

const (
	// listMarkerCharWidth 是列表编号/圆点的单字符宽度估算（13px 字号）。
	// 只用来算「所有编号里最宽的那个」给一个统一宽度，不需要精确：估小了
	// 编号会被裁掉，估大了只是多留一点白。
	listMarkerCharWidth = 8
	// listMarkerGap 是编号与正文之间的水平间距。
	listMarkerGap = 4
)

// renderMarkdown 渲染一条消息里的 markdown 文本。
func (a *App) renderMarkdown(c *ui.Context, messageID string, partIndex int, source string) {
	blocks := a.markdownBlocks(messageID, partIndex, source)
	a.renderBlocks(c, messageID, partIndex, blocks, 0)
}

// markdownBlocks 取（或解析并缓存）渲染块。
func (a *App) markdownBlocks(messageID string, partIndex int, source string) []markdown.Block {
	key := markdown.Key(messageID, partIndex, source)
	if blocks, ok := a.mdCache.Get(key); ok {
		return blocks
	}
	blocks := a.mdParser.Parse(source)
	a.mdCache.Put(key, blocks)
	return blocks
}

// renderBlocks 渲染一组块。depth 用于列表/引用的缩进。
func (a *App) renderBlocks(c *ui.Context, messageID string, partIndex int, blocks []markdown.Block, depth int) {
	theme := c.Theme()
	for _, block := range blocks {
		switch block.Kind {
		case markdown.Heading:
			size := float32(15)
			switch block.Level {
			case 1:
				size = 19
			case 2:
				size = 17
			case 3:
				size = 16
			}
			ui.RichText(c, a.spans(block.Spans, theme)...).FontSize(size).Bold().Selectable()

		case markdown.CodeBlock:
			a.renderCodeBlock(c, messageID, partIndex, block)

		case markdown.List:
			// 编号/圆点必须和首行文字**顶端对齐**。
			//
			// `ui.Row` 默认把交叉轴对齐设成 `Center`（element.go 的 Row()），
			// 而列表项经常是多行（截图里的编号就飘到了两行中间）。所以这里显式
			// 用 Start；编号再给一个右对齐的固定宽度，让「9.」和「10.」的
			// 文字左边界对齐，而不是随位数左右跳。
			markers := make([]string, len(block.Items))
			markerWidth := float32(0)
			for index := range block.Items {
				if block.Ordered {
					markers[index] = itoa(block.Start+index) + "."
				} else {
					markers[index] = "•"
				}
				if width := float32(len([]rune(markers[index]))) * listMarkerCharWidth; width > markerWidth {
					markerWidth = width
				}
			}
			markerWidth += listMarkerGap
			for index, item := range block.Items {
				marker := markers[index]
				ui.Row(c).Gap(listMarkerGap).AlignItems(ui.Start).Children(func() {
					ui.Text(c, marker).Width(markerWidth).TextAlign(ui.End).
						FontSize(13).TextColor(theme.TextMuted)
					ui.Column(c).Grow(1).Children(func() {
						a.renderBlocks(c, messageID, partIndex, item.Blocks, depth+1)
					})
				})
			}

		case markdown.Quote:
			ui.Row(c).Gap(8).Padding(2, 0).Children(func() {
				ui.Box(c).Width(3).Background(theme.Border).Radius(2)
				ui.Column(c).Grow(1).Children(func() {
					a.renderBlocks(c, messageID, partIndex, block.Blocks, depth+1)
				})
			})

		case markdown.Divider:
			ui.Divider(c)

		case markdown.Table:
			a.renderTable(c, block)

		default: // Paragraph
			a.renderParagraph(c, block.Spans, theme)
		}
	}
}

// renderParagraph 渲染段落；含链接时用行内元素拼（可点击）。
func (a *App) renderParagraph(c *ui.Context, spans []markdown.Span, theme *ui.Theme) {
	if len(spans) == 0 {
		return
	}
	// 含图片：图片单独一行渲染（它是块级元素，塞不进 RichText）
	hasImage := false
	for _, span := range spans {
		if span.ImageURL != "" {
			hasImage = true
			break
		}
	}
	if hasImage {
		text := make([]markdown.Span, 0, len(spans))
		for _, span := range spans {
			if span.ImageURL != "" {
				if len(text) > 0 {
					ui.RichText(c, a.spans(text, theme)...).TextColor(theme.Text).Selectable()
					text = text[:0]
				}
				a.renderImage(c, span.Text, span.ImageURL)
				continue
			}
			text = append(text, span)
		}
		if len(text) > 0 {
			ui.RichText(c, a.spans(text, theme)...).TextColor(theme.Text).Selectable()
		}
		return
	}

	hasLink := false
	for _, span := range spans {
		if span.Link != "" {
			hasLink = true
			break
		}
	}
	if !hasLink {
		ui.RichText(c, a.spans(spans, theme)...).TextColor(theme.Text).LineHeight(1.6).Selectable()
		return
	}

	// 有链接：用 RichText 的行内 Children，链接保持可点击
	ui.RichText(c).TextColor(theme.Text).LineHeight(1.6).Selectable().Children(func() {
		for _, span := range spans {
			if span.Link == "" {
				ui.Text(c, span.Text)
				continue
			}
			if !allowedURL(span.Link) {
				// 不安全的 scheme（file: 等）当普通文本显示，不可点
				ui.Text(c, span.Text).TextColor(theme.TextMuted)
				continue
			}
			link := span.Link
			// 悬停显示真实 URL；文本本身是 URL 但主机不同时额外提示（防钓鱼）
			label := span.Text
			if hint := linkHostHint(span.Text, link); hint != "" {
				label += " → " + hint
			}
			element := ui.Link(c, label, "")
			element.Tooltip(link)
			if element.Clicked() {
				c.OpenURL(link)
			}
		}
	})
}

// spans 把行内 span 转成 mygo 的 RichText span。
func (a *App) spans(spans []markdown.Span, theme *ui.Theme) []ui.Span {
	out := make([]ui.Span, 0, len(spans))
	for _, span := range spans {
		style := ui.Span{Text: span.Text}
		if span.Bold {
			style.Weight = 700
		}
		if span.Italic {
			style.Italic = true
		}
		if span.Strike {
			style.Strikethrough = true
		}
		if span.Code {
			// 行内代码：dsh 是「浅底小 chip、正常字重、等宽字体」，不是加粗
			// （dsh：`ui-renderer` 的 markdown inline code 样式 + 令牌
			// `--dsw-alias-markdown-inline-code`）。
			style.Font = dshCodeFontStack
			style.Background = dsh.InlineCode
			style.Size = 12
		}
		if span.Link != "" && allowedURL(span.Link) {
			style.Color = theme.Accent
			style.Underline = true
		}
		out = append(out, style)
	}
	return out
}

// linkHostHint 在「链接文字本身是 URL，但主机与目标不同」时返回目标主机。
//
// 只在这种情况提示：普通的 [文档](https://…) 不该到处加标注，但
// [https://bank.com](https://evil.com) 这种典型钓鱼要能看出来。
func linkHostHint(text, target string) string {
	textURL := strings.TrimSpace(text)
	lower := strings.ToLower(textURL)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return ""
	}
	textHost := hostOfURL(textURL)
	targetHost := hostOfURL(target)
	if textHost == "" || targetHost == "" || textHost == targetHost {
		return ""
	}
	return targetHost
}

// hostOfURL 取 URL 的主机部分（小写，去掉端口）。
func hostOfURL(url string) string {
	lower := strings.ToLower(strings.TrimSpace(url))
	index := strings.Index(lower, "//")
	if index < 0 {
		return ""
	}
	rest := lower[index+2:]
	if cut := strings.IndexAny(rest, "/?#"); cut >= 0 {
		rest = rest[:cut]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:] // 去掉 user:pass@
	}
	if colon := strings.LastIndex(rest, ":"); colon >= 0 && !strings.Contains(rest[colon:], "]") {
		rest = rest[:colon]
	}
	return strings.Trim(rest, "[]")
}

// allowedURL 只允许 http / https / mailto。
//
// 模型的输出不可信：file:/javascript:/自定义 scheme 一律不打开，
// 否则一句"点这里"就能让用户在本地打开任意文件。
func allowedURL(url string) bool {
	lower := strings.ToLower(strings.TrimSpace(url))
	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
		return true
	case strings.HasPrefix(lower, "mailto:"):
		return true
	}
	return false
}

// itoa 是 strconv.Itoa 的小包装（避免在这个文件里 import strconv）。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	negative := value < 0
	if negative {
		value = -value
	}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// renderCodeBlock 渲染代码块（chroma 高亮，走同一份 LRU 缓存）。
//
// 缓存键含语言与内容：流式时代码块每帧都在变，只有内容没变才复用。
// blockIndex 给代码块一个稳定标识：同一段代码在同一个 part 里内容相同，
// 用代码本身的哈希（而不是遍历下标）就能在每帧之间保持一致。
func blockIndex(block markdown.Block) int {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(block.Lang))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(block.Code))
	return int(hash.Sum32() & 0x7fffffff)
}

func (a *App) renderCodeBlock(c *ui.Context, messageID string, partIndex int, block markdown.Block) {
	theme := c.Theme()
	lines := a.highlightCode(messageID, partIndex, block.Lang, block.Code, theme.Dark)

	// 代码块有自己的 id：复制按钮的"已复制"反馈要按块区分
	key := "code:" + messageID + "#" + itoa(partIndex) + "#" + itoa(blockIndex(block))
	ui.Column(c).Padding(10).Radius(8).Background(theme.Surface).
		Border(1, theme.Border).FillWidth().Gap(4).Children(func() {
		ui.Row(c).FillWidth().Gap(6).Children(func() {
			if block.Lang != "" {
				ui.Text(c, block.Lang).FontSize(10).TextColor(theme.TextMuted)
			}
			ui.Spacer(c)
			// 只复制这一段代码：不带 ``` 围栏，也不带语言标记。
			// 无障碍标签带上语言，多个代码块才能被区分（测试也用它定位）
			a.copyButton(c, key, block.Code, a.tr("chat.copyCode", block.Lang))
		})
		// 横向滚动：长行不该把窗口撑宽
		ui.ScrollHorizontal(c).Children(func() {
			ui.Column(c).Children(func() {
				for _, line := range lines {
					ui.RichText(c, highlightSpans(line, theme)...).Font(dshCodeFontStack).FontSize(12).Selectable()
				}
			})
		})
	})
}

// highlightCode 取（或计算并缓存）代码高亮结果。
func (a *App) highlightCode(messageID string, partIndex int, lang, code string, dark bool) []markdown.HighlightLine {
	// 深浅主题的 token 颜色不同，缓存键必须带上模式
	mode := "light"
	if dark {
		mode = "dark"
	}
	key := markdown.CacheKey(messageID, partIndex, mode+":"+lang, code)
	if lines, ok := a.hlCache.Get(key); ok {
		return lines
	}
	highlighter := a.highlighter
	if highlighter == nil || highlighter.Dark() != dark {
		highlighter = markdown.NewHighlighter(dark)
		a.highlighter = highlighter
	}
	lines := highlighter.Highlight(code, lang)
	a.hlCache.Put(key, lines)
	return lines
}

// highlightSpans 把一行高亮 token 转成 RichText span。
func highlightSpans(line markdown.HighlightLine, theme *ui.Theme) []ui.Span {
	spans := make([]ui.Span, 0, len(line.Tokens))
	for _, token := range line.Tokens {
		style := ui.Span{Text: token.Text}
		if token.Color != 0 {
			style.Color = ui.RGB(uint8(token.Color>>16), uint8(token.Color>>8), uint8(token.Color))
		} else {
			style.Color = theme.Text
		}
		if token.Bold {
			style.Weight = 700
		}
		if token.Italic {
			style.Italic = true
		}
		spans = append(spans, style)
	}
	return spans
}
