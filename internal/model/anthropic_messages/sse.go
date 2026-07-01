package anthropicmessages

import (
	"bytes"
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
	toolNames   *toolNameMapper
	toolUses    map[int]*anthropicToolUseAccumulator
}

type anthropicToolUseAccumulator struct {
	ID         string
	Name       string
	StartInput string
	Arguments  strings.Builder
}

func newStreamEventDecoder(toolNames ...*toolNameMapper) *streamEventDecoder {
	var mapper *toolNameMapper
	if len(toolNames) > 0 {
		mapper = toolNames[0]
	}
	return &streamEventDecoder{
		toolNames: mapper,
		toolUses:  make(map[int]*anthropicToolUseAccumulator),
	}
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
	case "content_block_start":
		return d.contentBlockStartEvents(chunk), false, nil
	case "content_block_delta":
		return d.contentBlockDeltaEvents(chunk), false, nil
	case "content_block_stop":
		return d.contentBlockStopEvents(chunk.Index), false, nil
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

func (d *streamEventDecoder) contentBlockStartEvents(chunk anthropicStreamEvent) []model.Event {
	block := chunk.ContentBlock
	if block == nil || block.Type != "tool_use" {
		return nil
	}

	name := d.internalToolName(block.Name)
	d.toolUses[chunk.Index] = &anthropicToolUseAccumulator{
		ID:         block.ID,
		Name:       name,
		StartInput: compactRawJSON(block.Input),
	}
	return []model.Event{model.ToolCallDeltaEvent{
		Index: chunk.Index,
		ID:    block.ID,
		Name:  name,
	}}
}

func (d *streamEventDecoder) contentBlockDeltaEvents(chunk anthropicStreamEvent) []model.Event {
	switch chunk.Delta.Type {
	case "text_delta":
		if chunk.Delta.Text == "" {
			return nil
		}
		return []model.Event{model.TextDeltaEvent{Text: chunk.Delta.Text}}
	case "thinking_delta":
		text := chunk.Delta.Thinking
		if text == "" {
			text = chunk.Delta.Text
		}
		if text == "" {
			return nil
		}
		return []model.Event{model.ReasoningDeltaEvent{Text: text}}
	case "input_json_delta":
		accumulator := d.toolUseAccumulator(chunk.Index)
		accumulator.Arguments.WriteString(chunk.Delta.PartialJSON)
		return []model.Event{model.ToolCallDeltaEvent{
			Index:          chunk.Index,
			ID:             accumulator.ID,
			Name:           accumulator.Name,
			ArgumentsDelta: chunk.Delta.PartialJSON,
		}}
	default:
		return nil
	}
}

func (d *streamEventDecoder) contentBlockStopEvents(index int) []model.Event {
	accumulator, ok := d.toolUses[index]
	if !ok {
		return nil
	}
	delete(d.toolUses, index)

	arguments := accumulator.Arguments.String()
	if arguments == "" {
		arguments = accumulator.StartInput
	}
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	return []model.Event{model.ToolCallDoneEvent{
		ToolCall: model.ToolCall{
			ID:        accumulator.ID,
			Name:      accumulator.Name,
			Arguments: arguments,
		},
	}}
}

func (d *streamEventDecoder) toolUseAccumulator(index int) *anthropicToolUseAccumulator {
	accumulator := d.toolUses[index]
	if accumulator == nil {
		accumulator = &anthropicToolUseAccumulator{}
		d.toolUses[index] = accumulator
	}
	return accumulator
}

func (d *streamEventDecoder) internalToolName(name string) string {
	if d.toolNames == nil {
		return name
	}
	return d.toolNames.internalName(name)
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
	Type         string                 `json:"type"`
	Index        int                    `json:"index"`
	Message      *anthropicMessage      `json:"message"`
	ContentBlock *anthropicContentBlock `json:"content_block"`
	Delta        anthropicDelta         `json:"delta"`
	Usage        *anthropicUsage        `json:"usage"`
}

type anthropicMessage struct {
	Usage *anthropicUsage `json:"usage"`
}

type anthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	PartialJSON string `json:"partial_json"`
}

type anthropicContentBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type anthropicUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
}

func compactRawJSON(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}

	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return string(raw)
	}
	return buffer.String()
}
