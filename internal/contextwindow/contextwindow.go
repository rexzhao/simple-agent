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
	"time"
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
	LastUsageCountTokens    int    `json:"last_usage_count_tokens,omitempty"`
	LastCachedTokens        int    `json:"last_cached_tokens,omitempty"`
	LastCacheWriteTokens    int    `json:"last_cache_write_tokens,omitempty"`
	LastReasoningTokens     int    `json:"last_reasoning_tokens,omitempty"`
	LastUsageSource         string `json:"last_usage_source,omitempty"`
	LastUsageAnchorMessages int    `json:"last_usage_anchor_messages,omitempty"`
	LastUsageAnchorHash     string `json:"last_usage_anchor_hash,omitempty"`
	// Total usage is cumulative for the durable session. The Last* fields
	// above intentionally remain the most recent provider request because the
	// context-window meter uses that request, while cost reporting uses these
	// totals.
	TotalInputTokens            int   `json:"total_input_tokens,omitempty"`
	TotalOutputTokens           int   `json:"total_output_tokens,omitempty"`
	TotalTokens                 int   `json:"total_tokens,omitempty"`
	TotalRequests               int   `json:"total_requests,omitempty"`
	TotalCachedTokens           int   `json:"total_cached_tokens,omitempty"`
	TotalCacheWriteTokens       int   `json:"total_cache_write_tokens,omitempty"`
	TotalReasoningTokens        int   `json:"total_reasoning_tokens,omitempty"`
	TotalShortInputTokens       int   `json:"total_short_input_tokens,omitempty"`
	TotalShortOutputTokens      int   `json:"total_short_output_tokens,omitempty"`
	TotalShortCachedTokens      int   `json:"total_short_cached_tokens,omitempty"`
	TotalShortCacheWriteTokens  int   `json:"total_short_cache_write_tokens,omitempty"`
	TotalLongInputTokens        int   `json:"total_long_input_tokens,omitempty"`
	TotalLongOutputTokens       int   `json:"total_long_output_tokens,omitempty"`
	TotalLongCachedTokens       int   `json:"total_long_cached_tokens,omitempty"`
	TotalLongCacheWriteTokens   int   `json:"total_long_cache_write_tokens,omitempty"`
	LastRequestDurationMillis   int64 `json:"last_request_duration_ms,omitempty"`
	LastTimeToFirstEventMillis  int64 `json:"last_time_to_first_event_ms,omitempty"`
	TotalRequestDurationMillis  int64 `json:"total_request_duration_ms,omitempty"`
	TotalTimeToFirstEventMillis int64 `json:"total_time_to_first_event_ms,omitempty"`
	RequestTimingSamples        int   `json:"request_timing_samples,omitempty"`
	WarningIssued               bool  `json:"warning_issued,omitempty"`
}

type RequestEstimate struct {
	InputTokens             int
	ContextWindow           int
	HardInputLimit          int
	WarningThresholdPercent int
}

type BudgetExceededError struct {
	EstimatedInputTokens int
	ContextWindow        int
	HardInputLimit       int
}

func (e *BudgetExceededError) Error() string {
	if e == nil {
		return "context window budget exceeded"
	}
	limit := e.HardInputLimit
	if limit <= 0 {
		limit = e.ContextWindow
	}
	return fmt.Sprintf("context input budget exceeded: estimated input tokens %d >= safe input limit %d; refusing to send provider request; no context was truncated", e.EstimatedInputTokens, limit)
}

