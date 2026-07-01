package openairesponses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

type SSEMessage struct {
	Data string
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

func EventsFromChunk(data []byte) ([]model.Event, bool, error) {
	return newStreamEventDecoder().eventsFromChunk(data)
}

type streamEventDecoder struct{}

func newStreamEventDecoder() *streamEventDecoder {
	return &streamEventDecoder{}
}

func (d *streamEventDecoder) EventsFromSSE(data []byte) ([]model.Event, bool, error) {
	var events []model.Event
	done := false
	for _, message := range ParseSSE(data) {
		if strings.TrimSpace(message.Data) == "[DONE]" {
			continue
		}

		chunkEvents, chunkDone, err := d.eventsFromChunk([]byte(message.Data))
		if err != nil {
			return nil, false, err
		}
		events = append(events, chunkEvents...)
		if chunkDone {
			done = true
		}
	}
	return events, done, nil
}

func (d *streamEventDecoder) eventsFromChunk(data []byte) ([]model.Event, bool, error) {
	var event responseStreamEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, false, fmt.Errorf("parse OpenAI Responses stream event: %w", err)
	}

	switch event.Type {
	case "response.output_text.delta":
		if event.Delta == "" {
			return nil, false, nil
		}
		return []model.Event{model.TextDeltaEvent{Text: event.Delta}}, false, nil
	case "response.completed":
		if event.Response == nil || event.Response.Usage == nil {
			return nil, true, nil
		}
		return []model.Event{model.UsageEvent{Usage: usageFromResponse(event.Response.Usage)}}, true, nil
	case "error":
		return []model.Event{responseErrorEvent("OpenAI Responses stream error", event.Error, event.Message)}, true, nil
	case "response.failed":
		var responseError *responseError
		if event.Response != nil {
			responseError = event.Response.Error
		}
		return []model.Event{responseErrorEvent("OpenAI Responses response failed", responseError, event.Message)}, true, nil
	default:
		if isReasoningDeltaEvent(event.Type) {
			text := event.Delta
			if text == "" {
				text = event.Text
			}
			if text != "" {
				return []model.Event{model.ReasoningDeltaEvent{Text: text}}, false, nil
			}
		}
		return nil, false, nil
	}
}

func isReasoningDeltaEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "response.") &&
		strings.Contains(eventType, "reasoning") &&
		strings.HasSuffix(eventType, ".delta")
}

func usageFromResponse(usage *responseUsage) model.Usage {
	return model.Usage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
	}
}

func responseErrorEvent(message string, responseError *responseError, fallback string) model.ErrorEvent {
	errorMessage := strings.TrimSpace(fallback)
	if responseError != nil && strings.TrimSpace(responseError.Message) != "" {
		errorMessage = strings.TrimSpace(responseError.Message)
	}
	if errorMessage == "" {
		errorMessage = "unknown error"
	}

	if responseError != nil && strings.TrimSpace(responseError.Type) != "" {
		errorMessage = strings.TrimSpace(responseError.Type) + ": " + errorMessage
	}
	if responseError != nil && responseError.Code != nil {
		errorMessage = fmt.Sprintf("%s (code %v)", errorMessage, responseError.Code)
	}

	return model.ErrorEvent{
		Err:     fmt.Errorf("%s", errorMessage),
		Message: message,
	}
}

func appendSSEMessage(messages []SSEMessage, dataLines []string) []SSEMessage {
	if len(dataLines) == 0 {
		return messages
	}

	return append(messages, SSEMessage{
		Data: strings.Join(dataLines, "\n"),
	})
}

type responseStreamEvent struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Text     string          `json:"text"`
	Message  string          `json:"message"`
	Error    *responseError  `json:"error"`
	Response *responseObject `json:"response"`
}

type responseObject struct {
	Usage *responseUsage `json:"usage"`
	Error *responseError `json:"error"`
}

type responseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type responseError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    any    `json:"code"`
}
