package anthropicmessages

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

const anthropicVersion = "2023-06-01"

type ProviderConfig struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

type Provider struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
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
		baseURL:    baseURL,
		apiKey:     strings.TrimSpace(config.APIKey),
		httpClient: httpClient,
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

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Anthropic Messages request: %w", err)
	}
	httpRequest.Header.Set("x-api-key", apiKey)
	httpRequest.Header.Set("anthropic-version", anthropicVersion)
	httpRequest.Header.Set("Content-Type", "application/json")

	response, err := p.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("send Anthropic Messages request: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		return nil, fmt.Errorf("Anthropic Messages request failed: %s: %s", response.Status, readErrorBody(response.Body, apiKey))
	}

	events := make(chan model.Event)
	go func() {
		defer close(events)
		defer response.Body.Close()
		streamResponseEvents(response.Body, events, toolNames)
	}()
	return events, nil
}

func (p *Provider) apiKeyValue() (string, error) {
	if p.apiKey != "" {
		return p.apiKey, nil
	}
	return "", fmt.Errorf("API key is required")
}

func streamResponseEvents(body io.Reader, events chan<- model.Event, toolNames *toolNameMapper) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	decoder := newStreamEventDecoder(toolNames)
	var frame bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line == "\r" {
			if emitSSEFrame(decoder, frame.Bytes(), events) {
				return
			}
			frame.Reset()
			continue
		}
		frame.WriteString(line)
		frame.WriteByte('\n')
	}
	if frame.Len() > 0 {
		emitSSEFrame(decoder, frame.Bytes(), events)
	}
	if err := scanner.Err(); err != nil {
		events <- model.ErrorEvent{Err: err, Message: "read Anthropic Messages stream"}
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

func readErrorBody(body io.Reader, apiKey string) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return "read response body: " + err.Error()
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		return "empty response body"
	}
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		message = strings.ReplaceAll(message, apiKey, "<redacted>")
	}
	return message
}
