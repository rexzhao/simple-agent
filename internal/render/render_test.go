package render_test

import (
	"strings"
	"testing"

	"simple-agent/internal/message"
	"simple-agent/internal/render"
)

func TestClassifyTextKinds(t *testing.T) {
	tests := []struct {
		name     string
		part     message.Part
		wantKind render.Kind
		wantLang string
	}{
		{"未声明 → markdown", message.NewTextPart("# hi"), render.KindMarkdown, ""},
		{"显式 markdown", message.NewTextPart("# hi").WithMediaType(render.MediaMarkdown), render.KindMarkdown, ""},
		{"text/plain 带参数", message.NewTextPart("hi").WithMediaType("text/plain; charset=utf-8"), render.KindPlainText, ""},
		{"大写 TEXT/PLAIN", message.NewTextPart("hi").WithMediaType("TEXT/PLAIN"), render.KindPlainText, ""},
		{"text/x-rust → code", message.NewTextPart("fn main(){}").WithMediaType("text/x-rust"), render.KindCode, "rust"},
		{"TEXT/X-RUST → code", message.NewTextPart("fn main(){}").WithMediaType("TEXT/X-RUST"), render.KindCode, "rust"},
		{"application/json → code json", message.NewTextPart("{}").WithMediaType("application/json"), render.KindCode, "json"},
		{"推理过程", message.NewTextPart("想一想").WithMediaType(render.MediaReasoning), render.KindReasoning, ""},
		{"摘要", message.NewTextPart("摘要").WithMediaType(render.MediaSummary), render.KindSummary, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			view := render.Classify(tc.part)
			if view.Kind != tc.wantKind {
				t.Fatalf("kind 应为 %v，实际 %v", tc.wantKind, view.Kind)
			}
			if tc.wantLang != "" && view.Language != tc.wantLang {
				t.Fatalf("language 应为 %q，实际 %q", tc.wantLang, view.Language)
			}
		})
	}
}

func TestClassifyURL(t *testing.T) {
	img := render.Classify(message.NewURLPart("https://x.test/a.png").WithMediaType("Image/PNG"))
	if img.Kind != render.KindImage || img.Image == nil {
		t.Fatalf("image/* 应为 KindImage，实际 %#v", img)
	}
	if img.Image.MediaType != "image/png" {
		t.Fatalf("mediaType 应规范化为 image/png，实际 %q", img.Image.MediaType)
	}

	file := render.Classify(message.NewURLPart("https://x.test/a.pdf").WithMediaType("application/pdf"))
	if file.Kind != render.KindFile {
		t.Fatalf("非图片 URL 应为 KindFile，实际 %v", file.Kind)
	}

	// 无 mediaType 的 URL 不猜类型
	unknown := render.Classify(message.NewURLPart("https://x.test/a.png"))
	if unknown.Kind != render.KindFile {
		t.Fatalf("无 mediaType 的 URL 应按文件展示，实际 %v", unknown.Kind)
	}
}

func TestClassifyRaw(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}

	declared := render.Classify(message.NewRawPart(png).WithMediaType("image/png"))
	if declared.Kind != render.KindImage {
		t.Fatalf("声明 image/* 应为 KindImage，实际 %v", declared.Kind)
	}

	sniffed := render.Classify(message.NewRawPart(png))
	if sniffed.Kind != render.KindImage || sniffed.Image.MediaType != "image/png" {
		t.Fatalf("PNG 魔数应嗅探为 image/png，实际 %#v", sniffed)
	}

	// 无声明且非图片 → 文件（支持另存为），不是 Unknown
	blob := render.Classify(message.NewRawPart([]byte{0, 1, 2, 3}))
	if blob.Kind != render.KindFile {
		t.Fatalf("未知字节应为 KindFile，实际 %v", blob.Kind)
	}

	// 声明为非图片时尊重声明，不嗅探
	respect := render.Classify(message.NewRawPart(png).WithMediaType("application/octet-stream"))
	if respect.Kind != render.KindFile {
		t.Fatalf("声明非图片应为 KindFile，实际 %v", respect.Kind)
	}
}

