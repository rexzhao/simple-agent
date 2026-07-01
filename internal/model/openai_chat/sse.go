package openaichat

import (
	"encoding/json"
	"fmt"
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
	var events []model.Event
	done := false
	for _, message := range ParseSSE(data) {
		if message.Done {
			done = true
			continue
		}

		chunkEvents, err := EventsFromChunk([]byte(message.Data))
		if err != nil {
			return nil, false, err
		}
		events = append(events, chunkEvents...)
	}
	return events, done, nil
}

func EventsFromChunk(data []byte) ([]model.Event, error) {
	var chunk chatCompletionChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("parse chat completion chunk: %w", err)
	}

	var events []model.Event
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			events = append(events, model.TextDeltaEvent{Text: *choice.Delta.Content})
		}
		if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
			events = append(events, model.ReasoningDeltaEvent{Text: *choice.Delta.ReasoningContent})
		}
	}
	if chunk.Usage != nil {
		events = append(events, model.UsageEvent{
			Usage: model.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			},
		})
	}
	return events, nil
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
}

type chatCompletionChoice struct {
	Delta chatCompletionDelta `json:"delta"`
}

type chatCompletionDelta struct {
	Content          *string `json:"content"`
	ReasoningContent *string `json:"reasoning_content"`
}

type chatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
