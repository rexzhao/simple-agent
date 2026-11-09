package markdown_test

import (
	"strings"
	"testing"

	"simple-agent/internal/markdown"
)

func parse(t *testing.T, source string) []markdown.Block {
	t.Helper()
	return markdown.NewParser().Parse(source)
}

func plain(t *testing.T, source string) string {
	t.Helper()
	return markdown.PlainText(parse(t, source))
}

func findSpan(blocks []markdown.Block, text string) (markdown.Span, bool) {
	for _, block := range blocks {
		for _, span := range block.Spans {
			if strings.Contains(span.Text, text) {
				return span, true
			}
		}
		for _, item := range block.Items {
			if span, ok := findSpan(item.Blocks, text); ok {
				return span, true
			}
		}
		if span, ok := findSpan(block.Blocks, text); ok {
			return span, true
		}
	}
	return markdown.Span{}, false
}

func TestParseParagraphAndInlineStyles(t *testing.T) {
	blocks := parse(t, "普通 **粗体** *斜体* ~~删除~~ `代码` 结尾")
	if len(blocks) != 1 || blocks[0].Kind != markdown.Paragraph {
		t.Fatalf("应是一个段落：%#v", blocks)
	}
	if s, ok := findSpan(blocks, "粗体"); !ok || !s.Bold {
		t.Fatalf("粗体未识别：%#v", blocks[0].Spans)
	}
	if s, ok := findSpan(blocks, "斜体"); !ok || !s.Italic {
		t.Fatalf("斜体未识别")
	}
	if s, ok := findSpan(blocks, "删除"); !ok || !s.Strike {
		t.Fatalf("删除线未识别")
	}
	if s, ok := findSpan(blocks, "代码"); !ok || !s.Code {
		t.Fatalf("行内代码未识别")
	}
}

func TestParseHeadingLevels(t *testing.T) {
	blocks := parse(t, "# 一级\n\n### 三级")
	if len(blocks) != 2 {
		t.Fatalf("应有两个标题：%#v", blocks)
	}
	if blocks[0].Kind != markdown.Heading || blocks[0].Level != 1 {
		t.Fatalf("一级标题不正确：%#v", blocks[0])
	}
	if blocks[1].Level != 3 {
		t.Fatalf("三级标题不正确：%#v", blocks[1])
	}
}

func TestParseCodeBlock(t *testing.T) {
	blocks := parse(t, "```go\nfmt.Println(\"hi\")\n```")
	if len(blocks) != 1 || blocks[0].Kind != markdown.CodeBlock {
		t.Fatalf("应是代码块：%#v", blocks)
	}
	if blocks[0].Lang != "go" {
		t.Fatalf("语言应为 go，实际 %q", blocks[0].Lang)
	}
	if !strings.Contains(blocks[0].Code, "fmt.Println") {
		t.Fatalf("代码内容丢失：%q", blocks[0].Code)
	}
}

// 流式时输入可能没写完：未闭合的代码块与列表不能崩，也不能把后面全部吞成代码。
func TestParseIncompleteInput(t *testing.T) {
	// 未闭合的围栏：goldmark 会把剩余内容当成代码块（这是可接受的），
	// 关键是不要 panic。
	blocks := parse(t, "前言\n\n```go\nfunc main() {")
	if len(blocks) == 0 {
		t.Fatal("未闭合代码块解析结果为空")
	}
	if blocks[0].Kind != markdown.Paragraph {
		t.Fatalf("代码块之前的段落应保留：%#v", blocks[0])
	}

	// 未闭合的列表
	blocks = parse(t, "- 第一项\n- 第二项")
	if len(blocks) != 1 || blocks[0].Kind != markdown.List {
		t.Fatalf("列表未解析：%#v", blocks)
	}
	if len(blocks[0].Items) != 2 {
		t.Fatalf("应有 2 个条目，实际 %d", len(blocks[0].Items))
	}

	// 只有半个强调标记
	blocks = parse(t, "这是 **没写完")
	if len(blocks) != 1 {
		t.Fatalf("半个强调标记不应改变块结构：%#v", blocks)
	}

	// 空输入
	if blocks := parse(t, ""); len(blocks) != 0 {
		t.Fatalf("空输入应返回空：%#v", blocks)
	}
}

