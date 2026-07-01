package openaichat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
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

func TestProviderStreamReturnsUsefulHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
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
