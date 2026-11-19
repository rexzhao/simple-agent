package web_test

import (
	"strings"
	"testing"

	"simple-agent/internal/web"
)

// 表格：每张表只输出一条表头分隔行，单元格转义管道符。
//
// 回归：早期实现逐 <tr> 输出，第二行起就落在"没有分隔行的表格"里，
// goldmark 渲染出来是错位的（真实页面上的参数表直接不可读）。
func TestTableRendering(t *testing.T) {
	html := `<table>
<thead><tr><th>参数</th><th>说明</th></tr></thead>
<tbody>
<tr><td>a|b</td><td>管道符</td></tr>
<tr><td colspan="2">跨列</td></tr>
</tbody></table>`
	out := web.ToMarkdown(html)
	if got := strings.Count(out, "| --- |"); got != 1 {
		t.Fatalf("分隔行应只出现一次（每张表一条），实际 %d 次：\n%s", got, out)
	}
	for _, want := range []string{"| 参数 | 说明 |", `| a\|b | 管道符 |`, "| 跨列 |  |"} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q：\n%s", want, out)
		}
	}
	// 两列的表格每行都补齐成两格（colspan 不展开，缺的补空）。
	// 数「空格+竖线」而不是竖线：单元格里的 `\|` 是转义，不算分列。
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "|") && strings.Count(line, " |") != 2 {
			t.Fatalf("每行应是两列：%q\n%s", line, out)
		}
	}
}

// 标题、列表（含嵌套与有序）、代码块、引用。
func TestBlocksAndLists(t *testing.T) {
	html := `<h1>一</h1><h3>三</h3>
<ul><li>甲</li><li>乙<ul><li>乙-1</li></ul></li></ul>
<ol><li>第一</li><li>第二</li></ol>
<pre><code class="language-go">fmt.Println("hi")
</code></pre>
<blockquote>引用</blockquote>`
	out := web.ToMarkdown(html)
	for _, want := range []string{
		"# 一", "### 三",
		"- 甲", "  - 乙-1",
		"1. 第一", "2. 第二",
		"```go\nfmt.Println(\"hi\")\n```",
		"引用",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q：\n%s", want, out)
		}
	}
}

// 脚本/样式/隐藏元素整块丢弃，正文里的同类词不受影响。
func TestDropsNonVisibleContent(t *testing.T) {
	html := `<div>
<script>var scriptText = 1</script>
<style>.a{color:red}</style>
<noscript>noscriptText</noscript>
<div hidden>hiddenText</div>
<div aria-hidden="true">ariaText</div>
<div style="display: none">displayNoneText</div>
<div style="visibility: hidden !important">visibilityText</div>
<p>正文本 visibleText</p>
</div>`
	out := web.ToMarkdown(html)
	for _, gone := range []string{"scriptText", "noscriptText", "hiddenText", "ariaText", "displayNoneText", "visibilityText", "color:red"} {
		if strings.Contains(out, gone) {
			t.Fatalf("%q 不该出现在正文里：\n%s", gone, out)
		}
	}
	if !strings.Contains(out, "visibleText") {
		t.Fatalf("正文应保留：\n%s", out)
	}
}

// 链接与图片：空文本的链接不留空括号，图片带 alt。
func TestLinksAndImages(t *testing.T) {
	out := web.ToMarkdown(`<p><a href="/x">文档</a> <a href="/y"></a>
<img src="/a.png" alt="图[1]"> <img src="/b.png"></p>`)
	if !strings.Contains(out, "[文档](/x)") {
		t.Fatalf("链接应转成 markdown：\n%s", out)
	}
	if !strings.Contains(out, `![图\[1\]](/a.png)`) {
		t.Fatalf("图片应带 alt 并转义方括号：\n%s", out)
	}
	if !strings.Contains(out, "![图片](/b.png)") {
		t.Fatalf("缺 alt 时应有占位：\n%s", out)
	}
	if strings.Contains(out, "[](") {
		t.Fatalf("空文字链接不该产出空括号：\n%s", out)
	}
}

// 回归（真实页面）：链接与后续文字之间只有一个换行时，那个空白是一个空格，
// 不能丢——否则 `[x](url)for details` 会把两个词粘在一起。
//
// go.dev 的文档全是这种写法，压掉后模型读到的句子就是错的。
func TestInlineSpacesAroundLinksAreKept(t *testing.T) {
	out := web.ToMarkdown("<p>See the <a href=\"/spec\">language spec</a>\nfor details.</p>")
	if !strings.Contains(out, "[language spec](/spec) for details.") {
		t.Fatalf("链接后的空格应保留：%q", out)
	}
	// 链接文字首尾不留下多余空格
	if strings.Contains(out, "[ language spec ]") {
		t.Fatalf("链接文字不该带首尾空格：%q", out)
	}
	// 段首也不该多出缩进
	if strings.HasPrefix(out, " ") {
		t.Fatalf("段首不该有空格：%q", out)
	}
}

// 实体与空白：HTML 实体解码、行内换行折叠成空格。
func TestEntitiesAndWhitespace(t *testing.T) {
	out := web.ToMarkdown("<p>a &amp; b &lt;c&gt;\n   多行\n  折叠</p>")
	if !strings.Contains(out, "a & b <c> 多行 折叠") {
		t.Fatalf("实体与空白处理不对：%q", out)
	}
}

// 深层嵌套不会卡死（上限定在 512 层），也不会输出未闭合的垃圾。
func TestDeepNestingIsBounded(t *testing.T) {
	deep := strings.Repeat("<div>", 5000) + "x" + strings.Repeat("</div>", 5000)
	out := web.ToMarkdown(deep)
	if len(out) > 1000 {
		t.Fatalf("深层嵌套应被截断，实际长度 %d", len(out))
	}
}

// 空输入与纯标签输入都返回空串（调用方据此给"无法转换"）。
func TestEmptyAndMarkupOnly(t *testing.T) {
	if out := web.ToMarkdown(""); out != "" {
		t.Fatalf("空输入应返回空串：%q", out)
	}
	if out := web.ToMarkdown("<div><span></span></div>"); out != "" {
		t.Fatalf("没有文本时应返回空串：%q", out)
	}
}

// 代码块里自带 ``` 时要加长围栏，否则 markdown 会被提前闭合。
func TestCodeFenceIsLengthened(t *testing.T) {
	out := web.ToMarkdown("<pre><code>a\n```\nb</code></pre>")
	if !strings.Contains(out, "````") {
		t.Fatalf("内部含 ``` 时应加长围栏：\n%s", out)
	}
}
