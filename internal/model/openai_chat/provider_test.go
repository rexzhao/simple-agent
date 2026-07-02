package openaichat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

func TestProviderStreamPostsChatCompletionsRequest(t *testing.T) {
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
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
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
		Model: "glm-5.2",
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
	if got := collectEvents(t, events); len(got) != 0 {
		t.Fatalf("events = %#v, want none", got)
	}

	gotRequest := <-requests
	if gotRequest.Method != http.MethodPost {
		t.Fatalf("method = %q, want %q", gotRequest.Method, http.MethodPost)
	}
	if gotRequest.Path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotRequest.Path)
	}
	if gotRequest.Authorization != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want bearer token", gotRequest.Authorization)
	}
	if gotRequest.ContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotRequest.ContentType)
	}
	assertJSONEqual(t, gotRequest.Body, `{
		"model": "glm-5.2",
		"messages": [
			{"role": "user", "content": "Hello"}
		],
		"stream": true,
		"temperature": 0.6
	}`)
}

func TestProviderStreamEmitsContentReasoningUsageAndStopsAtDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	provider, err := NewProvider(ProviderConfig{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{Model: "glm-5.2"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	got := collectEvents(t, events)
	if len(got) != 3 {
		t.Fatalf("len(events) = %d, want 3: %#v", len(got), got)
	}
	text, ok := got[0].(model.TextDeltaEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.TextDeltaEvent", got[0])
	}
	if text.Text != "hello" {
		t.Fatalf("Text = %q, want hello", text.Text)
	}
	reasoning, ok := got[1].(model.ReasoningDeltaEvent)
	if !ok {
		t.Fatalf("event[1] = %T, want model.ReasoningDeltaEvent", got[1])
	}
	if reasoning.Text != "thinking" {
		t.Fatalf("Reasoning = %q, want thinking", reasoning.Text)
	}
	usage, ok := got[2].(model.UsageEvent)
	if !ok {
		t.Fatalf("event[2] = %T, want model.UsageEvent", got[2])
	}
	wantUsage := model.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}
	if usage.Usage != wantUsage {
		t.Fatalf("Usage = %#v, want %#v", usage.Usage, wantUsage)
	}
}

func TestProviderStreamEmitsCompleteToolCallDoneFromSplitArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"docs/\"}}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"checklist.md\\\"}\"}}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	provider, err := NewProvider(ProviderConfig{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{Model: "glm-5.2"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	got := collectEvents(t, events)
	if len(got) != 3 {
		t.Fatalf("len(events) = %d, want 3: %#v", len(got), got)
	}
	done, ok := got[2].(model.ToolCallDoneEvent)
	if !ok {
		t.Fatalf("event[2] = %T, want model.ToolCallDoneEvent", got[2])
	}
	want := model.ToolCall{ID: "call_1", Name: "read_file", Arguments: `{"path":"docs/checklist.md"}`}
	if done.ToolCall != want {
		t.Fatalf("ToolCall = %#v, want %#v", done.ToolCall, want)
	}
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

	events, err := provider.Stream(context.Background(), model.Request{Model: "glm-5.2"})
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

func TestProviderStreamRequestTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	provider, err := NewProvider(ProviderConfig{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		HTTPClient: server.Client(),
		HTTPOptions: httpstream.Options{
			RequestTimeout:    20 * time.Millisecond,
			StreamIdleTimeout: time.Second,
			MaxRetryAttempts:  1,
			RetryBackoff:      time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	start := time.Now()
	events, err := provider.Stream(context.Background(), model.Request{Model: "glm-5.2"})
	if err == nil {
		t.Fatalf("Stream() error = nil, want request timeout")
	}
	if events != nil {
		t.Fatalf("events = %#v, want nil", events)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Stream() took %s, want bounded request timeout", elapsed)
	}
	if got := err.Error(); !strings.Contains(got, "request timeout waiting for response headers") {
		t.Fatalf("Stream() error = %q, want request timeout message", got)
	}
}

func TestProviderStreamEmitsStreamIdleTimeout(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	provider, err := NewProvider(ProviderConfig{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		HTTPClient: server.Client(),
		HTTPOptions: httpstream.Options{
			RequestTimeout:    time.Second,
			StreamIdleTimeout: 20 * time.Millisecond,
			MaxRetryAttempts:  3,
			RetryBackoff:      time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{Model: "glm-5.2"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	event := nextEvent(t, events)
	errorEvent, ok := event.(model.ErrorEvent)
	if !ok {
		t.Fatalf("event = %T, want model.ErrorEvent", event)
	}
	if !strings.Contains(errorEvent.Message, "idle timeout") {
		t.Fatalf("ErrorEvent.Message = %q, want idle timeout", errorEvent.Message)
	}
	if errorEvent.Err == nil || !strings.Contains(errorEvent.Err.Error(), "SSE stream idle timeout") {
		t.Fatalf("ErrorEvent.Err = %v, want SSE idle timeout", errorEvent.Err)
	}
	assertChannelClosed(t, events)
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want no retry after stream started", got)
	}
}

func TestProviderStreamEmitsErrorWhenContextCanceledAfterStreamStarts(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	provider, err := NewProvider(ProviderConfig{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		HTTPClient: server.Client(),
		HTTPOptions: httpstream.Options{
			RequestTimeout:    time.Second,
			StreamIdleTimeout: time.Second,
			MaxRetryAttempts:  1,
			RetryBackoff:      time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	events, err := provider.Stream(ctx, model.Request{Model: "glm-5.2"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	event := nextEvent(t, events)
	text, ok := event.(model.TextDeltaEvent)
	if !ok {
		t.Fatalf("event = %T, want model.TextDeltaEvent", event)
	}
	if text.Text != "partial" {
		t.Fatalf("Text = %q, want partial", text.Text)
	}

	cancel()
	event = nextEvent(t, events)
	errorEvent, ok := event.(model.ErrorEvent)
	if !ok {
		t.Fatalf("event = %T, want model.ErrorEvent", event)
	}
	if !errors.Is(errorEvent.Err, context.Canceled) {
		t.Fatalf("ErrorEvent.Err = %v, want context.Canceled", errorEvent.Err)
	}
	if !strings.Contains(errorEvent.Message, "read OpenAI chat stream") {
		t.Fatalf("ErrorEvent.Message = %q, want read stream message", errorEvent.Message)
	}
	assertChannelClosed(t, events)
}

func TestProviderStreamKeepsLongStreamAliveWhenChunksArrive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		flusher.Flush()
		time.Sleep(25 * time.Millisecond)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	provider, err := NewProvider(ProviderConfig{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		HTTPClient: server.Client(),
		HTTPOptions: httpstream.Options{
			RequestTimeout:    time.Second,
			StreamIdleTimeout: 100 * time.Millisecond,
			MaxRetryAttempts:  1,
			RetryBackoff:      time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{Model: "glm-5.2"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got := collectEvents(t, events)
	if len(got) != 2 {
		t.Fatalf("len(events) = %d, want 2: %#v", len(got), got)
	}
}

func TestProviderStreamRetries429AndPreservesRequestBody(t *testing.T) {
	var requests atomic.Int32
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), body...))
		mu.Unlock()

		if requests.Add(1) == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
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

	events, err := provider.Stream(context.Background(), model.Request{
		Model:    "glm-5.2",
		Messages: []model.Message{{Role: model.MessageRoleUser, Content: "retry me"}},
	})
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
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("len(bodies) = %d, want 2", len(bodies))
	}
	if string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("retry body changed:\nfirst: %s\nsecond: %s", bodies[0], bodies[1])
	}
}

func TestProviderStreamRetries5xxThenReturnsRedactedError(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "upstream failed for Authorization: Bearer http-secret-value", http.StatusInternalServerError)
	}))
	defer server.Close()

	provider, err := NewProvider(ProviderConfig{
		BaseURL:    server.URL,
		APIKey:     "http-secret-value",
		HTTPClient: server.Client(),
		HTTPOptions: httpstream.Options{
			RequestTimeout:    time.Second,
			StreamIdleTimeout: time.Second,
			MaxRetryAttempts:  2,
			RetryBackoff:      time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{Model: "glm-5.2"})
	if err == nil {
		t.Fatalf("Stream() error = nil, want HTTP error")
	}
	if events != nil {
		t.Fatalf("events = %#v, want nil", events)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	message := err.Error()
	for _, want := range []string{"500 Internal Server Error", "after 2 attempts", "upstream failed"} {
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

func TestProviderStreamDoesNotRetry400(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
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

	events, err := provider.Stream(context.Background(), model.Request{Model: "glm-5.2"})
	if err == nil {
		t.Fatalf("Stream() error = nil, want HTTP error")
	}
	if events != nil {
		t.Fatalf("events = %#v, want nil", events)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want no retry for 400", got)
	}
	if got := err.Error(); !strings.Contains(got, "400 Bad Request") {
		t.Fatalf("Stream() error = %q, want 400 status", got)
	}
}

func TestProviderStreamRequiresAPIKey(t *testing.T) {
	provider, err := NewProvider(ProviderConfig{
		BaseURL: "http://127.0.0.1:1/v1",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{Model: "glm-5.2"})
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

func nextEvent(t *testing.T, events <-chan model.Event) model.Event {
	t.Helper()

	select {
	case event, ok := <-events:
		if !ok {
			t.Fatalf("events closed, want event")
		}
		return event
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for stream event")
	}
	return nil
}

func assertChannelClosed(t *testing.T, events <-chan model.Event) {
	t.Helper()

	select {
	case event, ok := <-events:
		if ok {
			t.Fatalf("event = %#v, want closed channel", event)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for stream close")
	}
}
