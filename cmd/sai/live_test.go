package main

import (
	"strings"
	"sync"
	"testing"

	"github.com/rexzhao/simple-agent/model"
)

func TestFormatLiveChunkIndentsAndMarksReasoning(t *testing.T) {
	// Text chunks: two-space indent at each line start.
	lineStart := true
	got := formatLiveChunk("hi", false, &lineStart)
	if got != "  hi" || lineStart {
		t.Fatalf("first text chunk = %q, lineStart=%v", got, lineStart)
	}
	got = formatLiveChunk(" there\nand\n", false, &lineStart)
	if got != " there\n  and\n" || !lineStart {
		t.Fatalf("continuation = %q, lineStart=%v", got, lineStart)
	}

	// Reasoning chunks carry the '· ' marker per line.
	got = formatLiveChunk("think", true, &lineStart)
	if got != "  · think" {
		t.Fatalf("reasoning chunk = %q", got)
	}
	got = formatLiveChunk(" more\nnext", true, &lineStart)
	if got != " more\n  · next" {
		t.Fatalf("multi-line reasoning = %q", got)
	}

	// Empty chunks change nothing.
	lineStart = true
	if got := formatLiveChunk("", true, &lineStart); got != "" || !lineStart {
		t.Fatalf("empty chunk = %q, lineStart=%v", got, lineStart)
	}
}

func TestLiveWriterStateTransitions(t *testing.T) {
	w := &liveWriter{printer: &spinnerPrinter{}}
	if w.inMessage.Load() || w.sawOutput.Load() {
		t.Fatal("fresh writer reports active message")
	}
	// The state machine is driven by handle; the rendering itself needs a
	// terminal, so only the flags are asserted here.
	w.inMessage.Store(true)
	w.settle()
	if w.inMessage.Load() {
		t.Fatal("settle left the message open")
	}
}

// recordingTerminal captures everything the live writer emits.
type recordingTerminal struct {
	mu     sync.Mutex
	buffer strings.Builder
}

func (r *recordingTerminal) line(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buffer.WriteString(s)
	r.buffer.WriteString("\n")
}

func (r *recordingTerminal) write(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buffer.WriteString(s)
}

func (r *recordingTerminal) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buffer.String()
}

func TestLiveWriterBreaksLineBetweenReasoningAndContent(t *testing.T) {
	rec := &recordingTerminal{}
	w := &liveWriter{printer: rec}
	w.handle(model.AssistantMessageStartedEvent{ItemID: "a1"})
	w.handle(model.ReasoningDeltaEvent{Text: "thinking about the greeting"})
	w.handle(model.TextDeltaEvent{Text: "Hello! How can I help?"})
	w.handle(model.AssistantMessageCompletedEvent{ItemID: "a1"})

	got := rec.String()
	// The answer must open its own line: the record rendering shows
	// reasoning and content as separate parts.
	if !strings.Contains(got, "thinking about the greeting\n  Hello! How can I help?") {
		t.Fatalf("reasoning and content glued or mis-indented:\n%s", got)
	}
	// The phase break must not fire when reasoning already ended the line.
	rec = &recordingTerminal{}
	w = &liveWriter{printer: rec}
	w.handle(model.AssistantMessageStartedEvent{ItemID: "a1"})
	w.handle(model.ReasoningDeltaEvent{Text: "thought\n"})
	w.handle(model.TextDeltaEvent{Text: "answer"})
	w.handle(model.AssistantMessageCompletedEvent{ItemID: "a1"})
	if got := rec.String(); strings.Contains(got, "\n\n  answer") {
		t.Fatalf("phase break doubled a newline:\n%s", got)
	}
}
