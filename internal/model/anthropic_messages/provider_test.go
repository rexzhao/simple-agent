package anthropicmessages

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

func TestProviderStreamPostsMessagesRequestAndEmitsText(t *testing.T) {
	requests := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
		}
		requests <- capturedRequest{
			Method:           r.Method,
			Path:             r.URL.Path,
			XAPIKey:          r.Header.Get("x-api-key"),
			AnthropicVersion: r.Header.Get("anthropic-version"),
			ContentType:      r.Header.Get("Content-Type"),
			Body:             body,
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
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
		Model: "claude-sonnet-5",
		Messages: []model.Message{
			{Role: model.MessageRoleSystem, Content: "Be concise."},
			{Role: model.MessageRoleUser, Content: "Hello"},
		},
		Parameters: map[string]any{
			"max_tokens": 1024,
		},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	got := collectEvents(t, events)
	if len(got) != 1 {
		t.Fatalf("len(events) = %d, want 1: %#v", len(got), got)
	}
	text, ok := got[0].(model.TextDeltaEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.TextDeltaEvent", got[0])
	}
	if text.Text != "hello" {
		t.Fatalf("Text = %q, want hello", text.Text)
	}

	gotRequest := <-requests
	if gotRequest.Method != http.MethodPost {
		t.Fatalf("method = %q, want %q", gotRequest.Method, http.MethodPost)
	}
	if gotRequest.Path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", gotRequest.Path)
	}
	if gotRequest.XAPIKey != "test-key" {
		t.Fatalf("x-api-key = %q, want test key", gotRequest.XAPIKey)
	}
	if gotRequest.AnthropicVersion != anthropicVersion {
		t.Fatalf("anthropic-version = %q, want %q", gotRequest.AnthropicVersion, anthropicVersion)
	}
	if gotRequest.ContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotRequest.ContentType)
	}
	assertJSONEqual(t, gotRequest.Body, `{
		"model": "claude-sonnet-5",
		"system": "Be concise.",
		"messages": [
			{"role": "user", "content": "Hello"}
		],
		"stream": true,
		"max_tokens": 1024
	}`)
}

func TestProviderStreamReturnsUsefulHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited for x-api-key: http-secret-value", http.StatusTooManyRequests)
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

	events, err := provider.Stream(context.Background(), model.Request{Model: "claude-sonnet-5"})
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
	if strings.Contains(message, "http-secret-value") {
		t.Fatalf("error leaked API key: %s", message)
	}
}

func TestProviderStreamRequiresAPIKey(t *testing.T) {
	provider, err := NewProvider(ProviderConfig{
		BaseURL: "http://127.0.0.1:1/v1",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	events, err := provider.Stream(context.Background(), model.Request{Model: "claude-sonnet-5"})
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
	Method           string
	Path             string
	XAPIKey          string
	AnthropicVersion string
	ContentType      string
	Body             []byte
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
