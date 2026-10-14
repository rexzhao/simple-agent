package main

import (
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/model"
)

func TestGrayGatedByColorEnabled(t *testing.T) {
	original := colorEnabled
	t.Cleanup(func() { colorEnabled = original })

	// Disabled (pipes, files): plain text, no escape sequences.
	colorEnabled = false
	if got := gray("thinking"); got != "thinking" {
		t.Fatalf("gray with color disabled = %q", got)
	}
	if got := gray(""); got != "" {
		t.Fatalf("gray of empty = %q", got)
	}

	// Enabled: the span is self-contained — gray prefix, reset suffix —
	// so the terminal state is restored at the end of every write.
	colorEnabled = true
	got := gray("thinking")
	if !strings.HasPrefix(got, ansiGray) || !strings.HasSuffix(got, ansiReset) {
		t.Fatalf("gray with color enabled = %q, want %s...%s", got, ansiGray, ansiReset)
	}
	if inner := strings.TrimSuffix(strings.TrimPrefix(got, ansiGray), ansiReset); inner != "thinking" {
		t.Fatalf("gray altered the text: %q", inner)
	}
	if got := gray(""); got != "" {
		t.Fatalf("gray of empty emitted codes: %q", got)
	}
}

func TestLiveWriterGraysReasoningNotContent(t *testing.T) {
	original := colorEnabled
	colorEnabled = true
	t.Cleanup(func() { colorEnabled = original })

	rec := &recordingTerminal{}
	w := &liveWriter{printer: rec}
	w.handle(model.AssistantMessageStartedEvent{ItemID: "a1"})
	w.handle(model.ReasoningDeltaEvent{Text: "thinking"})
	w.handle(model.TextDeltaEvent{Text: "answer"})

	got := rec.String()
	// The reasoning span is wrapped; the answer is not.
	if !strings.Contains(got, ansiGray+"  · thinking"+ansiReset) {
		t.Fatalf("reasoning not self-contained gray:\n%q", got)
	}
	if !strings.Contains(got, "answer"+ansiReset) && strings.Contains(got, ansiGray+"  answer") {
		t.Fatalf("answer must not be gray:\n%q", got)
	}
	// The final state is restored: no gray left open at the end.
	if strings.HasSuffix(strings.TrimRight(got, "\n"), ansiGray) {
		t.Fatalf("terminal left in gray state:\n%q", got)
	}
}
