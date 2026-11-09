package providers_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
)

func userMsg(text string) message.Message {
	return message.Message{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart(text)}}
}

// P1-1：include_usage=true 但网关忽略 stream_options、不发 usage 也不发 [DONE]，
// 直接断开。stopReason 已记录，必须补发 Done —— 否则正常回答会被判成中断。
func TestOpenAIDoneOnEOFWithoutUsageOrDoneMarker(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	srv := sseServer(t, body)
	p := providers.NewOpenAIProvider(openaiConfig(srv.URL, true), "key") // include_usage = true

	events, err := p.Stream(context.Background(), providers.ChatRequest{Model: "m", Messages: []message.Message{userMsg("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	last := collected[len(collected)-1]
	if last.Kind != providers.EventDone {
		t.Fatalf("EOF 兜底应补 Done，实际最后一个事件是 %v（错误=%v）", last.Kind, last.Err)
	}
	if last.StopReason != "stop" {
		t.Fatalf("Done 应带 stop_reason，实际 %q", last.StopReason)
	}
}

// P1-1 反向：连 finish_reason 都没有就断开 → 必须报错，不能当成正常结束。
func TestOpenAIErrorWhenEOFBeforeFinishReason(t *testing.T) {
	srv := sseServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"半句\"}}]}\n\n")
	p := providers.NewOpenAIProvider(openaiConfig(srv.URL, true), "key")

	events, err := p.Stream(context.Background(), providers.ChatRequest{Model: "m", Messages: []message.Message{userMsg("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	last := collected[len(collected)-1]
	if last.Kind != providers.EventError {
		t.Fatalf("截断的流应报错，实际 %v", last.Kind)
	}
}

// Anthropic：没有 message_delta/message_stop 就 EOF，同样是失败。
func TestAnthropicErrorOnTruncatedStream(t *testing.T) {
	body := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"半句\"}}\n\n"
	srv := sseServer(t, body)
	cfg := providers.ProviderConfig{ID: "a", Kind: providers.KindAnthropic, BaseURL: srv.URL}
	p := providers.NewAnthropicProvider(cfg, "key")

	events, err := p.Stream(context.Background(), providers.ChatRequest{Model: "m", Messages: []message.Message{userMsg("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	last := collected[len(collected)-1]
	if last.Kind != providers.EventError {
		t.Fatalf("截断的 Anthropic 流应报错，实际 %v", last.Kind)
	}
}

// P1-2：mediaType 与 Content 不自洽的 part 不能让程序 panic。
// 这条路径每次发请求都会走（HistoryForRequest），历史里一条坏数据就能崩掉程序。
func TestMalformedPartsDoNotPanic(t *testing.T) {
	badParts := []message.Part{
		{Content: message.TextPart{Text: "oops"}, MediaType: render.MediaToolResult},
		{Content: message.TextPart{Text: "oops"}, MediaType: render.MediaToolUse},
		{Content: message.DataPart{Data: "not-an-object"}, MediaType: render.MediaToolResult},
		{Content: message.DataPart{Data: nil}, MediaType: render.MediaToolUse},
		{Content: message.TextPart{Text: "plain"}, MediaType: ""},
	}
	msgs := make([]message.Message, 0, len(badParts))
	for i, part := range badParts {
		msgs = append(msgs, message.Message{
			MessageID: "m" + string(rune('0'+i)),
			Role:      message.RoleAgent,
			Parts:     []message.Part{part},
		})
	}

	// 四个入口都不能 panic
	_ = providers.HistoryForRequest(msgs)
	for i := range msgs {
		providers.SettleUnfinishedToolCalls(&msgs[i], "停止")
	}
	if _, err := providers.ToOpenAIMessages(msgs, nil); err != nil {
		t.Fatalf("OpenAI 映射不应因坏 part 失败：%v", err)
	}
	if _, err := providers.ToAnthropicMessages(msgs, nil); err != nil {
		t.Fatalf("Anthropic 映射不应因坏 part 失败：%v", err)
	}
}

// P1-3：推理内容不能当成正文回传（DeepSeek 明确要求不带回 reasoning_content）。
func TestReasoningExcludedFromRequest(t *testing.T) {
	msg := message.Message{MessageID: "a1", Role: message.RoleAgent, Parts: []message.Part{
		{Content: message.TextPart{Text: "我在思考……"}, MediaType: render.MediaReasoning},
		message.NewTextPart("答案"),
	}}

	ex := providers.Extract(msg, nil)
	if ex.Text != "答案" {
		t.Fatalf("推理内容不应进请求正文，实际 %q", ex.Text)
	}

	openai, err := providers.ToOpenAIMessages([]message.Message{msg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if content, _ := openai[0]["content"].(string); content != "答案" {
		t.Fatalf("OpenAI content 不应含推理：%q", content)
	}

	anthropic, err := providers.ToAnthropicMessages([]message.Message{msg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks := anthropic[0]["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("应只有 1 个文本块（推理不发），实际 %d：%#v", len(blocks), blocks)
	}
	if text := blocks[0].(map[string]any)["text"]; text != "答案" {
		t.Fatalf("Anthropic 文本块不应含推理：%#v", blocks[0])
	}
}

// 摘要 part 必须保留（它就是压缩后的上下文）。
func TestSummaryKeptInRequest(t *testing.T) {
	msg := message.Message{MessageID: "s1", Role: message.RoleUser, Parts: []message.Part{
		{Content: message.TextPart{Text: "早前对话摘要"}, MediaType: render.MediaSummary},
	}}
	if got := providers.Extract(msg, nil).Text; got != "早前对话摘要" {
		t.Fatalf("摘要应保留，实际 %q", got)
	}
}

// 孤儿工具结果：两家协议都会拒收，必须丢弃，且请求体保持合法。
func TestOrphanToolResultDropped(t *testing.T) {
	msgs := []message.Message{
		{MessageID: "a1", Role: message.RoleAgent, Parts: []message.Part{
			toolUse("call-1", "read", map[string]any{}),
			toolResult("call-1", "read", "ok"),
			toolResult("call-orphan", "read", "没对应的调用"),
		}},
	}

	openai, err := providers.ToOpenAIMessages(msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	var toolMsgs []map[string]any
	for _, m := range openai {
		if m["role"] == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 1 || toolMsgs[0]["tool_call_id"] != "call-1" {
		t.Fatalf("孤儿结果应被丢弃，实际 %#v", toolMsgs)
	}

	anthropic, err := providers.ToAnthropicMessages(msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range anthropic {
		if m["role"] != "user" {
			continue
		}
		for _, block := range m["content"].([]any) {
			if id, ok := block.(map[string]any)["tool_use_id"]; ok && id == "call-orphan" {
				t.Fatalf("孤儿结果应被丢弃：%#v", block)
			}
		}
	}
}

// 空闲超时：响应头到了之后长时间没有数据，应报错而不是一直挂着。
func TestIdleTimeoutSurfaced(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 之后一直不发数据（超过空闲超时）
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	cfg := openaiConfig(srv.URL, false)
	cfg.IdleTimeout = 300 * time.Millisecond // 短超时，不必等默认的 180s
	p := providers.NewOpenAIProvider(cfg, "key")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, err := p.Stream(ctx, providers.ChatRequest{Model: "m", Messages: []message.Message{userMsg("hi")}})
	if err != nil {
		t.Fatal(err)
	}

	var sawError bool
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if !sawError {
					t.Fatal("空闲超时后应收到错误事件")
				}
				return
			}
			if ev.Kind == providers.EventError {
				sawError = true
				if !strings.Contains(ev.Err.Error(), "空闲") {
					t.Fatalf("错误信息应说明空闲超时，实际 %v", ev.Err)
				}
			}
		case <-deadline:
			t.Fatal("空闲超时未触发")
		}
	}
}
