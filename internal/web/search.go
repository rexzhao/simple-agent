package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DeepSeek Search API 客户端（对齐 dsh 的 `web-deepseek-api` + `web-search-deepseek`）。
//
// 为什么是这家：dsh 的默认搜索后端就是它（`https://api.deepseek.com/v1/tools/search`），
// 用同一个 `DEEPSEEK_API_KEY`，用户不必为搜索再申请一套凭据。请求体与响应字段
// 逐字对齐 dsh：
//
//	请求  {query, count, need_content:false, ori_question?}
//	响应  {results:[{url, title, snippet, summary?, date?}]}
//
// 与 dsh 的差异（都用注释标出）：dsh 支持"账号凭据 / API Key"两种来源与失效回调，
// 本仓只有环境变量里的一个 Key；也没有它那套 provider 注册表——搜索后端只有这一个。

const (
	// DeepSeekSearchURL 是 Search API 的地址（dsh 的 `DEEPSEEK_WEB_API_BASE_URL` + `/search`）。
	DeepSeekSearchURL = "https://api.deepseek.com/v1/tools/search"
	// SearchDefaultCount 是单次搜索的默认结果数（服务端默认值）。
	SearchDefaultCount = 10
	// SearchMaxCount 是服务端接受的 count 上限。
	SearchMaxCount = 50
	// SearchRequestsPerSecond 是服务端的账号级 QPS 限制（5 QPS）。
	//
	// 必须自己限速：多查询会并发发出去，不限速就直接吃 429。
	SearchRequestsPerSecond = 5
	// SearchTimeout 是单次搜索的总时限。
	SearchTimeout = 30 * time.Second
	// oriQuestionMaxChars 是 `ori_question` 允许的最长字符数（服务端限制 128）。
	oriQuestionMaxChars = 128
)

// Source 是一条搜索结果的来源。
type Source struct {
	// URL 是来源地址。
	URL string
	// Title 是标题（可能为空）。
	Title string
	// Snippet 是摘要片段（可能为空）。
	Snippet string
	// Summary 是服务端生成的较长摘要（可能为空）。
	Summary string
	// PublishedAt 是发布时间（可能为空）。
	PublishedAt string
}

// SearchResult 是一次搜索的结果。
type SearchResult struct {
	// Sources 按服务端顺序排列（已去重）。
	Sources []Source
	// Truncated 表示结果被 maxResults 截断。
	Truncated bool
}

// Searcher 调 DeepSeek Search API。
//
// 零值不可用（没有 Key）：调用方应先用 Available 判断，再决定是否注册工具——
// 注册了却每次报"没配 Key"会让模型反复试错。
type Searcher struct {
	// APIKey 是 DeepSeek 的 API Key（取 DEEPSEEK_API_KEY）。
	APIKey string
	// Endpoint 覆盖默认地址（测试用；空 = DeepSeekSearchURL）。
	Endpoint string
	// Client 覆盖 HTTP 客户端（测试用）。
	Client *http.Client
	// RequestsPerSecond 覆盖限速（测试可调高，避免用例等秒级）。
	RequestsPerSecond int

	mu sync.Mutex
	// starts 是当前一秒窗口内已发出的请求时间（用来限速）。
	starts []time.Time
}

// Available 报告搜索是否可用（本仓只有"有没有 Key"这一个条件）。
func (s *Searcher) Available() bool {
	return s != nil && strings.TrimSpace(s.APIKey) != ""
}

// endpoint 返回实际请求地址。
func (s *Searcher) endpoint() string {
	if s.Endpoint != "" {
		return s.Endpoint
	}
	return DeepSeekSearchURL
}

// ratePerSecond 返回生效的限速值。
func (s *Searcher) ratePerSecond() int {
	if s.RequestsPerSecond > 0 {
		return s.RequestsPerSecond
	}
	return SearchRequestsPerSecond
}

// Search 执行一次搜索。
func (s *Searcher) Search(ctx context.Context, query string, maxResults int, originalQuestion string) (SearchResult, error) {
	if !s.Available() {
		return SearchResult{}, &Error{
			Code:    CodeSearchUnavailable,
			Message: "搜索不可用：没有配置 DEEPSEEK_API_KEY",
		}
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchResult{}, &Error{Code: CodeSearchInvalidArgs, Message: "搜索词不能为空"}
	}

	count := maxResults
	// 0 表示"用默认值"；有值时只受服务端上限约束（不要顺手压到默认 10——
	// 那会让模型要 20 条时静默只拿到 10 条）。
	if count <= 0 {
		count = SearchDefaultCount
	}
	if count > SearchMaxCount {
		count = SearchMaxCount
	}

	if err := s.waitForSlot(ctx); err != nil {
		return SearchResult{}, err
	}

	body := map[string]any{
		"query": query,
		"count": count,
		// 只取摘要与链接：正文由 web_fetch 按需抓（省流量也省服务端内容费）。
		"need_content": false,
	}
	if question := boundedQuestion(originalQuestion); question != "" {
		body["ori_question"] = question
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return SearchResult{}, &Error{Code: CodeProviderError, Message: "构造请求失败：" + err.Error()}
	}

	timeout := SearchTimeout
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, s.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return SearchResult{}, &Error{Code: CodeProviderError, Message: "构造请求失败：" + err.Error()}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+s.APIKey)
	request.Header.Set("User-Agent", UserAgent)

	response, err := s.client().Do(request)
	if err != nil {
		if requestCtx.Err() != nil {
			if ctx.Err() != nil {
				return SearchResult{}, &Error{Code: CodeAborted, Message: "已中止"}
			}
			return SearchResult{}, &Error{Code: CodeTimeout, Message: "搜索超时，请稍后重试"}
		}
		return SearchResult{}, &Error{Code: CodeTransportFailure, Message: "搜索请求失败：" + err.Error()}
	}
	defer response.Body.Close()

	// 读响应体时也限流，避免异常服务端把内存吃满。
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes))
	if err != nil {
		return SearchResult{}, &Error{Code: CodeTransportFailure, Message: "读取搜索响应失败：" + err.Error()}
	}

	if response.StatusCode != http.StatusOK {
		return SearchResult{}, providerError(response.StatusCode, raw)
	}
	return mapSearchResponse(raw)
}

