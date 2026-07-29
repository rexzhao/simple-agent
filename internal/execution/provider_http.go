package execution

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/rexzhao/simple-agent/internal/config"
)

// providerConcurrencyLimiters holds the process-wide request limiter shared by
// every HTTP client built for the same provider name. Sessions, subagent runs,
// and compaction summaries each construct their own client, so the cap has to
// live outside any single client to cover all of them.
var providerConcurrencyLimiters = struct {
	sync.Mutex
	byName map[string]*providerConcurrencyLimiter
}{byName: make(map[string]*providerConcurrencyLimiter)}

type providerConcurrencyLimiter struct {
	limit int
	slots chan struct{}
}

func (l *providerConcurrencyLimiter) release() {
	<-l.slots
}

// concurrencyLimiterForProvider returns the limiter for the named provider,
// replacing it when the configured limit changes. Clients built earlier keep
// the limiter they were constructed with, so in-flight runs are never
// retroactively re-capped.
func concurrencyLimiterForProvider(name string, limit int) *providerConcurrencyLimiter {
	providerConcurrencyLimiters.Lock()
	defer providerConcurrencyLimiters.Unlock()
	limiter, ok := providerConcurrencyLimiters.byName[name]
	if !ok || limiter.limit != limit {
		limiter = &providerConcurrencyLimiter{limit: limit, slots: make(chan struct{}, limit)}
		providerConcurrencyLimiters.byName[name] = limiter
	}
	return limiter
}

// concurrencyLimitingTransport queues requests once the provider's in-flight
// budget is exhausted. A request holds its slot until the response body is
// closed, so a streaming response stays counted for its full lifetime.
type concurrencyLimitingTransport struct {
	base    http.RoundTripper
	limiter *providerConcurrencyLimiter
}

func (t *concurrencyLimitingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	select {
	case t.limiter.slots <- struct{}{}:
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		t.limiter.release()
		return nil, err
	}
	response.Body = &slotReleasingBody{ReadCloser: response.Body, release: t.limiter.release}
	return response, nil
}

type slotReleasingBody struct {
	io.ReadCloser
	releaseOnce sync.Once
	release     func()
}

func (b *slotReleasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.releaseOnce.Do(b.release)
	return err
}

// providerHTTPClient returns nil when the provider has no proxy or concurrency
// override so callers retain the standard transport, including environment
// proxy support.
func providerHTTPClient(provider config.ProviderConfig) (*http.Client, error) {
	httpProxy, err := parseProviderProxyURL("http_proxy", provider.HTTPProxy)
	if err != nil {
		return nil, err
	}
	httpsProxy, err := parseProviderProxyURL("https_proxy", provider.HTTPSProxy)
	if err != nil {
		return nil, err
	}

	var limiter *providerConcurrencyLimiter
	if provider.MaxConcurrentRequests > 0 {
		limiter = concurrencyLimiterForProvider(provider.Name, provider.MaxConcurrentRequests)
	}
	if httpProxy == nil && httpsProxy == nil && limiter == nil {
		return nil, nil
	}

	var transport http.RoundTripper = http.DefaultTransport
	if httpProxy != nil || httpsProxy != nil {
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("default HTTP transport does not support provider proxy configuration")
		}
		proxiedTransport := defaultTransport.Clone()
		proxiedTransport.Proxy = func(request *http.Request) (*url.URL, error) {
			switch strings.ToLower(request.URL.Scheme) {
			case "http":
				return httpProxy, nil
			case "https":
				return httpsProxy, nil
			default:
				return nil, nil
			}
		}
		transport = proxiedTransport
	}
	if limiter != nil {
		transport = &concurrencyLimitingTransport{base: transport, limiter: limiter}
	}
	return &http.Client{Transport: transport}, nil
}

func parseProviderProxyURL(field, value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	proxyURL, err := url.Parse(value)
	if err != nil || proxyURL.Host == "" {
		return nil, fmt.Errorf("%s must be an absolute HTTP or HTTPS URL", field)
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https":
		return proxyURL, nil
	default:
		return nil, fmt.Errorf("%s must be an absolute HTTP or HTTPS URL", field)
	}
}
