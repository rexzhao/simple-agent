package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/storage"
)

func TestFormatRecordCommonHeaderAndBodies(t *testing.T) {
	const stamp = "2026-01-02T03:04:05Z"
	// header builds the expected fixed-width header column so the
	// alignment itself is under test, not hand-counted spaces.
	header := func(label string) string {
		return fmt.Sprintf("] %-*s", recordLabelWidth, label)
	}
	cases := []struct {
		name   string
		record storage.Record
		want   []string // substrings the rendering must contain
		banned []string
	}{
		{
			name:   "session created shows provider model and cwd",
			record: storage.Record{Type: storage.TypeSessionCreated, Time: stamp, Provider: "paperhub", Model: "glm-5", CWD: `F:\work`},
			want:   []string{header("session") + " paperhub/glm-5 · F:\\work"},
		},
		{
			name:   "session header shows provider alone when model is absent",
			record: storage.Record{Type: storage.TypeSessionCreated, Time: stamp, Provider: "paperhub"},
			want:   []string{header("session") + " paperhub"},
		},
		{
			name:   "user input shows only the text",
			record: storage.Record{Type: storage.TypeUserInput, Time: stamp, RunID: "run-1", ItemID: "u1", Content: "fix the bug"},
			want:   []string{header("user") + " fix the bug"},
			banned: []string{"run-1", "u1"},
		},
		{
			name:   "short single-line body renders inline on the header",
			record: storage.Record{Type: storage.TypeUserInput, Time: stamp, Content: "hi"},
			want:   []string{header("user") + " hi"},
		},
		{
			name:   "multi-line body renders as an indented block",
			record: storage.Record{Type: storage.TypeUserInput, Time: stamp, Content: "line one\nline two"},
			want:   []string{header("user") + "\n", "  line one", "  line two"},
		},
		{
			name: "assistant shows reasoning marked, then the text",
			record: storage.Record{Type: storage.TypeAssistantMessage, Time: stamp, RunID: "run-1", Iter: 2,
				ItemID: "a1", Content: "done", Reasoning: "internal reasoning"},
			want:   []string{header("assistant"), "· internal reasoning", "done"},
			banned: []string{"iter", "a1"},
		},
		{
			name: "assistant tool call shows the call line",
			record: storage.Record{Type: storage.TypeAssistantMessage, Time: stamp,
				Content: "checking", ToolCalls: []storage.WireToolCall{{ID: "c1", Name: "shell", Arguments: `{"command":"ls"}`}}},
			want:   []string{header("assistant"), "checking", "-> shell {\"command\":\"ls\"}"},
			banned: []string{"c1"},
		},
		{
			name: "tool result shows name status and output",
			record: storage.Record{Type: storage.TypeToolResult, Time: stamp, ToolName: "shell",
				ToolCallID: "c1", Content: "file1\nfile2", IsError: false},
			want:   []string{header("tool") + " shell · ok", "file1", "file2"},
			banned: []string{"c1"},
		},
		{
			name: "tool error result is marked",
			record: storage.Record{Type: storage.TypeToolResult, Time: stamp, ToolName: "shell",
				Content: "boom", IsError: true},
			want: []string{"shell · error", "boom"},
		},
		{
			name:   "run failed shows the error",
			record: storage.Record{Type: storage.TypeRunFailed, Time: stamp, RunID: "run-1", Error: "request model: quota exceeded"},
			want:   []string{header("error") + " request model: quota exceeded"},
			banned: []string{"run-1"},
		},
		{
			name:   "unparseable time collapses to a placeholder",
			record: storage.Record{Type: storage.TypeUserInput, Time: "not-a-time", Content: "hi"},
			want:   []string{"[--:--:--] user"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := formatRecord(testCase.record)
			for _, want := range testCase.want {
				if !strings.Contains(got, want) {
					t.Fatalf("formatRecord = %q, want it to contain %q", got, want)
				}
			}
			for _, banned := range testCase.banned {
				if strings.Contains(got, banned) {
					t.Fatalf("formatRecord = %q, must not leak %q", got, banned)
				}
			}
		})
	}
}

func TestTruncateCallArguments(t *testing.T) {
	short := `{"command":"ls"}`
	if got := truncateCallArguments(short); got != short {
		t.Fatalf("short arguments truncated: %q", got)
	}
	long := strings.Repeat("x", displayCallRunes+50)
	got := truncateCallArguments(long)
	if !strings.Contains(got, "…(+50 chars)") {
		t.Fatalf("long arguments = %q, want an elision count", got)
	}
	if got := truncateCallArguments(""); got != "" {
		t.Fatalf("empty arguments = %q", got)
	}
}

func TestTruncateResultBodyKeepsTailWithNotice(t *testing.T) {
	short := "line1\nline2"
	if got := truncateResultBody(short); got != short {
		t.Fatalf("short result truncated: %q", got)
	}

	lines := make([]string, displayResultLines+10)
	for i := range lines {
		lines[i] = "l" + string(rune('a'+i%26)) + "-" + itoa(i)
	}
	lines[len(lines)-1] = "the-summary"
	content := strings.Join(lines, "\n")
	got := truncateResultBody(content)
	if !strings.Contains(got, "the-summary") {
		t.Fatalf("tail of result missing: %q", got)
	}
	if !strings.Contains(got, "… showing tail:") {
		t.Fatalf("truncation notice missing: %q", got)
	}
	if strings.Contains(got, "l-a-0\n") {
		t.Fatalf("head lines should be elided: %q", got)
	}

	// A single huge line trips the byte bound, not the line bound.
	huge := strings.Repeat("x", displayResultBytes+1000)
	got = truncateResultBody(huge)
	if !strings.Contains(got, "… showing tail:") {
		t.Fatalf("byte-bound notice missing: %q", got)
	}
	if len(got) > displayResultBytes+200 {
		t.Fatalf("byte-bound result still huge: %d bytes", len(got))
	}
	if !utf8Valid(got) {
		t.Fatal("byte-bound cut produced invalid UTF-8")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestRenderReasoningTruncation(t *testing.T) {
	short := "frame the problem\nthen answer"
	if got := renderReasoning(short); got != short {
		t.Fatalf("short reasoning truncated: %q", got)
	}

	lines := make([]string, displayReasoningLines+5)
	for i := range lines {
		lines[i] = "thought-" + itoa(i)
	}
	got := renderReasoning(strings.Join(lines, "\n"))
	if !strings.Contains(got, "thought-0") {
		t.Fatalf("reasoning head missing: %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf("showing head: %d of %d lines", displayReasoningLines, displayReasoningLines+5)) {
		t.Fatalf("elision notice missing: %q", got)
	}
	if strings.Contains(got, "thought-"+itoa(displayReasoningLines+4)) {
		t.Fatalf("reasoning tail should be elided: %q", got)
	}

	// A single huge line trips the byte bound: never a nonsensical
	// "+0 more lines", and both dimensions are reported.
	huge := strings.Repeat("界", displayReasoningBytes) // 3-byte runes
	got = renderReasoning(huge)
	if !utf8Valid(got) {
		t.Fatal("byte-bound reasoning cut produced invalid UTF-8")
	}
	if strings.Contains(got, "+0 more lines") {
		t.Fatalf("notice lies about the byte cut: %q", got)
	}
	if !strings.Contains(got, "showing head: 1 of 1 lines") || !strings.Contains(got, displaySize(len(huge))) {
		t.Fatalf("byte-bound notice missing its dimensions: %q", got)
	}
}
