package openaichat

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

type ProviderConfig struct {
	BaseURL       string
	APIKey        string
	Compatibility string
	HTTPClient    *http.Client
	HTTPOptions   httpstream.Options
	RecordRequest func(endpoint string, body []byte) error
}

type Provider struct {
	baseURL       string
	apiKey        string
	compatibility chatCompatibility
	httpClient    *http.Client
	httpOptions   httpstream.Options
	recordRequest func(endpoint string, body []byte) error
}

var _ model.Provider = (*Provider)(nil)

func NewProvider(config ProviderConfig) (*Provider, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}
	compatibility, err := resolveCompatibility(config.Compatibility)
	if err != nil {
		return nil, err
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &Provider{
		baseURL:       baseURL,
		apiKey:        strings.TrimSpace(config.APIKey),
		compatibility: compatibility,
		httpClient:    httpClient,
		httpOptions:   config.HTTPOptions,
		recordRequest: config.RecordRequest,
	}, nil
}

func (p *Provider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	apiKey, err := p.apiKeyValue()
	if err != nil {
		return nil, err
	}

	body, err := buildRequestBody(request, true, p.compatibility)
	if err != nil {
		return nil, fmt.Errorf("build OpenAI chat request body: %w", err)
	}
	if p.recordRequest != nil {
		if err := p.recordRequest("/chat/completions", body); err != nil {
			return nil, fmt.Errorf("record OpenAI chat request: %w", err)
		}
	}

	response, err := httpstream.DoRequest(ctx, p.httpClient, p.httpOptions, func(requestCtx context.Context) (*http.Request, error) {
		httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create OpenAI chat request: %w", err)
		}
		httpRequest.Header.Set("Authorization", "Bearer "+apiKey)
		httpRequest.Header.Set("Content-Type", "application/json")
		return httpRequest, nil
	}, func(body io.Reader) string {
		return httpstream.ReadErrorBody(body, apiKey)
	})
	if err != nil {
		if _, ok := err.(*httpstream.StatusError); ok {
			return nil, fmt.Errorf("OpenAI chat request failed: %w", err)
		}
		return nil, fmt.Errorf("send OpenAI chat request: %w", err)
	}

	events := make(chan model.Event, 1)
	go func() {
		defer close(events)
		defer response.Body.Close()
		streamResponseEvents(ctx, response.Body, p.httpOptions.WithDefaults().StreamIdleTimeout, p.compatibility, events, newToolNameMapper(request.Tools))
	}()
	return events, nil
}

func (p *Provider) apiKeyValue() (string, error) {
	if p.apiKey != "" {
		return p.apiKey, nil
	}
	return "", fmt.Errorf("API key is required")
}

func streamResponseEvents(ctx context.Context, body io.Reader, idleTimeout time.Duration, compatibility chatCompatibility, events chan<- model.Event, toolNames ...*toolNameMapper) {
	decoder := newStreamEventDecoderWithCompatibility(compatibility, toolNames...)
	err := httpstream.ReadSSEFrames(ctx, body, idleTimeout, func(frame []byte) bool {
		return emitSSEFrame(decoder, frame, events)
	})
	if err != nil {
		sendStreamError(events, err, "OpenAI chat")
	}
}

func emitSSEFrame(decoder *streamEventDecoder, frame []byte, events chan<- model.Event) bool {
	if len(frame) == 0 {
		return false
	}

	chunkEvents, done, err := decoder.EventsFromSSE(frame)
	if err != nil {
		events <- model.ErrorEvent{Err: err, Message: "parse OpenAI chat stream"}
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
