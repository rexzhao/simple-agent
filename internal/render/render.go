// Package render 把 wire 层的 Part 分发成强类型渲染视图（纯逻辑，不依赖 UI）。
//
// 设计原则（迁移自 Rust 版）：
//   - wire 层（message.Part）保持 A2A 的四种内容不扩展，保证与 A2A JSON 双向无损；
//   - OpenAI/Anthropic 风格的内容块（tool use/result 等）在 provider adapter 边界
//     转成 Data part，用本包约定的 mediaType 标识，此处按 (内容, mediaType) 分类；
//   - 分类规则基于规范化后的 MIME 主体（截掉 `;` 参数、去空白、忽略大小写）。
package render

import (
	json "encoding/json/v2"
	"strings"

	"simple-agent/internal/message"
)

// 本项目约定的 mediaType（工具调用/结果、推理过程、摘要）。
const (
	MediaMarkdown  = "text/markdown"
	MediaPlainText = "text/plain"
	// MediaToolUse 承载工具调用：{"id","name","args"}。
	MediaToolUse = "application/vnd.simple-agent.tool-use+json"
	// MediaToolResult 承载工具结果：{"id","name","output","is_error"}。
	MediaToolResult = "application/vnd.simple-agent.tool-result+json"
	// MediaReasoning 承载模型推理过程（弱化展示）。
	MediaReasoning = "text/vnd.simple-agent.reasoning"
	// MediaSummary 承载上下文压缩后的摘要。
	MediaSummary = "text/vnd.simple-agent.summary"
)

// Kind 是渲染视图的类型标签。
type Kind string

const (
	KindMarkdown   Kind = "markdown"
	KindPlainText  Kind = "plainText"
	KindCode       Kind = "code"
	KindReasoning  Kind = "reasoning"
	KindSummary    Kind = "summary"
	KindImage      Kind = "image"
	KindFile       Kind = "file"
	KindToolUse    Kind = "toolUse"
	KindToolResult Kind = "toolResult"
	KindJSON       Kind = "json"
)

// ImageSource 是图片内容来源。
type ImageSource struct {
	// URL 非空表示远程图片。
	URL string
	// Bytes 非 nil 表示内嵌字节。
	Bytes []byte
	// Blob 非空表示本地内容寻址存储里的引用（`blob:sha256:<hex>`）里的 hash。
	// 与 URL 互斥：它不是网络资源，界面必须经 blob 存储读取，绝不能当远程图抓。
	Blob string
	// MediaType 是规范化后的图片 MIME（image/png 等）。
	MediaType string
}

// FileSource 是非图片文件来源。
type FileSource struct {
	URL       string
	Bytes     []byte
	MediaType string
}

// ToolPayload 是工具调用/结果的借用视图（不复制 JSON 树）。
type ToolPayload struct {
	ID      string
	Name    string
	Args    any
	Output  any
	IsError bool
	// OK 表示数据形状符合约定（name 至少存在）；否则渲染层应降级为 JSON。
	OK bool
}

// View 是 Part 的渲染视图。用 Kind + 字段而不是接口，便于 mygo 侧 switch。
type View struct {
	Kind Kind

	// 文本类
	Text     string
	Language string
	Media    string

	// 图片/文件
	Image *ImageSource
	File  *FileSource
	Name  string // 文件名

	// 工具
	Tool *ToolPayload

	// 原始 JSON（KindJSON 时）
	JSON any

	// 注意：不保存 Part 引用——Classify 收到的是副本，留引用会让人误以为
	// 可以改回会话；G3 的缓存键用 (messageID, partIndex) 就够了。
}

// Classify 把 Part 分发成渲染视图。
func Classify(p message.Part) View {
	view := classify(p)
	if view.Kind == "" {
		// 内容为 nil 等编程错误：返回安全的 JSON 视图，避免 UI 解引用 nil
		view = View{Kind: KindJSON}
	}
	return view
}

