// Package markdown 把 markdown 解析成「块 + 行内 span」的渲染中间表示。
//
// 只用 goldmark 解析（不手写解析器），这里只负责 AST → 中间表示；真正画到屏幕上
// 是 uiapp 的事。这样解析/缓存都能脱离 UI 单测。
//
// 中间表示刻意做得很小：块（段落/标题/代码块/列表/引用/分隔线）与行内 span
// （样式 + 链接）。表格、图片在 M7 第二批接入。
package markdown

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// Kind 是块的类型。
type Kind int

const (
	// Paragraph 是段落。
	Paragraph Kind = iota
	// Heading 是标题（Level 是 1..6）。
	Heading
	// CodeBlock 是代码块（Lang 是围栏语言，可能为空）。
	CodeBlock
	// List 是列表（Ordered 决定项目符号，Items 是各条目）。
	List
	// Quote 是引用块（Blocks 是里面的内容）。
	Quote
	// Divider 是分隔线。
	Divider
	// Table 是 GFM 表格（Header 是表头，Rows 是数据行）。
	Table
)

// Span 是行内的一段文本。
type Span struct {
	Text   string
	Bold   bool
	Italic bool
	Strike bool
	Code   bool
	// Link 非空表示这段是可点击的链接。
	Link string
	// ImageURL 非空表示这是一张图片（Text 是 alt 文本作为占位）。
	ImageURL string
}

// Item 是列表的一项。
type Item struct {
	Blocks []Block
}

// Block 是一个渲染块。
type Block struct {
	Kind    Kind
	Level   int     // 标题级别
	Ordered bool    // 列表是否有序
	Start   int     // 有序列表的起始序号
	Lang    string  // 代码块语言
	Code    string  // 代码块内容
	Spans   []Span  // 段落/标题/引用的行内内容
	Blocks  []Block // 引用块内部
	Items   []Item  // 列表项
	// Header 是表头各单元格的行内内容；Rows 是数据行。
	Header [][]Span
	Rows   [][][]Span
	// Aligns 是各列对齐（"left"/"center"/"right"）。
	Aligns []string
}

// Parser 解析 markdown（goldmark 实例可复用）。
type Parser struct {
	md goldmark.Markdown
}

// NewParser 构造解析器。
func NewParser() *Parser {
	return &Parser{md: goldmark.New(
		goldmark.WithExtensions(
			extension.Strikethrough,
			extension.Linkify,
			extension.Table,
		),
	)}
}

// Parse 解析 markdown 文本。
//
// 容错：流式时输入可能还没写完（代码围栏没闭合、列表没结束），goldmark 对不完整
// 输入是宽容的；这里再兜一层——任何 panic 都退化成「一个纯文本段落」，绝不因为
// 模型的半句话让界面崩掉。
func (p *Parser) Parse(source string) (blocks []Block) {
	defer func() {
		if recover() != nil {
			blocks = []Block{{Kind: Paragraph, Spans: []Span{{Text: source}}}}
		}
	}()
	if source == "" {
		return nil
	}

	root := p.md.Parser().Parse(text.NewReader([]byte(source)))
	blocks = p.walkChildren(root, []byte(source))
	return blocks
}

