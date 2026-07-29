package execution

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestProviderHTTPClientWithConcurrencyLimitBuildsClient(t *testing.T) {
	client, err := providerHTTPClient(config.ProviderConfig{Name: "limited-no-proxy", MaxConcurrentRequests: 2})
	if err != nil {
		t.Fatalf("providerHTTPClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("providerHTTPClient() = nil, want a client when max_concurrent_requests is set")
	}
	if _, ok := client.Transport.(*concurrencyLimitingTransport); !ok {
		t.Fatalf("client transport = %T, want *concurrencyLimitingTransport", client.Transport)
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

func TestProviderHTTPClientCapsInFlightRequests(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	server := httptest.NewServer(blockingProviderHandler(entered, release))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	client, err := providerHTTPClient(config.ProviderConfig{Name: "cap-in-flight", MaxConcurrentRequests: 2})
	if err != nil {
		t.Fatalf("providerHTTPClient() error = %v", err)
	}

	done := make(chan error, 3)
	for range 3 {
		go func() {
			done <- runSimpleGet(client, server.URL)
		}()
	}

	waitForHandlerEntries(t, entered, 2)
	select {
	case <-entered:
		t.Fatal("third request reached the server while two requests hold the limit")
	case <-time.After(300 * time.Millisecond):
	}

	// Completing one request frees its slot only after the body is closed, so
	// the queued third request can now enter the handler.
	release <- struct{}{}
	waitForHandlerEntries(t, entered, 1)

	release <- struct{}{}
	release <- struct{}{}
	for range 3 {
		if err := <-done; err != nil {
			t.Fatalf("request error = %v", err)
		}
	}
}

func TestProviderHTTPClientSharesLimitAcrossClients(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(blockingProviderHandler(entered, release))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	providerConfig := config.ProviderConfig{Name: "shared-across-clients", MaxConcurrentRequests: 1}
	firstClient, err := providerHTTPClient(providerConfig)
	if err != nil {
		t.Fatalf("providerHTTPClient() error = %v", err)
	}
	secondClient, err := providerHTTPClient(providerConfig)
	if err != nil {
		t.Fatalf("providerHTTPClient() error = %v", err)
	}
	if firstClient == secondClient {
		t.Fatal("providerHTTPClient() returned the same client twice, want independent clients")
	}

	first := make(chan error, 1)
	go func() {
		first <- runSimpleGet(firstClient, server.URL)
	}()
	waitForHandlerEntries(t, entered, 1)

	second := make(chan error, 1)
	go func() {
		second <- runSimpleGet(secondClient, server.URL)
	}()
	select {
	case <-entered:
		t.Fatal("request from a second client bypassed the shared provider limit")
	case <-time.After(300 * time.Millisecond):
	}

	release <- struct{}{}
	waitForHandlerEntries(t, entered, 1)
	release <- struct{}{}
	for name, result := range map[string]chan error{"first": first, "second": second} {
		if err := <-result; err != nil {
			t.Fatalf("%s request error = %v", name, err)
		}
	}
}

func TestProviderHTTPClientQueuedRequestRespectsCancellation(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(blockingProviderHandler(entered, release))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	client, err := providerHTTPClient(config.ProviderConfig{Name: "queued-cancel", MaxConcurrentRequests: 1})
	if err != nil {
		t.Fatalf("providerHTTPClient() error = %v", err)
	}

	holding := make(chan error, 1)
	go func() {
		holding <- runSimpleGet(client, server.URL)
	}()
	waitForHandlerEntries(t, entered, 1)

	queuedCtx, stopQueued := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() {
		request, err := http.NewRequestWithContext(queuedCtx, http.MethodGet, server.URL, nil)
		if err != nil {
			queued <- err
			return
		}
		response, err := client.Do(request)
		if err != nil {
			queued <- err
			return
		}
		_ = response.Body.Close()
		queued <- nil
	}()

	// Give the queued request a moment to block on the limiter, then cancel it.
	time.Sleep(100 * time.Millisecond)
	stopQueued()
	select {
	case err := <-queued:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("queued request error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request did not observe context cancellation")
	}

	release <- struct{}{}
	if err := <-holding; err != nil {
		t.Fatalf("holding request error = %v", err)
	}
}

// blockingProviderHandler signals each arriving request on entered, then holds
// it open until release receives (or the request is canceled). Tests close
// release during cleanup so a failed assertion can never wedge server.Close.
func blockingProviderHandler(entered chan<- struct{}, release <-chan struct{}) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
		}
		_, _ = io.WriteString(response, "ok")
	})
}

func runSimpleGet(client *http.Client, url string) error {
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return response.Body.Close()
}

func waitForHandlerEntries(t *testing.T, entered <-chan struct{}, count int) {
	t.Helper()
	for range count {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for request to reach the server")
		}
	}
}
