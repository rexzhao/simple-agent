package httpstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoRequestRetriesRequestTimeoutOnce(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)

	response, err := DoRequest(context.Background(), server.Client(), Options{
		RequestTimeout:    20 * time.Millisecond,
		StreamIdleTimeout: time.Second,
		MaxRetryAttempts:  1,
		RetryBackoff:      time.Millisecond,
	}, func(requestCtx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(requestCtx, http.MethodPost, server.URL, strings.NewReader("body"))
	}, func(body io.Reader) string {
		return ReadErrorBody(body, "")
	})
	if err != nil {
		t.Fatalf("DoRequest() error = %v", err)
	}
	defer response.Body.Close()
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want initial request plus one retry", got)
	}
}

func TestDoRequestReportsBothRequestTimeoutAttempts(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	response, err := DoRequest(context.Background(), server.Client(), Options{
		RequestTimeout:    20 * time.Millisecond,
		StreamIdleTimeout: time.Second,
		MaxRetryAttempts:  1,
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
	var timeoutErr *RequestTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("DoRequest() error = %T(%v), want *RequestTimeoutError", err, err)
	}
	if timeoutErr.Attempts != 2 {
		t.Fatalf("RequestTimeoutError.Attempts = %d, want 2", timeoutErr.Attempts)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

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

func TestIsRetryableStatus(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{599, true},
		{600, false},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
		{http.StatusOK, false},
	}
	for _, test := range tests {
		if got := IsRetryableStatus(test.code); got != test.want {
			t.Fatalf("IsRetryableStatus(%d) = %v, want %v", test.code, got, test.want)
		}
	}
}

func TestDoRequestDoesNotRetryStatusWhenMaxAttemptsIsOne(t *testing.T) {
	requests := make(chan struct{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "server error")
	}))
	defer server.Close()

	response, err := DoRequest(context.Background(), server.Client(), Options{
		RequestTimeout:    time.Second,
		StreamIdleTimeout: time.Second,
		MaxRetryAttempts:  1,
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
	if statusErr.StatusCode != 500 || statusErr.Attempts != 1 {
		t.Fatalf("StatusError = %#v, want status 500 after 1 attempt", statusErr)
	}
	if got := len(requests); got != 1 {
		t.Fatalf("requests = %d, want exactly 1 (status retry disabled)", got)
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
