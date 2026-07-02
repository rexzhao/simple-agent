package httpstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDoRequestStopsRetryBackoffOnContextCancel(t *testing.T) {
	requests := make(chan struct{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		http.Error(w, "temporary failure", http.StatusInternalServerError)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		response, err := DoRequest(ctx, server.Client(), Options{
			RequestTimeout:    time.Second,
			StreamIdleTimeout: time.Second,
			MaxRetryAttempts:  3,
			RetryBackoff:      time.Hour,
		}, func(requestCtx context.Context) (*http.Request, error) {
			return http.NewRequestWithContext(requestCtx, http.MethodPost, server.URL, strings.NewReader("body"))
		}, func(body io.Reader) string {
			return ReadErrorBody(body, "")
		})
		if response != nil {
			_ = response.Body.Close()
		}
		errs <- err
	}()

	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for first request")
	}
	cancel()

	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DoRequest() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for retry backoff cancellation")
	}
	if got := len(requests); got != 0 {
		t.Fatalf("extra requests during canceled backoff = %d", got)
	}
}

func TestDoRequestDoesNotRetryStatus600(t *testing.T) {
	requests := make(chan struct{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(600)
		_, _ = io.WriteString(w, "outside 5xx")
	}))
	defer server.Close()

	response, err := DoRequest(context.Background(), server.Client(), Options{
		RequestTimeout:    time.Second,
		StreamIdleTimeout: time.Second,
		MaxRetryAttempts:  3,
		RetryBackoff:      time.Millisecond,
	}, func(requestCtx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(requestCtx, http.MethodPost, server.URL, strings.NewReader("body"))
	}, func(body io.Reader) string {
		return ReadErrorBody(body, "")
	})
	if response != nil {
		_ = response.Body.Close()
		t.Fatalf("response = %#v, want nil", response)
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("DoRequest() error = %T(%v), want *StatusError", err, err)
	}
	if statusErr.StatusCode != 600 || statusErr.Attempts != 1 {
		t.Fatalf("StatusError = %#v, want status 600 after 1 attempt", statusErr)
	}
	if got := len(requests); got != 1 {
		t.Fatalf("requests = %d, want no retry for 600", got)
	}
}