func TestParseLists(t *testing.T) {
	blocks := parse(t, "- a\n- b\n  - b1\n\n1. x\n2. y")
	if len(blocks) != 2 {
		t.Fatalf("应有两个列表：%#v", blocks)
	}
	if blocks[0].Kind != markdown.List || blocks[0].Ordered {
		t.Fatalf("第一个应是无序列表：%#v", blocks[0])
	}
	if len(blocks[0].Items) != 2 {
		t.Fatalf("无序列表应有 2 项：%#v", blocks[0].Items)
	}
	if blocks[1].Kind != markdown.List || !blocks[1].Ordered {
		t.Fatalf("第二个应是有序列表：%#v", blocks[1])
	}
	if blocks[1].Start != 1 {
		t.Fatalf("有序列表起始应为 1，实际 %d", blocks[1].Start)
	}
}

func TestParseQuoteAndDivider(t *testing.T) {
	blocks := parse(t, "> 引用一行\n\n---\n\n普通")
	if len(blocks) != 3 {
		t.Fatalf("应是 引用 + 分隔线 + 段落：%#v", blocks)
	}
	if blocks[0].Kind != markdown.Quote {
		t.Fatalf("第一块应是引用：%v", blocks[0].Kind)
	}
	if blocks[1].Kind != markdown.Divider {
		t.Fatalf("第二块应是分隔线：%v", blocks[1].Kind)
	}
	if blocks[2].Kind != markdown.Paragraph {
		t.Fatalf("第三块应是段落：%v", blocks[2].Kind)
	}
}

func TestParseLinks(t *testing.T) {
	blocks := parse(t, "看 [文档](https://example.com/doc) 和 https://auto.link/x")
	span, ok := findSpan(blocks, "文档")
	if !ok || span.Link != "https://example.com/doc" {
		t.Fatalf("链接未识别：%#v", blocks[0].Spans)
	}
	if auto, ok := findSpan(blocks, "auto.link"); !ok || auto.Link == "" {
		t.Fatalf("自动链接未识别：%#v", blocks[0].Spans)
	}
}

// HTML 不渲染（模型的输出不可信）。
func TestParseIgnoresHTML(t *testing.T) {
	blocks := parse(t, "<div>hi</div>\n\n普通文本")
	for _, block := range blocks {
		for _, span := range block.Spans {
			if strings.Contains(span.Text, "<div>") {
				t.Fatalf("HTML 不应出现在渲染内容里：%#v", span)
			}
		}
	}
}

// PlainText 便于预览与测试。
func TestPlainText(t *testing.T) {
	got := plain(t, "# 标题\n\n正文 **粗**\n\n- 一\n- 二")
	for _, want := range []string{"标题", "正文", "粗", "一", "二"} {
		if !strings.Contains(got, want) {
			t.Fatalf("纯文本缺少 %q：%q", want, got)
		}
	}
}

// 缓存：键由 message id、part 序号与内容哈希决定。
func TestCacheKeyAndLRU(t *testing.T) {
	cache := markdown.NewCache(2)
	keyA := markdown.Key("m1", 0, "内容 A")
	keyB := markdown.Key("m1", 1, "内容 A")
	if keyA == keyB {
		t.Fatal("不同 part 的键应不同")
	}
	if keyA != markdown.Key("m1", 0, "内容 A") {
		t.Fatal("同样的输入应得到同样的键")
	}
	if keyA == markdown.Key("m1", 0, "内容 B") {
		t.Fatal("内容变化时键应变化")
	}

	cache.Put(keyA, []markdown.Block{{Kind: markdown.Paragraph}})
	if _, ok := cache.Get(keyA); !ok {
		t.Fatal("应命中")
	}
	keyC := markdown.Key("m2", 0, "内容 C")
	keyD := markdown.Key("m3", 0, "内容 D")
	cache.Put(keyC, nil)
	cache.Put(keyD, nil)
	if cache.Len() != 2 {
		t.Fatalf("超出上限应淘汰，实际 %d 条", cache.Len())
	}
	// keyA 是最久未用的（keyC 被 Get 过吗？没有）→ 淘汰最早写入的
	if _, ok := cache.Get(keyA); ok {
		t.Fatal("最久未用的项应被淘汰")
	}
	// keyA 命中 1 次；keyA 被淘汰后的那次查询计 1 次未命中
	if hits, misses := cache.Stats(); hits != 1 || misses != 1 {
		t.Fatalf("统计不一致：hits=%d misses=%d", hits, misses)
	}
}

