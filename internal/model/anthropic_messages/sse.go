package anthropicmessages

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

type streamEventDecoder struct {
	inputTokens int
	hasInput    bool
}

func newStreamEventDecoder() *streamEventDecoder {
	return &streamEventDecoder{}
}

func (d *streamEventDecoder) EventsFromSSE(data []byte) ([]model.Event, bool, error) {
	var events []model.Event
	done := false
	for _, message := range ParseSSE(data) {
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
	var chunk anthropicStreamEvent
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, false, fmt.Errorf("parse Anthropic Messages stream event: %w", err)
	}

	switch chunk.Type {
	case "message_start":
		if chunk.Message != nil && chunk.Message.Usage != nil && chunk.Message.Usage.InputTokens != nil {
			d.inputTokens = *chunk.Message.Usage.InputTokens
			d.hasInput = true
		}
		return nil, false, nil
	case "content_block_delta":
		return contentBlockDeltaEvents(chunk.Delta), false, nil
	case "message_delta":
		if chunk.Usage == nil {
			return nil, false, nil
		}
		return []model.Event{d.usageEvent(chunk.Usage)}, false, nil
	case "message_stop":
		return nil, true, nil
	default:
		return nil, false, nil
	}
}

func contentBlockDeltaEvents(delta anthropicDelta) []model.Event {
	switch delta.Type {
	case "text_delta":
		if delta.Text == "" {
			return nil
		}
		return []model.Event{model.TextDeltaEvent{Text: delta.Text}}
	case "thinking_delta":
		text := delta.Thinking
		if text == "" {
			text = delta.Text
		}
		if text == "" {
			return nil
		}
		return []model.Event{model.ReasoningDeltaEvent{Text: text}}
	default:
		return nil
	}
}

func (d *streamEventDecoder) usageEvent(usage *anthropicUsage) model.UsageEvent {
	inputTokens := 0
	if usage.InputTokens != nil {
		inputTokens = *usage.InputTokens
		d.inputTokens = inputTokens
		d.hasInput = true
	} else if d.hasInput {
		inputTokens = d.inputTokens
	}

	outputTokens := 0
	if usage.OutputTokens != nil {
		outputTokens = *usage.OutputTokens
	}

	return model.UsageEvent{
		Usage: model.Usage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		},
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

type anthropicStreamEvent struct {
	Type    string            `json:"type"`
	Message *anthropicMessage `json:"message"`
	Delta   anthropicDelta    `json:"delta"`
	Usage   *anthropicUsage   `json:"usage"`
}

type anthropicMessage struct {
	Usage *anthropicUsage `json:"usage"`
}

type anthropicDelta struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
}

type anthropicUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
}
