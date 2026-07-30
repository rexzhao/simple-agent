package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

// IsRetryableProviderError reports whether err is a transient provider or
// transport failure that is worth retrying at the session level. It only
// classifies; callers decide whether a retry is allowed (attempt budget,
// progress made, context state).
//
// Retryable: HTTP 408/429/5xx, request and stream-idle timeouts, a narrow
// set of connection-level transport errors, and in-stream provider errors
// carrying server_error/overloaded/rate_limit markers.
//
// Not retryable: context cancellation/deadline, other 4xx statuses,
// authentication failures, TLS/certificate/DNS permanent failures, and
// anything unrecognized.
func IsRetryableProviderError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr *httpstream.StatusError
	if errors.As(err, &statusErr) {
		return httpstream.IsRetryableStatus(statusErr.StatusCode)
	}
	var timeoutErr *httpstream.RequestTimeoutError
	if errors.As(err, &timeoutErr) {
		return true
	}
	var idleErr *httpstream.StreamIdleTimeoutError
	if errors.As(err, &idleErr) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || isRetryableConnError(err) {
		return true
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		message := strings.ToLower(providerErr.Message)
		return strings.Contains(message, "server_error") ||
			strings.Contains(message, "overloaded") ||
			strings.Contains(message, "rate_limit")
	}
	return false
}

// RetryReason returns a short machine-readable reason classifying a
// retryable provider error. It is used for ProviderRetryEvent.Reason.
func RetryReason(err error) string {
	var statusErr *httpstream.StatusError
	if errors.As(err, &statusErr) {
		if statusErr.StatusCode == http.StatusTooManyRequests {
			return "rate_limited"
		}
		return "server_error"
	}
	var timeoutErr *httpstream.RequestTimeoutError
	if errors.As(err, &timeoutErr) {
		return "timeout"
	}
	var idleErr *httpstream.StreamIdleTimeoutError
	if errors.As(err, &idleErr) {
		return "timeout"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || isRetryableConnError(err) {
		return "transport"
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) && strings.Contains(strings.ToLower(providerErr.Message), "rate_limit") {
		return "rate_limited"
	}
	return "server_error"
}

// isRetryableConnError reports whether err wraps one of the platform-specific
// connection-level errnos listed in retryableConnErrnos.
func isRetryableConnError(err error) bool {
	for _, errno := range retryableConnErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