func classify(p message.Part) View {
	switch content := p.Content.(type) {
	case message.TextPart:
		switch {
		case mimeEqual(p.MediaType, MediaPlainText):
			return View{Kind: KindPlainText, Text: content.Text}
		case mimeEqual(p.MediaType, MediaReasoning):
			return View{Kind: KindReasoning, Text: content.Text}
		case mimeEqual(p.MediaType, MediaSummary):
			return View{Kind: KindSummary, Text: content.Text}
		case p.MediaType == "" || mimeEqual(p.MediaType, MediaMarkdown):
			// 只有未声明或显式 markdown 才按 markdown 渲染；
			// 显式声明其他文本类型（text/x-rust 等）按代码块，避免代码被当语法解析。
			return View{Kind: KindMarkdown, Text: content.Text}
		default:
			return View{
				Kind:     KindCode,
				Text:     content.Text,
				Language: codeLanguage(mimeEssence(p.MediaType)),
				Media:    p.MediaType,
			}
		}

	case message.URLPart:
		if isImageMedia(p.MediaType) {
			// `blob:` 引用是本地 blob 存储，不是远程图片：单独标出来，
			// 界面据此走本地读取（markdown 里的 blob: 仍然不被接受）。
			if hash, ok := BlobHash(content.URL); ok {
				return View{
					Kind:  KindImage,
					Image: &ImageSource{Blob: hash, MediaType: canonicalImageType(mimeEssence(p.MediaType))},
					Name:  p.Filename,
				}
			}
			return View{
				Kind:  KindImage,
				Image: &ImageSource{URL: content.URL, MediaType: canonicalImageType(mimeEssence(p.MediaType))},
				Name:  p.Filename,
			}
		}
		return View{
			Kind: KindFile,
			File: &FileSource{URL: content.URL, MediaType: p.MediaType},
			Name: p.Filename,
		}

	case message.RawPart:
		if isImageMedia(p.MediaType) {
			return View{
				Kind:  KindImage,
				Image: &ImageSource{Bytes: content.Raw, MediaType: canonicalImageType(mimeEssence(p.MediaType))},
				Name:  p.Filename,
			}
		}
		if p.MediaType == "" {
			// 无声明时按文件头嗅探（看内容，不算猜）
			if sniffed := SniffImage(content.Raw); sniffed != "" {
				return View{
					Kind:  KindImage,
					Image: &ImageSource{Bytes: content.Raw, MediaType: sniffed},
					Name:  p.Filename,
				}
			}
		}
		return View{
			Kind: KindFile,
			File: &FileSource{Bytes: content.Raw, MediaType: p.MediaType},
			Name: p.Filename,
		}

	case message.DataPart:
		switch {
		case mimeEqual(p.MediaType, MediaToolUse):
			if tool, ok := parseToolUse(content.Data); ok {
				return View{Kind: KindToolUse, Tool: tool}
			}
			// 畸形数据降级为 JSON，不崩
			return View{Kind: KindJSON, JSON: content.Data}
		case mimeEqual(p.MediaType, MediaToolResult):
			if tool, ok := parseToolResult(content.Data); ok {
				return View{Kind: KindToolResult, Tool: tool}
			}
			return View{Kind: KindJSON, JSON: content.Data}
		default:
			return View{Kind: KindJSON, JSON: content.Data}
		}
	}
	return View{Kind: KindJSON}
}

// parseToolUse 解析 {"id","name","args"}；name 缺失视为畸形。
func parseToolUse(data any) (*ToolPayload, bool) {
	obj, ok := data.(map[string]any)
	if !ok {
		return nil, false
	}
	name, _ := obj["name"].(string)
	if name == "" {
		return nil, false
	}
	id, _ := obj["id"].(string)
	return &ToolPayload{ID: id, Name: name, Args: obj["args"], OK: true}, true
}

// parseToolResult 解析 {"id","name","output"/"content","is_error"}。
func parseToolResult(data any) (*ToolPayload, bool) {
	obj, ok := data.(map[string]any)
	if !ok {
		return nil, false
	}
	name, _ := obj["name"].(string)
	if name == "" {
		return nil, false
	}
	id, _ := obj["id"].(string)
	output, hasOutput := obj["output"]
	if !hasOutput {
		output = obj["content"] // 兼容别名只对 result 生效
	}
	isError, _ := obj["is_error"].(bool)
	return &ToolPayload{ID: id, Name: name, Output: output, IsError: isError, OK: true}, true
}

// mimeEssence 返回 MIME 主体：截掉 `;` 参数并去空白（保留原大小写）。
// BlobHash 解析 `blob:sha256:<64 位小写 hex>` 引用（providers 组装请求时用它）。
//
// 这里自己实现一份（而不是 import store），是为了让 render 保持"只看消息内容"
// 的纯函数定位；规则必须与 store.ValidBlobHash 一致（store 侧另有测试）。
func BlobHash(url string) (string, bool) {
	const prefix = "blob:sha256:"
	if !strings.HasPrefix(url, prefix) {
		return "", false
	}
	hash := url[len(prefix):]
	if len(hash) != 64 {
		return "", false
	}
	for index := 0; index < len(hash); index++ {
		ch := hash[index]
		if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' {
			continue
		}
		return "", false
	}
	return hash, true
}

func mimeEssence(mediaType string) string {
	if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
		mediaType = mediaType[:idx]
	}
	return strings.TrimSpace(mediaType)
}

// mimeEqual 忽略大小写与参数比较 MIME。
func mimeEqual(mediaType, want string) bool {
	return strings.EqualFold(mimeEssence(mediaType), want)
}

// isImageMedia 判断是否 image/*（忽略大小写与参数）。
func isImageMedia(mediaType string) bool {
	essence := mimeEssence(mediaType)
	return len(essence) >= 6 && strings.EqualFold(essence[:6], "image/")
}

// canonicalImageType 把常见图片 MIME 规范化为小写标准名；未知原样返回。
var canonicalImageTypes = map[string]string{
	"image/png":  "image/png",
	"image/jpeg": "image/jpeg",
	"image/jpg":  "image/jpeg",
	"image/gif":  "image/gif",
	"image/webp": "image/webp",
	"image/bmp":  "image/bmp",
	"image/avif": "image/avif",
}

func canonicalImageType(essence string) string {
	if canonical, ok := canonicalImageTypes[strings.ToLower(essence)]; ok {
		return canonical
	}
	return essence
}

// codeLanguage 从 text/x-rust 这类 MIME 提取语言名（小写）。
func codeLanguage(essence string) string {
	lower := strings.ToLower(essence)
	subtype, ok := strings.CutPrefix(lower, "text/")
	if !ok {
		// 常见 application 类型映射
		switch lower {
		case "application/json":
			return "json"
		case "application/xml":
			return "xml"
		case "application/javascript":
			return "js"
		case "application/toml":
			return "toml"
		case "application/yaml":
			return "yaml"
		}
		return ""
	}
	return strings.TrimPrefix(subtype, "x-")
}

// SniffImage 按文件头嗅探图片类型，无法识别返回空串。
func SniffImage(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// AsJSON 把任意值序列化成紧凑 JSON（日志/调试用）。
func AsJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}
