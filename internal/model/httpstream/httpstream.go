package httpstream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultRequestTimeout    = 15 * time.Second
	DefaultStreamIdleTimeout = 2 * time.Minute
	DefaultMaxRetryAttempts  = 3
	DefaultRetryBackoff      = 200 * time.Millisecond
	DefaultTimeoutRetries    = 1
)

type Options struct {
	RequestTimeout    time.Duration
	StreamIdleTimeout time.Duration
	MaxRetryAttempts  int
	RetryBackoff      time.Duration
}

func (o Options) WithDefaults() Options {
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultRequestTimeout
	}
	if o.StreamIdleTimeout <= 0 {
		o.StreamIdleTimeout = DefaultStreamIdleTimeout
	}
	if o.MaxRetryAttempts <= 0 {
		o.MaxRetryAttempts = DefaultMaxRetryAttempts
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = DefaultRetryBackoff
	}
	return o
}

type StatusError struct {
	StatusCode int
	Status     string
	Body       string
	Attempts   int
}

func (e *StatusError) Error() string {
	if e.Attempts > 1 {
		return fmt.Sprintf("%s after %d attempts: %s", e.Status, e.Attempts, e.Body)
	}
	return fmt.Sprintf("%s: %s", e.Status, e.Body)
}

type RequestTimeoutError struct {
	Timeout  time.Duration
	Attempts int
}

func (e *RequestTimeoutError) Error() string {
	if e.Attempts > 1 {
		return fmt.Sprintf("request timeout waiting for response headers after %d attempts of %s each", e.Attempts, e.Timeout)
	}
	return fmt.Sprintf("request timeout waiting for response headers after %s", e.Timeout)
}

type StreamIdleTimeoutError struct {
	Timeout time.Duration
}

func (e *StreamIdleTimeoutError) Error() string {
	return fmt.Sprintf("SSE stream idle timeout after %s without data", e.Timeout)
}

func IsStreamIdleTimeout(err error) bool {
	var timeoutErr *StreamIdleTimeoutError
	return errors.As(err, &timeoutErr)
}

func DoRequest(ctx context.Context, client *http.Client, options Options, newRequest func(context.Context) (*http.Request, error), readErrorBody func(io.Reader) string) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	client = streamingClient(client)
	options = options.WithDefaults()

	var lastStatusErr *StatusError
	for attempt := 1; attempt <= options.MaxRetryAttempts; attempt++ {
		response, err := doRequestWithTimeoutRetry(ctx, client, options, newRequest)
		if err != nil {
			return nil, err
		}
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			return response, nil
		}

		statusErr := &StatusError{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Body:       readErrorBody(response.Body),
			Attempts:   attempt,
		}
		_ = response.Body.Close()
		lastStatusErr = statusErr

		if !isRetryableStatus(response.StatusCode) || attempt == options.MaxRetryAttempts {
			return nil, statusErr
		}
		if err := sleep(ctx, options.RetryBackoff); err != nil {
			return nil, err
		}
	}
	return nil, lastStatusErr
}

func doRequestWithTimeoutRetry(ctx context.Context, client *http.Client, options Options, newRequest func(context.Context) (*http.Request, error)) (*http.Response, error) {
	for attempt := 1; attempt <= DefaultTimeoutRetries+1; attempt++ {
		response, err := doRequestOnce(ctx, client, options.RequestTimeout, newRequest)
		var timeoutErr *RequestTimeoutError
		if !errors.As(err, &timeoutErr) {
			return response, err
		}
		timeoutErr.Attempts = attempt
		if attempt > DefaultTimeoutRetries {
			return nil, timeoutErr
		}
		if err := sleep(ctx, options.RetryBackoff); err != nil {
			return nil, err
		}
	}
	panic("unreachable")
}

