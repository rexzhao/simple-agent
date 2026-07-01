package openairesponses

import (
	"reflect"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
)

func TestParseSSEIgnoresEventAndCommentLines(t *testing.T) {
	got := ParseSSE([]byte("event: response.output_text.delta\n: ping\ndata: {\"type\":\"ping\"}\n\n"))
	want := []SSEMessage{{Data: `{"type":"ping"}`}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseSSE() = %#v, want %#v", got, want)
	}
}

func TestEventsFromSSEConvertsTextDeltasCompletedUsageAndIgnoresDone(t *testing.T) {
	events, done, err := EventsFromSSE([]byte(
		"event: response.output_text.delta\r\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\r\n\r\n" +
			"event: response.output_text.delta\r\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\r\n\r\n" +
			"event: response.completed\r\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\r\n\r\n" +
			"data: [DONE]\r\n\r\n",
	))
	if err != nil {
		t.Fatalf("EventsFromSSE() error = %v", err)
	}
	if !done {
		t.Fatalf("done = false, want true")
	}
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3: %#v", len(events), events)
	}
	first, ok := events[0].(model.TextDeltaEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.TextDeltaEvent", events[0])
	}
	if first.Text != "hel" {
		t.Fatalf("first Text = %q, want hel", first.Text)
	}
	second, ok := events[1].(model.TextDeltaEvent)
	if !ok {
		t.Fatalf("event[1] = %T, want model.TextDeltaEvent", events[1])
	}
	if second.Text != "lo" {
		t.Fatalf("second Text = %q, want lo", second.Text)
	}
	usage, ok := events[2].(model.UsageEvent)
	if !ok {
		t.Fatalf("event[2] = %T, want model.UsageEvent", events[2])
	}
	wantUsage := model.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}
	if usage.Usage != wantUsage {
		t.Fatalf("Usage = %#v, want %#v", usage.Usage, wantUsage)
	}
}

func TestEventsFromChunkConvertsReasoningSummaryDelta(t *testing.T) {
	events, done, err := EventsFromChunk([]byte(`{
		"type": "response.reasoning_summary_text.delta",
		"delta": "thinking"
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

func TestEventsFromChunkConvertsErrorEvents(t *testing.T) {
	events, done, err := EventsFromChunk([]byte(`{
		"type": "response.failed",
		"response": {
			"error": {
				"type": "invalid_request_error",
				"message": "bad input",
				"code": "invalid"
			}
		}
	}`))
	if err != nil {
		t.Fatalf("EventsFromChunk() error = %v", err)
	}
	if !done {
		t.Fatalf("done = false, want true")
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1: %#v", len(events), events)
	}
	got, ok := events[0].(model.ErrorEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.ErrorEvent", events[0])
	}
	if got.Message != "OpenAI Responses response failed" || got.Err == nil {
		t.Fatalf("error event = %#v, want response failed with error", got)
	}
}
