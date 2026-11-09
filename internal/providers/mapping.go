package providers

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	"simple-agent/internal/message"
	"simple-agent/internal/render"
)

// ExtractedToolCall 是从消息里提取出的工具调用。
type ExtractedToolCall struct {
	ID      string
	Name    string
	Args    any
	RawArgs string
}

// ExtractedToolResult 是从消息里提取出的工具结果。
type ExtractedToolResult struct {
	ID      string
	Name    string
	Output  any
	IsError bool
	// Images 是工具返回的图片（read_image），已从 blob 存储读出。
	Images []ExtractedImage
}

// ExtractedImage 是请求里的一张图片。
type ExtractedImage struct {
	// MediaType 是规范化后的图片 MIME（image/png 等）。
	MediaType string
	// Bytes 是图片字节（已从 blob 存储读出，见 BlobReader）。
	Bytes []byte
	// Missing 为真表示引用在本地已经找不到（blob 被删/损坏）。
	// 请求里会用文字「[图片丢失]」代替，不能让整个会话发不出去。
	Missing bool
}

// Extracted 是一条消息的语义内容。
type Extracted struct {
	Role        message.Role
	Text        string
	Images      []ExtractedImage
	ToolCalls   []ExtractedToolCall
	ToolResults []ExtractedToolResult
}

// BlobReader 把 `blob:sha256:<hex>` 引用读成字节。
//
// **必须在异步开流的 goroutine 里调用**（请求在那里组装），不能在主线程上读
// 几 MB 的图片，否则界面会卡。读不到就返回错误，由调用方降级成「[图片丢失]」。
type BlobReader func(hash string) ([]byte, error)

// Extract 提取一条消息的文本、图片、工具调用与工具结果。
func Extract(msg message.Message, blobs BlobReader) Extracted {
	out := Extracted{Role: msg.Role}
	var texts []string

	for _, part := range msg.Parts {
		switch content := part.Content.(type) {
		case message.URLPart:
			// 只把**本地 blob 引用**的图片发出去：远程 URL 图片不参与请求
			// （v1 不代用户上传外链，避免隐私与超时问题）。
			view := render.Classify(part)
			if view.Kind != render.KindImage || view.Image == nil {
				continue
			}
			image := ExtractedImage{MediaType: view.Image.MediaType}
			hash, ok := render.BlobHash(content.URL)
			if !ok || blobs == nil {
				image.Missing = true
			} else if data, err := blobs(hash); err != nil {
				image.Missing = true
			} else {
				image.Bytes = data
			}
			out.Images = append(out.Images, image)
		case message.RawPart:
			// 内嵌字节（没有走 blob 的旧数据/内存构造的消息）
			view := render.Classify(part)
			if view.Kind != render.KindImage || view.Image == nil {
				continue
			}
			out.Images = append(out.Images, ExtractedImage{
				MediaType: view.Image.MediaType,
				Bytes:     content.Raw,
			})
		case message.TextPart:
			// 推理内容（CoT）不进请求：DeepSeek 明确说过之前轮次的
			// reasoning_content 不应回传，否则既浪费 token 又会改变模型行为。
			// 摘要 part 必须保留（它就是压缩后的上下文）。
			if isMedia(part.MediaType, render.MediaReasoning) {
				continue
			}
			texts = append(texts, content.Text)
		case message.DataPart:
			data, ok := content.Data.(map[string]any)
			if !ok {
				continue
			}
			switch {
			case isMedia(part.MediaType, render.MediaToolUse):
				call := ExtractedToolCall{
					ID:   stringField(data, "id"),
					Name: stringField(data, "name"),
				}
				if args, ok := data["args"]; ok {
					call.Args = args
					call.RawArgs = marshalCompact(args)
				} else {
					// args 缺失或 null 一律按空对象处理：OpenAI 会把 "null"
					// 当 arguments 发给模型，Anthropic 的 input 必须是 object
					call.Args = map[string]any{}
					call.RawArgs = "{}"
				}
				// 双重编码的字符串（部分兼容实现会这么做）尝试解开
				if s, ok := call.Args.(string); ok {
					if decoded := tryDecodeObject(s); decoded != nil {
						call.Args = decoded
						call.RawArgs = marshalCompact(decoded)
					}
				}
				out.ToolCalls = append(out.ToolCalls, call)
			case isMedia(part.MediaType, render.MediaToolResult):
				output, ok := data["output"]
				if !ok {
					output = data["content"] // 兼容别名只对 result 生效
				}
				out.ToolResults = append(out.ToolResults, ExtractedToolResult{
					ID:      stringField(data, "id"),
					Name:    stringField(data, "name"),
					Output:  output,
					IsError: boolField(data, "is_error"),
					Images:  extractResultImages(data, blobs),
				})
			}
		}
	}

	out.Text = joinNonEmpty(texts, "\n")
	return out
}

