package anthropicmessages

import (
	"reflect"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestParseSSEIgnoresEventAndCommentLines(t *testing.T) {
	got := ParseSSE([]byte("event: content_block_delta\n: ping\ndata: {\"type\":\"ping\"}\n\n"))
	want := []SSEMessage{{Data: `{"type":"ping"}`}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseSSE() = %#v, want %#v", got, want)
	}
}

func TestEventsFromSSEConvertsTextDeltaAndStopsAtMessageStop(t *testing.T) {
	events, done, err := EventsFromSSE([]byte(
		"event: content_block_delta\r\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\r\n\r\n" +
			"event: ping\r\n" +
			"data: {\"type\":\"ping\"}\r\n\r\n" +
			"event: message_stop\r\n" +
			"data: {\"type\":\"message_stop\"}\r\n\r\n",
	))
	if err != nil {
		t.Fatalf("EventsFromSSE() error = %v", err)
	}
	if !done {
		t.Fatalf("done = false, want true")
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

func TestEventsFromChunkConvertsThinkingDelta(t *testing.T) {
	events, done, err := EventsFromChunk([]byte(`{
		"type": "content_block_delta",
		"index": 0,
		"delta": {"type": "thinking_delta", "thinking": "thinking"}
	}`))
	if err != nil {
		t.Fatalf("EventsFromChunk() error = %v", err)
	}
	if done {
		t.Fatalf("done = true, want false")
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

func TestStreamEventDecoderCarriesMessageStartInputTokensIntoUsage(t *testing.T) {
	decoder := newStreamEventDecoder()

	events, done, err := decoder.EventsFromSSE([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n"))
	if err != nil {
		t.Fatalf("message_start EventsFromSSE() error = %v", err)
	}
	if done {
		t.Fatalf("message_start done = true, want false")
	}
	if len(events) != 0 {
		t.Fatalf("message_start events = %#v, want none", events)
	}

	events, done, err = decoder.EventsFromSSE([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":11}}\n\n"))
	if err != nil {
		t.Fatalf("message_delta EventsFromSSE() error = %v", err)
	}
	if done {
		t.Fatalf("message_delta done = true, want false")
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1: %#v", len(events), events)
	}
	got, ok := events[0].(model.UsageEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.UsageEvent", events[0])
	}
	want := model.Usage{InputTokens: 7, OutputTokens: 11, TotalTokens: 18}
	if got.Usage != want {
		t.Fatalf("Usage = %#v, want %#v", got.Usage, want)
	}
}
