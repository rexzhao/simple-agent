package main

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/storage"
)

// liveWriter renders the transient event stream of the active run onto the
// terminal: the assistant message appears character by character as the
// model generates it, reasoning lines marked like in the committed record
// rendering. The committed assistant.message record is then skipped by the
// record watcher — its content was already shown.
type liveWriter struct {
	printer *spinnerPrinter
	// inMessage is set between AssistantMessageStarted and
	// Completed/Failed; sawOutput once any character of the current
	// message was written. The spinner loop reads both: it spins while
	// nothing is flowing (waiting for the first token, running tools) and
	// stays out of the way while text streams.
	inMessage atomic.Bool
	sawOutput atomic.Bool

	lineStart bool // at the start of a body line (drives the indent)
}

// handle consumes one transient event.
func (w *liveWriter) handle(event model.Event) {
	switch event := event.(type) {
	case model.AssistantMessageStartedEvent:
		w.inMessage.Store(true)
		w.sawOutput.Store(false)
		w.lineStart = true
		// The header prints at message start, with the start time; the
		// committed record is skipped, so no duplicate header appears.
		w.printer.line(formatRecord(storage.Record{
			Type: storage.TypeAssistantMessage,
			Time: time.Now().UTC().Format(time.RFC3339Nano),
		}))
	case model.ReasoningDeltaEvent:
		w.writeChunk(event.Text, true)
	case model.TextDeltaEvent:
		w.writeChunk(event.Text, false)
	case model.AssistantMessageCompletedEvent:
		w.endMessage()
		// Tool calls were not streamed as text; show them exactly as the
		// committed record would.
		for _, call := range event.Message.ToolCalls {
			arguments := call.Arguments
			if arguments == "" {
				arguments = "{}"
			}
			w.printer.line("  -> " + call.Name + " " + truncateCallArguments(arguments))
		}
		w.inMessage.Store(false)
	case model.AssistantMessageFailedEvent:
		w.endMessage()
		w.inMessage.Store(false)
	}
}

// endMessage closes the streamed body after its last character.
func (w *liveWriter) endMessage() {
	if w.sawOutput.Load() && !w.lineStart {
		w.printer.write("\n")
	}
}

// settle is called when the run's event stream closed: an interrupted
// message may have ended mid-line.
func (w *liveWriter) settle() {
	w.endMessage()
	w.inMessage.Store(false)
}

// writeChunk streams one delta chunk into the body.
func (w *liveWriter) writeChunk(chunk string, reasoning bool) {
	if chunk == "" {
		return
	}
	w.sawOutput.Store(true)
	w.printer.write(formatLiveChunk(chunk, reasoning, &w.lineStart))
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