// extractResultImages 读出工具结果里挂的图片（`images: [{ref, media_type}]`）。
//
// 读不到（blob 被删/损坏）不报错：结果里保留一条 Missing 的图片，请求组装时
// 换成「[图片丢失]」文字，整个会话不会因为一张图片发不出去。
func extractResultImages(data map[string]any, blobs BlobReader) []ExtractedImage {
	list, _ := data["images"].([]any)
	if len(list) == 0 {
		return nil
	}
	images := make([]ExtractedImage, 0, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		image := ExtractedImage{MediaType: stringField(entry, "media_type")}
		ref := stringField(entry, "ref")
		bytes, err := readBlobRef(ref, blobs)
		if err != nil || len(bytes) == 0 {
			image.Missing = true
		} else {
			image.Bytes = bytes
		}
		images = append(images, image)
	}
	return images
}

// readBlobRef 把 `blob:sha256:<hex>` 引用读成字节。
func readBlobRef(ref string, blobs BlobReader) ([]byte, error) {
	if blobs == nil {
		return nil, fmt.Errorf("没有 blob 存储")
	}
	hash, ok := strings.CutPrefix(ref, "blob:sha256:")
	if !ok || hash == "" {
		return nil, fmt.Errorf("无法识别的图片引用 %q", ref)
	}
	return blobs(hash)
}

// openAIContent 构造多模态 content 数组（文本 + 图片）。
func openAIContent(ex Extracted) []any {
	content := make([]any, 0, len(ex.Images)+1)
	if ex.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": ex.Text})
	}
	for _, image := range ex.Images {
		if image.Missing || len(image.Bytes) == 0 {
			content = append(content, map[string]any{"type": "text", "text": missingImageText})
			continue
		}
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": dataURL(image.MediaType, image.Bytes),
			},
		})
	}
	return content
}

// attachedToolImageText 是"工具结果里的图片"那条 user 消息的引导文字
// （与 pi-ai 的 openai-completions 适配器同一句）。
const attachedToolImageText = "Attached image(s) from tool result:"

// missingImageText 是 blob 丢失时替换进请求的文字。
const missingImageText = "[图片丢失]"

// dataURL 把字节编码成 data URL（OpenAI 兼容实现都接受这种形式）。
func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// ToOpenAIMessages 构造 OpenAI Chat Completions 的 messages。
//
// 顺序约束：assistant(tool_calls) 在前，role:"tool" 紧随其后。
func ToOpenAIMessages(msgs []message.Message, blobs BlobReader) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(msgs))

	for _, msg := range msgs {
		ex := Extract(msg, blobs)
		role := "user"
		if ex.Role == message.RoleAgent {
			role = "assistant"
		}

		if ex.Text != "" || len(ex.ToolCalls) > 0 || len(ex.Images) > 0 {
			entry := map[string]any{"role": role}
			switch {
			case len(ex.Images) > 0:
				// 有图片时 content 必须是数组（OpenAI 多模态格式）
				entry["content"] = openAIContent(ex)
			case ex.Text != "":
				// 纯文本仍用字符串：不改动既有行为
				entry["content"] = ex.Text
			}
			if len(ex.ToolCalls) > 0 {
				calls := make([]any, 0, len(ex.ToolCalls))
				for _, call := range ex.ToolCalls {
					raw := call.RawArgs
					if raw == "" {
						raw = "{}"
					}
					calls = append(calls, map[string]any{
						"id":   call.ID,
						"type": "function",
						"function": map[string]any{
							"name":      call.Name,
							"arguments": raw,
						},
					})
				}
				entry["tool_calls"] = calls
			}
			out = append(out, entry)
		}

		// OpenAI 的 tool 消息 content **只能**是字符串或纯文本内容数组：
		// image_url 只在 user/assistant 消息里允许。工具返回的图片因此不能塞进
		// tool 消息（官方会 400，兼容网关会拒绝或静默丢图），而是收集起来，在这批
		// tool 消息**之后**追加一条带 image_url 的 user 消息——与 dsh 使用的
		// pi-ai 适配器（openai-completions.js）做法一致。
		var resultImages []ExtractedImage
		results := pairedResults(msg.MessageID, ex)
		for _, result := range results {
			text := contentToText(result.Output)
			if text == "" && len(result.Images) > 0 {
				// 只有图片、没有文本：给个占位，空 content 会被拒
				text = "(see attached image)"
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": result.ID,
				"content":      text,
			})
			resultImages = append(resultImages, result.Images...)
		}
		if len(resultImages) > 0 {
			parts := []any{map[string]any{"type": "text", "text": attachedToolImageText}}
			for _, image := range resultImages {
				if image.Missing || len(image.Bytes) == 0 {
					parts = append(parts, map[string]any{"type": "text", "text": missingImageText})
					continue
				}
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": "data:" + image.MediaType + ";base64," +
							base64.StdEncoding.EncodeToString(image.Bytes),
					},
				})
			}
			// TODO(兼容): pi-ai 还有个 requiresAssistantAfterToolResult 开关，会在
			// 这条 user 消息前插一句 assistant 以满足个别网关的顺序要求。目前
			// paperhub / OpenAI 都不需要，先不实现。
			out = append(out, map[string]any{"role": "user", "content": parts})
		}
	}
	return out, nil
}

