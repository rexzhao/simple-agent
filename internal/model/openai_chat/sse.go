package openaichat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

type SSEMessage struct {
	Data string
	Done bool
}

func ParseSSE(data []byte) []SSEMessage {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")

	var messages []SSEMessage
	var dataLines []string
	for _, line := range strings.Split(normalized, "\n") {
		if line == "" {
			messages = appendSSEMessage(messages, dataLines)
			dataLines = nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, ok := strings.Cut(line, ":")
		if !ok || field != "data" {
			continue
		}
		if strings.HasPrefix(value, " ") {
			value = strings.TrimPrefix(value, " ")
		}
		dataLines = append(dataLines, value)
	}
	return appendSSEMessage(messages, dataLines)
}

func EventsFromSSE(data []byte) ([]model.Event, bool, error) {
	return newStreamEventDecoder().EventsFromSSE(data)
}

func EventsFromChunk(data []byte) ([]model.Event, error) {
	return newStreamEventDecoder().eventsFromChunk(data)
}

type streamEventDecoder struct {
	compatibility chatCompatibility
	toolNames     *toolNameMapper
	choices       map[int]map[int]*toolCallAccumulator
}

type toolCallAccumulator struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func newStreamEventDecoder() *streamEventDecoder {
	compatibility, _ := resolveCompatibility("")
	return newStreamEventDecoderWithCompatibility(compatibility)
}

func newStreamEventDecoderWithCompatibility(compatibility chatCompatibility, toolNames ...*toolNameMapper) *streamEventDecoder {
	var mapper *toolNameMapper
	if len(toolNames) > 0 {
		mapper = toolNames[0]
	}
	return &streamEventDecoder{
		compatibility: compatibility,
		toolNames:     mapper,
		choices:       make(map[int]map[int]*toolCallAccumulator),
	}
}

func (d *streamEventDecoder) EventsFromSSE(data []byte) ([]model.Event, bool, error) {
	var events []model.Event
	done := false
	for _, message := range ParseSSE(data) {
		if message.Done {
			done = true
			continue
		}

		chunkEvents, err := d.eventsFromChunk([]byte(message.Data))
		if err != nil {
			return nil, false, err
		}
		events = append(events, chunkEvents...)
	}
	return events, done, nil
}

func (d *streamEventDecoder) eventsFromChunk(data []byte) ([]model.Event, error) {
	var chunk chatCompletionChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("parse chat completion chunk: %w", err)
	}
	if chunk.Error != nil {
		return []model.Event{chatCompletionErrorEvent(chunk.Error)}, nil
	}

	var events []model.Event
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			events = append(events, model.TextDeltaEvent{Text: *choice.Delta.Content})
		}
		if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
			events = append(events, model.ReasoningDeltaEvent{Text: *choice.Delta.ReasoningContent})
		}
		events = d.appendToolCallDeltaEvents(events, choice)
		if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
			events = d.appendToolCallDoneEvents(events, choice.Index)
		}
	}
	if usage := d.compatibility.usage(chunk); usage != nil {
		events = append(events, model.UsageEvent{
			Usage: usageFromChatCompletion(usage),
		})
	}
	return events, nil
}

func (d *streamEventDecoder) appendToolCallDeltaEvents(events []model.Event, choice chatCompletionChoice) []model.Event {
	for _, delta := range choice.Delta.ToolCalls {
		accumulator := d.toolCallAccumulator(choice.Index, delta.Index)
		id := ""
		if delta.ID != nil && *delta.ID != "" {
			accumulator.ID = *delta.ID
		}
		id = accumulator.ID

		name := ""
		argumentsDelta := ""
		if delta.Function != nil {
			if delta.Function.Name != nil && *delta.Function.Name != "" {
				accumulator.Name = d.internalToolName(*delta.Function.Name)
			}
			name = accumulator.Name
			if delta.Function.Arguments != nil {
				argumentsDelta = *delta.Function.Arguments
				accumulator.Arguments.WriteString(argumentsDelta)
			}
		} else {
			name = accumulator.Name
		}

		events = append(events, model.ToolCallDeltaEvent{
			Index:          delta.Index,
			ID:             id,
			Name:           name,
			ArgumentsDelta: argumentsDelta,
		})
	}
	return events
}

func (d *streamEventDecoder) internalToolName(name string) string {
	if d == nil || d.toolNames == nil {
		return name
	}
	return d.toolNames.internalName(name)
}

