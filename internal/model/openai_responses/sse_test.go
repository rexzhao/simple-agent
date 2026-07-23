package openairesponses

import (
	"encoding/json"
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
	want := model.ToolCall{ID: "call_1", ProviderID: "fc_1", Name: "mcp.local.search", Arguments: `{"query":"needle"}`}
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
	want := model.ToolCall{ID: "call_1", ProviderID: "fc_1", Name: "read_file", Arguments: `{"path":"note.txt"}`}
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

func TestEventsFromChunkMapsCacheAndReasoningUsage(t *testing.T) {
	tests := []struct {
		name string
		json string
		want model.Usage
	}{
		{
			name: "cache miss",
			json: `{"type":"response.completed","response":{"usage":{"input_tokens":1500,"output_tokens":12,"total_tokens":1512}}}`,
			want: model.Usage{InputTokens: 1500, OutputTokens: 12, TotalTokens: 1512},
		},
		{
			name: "cache read",
			json: `{"type":"response.completed","response":{"usage":{"input_tokens":1500,"output_tokens":12,"total_tokens":1512,"input_tokens_details":{"cached_tokens":1408},"output_tokens_details":{"reasoning_tokens":8}}}}`,
			want: model.Usage{InputTokens: 1500, OutputTokens: 12, TotalTokens: 1512, CachedTokens: 1408, ReasoningTokens: 8},
		},
		{
			name: "cache write",
			json: `{"type":"response.completed","response":{"usage":{"input_tokens":1500,"output_tokens":12,"total_tokens":1512,"input_tokens_details":{"cache_write_tokens":1408}}}}`,
			want: model.Usage{InputTokens: 1500, OutputTokens: 12, TotalTokens: 1512, CacheWriteTokens: 1408},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, done, err := EventsFromChunk([]byte(tt.json))
			if err != nil {
				t.Fatalf("EventsFromChunk() error = %v", err)
			}
			if !done {
				t.Fatal("done = false, want true")
			}
			if len(events) != 1 {
				t.Fatalf("len(events) = %d, want 1: %#v", len(events), events)
			}
			usage, ok := events[0].(model.UsageEvent)
			if !ok || usage.Usage != tt.want {
				t.Fatalf("event = %#v, want usage %#v", events[0], tt.want)
			}
		})
	}
}

func TestStreamDecoderPreservesResponseOutputStateForManualReplay(t *testing.T) {
	decoder := newStreamEventDecoderWithState(nil, model.ResponseState{
		Origin: "https://api.openai.com/v1",
		Model:  "gpt-5.6",
		Stored: false,
	})
	events, done, err := decoder.EventsFromSSE([]byte(
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
			"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs_1\",\"encrypted_content\":\"cipher\"},{\"type\":\"message\",\"id\":\"msg_1\",\"phase\":\"final_answer\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Done\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":4,\"total_tokens\":14}}}\n\n",
	))
	if err != nil {
		t.Fatalf("EventsFromSSE() error = %v", err)
	}
	if !done || !decoder.terminal {
		t.Fatalf("done = %v, terminal = %v, want both true", done, decoder.terminal)
	}
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3: %#v", len(events), events)
	}
	stateEvent, ok := events[2].(model.ResponseStateEvent)
	if !ok {
		t.Fatalf("event[2] = %T, want model.ResponseStateEvent", events[2])
	}
	state := stateEvent.State
	if state.ID != "resp_1" || state.Origin != "https://api.openai.com/v1" || state.Model != "gpt-5.6" || state.Stored || state.MessageID != "msg_1" || state.MessagePhase != "final_answer" {
		t.Fatalf("state = %#v, want preserved response metadata", state)
	}
	if len(state.ReasoningItems) != 1 {
		t.Fatalf("len(ReasoningItems) = %d, want 1", len(state.ReasoningItems))
	}
	var reasoning map[string]any
	if err := json.Unmarshal(state.ReasoningItems[0], &reasoning); err != nil {
		t.Fatalf("unmarshal reasoning item: %v", err)
	}
	if reasoning["encrypted_content"] != "cipher" {
		t.Fatalf("reasoning item = %#v, want terminal encrypted content", reasoning)
	}
	if len(state.OutputItems) != 2 {
		t.Fatalf("len(OutputItems) = %d, want 2", len(state.OutputItems))
	}
	var outputMessage map[string]any
	if err := json.Unmarshal(state.OutputItems[1], &outputMessage); err != nil {
		t.Fatalf("unmarshal exact output message: %v", err)
	}
	if outputMessage["id"] != "msg_1" {
		t.Fatalf("output item = %#v, want exact terminal message", outputMessage)
	}
}

func TestEventsFromChunkTreatsIncompleteAsTerminal(t *testing.T) {
	events, done, err := EventsFromChunk([]byte(`{
		"type":"response.incomplete",
		"response":{"id":"resp_incomplete","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}
	}`))
	if err != nil {
		t.Fatalf("EventsFromChunk() error = %v", err)
	}
	if !done {
		t.Fatal("done = false, want true")
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want usage and state: %#v", len(events), events)
	}
}

func TestEventsFromChunkBackfillsFinalMessageTextAndRefusal(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "output text", content: `{"type":"output_text","text":"complete text"}`, want: "complete text"},
		{name: "refusal", content: `{"type":"refusal","refusal":"cannot comply"}`, want: "cannot comply"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			decoder := newStreamEventDecoder()
			events, done, err := decoder.eventsFromChunk([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","content":[` + tt.content + `]}}`))
			if err != nil {
				t.Fatalf("eventsFromChunk() error = %v", err)
			}
			if done || len(events) != 1 {
				t.Fatalf("done = %v, events = %#v, want one non-terminal event", done, events)
			}
			text, ok := events[0].(model.TextDeltaEvent)
			if !ok || text.Text != tt.want {
				t.Fatalf("event = %#v, want text %q", events[0], tt.want)
			}
		})
	}
}

func TestEventsFromChunkBackfillsTerminalOutputWithoutItemDoneEvents(t *testing.T) {
	events, done, err := EventsFromChunk([]byte(`{
		"type":"response.completed",
		"response":{
			"output":[
				{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"terminal text"}]},
				{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"}
			]
		}
	}`))
	if err != nil {
		t.Fatalf("EventsFromChunk() error = %v", err)
	}
	if !done || len(events) != 3 {
		t.Fatalf("done = %v, events = %#v, want text, tool call, and state", done, events)
	}
	text, ok := events[0].(model.TextDeltaEvent)
	if !ok || text.Text != "terminal text" {
		t.Fatalf("event[0] = %#v, want terminal text", events[0])
	}
	tool, ok := events[1].(model.ToolCallDoneEvent)
	if !ok || tool.ToolCall != (model.ToolCall{ID: "call_1", ProviderID: "fc_1", Name: "lookup", Arguments: `{"id":1}`}) {
		t.Fatalf("event[1] = %#v, want terminal tool call", events[1])
	}
	if _, ok := events[2].(model.ResponseStateEvent); !ok {
		t.Fatalf("event[2] = %T, want model.ResponseStateEvent", events[2])
	}
}
