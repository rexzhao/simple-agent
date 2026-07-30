package model

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

func TestIsRetryableProviderError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"wrapped context canceled", fmt.Errorf("send request: %w", context.Canceled), false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"status 500", &httpstream.StatusError{StatusCode: 500, Status: "500 Internal Server Error"}, true},
		{"status 503", &httpstream.StatusError{StatusCode: 503}, true},
		{"status 429", &httpstream.StatusError{StatusCode: 429}, true},
		{"status 408", &httpstream.StatusError{StatusCode: 408}, true},
		{"wrapped status 500", fmt.Errorf("OpenAI Responses request failed: %w", &httpstream.StatusError{StatusCode: 500}), true},
		{"status 400", &httpstream.StatusError{StatusCode: 400}, false},
		{"status 401", &httpstream.StatusError{StatusCode: 401}, false},
		{"status 404", &httpstream.StatusError{StatusCode: 404}, false},
		{"request timeout", &httpstream.RequestTimeoutError{Timeout: 15 * time.Second}, true},
		{"stream idle timeout", &httpstream.StreamIdleTimeoutError{Timeout: 2 * time.Minute}, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"wrapped unexpected EOF", &url.Error{Op: "Post", Err: io.ErrUnexpectedEOF}, true},
		{"bare EOF", io.EOF, false},
		{"provider server_error", &ProviderError{Message: "server_error: An error occurred (code server_error)"}, true},
		{"provider overloaded", &ProviderError{Message: "overloaded_error: Overloaded"}, true},
		{"provider rate_limit", &ProviderError{Message: "rate_limit_error: slow down (code rate_limited)"}, true},
		{"provider other", &ProviderError{Message: "invalid_request: bad input"}, false},
		{"tls unknown authority", &url.Error{Op: "Post", Err: x509.UnknownAuthorityError{}}, false},
		{"op error non-errno", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("tls: handshake failure")}, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsRetryableProviderError(test.err); got != test.want {
				t.Fatalf("IsRetryableProviderError(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestRetryReason(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"status 429", &httpstream.StatusError{StatusCode: 429}, "rate_limited"},
		{"status 500", &httpstream.StatusError{StatusCode: 500}, "server_error"},
		{"status 408", &httpstream.StatusError{StatusCode: 408}, "server_error"},
		{"request timeout", &httpstream.RequestTimeoutError{Timeout: time.Second}, "timeout"},
		{"stream idle timeout", &httpstream.StreamIdleTimeoutError{Timeout: time.Second}, "timeout"},
		{"unexpected EOF", io.ErrUnexpectedEOF, "transport"},
		{"provider rate_limit", &ProviderError{Message: "rate_limit_error: slow down"}, "rate_limited"},
		{"provider server_error", &ProviderError{Message: "server_error: boom"}, "server_error"},
		{"provider overloaded", &ProviderError{Message: "overloaded_error: Overloaded"}, "server_error"},
		{"unknown defaults to server_error", errors.New("boom"), "server_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RetryReason(test.err); got != test.want {
				t.Fatalf("RetryReason(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}

func TestIsRetryableProviderErrorConnErrno(t *testing.T) {
	if len(retryableConnErrnos) == 0 {
		t.Fatal("retryableConnErrnos is empty")
	}
	wrapped := &url.Error{Op: "Post", Err: &net.OpError{Op: "read", Net: "tcp",
		Err: fmt.Errorf("wsarecv: %w", retryableConnErrnos[0])}}
	if !IsRetryableProviderError(wrapped) {
		t.Fatalf("IsRetryableProviderError(%v) = false, want true for platform conn errno", wrapped)
	}
	if got := RetryReason(wrapped); got != "transport" {
		t.Fatalf("RetryReason(%v) = %q, want transport", wrapped, got)
	}
}
