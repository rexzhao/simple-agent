package execution

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rexzhao/simple-agent/internal/config"
)

// providerHTTPClient returns nil when the provider has no proxy override so
// callers retain the standard transport, including environment proxy support.
func providerHTTPClient(provider config.ProviderConfig) (*http.Client, error) {
	httpProxy, err := parseProviderProxyURL("http_proxy", provider.HTTPProxy)
	if err != nil {
		return nil, err
	}
	httpsProxy, err := parseProviderProxyURL("https_proxy", provider.HTTPSProxy)
	if err != nil {
		return nil, err
	}
	if httpProxy == nil && httpsProxy == nil {
		return nil, nil
	}

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default HTTP transport does not support provider proxy configuration")
	}
	transport := defaultTransport.Clone()
	transport.Proxy = func(request *http.Request) (*url.URL, error) {
		switch strings.ToLower(request.URL.Scheme) {
		case "http":
			return httpProxy, nil
		case "https":
			return httpsProxy, nil
		default:
			return nil, nil
		}
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
