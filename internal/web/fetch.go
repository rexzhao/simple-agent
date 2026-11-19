// Package web 是 web_fetch 工具的抓取与正文转换（对齐 deepseek-harness 的
// `web-fetch-http` + `tool-web` 两半）。
//
// 分两块：
//   - Fetcher：URL 校验、同源跳转、字节/字符上限、按 Content-Type 分类并解码；
//   - ToMarkdown：HTML 正文转 markdown（模型读的是文本，不是标签）。
//
// 与 dsh 的差异（按本仓的取舍）：
//   - 不做 web_search（用户决定只做抓取），所以这里没有搜索相关的任何东西；
//   - 不在 DNS 层拦内网地址：本工具默认就对用户可见，能访问内网与本地地址是
//     刻意保留的能力（对齐 dsh 的 http provider 描述），只做 scheme/凭据/跳转
//     这类与网络安全无关的校验；
//   - 不跟随跨源跳转（每次跳转都要模型自己重新决定），最多 5 跳。
package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html/charset"
)

// 限额取自 dsh 的 `web-fetch-http` 默认值（这些数不是随手定的：字节上限挡住
// 假装是文本的巨大响应，字符上限决定模型看到多少正文）。
const (
	// MaxURLBytes 是一条请求 URL 的长度上限。
	MaxURLBytes = 2048
	// MaxResponseBytes 是读取响应的字节上限（超出即截断，不报错）。
	MaxResponseBytes = 5_000_000
	// MaxBodyChars 是解码后正文的字符上限。
	MaxBodyChars = 100_000
	// MaxRedirects 是同源跳转的最大跳数。
	MaxRedirects = 5
	// DefaultTimeout 是一次抓取的总时限。
	DefaultTimeout = 30 * time.Second
	// UserAgent 明确自称本产品，不伪装浏览器（伪装反而更容易被特殊对待）。
	UserAgent = "simple-agent/0.1 (+https://github.com/egoist/simple-agent)"
)

// BodyKind 是正文类型：HTML 会被转成 markdown，text 原样返回。
type BodyKind string

const (
	BodyHTML BodyKind = "html"
	BodyText BodyKind = "text"
)

// Result 是一次成功的抓取。
type Result struct {
	// URL 是跳转之后的最终地址（可能和请求的不同）。
	URL string
	// Status 是最终响应的 HTTP 状态码。
	Status int
	// Kind 是正文类型。
	Kind BodyKind
	// Content 是已解码、已按字符上限截断的正文。
	Content string
	// Truncated 表示正文被字节上限或字符上限截过。
	Truncated bool
}

// Error 是抓取失败（模型看到的错误信息，不含内部细节）。
type Error struct {
	// Code 是稳定分类（WEB_INVALID_URL / WEB_REDIRECT_BLOCKED / …）。
	Code string
	// Message 是给人看的原因。
	Message string
}

func (e *Error) Error() string { return e.Message }

// 错误分类。与 dsh 的 WebError code 同名，便于对照排查。
const (
	CodeInvalidURL       = "WEB_INVALID_URL"
	CodeBlockedURL       = "WEB_BLOCKED_URL"
	CodeRedirectBlocked  = "WEB_REDIRECT_BLOCKED"
	CodeUnsupportedType  = "WEB_UNSUPPORTED_CONTENT_TYPE"
	CodeTooLarge         = "WEB_FETCH_TOO_LARGE"
	CodeTimeout          = "WEB_FETCH_TIMEOUT"
	CodeAborted          = "WEB_ABORTED"
	CodeProviderError    = "WEB_PROVIDER_ERROR"
	CodeTransportFailure = "WEB_TRANSPORT_FAILURE"
)

// Fetcher 抓取一个 URL 并把正文解码成文本。
//
// 零值不可用（Client 为 nil 时用 http.DefaultClient）；Timeout 为 0 时用默认值。
type Fetcher struct {
	// Client 是可注入的 HTTP 客户端（测试用 httptest 的 client）。
	Client *http.Client
	// Timeout 是单次抓取的总时限（0 = DefaultTimeout）。
	Timeout time.Duration
}

