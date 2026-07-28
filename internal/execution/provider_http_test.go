package execution

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rexzhao/simple-agent/internal/config"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

func TestProviderHTTPClientSelectsProxyByRequestScheme(t *testing.T) {
	client, err := providerHTTPClient(config.ProviderConfig{
		HTTPProxy:  "http://127.0.0.1:8080",
		HTTPSProxy: "https://proxy.example.test:8443",
	})
	if err != nil {
		t.Fatalf("providerHTTPClient() error = %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", client.Transport)
	}

	tests := []struct {
		requestURL string
		wantProxy  string
	}{
		{requestURL: "http://api.example.test/v1", wantProxy: "http://127.0.0.1:8080"},
		{requestURL: "https://api.example.test/v1", wantProxy: "https://proxy.example.test:8443"},
		{requestURL: "ftp://api.example.test/file", wantProxy: ""},
	}
	for _, test := range tests {
		request, err := http.NewRequest(http.MethodGet, test.requestURL, nil)
		if err != nil {
			t.Fatalf("NewRequest(%q) error = %v", test.requestURL, err)
		}
		proxyURL, err := transport.Proxy(request)
		if err != nil {
			t.Fatalf("Proxy(%q) error = %v", test.requestURL, err)
		}
		got := ""
		if proxyURL != nil {
			got = proxyURL.String()
		}
		if got != test.wantProxy {
			t.Fatalf("Proxy(%q) = %q, want %q", test.requestURL, got, test.wantProxy)
		}
	}
}

func TestProviderHTTPClientWithoutOverrideUsesDefaultClient(t *testing.T) {
	client, err := providerHTTPClient(config.ProviderConfig{})
	if err != nil {
		t.Fatalf("providerHTTPClient() error = %v", err)
	}
	if client != nil {
		t.Fatalf("providerHTTPClient() = %#v, want nil without a provider override", client)
	}
}

func TestProviderHTTPClientRejectsInvalidProxyURL(t *testing.T) {
	_, err := providerHTTPClient(config.ProviderConfig{HTTPSProxy: "127.0.0.1:8080"})
	if err == nil {
		t.Fatal("providerHTTPClient() error = nil, want invalid URL error")
	}
}

func TestProviderRunUsesConfiguredHTTPProxy(t *testing.T) {
	requests := make(chan *http.Request, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests <- request.Clone(request.Context())
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: [DONE]\n\n")
	}))
	defer proxy.Close()

	provider, err := newProviderForRun("proxied", config.ProviderTypeOpenAIChat, "", config.ProviderConfig{
		BaseURL:        "http://provider.example.test/v1",
		ResolvedAPIKey: "test-key",
		HTTPProxy:      proxy.URL,
	}, nil)
	if err != nil {
		t.Fatalf("newProviderForRun() error = %v", err)
	}
	events, err := provider.Stream(context.Background(), model.Request{Model: "test-model"})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for event := range events {
		if failure, ok := event.(model.ErrorEvent); ok {
			t.Fatalf("Stream() emitted error = %v", failure.Err)
		}
	}

	request := <-requests
	if request.Method != http.MethodPost || request.URL.String() != "http://provider.example.test/v1/chat/completions" {
		t.Fatalf("proxy request = %s %s, want POST provider chat completions URL", request.Method, request.URL)
	}
}

func TestProviderAdapterConfigsUseSharedHTTPClient(t *testing.T) {
	client := &http.Client{}
	provider := config.ProviderConfig{BaseURL: "https://provider.example.test/v1", ResolvedAPIKey: "test-key"}
	options := httpstream.Options{}

	clients := map[string]*http.Client{
		config.ProviderTypeOpenAIChat:        openAIChatProviderConfig(provider, "", client, options).HTTPClient,
		config.ProviderTypeOpenAIResponses:   openAIResponsesProviderConfig(provider, client, options).HTTPClient,
		config.ProviderTypeOpenAICodex:       openAICodexProviderConfig(provider, client, options).HTTPClient,
		config.ProviderTypeAnthropicMessages: anthropicMessagesProviderConfig(provider, client, options).HTTPClient,
	}
	for modelType, configuredClient := range clients {
		if configuredClient != client {
			t.Fatalf("%s HTTP client = %#v, want shared provider client %#v", modelType, configuredClient, client)
		}
	}

	codexConfig := openAICodexProviderConfig(provider, client, options)
	tokenSource, ok := codexConfig.TokenSource.(codexResponsesTokenSource)
	if !ok || tokenSource.source.HTTPClient != client {
		t.Fatalf("Codex token source does not use the shared provider HTTP client")
	}
}
