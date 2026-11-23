package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 首字节慢（服务端 prefill 大上下文）不该被当成"没有输出"掐断。
//
// 真实的坑：contentTimeout（90s）原本从请求发出就计时，于是首字节前的
// 等待也算"无输出"。实测 101 条消息的会话首字节要 41 秒，网关稍慢就撞上
// 90 秒被自己掐断 —— 日志里是"卡 95 秒后失败、out=0"，看着像网关挂了。
//
// 这里把 contentTimeout 设得极短（50ms），首字节故意延迟 300ms：
// 旧逻辑会立刻失败，新逻辑应当正常拿到内容。
func TestSlowFirstByteNotKilledByContentTimeout(t *testing.T) {
	SetContentTimeoutForTest(50 * time.Millisecond)
	defer SetContentTimeoutForTest(90 * time.Second)
	SetFirstEventTimeoutForTest(2 * time.Second)
	defer SetFirstEventTimeoutForTest(300 * time.Second)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// 首字节前慢 300ms（远超 contentTimeout 的 50ms）
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := postStream(ctx, streamOptions{
		URL:     server.URL,
		Parser:  newOpenAIParser(false),
		Headers: map[string]string{},
		Body:    []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	gotText := false
	for event := range events {
		if event.Kind == EventError {
			t.Fatalf("首字节慢不该被判成失败：%v", event.Err)
		}
		if event.Kind == EventText && strings.Contains(event.Text, "hello") {
			gotText = true
		}
	}
	if !gotText {
		t.Fatal("应正常收到内容")
	}
}