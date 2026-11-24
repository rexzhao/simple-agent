package driver_test

import (
	"testing"

	"simple-agent/internal/driver"
)

// 哪些失败值得自动重试。
//
// 自动重试的价值（实测）：这个网关波动极大 —— 同一会话响应头 6s～95s、
// 思考期 78～84s、偶发断流。pi 有退避重试，我们原来只能手动点。
//
// 但**不能一律重试**：鉴权/额度/上下文超限重发一万次也一样，
// 自动重试只会让用户白等两轮（每轮可能几十秒）。
func TestRetryableFailure(t *testing.T) {
	retryable := []string{
		`network: Post "https://x": dial tcp: i/o timeout`,
		"timeout: 超过 3m0s 模型仍未产出任何内容，已中止",
		"connection refused",
		"no such host",
		"unexpected EOF",
		"read tcp: connection reset by peer",
		"502 Bad Gateway",
		"503 Service Unavailable",
		"504 Gateway Timeout",
		"429 Too Many Requests",
		// 断流：StreamClosed 用的就是这个文本
		"连接意外中断（未收到结束标记）",
		"stream: read tcp: broken pipe",
	}
	for _, reason := range retryable {
		if !driver.RetryableFailureForTest(reason) {
			t.Fatalf("应可自动重试：%q", reason)
		}
	}

	hopeless := []string{
		"401 Unauthorized",
		"invalid api key",
		"403 Forbidden",
		"402 insufficient balance",
		"context_length_exceeded",
		"prompt is too long",
		"max_tokens: 128000",
		"400 Bad Request",
		"404 Not Found",
		// 请求构造错误：重发还是同样的请求，必然再错
		"This model does not support assistant message prefill",
		"",
	}
	for _, reason := range hopeless {
		if driver.RetryableFailureForTest(reason) {
			t.Fatalf("不该自动重试：%q", reason)
		}
	}
}
