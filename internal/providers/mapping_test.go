package providers_test

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"testing"

	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
)

func toolUse(id, name string, args any) message.Part {
	return message.NewDataPart(map[string]any{"id": id, "name": name, "args": args}).
		WithMediaType(render.MediaToolUse)
}

func toolResult(id, name string, output any) message.Part {
	return message.NewDataPart(map[string]any{"id": id, "name": name, "output": output, "is_error": false}).
		WithMediaType(render.MediaToolResult)
}

// 回归：工具调用与结果必须按协议顺序拆分（assistant 在前、tool 在后）。
func TestToOpenAIMessagesSplitsToolCallAndResult(t *testing.T) {
	msgs := []message.Message{
		{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("查天气")}},
		{MessageID: "a1", Role: message.RoleAgent, Parts: []message.Part{
			message.NewTextPart("我调用工具"),
			toolUse("call-1", "weather", map[string]any{"city": "北京"}),
			toolResult("call-1", "weather", "晴 25℃"),
		}},
	}
	out, err := providers.ToOpenAIMessages(msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("应为 user + assistant(tool_calls) + tool 三条，实际 %d：%#v", len(out), out)
	}
	if out[0]["role"] != "user" {
		t.Fatalf("第 1 条应为 user：%#v", out[0])
	}
	if out[1]["role"] != "assistant" {
		t.Fatalf("第 2 条应为 assistant：%#v", out[1])
	}
	calls, ok := out[1]["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant 应带 1 个 tool_calls：%#v", out[1])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"].(string) == "" {
		t.Fatal("arguments 必须是 JSON 字符串，不能为空")
	}
	if out[2]["role"] != "tool" || out[2]["tool_call_id"] != "call-1" {
		t.Fatalf("第 3 条应为配对 tool 消息：%#v", out[2])
	}
}

