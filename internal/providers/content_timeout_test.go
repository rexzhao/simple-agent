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

// 网关只发心跳注释、一直不给真正的 chunk：必须按超时中止，
// 不能因为字节不断而永远挂着（用户看到"停止按钮还在、没有输出"）。
//
// 注意这里走的是**首字节超时**：心跳行不是"事件"，所以在收到第一个
// 真正的事件之前，计时器用的是 firstEventTimeout（比 contentTimeout 宽）。
// 两种超时都必须能中止 —— 区别只是上限与报错文案。
func TestStreamContentTimeoutWithHeartbeats(t *testing.T) {
	SetFirstEventTimeoutForTest(150 * time.Millisecond)
	defer SetFirstEventTimeoutForTest(300 * time.Second)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("通道关闭但没有错误事件")
			}
			if event.Kind == EventError {
				if event.Err == nil || !strings.Contains(event.Err.Error(), "仍未产出任何内容") {
					t.Fatalf("错误原因不对：%v", event.Err)
				}
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("只有心跳时没有按超时中止")
}
