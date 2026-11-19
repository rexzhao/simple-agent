package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"simple-agent/internal/web"
)

// newSearchServer 起一个假 Search API，返回给定响应。
func newSearchServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *web.Searcher) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, &web.Searcher{APIKey: "test-key", Endpoint: server.URL, Client: server.Client()}
}

// 正常响应：字段映射、去重、忽略没有 URL 的条目。
func TestSearchMapsResponse(t *testing.T) {
	var gotBody map[string]any
	server, searcher := newSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization 头不对：%q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type 不对：%q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"url":"https://a.test/1","title":"甲","snippet":"片段甲","summary":"摘要甲","date":"2026-01-02"},
			{"url":"https://a.test/1","title":"重复","snippet":"x"},
			{"url":"","title":"没有链接","snippet":"x"},
			{"url":"https://a.test/2","title":"乙","snippet":"片段乙","summary":null,"date":null}
		]}`))
	})
	defer server.Close()

	result, err := searcher.Search(context.Background(), "golang", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["query"] != "golang" {
		t.Fatalf("请求 query 不对：%v", gotBody["query"])
	}
	if gotBody["count"] != float64(5) {
		t.Fatalf("请求 count 不对：%v", gotBody["count"])
	}
	// 只要摘要与链接，不拉正文（省流量，正文交给 web_fetch）
	if gotBody["need_content"] != false {
		t.Fatalf("need_content 应为 false：%v", gotBody["need_content"])
	}
	if _, ok := gotBody["ori_question"]; ok {
		t.Fatal("没传 ori_question 时不该出现在请求体里")
	}

	if len(result.Sources) != 2 {
		t.Fatalf("应去重并丢掉无 URL 的条目，实际 %d 条：%+v", len(result.Sources), result.Sources)
	}
	first := result.Sources[0]
	if first.URL != "https://a.test/1" || first.Title != "甲" || first.Summary != "摘要甲" || first.PublishedAt != "2026-01-02" {
		t.Fatalf("第一条映射不对：%+v", first)
	}
	// null 的 summary/date 不该变成字符串 "null"
	if result.Sources[1].Summary != "" || result.Sources[1].PublishedAt != "" {
		t.Fatalf("null 字段应留空：%+v", result.Sources[1])
	}
}

// ori_question 会被传上去，并裁到 128 字符。
func TestSearchSendsBoundedOriginalQuestion(t *testing.T) {
	var gotBody map[string]any
	server, searcher := newSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	defer server.Close()

	question := strings.Repeat("问", 200)
	if _, err := searcher.Search(context.Background(), "q", 3, "  多余   空白  "+question); err != nil {
		t.Fatal(err)
	}
	sent, _ := gotBody["ori_question"].(string)
	if runes := []rune(sent); len(runes) != 128 {
		t.Fatalf("ori_question 应裁到 128 字符，实际 %d", len(runes))
	}
	if strings.Contains(sent, "  ") {
		t.Fatalf("连续空白应折叠：%q", sent)
	}
}

// count 有上限，超出按服务端上限截断。
func TestSearchClampsCount(t *testing.T) {
	var gotBody map[string]any
	server, searcher := newSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	defer server.Close()

	if _, err := searcher.Search(context.Background(), "q", 999, ""); err != nil {
		t.Fatal(err)
	}
	if gotBody["count"] != float64(web.SearchMaxCount) {
		t.Fatalf("count 应被截到 %d，实际 %v", web.SearchMaxCount, gotBody["count"])
	}
}

// 401 要点名"检查 Key"——这是用户唯一能自己修的错误。
func TestSearchAuthError(t *testing.T) {
	server, searcher := newSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	})
	defer server.Close()

	_, err := searcher.Search(context.Background(), "q", 3, "")
	if code := codeOf(t, err); code != web.CodeSearchAuth {
		t.Fatalf("应报 %s，实际 %s（%v）", web.CodeSearchAuth, code, err)
	}
	if !strings.Contains(err.Error(), "DEEPSEEK_API_KEY") || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("错误信息应带上 Key 提示与服务端原因：%v", err)
	}
}

// 结构不对的响应明确报错，不猜。
func TestSearchInvalidResponse(t *testing.T) {
	cases := map[string]string{
		"不是 JSON":   `not json`,
		"缺 results": `{"foo":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server, searcher := newSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			defer server.Close()
			_, err := searcher.Search(context.Background(), "q", 3, "")
			if code := codeOf(t, err); code != web.CodeProviderError {
				t.Fatalf("应报 %s，实际 %s（%v）", web.CodeProviderError, code, err)
			}
		})
	}
}

// 没有 Key：Available 为假，Search 直接报不可用（不偷偷发请求）。
func TestSearchUnavailableWithoutKey(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	searcher := &web.Searcher{Endpoint: server.URL, Client: server.Client()}
	if searcher.Available() {
		t.Fatal("没有 Key 时 Available 应为假")
	}
	if _, err := searcher.Search(context.Background(), "q", 3, ""); err == nil {
		t.Fatal("没有 Key 应报错")
	}
	if calls.Load() != 0 {
		t.Fatal("没有 Key 时不该发出请求")
	}
}

// QPS 限速：第 2 个请求要等满一秒窗口。
func TestSearchRateLimit(t *testing.T) {
	server, searcher := newSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	defer server.Close()
	searcher.RequestsPerSecond = 1

	started := time.Now()
	if _, err := searcher.Search(context.Background(), "q1", 3, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := searcher.Search(context.Background(), "q2", 3, ""); err != nil {
		t.Fatal(err)
	}
	// 第 2 个被限速等待，所以总耗时至少接近 1 秒（留点余量）
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("第二个请求应被限速，实际只用了 %v", elapsed)
	}
}

// 限速等待期间可取消，不能卡住用户的停止操作。
func TestSearchRateLimitHonorsContext(t *testing.T) {
	server, searcher := newSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	defer server.Close()
	searcher.RequestsPerSecond = 1

	if _, err := searcher.Search(context.Background(), "q1", 3, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := searcher.Search(ctx, "q2", 3, "")
	if err == nil {
		t.Fatal("限速等待应被取消")
	}
	if elapsed := time.Since(started); elapsed > 800*time.Millisecond {
		t.Fatalf("取消后应立即返回，实际等了 %v", elapsed)
	}
}

// 多查询合并：轮转、去重、按上限截断。
func TestMergeSearchResults(t *testing.T) {
	results := []web.SearchResult{
		{Sources: []web.Source{{URL: "a"}, {URL: "b"}, {URL: "c"}}},
		{Sources: []web.Source{{URL: "d"}, {URL: "b"}}},
	}
	merged := web.MergeSearchResults(results, 10)
	// 轮转：a、d、b、c（b 重复只留第一次出现的位置）
	want := []string{"a", "d", "b", "c"}
	if len(merged.Sources) != len(want) {
		t.Fatalf("合并后应有 %d 条，实际 %d：%+v", len(want), len(merged.Sources), merged.Sources)
	}
	for index, url := range want {
		if merged.Sources[index].URL != url {
			t.Fatalf("第 %d 条应是 %s，实际 %s（全部：%+v）", index, url, merged.Sources[index].URL, merged.Sources)
		}
	}
	if merged.Truncated {
		t.Fatal("没超上限不该标记截断")
	}

	// 超上限：截断并标记
	limited := web.MergeSearchResults(results, 2)
	if len(limited.Sources) != 2 || !limited.Truncated {
		t.Fatalf("超上限应截断并标记：%+v", limited)
	}
}
