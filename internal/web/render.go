package web

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTML→markdown 转换：只留正文。
//
// 为什么不用第三方转换库：模型需要的是"能读的正文"，标题/段落/列表/链接/代码/
// 表格已覆盖实际页面绝大部分内容，剩下的 div/span 结构对模型没有意义。用官方
// 的 x/net/html 解析（容错性与浏览器一致），自己遍历比引入 turndown 的 Go 移植
// 更可控——尤其是"整块丢弃脚本/样式/隐藏元素"这条，prompt injection 就在那里。

// maxNestingDepth 是遍历的嵌套上限。
//
// 不是随手定的：未闭合标签会让树的深度爆炸，递归遍历能卡死（dsh 在 turndown 上
// 实测同一问题，把上限定在 512）。真实页面几十层，512 对内容足够，对攻击不够。
const maxNestingDepth = 512

// blockTags 是周围要有空行的元素（markdown 的段落边界）。
var blockTags = map[atom.Atom]bool{
	atom.Address: true, atom.Article: true, atom.Aside: true, atom.Blockquote: true,
	atom.Body: true, atom.Center: true, atom.Dd: true, atom.Div: true, atom.Dl: true,
	atom.Dt: true, atom.Fieldset: true, atom.Figcaption: true, atom.Figure: true,
	atom.Footer: true, atom.Form: true, atom.Header: true, atom.Hr: true,
	atom.Html: true, atom.Li: true, atom.Main: true, atom.Nav: true,
	atom.Ol: true, atom.P: true, atom.Section: true, atom.Table: true,
	atom.Tbody: true, atom.Tfoot: true, atom.Thead: true, atom.Tr: true, atom.Ul: true,
}

// headingLevels 是 h1..h6 的级别。
var headingLevels = map[atom.Atom]int{
	atom.H1: 1, atom.H2: 2, atom.H3: 3, atom.H4: 4, atom.H5: 5, atom.H6: 6,
}

// ToMarkdown 把 HTML 转成 markdown 正文。
//
// 解析失败返回空串：调用方据此给出"无法转换"的结果，绝不要把原始标签直接塞给
// 模型（既难读，也让不可信内容更容易伪装成指令）。
func ToMarkdown(source string) string {
	root, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return ""
	}
	renderer := &markdownRenderer{}
	renderer.walk(root, 0)
	return tidy(renderer.out.String())
}

type markdownRenderer struct {
	out strings.Builder
	// lists 是当前打开的列表（嵌套时按层缩进）。
	lists []listFrame
}

type listFrame struct {
	ordered bool
	index   int
}

// walk 递归渲染 node 的子节点。
func (r *markdownRenderer) walk(node *html.Node, depth int) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		r.node(child, depth+1)
	}
}

// node 渲染一个节点。
func (r *markdownRenderer) node(node *html.Node, depth int) {
	// 深度超限：不再往下走。深层内容会丢，但保证转换一定会结束。
	if depth > maxNestingDepth {
		return
	}

	switch node.Type {
	case html.TextNode:
		r.text(collapseSpaces(node.Data))
		return
	case html.DocumentNode:
		r.walk(node, depth)
		return
	case html.ElementNode:
	default:
		return // 注释、doctype 一律丢弃
	}

	if isHidden(node) {
		return
	}

	switch node.DataAtom {
	case atom.Script, atom.Style, atom.Noscript, atom.Template,
		atom.Iframe, atom.Object, atom.Embed, atom.Svg, atom.Canvas, atom.Head:
		return
	case atom.Br:
		r.out.WriteString("\n")
	case atom.Hr:
		r.blank()
		r.out.WriteString("---\n")
		r.blank()
	case atom.Img:
		r.image(node)
	case atom.Input, atom.Select, atom.Textarea:
		// 表单控件没有正文价值（按钮的文字在 Button 分支里照常渲染）
	case atom.A:
		r.link(node, depth)
	case atom.Pre:
		r.pre(node)
	case atom.Code:
		r.out.WriteString("`")
		r.walk(node, depth)
		r.out.WriteString("`")
	case atom.Ul, atom.Ol:
		r.list(node, depth)
	case atom.Table:
		r.table(node)
	default:
		if level, ok := headingLevels[node.DataAtom]; ok {
			r.blank()
			r.out.WriteString(strings.Repeat("#", level) + " ")
			r.walk(node, depth)
			r.blank()
			return
		}
		if blockTags[node.DataAtom] {
			r.blank()
			r.walk(node, depth)
			r.blank()
			return
		}
		r.walk(node, depth)
	}
}