type Tracker struct {
	mu                        sync.Mutex
	metadata                  Metadata
	longContextTokenThreshold int
	hardInputLimit            int
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
		if metadata.WarningThresholdPercent <= 0 {
			metadata.WarningThresholdPercent = WarningThresholdPercent
		}
		// Usage history carries over, but the window itself always reflects
		// the current resolution so config edits reach existing sessions.
		metadata.ContextWindow = window.Tokens
		metadata.ContextWindowSource = string(window.Source)
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

// SetLongContextTokenThreshold configures the boundary used to split
// cumulative provider usage into short- and long-context billing buckets.
// Requests above the boundary are long-context requests.
func (t *Tracker) SetLongContextTokenThreshold(threshold int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.longContextTokenThreshold = threshold
}

// SetHardInputLimit installs the effective per-request input ceiling after
// output reserve and estimation safety margin have been applied.
func (t *Tracker) SetHardInputLimit(limit int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hardInputLimit = limit
}

// EstimateRequest returns the best current input estimate without mutating
// warning or request accounting state. A provider usage anchor is used when
// the request extends the exact request that produced that usage; otherwise
// the complete request is estimated locally.
func (t *Tracker) EstimateRequest(request model.Request) RequestEstimate {
	if t == nil {
		return RequestEstimate{InputTokens: EstimateRequestTokens(request)}
	}
	fullEstimate := EstimateRequestTokens(request)
	t.mu.Lock()
	defer t.mu.Unlock()
	inputTokens := fullEstimate
	if anchoredEstimate, ok := t.estimateFromProviderUsage(request); ok {
		inputTokens = anchoredEstimate
	}
	return RequestEstimate{
		InputTokens:             inputTokens,
		ContextWindow:           t.metadata.ContextWindow,
		HardInputLimit:          t.hardInputLimit,
		WarningThresholdPercent: t.metadata.WarningThresholdPercent,
	}
}

func (t *Tracker) CheckRequest(request model.Request) (RequestEstimate, bool, error) {
	if t == nil {
		return RequestEstimate{}, false, nil
	}
	estimate := t.EstimateRequest(request)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.metadata.LastRequestTokens = estimate.InputTokens
	limit := estimate.HardInputLimit
	if limit <= 0 {
		limit = estimate.ContextWindow
	}
	if limit > 0 && estimate.InputTokens >= limit {
		return estimate, false, &BudgetExceededError{EstimatedInputTokens: estimate.InputTokens, ContextWindow: estimate.ContextWindow, HardInputLimit: limit}
	}
	if estimate.ContextWindow <= 0 {
		return estimate, false, nil
	}

	threshold := int(math.Ceil(float64(estimate.ContextWindow) * float64(estimate.WarningThresholdPercent) / 100))
	shouldWarn := threshold > 0 && estimate.InputTokens >= threshold && !t.metadata.WarningIssued
	if shouldWarn {
		t.metadata.WarningIssued = true
	}
	return estimate, shouldWarn, nil
}

func (t *Tracker) RecordProviderUsage(usage model.Usage) {
	t.recordUsage(UsageSourceProvider, usage, nil, true)
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
	t.recordUsage(UsageSourceProvider, usage, anchor, true)
}

func (t *Tracker) RecordEstimatedUsage(inputTokens, outputTokens int) {
	totalTokens := inputTokens + outputTokens
	t.recordUsage(UsageSourceEstimated, model.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
	}, nil, true)
}

// RecordEstimatedContextUsage updates the latest context estimate without
// counting it as an API request. This is used after compaction to show the
// replacement history's context size.
func (t *Tracker) RecordEstimatedContextUsage(inputTokens, outputTokens int) {
	totalTokens := inputTokens + outputTokens
	t.recordUsage(UsageSourceEstimated, model.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
	}, nil, false)
}

func (t *Tracker) RecordRequestTiming(duration, timeToFirstEvent time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	durationMillis := duration.Milliseconds()
	firstEventMillis := timeToFirstEvent.Milliseconds()
	t.metadata.LastRequestDurationMillis = durationMillis
	t.metadata.LastTimeToFirstEventMillis = firstEventMillis
	t.metadata.TotalRequestDurationMillis += durationMillis
	t.metadata.TotalTimeToFirstEventMillis += firstEventMillis
	t.metadata.RequestTimingSamples++
}

type usageAnchor struct {
	messageCount int
	hash         string
}

