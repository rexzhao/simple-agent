package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"simple-agent/internal/web"
)

// newServer 起一个测试服务器，返回带默认客户端的 Fetcher。
func newServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *web.Fetcher) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, &web.Fetcher{Client: server.Client()}
}

// codeOf 取错误分类（断言用）。
func codeOf(t *testing.T, err error) string {
	t.Helper()
	var failure *web.Error
	if !errors.As(err, &failure) {
		t.Fatalf("错误类型不是 *web.Error：%T %v", err, err)
	}
	return failure.Code
}

// HTML 抓取：状态码、最终地址、类型、正文（已转 markdown）。
func TestFetchHTML(t *testing.T) {
	server, fetcher := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "simple-agent") {
			t.Errorf("User-Agent 应自称 simple-agent，实际 %q", got)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>T</title><script>alert(1)</script></head>
<body><h1>标题</h1><p>正文 <a href="https://example.com/x">链接</a></p></body></html>`))
	})

	result, err := fetcher.Fetch(context.Background(), server.URL+"/page")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != 200 || result.Kind != web.BodyHTML {
		t.Fatalf("状态/类型不对：%+v", result)
	}
	// Fetch 只负责取回原文；转 markdown 是工具层的事（与 dsh 的分层一致）
	markdown := web.ToMarkdown(result.Content)
	if !strings.Contains(markdown, "# 标题") {
		t.Fatalf("标题应转成 markdown：%q", markdown)
	}
	if !strings.Contains(markdown, "[链接](https://example.com/x)") {
		t.Fatalf("链接应是 markdown 链接：%q", markdown)
	}
	// 脚本整块丢弃：它既没有正文价值，也是注入的主要入口
	if strings.Contains(markdown, "alert(1)") {
		t.Fatalf("脚本内容不应出现在正文里：%q", markdown)
	}
	if result.Truncated {
		t.Fatal("小页面不该标记截断")
	}
}

// 纯文本与 JSON 原样返回（不当 HTML 解析）。
func TestFetchPlainTextAndJSON(t *testing.T) {
	server, fetcher := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"a": 1}`))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("  <不是标签>  "))
	})

	text, err := fetcher.Fetch(context.Background(), server.URL+"/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if text.Kind != web.BodyText || text.Content != "  <不是标签>  " {
		t.Fatalf("纯文本应原样返回：%+v", text)
	}

	json, err := fetcher.Fetch(context.Background(), server.URL+"/a.json")
	if err != nil {
		t.Fatal(err)
	}
	if json.Kind != web.BodyText || json.Content != `{"a": 1}` {
		t.Fatalf("JSON 应原样返回：%+v", json)
	}
}

// 中文站点常见的 GBK：要么按头部声明解码，要么按 <meta charset> 嗅探。
func TestFetchGBK(t *testing.T) {
	// "中文" 的 GBK 字节
	gbk := []byte{0xd6, 0xd0, 0xce, 0xc4}
	body := append([]byte(`<html><head><meta charset="gbk"></head><body><p>`), gbk...)
	body = append(body, []byte(`</p></body></html>`)...)

	server, fetcher := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(body)
	})
	result, err := fetcher.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "中文") {
		t.Fatalf("GBK 页面应解码成中文：%q", result.Content)
	}
}

// 响应体超过字节上限：截断而不是报错（服务器少报 Content-Length 也一样）。
func TestFetchTruncatesHugeBody(t *testing.T) {
	if testing.Short() {
		t.Skip("会传输 5MB")
	}
	server, fetcher := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		chunk := strings.Repeat("x", 64<<10)
		for written := 0; written <= web.MaxResponseBytes; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	})
	result, err := fetcher.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated {
		t.Fatal("超限响应应标记截断")
	}
	if got := len([]rune(result.Content)); got > web.MaxBodyChars {
		t.Fatalf("正文应被字符上限截断，实际 %d 字符", got)
	}
}

// Content-Length 声明的体积超限：直接拒绝，不白读。
func TestFetchRejectsDeclaredOversize(t *testing.T) {
	server, fetcher := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "999999999")
		w.WriteHeader(http.StatusOK)
	})
	_, err := fetcher.Fetch(context.Background(), server.URL)
	if code := codeOf(t, err); code != web.CodeTooLarge {
		t.Fatalf("应报 %s，实际 %s（%v）", web.CodeTooLarge, code, err)
	}
}

