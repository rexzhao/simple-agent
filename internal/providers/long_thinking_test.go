package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 思考期本身可以很长：`content_block_start` 之后到第一个 delta 之间，
// 实测这个网关要 **78～84 秒**（effort=high + 大上下文）。
//
// contentTimeout 原来是 90s —— 几乎没有余量，一次稍慢的思考就被掐断，
// 用户看到的症状是"每轮都超时"、"pi 不超时但我们超时"。
//
// 这里把 contentTimeout 调到 100ms 来模拟"阈值太紧"的旧配置：
// 思考 300ms 就该被掐断；而按现在的默认值（180s）不会。
func TestLongThinkingNotKilledByTightContentTimeout(t *testing.T) {
	SetContentTimeoutForTest(100 * time.Millisecond)
	defer SetContentTimeoutForTest(180 * time.Second)
	SetFirstEventTimeoutForTest(5 * time.Second)
	defer SetFirstEventTimeoutForTest(300 * time.Second)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		// 握手：content_block_start 立刻到达（但它不是"内容"）
		fmt.Fprint(w, "event: message_start\n")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n")
		fmt.Fprint(w, "event: content_block_start\n")
		fmt.Fprint(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}

		// 思考 300ms（远超 100ms 的 contentTimeout，但在真实的 180s 内）
		time.Sleep(300 * time.Millisecond)

		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}`+"\n\n")
		fmt.Fprint(w, "event: message_stop\n")
		fmt.Fprint(w, `data: {"type":"message_stop"}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := postStream(ctx, streamOptions{
		URL:     server.URL,
		Parser:  &anthropicParser{},
		Headers: map[string]string{"anthropic-version": "2023-06-01"},
		Body:    []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	gotText := false
	for event := range events {
		if event.Kind == EventError {
			t.Fatalf("content_block_start 之后的长思考不该被内容超时掐断：%v", event.Err)
		}
		if event.Kind == EventText {
			gotText = true
		}
	}
	if !gotText {
		t.Fatal("应收到最终文本")
	}
}

// 默认值必须有足够余量：实测思考期能到 84 秒。
func TestContentTimeoutHasHeadroomForRealThinking(t *testing.T) {
	// 84 秒是实测值；留一倍余量。
	const realThinking = 90 * time.Second
	if contentTimeout < 2*realThinking {
		t.Fatalf("contentTimeout=%s 余量不足（实测思考期可达 84 秒，偶尔更长）", contentTimeout)
	}
}