package contextwindow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	DefaultContextWindowTokens = 32000
	WarningThresholdPercent    = 80

	requestOverheadTokens    = 16
	messageOverheadTokens    = 8
	toolSchemaOverheadTokens = 16
)

type WindowSource string

const (
	WindowSourceConfigured WindowSource = "configured"
	WindowSourceEstimated  WindowSource = "estimated"
)

type UsageSource string

const (
	UsageSourceProvider  UsageSource = "provider"
	UsageSourceEstimated UsageSource = "estimated"
)

type Window struct {
	Tokens int
	Source WindowSource
}

type Metadata struct {
	ContextWindow           int    `json:"context_window"`
	ContextWindowSource     string `json:"context_window_source"`
	WarningThresholdPercent int    `json:"warning_threshold_percent"`
	LastRequestTokens       int    `json:"last_request_tokens,omitempty"`
	LastInputTokens         int    `json:"last_input_tokens,omitempty"`
	LastOutputTokens        int    `json:"last_output_tokens,omitempty"`
	LastTotalTokens         int    `json:"last_total_tokens,omitempty"`
	LastCachedTokens        int    `json:"last_cached_tokens,omitempty"`
	LastCacheWriteTokens    int    `json:"last_cache_write_tokens,omitempty"`
	LastReasoningTokens     int    `json:"last_reasoning_tokens,omitempty"`
	LastUsageSource         string `json:"last_usage_source,omitempty"`
	LastUsageAnchorMessages int    `json:"last_usage_anchor_messages,omitempty"`
	LastUsageAnchorHash     string `json:"last_usage_anchor_hash,omitempty"`
	WarningIssued           bool   `json:"warning_issued,omitempty"`
}

type RequestEstimate struct {
	InputTokens             int
	ContextWindow           int
	WarningThresholdPercent int
}

type BudgetExceededError struct {
	EstimatedInputTokens int
	ContextWindow        int
}

func (e *BudgetExceededError) Error() string {
	if e == nil {
		return "context window budget exceeded"
	}
	return fmt.Sprintf(
		"context window budget exceeded: estimated input tokens %d >= context window %d; refusing to send provider request; no context was truncated",
		e.EstimatedInputTokens,
		e.ContextWindow,
	)
}

type Tracker struct {
	mu       sync.Mutex
	metadata Metadata
}

type TrackingProvider struct {
	Inner         model.Provider
	Tracker       *Tracker
	WarningWriter io.Writer
}

func ResolveWindow(configuredTokens int) Window {
	if configuredTokens > 0 {
		return Window{Tokens: configuredTokens, Source: WindowSourceConfigured}
	}
	return Window{Tokens: DefaultContextWindowTokens, Source: WindowSourceEstimated}
}

func NewTracker(window Window, saved Metadata) *Tracker {
	metadata := Metadata{
		ContextWindow:           window.Tokens,
		ContextWindowSource:     string(window.Source),
		WarningThresholdPercent: WarningThresholdPercent,
	}
	if saved.ContextWindow > 0 {
		metadata = saved
		if metadata.ContextWindowSource == "" {
			metadata.ContextWindowSource = string(WindowSourceEstimated)
		}
		if metadata.WarningThresholdPercent <= 0 {
			metadata.WarningThresholdPercent = WarningThresholdPercent
		}
	}
	return &Tracker{metadata: metadata}
}