// walkChildren 遍历容器节点的子节点。
func (p *Parser) walkChildren(node ast.Node, source []byte) []Block {
	var blocks []Block
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if block, ok := p.block(child, source); ok {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// block 把一个 AST 节点转成渲染块。
func (p *Parser) block(node ast.Node, source []byte) (Block, bool) {
	switch n := node.(type) {
	case *ast.Paragraph:
		return Block{Kind: Paragraph, Spans: p.inline(n, source)}, true
	case *ast.TextBlock:
		// 紧凑列表里的段落
		return Block{Kind: Paragraph, Spans: p.inline(n, source)}, true
	case *ast.Heading:
		return Block{Kind: Heading, Level: n.Level, Spans: p.inline(n, source)}, true
	case *ast.FencedCodeBlock:
		return Block{
			Kind: CodeBlock,
			Lang: string(n.Language(source)),
			Code: codeText(n, source),
		}, true
	case *ast.CodeBlock:
		return Block{Kind: CodeBlock, Code: codeText(n, source)}, true
	case *ast.List:
		block := Block{Kind: List, Ordered: n.IsOrdered(), Start: n.Start}
		for item := n.FirstChild(); item != nil; item = item.NextSibling() {
			entry := Item{}
			for child := item.FirstChild(); child != nil; child = child.NextSibling() {
				if inner, ok := p.block(child, source); ok {
					entry.Blocks = append(entry.Blocks, inner)
				}
			}
			block.Items = append(block.Items, entry)
		}
		return block, true
	case *ast.Blockquote:
		return Block{Kind: Quote, Blocks: p.walkChildren(n, source)}, true
	case *ast.ThematicBreak:
		return Block{Kind: Divider}, true
	case *extast.Table:
		return p.table(n, source), true
	case *ast.HTMLBlock:
		// 不渲染 HTML（模型的输出不可信，也避免出现半截标签）
		return Block{}, false
	}
	return Block{}, false
}

// codeText 取代码块的文本。
func codeText(node ast.Node, source []byte) string {
	var builder bytes.Buffer
	for i := 0; i < node.Lines().Len(); i++ {
		segment := node.Lines().At(i)
		builder.Write(segment.Value(source))
	}
	return builder.String()
}

// table 把 GFM 表格转成渲染块。
func (p *Parser) table(node *extast.Table, source []byte) Block {
	block := Block{Kind: Table}
	for _, alignment := range node.Alignments {
		switch alignment {
		case extast.AlignCenter:
			block.Aligns = append(block.Aligns, "center")
		case extast.AlignRight:
			block.Aligns = append(block.Aligns, "right")
		default:
			block.Aligns = append(block.Aligns, "left")
		}
	}
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if child.Kind() == extast.KindTableHeader {
			block.Header = p.cells(child, source)
			continue
		}
		block.Rows = append(block.Rows, p.cells(child, source))
	}
	return block
}

// cells 收集一行里的各单元格行内内容。
func (p *Parser) cells(row ast.Node, source []byte) [][]Span {
	var out [][]Span
	for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
		out = append(out, p.inline(cell, source))
	}
	return out
}

// inlinePlain 取节点下的纯文本（图片 alt 用）。
func inlinePlain(node ast.Node, source []byte) string {
	var builder bytes.Buffer
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if text, ok := child.(*ast.Text); ok {
			builder.Write(text.Segment.Value(source))
			continue
		}
		builder.WriteString(inlinePlain(child, source))
	}
	return builder.String()
}

// inline 收集节点下的行内 span。
func (p *Parser) inline(node ast.Node, source []byte) []Span {
	state := &inlineState{source: source}
	p.collectInline(node, state, style{})
	return state.spans
}

// style 是行内样式的累积状态（嵌套的强调会叠加）。
type style struct {
	bold, italic, strike bool
	code                 bool
	link                 string
}

type inlineState struct {
	source []byte
	spans  []Span
}