// ToAnthropicMessages 构造 Anthropic Messages 的 messages（内容块形式）。
//
// 顺序约束：tool_use 在前，tool_result 用 user 消息跟随。
func ToAnthropicMessages(msgs []message.Message, blobs BlobReader) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(msgs))

	for _, msg := range msgs {
		ex := Extract(msg, blobs)

		if ex.Text != "" || len(ex.ToolCalls) > 0 || len(ex.Images) > 0 {
			blocks := make([]any, 0, len(ex.ToolCalls)+len(ex.Images)+1)
			if ex.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": ex.Text})
			}
			for _, image := range ex.Images {
				if image.Missing || len(image.Bytes) == 0 {
					blocks = append(blocks, map[string]any{"type": "text", "text": missingImageText})
					continue
				}
				blocks = append(blocks, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": image.MediaType,
						"data":       base64.StdEncoding.EncodeToString(image.Bytes),
					},
				})
			}
			for _, call := range ex.ToolCalls {
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    call.ID,
					"name":  call.Name,
					"input": call.Args,
				})
			}
			role := "user"
			if ex.Role == message.RoleAgent {
				role = "assistant"
			}
			out = append(out, map[string]any{"role": role, "content": blocks})
		}

		if results := pairedResults(msg.MessageID, ex); len(results) > 0 {
			blocks := make([]any, 0, len(results))
			for _, result := range results {
				block := map[string]any{
					"type":        "tool_result",
					"tool_use_id": result.ID,
					"content":     contentToText(result.Output),
					"is_error":    result.IsError,
				}
				if len(result.Images) > 0 {
					content := []any{map[string]any{"type": "text", "text": contentToText(result.Output)}}
					for _, image := range result.Images {
						if image.Missing || len(image.Bytes) == 0 {
							content = append(content, map[string]any{"type": "text", "text": missingImageText})
							continue
						}
						content = append(content, map[string]any{
							"type": "image",
							"source": map[string]any{
								"type":       "base64",
								"media_type": image.MediaType,
								"data":       base64.StdEncoding.EncodeToString(image.Bytes),
							},
						})
					}
					block["content"] = content
				}
				blocks = append(blocks, block)
			}
			out = append(out, map[string]any{"role": "user", "content": blocks})
		}
	}
	return out, nil
}

// pairedResults 只返回能在本条消息里找到对应 tool_use 的结果。
//
// 孤儿结果（数据损坏或对端消息异常）会被丢弃并记日志：两家协议都要求 tool_result
// 必须紧跟对应的 tool_use，带着孤儿结果发请求会被直接拒收。
// 反向的「有调用没结果」由 SettleUnfinishedToolCalls 负责补齐，映射层不编造结果。
func pairedResults(messageID string, ex Extracted) []ExtractedToolResult {
	if len(ex.ToolResults) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(ex.ToolCalls))
	for _, call := range ex.ToolCalls {
		known[call.ID] = struct{}{}
	}
	kept := make([]ExtractedToolResult, 0, len(ex.ToolResults))
	for _, result := range ex.ToolResults {
		if _, ok := known[result.ID]; !ok {
			slog.Warn("丢弃孤儿工具结果（没有对应的 tool_use）",
				"messageId", messageID, "toolCallId", result.ID, "name", result.Name)
			continue
		}
		kept = append(kept, result)
	}
	return kept
}

