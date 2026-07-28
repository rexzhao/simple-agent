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
	toolNames     *toolNameMapper
	toolCalls     map[int]*responsesToolCallAccumulator
	textByOutput  map[int]*strings.Builder
	state         model.ResponseState
	reasoningByID map[string]int
	terminal      bool
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
	return newStreamEventDecoderWithState(mapper, model.ResponseState{})
}

func newStreamEventDecoderWithState(toolNames *toolNameMapper, state model.ResponseState) *streamEventDecoder {
	return &streamEventDecoder{
		toolNames:     toolNames,
		toolCalls:     make(map[int]*responsesToolCallAccumulator),
		textByOutput:  make(map[int]*strings.Builder),
		state:         state,
		reasoningByID: make(map[string]int),
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
	case "response.created":
		d.applyResponseState(event.Response)
		return nil, false, nil
	case "response.output_text.delta":
		if event.Delta == "" {
			return nil, false, nil
		}
		d.textAccumulator(event.OutputIndex).WriteString(event.Delta)
		return []model.Event{model.TextDeltaEvent{Text: event.Delta}}, false, nil
	case "response.refusal.delta":
		if event.Delta == "" {
			return nil, false, nil
		}
		d.textAccumulator(event.OutputIndex).WriteString(event.Delta)
		return []model.Event{model.TextDeltaEvent{Text: event.Delta}}, false, nil
	case "response.output_item.added":
		return d.outputItemAddedEvents(event), false, nil
	case "response.function_call_arguments.delta":
		return d.functionCallArgumentsDeltaEvents(event), false, nil
	case "response.function_call_arguments.done":
		return d.functionCallArgumentsDoneEvents(event), false, nil
	case "response.output_item.done":
		return d.outputItemDoneEvents(event), false, nil
	case "response.completed", "response.incomplete":
		d.terminal = true
		d.applyResponseState(event.Response)
		events := d.terminalOutputEvents(event.Response)
		if event.Response != nil && event.Response.Usage != nil {
			events = append(events, model.UsageEvent{Usage: usageFromResponse(event.Response.Usage)})
		}
		if d.hasResponseState() {
			events = append(events, model.ResponseStateEvent{State: d.copyResponseState()})
		}
		return events, true, nil
	case "error":
		d.terminal = true
		return []model.Event{responseErrorEvent("OpenAI Responses stream error", event.Error, event.Message)}, true, nil
	case "response.failed", "response.cancelled":
		d.terminal = true
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

func (d *streamEventDecoder) terminalOutputEvents(response *responseObject) []model.Event {
	if response == nil {
		return nil
	}
	var events []model.Event
	for outputIndex, raw := range response.Output {
		events = append(events, d.outputItemDoneEvents(responseStreamEvent{
			OutputIndex: outputIndex,
			Item:        raw,
		})...)
	}
	return events
}

func (d *streamEventDecoder) outputItemAddedEvents(event responseStreamEvent) []model.Event {
	item := event.outputItem()
	if item == nil || item.Type != "function_call" {
		return nil
	}
	accumulator := d.toolCallAccumulator(event.OutputIndex)
	d.applyOutputItem(accumulator, item, false)
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
	if item := event.outputItem(); item != nil && item.Type == "function_call" {
		d.applyOutputItem(accumulator, item, true)
	}
	return d.toolCallDoneEvents(event.OutputIndex)
}

func (d *streamEventDecoder) outputItemDoneEvents(event responseStreamEvent) []model.Event {
	item := event.outputItem()
	if item == nil {
		return nil
	}
	d.captureOutputItem(event.OutputIndex, event.Item, item)
	if item.Type == "message" {
		finalText := item.outputText()
		seen := d.textAccumulator(event.OutputIndex).String()
		if strings.HasPrefix(finalText, seen) && len(finalText) > len(seen) {
			delta := finalText[len(seen):]
			d.textAccumulator(event.OutputIndex).WriteString(delta)
			return []model.Event{model.TextDeltaEvent{Text: delta}}
		}
		return nil
	}
	if item.Type != "function_call" {
		return nil
	}
	accumulator := d.toolCallAccumulator(event.OutputIndex)
	d.applyOutputItem(accumulator, item, true)
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
			ID:         accumulator.CallID,
			ProviderID: accumulator.ItemID,
			Name:       accumulator.Name,
			Arguments:  arguments,
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

func (d *streamEventDecoder) textAccumulator(outputIndex int) *strings.Builder {
	accumulator := d.textByOutput[outputIndex]
	if accumulator == nil {
		accumulator = &strings.Builder{}
		d.textByOutput[outputIndex] = accumulator
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

func (event responseStreamEvent) outputItem() *responseOutputItem {
	if len(event.Item) == 0 || string(event.Item) == "null" {
		return nil
	}
	var item responseOutputItem
	if json.Unmarshal(event.Item, &item) != nil {
		return nil
	}
	return &item
}

func (d *streamEventDecoder) applyResponseState(response *responseObject) {
	if response == nil {
		return
	}
	if response.ID != "" {
		d.state.ID = response.ID
	}
	for index, raw := range response.Output {
		var item responseOutputItem
		if json.Unmarshal(raw, &item) == nil {
			d.captureOutputItem(index, raw, &item)
		}
	}
}

func (d *streamEventDecoder) captureOutputItem(outputIndex int, raw json.RawMessage, item *responseOutputItem) {
	if item == nil {
		return
	}
	if outputIndex >= 0 && len(raw) > 0 {
		for len(d.state.OutputItems) <= outputIndex {
			d.state.OutputItems = append(d.state.OutputItems, nil)
		}
		d.state.OutputItems[outputIndex] = append(json.RawMessage(nil), raw...)
	}
	switch item.Type {
	case "reasoning":
		copied := append(json.RawMessage(nil), raw...)
		if index, ok := d.reasoningByID[item.ID]; ok {
			d.state.ReasoningItems[index] = copied
			return
		}
		d.reasoningByID[item.ID] = len(d.state.ReasoningItems)
		d.state.ReasoningItems = append(d.state.ReasoningItems, copied)
	case "message":
		if item.ID != "" {
			d.state.MessageID = item.ID
		}
		if item.Phase != "" {
			d.state.MessagePhase = item.Phase
		}
	}
}

func (d *streamEventDecoder) hasResponseState() bool {
	return d.state.ID != "" || d.state.MessageID != "" || len(d.state.ReasoningItems) > 0 || hasRawOutputItems(d.state.OutputItems)
}

func (d *streamEventDecoder) copyResponseState() model.ResponseState {
	state := d.state
	state.ReasoningItems = make([]json.RawMessage, len(d.state.ReasoningItems))
	for index, item := range d.state.ReasoningItems {
		state.ReasoningItems[index] = append(json.RawMessage(nil), item...)
	}
	state.OutputItems = make([]json.RawMessage, 0, len(d.state.OutputItems))
	for _, item := range d.state.OutputItems {
		if len(item) > 0 {
			state.OutputItems = append(state.OutputItems, append(json.RawMessage(nil), item...))
		}
	}
	return state
}

func hasRawOutputItems(items []json.RawMessage) bool {
	for _, item := range items {
		if len(item) > 0 {
			return true
		}
	}
	return false
}

func isReasoningDeltaEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "response.") &&
		strings.Contains(eventType, "reasoning") &&
		strings.HasSuffix(eventType, ".delta")
}

func usageFromResponse(usage *responseUsage) model.Usage {
	return model.UsageFromInclusiveInput(
		usage.InputTokens,
		usage.OutputTokens,
		usage.InputTokensDetails.CachedTokens,
		usage.InputTokensDetails.CacheWriteTokens,
		usage.OutputTokensDetails.ReasoningTokens,
	)
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
		Err:     &model.ProviderError{Message: errorMessage},
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
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	Text        string          `json:"text"`
	Message     string          `json:"message"`
	ItemID      string          `json:"item_id"`
	OutputIndex int             `json:"output_index"`
	Arguments   *string         `json:"arguments"`
	Item        json.RawMessage `json:"item"`
	Error       *responseError  `json:"error"`
	Response    *responseObject `json:"response"`
}

type responseOutputItem struct {
	Type      string                  `json:"type"`
	ID        string                  `json:"id"`
	Phase     string                  `json:"phase"`
	CallID    string                  `json:"call_id"`
	Name      string                  `json:"name"`
	Arguments string                  `json:"arguments"`
	Content   []responseOutputContent `json:"content"`
}

func (item responseOutputItem) outputText() string {
	var text strings.Builder
	for _, content := range item.Content {
		if content.Type == "output_text" {
			text.WriteString(content.Text)
		} else if content.Type == "refusal" {
			text.WriteString(content.Refusal)
		}
	}
	return text.String()
}

type responseOutputContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responseObject struct {
	ID     string            `json:"id"`
	Output []json.RawMessage `json:"output"`
	Usage  *responseUsage    `json:"usage"`
	Error  *responseError    `json:"error"`
}

type responseUsage struct {
	InputTokens         int                        `json:"input_tokens"`
	OutputTokens        int                        `json:"output_tokens"`
	TotalTokens         int                        `json:"total_tokens"`
	InputTokensDetails  responseInputTokenDetails  `json:"input_tokens_details"`
	OutputTokensDetails responseOutputTokenDetails `json:"output_tokens_details"`
}

type responseInputTokenDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type responseOutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type responseError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    any    `json:"code"`
}
