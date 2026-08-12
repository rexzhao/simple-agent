package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/model/httpstream"
)

type ProviderConfig struct {
	BaseURL         string
	APIKey          string
	TokenSource     TokenSource
	ForceStoreFalse bool
	// OmitMaxOutputTokens disables the max_output_tokens injection for
	// backends with a strict parameter allowlist (Codex answers 400).
	OmitMaxOutputTokens bool
	HTTPClient          *http.Client
	HTTPOptions         httpstream.Options
	RecordRequest       func(endpoint string, body []byte) error
}

type TokenSource interface {
	AccessToken(ctx context.Context) (AccessToken, error)
}

type AccessToken struct {
	Token     string
	AccountID string
}

type Provider struct {
	baseURL             string
	apiKey              string
	tokenSource         TokenSource
	forceStoreFalse     bool
	omitMaxOutputTokens bool
	httpClient          *http.Client
	httpOptions         httpstream.Options
	recordRequest       func(endpoint string, body []byte) error
	turnStateMu         sync.Mutex
	turnState           string
}

var _ model.Provider = (*Provider)(nil)
var _ model.CompactionProvider = (*Provider)(nil)

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
		baseURL:             baseURL,
		apiKey:              strings.TrimSpace(config.APIKey),
		tokenSource:         config.TokenSource,
		forceStoreFalse:     config.ForceStoreFalse,
		omitMaxOutputTokens: config.OmitMaxOutputTokens,
		httpClient:          httpClient,
		httpOptions:         config.HTTPOptions,
		recordRequest:       config.RecordRequest,
	}, nil
}

func (p *Provider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	token, err := p.authToken(ctx)
	if err != nil {
		return nil, err
	}

	body, toolNames, metadata, err := buildProviderRequest(request, true, requestBodyOptions{
		forceStoreFalse:     p.forceStoreFalse,
		omitMaxOutputTokens: p.omitMaxOutputTokens,
		origin:              p.baseURL,
	})
	if err != nil {
		return nil, fmt.Errorf("build OpenAI Responses request body: %w", err)
	}

	response, err := p.doRequest(ctx, token, body, metadata)
	if shouldRetryWithoutContinuation(err, metadata) {
		body, toolNames, metadata, err = buildProviderRequest(request, true, requestBodyOptions{
			forceStoreFalse:     p.forceStoreFalse,
			omitMaxOutputTokens: p.omitMaxOutputTokens,
			origin:              p.baseURL,
			disableContinuation: true,
		})
		if err == nil {
			response, err = p.doRequest(ctx, token, body, metadata)
		}
	}
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
		streamResponseEvents(ctx, response.Body, p.httpOptions.WithDefaults().StreamIdleTimeout, events, toolNames, model.ResponseState{
			Origin: p.baseURL,
			Model:  request.Model,
			Stored: metadata.Store,
		})
	}()
	return events, nil
}

func (p *Provider) Compact(ctx context.Context, request model.Request) (model.CompactionResult, error) {
	token, err := p.authToken(ctx)
	if err != nil {
		return model.CompactionResult{}, err
	}
	body, metadata, err := buildCompactionRequestBody(request, requestBodyOptions{omitMaxOutputTokens: p.omitMaxOutputTokens, origin: p.baseURL})
	if err != nil {
		return model.CompactionResult{}, fmt.Errorf("build OpenAI Responses compact request body: %w", err)
	}
	// The compact endpoint has no session-level retry loop above it, so allow
	// one retry for transient failures. Status codes are not retried by the
	// transport layer (MaxRetryAttempts is 1 in production wiring); each
	// attempt may still perform the transport's quick timeout retry.
	const maxCompactAttempts = 2
	var response *http.Response
	for attempt := 1; attempt <= maxCompactAttempts; attempt++ {
		response, err = p.doRequestTo(ctx, token, body, metadata, "/responses/compact")
		if err == nil {
			break
		}
		if attempt < maxCompactAttempts && ctx.Err() == nil && model.IsRetryableProviderError(err) {
			if waitErr := waitForCompactRetry(ctx, compactRetryBackoff); waitErr != nil {
				return model.CompactionResult{}, waitErr
			}
			continue
		}
		if _, ok := err.(*httpstream.StatusError); ok {
			return model.CompactionResult{}, fmt.Errorf("OpenAI Responses compact request failed: %w", err)
		}
		return model.CompactionResult{}, fmt.Errorf("send OpenAI Responses compact request: %w", err)
	}
	defer response.Body.Close()

	var compacted struct {
		Object string            `json:"object"`
		Output []json.RawMessage `json:"output"`
		Usage  *responseUsage    `json:"usage"`
	}
	if err := json.NewDecoder(response.Body).Decode(&compacted); err != nil {
		return model.CompactionResult{}, fmt.Errorf("decode OpenAI Responses compact response: %w", err)
	}
	if compacted.Object != "" && compacted.Object != "response.compaction" {
		return model.CompactionResult{}, fmt.Errorf("OpenAI Responses compact response object is %q", compacted.Object)
	}
	if len(compacted.Output) == 0 {
		return model.CompactionResult{}, fmt.Errorf("OpenAI Responses compact response output is empty")
	}

	result := model.CompactionResult{Items: make([]model.ProviderItem, 0, len(compacted.Output))}
	hasCompaction := false
	for index, raw := range compacted.Output {
		var item struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &item); err != nil || strings.TrimSpace(item.Type) == "" {
			return model.CompactionResult{}, fmt.Errorf("OpenAI Responses compact output item %d is invalid", index)
		}
		hasCompaction = hasCompaction || item.Type == "compaction" || item.Type == "compaction_summary"
		result.Items = append(result.Items, model.ProviderItem{
			Origin: p.baseURL,
			Model:  request.Model,
			Data:   append(json.RawMessage(nil), raw...),
		})
	}
	if !hasCompaction {
		return model.CompactionResult{}, fmt.Errorf("OpenAI Responses compact response has no compaction item")
	}
	if compacted.Usage != nil {
		result.Usage = usageFromResponse(compacted.Usage)
	}
	return result, nil
}