func (t *Tracker) Metadata() Metadata {
	if t == nil {
		return Metadata{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.metadata
}

func (t *Tracker) CheckRequest(request model.Request) (RequestEstimate, bool, error) {
	if t == nil {
		return RequestEstimate{}, false, nil
	}
	fullEstimate := EstimateRequestTokens(request)

	t.mu.Lock()
	defer t.mu.Unlock()

	inputTokens := fullEstimate
	if anchoredEstimate, ok := t.estimateFromProviderUsage(request); ok {
		inputTokens = anchoredEstimate
	}
	estimate := RequestEstimate{
		InputTokens:             inputTokens,
		ContextWindow:           t.metadata.ContextWindow,
		WarningThresholdPercent: t.metadata.WarningThresholdPercent,
	}
	t.metadata.LastRequestTokens = inputTokens
	if estimate.ContextWindow <= 0 {
		return estimate, false, nil
	}
	if inputTokens >= estimate.ContextWindow {
		return estimate, false, &BudgetExceededError{
			EstimatedInputTokens: inputTokens,
			ContextWindow:        estimate.ContextWindow,
		}
	}

	threshold := int(math.Ceil(float64(estimate.ContextWindow) * float64(estimate.WarningThresholdPercent) / 100))
	shouldWarn := threshold > 0 && inputTokens >= threshold && !t.metadata.WarningIssued
	if shouldWarn {
		t.metadata.WarningIssued = true
	}
	return estimate, shouldWarn, nil
}

func (t *Tracker) RecordProviderUsage(usage model.Usage) {
	t.recordUsage(UsageSourceProvider, usage, nil)
}

func (t *Tracker) RecordProviderUsageForRequest(usage model.Usage, request model.Request) {
	hash := requestPrefixHash(request, len(request.Messages))
	var anchor *usageAnchor
	if hash != "" {
		anchor = &usageAnchor{
			messageCount: len(request.Messages),
			hash:         hash,
		}
	}
	t.recordUsage(UsageSourceProvider, usage, anchor)
}

func (t *Tracker) RecordEstimatedUsage(inputTokens, outputTokens int) {
	totalTokens := inputTokens + outputTokens
	t.recordUsage(UsageSourceEstimated, model.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
	}, nil)
}

type usageAnchor struct {
	messageCount int
	hash         string
}

func (t *Tracker) recordUsage(source UsageSource, usage model.Usage, anchor *usageAnchor) {
	if t == nil {
		return
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.metadata.LastInputTokens = usage.InputTokens
	t.metadata.LastOutputTokens = usage.OutputTokens
	t.metadata.LastTotalTokens = usage.TotalTokens
	t.metadata.LastCachedTokens = usage.CachedTokens
	t.metadata.LastCacheWriteTokens = usage.CacheWriteTokens
	t.metadata.LastReasoningTokens = usage.ReasoningTokens
	t.metadata.LastUsageSource = string(source)
	t.metadata.LastUsageAnchorMessages = 0
	t.metadata.LastUsageAnchorHash = ""
	if anchor != nil {
		t.metadata.LastUsageAnchorMessages = anchor.messageCount
		t.metadata.LastUsageAnchorHash = anchor.hash
	}
}

func (p TrackingProvider) Stream(ctx context.Context, request model.Request) (<-chan model.Event, error) {
	if p.Inner == nil {
		return nil, fmt.Errorf("provider is required")
	}
	estimate, shouldWarn, err := p.Tracker.CheckRequest(request)
	if err != nil {
		return nil, err
	}
	if shouldWarn && p.WarningWriter != nil {
		if _, err := fmt.Fprintf(p.WarningWriter, "sai: warning: estimated context usage %d/%d tokens is near the model context window; no context was truncated.\n", estimate.InputTokens, estimate.ContextWindow); err != nil {
			return nil, err
		}
	}

	stream, err := p.Inner.Stream(ctx, request)
	if err != nil {
		return nil, err
	}

	events := make(chan model.Event)
	go func() {
		defer close(events)

		sawUsage := false
		sawError := false
		var providerUsage model.Usage
		outputTokens := 0
		for event := range stream {
			switch event := event.(type) {
			case model.UsageEvent:
				sawUsage = true
				providerUsage = event.Usage
			case model.ErrorEvent:
				sawError = true
			case model.TextDeltaEvent:
				outputTokens += EstimateTextTokens(event.Text)
			case model.ReasoningDeltaEvent:
				outputTokens += EstimateTextTokens(event.Text)
			case model.ToolCallDoneEvent:
				outputTokens += EstimateTextTokens(event.ToolCall.Name)
				outputTokens += EstimateTextTokens(event.ToolCall.Arguments)
			case model.MessageDoneEvent:
				outputTokens += EstimateMessageTokens(event.Message)
			}

			events <- event
		}
		if sawUsage && !sawError && ctx.Err() == nil {
			p.Tracker.RecordProviderUsageForRequest(providerUsage, request)
		} else if !sawUsage && !sawError && ctx.Err() == nil {
			p.Tracker.RecordEstimatedUsage(estimate.InputTokens, outputTokens)
		}
	}()
	return events, nil
}

func EstimateRequestTokens(request model.Request) int {
	total := requestOverheadTokens
	for _, message := range request.Messages {
		total += EstimateMessageTokens(message)
	}
	for _, tool := range request.Tools {
		total += EstimateToolTokens(tool)
	}
	if total < 1 {
		return 1
	}
	return total
}

func EstimateMessageTokens(message model.Message) int {
	total := messageOverheadTokens
	total += EstimateTextTokens(string(message.Role))
	total += EstimateTextTokens(message.Content)
	for _, block := range message.ContentBlocks {
		total += EstimateTextTokens(block.Type)
		total += EstimateTextTokens(block.Text)
		total += EstimateTextTokens(block.ImageURL)
		total += EstimateTextTokens(block.FileID)
	}
	total += EstimateTextTokens(message.ToolCallID)
	for _, toolCall := range message.ToolCalls {
		total += EstimateTextTokens(toolCall.ID)
		total += EstimateTextTokens(toolCall.ProviderID)
		total += EstimateTextTokens(toolCall.Name)
		total += EstimateTextTokens(toolCall.Arguments)
	}
	for _, item := range message.ProviderItems {
		total += estimateProviderItemTokens(item)
	}
	if message.ResponseState != nil {
		for _, item := range message.ResponseState.ReasoningItems {
			total += EstimateTextTokens(string(item))
		}
	}
	return total
}

func estimateProviderItemTokens(item model.ProviderItem) int {
	var value any
	if json.Unmarshal(item.Data, &value) != nil {
		return 0
	}
	stripEncryptedProviderContent(value)
	data, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return EstimateTextTokens(string(data))
}

func stripEncryptedProviderContent(value any) {
	switch value := value.(type) {
	case map[string]any:
		delete(value, "encrypted_content")
		for _, child := range value {
			stripEncryptedProviderContent(child)
		}
	case []any:
		for _, child := range value {
			stripEncryptedProviderContent(child)
		}
	}
}

func EstimateToolTokens(tool model.Tool) int {
	total := toolSchemaOverheadTokens
	total += EstimateTextTokens(tool.Name)
	total += EstimateTextTokens(tool.Description)
	if len(tool.InputSchema) > 0 {
		if data, err := json.Marshal(tool.InputSchema); err == nil {
			total += EstimateTextTokens(string(data))
		}
	}
	return total
}

func EstimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	asciiCharacters := 0
	nonASCIICharacters := 0
	for _, character := range text {
		if character < utf8.RuneSelf {
			asciiCharacters++
		} else {
			nonASCIICharacters++
		}
	}
	return int(math.Ceil(float64(asciiCharacters)/4)) + nonASCIICharacters
}

func (t *Tracker) estimateFromProviderUsage(request model.Request) (int, bool) {
	if t.metadata.LastUsageSource != string(UsageSourceProvider) ||
		t.metadata.LastTotalTokens <= 0 ||
		t.metadata.LastUsageAnchorHash == "" {
		return 0, false
	}
	prefixMessages := t.metadata.LastUsageAnchorMessages
	if prefixMessages < 0 || len(request.Messages) <= prefixMessages {
		return 0, false
	}
	if request.Messages[prefixMessages].Role != model.MessageRoleAssistant {
		return 0, false
	}
	if requestPrefixHash(request, prefixMessages) != t.metadata.LastUsageAnchorHash {
		return 0, false
	}

	total := t.metadata.LastTotalTokens
	for _, message := range request.Messages[prefixMessages+1:] {
		total += EstimateMessageTokens(message)
	}
	if total < 1 {
		return 1, true
	}
	return total, true
}

func requestPrefixHash(request model.Request, messageCount int) string {
	if messageCount < 0 || messageCount > len(request.Messages) {
		return ""
	}
	data, err := json.Marshal(struct {
		Model    string
		Messages []model.Message
		Tools    []model.Tool
	}{
		Model:    request.Model,
		Messages: request.Messages[:messageCount],
		Tools:    request.Tools,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func ParseWindowSource(source string) WindowSource {
	switch WindowSource(strings.TrimSpace(source)) {
	case WindowSourceConfigured:
		return WindowSourceConfigured
	default:
		return WindowSourceEstimated
	}
}