func TestClassifyToolUseAndResult(t *testing.T) {
	use := render.Classify(message.NewDataPart(map[string]any{
		"id": "call-1", "name": "read", "args": map[string]any{"path": "a.txt"},
	}).WithMediaType(render.MediaToolUse))
	if use.Kind != render.KindToolUse || use.Tool == nil {
		t.Fatalf("应为 KindToolUse，实际 %#v", use)
	}
	if use.Tool.Name != "read" || use.Tool.ID != "call-1" {
		t.Fatalf("工具字段不一致：%#v", use.Tool)
	}

	result := render.Classify(message.NewDataPart(map[string]any{
		"id": "call-1", "name": "read", "output": "ok", "is_error": false,
	}).WithMediaType(render.MediaToolResult))
	if result.Kind != render.KindToolResult || result.Tool.Output != "ok" || result.Tool.IsError {
		t.Fatalf("工具结果不一致：%#v", result)
	}

	// content 作为 result 的兼容字段
	legacy := render.Classify(message.NewDataPart(map[string]any{
		"name": "t", "content": "x",
	}).WithMediaType(render.MediaToolResult))
	if legacy.Kind != render.KindToolResult || legacy.Tool.Output != "x" {
		t.Fatalf("content 兼容字段应生效：%#v", legacy)
	}

	// name 缺失 → 降级为 JSON，不崩
	malformed := render.Classify(message.NewDataPart(map[string]any{
		"id": "call-1", "args": map[string]any{},
	}).WithMediaType(render.MediaToolUse))
	if malformed.Kind != render.KindJSON {
		t.Fatalf("缺 name 应降级 KindJSON，实际 %v", malformed.Kind)
	}

	// 普通 data → JSON
	plain := render.Classify(message.NewDataPart(map[string]any{"k": 1}))
	if plain.Kind != render.KindJSON {
		t.Fatalf("普通 data 应为 KindJSON，实际 %v", plain.Kind)
	}

	// data:null 也是合法 JSON
	nullData := render.Classify(message.NewDataPart(nil))
	if nullData.Kind != render.KindJSON || nullData.JSON != nil {
		t.Fatalf("data:null 应为 render.KindJSON 且值为 nil，实际 %#v", nullData)
	}
}

func TestSniffImage(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"png", []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}, "image/png"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg"},
		{"gif87", []byte("GIF87a...."), "image/gif"},
		{"gif89", []byte("GIF89a...."), "image/gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"unknown", []byte{1, 2, 3}, ""},
		{"empty", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := render.SniffImage(tc.data); got != tc.want {
				t.Fatalf("嗅探应为 %q，实际 %q", tc.want, got)
			}
		})
	}
}

// CJK 与 emoji 的文本不应被截断或改写（分类层只做透传）。
func TestCJKAndEmojiPassThrough(t *testing.T) {
	text := "中文 🎉 emoji"
	view := render.Classify(message.NewTextPart(text).WithMediaType(render.MediaMarkdown))
	if view.Text != text {
		t.Fatalf("文本应原样透传，实际 %q", view.Text)
	}
}

// 回归（opus）：内容为 nil 时不能返回 KindFile + nil File（UI 会空指针）。
func TestNilContentIsSafe(t *testing.T) {
	view := render.Classify(message.Part{})
	if view.Kind != render.KindJSON {
		t.Fatalf("nil 内容应安全降级为 KindJSON，实际 %v", view.Kind)
	}
	if view.File != nil || view.Image != nil {
		t.Fatal("降级结果不应带可能为 nil 的 File/Image 指针")
	}
}

// blob: 引用是本地存储，不能被当成远程图片 URL。
func TestClassifyBlobImage(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	view := render.Classify(message.Part{
		Content:   message.URLPart{URL: "blob:sha256:" + hash},
		MediaType: "image/png",
	})
	if view.Kind != render.KindImage || view.Image == nil {
		t.Fatalf("应分类为图片：%#v", view)
	}
	if view.Image.Blob != hash {
		t.Fatalf("应从引用里取出 hash：%#v", view.Image)
	}
	if view.Image.URL != "" {
		t.Fatalf("blob 引用不应带远程 URL：%#v", view.Image)
	}

	// 非法/非 blob 的 URL 仍然按远程图片处理
	for _, url := range []string{
		"blob:sha256:../etc/passwd",
		"blob:sha256:" + strings.ToUpper(hash),
		"blob:sha256:" + hash[:10],
		"https://example.com/a.png",
	} {
		view := render.Classify(message.Part{
			Content:   message.URLPart{URL: url},
			MediaType: "image/png",
		})
		if view.Kind != render.KindImage || view.Image == nil {
			t.Fatalf("%q 应仍分类为图片：%#v", url, view)
		}
		if view.Image.Blob != "" {
			t.Fatalf("%q 不该被当成 blob：%#v", url, view.Image)
		}
	}
}
