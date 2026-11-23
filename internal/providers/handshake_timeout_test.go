package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// **协议握手事件不能让严格超时生效**。
//
// 真实踩过的坑（第二次）：Anthropic 的 message_start 在响应一开头就到，
// 它带 usage 与缓存字段，但不产出内容。原来的判定是"收到任意 SSE 事件就
// 切到 90 秒的 contentTimeout"，于是：
//
//  1. message_start 到达 → 切到 90 秒
//  2. 模型还在 thinking（effort=high），一个内容块都没产出
//  3. 90 秒后被掐断，报"没有收到任何模型输出"
//
// 大上下文 + 长思考必然触发。用户看到的还是"卡 95 秒后失败"。
//
// 正确行为：只有**真正的内容事件**（文本/思考/工具调用/用量）才算
// "开始产出了"，在那之前一直用宽超时（firstEventTimeout）。
func TestHandshakeEventsDoNotTriggerContentTimeout(t *testing.T) {
	SetContentTimeoutForTest(100 * time.Millisecond)
	defer SetContentTimeoutForTest(90 * time.Second)
	SetFirstEventTimeoutForTest(3 * time.Second)
	defer SetFirstEventTimeoutForTest(300 * time.Second)

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		// 先发 message_start（Anthropic 的握手事件，带 usage 但无内容）
		fmt.Fprint(w, "event: message_start\n")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":1}}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}

		// 然后"思考"远超 contentTimeout（100ms），期间没有任何内容
		select {
		case <-release:
		case <-time.After(600 * time.Millisecond):
		}

		// 思考完了才产出内容
		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"thinking done"}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "event: message_stop\n")
		fmt.Fprint(w, `data: {"type":"message_stop"}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := postStream(ctx, streamOptions{
		URL:    server.URL,
		Parser: &anthropicParser{},
		Headers: map[string]string{
			"anthropic-version": "2023-06-01",
		},
		Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	gotText := false
	for event := range events {
		if event.Kind == EventError {
			t.Fatalf("握手后长时间思考不该被内容超时掐断：%v", event.Err)
		}
		if event.Kind == EventText {
			gotText = true
		}
	}
	if !gotText {
		t.Fatal("应正常收到思考后的文本")
	}
}