// SettleUnfinishedToolCalls 给消息里每个没有正式结果的 tool_use 补一条结果。
//
// 两处共用：用户停止生成、以及重放时发现「执行工具中崩溃」。
// pending 占位结果会被覆盖。返回是否做了修改。
func SettleUnfinishedToolCalls(msg *message.Message, reason string) bool {
	type call struct{ id, name string }
	var calls []call

	for _, part := range msg.Parts {
		if !isMedia(part.MediaType, render.MediaToolUse) {
			continue
		}
		data, ok := dataOf(part)
		if !ok {
			continue
		}
		calls = append(calls, call{id: stringField(data, "id"), name: stringField(data, "name")})
	}
	if len(calls) == 0 {
		return false
	}

	changed := false
	for _, c := range calls {
		var settled bool
		pendingSlot := -1
		for i, part := range msg.Parts {
			if !isMedia(part.MediaType, render.MediaToolResult) {
				continue
			}
			data, ok := dataOf(part)
			if !ok || stringField(data, "id") != c.id {
				continue
			}
			if isPending(data) {
				pendingSlot = i
			} else {
				settled = true
			}
		}
		if settled {
			continue
		}

		synthetic := message.Part{
			Content: message.DataPart{Data: map[string]any{
				"id":       c.id,
				"name":     c.name,
				"output":   reason,
				"is_error": true,
			}},
			MediaType: render.MediaToolResult,
		}
		if pendingSlot >= 0 {
			msg.Parts[pendingSlot] = synthetic
		} else {
			msg.Parts = append(msg.Parts, synthetic)
		}
		changed = true
	}
	return changed
}

// IsPendingResult 判断工具结果是否仍是「等待用户回答」的占位。
func IsPendingResult(part message.Part) bool {
	if !isMedia(part.MediaType, render.MediaToolResult) {
		return false
	}
	data, ok := dataOf(part)
	return ok && isPending(data)
}

// HistoryForRequest 过滤出可发给模型的消息：
// 只取正常完成、且不含 pending 占位结果的消息。
func HistoryForRequest(history []message.Message) []message.Message {
	out := make([]message.Message, 0, len(history))
	for _, msg := range history {
		pending := false
		for _, part := range msg.Parts {
			if IsPendingResult(part) {
				pending = true
				break
			}
		}
		if !pending {
			out = append(out, msg)
		}
	}
	return out
}

// dataOf 取 part 的 data 对象。
//
// 一律用两值形式：history 可能来自日志或对端消息，mediaType 与 Content 不一定自洽
// （例如 mediaType=tool-result 但内容是文本）。单值断言会 panic，而这条路径每次
// 发请求都要走。
func dataOf(part message.Part) (map[string]any, bool) {
	dataPart, ok := part.Content.(message.DataPart)
	if !ok {
		return nil, false
	}
	data, ok := dataPart.Data.(map[string]any)
	return data, ok
}

func isMedia(actual, want string) bool {
	if actual == "" {
		return false
	}
	essence := actual
	if idx := indexByte(actual, ';'); idx >= 0 {
		essence = actual[:idx]
	}
	return equalFoldTrim(essence, want)
}

func isPending(data map[string]any) bool {
	v, ok := data["pending"].(bool)
	return ok && v
}

func stringField(data map[string]any, key string) string {
	v, _ := data[key].(string)
	return v
}

func boolField(data map[string]any, key string) bool {
	v, _ := data[key].(bool)
	return v
}

func contentToText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return marshalCompact(v)
	}
}

func joinNonEmpty(parts []string, sep string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

func tryDecodeObject(s string) map[string]any {
	var decoded map[string]any
	if err := unmarshalJSON([]byte(s), &decoded); err != nil {
		return nil
	}
	return decoded
}

func requireNonEmptyMessages(msgs []map[string]any) error {
	if len(msgs) == 0 {
		return fmt.Errorf("没有可发送的消息")
	}
	return nil
}