// collectInline 递归收集行内节点。
func (p *Parser) collectInline(node ast.Node, state *inlineState, current style) {
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch n := child.(type) {
		case *ast.Text:
			text := string(n.Segment.Value(state.source))
			if n.SoftLineBreak() || n.HardLineBreak() {
				text += "\n"
			}
			state.push(Span{
				Text:   text,
				Bold:   current.bold,
				Italic: current.italic,
				Strike: current.strike,
				Code:   current.code,
				Link:   current.link,
			})
		case *ast.String:
			state.push(Span{
				Text:   string(n.Value),
				Bold:   current.bold,
				Italic: current.italic,
				Strike: current.strike,
				Code:   current.code,
				Link:   current.link,
			})
		case *ast.CodeSpan:
			var builder bytes.Buffer
			for inner := n.FirstChild(); inner != nil; inner = inner.NextSibling() {
				if text, ok := inner.(*ast.Text); ok {
					builder.Write(text.Segment.Value(state.source))
				}
			}
			codeStyle := current
			codeStyle.code = true
			state.push(Span{
				Text:   builder.String(),
				Bold:   codeStyle.bold,
				Italic: codeStyle.italic,
				Strike: codeStyle.strike,
				Code:   true,
				Link:   codeStyle.link,
			})
		case *ast.Emphasis:
			switch n.Level {
			case 1:
				current.italic = true
			case 2:
				current.bold = true
			}
			p.collectInline(n, state, current)
		case *ast.Link:
			linkStyle := current
			linkStyle.link = string(n.Destination)
			p.collectInline(n, state, linkStyle)
		case *ast.Image:
			// 图片先以 alt 文本占位（界面会异步加载真正的图片）
			alt := inlinePlain(n, state.source)
			if alt == "" {
				alt = "图片"
			}
			state.push(Span{
				Text:     alt,
				Bold:     current.bold,
				Italic:   current.italic,
				ImageURL: string(n.Destination),
			})
		case *ast.RawHTML:
			// 行内 HTML 原样显示：用户可能正在讨论 HTML 代码，静默吞掉更糟
			var builder bytes.Buffer
			for i := 0; i < n.Segments.Len(); i++ {
				segment := n.Segments.At(i)
				builder.Write(segment.Value(state.source))
			}
			state.push(Span{Text: builder.String(), Bold: current.bold, Italic: current.italic})
		case *ast.AutoLink:
			url := string(n.URL(state.source))
			// Linkify 识别出的 www.example.com 没有 scheme，补上 https 才可点
			if strings.HasPrefix(strings.ToLower(url), "www.") {
				url = "https://" + url
			}
			state.push(Span{Text: url, Link: url, Italic: current.italic, Bold: current.bold})
		default:
			// 删除线等扩展节点：名字匹配，避免依赖具体类型
			if child.Kind().String() == "Strikethrough" {
				strikeStyle := current
				strikeStyle.strike = true
				p.collectInline(child, state, strikeStyle)
				continue
			}
			p.collectInline(child, state, current)
		}
	}
}

// push 追加 span，相同样式且相邻的合并（减少 span 数量）。
func (s *inlineState) push(span Span) {
	if span.Text == "" {
		return
	}
	if len(s.spans) > 0 {
		last := &s.spans[len(s.spans)-1]
		if last.Bold == span.Bold && last.Italic == span.Italic &&
			last.Strike == span.Strike && last.Code == span.Code &&
			last.Link == span.Link && last.ImageURL == span.ImageURL &&
			span.ImageURL == "" {
			last.Text += span.Text
			return
		}
	}
	s.spans = append(s.spans, span)
}

// PlainText 把块压成纯文本（测试与预览用）。
func PlainText(blocks []Block) string {
	var builder bytes.Buffer
	for _, block := range blocks {
		builder.WriteString(blockPlain(block))
		builder.WriteString("\n")
	}
	return builder.String()
}

func blockPlain(block Block) string {
	switch block.Kind {
	case CodeBlock:
		return block.Code
	case List:
		var builder bytes.Buffer
		for _, item := range block.Items {
			for _, inner := range item.Blocks {
				builder.WriteString(blockPlain(inner))
			}
			builder.WriteString("\n")
		}
		return builder.String()
	case Quote:
		var builder bytes.Buffer
		for _, inner := range block.Blocks {
			builder.WriteString(blockPlain(inner))
		}
		return builder.String()
	case Divider:
		return "---"
	case Table:
		var builder bytes.Buffer
		for _, cell := range block.Header {
			builder.WriteString(spansPlain(cell))
			builder.WriteString(" | ")
		}
		builder.WriteString("\n")
		for _, row := range block.Rows {
			for _, cell := range row {
				builder.WriteString(spansPlain(cell))
				builder.WriteString(" | ")
			}
			builder.WriteString("\n")
		}
		return builder.String()
	}
	return spansPlain(block.Spans)
}

// spansPlain 把行内 span 拼成纯文本。
func spansPlain(spans []Span) string {
	var builder bytes.Buffer
	for _, span := range spans {
		builder.WriteString(span.Text)
	}
	return builder.String()
}