func (s *Searcher) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

// waitForSlot 按 QPS 限速（滑动一秒窗口）。
//
// 多查询会并发发出去，不限速就直接吃 429（服务端是账号级 5 QPS）。等待期间
// 必须能被 ctx 取消，否则用户点停止还得等窗口滑过去。
func (s *Searcher) waitForSlot(ctx context.Context) error {
	limit := s.ratePerSecond()
	for {
		s.mu.Lock()
		now := time.Now()
		// 丢掉一秒以外的记录
		for len(s.starts) > 0 && now.Sub(s.starts[0]) > time.Second {
			s.starts = s.starts[1:]
		}
		if len(s.starts) < limit {
			s.starts = append(s.starts, now)
			s.mu.Unlock()
			return nil
		}
		wait := time.Second - now.Sub(s.starts[0])
		s.mu.Unlock()

		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return &Error{Code: CodeAborted, Message: "已中止"}
		case <-timer.C:
		}
	}
}

// mapSearchResponse 把服务端 JSON 映射成 SearchResult。
//
// 字段校验对齐 dsh：url/title/snippet 必须是字符串（title/snippet 可为空串），
// summary 与 date 可缺省或为 null。任何结构不符都当 provider 错误——不猜。
func mapSearchResponse(raw []byte) (SearchResult, error) {
	var payload struct {
		Results []struct {
			URL     *string `json:"url"`
			Title   *string `json:"title"`
			Snippet *string `json:"snippet"`
			Summary *string `json:"summary"`
			Date    *string `json:"date"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return SearchResult{}, &Error{
			Code:    CodeProviderError,
			Message: "搜索服务返回了无法解析的内容",
		}
	}
	if payload.Results == nil {
		return SearchResult{}, &Error{
			Code:    CodeProviderError,
			Message: "搜索服务返回了无法解析的内容（缺少 results）",
		}
	}

	result := SearchResult{Sources: make([]Source, 0, len(payload.Results))}
	seen := map[string]bool{}
	for _, item := range payload.Results {
		if item.URL == nil || *item.URL == "" {
			// 没有链接的结果对模型没有用处（无法引用、无法跟进抓取）
			continue
		}
		if seen[*item.URL] {
			continue
		}
		seen[*item.URL] = true
		source := Source{URL: *item.URL}
		if item.Title != nil {
			source.Title = *item.Title
		}
		if item.Snippet != nil {
			source.Snippet = *item.Snippet
		}
		if item.Summary != nil {
			source.Summary = *item.Summary
		}
		if item.Date != nil {
			source.PublishedAt = *item.Date
		}
		result.Sources = append(result.Sources, source)
	}
	return result, nil
}

// providerError 把非 200 转成可读错误。
//
// 401 单独点名：这是用户唯一能自己修的一类（Key 没配/失效），提示要能照着做。
func providerError(status int, raw []byte) error {
	detail := strings.TrimSpace(string(raw))
	// 尝试取服务端的结构化信息（{"error":{"message":...}} 或 {"error":"..."}）
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && len(payload.Error) > 0 {
		var message string
		if json.Unmarshal(payload.Error, &message) == nil && message != "" {
			detail = message
		} else {
			var nested struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(payload.Error, &nested) == nil && nested.Message != "" {
				detail = nested.Message
			}
		}
	}
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}

	message := fmt.Sprintf("搜索服务返回 HTTP %d", status)
	if detail != "" {
		message += "：" + detail
	}
	code := CodeProviderError
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		code = CodeSearchAuth
		message += "（请检查 DEEPSEEK_API_KEY）"
	}
	return &Error{Code: code, Message: message}
}

// boundedQuestion 把触发搜索的用户问题裁到 `ori_question` 允许的长度。
func boundedQuestion(question string) string {
	collapsed := strings.Join(strings.Fields(question), " ")
	if collapsed == "" {
		return ""
	}
	runes := []rune(collapsed)
	if len(runes) > oriQuestionMaxChars {
		return string(runes[:oriQuestionMaxChars])
	}
	return collapsed
}

// MergeSearchResults 把多个查询的结果合并成一份（对齐 dsh 的 mergeSearchResults）。
//
// 轮转合并（每个查询各取第 1 名、再各取第 2 名…）而不是简单拼接：多查询通常是
// 同一主题的不同说法，轮转能让每个说法都进前列，拼接会让第一个查询占满名额。
// 按 URL 去重，超过 maxResults 就截断并标记 Truncated。
func MergeSearchResults(results []SearchResult, maxResults int) SearchResult {
	seen := map[string]bool{}
	sources := make([]Source, 0, maxResults)
	truncated := false
	deepest := 0
	for _, result := range results {
		deepest = max(deepest, len(result.Sources))
		truncated = truncated || result.Truncated
	}

merge:
	for rank := 0; rank < deepest; rank++ {
		for _, result := range results {
			if rank >= len(result.Sources) {
				continue
			}
			source := result.Sources[rank]
			if seen[source.URL] {
				continue
			}
			seen[source.URL] = true
			if len(sources) == maxResults {
				truncated = true
				break merge
			}
			sources = append(sources, source)
		}
	}
	return SearchResult{Sources: sources, Truncated: truncated}
}