// Fetch 抓取 url。
//
// 只跟随**同源**跳转（scheme + host + port 相同）：跨源跳转要模型自己重新决定，
// 否则一次工具调用就能被服务端带着去访问任意站点（dsh 的同一取舍）。
func (f *Fetcher) Fetch(ctx context.Context, raw string) (Result, error) {
	target, err := validateURL(raw)
	if err != nil {
		return Result{}, err
	}

	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := *f.client()
	// 自己处理跳转：要逐跳校验同源与 URL 规范，也要数跳数。
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	current := target
	for hop := 0; ; hop++ {
		response, err := f.request(ctx, &client, current)
		if err != nil {
			return Result{}, err
		}
		if !isRedirect(response.StatusCode) {
			return readBody(response, current)
		}

		if hop >= MaxRedirects {
			response.Body.Close()
			return Result{}, &Error{
				Code:    CodeRedirectBlocked,
				Message: fmt.Sprintf("跳转次数超过上限（%d 次），请直接请求最终地址", MaxRedirects),
			}
		}
		location := response.Header.Get("Location")
		response.Body.Close()
		if location == "" {
			return Result{}, &Error{
				Code:    CodeProviderError,
				Message: fmt.Sprintf("HTTP %d 跳转没有 Location 头", response.StatusCode),
			}
		}
		next, err := current.Parse(location)
		if err != nil {
			return Result{}, &Error{Code: CodeProviderError, Message: "跳转地址无法解析：" + location}
		}
		if !sameOrigin(current, next) {
			return Result{}, &Error{
				Code: CodeRedirectBlocked,
				Message: fmt.Sprintf("跳转跨站到 %s://%s，不会自动跟随；请直接请求那个地址",
					next.Scheme, next.Host),
			}
		}
		if _, err := validateURL(next.String()); err != nil {
			return Result{}, err
		}
		current = next
	}
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return http.DefaultClient
}