func (d *streamEventDecoder) appendToolCallDoneEvents(events []model.Event, choiceIndex int) []model.Event {
	toolCalls := d.choices[choiceIndex]
	if len(toolCalls) == 0 {
		return events
	}

	indexes := make([]int, 0, len(toolCalls))
	for index := range toolCalls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	for _, index := range indexes {
		accumulator := toolCalls[index]
		events = append(events, model.ToolCallDoneEvent{
			ToolCall: model.ToolCall{
				ID:        accumulator.ID,
				Name:      accumulator.Name,
				Arguments: accumulator.Arguments.String(),
			},
		})
	}
	delete(d.choices, choiceIndex)
	return events
}

func (d *streamEventDecoder) toolCallAccumulator(choiceIndex int, toolCallIndex int) *toolCallAccumulator {
	toolCalls := d.choices[choiceIndex]
	if toolCalls == nil {
		toolCalls = make(map[int]*toolCallAccumulator)
		d.choices[choiceIndex] = toolCalls
	}
	accumulator := toolCalls[toolCallIndex]
	if accumulator == nil {
		accumulator = &toolCallAccumulator{}
		toolCalls[toolCallIndex] = accumulator
	}
	return accumulator
}

func appendSSEMessage(messages []SSEMessage, dataLines []string) []SSEMessage {
	if len(dataLines) == 0 {
		return messages
	}

	data := strings.Join(dataLines, "\n")
	messages = append(messages, SSEMessage{
		Data: data,
		Done: data == "[DONE]",
	})
	return messages
}

type chatCompletionChunk struct {
	Choices []chatCompletionChoice `json:"choices"`
	Usage   *chatCompletionUsage   `json:"usage"`
	Error   *chatCompletionError   `json:"error"`
}

type chatCompletionError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// Some providers deliver failures as an in-stream error object instead of
// an HTTP error status.
func chatCompletionErrorEvent(chatError *chatCompletionError) model.Event {
	errorMessage := strings.TrimSpace(chatError.Message)
	if errorMessage == "" {
		errorMessage = "unknown error"
	}
	if strings.TrimSpace(chatError.Type) != "" {
		errorMessage = strings.TrimSpace(chatError.Type) + ": " + errorMessage
	}
	if chatError.Code != nil {
		errorMessage = fmt.Sprintf("%s (code %v)", errorMessage, chatError.Code)
	}
	return model.ErrorEvent{
		Err:     &model.ProviderError{Message: errorMessage},
		Message: "OpenAI chat stream error",
	}
}

type chatCompletionChoice struct {
	Index        int                  `json:"index"`
	Delta        chatCompletionDelta  `json:"delta"`
	FinishReason *string              `json:"finish_reason"`
	Usage        *chatCompletionUsage `json:"usage"`
}

type chatCompletionDelta struct {
	Content          *string                       `json:"content"`
	ReasoningContent *string                       `json:"reasoning_content"`
	ToolCalls        []chatCompletionToolCallDelta `json:"tool_calls"`
}

type chatCompletionToolCallDelta struct {
	Index    int                              `json:"index"`
	ID       *string                          `json:"id"`
	Function *chatCompletionToolFunctionDelta `json:"function"`
}

type chatCompletionToolFunctionDelta struct {
	Name      *string `json:"name"`
	Arguments *string `json:"arguments"`
}

type chatCompletionUsage struct {
	PromptTokens            int                         `json:"prompt_tokens"`
	CompletionTokens        int                         `json:"completion_tokens"`
	TotalTokens             int                         `json:"total_tokens"`
	CachedTokens            int                         `json:"cached_tokens"`
	CacheWriteTokens        int                         `json:"cache_write_tokens"`
	PromptCacheHitTokens    int                         `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails     chatPromptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails chatCompletionTokensDetails `json:"completion_tokens_details"`
}

type chatPromptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type chatCompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func usageFromChatCompletion(usage *chatCompletionUsage) model.Usage {
	cachedTokens := usage.CachedTokens
	if cachedTokens == 0 {
		cachedTokens = usage.PromptTokensDetails.CachedTokens
	}
	if cachedTokens == 0 {
		cachedTokens = usage.PromptCacheHitTokens
	}
	cacheWriteTokens := usage.CacheWriteTokens
	if cacheWriteTokens == 0 {
		cacheWriteTokens = usage.PromptTokensDetails.CacheWriteTokens
	}
	return model.UsageFromInclusiveInput(
		usage.PromptTokens,
		usage.CompletionTokens,
		cachedTokens,
		cacheWriteTokens,
		usage.CompletionTokensDetails.ReasoningTokens,
	)
}