// link 渲染 <a href>：文本之后跟地址；没有文本时按普通行内内容渲染。
func (r *markdownRenderer) link(node *html.Node, depth int) {
	href := attr(node, "href")
	if href == "" {
		r.walk(node, depth)
		return
	}
	// 先渲染到子渲染器：链接文字首尾的空白要剪掉（`[ text ](url)` 没必要），
	// 而分列两边的空白属于周边文本，由 text 负责。
	text := strings.TrimSpace(r.capture(func(sub *markdownRenderer) {
		sub.walk(node, depth)
	}))
	if text == "" {
		// 没有可见文字（图标链接、空 <a>）：不产出空的 md 链接
		return
	}
	r.out.WriteString("[" + text + "](" + href + ")")
}

// capture 用子渲染器渲染一段内容并把结果取回来。
//
// strings.Builder 不能回退，需要"先看内容再决定怎么写"的地方（链接、图片 alt）
// 都走这里；列表状态要带过去，否则嵌套列表的缩进会错。
func (r *markdownRenderer) capture(render func(sub *markdownRenderer)) string {
	sub := &markdownRenderer{lists: r.lists}
	render(sub)
	r.lists = sub.lists
	return sub.out.String()
}

// image 渲染 <img alt src>。
func (r *markdownRenderer) image(node *html.Node) {
	src := attr(node, "src")
	if src == "" {
		return
	}
	alt := strings.TrimSpace(attr(node, "alt"))
	if alt == "" {
		alt = "图片"
	}
	r.out.WriteString("![" + escapeBrackets(alt) + "](" + src + ")")
}

// pre 渲染 <pre>（通常内含 <code>）为围栏代码块。
func (r *markdownRenderer) pre(node *html.Node) {
	language := ""
	if code := findChild(node, atom.Code); code != nil {
		for _, token := range strings.Fields(attr(code, "class")) {
			if after, ok := strings.CutPrefix(token, "language-"); ok {
				language = after
				break
			}
		}
	}
	body := strings.TrimRight(textContent(node), "\n")
	// 代码里自带 ``` 会提前闭合围栏：用更长的围栏包住
	fence := "```"
	for strings.Contains(body, fence) {
		fence += "`"
	}
	r.blank()
	r.out.WriteString(fence + language + "\n" + body + "\n" + fence + "\n")
	r.blank()
}

// list 渲染 <ul>/<ol>，嵌套按缩进，有序列表自己数序号。
func (r *markdownRenderer) list(node *html.Node, depth int) {
	r.blank()
	r.lists = append(r.lists, listFrame{ordered: node.DataAtom == atom.Ol})
	for item := node.FirstChild; item != nil; item = item.NextSibling {
		if item.Type != html.ElementNode || item.DataAtom != atom.Li {
			continue
		}
		frame := &r.lists[len(r.lists)-1]
		marker := "- "
		if frame.ordered {
			frame.index++
			marker = strconv.Itoa(frame.index) + ". "
		}
		r.out.WriteString(strings.Repeat("  ", len(r.lists)-1) + marker)
		r.listItem(item, depth)
		r.blank()
	}
	r.lists = r.lists[:len(r.lists)-1]
	r.blank()
}

// listItem 渲染一项 <li>：块级子元素之后缩进续行，才能留在同一项里。
func (r *markdownRenderer) listItem(item *html.Node, depth int) {
	indent := strings.Repeat("  ", len(r.lists))
	first := true
	for child := item.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && (child.DataAtom == atom.Ul || child.DataAtom == atom.Ol) {
			r.blank()
			r.list(child, depth)
			continue
		}
		if !first && isBlock(child) {
			r.out.WriteString("\n" + indent)
		}
		first = false
		r.node(child, depth+1)
	}
}

// table 把 <table> 收成一张 GFM 表格。
//
// 为什么整表处理而不逐 <tr> 输出：GFM 的表头分隔行属于**表格**，逐行输出会让
// 第二行起变成"没有分隔行的表格"，goldmark 渲染出来是错位的（实测）。
func (r *markdownRenderer) table(node *html.Node) {
	var rows [][]string
	collectRows(node, &rows)
	if len(rows) == 0 {
		return
	}
	width := 0
	for _, row := range rows {
		width = max(width, len(row))
	}
	if width == 0 {
		return
	}

	r.blank()
	for index, row := range rows {
		r.out.WriteString(renderRow(row, width) + "\n")
		if index == 0 {
			r.out.WriteString(renderSeparator(width) + "\n")
		}
	}
	r.blank()
}

// collectRows 递归收集表格里的每一行（thead/tbody/tfoot 都算）。
func collectRows(node *html.Node, rows *[][]string) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != html.ElementNode {
			continue
		}
		switch child.DataAtom {
		case atom.Tr:
			var cells []string
			for cell := child.FirstChild; cell != nil; cell = cell.NextSibling {
				if cell.Type == html.ElementNode && (cell.DataAtom == atom.Td || cell.DataAtom == atom.Th) {
					cells = append(cells, cellText(cell))
				}
			}
			*rows = append(*rows, cells)
		case atom.Table:
			// 嵌套表格当独立表格处理（跳到同级）
			continue
		default:
			collectRows(child, rows)
		}
	}
}

