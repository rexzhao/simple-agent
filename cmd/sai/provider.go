package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/codexauth"
	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/model/anthropic_messages"
	"github.com/rexzhao/simple-agent/model/httpstream"
	"github.com/rexzhao/simple-agent/model/openai_chat"
	"github.com/rexzhao/simple-agent/model/openai_responses"
)

// newProvider builds the transport provider for a resolved model. This is
// startup-time assembly: it lives in the command, not in the agent, so the
// agent package stays free of config dependencies.
func newProvider(resolved config.ResolvedModel) (model.Provider, error) {
	provider := resolved.Provider
	httpOptions, err := providerHTTPOptions(provider)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", resolved.ProviderName, err)
	}
	httpClient, err := providerHTTPClient(provider)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", resolved.ProviderName, err)
	}
	switch resolved.Type {
	case config.ProviderTypeOpenAIChat:
		return openaichat.NewProvider(openaichat.ProviderConfig{
			BaseURL:       provider.BaseURL,
			APIKey:        provider.ResolvedAPIKey,
			Compatibility: resolved.Compatibility,
			HTTPClient:    httpClient,
			HTTPOptions:   httpOptions,
		})
	case config.ProviderTypeOpenAIResponses:
		return openairesponses.NewProvider(openairesponses.ProviderConfig{
			BaseURL:     provider.BaseURL,
			APIKey:      provider.ResolvedAPIKey,
			HTTPClient:  httpClient,
			HTTPOptions: httpOptions,
		})
	case config.ProviderTypeOpenAICodex:
		return openairesponses.NewProvider(openairesponses.ProviderConfig{
			BaseURL:             provider.BaseURL,
			ForceStoreFalse:     true,
			OmitMaxOutputTokens: true,
			HTTPClient:          httpClient,
			HTTPOptions:         httpOptions,
			TokenSource: codexTokenSource{
				source: &codexauth.TokenSource{
					Store:      codexauth.Store{Path: provider.AuthFile},
					HTTPClient: httpClient,
				},
			},
		})
	case config.ProviderTypeAnthropicMessages:
		return anthropicmessages.NewProvider(anthropicmessages.ProviderConfig{
			BaseURL:     provider.BaseURL,
			APIKey:      provider.ResolvedAPIKey,
			HTTPClient:  httpClient,
			HTTPOptions: httpOptions,
		})
	default:
		return nil, fmt.Errorf("unsupported model type %q for provider %q", resolved.Type, resolved.ProviderName)
	}
}

type codexTokenSource struct {
	source *codexauth.TokenSource
}

func (s codexTokenSource) AccessToken(ctx context.Context) (openairesponses.AccessToken, error) {
	token, err := s.source.AccessToken(ctx)
	if err != nil {
		return openairesponses.AccessToken{}, err
	}
	return openairesponses.AccessToken{Token: token.Token, AccountID: token.AccountID}, nil
}

func providerHTTPOptions(provider config.ProviderConfig) (httpstream.Options, error) {
	options := httpstream.Options{MaxRetryAttempts: 1}
	if provider.RequestTimeout == "" {
		return options, nil
	}
	requestTimeout, err := time.ParseDuration(provider.RequestTimeout)
	if err != nil || requestTimeout <= 0 {
		return httpstream.Options{}, fmt.Errorf("request_timeout must be a positive duration")
	}
	options.RequestTimeout = requestTimeout
	return options, nil
}

// providerHTTPClient returns nil for the default transport unless the
// provider configures a proxy.
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
	proxied := defaultTransport.Clone()
	proxied.Proxy = func(request *http.Request) (*url.URL, error) {
		switch strings.ToLower(request.URL.Scheme) {
		case "http":
			return httpProxy, nil
		case "https":
			return httpsProxy, nil
		default:
			return nil, nil
		}
	}
	return &http.Client{Transport: proxied}, nil
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
