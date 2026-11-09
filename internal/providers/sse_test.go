package providers_test

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"simple-agent/internal/providers"
)

func collect(t *testing.T, input string) []providers.SSEEvent {
	t.Helper()
	r := providers.NewSSEReader(strings.NewReader(input))
	var events []providers.SSEEvent
	for {
		event, err := r.Next()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("读取失败：%v", err)
		}
		events = append(events, event)
	}
}

func TestSSEOpenAIStyle(t *testing.T) {
	input := "data: {\"a\":1}\n\ndata: {\"a\":2}\n\ndata: [DONE]\n\n"
	events := collect(t, input)
	want := []string{`{"a":1}`, `{"a":2}`, `[DONE]`}
	if len(events) != len(want) {
		t.Fatalf("事件数应为 %d，实际 %d：%#v", len(want), len(events), events)
	}
	for i, w := range want {
		if events[i].Data != w {
			t.Fatalf("第 %d 条应为 %q，实际 %q", i, w, events[i].Data)
		}
	}
}

func TestSSEAnthropicStyleWithEventField(t *testing.T) {
	input := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	events := collect(t, input)
	if len(events) != 2 {
		t.Fatalf("应为 2 条事件，实际 %d", len(events))
	}
	if events[0].Event != "message_start" || events[0].Data != `{"type":"message_start"}` {
		t.Fatalf("第 1 条不一致：%#v", events[0])
	}
	if events[1].Event != "content_block_delta" {
		t.Fatalf("第 2 条 event 不一致：%q", events[1].Event)
	}
}

func TestSSECRLFCommentsAndHeartbeat(t *testing.T) {
	input := ": ping\r\n\r\ndata: {\"x\":1}\r\n\r\n: keep-alive\r\n\r\n"
	events := collect(t, input)
	if len(events) != 1 || events[0].Data != `{"x":1}` {
		t.Fatalf("注释行应被忽略，实际：%#v", events)
	}
}

func TestSSEMultiLineData(t *testing.T) {
	events := collect(t, "data: {\"a\":\ndata: 1}\n\n")
	if len(events) != 1 || events[0].Data != "{\"a\":\n1}" {
		t.Fatalf("多行 data 应拼接，实际：%#v", events)
	}
}

func TestSSETrailingEventWithoutBlankLine(t *testing.T) {
	events := collect(t, `data: {"a":1}`)
	if len(events) != 1 || events[0].Data != `{"a":1}` {
		t.Fatalf("无结尾空行时应交出残余事件，实际：%#v", events)
	}
}

func TestSSEEmptyDataIgnored(t *testing.T) {
	events := collect(t, "data:\n\ndata: real\n\n")
	if len(events) != 1 || events[0].Data != "real" {
		t.Fatalf("空 data 应忽略，实际：%#v", events)
	}
}

// 长行（大 chunk）不能被 scanner 截断。
func TestSSELongLine(t *testing.T) {
	long := strings.Repeat("x", 2*1024*1024)
	events := collect(t, fmt.Sprintf("data: %s\n\n", long))
	if len(events) != 1 || len(events[0].Data) != len(long) {
		t.Fatalf("长行被截断：%d / %d", len(events[0].Data), len(long))
	}
}