func TestToAnthropicMessagesUsesBlocks(t *testing.T) {
	msgs := []message.Message{
		{MessageID: "a1", Role: message.RoleAgent, Parts: []message.Part{
			message.NewTextPart("我调用工具"),
			toolUse("call-1", "weather", map[string]any{"city": "北京"}),
			toolResult("call-1", "weather", "晴"),
		}},
	}
	out, err := providers.ToAnthropicMessages(msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	// assistant(tool_use) 后跟 user(tool_result)
	if len(out) != 2 {
		t.Fatalf("应为 2 条消息，实际 %d：%#v", len(out), out)
	}
	if out[0]["role"] != "assistant" {
		t.Fatalf("第 1 条应为 assistant：%#v", out[0])
	}
	blocks := out[0]["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "text" {
		t.Fatalf("首块应为 text：%#v", blocks)
	}
	toolBlock := blocks[1].(map[string]any)
	if toolBlock["type"] != "tool_use" || toolBlock["id"] != "call-1" {
		t.Fatalf("tool_use 块不一致：%#v", toolBlock)
	}
	if _, ok := toolBlock["input"].(map[string]any); !ok {
		t.Fatalf("Anthropic 用 input 承载参数对象：%#v", toolBlock)
	}
	if out[1]["role"] != "user" {
		t.Fatalf("tool_result 应用 user 消息：%#v", out[1])
	}
	resBlock := out[1]["content"].([]any)[0].(map[string]any)
	if resBlock["type"] != "tool_result" || resBlock["tool_use_id"] != "call-1" {
		t.Fatalf("tool_result 块不一致：%#v", resBlock)
	}
}

// args 缺失/null 归一成 {}（否则 OpenAI 收到 "null"、Anthropic input 非 object 会 400）。
func TestExtractNormalizesMissingArgs(t *testing.T) {
	part := message.NewDataPart(map[string]any{"id": "c1", "name": "noargs"}).
		WithMediaType(render.MediaToolUse)
	ex := providers.Extract(message.Message{MessageID: "m", Role: message.RoleAgent, Parts: []message.Part{part}}, nil)
	if len(ex.ToolCalls) != 1 {
		t.Fatalf("应提取 1 个调用，实际 %d", len(ex.ToolCalls))
	}
	if _, ok := ex.ToolCalls[0].Args.(map[string]any); !ok {
		t.Fatalf("args 应归一成 object，实际 %#v", ex.ToolCalls[0].Args)
	}
	if ex.ToolCalls[0].RawArgs != "{}" {
		t.Fatalf("rawArgs 应为 {}，实际 %q", ex.ToolCalls[0].RawArgs)
	}
}

// 双重编码的 args（字符串里套 JSON）应被解开。
func TestExtractDecodesDoubleEncodedArgs(t *testing.T) {
	part := message.NewDataPart(map[string]any{
		"id": "c1", "name": "f", "args": `{"k":"v"}`,
	}).WithMediaType(render.MediaToolUse)
	ex := providers.Extract(message.Message{MessageID: "m", Role: message.RoleAgent, Parts: []message.Part{part}}, nil)
	args, ok := ex.ToolCalls[0].Args.(map[string]any)
	if !ok || args["k"] != "v" {
		t.Fatalf("双重编码应解开，实际 %#v", ex.ToolCalls[0].Args)
	}
}

func TestSettleUnfinishedToolCalls(t *testing.T) {
	// 有 tool_use 但没有结果 → 补一条错误结果
	msg := message.Message{MessageID: "m1", Role: message.RoleAgent, Parts: []message.Part{
		toolUse("c1", "read", map[string]any{}),
	}}
	if !providers.SettleUnfinishedToolCalls(&msg, "用户已停止") {
		t.Fatal("应报告已修改")
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("应补一条结果，实际 %d 个 part", len(msg.Parts))
	}
	data := msg.Parts[1].Content.(message.DataPart).Data.(map[string]any)
	if data["is_error"] != true || data["output"] != "用户已停止" {
		t.Fatalf("补写结果不一致：%#v", data)
	}

	// 已有正式结果 → 不再改动
	msg2 := message.Message{MessageID: "m2", Role: message.RoleAgent, Parts: []message.Part{
		toolUse("c1", "read", map[string]any{}),
		toolResult("c1", "read", "ok"),
	}}
	if providers.SettleUnfinishedToolCalls(&msg2, "x") {
		t.Fatal("已有正式结果时不应修改")
	}

	// pending 占位 → 被覆盖（不新增）
	pendingResult := message.NewDataPart(map[string]any{"id": "c1", "name": "ask", "pending": true}).
		WithMediaType(render.MediaToolResult)
	msg3 := message.Message{MessageID: "m3", Role: message.RoleAgent, Parts: []message.Part{
		toolUse("c1", "ask", map[string]any{}),
		pendingResult,
	}}
	if !providers.SettleUnfinishedToolCalls(&msg3, "已停止") {
		t.Fatal("pending 占位应被覆盖")
	}
	if len(msg3.Parts) != 2 {
		t.Fatalf("覆盖而不是新增，实际 %d 个 part", len(msg3.Parts))
	}
	if providers.IsPendingResult(msg3.Parts[1]) {
		t.Fatal("覆盖后不应再是 pending")
	}
}

func TestHistoryForRequestSkipsPending(t *testing.T) {
	pendingResult := message.NewDataPart(map[string]any{"id": "c1", "name": "ask", "pending": true}).
		WithMediaType(render.MediaToolResult)
	history := []message.Message{
		{MessageID: "m1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}},
		{MessageID: "m2", Role: message.RoleAgent, Parts: []message.Part{toolUse("c1", "ask", map[string]any{}), pendingResult}},
	}
	out := providers.HistoryForRequest(history)
	if len(out) != 1 || out[0].MessageID != "m1" {
		t.Fatalf("含 pending 的消息应被过滤，实际 %#v", out)
	}
}

// 生成的 messages 必须能被标准 JSON 编码（不含不可序列化的值）。
func TestMessagesAreJSONEncodable(t *testing.T) {
	msgs := []message.Message{
		{MessageID: "a1", Role: message.RoleAgent, Parts: []message.Part{
			message.NewTextPart("hi"),
			toolUse("c1", "f", map[string]any{"k": 1}),
			toolResult("c1", "f", map[string]any{"ok": true}),
		}},
	}
	openai, err := providers.ToOpenAIMessages(msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	anthropic, err := providers.ToAnthropicMessages(msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(openai); err != nil {
		t.Fatalf("OpenAI messages 应可序列化：%v", err)
	}
	if _, err := json.Marshal(anthropic); err != nil {
		t.Fatalf("Anthropic messages 应可序列化：%v", err)
	}
	_ = jsontext.Value(nil)
}

// 图片 part 会以多模态 content 发出去；blob 丢失时降级成文字。
func TestImagePartsBecomeMultimodalContent(t *testing.T) {
	blob := []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4}
	hash := strings.Repeat("ab", 32)
	readBlob := func(got string) ([]byte, error) {
		if got != hash {
			return nil, fmt.Errorf("unexpected hash %q", got)
		}
		return blob, nil
	}
	msg := message.Message{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{
		message.NewTextPart("看图"),
		message.NewURLPart("blob:sha256:" + hash).WithMediaType("image/png"),
	}}

	openai, err := providers.ToOpenAIMessages([]message.Message{msg}, readBlob)
	if err != nil {
		t.Fatal(err)
	}
	content, ok := openai[0]["content"].([]any)
	if !ok {
		t.Fatalf("有图片时 content 应是数组：%#v", openai[0]["content"])
	}
	if len(content) != 2 {
		t.Fatalf("应有一段文字 + 一张图片：%#v", content)
	}
	image := content[1].(map[string]any)
	url := image["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("图片应编码成 data URL：%q", url)
	}

	anthropic, err := providers.ToAnthropicMessages([]message.Message{msg}, readBlob)
	if err != nil {
		t.Fatal(err)
	}
	blocks := anthropic[0]["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("Anthropic 应有文字块 + 图片块：%#v", blocks)
	}
	source := blocks[1].(map[string]any)["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" {
		t.Fatalf("Anthropic 图片块格式不对：%#v", source)
	}
	if source["data"] == "" {
		t.Fatal("Anthropic 图片数据不应为空")
	}

	// blob 读不出来（被删/损坏）→ 文字降级，不能报错、也不能丢整条消息
	missing := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	openai, err = providers.ToOpenAIMessages([]message.Message{msg}, missing)
	if err != nil {
		t.Fatal(err)
	}
	content = openai[0]["content"].([]any)
	last := content[len(content)-1].(map[string]any)
	if last["type"] != "text" || last["text"] != "[图片丢失]" {
		t.Fatalf("缺失的图片应降级成文字：%#v", last)
	}
	anthropic, err = providers.ToAnthropicMessages([]message.Message{msg}, missing)
	if err != nil {
		t.Fatal(err)
	}
	blocks = anthropic[0]["content"].([]any)
	last = blocks[len(blocks)-1].(map[string]any)
	if last["type"] != "text" || last["text"] != "[图片丢失]" {
		t.Fatalf("Anthropic 缺失的图片也应降级：%#v", last)
	}
}

// 内嵌字节（RawPart）也参与请求。
func TestRawImagePartBecomesMultimodal(t *testing.T) {
	raw := []byte{0x89, 'P', 'N', 'G', 9}
	msg := message.Message{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{
		message.NewRawPart(raw).WithMediaType("image/png"),
	}}
	openai, err := providers.ToOpenAIMessages([]message.Message{msg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	content, ok := openai[0]["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("内嵌图片应变成多模态 content：%#v", openai[0])
	}
}

// read_image 的工具结果带图片：**OpenAI 的 tool 消息只能放文本**，图片要收集起来
// 在这批 tool 消息之后追加一条 user 消息（image_url）；Anthropic 的 tool_result
// 块数组本身可以带 image，就地放。
func TestToolResultImagesMapping(t *testing.T) {
	blob := []byte{0x89, 'P', 'N', 'G', 9, 9}
	hash := strings.Repeat("cd", 32)
	readBlob := func(got string) ([]byte, error) {
		if got != hash {
			return nil, fmt.Errorf("unexpected hash %q", got)
		}
		return blob, nil
	}
	resultImage := func(callID string) message.Part {
		return message.NewDataPart(map[string]any{
			"id": callID, "name": "read_image", "output": "<type>image</type>", "is_error": false,
			"images": []any{map[string]any{
				"ref": "blob:sha256:" + hash, "media_type": "image/png", "width": 2, "height": 3,
			}},
		}).WithMediaType(render.MediaToolResult)
	}
	call := message.NewDataPart(map[string]any{
		"id": "c1", "name": "read_image", "args": map[string]any{"file_path": "a.png"},
	}).WithMediaType(render.MediaToolUse)

	msgs := []message.Message{
		{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("看图")}},
		// 工具调用与结果在**同一条** agent 消息里（driver 就是这么追加的）
		{MessageID: "a1", Role: message.RoleAgent, Parts: []message.Part{call, resultImage("c1")}},
	}

	openai, err := providers.ToOpenAIMessages(msgs, readBlob)
	if err != nil {
		t.Fatal(err)
	}
	// user + assistant(tool_calls) + tool + user(图片)
	if len(openai) != 4 {
		t.Fatalf("应是 user + assistant + tool + user(图片)：%#v", openai)
	}
	toolMsg := openai[2]
	if toolMsg["role"] != "tool" {
		t.Fatalf("第二条应是 tool 消息：%#v", toolMsg)
	}
	// tool 消息的 content 必须是**纯文本**（不能是数组）
	if _, isArray := toolMsg["content"].([]any); isArray {
		t.Fatalf("tool 消息 content 不能是内容数组：%#v", toolMsg["content"])
	}
	imageMsg := openai[3]
	if imageMsg["role"] != "user" {
		t.Fatalf("图片应放在 tool 之后的 user 消息里：%#v", imageMsg)
	}
	parts := imageMsg["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("应是引导文字 + 一张图：%#v", parts)
	}
	if parts[0].(map[string]any)["type"] != "text" {
		t.Fatalf("第一段应是引导文字：%#v", parts[0])
	}
	url := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("图片应编码成 data URL：%q", url)
	}

	// Anthropic：tool_result 的 content 本身就是块数组，图片就地放
	anthropic, err := providers.ToAnthropicMessages(msgs, readBlob)
	if err != nil {
		t.Fatal(err)
	}
	resultMsg := anthropic[len(anthropic)-1]
	blocks := resultMsg["content"].([]any)
	block := blocks[0].(map[string]any)
	if block["type"] != "tool_result" {
		t.Fatalf("应是 tool_result 块：%#v", block)
	}
	content, ok := block["content"].([]any)
	if !ok {
		t.Fatalf("带图片的 tool_result content 应是块数组：%#v", block["content"])
	}
	if len(content) != 2 || content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("应有一段文字 + 一个图片块：%#v", content)
	}

	// blob 丢失 → 「[图片丢失]」文字，不报错
	missing := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	openaiMissing, err := providers.ToOpenAIMessages(msgs, missing)
	if err != nil {
		t.Fatal(err)
	}
	missingParts := openaiMissing[len(openaiMissing)-1]["content"].([]any)
	if missingParts[1].(map[string]any)["type"] != "text" {
		t.Fatalf("丢失的图片应降级成文字：%#v", missingParts)
	}
}

// 一次读多张图：所有图片合并进**同一条** user 消息。
func TestToolResultImagesMergeIntoOneUserMessage(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	readBlob := func(string) ([]byte, error) { return []byte{1, 2, 3}, nil }
	imagePart := func(callID string) message.Part {
		return message.NewDataPart(map[string]any{
			"id": callID, "name": "read_image", "output": "<type>image</type>", "is_error": false,
			"images": []any{map[string]any{
				"ref": "blob:sha256:" + hash, "media_type": "image/png", "width": 1, "height": 1,
			}},
		}).WithMediaType(render.MediaToolResult)
	}
	callPart := func(callID string) message.Part {
		return message.NewDataPart(map[string]any{
			"id": callID, "name": "read_image", "args": map[string]any{"file_path": callID + ".png"},
		}).WithMediaType(render.MediaToolUse)
	}
	msgs := []message.Message{
		{MessageID: "a1", Role: message.RoleAgent, Parts: []message.Part{
			callPart("c1"), callPart("c2"), imagePart("c1"), imagePart("c2"),
		}},
	}
	out, err := providers.ToOpenAIMessages(msgs, readBlob)
	if err != nil {
		t.Fatal(err)
	}
	// assistant(calls) + tool + tool + user(2 张图)
	if len(out) != 4 {
		t.Fatalf("应是 assistant + 2×tool + 1×user：%#v", out)
	}
	if out[1]["role"] != "tool" || out[2]["role"] != "tool" {
		t.Fatalf("中间应是两条 tool 消息：%#v", out)
	}
	parts := out[3]["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("应是引导文字 + 两张图（合并成一条）：%#v", parts)
	}
}
