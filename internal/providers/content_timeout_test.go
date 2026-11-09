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

// 网关只发心跳注释、一直不给真正的 chunk：必须按"无输出"超时中止，
// 不能因为字节不断而永远挂着（用户看到"停止按钮还在、没有输出"）。
func TestStreamContentTimeoutWithHeartbeats(t *testing.T) {
	SetContentTimeoutForTest(150 * time.Millisecond)
	defer SetContentTimeoutForTest(90 * time.Second)

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
				if event.Err == nil || !strings.Contains(event.Err.Error(), "没有收到任何模型输出") {
					t.Fatalf("错误原因不对：%v", event.Err)
				}
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("只有心跳时没有按无输出超时中止")
}