// 同源跳转跟随；跨源跳转拒绝（要把决定权交回模型）。
func TestRedirectPolicy(t *testing.T) {
	var sameOrigin atomic.Int32
	var server *httptest.Server
	server, fetcher := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			sameOrigin.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("done"))
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/away":
			http.Redirect(w, r, "https://example.com/x", http.StatusFound)
		}
	})
	defer server.Close()

	result, err := fetcher.Fetch(context.Background(), server.URL+"/start")
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "done" || sameOrigin.Load() != 1 {
		t.Fatalf("同源跳转应跟随：%+v", result)
	}
	if !strings.HasSuffix(result.URL, "/final") {
		t.Fatalf("结果里的 URL 应是跳转后的地址：%q", result.URL)
	}

	_, err = fetcher.Fetch(context.Background(), server.URL+"/away")
	if code := codeOf(t, err); code != web.CodeRedirectBlocked {
		t.Fatalf("跨源跳转应拒绝，实际 %s（%v）", code, err)
	}

	_, err = fetcher.Fetch(context.Background(), server.URL+"/loop")
	if code := codeOf(t, err); code != web.CodeRedirectBlocked {
		t.Fatalf("跳转次数超限应拒绝，实际 %s（%v）", code, err)
	}
	if !strings.Contains(err.Error(), "跳转") {
		t.Fatalf("错误信息应说明是跳转问题：%v", err)
	}
}

// URL 校验：只允许 http/https、不许内嵌凭据、长度有上限。
func TestURLValidation(t *testing.T) {
	fetcher := &web.Fetcher{Client: http.DefaultClient}
	cases := []struct {
		url  string
		want string
	}{
		{"file:///etc/passwd", web.CodeInvalidURL},
		{"ftp://example.com/x", web.CodeInvalidURL},
		{"https://user:pass@example.com/x", web.CodeBlockedURL},
		{"not a url", web.CodeInvalidURL},
		{"https://example.com/" + strings.Repeat("a", web.MaxURLBytes), web.CodeInvalidURL},
	}
	for _, c := range cases {
		_, err := fetcher.Fetch(context.Background(), c.url)
		if code := codeOf(t, err); code != c.want {
			t.Fatalf("%q 期望 %s，实际 %s（%v）", c.url, c.want, code, err)
		}
	}
}

// 不支持的 content-type（二进制）明确拒绝。
func TestFetchRejectsBinaryContentType(t *testing.T) {
	server, fetcher := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G', 0, 0})
	})
	_, err := fetcher.Fetch(context.Background(), server.URL)
	if code := codeOf(t, err); code != web.CodeUnsupportedType {
		t.Fatalf("二进制类型应拒绝，实际 %s（%v）", code, err)
	}
}

// 上下文取消：报"已中止"，不报成网络错误。
func TestFetchAborted(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	server, fetcher := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-arrived // 请求真的到达再取消，避免"取消早于连接"退化成连接失败
		cancel()
	}()
	_, err := fetcher.Fetch(ctx, server.URL)
	if code := codeOf(t, err); code != web.CodeAborted {
		t.Fatalf("取消应报中止，实际 %s（%v）", code, err)
	}
	if !strings.Contains(err.Error(), "已中止") {
		t.Fatalf("错误信息应是「已中止」：%v", err)
	}
}

// 超时与"连不上"都要给人能看懂的原因。
func TestFetchTimeoutAndTransport(t *testing.T) {
	block := make(chan struct{})
	server, fetcher := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-block
	})
	defer close(block)
	fetcher.Timeout = 1 // 1ns：必然超时

	_, err := fetcher.Fetch(context.Background(), server.URL)
	if code := codeOf(t, err); code != web.CodeTimeout {
		t.Fatalf("应报超时，实际 %s（%v）", code, err)
	}

	// 关掉服务器后的连接失败
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	_, err = (&web.Fetcher{}).Fetch(context.Background(), deadURL)
	if code := codeOf(t, err); code != web.CodeTransportFailure {
		t.Fatalf("应报连接失败，实际 %s（%v）", code, err)
	}
	if !strings.Contains(err.Error(), "连接失败") {
		t.Fatalf("错误信息应说明连接失败：%v", err)
	}
}

// HTTP 错误状态（404/500）仍然返回正文：模型要能读到错误页上的说明。
func TestFetchKeepsErrorStatusBody(t *testing.T) {
	server, fetcher := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("页面不存在"))
	})
	result, err := fetcher.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != 404 || result.Content != "页面不存在" {
		t.Fatalf("404 的正文应保留：%+v", result)
	}
}

// 本地文件不应被 http 客户端当成 URL（防误用）。
func TestFetchRejectsLocalPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := (&web.Fetcher{}).Fetch(context.Background(), path)
	if code := codeOf(t, err); code != web.CodeInvalidURL {
		t.Fatalf("本地路径应报 URL 非法，实际 %s（%v）", code, err)
	}
}
