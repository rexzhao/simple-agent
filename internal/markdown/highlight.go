package markdown

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"

	// 常见语言的内置词法分析器（chroma 按需注册）
	_ "github.com/alecthomas/chroma/v2/lexers"
)

// HighlightToken 是一个着色片段。
type HighlightToken struct {
	Text string
	// Color 是 0xRRGGBB（0 表示用默认前景色）。
	Color  uint32
	Bold   bool
	Italic bool
}

// HighlightLine 是一行代码。
type HighlightLine struct {
	Tokens []HighlightToken
}

// Highlighter 把代码块切成带颜色的行。
//
// 用 chroma（成熟的高亮库），只做「代码 → 行 + token」，画到屏幕上是 uiapp 的事。
// 未知语言退化成单色（不报错）——模型经常写 ```` ```text ```` 或干脆不写语言。
type Highlighter struct {
	style *chroma.Style
	dark  bool
}

// NewHighlighter 构造高亮器（dark 决定用深色还是浅色主题）。
func NewHighlighter(dark bool) *Highlighter {
	name := "github"
	if dark {
		name = "github-dark"
	}
	style := styles.Get(name)
	if style == nil {
		style = styles.Fallback
	}
	return &Highlighter{style: style, dark: dark}
}

// Dark 报告这个高亮器用的是深色主题。
func (h *Highlighter) Dark() bool { return h.dark }

// Highlight 高亮一段代码。
func (h *Highlighter) Highlight(code, lang string) []HighlightLine {
	code = strings.TrimRight(code, "\n")
	if code == "" {
		return nil
	}

	var lexer chroma.Lexer
	if lang != "" {
		lexer = lexers.Get(lang)
	}
	if lexer == nil {
		// 没有语言提示：按纯文本处理（保留换行，不尝试猜测）
		return plainLines(code)
	}
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, code)
	if err != nil {
		return plainLines(code)
	}
	var tokens []chroma.Token
	for {
		token := iterator()
		if token == chroma.EOF {
			break
		}
		tokens = append(tokens, token)
	}
	return h.lines(tokens)
}

// lines 把 chroma 的 token 流切成按行组织的着色片段。
func (h *Highlighter) lines(tokens []chroma.Token) []HighlightLine {
	var out []HighlightLine
	line := HighlightLine{}
	flush := func() {
		out = append(out, line)
		line = HighlightLine{}
	}
	for _, token := range tokens {
		entry := h.style.Get(token.Type)
		color := uint32(entry.Colour)
		if entry.Colour == 0 {
			color = 0
		}
		parts := strings.Split(token.Value, "\n")
		for index, part := range parts {
			if index > 0 {
				flush()
			}
			if part == "" {
				continue
			}
			line.Tokens = append(line.Tokens, HighlightToken{
				Text:   part,
				Color:  color,
				Bold:   entry.Bold == chroma.Yes,
				Italic: entry.Italic == chroma.Yes,
			})
		}
	}
	if len(line.Tokens) > 0 {
		flush()
	}
	return out
}

// plainLines 把代码按行切成单色片段。
func plainLines(code string) []HighlightLine {
	lines := strings.Split(code, "\n")
	out := make([]HighlightLine, 0, len(lines))
	for _, line := range lines {
		out = append(out, HighlightLine{Tokens: []HighlightToken{{Text: line}}})
	}
	return out
}

// CacheKey 生成代码高亮的缓存键（语言与内容都参与）。
func CacheKey(messageID string, partIndex int, lang, code string) string {
	return Key(messageID, partIndex, "code:"+lang+":"+code)
}
