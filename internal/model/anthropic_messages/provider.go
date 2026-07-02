package anthropicmessages

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

const anthropicVersion = "2023-06-01"

type ProviderConfig struct {
	BaseURL     string
	APIKey      string
	HTTPClient  *http.Client
	HTTPOptions httpstream.Options
}

type Provider struct {
	baseURL     string
	apiKey      string
	httpClient  *http.Client
	httpOptions httpstream.Options
}

var _ model.Provider = (*Provider)(nil)

func NewProvider(config ProviderConfig) (*Provider, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &Provider{
		baseURL:     baseURL,
		apiKey:      strings.TrimSpace(config.APIKey),
		httpClient:  httpClient,
		httpOptions: config.HTTPOptions,
	}, nil
}

func (p *Provider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	apiKey, err := p.apiKeyValue()
	if err != nil {
		return nil, err
	}

	body, toolNames, err := buildRequestBody(request, true)
	if err != nil {
		return nil, fmt.Errorf("build Anthropic Messages request body: %w", err)
	}

	response, err := httpstream.DoRequest(ctx, p.httpClient, p.httpOptions, func(requestCtx context.Context) (*http.Request, error) {
		httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create Anthropic Messages request: %w", err)
		}
		httpRequest.Header.Set("x-api-key", apiKey)
		httpRequest.Header.Set("anthropic-version", anthropicVersion)
		httpRequest.Header.Set("Content-Type", "application/json")
		return httpRequest, nil
	}, func(body io.Reader) string {
		return httpstream.ReadErrorBody(body, apiKey)
	})
	if err != nil {
		if _, ok := err.(*httpstream.StatusError); ok {
			return nil, fmt.Errorf("Anthropic Messages request failed: %w", err)
		}
		return nil, fmt.Errorf("send Anthropic Messages request: %w", err)
	}

	events := make(chan model.Event, 1)
	go func() {
		defer close(events)
		defer response.Body.Close()
		streamResponseEvents(ctx, response.Body, p.httpOptions.WithDefaults().StreamIdleTimeout, events, toolNames)
	}()
	return events, nil
}

func (p *Provider) apiKeyValue() (string, error) {
	if p.apiKey != "" {
		return p.apiKey, nil
	}
	return "", fmt.Errorf("API key is required")
}

func streamResponseEvents(ctx context.Context, body io.Reader, idleTimeout time.Duration, events chan<- model.Event, toolNames *toolNameMapper) {
	decoder := newStreamEventDecoder(toolNames)
	err := httpstream.ReadSSEFrames(ctx, body, idleTimeout, func(frame []byte) bool {
		return emitSSEFrame(decoder, frame, events)
	})
	if err != nil {
		sendStreamError(events, err, "Anthropic Messages")
	}
}

func emitSSEFrame(decoder *streamEventDecoder, frame []byte, events chan<- model.Event) bool {
	if len(frame) == 0 {
		return false
	}

	chunkEvents, done, err := decoder.EventsFromSSE(frame)
	if err != nil {
		events <- model.ErrorEvent{Err: err, Message: "parse Anthropic Messages stream"}
		return true
	}
	for _, event := range chunkEvents {
		events <- event
	}
	return done
}

func sendStreamError(events chan<- model.Event, err error, providerName string) {
	message := "read " + providerName + " stream"
	if httpstream.IsStreamIdleTimeout(err) {
		message = providerName + " stream idle timeout"
	}
	events <- model.ErrorEvent{Err: err, Message: message}
}
