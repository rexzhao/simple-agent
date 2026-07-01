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

type streamEventDecoder struct {
	toolNames *toolNameMapper
	toolCalls map[int]*responsesToolCallAccumulator
}

type responsesToolCallAccumulator struct {
	ItemID    string
	CallID    string
	Name      string
	Arguments strings.Builder
	Done      bool
}

func newStreamEventDecoder(toolNames ...*toolNameMapper) *streamEventDecoder {
	var mapper *toolNameMapper
	if len(toolNames) > 0 {
		mapper = toolNames[0]
	}
	return &streamEventDecoder{
		toolNames: mapper,
		toolCalls: make(map[int]*responsesToolCallAccumulator),
	}
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
	case "response.output_item.added":
		return d.outputItemAddedEvents(event), false, nil
	case "response.function_call_arguments.delta":
		return d.functionCallArgumentsDeltaEvents(event), false, nil
	case "response.function_call_arguments.done":
		return d.functionCallArgumentsDoneEvents(event), false, nil
	case "response.output_item.done":
		return d.outputItemDoneEvents(event), false, nil
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

func (d *streamEventDecoder) outputItemAddedEvents(event responseStreamEvent) []model.Event {
	if event.Item == nil || event.Item.Type != "function_call" {
		return nil
	}
	accumulator := d.toolCallAccumulator(event.OutputIndex)
	d.applyOutputItem(accumulator, event.Item, false)
	return nil
}

func (d *streamEventDecoder) functionCallArgumentsDeltaEvents(event responseStreamEvent) []model.Event {
	if event.Delta == "" {
		return nil
	}

	accumulator := d.toolCallAccumulator(event.OutputIndex)
	if event.ItemID != "" {
		accumulator.ItemID = event.ItemID
	}
	accumulator.Arguments.WriteString(event.Delta)

	return []model.Event{model.ToolCallDeltaEvent{
		Index:          event.OutputIndex,
		ID:             accumulator.CallID,
		Name:           accumulator.Name,
		ArgumentsDelta: event.Delta,
	}}
}

func (d *streamEventDecoder) functionCallArgumentsDoneEvents(event responseStreamEvent) []model.Event {
	accumulator := d.toolCallAccumulator(event.OutputIndex)
	if event.ItemID != "" {
		accumulator.ItemID = event.ItemID
	}
	if event.Arguments != nil {
		accumulator.setArguments(*event.Arguments)
	}
	if event.Item != nil && event.Item.Type == "function_call" {
		d.applyOutputItem(accumulator, event.Item, true)
	}
	return d.toolCallDoneEvents(event.OutputIndex)
}

func (d *streamEventDecoder) outputItemDoneEvents(event responseStreamEvent) []model.Event {
	if event.Item == nil || event.Item.Type != "function_call" {
		return nil
	}
	accumulator := d.toolCallAccumulator(event.OutputIndex)
	d.applyOutputItem(accumulator, event.Item, true)
	return d.toolCallDoneEvents(event.OutputIndex)
}

func (d *streamEventDecoder) applyOutputItem(accumulator *responsesToolCallAccumulator, item *responseOutputItem, replaceArguments bool) {
	if item.ID != "" {
		accumulator.ItemID = item.ID
	}
	if item.CallID != "" {
		accumulator.CallID = item.CallID
	}
	if item.Name != "" {
		accumulator.Name = d.internalToolName(item.Name)
	}
	if replaceArguments {
		accumulator.setArguments(item.Arguments)
		return
	}
	if item.Arguments != "" && accumulator.Arguments.Len() == 0 {
		accumulator.Arguments.WriteString(item.Arguments)
	}
}

func (d *streamEventDecoder) toolCallDoneEvents(outputIndex int) []model.Event {
	accumulator := d.toolCalls[outputIndex]
	if accumulator == nil || accumulator.Done {
		return nil
	}
	if accumulator.CallID == "" || accumulator.Name == "" {
		return nil
	}

	arguments := accumulator.Arguments.String()
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	accumulator.Done = true
	return []model.Event{model.ToolCallDoneEvent{
		ToolCall: model.ToolCall{
			ID:        accumulator.CallID,
			Name:      accumulator.Name,
			Arguments: arguments,
		},
	}}
}

func (d *streamEventDecoder) toolCallAccumulator(outputIndex int) *responsesToolCallAccumulator {
	accumulator := d.toolCalls[outputIndex]
	if accumulator == nil {
		accumulator = &responsesToolCallAccumulator{}
		d.toolCalls[outputIndex] = accumulator
	}
	return accumulator
}

func (d *streamEventDecoder) internalToolName(name string) string {
	if d.toolNames == nil {
		return name
	}
	return d.toolNames.internalName(name)
}

func (a *responsesToolCallAccumulator) setArguments(arguments string) {
	a.Arguments.Reset()
	a.Arguments.WriteString(arguments)
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
	Type        string              `json:"type"`
	Delta       string              `json:"delta"`
	Text        string              `json:"text"`
	Message     string              `json:"message"`
	ItemID      string              `json:"item_id"`
	OutputIndex int                 `json:"output_index"`
	Arguments   *string             `json:"arguments"`
	Item        *responseOutputItem `json:"item"`
	Error       *responseError      `json:"error"`
	Response    *responseObject     `json:"response"`
}

type responseOutputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
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
