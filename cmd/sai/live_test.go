package main

import "testing"

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