func ReadSSEFrames(ctx context.Context, body io.Reader, idleTimeout time.Duration, handle func([]byte) bool) error {
	if idleTimeout <= 0 {
		idleTimeout = DefaultStreamIdleTimeout
	}

	items := make(chan scanItem, 1)
	done := make(chan struct{})
	var closeDone sync.Once
	defer closeDone.Do(func() { close(done) })
	go scanLines(body, items, done)

	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()

	var frame bytes.Buffer
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return &StreamIdleTimeoutError{Timeout: idleTimeout}
		case item, ok := <-items:
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				if frame.Len() > 0 {
					handle(frame.Bytes())
				}
				return nil
			}
			resetTimer(timer, idleTimeout)
			if item.err != nil {
				return item.err
			}
			if item.done {
				if err := ctx.Err(); err != nil {
					return err
				}
				if frame.Len() > 0 {
					handle(frame.Bytes())
				}
				return nil
			}
			line := item.line
			if line == "" || line == "\r" {
				if handle(frame.Bytes()) {
					return nil
				}
				frame.Reset()
				continue
			}
			frame.WriteString(line)
			frame.WriteByte('\n')
		}
	}
}

func ReadErrorBody(body io.Reader, secret string) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return "read response body: " + err.Error()
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		return "empty response body"
	}
	if secret = strings.TrimSpace(secret); secret != "" {
		message = strings.ReplaceAll(message, secret, "<redacted>")
	}
	return message
}

type scanItem struct {
	line string
	done bool
	err  error
}

func scanLines(body io.Reader, items chan<- scanItem, done <-chan struct{}) {
	defer close(items)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		select {
		case items <- scanItem{line: scanner.Text()}:
		case <-done:
			return
		}
	}
	select {
	case items <- scanItem{done: true, err: scanner.Err()}:
	case <-done:
	}
}

func doRequestOnce(ctx context.Context, client *http.Client, timeout time.Duration, newRequest func(context.Context) (*http.Request, error)) (*http.Response, error) {
	requestCtx, cancel := context.WithCancel(ctx)
	request, err := newRequest(requestCtx)
	if err != nil {
		cancel()
		return nil, err
	}

	result := make(chan requestResult, 1)
	timeoutDone := make(chan struct{})
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		cancel()
		close(timeoutDone)
	})
	go func() {
		response, err := client.Do(request)
		result <- requestResult{response: response, err: err}
	}()

	select {
	case <-ctx.Done():
		timer.Stop()
		cancel()
		go closeResponseWhenDone(result)
		return nil, ctx.Err()
	case <-timeoutDone:
		go closeResponseWhenDone(result)
		return nil, &RequestTimeoutError{Timeout: timeout}
	case got := <-result:
		if !timer.Stop() && timedOut.Load() {
			cancel()
			if got.response != nil {
				_ = got.response.Body.Close()
			}
			return nil, &RequestTimeoutError{Timeout: timeout}
		}
		if got.err != nil {
			cancel()
			if timedOut.Load() {
				return nil, &RequestTimeoutError{Timeout: timeout}
			}
			return nil, got.err
		}
		if timedOut.Load() {
			cancel()
			if got.response != nil {
				_ = got.response.Body.Close()
			}
			return nil, &RequestTimeoutError{Timeout: timeout}
		}
		got.response.Body = &cancelOnCloseReadCloser{
			ReadCloser: got.response.Body,
			cancel:     cancel,
		}
		return got.response, nil
	}
}

type requestResult struct {
	response *http.Response
	err      error
}

type cancelOnCloseReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (r *cancelOnCloseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.cancel)
	return err
}

func closeResponseWhenDone(result <-chan requestResult) {
	got := <-result
	if got.response != nil && got.response.Body != nil {
		_ = got.response.Body.Close()
	}
}

func streamingClient(client *http.Client) *http.Client {
	copy := *client
	copy.Timeout = 0
	return &copy
}

func isRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || (statusCode >= http.StatusInternalServerError && statusCode <= 599)
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}
