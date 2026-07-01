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
