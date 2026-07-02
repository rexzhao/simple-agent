package openairesponses

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

func TestProviderStreamPostsResponsesRequestAndEmitsEvents(t *testing.T) {
	requests := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requests <- capturedRequest{
			Method:        r.Method,
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			Body:          body,
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n")
	}))
	defer server.Close()

	provider, err := NewProvider(ProviderConfig{
		BaseURL:    server.URL + "/v1/",
		APIKey:     "test-key",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{
		Model: "gpt-5.1",
		Messages: []model.Message{
			{Role: model.MessageRoleUser, Content: "Hello"},
		},
		Parameters: map[string]any{
			"temperature": 0.6,
		},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	got := collectEvents(t, events)
	if len(got) != 2 {
		t.Fatalf("len(events) = %d, want 2: %#v", len(got), got)
	}
	text, ok := got[0].(model.TextDeltaEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.TextDeltaEvent", got[0])
	}
	if text.Text != "hello" {
		t.Fatalf("Text = %q, want hello", text.Text)
	}
	usage, ok := got[1].(model.UsageEvent)
	if !ok {
		t.Fatalf("event[1] = %T, want model.UsageEvent", got[1])
	}
	wantUsage := model.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}
	if usage.Usage != wantUsage {
		t.Fatalf("Usage = %#v, want %#v", usage.Usage, wantUsage)
	}

	gotRequest := <-requests
	if gotRequest.Method != http.MethodPost {
		t.Fatalf("method = %q, want %q", gotRequest.Method, http.MethodPost)
	}
	if gotRequest.Path != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotRequest.Path)
	}
	if gotRequest.Authorization != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want bearer token", gotRequest.Authorization)
	}
	if gotRequest.ContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotRequest.ContentType)
	}
	assertJSONEqual(t, gotRequest.Body, `{
		"model": "gpt-5.1",
		"input": [
			{"role": "user", "content": "Hello"}
		],
		"stream": true,
		"temperature": 0.6
	}`)
}

func TestProviderStreamReturnsUsefulHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited for Authorization: Bearer http-secret-value", http.StatusTooManyRequests)
	}))
	defer server.Close()

	provider, err := NewProvider(ProviderConfig{
		BaseURL:    server.URL,
		APIKey:     "http-secret-value",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{Model: "gpt-5.1"})
	if err == nil {
		t.Fatalf("Stream() error = nil, want HTTP error")
	}
	if events != nil {
		t.Fatalf("events = %#v, want nil", events)
	}
	message := err.Error()
	for _, want := range []string{"429 Too Many Requests", "rate limited"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q missing %q", message, want)
		}
	}
	for _, leaked := range []string{"http-secret-value", "Bearer http-secret-value"} {
		if strings.Contains(message, leaked) {
			t.Fatalf("error leaked %q: %s", leaked, message)
		}
	}
}

func TestProviderStreamRetries429AndEmitsText(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()

	provider, err := NewProvider(ProviderConfig{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		HTTPClient: server.Client(),
		HTTPOptions: httpstream.Options{
			RequestTimeout:    time.Second,
			StreamIdleTimeout: time.Second,
			MaxRetryAttempts:  3,
			RetryBackoff:      time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{Model: "gpt-5.1"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectEvents(t, events)
	if len(got) != 1 {
		t.Fatalf("len(events) = %d, want 1: %#v", len(got), got)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestProviderStreamRequiresAPIKey(t *testing.T) {
	provider, err := NewProvider(ProviderConfig{
		BaseURL: "http://127.0.0.1:1/v1",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{Model: "gpt-5.1"})
	if err == nil {
		t.Fatal("Stream() error = nil, want missing API key error")
	}
	if events != nil {
		t.Fatalf("events = %#v, want nil", events)
	}
	if got := err.Error(); !strings.Contains(got, "API key is required") {
		t.Fatalf("Stream() error = %q, want missing API key message", got)
	}
}

type capturedRequest struct {
	Method        string
	Path          string
	Authorization string
	ContentType   string
	Body          []byte
}

func collectEvents(t *testing.T, events <-chan model.Event) []model.Event {
	t.Helper()

	var got []model.Event
	timeout := time.After(time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return got
			}
			if errorEvent, ok := event.(model.ErrorEvent); ok {
				t.Fatalf("stream error event = %v (%s)", errorEvent.Err, errorEvent.Message)
			}
			got = append(got, event)
		case <-timeout:
			t.Fatalf("timed out waiting for stream events")
		}
	}
}
