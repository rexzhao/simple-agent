package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/storage"
)

// recordLabelWidth pads the type column of the common header so the bodies
// of different record kinds align.
const recordLabelWidth = 9

// Display truncation bounds. The stored records are never shortened —
// these apply only to the terminal rendering.
const (
	// displayCallRunes bounds one tool-call argument line (the arguments
	// stay a single line under the header).
	displayCallRunes = 160
	// displayResultLines bounds how many lines of a tool result render.
	displayResultLines = 15
	// displayResultBytes bounds the rendered bytes of a tool result; a
	// single long line alone can exceed the line bound.
	displayResultBytes = 4 * 1024
	// displayReasoningLines / displayReasoningBytes bound the rendered
	// reasoning; the head is kept (its opening frames the problem).
	displayReasoningLines = 8
	displayReasoningBytes = 2 * 1024
	// displayInlineRunes bounds the single-line body rendered inline on the
	// header line instead of as an indented block.
	displayInlineRunes = 120
)

// formatRecord renders one committed record for the terminal: a common
// one-line header (local time, a fixed-width type label, and a
// type-specific summary), followed by the record's content — inline on
// the header line when the content is a short single line, as an
// indented block otherwise.
func formatRecord(record storage.Record) string {
	header := recordHeader(record)
	body := recordBody(record)
	switch {
	case body == "":
		return header
	case !strings.Contains(body, "\n") && len([]rune(body)) <= displayInlineRunes:
		return header + " " + body
	default:
		return header + "\n" + indent(body)
	}
}

// recordHeader is the common first line of every record's rendering.
func recordHeader(record storage.Record) string {
	var label string
	switch record.Type {
	case storage.TypeSessionCreated:
		label = "session"
	case storage.TypeUserInput:
		label = "user"
	case storage.TypeAssistantMessage:
		label = "assistant"
	case storage.TypeToolResult:
		label = "tool"
	case storage.TypeRunFailed:
		label = "error"
	default:
		label = record.Type
	}
	header := fmt.Sprintf("[%s] %-*s", recordClock(record.Time), recordLabelWidth, label)
	switch record.Type {
	case storage.TypeSessionCreated:
		var meta []string
		if record.Provider != "" && record.Model != "" {
			meta = append(meta, record.Provider+"/"+record.Model)
		}
		if record.CWD != "" {
			meta = append(meta, record.CWD)
		}
		if len(meta) > 0 {
			header += " " + strings.Join(meta, " · ")
		}
	case storage.TypeToolResult:
		status := "ok"
		if record.IsError {
			status = "error"
		}
		name := record.ToolName
		if name == "" {
			name = "tool"
		}
		header += fmt.Sprintf(" %s · %s", name, status)
	}
	return header
}

// recordBody is the type-specific content below the header: the text a
// human wants to read, not the wire fields.
func recordBody(record storage.Record) string {
	switch record.Type {
	case storage.TypeUserInput:
		return record.Content
	case storage.TypeAssistantMessage:
		var parts []string
		if record.Reasoning != "" {
			parts = append(parts, prefixLines(renderReasoning(record.Reasoning), "· "))
		}
		if record.Content != "" {
			parts = append(parts, record.Content)
		}
		for _, call := range record.ToolCalls {
			arguments := call.Arguments
			if arguments == "" {
				arguments = "{}"
			}
			parts = append(parts, fmt.Sprintf("-> %s %s", call.Name, truncateCallArguments(arguments)))
		}
		return strings.Join(parts, "\n")
	case storage.TypeToolResult:
		return truncateResultBody(record.Content)
	case storage.TypeRunFailed:
		return record.Error
	default:
		return ""
	}
}

// recordClock renders the record time as a local HH:MM:SS stamp; an
// unparseable or absent time collapses to a placeholder.
func recordClock(value string) string {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.Local().Format("15:04:05")
	}
	return "--:--:--"
}

// indent prefixes every line of the body so multi-line content stays
// visually attached to its header.
func indent(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}

// truncateCallArguments shortens one tool-call argument line, keeping a
// count of what was elided.
func truncateCallArguments(arguments string) string {
	runes := []rune(arguments)
	if len(runes) <= displayCallRunes {
		return arguments
	}
	return string(runes[:displayCallRunes]) + fmt.Sprintf("…(+%d chars)", len(runes)-displayCallRunes)
}

// truncateResultBody bounds the rendered tool output: the tail is kept —
// command summaries and errors land at the end — behind a one-line notice
// of what was elided.
func truncateResultBody(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) <= displayResultLines && len(content) <= displayResultBytes {
		return content
	}
	kept := lines
	if len(kept) > displayResultLines {
		kept = kept[len(kept)-displayResultLines:]
	}
	body := strings.Join(kept, "\n")
	if len(body) > displayResultBytes {
		body = "…" + trimToRuneStart(body[len(body)-displayResultBytes:])
	}
	return fmt.Sprintf("[… showing tail: %d of %d lines, %s of %s]",
		len(kept), len(lines), displaySize(len(body)), displaySize(len(content))) + "\n" + body
}

// renderReasoning truncates long reasoning behind a notice of what was
// elided. Unlike tool results, the head is kept: a reasoning's opening
// frames the problem it is working through.
func renderReasoning(reasoning string) string {
	lines := strings.Split(reasoning, "\n")
	if len(lines) <= displayReasoningLines && len(reasoning) <= displayReasoningBytes {
		return reasoning
	}
	kept := lines
	if len(kept) > displayReasoningLines {
		kept = kept[:displayReasoningLines]
	}
	body := strings.Join(kept, "\n")
	if len(body) > displayReasoningBytes {
		body = trimToRuneEnd(body[:displayReasoningBytes])
	}
	return body + fmt.Sprintf("\n[… +%d more lines]", len(lines)-len(kept))
}

// prefixLines marks every line of the reasoning so it stays visually
// distinct from the answer.
func prefixLines(body, prefix string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// trimToRuneStart advances past a partial rune so a byte-level cut never
// emits invalid UTF-8.
func trimToRuneStart(s string) string {
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// trimToRuneEnd drops a trailing partial rune after a head-keeping cut.
// DecodeLastRuneInString reports an invalid tail as (RuneError, 1), so
// bytes are dropped until the last rune decodes cleanly.
func trimToRuneEnd(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			s = s[:len(s)-1]
			continue
		}
		return s
	}
	return s
}

func displaySize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%dKB", n/1024)
	default:
		return fmt.Sprintf("%dMB", n/(1024*1024))
	}
}