func (t *Tracker) recordUsage(source UsageSource, usage model.Usage, anchor *usageAnchor, countRequest bool) {
	if t == nil {
		return
	}
	usageCount := usage.TotalTokens
	if usageCount <= 0 {
		usageCount = usage.InputTokens + usage.OutputTokens + usage.CachedTokens + usage.CacheWriteTokens
	}
	if usage.TotalTokens <= 0 {
		usage.TotalTokens = usageCount
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.metadata.LastInputTokens = usage.InputTokens
	t.metadata.LastOutputTokens = usage.OutputTokens
	t.metadata.LastTotalTokens = usage.TotalTokens
	t.metadata.LastUsageCountTokens = usageCount
	t.metadata.LastCachedTokens = usage.CachedTokens
	t.metadata.LastCacheWriteTokens = usage.CacheWriteTokens
	t.metadata.LastReasoningTokens = usage.ReasoningTokens
	t.metadata.LastUsageSource = string(source)
	t.metadata.LastUsageAnchorMessages = 0
	t.metadata.LastUsageAnchorHash = ""
	// Provider streams report one usage event per request (and an agent turn
	// may contain multiple requests for tool iterations), so keep both the
	// latest request and an additive session total. Estimates are used for the
	// context-window meter only; they are not actual billable provider usage and
	// must not be included in the cost total.
	if countRequest {
		t.metadata.TotalRequests++
	}
	if source == UsageSourceProvider {
		t.metadata.TotalInputTokens += usage.InputTokens
		t.metadata.TotalOutputTokens += usage.OutputTokens
		t.metadata.TotalCachedTokens += usage.CachedTokens
		t.metadata.TotalCacheWriteTokens += usage.CacheWriteTokens
		t.metadata.TotalReasoningTokens += usage.ReasoningTokens
		t.metadata.TotalTokens += usage.TotalTokens
		inputTokens := usage.InputTokens + usage.CachedTokens + usage.CacheWriteTokens
		if t.longContextTokenThreshold > 0 && inputTokens > t.longContextTokenThreshold {
			t.metadata.TotalLongInputTokens += usage.InputTokens
			t.metadata.TotalLongOutputTokens += usage.OutputTokens
			t.metadata.TotalLongCachedTokens += usage.CachedTokens
			t.metadata.TotalLongCacheWriteTokens += usage.CacheWriteTokens
		} else {
			t.metadata.TotalShortInputTokens += usage.InputTokens
			t.metadata.TotalShortOutputTokens += usage.OutputTokens
			t.metadata.TotalShortCachedTokens += usage.CachedTokens
			t.metadata.TotalShortCacheWriteTokens += usage.CacheWriteTokens
		}
	}
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

	startedAt := time.Now()
	stream, err := p.Inner.Stream(ctx, request)
	if err != nil {
		duration := time.Since(startedAt)
		p.Tracker.RecordRequestTiming(duration, duration)
		return nil, err
	}

	events := make(chan model.Event)
	go func() {
		defer close(events)

		sawUsage := false
		sawError := false
		var providerUsage model.Usage
		outputTokens := 0
		var firstEventAt time.Time
		for event := range stream {
			if firstEventAt.IsZero() {
				firstEventAt = time.Now()
			}
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
		finishedAt := time.Now()
		if firstEventAt.IsZero() {
			firstEventAt = finishedAt
		}
		p.Tracker.RecordRequestTiming(finishedAt.Sub(startedAt), firstEventAt.Sub(startedAt))
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

const estimatedImageInputTokens = 1_000

func EstimateMessageTokens(message model.Message) int {
	total := messageOverheadTokens
	total += EstimateTextTokens(string(message.Role))
	total += EstimateTextTokens(message.Content)
	total += EstimateTextTokens(message.ReasoningContent)
	for _, block := range message.ContentBlocks {
		total += EstimateTextTokens(block.Type)
		total += EstimateTextTokens(block.Text)
		if block.Type == "input_image" {
			// A data URL contains base64 bytes, not model text. Counting that
			// representation would inflate an image into millions of text tokens;
			// use a stable vision-input estimate until the provider reports usage.
			total += estimatedImageInputTokens
		} else {
			total += EstimateTextTokens(block.ImageURL)
		}
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
