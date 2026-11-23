package uiapp

import (
	"testing"
)

// token 缩写：运行中/结束时那一行不能显示 "456789 tokens"。
func TestCompactTokens(t *testing.T) {
	cases := []struct {
		tokens int
		want   string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1k"},
		{1200, "1.2k"},
		{9999, "9.9k"},
		{10000, "10k"},
		{456789, "456k"},
		{1_000_000, "1M"},
		{1_900_000, "1.9M"},
		{2_500_000, "2.5M"},
	}
	for _, c := range cases {
		if got := compactTokens(c.tokens); got != c.want {
			t.Fatalf("%d → 期望 %q，实际 %q", c.tokens, c.want, got)
		}
	}
}

// 只有"重发有意义"的失败才给重试按钮。
//
// 用户的要求：(网络失败)"应该有个可以重试的按钮，不用我手打继续了"。
// 关键是不能一律给 —— 401、上下文超限重发一万次也一样，
// 给按钮只会让人白点，还以为是程序坏了。
func TestIsRetryable(t *testing.T) {
	retryable := []string{
		"Post \"https://api.example.com\": dial tcp: i/o timeout",
		"context deadline exceeded",
		"connection refused",
		"no such host",
		"network is unreachable",
		"unexpected EOF",
		"read tcp: connection reset by peer",
		"502 Bad Gateway",
		"503 Service Unavailable",
		"504 Gateway Timeout",
		"429 Too Many Requests",
		"rate limit exceeded",
	}
	for _, reason := range retryable {
		if !isRetryable(reason) {
			t.Fatalf("应可重试：%q", reason)
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
		"",
	}
	for _, reason := range hopeless {
		if isRetryable(reason) {
			t.Fatalf("不该给重试按钮：%q", reason)
		}
	}
}