// 表格：GFM 表格解析成表头 + 数据行 + 对齐。
func TestParseTable(t *testing.T) {
	source := "| 名称 | 数量 |\n|:---|---:|\n| 苹果 | 3 |\n| 香蕉 | 5 |\n"
	blocks := parse(t, source)
	if len(blocks) != 1 || blocks[0].Kind != markdown.Table {
		t.Fatalf("应是一个表格：%#v", blocks)
	}
	table := blocks[0]
	if len(table.Header) != 2 {
		t.Fatalf("应有 2 列：%#v", table.Header)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("应有 2 行数据：%#v", table.Rows)
	}
	if table.Aligns[0] != "left" || table.Aligns[1] != "right" {
		t.Fatalf("对齐信息不一致：%v", table.Aligns)
	}
	if text := markdown.PlainText(blocks); !strings.Contains(text, "苹果") {
		t.Fatalf("表格内容应能转成纯文本：%q", text)
	}
}

// 流式时表格只写了一半：不能 panic，也不影响后面的内容。
func TestParseIncompleteTable(t *testing.T) {
	blocks := parse(t, "| 名称 | 数量 |\n| 苹果 | 3 |")
	if len(blocks) == 0 {
		t.Fatal("半截表格也应产出内容")
	}
	// 分隔行还没写：goldmark 当成普通段落，可以接受，只要不崩
	_ = markdown.PlainText(blocks)
}

// 图片：先以 alt 文本占位，并带出 URL。
func TestParseImage(t *testing.T) {
	blocks := parse(t, "看到 ![截图](https://example.com/a.png) 了吗")
	span, ok := findSpan(blocks, "截图")
	if !ok {
		t.Fatalf("图片 alt 应作为占位显示：%#v", blocks[0].Spans)
	}
	if span.ImageURL != "https://example.com/a.png" {
		t.Fatalf("图片 URL 未带出：%#v", span)
	}
}

// 行内 HTML 原样显示（用户可能在讨论代码）。
func TestParseInlineRawHTML(t *testing.T) {
	blocks := parse(t, "这行有 <b>加粗</b> 标签")
	var found bool
	for _, span := range blocks[0].Spans {
		if strings.Contains(span.Text, "<b>") {
			found = true
		}
	}
	if !found {
		t.Fatalf("行内 HTML 不应被吞掉：%#v", blocks[0].Spans)
	}
}

// Linkify 的 www. 自动链接要补上 https，才能通过 scheme 白名单。
func TestParseWWWAutoLink(t *testing.T) {
	blocks := parse(t, "参考 www.example.com 的说明")
	span, ok := findSpan(blocks, "www.example.com")
	if !ok {
		t.Fatalf("应识别 www 链接：%#v", blocks[0].Spans)
	}
	if !strings.HasPrefix(span.Link, "https://") && !strings.HasPrefix(span.Link, "http://") {
		t.Fatalf("www 链接应带上 http(s) scheme：%q", span.Link)
	}
}

// 高亮：Go 代码应拆成带颜色的 token；未知语言退化成单色且不报错。
func TestHighlightGo(t *testing.T) {
	highlighter := markdown.NewHighlighter(false)
	lines := highlighter.Highlight("func main() {\n\tprintln(\"hi\")\n}\n", "go")
	if len(lines) != 3 {
		t.Fatalf("应有 3 行，实际 %d", len(lines))
	}
	var text strings.Builder
	colored := 0
	for _, line := range lines {
		for _, token := range line.Tokens {
			text.WriteString(token.Text)
			if token.Color != 0 {
				colored++
			}
		}
		text.WriteString("\n")
	}
	if !strings.Contains(text.String(), "func main()") {
		t.Fatalf("高亮后文本应保持一致：%q", text.String())
	}
	if colored == 0 {
		t.Fatal("Go 代码应有带颜色的 token")
	}
}

func TestHighlightUnknownLanguage(t *testing.T) {
	highlighter := markdown.NewHighlighter(true)
	lines := highlighter.Highlight("随便什么内容\n第二行", "")
	if len(lines) != 2 {
		t.Fatalf("退化成单色时仍按行切分，实际 %d 行", len(lines))
	}
	if lines[1].Tokens[0].Text != "第二行" {
		t.Fatalf("内容不一致：%#v", lines[1])
	}
	// 空代码不应产生行
	if got := highlighter.Highlight("", "go"); len(got) != 0 {
		t.Fatalf("空代码应返回空：%#v", got)
	}
}

// 高亮缓存键要区分语言。
func TestHighlightCacheKey(t *testing.T) {
	a := markdown.CacheKey("m1", 0, "go", "x := 1")
	b := markdown.CacheKey("m1", 0, "python", "x := 1")
	if a == b {
		t.Fatal("不同语言的键应不同")
	}
	if a != markdown.CacheKey("m1", 0, "go", "x := 1") {
		t.Fatal("同样输入应得到同样的键")
	}
}
