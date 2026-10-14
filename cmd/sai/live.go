package main

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/rexzhao/simple-agent/internal/storage"
	"github.com/rexzhao/simple-agent/model"
)

// terminal is the output surface the live writer needs; *spinnerPrinter
// satisfies it and tests record into a buffer.
type terminal interface {
	line(string)
	write(string)
}

// delta kinds, for the reasoning→content phase break.
const (
	deltaNone = iota
	deltaReasoning
	deltaText
)

// liveWriter renders the transient event stream of the active run onto the
// terminal: the assistant message appears character by character as the
// model generates it, reasoning lines marked like in the committed record
// rendering. The committed assistant.message record is then skipped by the
// render loop — its content was already shown.
type liveWriter struct {
	printer terminal
	// inMessage is set between AssistantMessageStarted and
	// Completed/Failed; sawOutput once any character of the current
	// message was written. The spinner loop reads both: it spins while
	// nothing is flowing (waiting for the first token, running tools) and
	// stays out of the way while text streams.
	inMessage atomic.Bool
	sawOutput atomic.Bool

	// item is the ItemID of the message being streamed: the render loop
	// uses it to skip the record of a live-rendered message. The stream's
	// order (started → deltas → record → completed) keeps the check
	// race-free: the record of the streaming item arrives while item is
	// still set (completed clears it after).
	item string

	lineStart     bool // at the start of a body line (drives the indent)
	lastKind      int  // delta kind of the last chunk (drives the phase break)
	headerPending bool // the message header is not yet on the terminal
}

// currentItem reports the item being streamed ("" between messages).
func (w *liveWriter) currentItem() string { return w.item }

// handle consumes one transient event.
func (w *liveWriter) handle(event model.Event) {
	switch event := event.(type) {
	case model.AssistantMessageStartedEvent:
		w.inMessage.Store(true)
		w.sawOutput.Store(false)
		w.lineStart = true
		w.lastKind = deltaNone
		w.item = event.ItemID
		// The header is pending, not printed: it must not overtake the
		// user-input record. It goes out with the first actual output — a
		// full provider roundtrip after the input committed (the stream's
		// seq order guarantees the input record is already printed).
		w.headerPending = true
	case model.ReasoningDeltaEvent:
		w.writeChunk(event.Text, true)
	case model.TextDeltaEvent:
		w.writeChunk(event.Text, false)
	case model.AssistantMessageCompletedEvent:
		if w.headerPending {
			// No streamed text (a tool-call-only message): header and call
			// lines in one write.
			w.headerPending = false
			var builder strings.Builder
			builder.WriteString(messageHeader())
			for _, call := range event.Message.ToolCalls {
				builder.WriteString("\n")
				builder.WriteString(toolCallLine(call))
			}
			w.printer.line(builder.String())
		} else {
			w.endMessage()
			for _, call := range event.Message.ToolCalls {
				w.printer.line(toolCallLine(call))
			}
		}
		w.inMessage.Store(false)
		w.item = ""
	case model.AssistantMessageFailedEvent:
		// A failed message may leave a pending header (nothing was shown)
		// or a half-streamed body; both end here.
		w.headerPending = false
		w.endMessage()
		w.inMessage.Store(false)
		w.item = ""
	}
}

// messageHeader is the assistant header line for live output.
func messageHeader() string {
	return formatRecord(storage.Record{
		Type: storage.TypeAssistantMessage,
		Time: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// toolCallLine renders one tool call exactly as the committed record would.
func toolCallLine(call model.ToolCall) string {
	arguments := call.Arguments
	if arguments == "" {
		arguments = "{}"
	}
	return "  -> " + call.Name + " " + truncateCallArguments(arguments)
}

// endMessage closes the streamed body after its last character.
func (w *liveWriter) endMessage() {
	if w.sawOutput.Load() && !w.lineStart {
		w.printer.write("\n")
	}
}

// settle closes any interrupted message: an interrupted run ends
// mid-message with no completed event, so runLoop calls this at the
// terminal status.
func (w *liveWriter) settle() {
	w.endMessage()
	w.inMessage.Store(false)
	w.item = ""
}

// writeChunk streams one delta chunk into the body. The pending header
// goes out in the same write as the first chunk, and a reasoning→content
// phase change opens a new line — matching the committed record rendering,
// where the two are separate parts — so the answer never glues onto the
// tail of the reasoning.
func (w *liveWriter) writeChunk(chunk string, reasoning bool) {
	if chunk == "" {
		return
	}
	kind := deltaText
	if reasoning {
		kind = deltaReasoning
	}
	var builder strings.Builder
	if w.lastKind != deltaNone && w.lastKind != kind && !w.lineStart {
		builder.WriteString("\n")
		w.lineStart = true
	}
	if w.headerPending {
		builder.WriteString(messageHeader())
		builder.WriteString("\n")
		w.headerPending = false
	}
	w.lastKind = kind
	w.sawOutput.Store(true)
	formatted := formatLiveChunk(chunk, reasoning, &w.lineStart)
	if reasoning {
		// Each reasoning chunk carries its own color and reset: the
		// gray never survives into the answer, the spinner, or a
		// following write.
		formatted = gray(formatted)
	}
	builder.WriteString(formatted)
	w.printer.write(builder.String())
}

// formatLiveChunk is the pure body formatter: a two-space indent at every
// line start, a '· ' marker on reasoning lines — matching the committed
// record rendering.
func formatLiveChunk(chunk string, reasoning bool, lineStart *bool) string {
	var builder strings.Builder
	for _, r := range chunk {
		if *lineStart {
			if reasoning {
				builder.WriteString("  · ")
			} else {
				builder.WriteString("  ")
			}
			*lineStart = false
		}
		builder.WriteRune(r)
		if r == '\n' {
			*lineStart = true
		}
	}
	return builder.String()
}
