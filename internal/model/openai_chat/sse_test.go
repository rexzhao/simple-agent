package openaichat

import (
	"reflect"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestParseSSEHandlesDone(t *testing.T) {
	got := ParseSSE([]byte("data: [DONE]\n\n"))
	want := []SSEMessage{{Data: "[DONE]", Done: true}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseSSE() = %#v, want %#v", got, want)
	}

	events, done, err := EventsFromSSE([]byte("data: [DONE]\n\n"))
	if err != nil {
		t.Fatalf("EventsFromSSE() error = %v", err)
	}
	if !done {
		t.Fatalf("EventsFromSSE() done = false, want true")
	}
	if len(events) != 0 {
		t.Fatalf("EventsFromSSE() events = %#v, want none", events)
	}
}

func TestEventsFromChunkConvertsTextDelta(t *testing.T) {
	events, err := EventsFromChunk([]byte(`{
		"object": "chat.completion.chunk",
		"choices": [
			{"delta": {"content": "hello"}}
		]
	}`))
	if err != nil {
		t.Fatalf("EventsFromChunk() error = %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1: %#v", len(events), events)
	}
	got, ok := events[0].(model.TextDeltaEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.TextDeltaEvent", events[0])
	}
	if got.Text != "hello" {
		t.Fatalf("Text = %q, want hello", got.Text)
	}
}

func TestEventsFromChunkConvertsReasoningDelta(t *testing.T) {
	events, err := EventsFromChunk([]byte(`{
		"object": "chat.completion.chunk",
		"choices": [
			{"delta": {"reasoning_content": "thinking"}}
		]
	}`))
	if err != nil {
		t.Fatalf("EventsFromChunk() error = %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1: %#v", len(events), events)
	}
	got, ok := events[0].(model.ReasoningDeltaEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.ReasoningDeltaEvent", events[0])
	}
	if got.Text != "thinking" {
		t.Fatalf("Text = %q, want thinking", got.Text)
	}
}

func TestEventsFromChunkConvertsUsage(t *testing.T) {
	events, err := EventsFromChunk([]byte(`{
		"object": "chat.completion.chunk",
		"choices": [],
		"usage": {
			"prompt_tokens": 3,
			"completion_tokens": 5,
			"total_tokens": 8
		}
	}`))
	if err != nil {
		t.Fatalf("EventsFromChunk() error = %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1: %#v", len(events), events)
	}
	got, ok := events[0].(model.UsageEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.UsageEvent", events[0])
	}
	want := model.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}
	if got.Usage != want {
		t.Fatalf("Usage = %#v, want %#v", got.Usage, want)
	}
}

func TestEventsFromChunkConvertsToolCallDeltaAndDone(t *testing.T) {
	events, err := EventsFromChunk([]byte(`{
		"object": "chat.completion.chunk",
		"choices": [
			{
				"index": 0,
				"delta": {
					"tool_calls": [
						{
							"index": 0,
							"id": "call_1",
							"function": {
								"name": "read_file",
								"arguments": "{\"path\":\"AGENTS.md\"}"
							}
						}
					]
				},
				"finish_reason": "tool_calls"
			}
		]
	}`))
	if err != nil {
		t.Fatalf("EventsFromChunk() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2: %#v", len(events), events)
	}

	delta, ok := events[0].(model.ToolCallDeltaEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.ToolCallDeltaEvent", events[0])
	}
	if delta.Index != 0 || delta.ID != "call_1" || delta.Name != "read_file" || delta.ArgumentsDelta != `{"path":"AGENTS.md"}` {
		t.Fatalf("delta = %#v, want call_1 read_file arguments delta", delta)
	}

	done, ok := events[1].(model.ToolCallDoneEvent)
	if !ok {
		t.Fatalf("event[1] = %T, want model.ToolCallDoneEvent", events[1])
	}
	want := model.ToolCall{ID: "call_1", Name: "read_file", Arguments: `{"path":"AGENTS.md"}`}
	if done.ToolCall != want {
		t.Fatalf("ToolCall = %#v, want %#v", done.ToolCall, want)
	}
}

func TestStreamEventDecoderAccumulatesSplitToolCallArguments(t *testing.T) {
	decoder := newStreamEventDecoder()
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"docs/"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"checklist.md\"}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
	}

	var events []model.Event
	for _, frame := range frames {
		chunkEvents, done, err := decoder.EventsFromSSE([]byte(frame))
		if err != nil {
			t.Fatalf("EventsFromSSE() error = %v", err)
		}
		if done {
			t.Fatalf("EventsFromSSE() done = true, want false")
		}
		events = append(events, chunkEvents...)
	}

	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3: %#v", len(events), events)
	}
	firstDelta, ok := events[0].(model.ToolCallDeltaEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.ToolCallDeltaEvent", events[0])
	}
	if firstDelta.ArgumentsDelta != `{"path":"docs/` {
		t.Fatalf("first ArgumentsDelta = %q, want first partial JSON chunk", firstDelta.ArgumentsDelta)
	}
	secondDelta, ok := events[1].(model.ToolCallDeltaEvent)
	if !ok {
		t.Fatalf("event[1] = %T, want model.ToolCallDeltaEvent", events[1])
	}
	if secondDelta.ArgumentsDelta != `checklist.md"}` {
		t.Fatalf("second ArgumentsDelta = %q, want second partial JSON chunk", secondDelta.ArgumentsDelta)
	}

	done, ok := events[2].(model.ToolCallDoneEvent)
	if !ok {
		t.Fatalf("event[2] = %T, want model.ToolCallDoneEvent", events[2])
	}
	want := model.ToolCall{ID: "call_1", Name: "read_file", Arguments: `{"path":"docs/checklist.md"}`}
	if done.ToolCall != want {
		t.Fatalf("ToolCall = %#v, want %#v", done.ToolCall, want)
	}
}

func TestEventsFromSSEConvertsDataFrames(t *testing.T) {
	events, done, err := EventsFromSSE([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n"))
	if err != nil {
		t.Fatalf("EventsFromSSE() error = %v", err)
	}
	if !done {
		t.Fatalf("done = false, want true")
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1: %#v", len(events), events)
	}
	if got := events[0].(model.TextDeltaEvent).Text; got != "hi" {
		t.Fatalf("Text = %q, want hi", got)
	}
}
