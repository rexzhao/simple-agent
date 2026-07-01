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

func TestEventsFromSSEConvertsFunctionCallArgumentDeltasAndDoneOnce(t *testing.T) {
	toolNames := newToolNameMapper([]model.Tool{{Name: "mcp.local.search"}})
	events, done, err := newStreamEventDecoder(toolNames).EventsFromSSE([]byte(
		"event: response.output_item.added\n" +
			"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"tool_0\",\"arguments\":\"\"}}\n\n" +
			"event: response.function_call_arguments.delta\n" +
			"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"output_index\":0,\"delta\":\"{\\\"query\\\":\"}\n\n" +
			"event: response.function_call_arguments.delta\n" +
			"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"output_index\":0,\"delta\":\"\\\"needle\\\"}\"}\n\n" +
			"event: response.function_call_arguments.done\n" +
			"data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"output_index\":0,\"arguments\":\"{\\\"query\\\":\\\"needle\\\"}\"}\n\n" +
			"event: response.output_item.done\n" +
			"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"tool_0\",\"arguments\":\"{\\\"query\\\":\\\"needle\\\"}\"}}\n\n",
	))
	if err != nil {
		t.Fatalf("EventsFromSSE() error = %v", err)
	}
	if done {
		t.Fatalf("done = true, want false")
	}
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3: %#v", len(events), events)
	}
	firstDelta, ok := events[0].(model.ToolCallDeltaEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want model.ToolCallDeltaEvent", events[0])
	}
	if firstDelta.Index != 0 || firstDelta.ID != "call_1" || firstDelta.Name != "mcp.local.search" || firstDelta.ArgumentsDelta != `{"query":` {
		t.Fatalf("first delta = %#v, want mapped call_1 mcp.local.search", firstDelta)
	}
	secondDelta, ok := events[1].(model.ToolCallDeltaEvent)
	if !ok {
		t.Fatalf("event[1] = %T, want model.ToolCallDeltaEvent", events[1])
	}
	if secondDelta.Index != 0 || secondDelta.ID != "call_1" || secondDelta.Name != "mcp.local.search" || secondDelta.ArgumentsDelta != `"needle"}` {
		t.Fatalf("second delta = %#v, want mapped call_1 mcp.local.search", secondDelta)
	}
	doneEvent, ok := events[2].(model.ToolCallDoneEvent)
	if !ok {
		t.Fatalf("event[2] = %T, want model.ToolCallDoneEvent", events[2])
	}
	want := model.ToolCall{ID: "call_1", Name: "mcp.local.search", Arguments: `{"query":"needle"}`}
	if doneEvent.ToolCall != want {
		t.Fatalf("ToolCall = %#v, want %#v", doneEvent.ToolCall, want)
	}
}

func TestEventsFromSSEConvertsFunctionCallOutputItemDone(t *testing.T) {
	events, done, err := EventsFromSSE([]byte(
		"event: response.output_item.added\n" +
			"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"read_file\",\"arguments\":\"\"}}\n\n" +
			"event: response.function_call_arguments.delta\n" +
			"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"output_index\":0,\"delta\":\"{\\\"path\\\":\"}\n\n" +
			"event: response.function_call_arguments.delta\n" +
			"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"output_index\":0,\"delta\":\"\\\"note.txt\\\"}\"}\n\n" +
			"event: response.output_item.done\n" +
			"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"note.txt\\\"}\"}}\n\n",
	))
	if err != nil {
		t.Fatalf("EventsFromSSE() error = %v", err)
	}
	if done {
		t.Fatalf("done = true, want false")
	}
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3: %#v", len(events), events)
	}
	doneEvent, ok := events[2].(model.ToolCallDoneEvent)
	if !ok {
		t.Fatalf("event[2] = %T, want model.ToolCallDoneEvent", events[2])
	}
	want := model.ToolCall{ID: "call_1", Name: "read_file", Arguments: `{"path":"note.txt"}`}
	if doneEvent.ToolCall != want {
		t.Fatalf("ToolCall = %#v, want %#v", doneEvent.ToolCall, want)
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
