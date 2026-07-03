package openairesponses

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
	BaseURL     string
	APIKey      string
	TokenSource TokenSource
	HTTPClient  *http.Client
	HTTPOptions httpstream.Options
}

type TokenSource interface {
	AccessToken(ctx context.Context) (AccessToken, error)
}

type AccessToken struct {
	Token     string
	AccountID string
}

type Provider struct {
	baseURL     string
	apiKey      string
	tokenSource TokenSource
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
		tokenSource: config.TokenSource,
		httpClient:  httpClient,
		httpOptions: config.HTTPOptions,
	}, nil
}

func (p *Provider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	token, err := p.authToken(ctx)
	if err != nil {
		return nil, err
	}

	body, toolNames, err := buildRequestBody(request, true)
	if err != nil {
		return nil, fmt.Errorf("build OpenAI Responses request body: %w", err)
	}

	response, err := httpstream.DoRequest(ctx, p.httpClient, p.httpOptions, func(requestCtx context.Context) (*http.Request, error) {
		httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create OpenAI Responses request: %w", err)
		}
		httpRequest.Header.Set("Authorization", "Bearer "+token.Token)
		if token.AccountID != "" {
			httpRequest.Header.Set("ChatGPT-Account-Id", token.AccountID)
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		return httpRequest, nil
	}, func(body io.Reader) string {
		return httpstream.ReadErrorBody(body, token.Token)
	})
	if err != nil {
		if _, ok := err.(*httpstream.StatusError); ok {
			return nil, fmt.Errorf("OpenAI Responses request failed: %w", err)
		}
		return nil, fmt.Errorf("send OpenAI Responses request: %w", err)
	}

	events := make(chan model.Event, 1)
	go func() {
		defer close(events)
		defer response.Body.Close()
		streamResponseEvents(ctx, response.Body, p.httpOptions.WithDefaults().StreamIdleTimeout, events, toolNames)
	}()
	return events, nil
}

func (p *Provider) authToken(ctx context.Context) (AccessToken, error) {
	if p.tokenSource != nil {
		token, err := p.tokenSource.AccessToken(ctx)
		if err != nil {
			return AccessToken{}, err
		}
		if strings.TrimSpace(token.Token) == "" {
			return AccessToken{}, fmt.Errorf("access token is required")
		}
		return token, nil
	}
	if p.apiKey != "" {
		return AccessToken{Token: p.apiKey}, nil
	}
	return AccessToken{}, fmt.Errorf("API key is required")
}

func streamResponseEvents(ctx context.Context, body io.Reader, idleTimeout time.Duration, events chan<- model.Event, toolNames *toolNameMapper) {
	decoder := newStreamEventDecoder(toolNames)
	err := httpstream.ReadSSEFrames(ctx, body, idleTimeout, func(frame []byte) bool {
		return emitSSEFrame(decoder, frame, events)
	})
	if err != nil {
		sendStreamError(events, err, "OpenAI Responses")
	}
}

func emitSSEFrame(decoder *streamEventDecoder, frame []byte, events chan<- model.Event) bool {
	if len(frame) == 0 {
		return false
	}

	chunkEvents, done, err := decoder.EventsFromSSE(frame)
	if err != nil {
		events <- model.ErrorEvent{Err: err, Message: "parse OpenAI Responses stream"}
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