// cellText 把单元格压成单行（GFM 单元格不能换行），并转义管道符。
func cellText(cell *html.Node) string {
	text := strings.Join(strings.Fields(textContent(cell)), " ")
	return strings.ReplaceAll(text, "|", `\|`)
}

// renderRow 渲染一行，缺的单元格补空。
func renderRow(cells []string, width int) string {
	var builder strings.Builder
	builder.WriteString("|")
	for index := 0; index < width; index++ {
		value := ""
		if index < len(cells) {
			value = cells[index]
		}
		builder.WriteString(" " + value + " |")
	}
	return builder.String()
}

// renderSeparator 渲染表头分隔行（统一左对齐，不解析 align/colspan）。
func renderSeparator(width int) string {
	var builder strings.Builder
	builder.WriteString("|")
	for index := 0; index < width; index++ {
		builder.WriteString(" --- |")
	}
	return builder.String()
}

// text 写文本节点。
//
// 行内空格必须保留：`<a>x</a> for details` 里的那个空格决定了渲染结果是
// `[x](url) for details` 还是 `[x](url)for details`（后者在 markdown 里
// 依然能读，但把单词粘在了一起）。只有当输出当前位置是行首（或还什么都没写）时
// 才丢掉前导空格，避免每段开头多一个缩进。
func (r *markdownRenderer) text(text string) {
	if r.out.Len() == 0 || strings.HasSuffix(r.out.String(), "\n") {
		text = strings.TrimLeft(text, " ")
	}
	r.out.WriteString(text)
}

// blank 写一个段落边界（不产生连续空行）。
func (r *markdownRenderer) blank() {
	current := r.out.String()
	switch {
	case current == "":
		return
	case strings.HasSuffix(current, "\n\n"):
		return
	case strings.HasSuffix(current, "\n"):
		r.out.WriteString("\n")
	default:
		r.out.WriteString("\n\n")
	}
}

// collapseSpaces 把连续空白压成**一个**空格（HTML 里换行不代表换行）。
//
// 首尾的空白必须保留成一个空格：它常常是行内元素之间的分隔。
// `<a>x</a>\nfor details` 里的那个换行就是 `[x](url) for details` 里的空格，
// 压掉它会让单词粘在一起（真实页面上到处都是这种写法）。行首要不要这个空格
// 由 text 决定。
func collapseSpaces(text string) string {
	var builder strings.Builder
	space := false
	for _, char := range text {
		switch char {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			space = true
		default:
			if space {
				builder.WriteByte(' ')
			}
			space = false
			builder.WriteRune(char)
		}
	}
	if space {
		builder.WriteByte(' ')
	}
	return builder.String()
}

// textContent 取节点的全部后代文本（表格单元格、代码块用）。
func textContent(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		switch current.Type {
		case html.TextNode:
			builder.WriteString(current.Data)
			return
		case html.ElementNode:
			if current.DataAtom == atom.Br {
				builder.WriteString("\n")
				return
			}
			if isHidden(current) {
				return
			}
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

// isBlock 判断元素是否块级（列表项续行缩进用）。
func isBlock(node *html.Node) bool {
	if node.Type != html.ElementNode {
		return false
	}
	if blockTags[node.DataAtom] {
		return true
	}
	_, heading := headingLevels[node.DataAtom]
	return heading
}

// isHidden 判断元素是否被显式隐藏（hidden 属性 / aria-hidden / display:none）。
func isHidden(node *html.Node) bool {
	if _, ok := attrOk(node, "hidden"); ok {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(attr(node, "aria-hidden")), "true") {
		return true
	}
	for _, declaration := range strings.Split(strings.ToLower(attr(node, "style")), ";") {
		property, value, ok := strings.Cut(declaration, ":")
		if !ok {
			continue
		}
		property = strings.TrimSpace(property)
		value = strings.TrimSpace(strings.ReplaceAll(value, "!important", ""))
		if property == "display" && value == "none" {
			return true
		}
		if property == "visibility" && (value == "hidden" || value == "collapse") {
			return true
		}
	}
	return false
}

// attr 取属性值（不存在返回空串）。
func attr(node *html.Node, name string) string {
	value, _ := attrOk(node, name)
	return value
}

// attrOk 取属性值与是否存在。
func attrOk(node *html.Node, name string) (string, bool) {
	for _, item := range node.Attr {
		if item.Key == name {
			return item.Val, true
		}
	}
	return "", false
}

// findChild 找第一个指定标签的直接子元素。
func findChild(node *html.Node, tag atom.Atom) *html.Node {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.DataAtom == tag {
			return child
		}
	}
	return nil
}

// escapeBrackets 转义 markdown 的方括号（图片 alt 用）。
func escapeBrackets(text string) string {
	return strings.NewReplacer("[", `\[`, "]", `\]`).Replace(text)
}

// tidy 收尾：去行尾空白、压掉连续空行、去掉首尾空行。
func tidy(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, line)
			continue
		}
		blank = 0
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