func (p *Provider) doRequest(ctx context.Context, token AccessToken, body []byte, metadata providerRequestMetadata) (*http.Response, error) {
	return p.doRequestTo(ctx, token, body, metadata, "/responses")
}

func (p *Provider) doRequestTo(ctx context.Context, token AccessToken, body []byte, metadata providerRequestMetadata, path string) (*http.Response, error) {
	if p.recordRequest != nil {
		if err := p.recordRequest(path, body); err != nil {
			return nil, fmt.Errorf("record OpenAI Responses request: %w", err)
		}
	}
	response, err := httpstream.DoRequest(ctx, p.httpClient, p.httpOptions, func(requestCtx context.Context) (*http.Request, error) {
		httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, p.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create OpenAI Responses request: %w", err)
		}
		httpRequest.Header.Set("Authorization", "Bearer "+token.Token)
		if token.AccountID != "" {
			httpRequest.Header.Set("ChatGPT-Account-Id", token.AccountID)
		}
		applySessionAffinityHeaders(httpRequest.Header, metadata, p.baseURL)
		if turnState := p.currentTurnState(); turnState != "" {
			httpRequest.Header.Set("x-codex-turn-state", turnState)
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		return httpRequest, nil
	}, func(body io.Reader) string {
		return httpstream.ReadErrorBody(body, token.Token)
	})
	if err == nil {
		p.captureTurnState(response.Header.Get("x-codex-turn-state"))
	}
	return response, err
}

func applySessionAffinityHeaders(headers http.Header, metadata providerRequestMetadata, baseURL string) {
	if metadata.CacheKey == "" || metadata.SessionAffinity == "none" {
		return
	}
	format := metadata.SessionAffinity
	if format == "" || format == "auto" {
		if strings.Contains(strings.ToLower(baseURL), "openrouter.ai") {
			format = "openrouter"
		} else {
			format = "openai"
		}
	}
	if format == "openrouter" {
		headers.Set("x-session-id", metadata.CacheKey)
		return
	}
	headers.Set("session-id", metadata.CacheKey)
	headers.Set("thread-id", metadata.CacheKey)
	headers.Set("x-client-request-id", metadata.CacheKey)
}

func (p *Provider) currentTurnState() string {
	p.turnStateMu.Lock()
	defer p.turnStateMu.Unlock()
	return p.turnState
}

func (p *Provider) captureTurnState(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	p.turnStateMu.Lock()
	defer p.turnStateMu.Unlock()
	if p.turnState == "" {
		p.turnState = value
	}
}

// compactRetryBackoff is a var so tests can shrink the wait.
var compactRetryBackoff = time.Second

func waitForCompactRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func shouldRetryWithoutContinuation(err error, metadata providerRequestMetadata) bool {
	if !metadata.UsedContinuation || err == nil {
		return false
	}
	var statusErr *httpstream.StatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(statusErr.Body), "previous_response_id")
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

func streamResponseEvents(ctx context.Context, body io.Reader, idleTimeout time.Duration, events chan<- model.Event, toolNames *toolNameMapper, state model.ResponseState) {
	decoder := newStreamEventDecoderWithState(toolNames, state)
	err := httpstream.ReadSSEFrames(ctx, body, idleTimeout, func(frame []byte) bool {
		return emitSSEFrame(decoder, frame, events)
	})
	if err != nil {
		sendStreamError(events, err, "OpenAI Responses")
	} else if !decoder.terminal {
		events <- model.ErrorEvent{Err: fmt.Errorf("stream ended before a terminal response event"), Message: "read OpenAI Responses stream"}
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