// request 发一次 GET 并做取消/超时的分类。
func (f *Fetcher) request(ctx context.Context, client *http.Client, url *neturl.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url.String(), nil)
	if err != nil {
		return nil, &Error{Code: CodeInvalidURL, Message: "无法构造请求：" + err.Error()}
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/*;q=0.9,application/json;q=0.8")

	response, err := client.Do(req)
	if err != nil {
		return nil, classifyTransport(ctx, err)
	}
	return response, nil
}

// readBody 读取、按字节上限截断、按 Content-Type 解码。
func readBody(response *http.Response, final *neturl.URL) (Result, error) {
	defer response.Body.Close()

	contentType := response.Header.Get("Content-Type")
	kind := classifyContentType(contentType)
	if kind == "" {
		return Result{}, &Error{
			Code: CodeUnsupportedType,
			Message: fmt.Sprintf("不支持的内容类型 %q（只处理 HTML 与文本）",
				orUnknown(contentType)),
		}
	}
	// Content-Length 超限直接拒绝（省得白读一遍）；没声明就靠流式截断。
	if declared := response.Header.Get("Content-Length"); declared != "" {
		if size, err := strconv.ParseInt(declared, 10, 64); err == nil && size > MaxResponseBytes {
			return Result{}, &Error{
				Code:    CodeTooLarge,
				Message: fmt.Sprintf("响应体 %d 字节，超过上限 %d", size, MaxResponseBytes),
			}
		}
	}

	// 多读一个字节用来判断"是否超限"：ReadAll + LimitReader 没有截断标志。
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return Result{}, &Error{Code: CodeTransportFailure, Message: "读取响应失败：" + err.Error()}
	}
	truncatedByBytes := len(raw) > MaxResponseBytes
	if truncatedByBytes {
		raw = raw[:MaxResponseBytes]
	}

	text := decode(raw, contentType)
	truncatedByChars := false
	if runes := []rune(text); len(runes) > MaxBodyChars {
		text = string(runes[:MaxBodyChars])
		truncatedByChars = true
	}

	return Result{
		URL:       final.String(),
		Status:    response.StatusCode,
		Kind:      kind,
		Content:   text,
		Truncated: truncatedByBytes || truncatedByChars,
	}, nil
}

// decode 按 Content-Type（含 <meta charset>）把字节解码成 UTF-8。
//
// 用 x/net/html/charset：它实现了 WHATWG 的编码嗅探顺序（BOM → 头部 charset →
// 正文 <meta> → UTF-8 探测 → windows-1252 兜底）。中文站点大量用 GBK，只按
// UTF-8 解会得到乱码，而乱码塞进模型上下文既无意义又费 token。
func decode(raw []byte, contentType string) string {
	encoding, _, _ := charset.DetermineEncoding(raw, contentType)
	decoded, err := encoding.NewDecoder().Bytes(raw)
	if err != nil {
		// 解码失败不致命：保留可读部分（替换非法字节）
		return strings.ToValidUTF8(string(raw), "\uFFFD")
	}
	return strings.ToValidUTF8(string(decoded), "\uFFFD")
}

// classifyContentType 把 Content-Type 归到可处理的两种类型，其它返回空串。
func classifyContentType(contentType string) BodyKind {
	mime := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch {
	case mime == "text/html", mime == "application/xhtml+xml":
		return BodyHTML
	case strings.HasPrefix(mime, "text/"):
		return BodyText
	case mime == "application/json", mime == "application/xml",
		strings.HasSuffix(mime, "+json"), strings.HasSuffix(mime, "+xml"):
		return BodyText
	}
	return ""
}

// validateURL 校验请求地址：长度、语法、scheme、内嵌凭据。
func validateURL(raw string) (*neturl.URL, error) {
	if len(raw) > MaxURLBytes {
		return nil, &Error{
			Code:    CodeInvalidURL,
			Message: fmt.Sprintf("URL 超过 %d 字节上限", MaxURLBytes),
		}
	}
	parsed, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, &Error{Code: CodeInvalidURL, Message: "URL 无法解析：" + raw}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, &Error{
			Code:    CodeInvalidURL,
			Message: fmt.Sprintf("只支持 http/https，收到 %q", parsed.Scheme),
		}
	}
	if parsed.Host == "" {
		return nil, &Error{Code: CodeInvalidURL, Message: "URL 缺少主机名：" + raw}
	}
	if parsed.User != nil {
		return nil, &Error{Code: CodeBlockedURL, Message: "URL 里不允许内嵌用户名/口令"}
	}
	return parsed, nil
}

// sameOrigin 判断两个地址是否同源（scheme + host + port）。
func sameOrigin(a, b *neturl.URL) bool {
	return a.Scheme == b.Scheme && a.Hostname() == b.Hostname() && portOf(a) == portOf(b)
}

// portOf 取实际端口（没写端口时按 scheme 补默认值）。
func portOf(url *neturl.URL) string {
	if port := url.Port(); port != "" {
		return port
	}
	switch url.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// classifyTransport 把网络错误分成人能看懂的几类。
//
// 区分"用户停止"与"超时"很重要：前者不该显示成错误原因，后者要提示调小页面
// 或换地址。剩余的都归到连接失败，并把底层原因带上（DNS/TLS/连接被拒对排查
// 很有用）。
func classifyTransport(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &Error{Code: CodeTimeout, Message: "抓取超时，请稍后重试或换一个地址"}
		}
		return &Error{Code: CodeAborted, Message: "已中止"}
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "no such host"):
		return &Error{Code: CodeTransportFailure, Message: "域名解析失败：" + message}
	case strings.Contains(message, "certificate"), strings.Contains(message, "tls:"):
		return &Error{Code: CodeTransportFailure, Message: "TLS 校验失败：" + message}
	}
	return &Error{Code: CodeTransportFailure, Message: "连接失败：" + message}
}

// orUnknown 给空 Content-Type 一个可读的占位。
func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "未声明"
	}
	return value
}
