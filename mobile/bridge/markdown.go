package bridge

import "simple-agent/internal/markdown"

// markdown 块结构转换：把 markdown 中间表示转成跨进程的 BlockView。
//
// 为什么在 Go 侧解析：桌面的 markdown 由 uiapp 用同一份 `internal/markdown`
// 渲染，Android 如果自己再实现一遍（或者干脆把原文交给 Compose 的 Markdown 库），
// 两边的换行、表格、行内代码样式迟早会分叉。这里只做"结构转换"，不做样式。

// blocks 解析一段 markdown 并转成可编码的块。
func (c *Client) blocks(text string) []BlockView {
	parsed := c.parseMarkdown(text)
	if len(parsed) == 0 {
		return nil
	}
	return toBlocks(parsed)
}

// toBlocks 递归转换块。
func toBlocks(blocks []markdown.Block) []BlockView {
	out := make([]BlockView, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, toBlock(block))
	}
	return out
}

// toBlock 转换单个块。
func toBlock(block markdown.Block) BlockView {
	view := BlockView{
		Kind:    blockKind(block.Kind),
		Level:   block.Level,
		Lang:    block.Lang,
		Code:    block.Code,
		Spans:   toSpans(block.Spans),
		Ordered: block.Ordered,
		Start:   block.Start,
		Aligns:  block.Aligns,
	}
	for _, item := range block.Items {
		view.Items = append(view.Items, toBlocks(item.Blocks))
	}
	view.Blocks = toBlocks(block.Blocks)
	for _, cell := range block.Header {
		view.Header = append(view.Header, toSpans(cell))
	}
	for _, row := range block.Rows {
		cells := make([][]SpanView, 0, len(row))
		for _, cell := range row {
			cells = append(cells, toSpans(cell))
		}
		view.Rows = append(view.Rows, cells)
	}
	return view
}

// toSpans 转换行内内容。
func toSpans(spans []markdown.Span) []SpanView {
	if len(spans) == 0 {
		return nil
	}
	out := make([]SpanView, 0, len(spans))
	for _, span := range spans {
		out = append(out, SpanView{
			Text:     span.Text,
			Bold:     span.Bold,
			Italic:   span.Italic,
			Strike:   span.Strike,
			Code:     span.Code,
			Link:     span.Link,
			ImageURL: span.ImageURL,
		})
	}
	return out
}

// blockKind 把 markdown.Kind 转成稳定的字符串（Kotlin 侧据此选布局）。
func blockKind(kind markdown.Kind) string {
	switch kind {
	case markdown.Paragraph:
		return "paragraph"
	case markdown.Heading:
		return "heading"
	case markdown.CodeBlock:
		return "code"
	case markdown.List:
		return "list"
	case markdown.Quote:
		return "quote"
	case markdown.Divider:
		return "divider"
	case markdown.Table:
		return "table"
	}
	return "paragraph"
}
