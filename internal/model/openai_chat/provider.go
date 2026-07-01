package openaichat

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

type EnvLookupFunc func(string) (string, bool)

type ProviderConfig struct {
	BaseURL    string
	APIKey     string
	APIKeyEnv  string
	LookupEnv  EnvLookupFunc
	HTTPClient *http.Client
}

type Provider struct {
	baseURL    string
	apiKey     string
	apiKeyEnv  string
	lookupEnv  EnvLookupFunc
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

	lookupEnv := config.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	return &Provider{
		baseURL:    baseURL,
		apiKey:     config.APIKey,
		apiKeyEnv:  config.APIKeyEnv,
		lookupEnv:  lookupEnv,
		httpClient: httpClient,
	}, nil
}

func (p *Provider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	apiKey, err := p.apiKeyValue()
	if err != nil {
		return nil, err
	}

	body, err := BuildRequestBody(request, true)
	if err != nil {
		return nil, fmt.Errorf("build OpenAI chat request body: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create OpenAI chat request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")

	response, err := p.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("send OpenAI chat request: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		return nil, fmt.Errorf("OpenAI chat request failed: %s: %s", response.Status, readErrorBody(response.Body))
	}

	events := make(chan model.Event)
	go func() {
		defer close(events)
		defer response.Body.Close()
		streamResponseEvents(response.Body, events)
	}()
	return events, nil
}

func (p *Provider) apiKeyValue() (string, error) {
	if p.apiKey != "" {
		return p.apiKey, nil
	}
	if p.apiKeyEnv == "" {
		return "", fmt.Errorf("API key is required")
	}
	if value, ok := p.lookupEnv(p.apiKeyEnv); ok && value != "" {
		return value, nil
	}
	return "", fmt.Errorf("API key environment variable %q is not set", p.apiKeyEnv)
}

func streamResponseEvents(body io.Reader, events chan<- model.Event) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var frame bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line == "\r" {
			if emitSSEFrame(frame.Bytes(), events) {
				return
			}
			frame.Reset()
			continue
		}
		frame.WriteString(line)
		frame.WriteByte('\n')
	}
	if frame.Len() > 0 {
		emitSSEFrame(frame.Bytes(), events)
	}
	if err := scanner.Err(); err != nil {
		events <- model.ErrorEvent{Err: err, Message: "read OpenAI chat stream"}
	}
}

func emitSSEFrame(frame []byte, events chan<- model.Event) bool {
	if len(frame) == 0 {
		return false
	}

	chunkEvents, done, err := EventsFromSSE(frame)
	if err != nil {
		events <- model.ErrorEvent{Err: err, Message: "parse OpenAI chat stream"}
		return true
	}
	for _, event := range chunkEvents {
		events <- event
	}
	return done
}

func readErrorBody(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return "read response body: " + err.Error()
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		return "empty response body"
	}
	return message
}
